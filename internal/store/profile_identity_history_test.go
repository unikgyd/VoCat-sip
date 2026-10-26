package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestProfileIdentityHistoryCASIsDurableAndNamespaceRestricted(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	one := json.RawMessage(`{"version":1,"cards":{}}`)
	two := json.RawMessage(`{"version":2}`)
	if ok, err := db.CompareProfileIdentityHistory(ctx, nil, one); err != nil || !ok {
		t.Fatalf("initial CAS: %v %v", ok, err)
	}
	if ok, err := db.CompareProfileIdentityHistory(ctx, two, one); err != nil || ok {
		t.Fatal("stale compare overwrote history")
	}
	if ok, err := db.CompareProfileIdentityHistory(ctx, one, two); err != nil || !ok {
		t.Fatal("valid compare failed")
	}
	saved, err := db.AppSetting(ctx, ProfileIdentityHistoryKey)
	if err != nil || !saved.Sensitive || string(saved.Value) != string(two) {
		t.Fatal("private history value mismatch")
	}
	if _, err := db.AppSetting(ctx, "sip.gateway.private.v1"); err != ErrNotFound {
		t.Fatal("identity journal touched SIP permissions")
	}
}

func TestProfileIdentityHistoryMemoryStoreIsNotDurabilityEvidence(t *testing.T) {
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ok, err := db.CompareProfileIdentityHistory(context.Background(), nil, json.RawMessage(`{"version":1}`)); err == nil || ok {
		t.Fatal("memory database claimed durable history")
	}
}
