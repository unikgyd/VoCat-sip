package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestIMSModePersistenceAndLegacyCompatibility(t *testing.T) {
	db := openTestStore(t, filepath.Join(t.TempDir(), "modes.db"))
	ctx := context.Background()
	for _, tc := range []struct {
		name, mode, wantMode         string
		enabled, legacy, wantEnabled bool
	}{
		{"legacy-on", "", IMSModeVoWiFi, false, true, true},
		{"legacy-off", "", IMSModeModule, false, false, false},
		{"host-on", IMSModeHost, IMSModeHost, true, true, true},
		{"host-off", IMSModeHost, IMSModeHost, false, true, false},
		{"wifi-on", IMSModeVoWiFi, IMSModeVoWiFi, true, false, true},
		{"wifi-off", IMSModeVoWiFi, IMSModeVoWiFi, false, true, false},
		{"module", IMSModeModule, IMSModeModule, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := Device{ID: tc.name, Name: tc.name, IMSMode: tc.mode, IMSEnabled: tc.enabled, VoWiFiEnabled: tc.legacy}
			if err := db.UpsertDevice(ctx, value); err != nil {
				t.Fatal(err)
			}
			got, err := db.Device(ctx, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if EffectiveIMSMode(got) != tc.wantMode || EffectiveIMSEnabled(got) != tc.wantEnabled {
				t.Fatalf("mode=%s enabled=%v", EffectiveIMSMode(got), EffectiveIMSEnabled(got))
			}
			if got.IMSMode == IMSModeHost && got.VoWiFiEnabled {
				t.Fatal("Host IMS retained RF-off legacy enable")
			}
		})
	}
	for _, value := range []Device{
		{ID: "bad", Name: "bad", IMSMode: "other"},
		{ID: "reader", Name: "reader", DeviceType: DeviceTypeUSBSIMReader, IMSMode: IMSModeHost, IMSEnabled: true},
	} {
		if db.UpsertDevice(ctx, value) == nil {
			t.Fatal("accepted unsupported mode/device")
		}
	}
}

func TestMigration28DoesNotAutomaticallyEnableHostIMS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 27; version++ {
		for _, statement := range migrationStatements(version) {
			if _, err := raw.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = raw.Exec(`INSERT INTO devices(id,name,vowifi_enabled,created_at,updated_at) VALUES ('legacy','Legacy',1,100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`PRAGMA user_version = 27`); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTestStore(t, path)
	got, err := db.Device(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.IMSMode != "" || got.IMSEnabled || !got.VoWiFiEnabled || EffectiveIMSMode(got) != IMSModeVoWiFi {
		t.Fatal("migration changed legacy desired mode")
	}
}
