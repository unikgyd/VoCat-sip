package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/integration"
)

type retryStatusScopeClock struct{}

func (retryStatusScopeClock) Snapshot() (vowifi.IKERetryTime, error) {
	return vowifi.IKERetryTime{Wall: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Boot: strings.Repeat("a", 32), Uptime: time.Hour}, nil
}

// Produce the observation with the real admission gate, not manually spelled
// status strings. Check both actual API projections after a SQLite round trip.
func TestIKERetryStatusEveryActualGateScopeSurvivesProjection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  uint16
		input vowifi.EPDGRejectionScope
		plmn  string
		pause bool
	}{
		{"epdg", 10500, vowifi.EPDGRejectionEndpoint, "46011", false},
		{"plmn_apn", 9002, vowifi.EPDGRejectionPLMNAPN, "46011", false},
		{"plmn", 9000, vowifi.EPDGRejectionPLMN, "46011", true},
		{"unbound_plmn_apn", 9002, vowifi.EPDGRejectionPLMNAPN, "", false},
		{"unbound_plmn", 9000, vowifi.EPDGRejectionPLMN, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			gate := vowifi.NewIKERetryGate(nil, retryStatusScopeClock{})
			card, err := gate.Prepare(ctx, "synthetic-scope-device", "synthetic-scope-card")
			if err != nil {
				t.Fatal(err)
			}
			target := vowifi.IKERetryTarget{Host: "epdg.example", PLMN: tc.plmn, APN: "ims"}
			token, err := gate.Begin(ctx, card, target)
			if err != nil {
				t.Fatal(err)
			}
			err = gate.Observe(ctx, card, target, token, vowifi.EPDGNetworkRejection{NotifyType: tc.code, Scope: tc.input, Backoff: vowifi.DecodeIKEBackoffTimer([]byte{1, 1}), EPDG: target.Host, Endpoint: "192.0.2.9", APN: target.APN})
			if err != nil {
				t.Fatal(err)
			}
			if err := gate.Complete(ctx, card, token); err != nil {
				t.Fatal(err)
			}
			var blocked *vowifi.IKERetryBlockedError
			if !errors.As(gate.Permit(ctx, card, target, ""), &blocked) {
				t.Fatal("real gate did not produce expected restriction")
			}
			state := retryStatusState()
			state.IKERetry = &blocked.Status
			database, err := store.Open(ctx, ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err := database.UpsertDevice(ctx, store.Device{ID: state.DeviceID, Name: "Synthetic scope"}); err != nil {
				t.Fatal(err)
			}
			if err := (integration.StateProjector{Store: database}).Save(ctx, state); err != nil {
				t.Fatal(err)
			}
			saved, err := database.VoWiFiRuntime(ctx, state.DeviceID)
			if err != nil {
				t.Fatal(err)
			}
			for _, response := range []map[string]any{liveVoWiFiRuntime(state), storedVoWiFiRuntime(saved)} {
				got := retryStatusJSON(t, response)
				if got["scope"] != tc.name || got["source"] != "operator" || got["notify_type"] != float64(tc.code) || got["until_card_event"] != tc.pause {
					t.Fatalf("actual gate evidence changed during projection: %#v", got)
				}
				if !tc.pause && got["after_seconds"] != float64(600) {
					t.Fatalf("valid wait became a pause: %#v", got)
				}
				if got["not_before"] != nil {
					t.Fatal("private deadline leaked")
				}
			}
		})
	}
}
