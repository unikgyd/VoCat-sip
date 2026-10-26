package hostims

import (
	"context"
	"errors"
	"testing"
)

func restartBearerPool(t *testing.T, old *BearerPool, b *fakeBearerBackend, j *memoryClaims, s Selection) (*BearerPool, Selection) {
	t.Helper()
	c := old.claims[s.key]
	if c != nil && c.lock != nil {
		c.lock.Close()
	} // Process exit releases descriptors, not its durable journal.
	p, err := NewBearerPool(b, j, func(context.Context, Selection) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	selection, err := p.Select(context.Background(), s.snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	return p, selection
}

func TestRecoveryCleanupAbsentJournalReleasesProbeLock(t *testing.T) {
	p, b, j, s := bearerFixture(t)
	owner, err := p.RecoveryCleanup(context.Background(), s)
	if err != nil || owner != nil || p.HasPendingClaim(s) || len(j.values) != 0 || len(b.calls) != 0 {
		t.Fatal("absence probe reserved equipment", err)
	}
	lock, err := j.Lock(s.key)
	if err != nil {
		t.Fatal("probe retained process lock", err)
	}
	lock.Close()
}

func TestRecoveryCleanupOriginalPDNAndOldCapability(t *testing.T) {
	ctx := context.Background()
	p, b, j, s := bearerFixture(t)
	if _, err := p.Acquire(ctx, s, BearerOptions{}); err != nil {
		t.Fatal(err)
	}
	p, s = restartBearerPool(t, p, b, j, s)
	owner, err := p.RecoveryCleanup(ctx, s)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	if err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if b.count("disconnect") != 1 || b.count("delete") != 1 || p.HasPendingClaim(s) {
		t.Fatal("original journal not acknowledged")
	}
	lease, err := p.Acquire(ctx, s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(ctx); err != nil || lease.Verify(ctx) != nil || b.count("delete") != 1 {
		t.Fatal("old closer affected successor", err)
	}
	if err := lease.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCleanupCannotStealLiveProcessOrGuessUnknownACK(t *testing.T) {
	ctx := context.Background()
	p, b, j, s := bearerFixture(t)
	b.failOp = "create"
	if _, err := p.Acquire(ctx, s, BearerOptions{}); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	foreign, err := NewBearerPool(b, j, func(context.Context, Selection) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	selected, err := foreign.Select(ctx, s.snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.RecoveryCleanup(ctx, selected); !errors.Is(err, ErrBusy) {
		t.Fatal("stole live process lock", err)
	}
	p, s = restartBearerPool(t, p, b, j, s)
	owner, err := p.RecoveryCleanup(ctx, s)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	if err := owner.Close(ctx); !errors.Is(err, ErrUncertain) {
		t.Fatal("guessed unknown create ACK", err)
	}
	if b.count("disconnect") != 0 || b.count("delete") != 0 || !p.HasPendingClaim(s) {
		t.Fatal("unknown record was erased/mutated")
	}
}

func TestRecoveryCleanupChangedSIMDoesNotDeleteOriginalPDN(t *testing.T) {
	ctx := context.Background()
	p, b, j, s := bearerFixture(t)
	if _, err := p.Acquire(ctx, s, BearerOptions{}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	m := b.modems[s.snapshot.Path]
	m.Binding.ICCID = "8901000000000000099"
	b.modems[s.snapshot.Path] = m
	b.mu.Unlock()
	p, s = restartBearerPool(t, p, b, j, s)
	owner, err := p.RecoveryCleanup(ctx, s)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	if err := owner.Close(ctx); err == nil {
		t.Fatal("replacement SIM authorized original cleanup")
	}
	if b.count("disconnect") != 0 || b.count("delete") != 0 || !p.HasPendingClaim(s) {
		t.Fatal("changed SIM lost ownership fence")
	}
}
