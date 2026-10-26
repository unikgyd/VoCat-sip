package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/integration"
)

func retryStatusJSON(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(response["ike_retry"])
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status == nil {
		t.Fatal("public runtime lost its structured IKE retry observation")
	}
	return status
}

func retryStatusState() vowifi.State {
	return vowifi.State{DeviceID: "retry-ui-a", ICCID: "8944100000000000061", Phase: vowifi.PhaseFailed, Enabled: true,
		LastErrorClass: "ike_retry_blocked", UpdatedAt: time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC),
		IKERetry: &vowifi.IKERetryStatus{Source: "operator", Scope: "epdg", NotifyType: 10500, AfterSeconds: 600}}
}

func TestIKERetryStatusLiveProjectionKeepsOnlySafeObservedFields(t *testing.T) {
	state := retryStatusState()
	deadline := state.UpdatedAt.Add(10 * time.Minute)
	state.IKERetry.NotBefore = &deadline
	got := retryStatusJSON(t, liveVoWiFiRuntime(state))
	if got["source"] != "operator" || got["scope"] != "epdg" || got["notify_type"] != float64(10500) || got["after_seconds"] != float64(600) || got["cached"] != false {
		t.Fatalf("wrong live observation: %#v", got)
	}
	if got["not_before"] != nil || got["token"] != nil || got["iccid"] != nil || got["endpoint"] != nil {
		t.Fatalf("private/deadline field leaked: %#v", got)
	}
	if got["observed_at"] != "2026-10-26T01:00:00Z" {
		t.Fatalf("observation time = %#v", got["observed_at"])
	}
	// The projection must not mutate the runtime's gate evidence.
	if state.IKERetry.NotBefore == nil || !state.IKERetry.NotBefore.Equal(deadline) {
		t.Fatal("projection changed gate evidence")
	}
}

func TestIKERetryStatusActualProjectorRoundTripRetainsCachedObservation(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	state := retryStatusState()
	state.CleanupErrors = []string{"synthetic cleanup is incomplete"}
	if err := database.UpsertDevice(context.Background(), store.Device{ID: state.DeviceID, Name: "Synthetic retry UI"}); err != nil {
		t.Fatal(err)
	}
	if err := (integration.StateProjector{Store: database}).Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	saved, err := database.VoWiFiRuntime(context.Background(), state.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	got := retryStatusJSON(t, storedVoWiFiRuntime(saved))
	if got["source"] != "operator" || got["cached"] != true || got["cleanup_pending"] != true || got["after_seconds"] != float64(600) {
		t.Fatalf("wrong saved observation: %#v", got)
	}
}

func TestIKERetryStatusStoredProjectionDoesNotExposeExtraPrivateFields(t *testing.T) {
	state := store.VoWiFiRuntime{DeviceID: "retry-ui-a", ICCID: "8944100000000000061", UpdatedAt: time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC),
		Extra: json.RawMessage(`{"enabled":true,"ike_retry":{"source":"uncertain-attempt","until_card_event":true,"token":"raw-private-token","subscriber":"raw-private-subscriber"}}`)}
	got := retryStatusJSON(t, storedVoWiFiRuntime(state))
	raw, _ := json.Marshal(got)
	if got["source"] != "uncertain-attempt" || got["until_card_event"] != true || strings.Contains(string(raw), "raw-private") {
		t.Fatalf("unsafe stored projection: %s", raw)
	}
}

func TestIKERetryStatusNilObservationDoesNotInventCarrierWait(t *testing.T) {
	state := retryStatusState()
	state.IKERetry = nil
	if liveVoWiFiRuntime(state)["ike_retry"] != nil {
		t.Fatal("invented live carrier wait")
	}
	if storedVoWiFiRuntime(store.VoWiFiRuntime{Extra: json.RawMessage(`{"enabled":true}`)})["ike_retry"] != nil {
		t.Fatal("invented legacy carrier wait")
	}
}

func TestIKERetryStatusDoesNotBorrowAnotherSIMInConfiguredSummary(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	state := retryStatusState()
	s := &Server{store: database, vowifi: &fakeVoWiFiController{state: state}}
	entry := &device.Device{ID: state.DeviceID, Snapshot: &device.Snapshot{ICCID: "8944100000000000062"}}
	response := s.configuredDeviceSummary(store.Device{ID: state.DeviceID, VoWiFiEnabled: true}, entry)
	runtime := response["vowifi_runtime"].(map[string]any)
	if runtime["ike_retry"] != nil {
		t.Fatalf("other card retry leaked: %#v", runtime)
	}
}

func TestIKERetryStatusConfiguredSummaryRequiresKnownMatchingCard(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	state := retryStatusState()
	s := &Server{store: database, vowifi: &fakeVoWiFiController{state: state}}
	config := store.Device{ID: state.DeviceID, VoWiFiEnabled: true}
	for _, card := range []string{state.ICCID, ""} {
		entry := &device.Device{ID: state.DeviceID, Snapshot: &device.Snapshot{ICCID: card}}
		got := s.configuredDeviceSummary(config, entry)["vowifi_runtime"].(map[string]any)
		if card == state.ICCID {
			retryStatusJSON(t, got)
		} else if got["ike_retry"] != nil {
			t.Fatal("unknown current card was labeled with a previous retry observation")
		}
	}
}
