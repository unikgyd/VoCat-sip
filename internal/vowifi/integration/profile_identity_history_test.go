package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func TestProfileIdentityHistorySurvivesRestartAndNeverCrossesCards(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "profile.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s := EPDGStore{Store: db}
	id := vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"}
	if err = s.RememberIdentityRegistered(ctx, id); err != nil {
		t.Fatal(err)
	}
	setting, err := db.AppSetting(ctx, store.ProfileIdentityHistoryKey)
	if err != nil || !setting.Sensitive || string(setting.Public().Value) != "null" || strings.Contains(string(setting.Value), id.ICCID) || !strings.Contains(string(setting.Value), id.IMSI) {
		t.Fatal("private identity history missing or exposed")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s.Store = db
	live := id
	live.IMSI = "204040123456789"
	live.HomeMCC = "204"
	live.HomeMNC = "04"
	candidates, err := s.IdentityCandidates(ctx, live)
	if err != nil || len(candidates) != 2 || candidates[0].IMSI != id.IMSI || candidates[1].IMSI != live.IMSI {
		t.Fatalf("success priority lost after restart: candidates=%v err=%v", candidates, err)
	}
	other := live
	other.ICCID = "8944100000000000002"
	candidates, err = s.IdentityCandidates(ctx, other)
	if err != nil || len(candidates) != 1 || candidates[0].IMSI != other.IMSI {
		t.Fatal("history crossed cards")
	}
}

func TestProfileIdentityHistoryRetainsCurrentReadWhenEightSuccessesExist(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "capacity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := EPDGStore{Store: db}
	id := vowifi.SIMIdentity{ICCID: "8944100000000000001", HomeMCC: "234", HomeMNC: "15"}
	for i := 0; i < 10; i++ {
		id.IMSI = fmt.Sprintf("234150%09d", i)
		if err := s.RememberIdentityRegistered(ctx, id); err != nil {
			doc, raw, loadErr := s.loadProfileIdentities(ctx)
			t.Fatalf("iteration=%d entries=%d rawBytes=%d load=%v update=%v", i, len(doc.Cards[epdgHash(id.ICCID)]), len(raw), loadErr, err)
		}
	}
	id.IMSI = "204040123456789"
	id.HomeMCC = "204"
	id.HomeMNC = "04"
	candidates, err := s.IdentityCandidates(ctx, id)
	if err != nil || len(candidates) != vowifi.MaxProfileIdentities {
		t.Fatalf("capacity: %d %v", len(candidates), err)
	}
	found := false
	for _, candidate := range candidates {
		found = found || candidate.IMSI == id.IMSI
	}
	if !found {
		t.Fatal("success history evicted the current physical identity")
	}
}

func TestProfileIdentityHistoryCASPreservesIndependentInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "profiles.db")
	one, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	var wg sync.WaitGroup
	for i, s := range []EPDGStore{{Store: one}, {Store: two}} {
		wg.Add(1)
		go func(i int, s EPDGStore) {
			defer wg.Done()
			for n := 0; n < 4; n++ {
				id := vowifi.SIMIdentity{ICCID: fmt.Sprintf("894410000000000000%d", i), IMSI: fmt.Sprintf("234150%09d", i*10+n), HomeMCC: "234", HomeMNC: "15"}
				if _, err := s.IdentityCandidates(ctx, id); err != nil {
					t.Error(err)
					return
				}
			}
		}(i, s)
	}
	wg.Wait()
	doc, _, err := (EPDGStore{Store: one}).loadProfileIdentities(ctx)
	if err != nil || len(doc.Cards) != 2 {
		t.Fatal("concurrent histories overwrote another card")
	}
	for _, entries := range doc.Cards {
		if len(entries) != 4 {
			t.Fatalf("lost observations: %d", len(entries))
		}
	}
}

func TestProfileIdentityCorruptHistoryIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	raw := []byte(`{"version":99,"cards":{}}`)
	if err := db.UpsertAppSetting(ctx, store.AppSetting{Key: store.ProfileIdentityHistoryKey, Value: raw, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	_, err := (EPDGStore{Store: db}).IdentityCandidates(ctx, vowifi.SIMIdentity{ICCID: "card", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"})
	if err == nil {
		t.Fatal("unknown history version silently replaced")
	}
	after, _ := db.AppSetting(ctx, store.ProfileIdentityHistoryKey)
	if string(after.Value) != string(raw) {
		t.Fatal("corrupt history was overwritten")
	}
}
