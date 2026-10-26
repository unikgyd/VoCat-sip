package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestHostIMSClaimAtomicAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "claims.db")
	one, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	key := "host.ims.claim.private.v1." + strings.Repeat("a", 64)
	before, after := json.RawMessage(`{"generation":1}`), json.RawMessage(`{"generation":2}`)
	var winners atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			db := one
			if i%2 == 1 {
				db = two
			}
			ok, err := db.CompareHostIMSClaim(ctx, key, nil, before)
			if err != nil {
				t.Error(err)
			}
			if ok {
				winners.Add(1)
			}
		}(i)
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatalf("claim winners=%d", winners.Load())
	}
	setting, err := two.AppSetting(ctx, key)
	if err != nil || !setting.Sensitive {
		t.Fatal("claim not stored as private state")
	}
	if ok, err := two.CompareHostIMSClaim(ctx, key, after, nil); err != nil || ok {
		t.Fatal("stale writer deleted claim")
	}
	if ok, err := two.CompareHostIMSClaim(ctx, key, before, after); err != nil || !ok {
		t.Fatal("valid CAS failed")
	}
	if ok, err := one.CompareHostIMSClaim(ctx, key, before, nil); err != nil || ok {
		t.Fatal("old generation deleted new claim")
	}
	if ok, err := one.CompareHostIMSClaim(ctx, key, after, nil); err != nil || !ok {
		t.Fatal("matching claim not deleted")
	}
}

func TestHostIMSClaimRejectsInvalidNamespaceAndPayload(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, key := range []string{"ordinary.setting", "host.ims.claim.private.v1.x", "host.ims.claim.private.v1." + strings.Repeat("z", 64)} {
		if _, err := db.CompareHostIMSClaim(context.Background(), key, nil, json.RawMessage(`{}`)); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	key := "host.ims.claim.private.v1." + strings.Repeat("a", 64)
	for _, body := range []json.RawMessage{[]byte(`{`), []byte{}, []byte(strings.Repeat("x", 16385))} {
		if _, err := db.CompareHostIMSClaim(context.Background(), key, nil, body); err == nil {
			t.Fatal("invalid payload accepted")
		}
	}
	oversize := json.RawMessage(`{"payload":"` + strings.Repeat("x", MaxHostIMSClaimBytes) + `"}`)
	if _, err := db.CompareHostIMSClaim(context.Background(), key, nil, oversize); err == nil {
		t.Fatal("oversize private WAL accepted")
	}
}
