package store

import (
	"context"
	"strings"
	"testing"
)

func TestHostIMSClaimPresenceReadsNoPrivatePayload(t *testing.T) {
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const prefix = "host.ims.claim.private.v1."
	key := prefix + strings.Repeat("a", 64)
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO app_settings(key,value_json,sensitive,updated_at)VALUES(?,?,0,0)`, key, "not-json-private-payload"); err != nil {
		t.Fatal(err)
	}
	if present, err := db.HasHostIMSClaim(context.Background(), key); err != nil || !present {
		t.Fatal("malformed opaque payload did not count as pending", err)
	}
	if present, err := db.HasHostIMSClaim(context.Background(), prefix+strings.Repeat("b", 64)); err != nil || present {
		t.Fatal("exact equipment fence matched another row", err)
	}
	if present, err := db.HasAnyHostIMSClaim(context.Background()); err != nil || !present {
		t.Fatal("namespace presence ignored malformed row", err)
	}
	if _, err := db.HasHostIMSClaim(context.Background(), prefix+"bad-key"); err == nil {
		t.Fatal("invalid equipment key accepted")
	}
}
