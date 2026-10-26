package main

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type policyIdentityReader struct {
	identity vowifi.SIMIdentity
	during   func()
}

func (s policyIdentityReader) ReadIdentity(context.Context, string) (vowifi.SIMIdentity, error) {
	if s.during != nil {
		s.during()
	}
	return s.identity, nil
}

func policyGuardStore(t *testing.T) (*store.Store, store.Device, store.TelephonyPolicy) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := store.TelephonyPolicy{ICCID: "8944100000000000001", CellularMode: store.IMSModeHost,
		AutoConnectCellular: true, AutoConnectWiFi: true, DefaultRoute: store.IMSModeHost, WiFiRadioPolicy: "coexist"}
	config := store.Device{ID: "modem", Name: "Modem"}
	if err := db.SaveTelephonyPolicy(context.Background(), config, p); err != nil {
		t.Fatal(err)
	}
	return db, config, p
}

func TestTelephonySubscriptionReaderRejectsSwapsAndAllowsRouteEdits(t *testing.T) {
	for _, scenario := range []string{"matching", "wrong-card", "swap-during-read", "default-route-only"} {
		t.Run(scenario, func(t *testing.T) {
			db, config, policy := policyGuardStore(t)
			reader := policyIdentityReader{identity: vowifi.SIMIdentity{ICCID: policy.ICCID, IMSI: "234150000000001"}}
			switch scenario {
			case "wrong-card":
				reader.identity.ICCID = "8944100000000000002"
			case "swap-during-read":
				reader.during = func() {
					policy.ICCID = "8944100000000000002"
					if err := db.SaveTelephonyPolicy(context.Background(), config, policy); err != nil {
						t.Fatal(err)
					}
				}
			case "default-route-only":
				reader.during = func() {
					policy.DefaultRoute = store.IMSModeVoWiFi
					if err := db.SaveTelephonyPolicy(context.Background(), config, policy); err != nil {
						t.Fatal(err)
					}
				}
			}
			identity, err := (subscriptionSIMReader{reader: reader, database: db}).ReadIdentity(context.Background(), config.ID)
			deny := scenario == "wrong-card" || scenario == "swap-during-read"
			if deny && (!errors.Is(err, errTelephonySubscriptionChanged) || identity.ICCID != "") {
				t.Fatalf("changed SIM was admitted: %+v, %v", identity, err)
			}
			if !deny && (err != nil || identity.ICCID != policy.ICCID) {
				t.Fatalf("unchanged access was interrupted: %+v, %v", identity, err)
			}
		})
	}
}

func TestTelephonyWiFiPolicyIsIndependentOfPreferredHostRoute(t *testing.T) {
	db, config, policy := policyGuardStore(t)
	if radio, err := applicationWiFiRadioPolicy(context.Background(), db, config.ID); err != nil || radio != vowifi.RadioPolicyCoexist {
		t.Fatalf("preferred Host route disabled Wi-Fi: %s, %v", radio, err)
	}
	policy.AutoConnectWiFi = false
	if err := db.SaveTelephonyPolicy(context.Background(), config, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := applicationWiFiRadioPolicy(context.Background(), db, config.ID); !errors.Is(err, errTelephonySubscriptionChanged) {
		t.Fatal("disabled Wi-Fi access was started")
	}
}
