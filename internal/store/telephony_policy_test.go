package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestTelephonyPolicyKeepsAccessFlagsIndependentFromDefaultRoute(t *testing.T) {
	db := openTestStore(t, filepath.Join(t.TempDir(), "access.db"))
	ctx := context.Background()
	p := TelephonyPolicy{ICCID: "8944100000000000001", CellularMode: IMSModeHost, AutoConnectCellular: true,
		AutoConnectWiFi: true, DefaultRoute: IMSModeVoWiFi, WiFiRadioPolicy: "coexist", HostIMSProfileID: 5}
	config := Device{ID: "modem", Name: "Modem"}
	if err := db.SaveTelephonyPolicy(ctx, config, p); err != nil {
		t.Fatal(err)
	}
	got, err := db.Device(ctx, config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Telephony == nil || *got.Telephony != p || CellularMode(got) != IMSModeHost || !AccessEnabled(got, IMSModeHost) || !AccessEnabled(got, IMSModeVoWiFi) || EffectiveIMSMode(got) != IMSModeVoWiFi || !got.VoWiFiEnabled {
		t.Fatalf("access policy was reduced to an exclusive mode: %+v", got)
	}
	p.DefaultRoute = IMSModeHost
	if err := db.SaveTelephonyPolicy(ctx, got, p); err != nil {
		t.Fatal(err)
	}
	got, _ = db.Device(ctx, config.ID)
	if !AccessEnabled(got, IMSModeVoWiFi) || !AccessEnabled(got, IMSModeHost) || !got.VoWiFiEnabled || EffectiveIMSMode(got) != IMSModeHost {
		t.Fatal("changing the default route changed access intent")
	}
	saved, err := db.SIMTelephonyPolicy(ctx, p.ICCID)
	if err != nil || saved != p {
		t.Fatalf("per-SIM preference: %+v, %v", saved, err)
	}
}

func TestTelephonyPolicyFollowsSIMAndSurvivesModemRemoval(t *testing.T) {
	db := openTestStore(t, filepath.Join(t.TempDir(), "move.db"))
	ctx := context.Background()
	p := TelephonyPolicy{ICCID: "8944100000000000001", CellularMode: IMSModeModule, AutoConnectCellular: true,
		AutoConnectWiFi: true, DefaultRoute: IMSModeModule, WiFiRadioPolicy: "coexist"}
	if err := db.SaveTelephonyPolicy(ctx, Device{ID: "old", Name: "Old"}, p); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteDevice(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	restored, err := db.SIMTelephonyPolicy(ctx, p.ICCID)
	if err != nil || restored != p {
		t.Fatalf("policy was tied to the removed modem: %v", err)
	}
	if _, err := db.SIMTelephonyPolicy(ctx, "8944100000000000002"); !errors.Is(err, ErrNotFound) {
		t.Fatal("new card inherited old policy")
	}
	if err := db.SaveTelephonyPolicy(ctx, Device{ID: "new", Name: "New"}, restored); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Device(ctx, "new")
	if got.Telephony == nil || *got.Telephony != p {
		t.Fatal("policy was not applied to the new modem")
	}
	// A reader may retain this SIM's cellular preference for its next modem,
	// while exposing no cellular capability on the current physical device.
	got.DeviceType = DeviceTypeUSBSIMReader
	if err := db.SaveTelephonyPolicy(ctx, got, restored); err != nil {
		t.Fatal(err)
	}
	got, _ = db.Device(ctx, "new")
	if AccessEnabled(got, IMSModeModule) || !AccessEnabled(got, IMSModeVoWiFi) {
		t.Fatal("reader acquired a cellular access capability")
	}
}

func TestLegacyTelephonyMigrationDoesNotAutoEnableAnotherAccess(t *testing.T) {
	for _, value := range []Device{
		{IMSMode: IMSModeVoWiFi, IMSEnabled: true}, {VoWiFiEnabled: true},
		{IMSMode: IMSModeHost, IMSEnabled: true}, {IMSMode: IMSModeHost},
		{IMSMode: IMSModeModule}, {},
	} {
		p := LegacyTelephonyPolicy(value, "8944100000000000001")
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{IMSModeModule, IMSModeHost, IMSModeVoWiFi} {
			before := AccessEnabled(value, mode)
			after := value
			after.Telephony = &p
			if AccessEnabled(after, mode) != before {
				t.Fatalf("migration enabled another access: %+v (%s)", p, mode)
			}
		}
		if EffectiveIMSMode(value) != p.DefaultRoute {
			t.Fatal("migration changed default route")
		}
	}
}

func TestTelephonyPolicyInvalidSaveIsAtomic(t *testing.T) {
	db := openTestStore(t, filepath.Join(t.TempDir(), "atomic.db"))
	ctx := context.Background()
	p := TelephonyPolicy{ICCID: "8944100000000000001", CellularMode: IMSModeModule, DefaultRoute: IMSModeModule, WiFiRadioPolicy: "coexist"}
	config := Device{ID: "modem", Name: "Modem"}
	if err := db.SaveTelephonyPolicy(ctx, config, p); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*TelephonyPolicy){
		func(p *TelephonyPolicy) { p.ICCID = "" }, func(p *TelephonyPolicy) { p.CellularMode = IMSModeVoWiFi },
		func(p *TelephonyPolicy) { p.DefaultRoute = "automatic" },
		func(p *TelephonyPolicy) { p.AutoConnectCellular = true; p.WiFiRadioPolicy = "exclusive" },
	} {
		next := p
		mutate(&next)
		if db.SaveTelephonyPolicy(ctx, config, next) == nil {
			t.Fatal("accepted invalid policy")
		}
		saved, _ := db.SIMTelephonyPolicy(ctx, p.ICCID)
		got, _ := db.Device(ctx, config.ID)
		if saved != p || got.Telephony == nil || *got.Telephony != p {
			t.Fatal("invalid save changed an existing policy")
		}
	}
}

func TestTelephonyPolicyWriteFailureRollsBackBothDeviceAndSIM(t *testing.T) {
	db := openTestStore(t, filepath.Join(t.TempDir(), "rollback.db"))
	ctx := context.Background()
	old := TelephonyPolicy{ICCID: "8944100000000000001", CellularMode: IMSModeModule, DefaultRoute: IMSModeModule, WiFiRadioPolicy: "coexist"}
	config := Device{ID: "modem", Name: "Original"}
	if err := db.SaveTelephonyPolicy(ctx, config, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER reject_new_sim BEFORE INSERT ON sim_telephony_policies
		WHEN new.iccid='8944100000000000002' BEGIN SELECT RAISE(ABORT,'injected disk write failure'); END`); err != nil {
		t.Fatal(err)
	}
	next := old
	next.ICCID = "8944100000000000002"
	next.AutoConnectWiFi = true
	config.Name = "Must not survive"
	if db.SaveTelephonyPolicy(ctx, config, next) == nil {
		t.Fatal("write failure was hidden")
	}
	got, err := db.Device(ctx, config.ID)
	if err != nil || got.Name != "Original" || got.Telephony == nil || *got.Telephony != old {
		t.Fatal("partial device projection survived rollback")
	}
	if _, err := db.SIMTelephonyPolicy(ctx, next.ICCID); !errors.Is(err, ErrNotFound) {
		t.Fatal("partial SIM policy survived rollback")
	}
}
