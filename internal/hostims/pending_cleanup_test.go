package hostims

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestPendingBearerCleanupUnknownACKRetainsOnlySelection(t *testing.T) {
	p, b, _, s := bearerFixture(t)
	other, err := p.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	b.failOp = "create"
	if lease, err := p.Acquire(context.Background(), s, BearerOptions{}); lease != nil || !errors.Is(err, ErrUncertain) {
		t.Fatal("failed Create was not uncertain", err)
	}
	owner, err := p.PendingCleanup(s)
	if err != nil || owner == nil || owner.claim.pendingCleanup != owner {
		t.Fatal("failed acquisition was not rooted", err)
	}
	if !p.HasPendingClaim(s) || p.HasPendingClaim(other) {
		t.Fatal("pending claim contaminated another selection")
	}
	if again, err := p.PendingCleanup(s); err != nil || again != owner {
		t.Fatal("cleanup capability was replaced", err)
	}
	if err := owner.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("unknown Create was guessed/adopted", err)
	}
	if err := p.Recover(context.Background(), s); !errors.Is(err, ErrBusy) {
		t.Fatal("public recovery bypassed cleanup owner", err)
	}
	if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatal("new acquisition bypassed cleanup owner", err)
	}
	old := &BearerLease{pool: p, selection: s, claim: owner.claim, token: owner.token}
	if err := old.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("lease bypassed cleanup owner", err)
	}
	b.mu.Lock()
	b.failOp = ""
	b.mu.Unlock()
	lease, err := p.Acquire(context.Background(), other, BearerOptions{})
	if err != nil {
		t.Fatal("one uncertain modem blocked a different modem", err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.count("delete") != 1 || b.count("disconnect") != 1 {
		t.Fatal("unknown selection performed destructive cleanup")
	}
	if !p.HasPendingClaim(s) || p.HasPendingClaim(other) {
		t.Fatal("per-selection fence changed incorrectly")
	}
}

func TestPendingBearerCleanupLateConfirmedACKAndStaleCapability(t *testing.T) {
	p, b, j, s := bearerFixture(t)
	b.blockOp, b.entered, b.release = "create", make(chan struct{}, 1), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(b.release) }) })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := p.Acquire(ctx, s, BearerOptions{}); result <- err }()
	<-b.entered
	cancel()
	if err := <-result; !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	owner, err := p.PendingCleanup(s)
	if err != nil || owner == nil {
		t.Fatal("canceled mutation lost cleanup owner", err)
	}
	if err := owner.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("pending reply allowed cleanup", err)
	}
	release.Do(func() { close(b.release) })
	awaitClaim(t, j, s.key, func(r claimRecord) bool { return r.Pending == "" && r.Path != "" && r.Uncertain })
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal("late confirmed original Create could not be released", err)
	}
	if p.HasPendingClaim(s) || owner.claim.pendingCleanup != nil || b.count("delete") != 1 {
		t.Fatal("cleanup did not ACK/release root")
	}
	b.mu.Lock()
	b.blockOp = ""
	b.mu.Unlock()
	lease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal("old cleanup capability was not idempotent", err)
	}
	if b.count("delete") != 1 || lease.Verify(context.Background()) != nil {
		t.Fatal("old cleanup capability touched a successor PDN")
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPendingBearerCleanupDoesNotStealLiveOwner(t *testing.T) {
	p, b, _, s := bearerFixture(t)
	lease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := p.PendingCleanup(s)
	if err != nil || waiter == nil || !waiter.waitOnly || lease.claim.pendingCleanup != nil {
		t.Fatal("live owner was stolen", err)
	}
	if err := waiter.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("waiter ACKed a live owner", err)
	}
	if b.count("delete") != 0 || lease.Verify(context.Background()) != nil {
		t.Fatal("waiter touched live bearer")
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := waiter.Close(context.Background()); err != nil {
		t.Fatal("released original claim did not end waiter", err)
	}
}
