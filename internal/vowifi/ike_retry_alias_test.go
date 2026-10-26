package vowifi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestIKERetryAliasPeerNormalizationExpiryAndOtherCards(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	clock := newRetryTestClock()
	g := NewIKERetryGate(store, clock)
	card := retryTestPrepare(t, g, "a", "card-a")
	other := retryTestPrepare(t, g, "b", "card-b")
	first := IKERetryTarget{"first.example", "23415", "ims"}
	alias := IKERetryTarget{"alias.example", "23415", "ims"}
	retryTestObserve(t, g, card, first, 10500, []byte{1, 1})
	if err := g.Permit(ctx, card, alias, ""); err != nil {
		t.Fatal("alias must be resolved before judging its address", err)
	}
	token, err := g.Begin(ctx, card, alias)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"192.0.2.9", "::ffff:192.0.2.9"} {
		retryTestBlocked(t, g.PermitEndpoint(ctx, card, alias, token, peer))
	}
	if err = g.PermitEndpoint(ctx, card, alias, token, "192.0.2.10"); err != nil {
		t.Fatal("different peer was blanket blocked", err)
	}
	if err = g.PermitEndpoint(ctx, other, alias, "", "192.0.2.9"); err != nil {
		t.Fatal("other SIM was blocked", err)
	}
	if err = g.Complete(ctx, card, token); err != nil {
		t.Fatal(err)
	}
	clock.advance(10*time.Minute, 0)
	g = NewIKERetryGate(store, clock)
	if err = g.PermitEndpoint(ctx, card, alias, "", "192.0.2.9"); err != nil {
		t.Fatal("expired peer prohibition survived", err)
	}
}

func TestIKERetryAliasLegacyRecordWithoutAddressCannotGuessDifferentPeer(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	g := NewIKERetryGate(store, newRetryTestClock())
	card := retryTestPrepare(t, g, "a", "card-a")
	first := IKERetryTarget{"first.example", "23415", "ims"}
	alias := IKERetryTarget{"alias.example", "23415", "ims"}
	retryTestObserve(t, g, card, first, 10500, []byte{1, 1})
	before, _ := store.LoadIKERetry(ctx)
	doc, err := decodeIKERetryJournal(before)
	if err != nil {
		t.Fatal(err)
	}
	for key, entry := range doc.Entries {
		delete(doc.Entries, key)
		entry.Endpoint = ""
		doc.Entries[retryEntryKey(entry)] = entry
		break
	}
	after, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.CompareIKERetry(ctx, before, after); err != nil || !ok {
		t.Fatal("legacy fixture failed", err)
	}
	g = NewIKERetryGate(store, newRetryTestClock())
	retryTestBlocked(t, g.PermitEndpoint(ctx, card, alias, "", "192.0.2.10"))
}

func TestIKERetryAliasMultiplePeerReceiptsCannotOverwriteOneAnother(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	g := NewIKERetryGate(store, newRetryTestClock())
	card := retryTestPrepare(t, g, "a", "card-a")
	first := IKERetryTarget{"first.example", "23415", "ims"}
	alias := IKERetryTarget{"alias.example", "23415", "ims"}
	token, err := g.Begin(ctx, card, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"192.0.2.9", "192.0.2.10"} {
		err = g.Observe(ctx, card, first, token, EPDGNetworkRejection{NotifyType: 10500, Scope: EPDGRejectionEndpoint, Backoff: DecodeIKEBackoffTimer([]byte{1, 1}), EPDG: first.Host, APN: first.APN, Endpoint: peer})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = g.Complete(ctx, card, token); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"192.0.2.9", "192.0.2.10"} {
		retryTestBlocked(t, g.PermitEndpoint(ctx, card, alias, "", peer))
	}
	if err = g.PermitEndpoint(ctx, card, alias, "", "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
}

func TestIKERetryAliasInvalidResolvedAddressCannotGrantAdmission(t *testing.T) {
	g := NewIKERetryGate(nil, newRetryTestClock())
	card := retryTestPrepare(t, g, "a", "card-a")
	target := IKERetryTarget{"alias.example", "23415", "ims"}
	for _, peer := range []string{"", "host.example", "127.0.0.1", "0.0.0.0", "192.0.2.9:500"} {
		if err := g.PermitEndpoint(context.Background(), card, target, "", peer); !errors.Is(err, ErrIKERetryBinding) {
			t.Fatalf("unbound peer %q accepted: %v", peer, err)
		}
	}
}
