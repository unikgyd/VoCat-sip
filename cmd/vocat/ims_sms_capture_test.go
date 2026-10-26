package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi/ims"
)

// Exercise the actual application callback shared by cellular Host IMS and
// VoWiFi. No modem, QMI, IMS Provider registration or external service starts.
func TestApplicationIMSReceiveCallbackAtomicallyKeepsOriginalSIPPlan(t *testing.T) {
	for _, mode := range []string{store.IMSModeHost, store.IMSModeVoWiFi} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "application-receive.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			// Same order as run(): install before configuring an IMS receiver.
			sipgateway.InstallSMSForwarding(database)
			manager := sipgateway.New(ctx, database)
			defer manager.Close()
			socket, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := socket.LocalAddr().String()
			if err := socket.Close(); err != nil {
				t.Fatal(err)
			}
			config := sipgateway.DefaultConfig()
			config.Enabled, config.SMSEnabled, config.Listen = true, true, address
			config.AllowedCIDRs = []string{"127.0.0.0/8"}
			config.Accounts = []sipgateway.Account{{Username: "1001", Password: "synthetic-application-one"}, {Username: "1002", Password: "synthetic-application-two"}}
			config.CarrierLines = []sipgateway.CarrierLine{{DeviceID: "module-a", AllowSMSReceive: true, SMSRecipients: []string{"1001"}}}
			if _, err := manager.Apply(ctx, config); err != nil {
				t.Fatal(err)
			}
			const iccid = "8901000000000000001"
			if err := database.UpsertPhoneAssociation(ctx, store.PhoneAssociation{ICCID: iccid, DeviceID: "module-a", Number: "+15550000009", Source: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			device := store.Device{ID: "module-a", IMSMode: mode, ModemIMEI: "860000000000001"}
			settings, err := applicationIMSConfig(ctx, device, database, regionTestLogger(), nil)
			if err != nil {
				t.Fatal(err)
			}
			message := ims.ReceivedSMS{DeviceID: device.ID, MessageID: "synthetic-real-callback", ICCID: iccid,
				IMSI: "001010000000001", From: "+15550000001", Text: "synthetic atomic callback body", Timestamp: time.Now()}
			if err := settings.OnSMS(ctx, message); err != nil {
				t.Fatal(err)
			}
			items, err := database.ListSMSMessages(ctx, store.SMSFilter{DeviceID: device.ID})
			if err != nil || len(items) != 1 || items[0].LocalPhone != "+15550000009" {
				t.Fatal("actual callback lost its Inbox or same-transaction display enrichment:", err)
			}
			changed := manager.Snapshot().Config
			changed.CarrierLines[0].SMSRecipients = []string{"1002"}
			if _, err := manager.Apply(ctx, changed); err != nil {
				t.Fatal(err)
			}
			// A network replay takes the same callback. It must not extend the
			// original first receipt plan using the newly saved recipient grant.
			if err := settings.OnSMS(ctx, message); err != nil {
				t.Fatal(err)
			}
			value, err := database.AppSetting(ctx, sipgateway.SMSLedgerSettingKey)
			var ledger struct {
				Inbox map[string]struct {
					InboxID           int64                      `json:"inbox_id"`
					SourceKey         string                     `json:"source_key"`
					SourceFingerprint string                     `json:"source_fingerprint"`
					Targets           map[string]json.RawMessage `json:"targets"`
				} `json:"inbox"`
			}
			if err != nil || !value.Sensitive || json.Unmarshal(value.Value, &ledger) != nil || len(ledger.Inbox) != 1 {
				t.Fatal("actual callback did not save one private atomic forwarding plan:", err)
			}
			for _, plan := range ledger.Inbox {
				if plan.InboxID != items[0].ID || len(plan.SourceKey) != 64 || plan.SourceFingerprint != store.SMSForwardFingerprint(items[0]) || len(plan.Targets) != 1 || plan.Targets["1001"] == nil || plan.Targets["1002"] != nil {
					t.Fatal("callback/policy change reassigned the original message or lost its actual Inbox guard")
				}
			}
		})
	}
}
