package hostims

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/store"
)

func TestHostIMSDurableEquipmentFenceKeepsUnknownRecordOpaque(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	j := SQLiteClaimJournal{Database: db}
	const equipment = "123456789012340"
	if pending, err := j.HasEquipmentClaim(context.Background(), equipment); err != nil || pending {
		t.Fatal("empty equipment was not clean", err)
	}
	if pending, err := j.HasAnyClaim(context.Background()); err != nil || pending {
		t.Fatal("empty namespace was not clean", err)
	}
	// Intentionally not a decodable claim: presence alone must deny release.
	if ok, err := j.Compare(context.Background(), hashText("imei:"+equipment), nil, []byte(`{"unknown_schema":true}`)); err != nil || !ok {
		t.Fatal(err)
	}
	if pending, err := j.HasEquipmentClaim(context.Background(), equipment); err != nil || !pending {
		t.Fatal("unknown durable record was treated as clean", err)
	}
	if pending, err := j.HasAnyClaim(context.Background()); err != nil || !pending {
		t.Fatal("unknown namespace record was ignored", err)
	}
	if pending, err := j.HasEquipmentClaim(context.Background(), "123456789012341"); err != nil || pending {
		t.Fatal("one equipment fenced another", err)
	}
	if _, err := j.HasEquipmentClaim(context.Background(), "000000000000000"); !errors.Is(err, ErrSelection) {
		t.Fatal("invalid equipment accepted", err)
	}
	if ok, err := j.Compare(context.Background(), hashText("imei:"+equipment), []byte(`{"unknown_schema":true}`), nil); err != nil || !ok {
		t.Fatal(err)
	}
	if pending, err := j.HasEquipmentClaim(context.Background(), equipment); err != nil || pending {
		t.Fatal("acknowledged removal still pending", err)
	}
}

func TestHostIMSDurableEquipmentFenceReadFailureIsNotClean(t *testing.T) {
	j := SQLiteClaimJournal{}
	if pending, err := j.HasEquipmentClaim(context.Background(), "123456789012340"); pending || !errors.Is(err, ErrJournal) {
		t.Fatal("unreadable journal was treated as clean", err)
	}
}
