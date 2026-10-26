package hostims

import (
	"context"
	"fmt"
	"io"
	"time"
)

// PendingBearerCleanup is not a bearer or a proof of connectivity. It only
// retains this exact failed acquisition until its write-ahead cleanup is ACKed.
// An unknown mutation reply is never reconstructed or adopted by APN.
type PendingBearerCleanup struct {
	pool      *BearerPool
	selection Selection
	claim     *bearerClaim
	token     string
	waitOnly  bool
}

func (*PendingBearerCleanup) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private pending cellular cleanup>")
}

func pendingClaimLocked(c *bearerClaim) bool {
	return c.busy || c.raw != nil || c.lock != nil || c.storageFailed || c.preparedAccess != nil || c.pendingCleanup != nil || len(c.retained) != 0 || len(c.networkHolds) != 0
}

// HasPendingClaim concerns only this equipment's claim, never another line.
// A changed live SIM cannot erase an older claim on that same equipment key.
func (p *BearerPool) HasPendingClaim(s Selection) bool {
	if p == nil || s.pool != p || s.key == "" {
		return false
	}
	p.mu.Lock()
	c := p.claims[s.key]
	p.mu.Unlock()
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return pendingClaimLocked(c)
}

// PendingCleanup transfers an uncertain/adopted failed acquisition to a rooted
// cleanup-only capability. An already live/busy owner is only observed, never
// stolen; its waiter cannot clean anything and ACKs only after that claim ends.
// Construction performs no modem or journal I/O.
func (p *BearerPool) PendingCleanup(s Selection) (*PendingBearerCleanup, error) {
	if p == nil || s.pool != p || s.key == "" {
		return nil, ErrSelection
	}
	p.mu.Lock()
	c := p.claims[s.key]
	p.mu.Unlock()
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !pendingClaimLocked(c) {
		return nil, nil
	}
	if c.pendingCleanup != nil {
		return c.pendingCleanup, nil
	}
	a := &PendingBearerCleanup{pool: p, selection: s, claim: c, token: c.record.Token}
	if c.preparedAccess != nil || c.busy || c.raw != nil && !c.record.Uncertain && !c.adopted && c.record.Pending == "" && !c.storageFailed {
		a.waitOnly = true
		return a, nil
	}
	c.pendingCleanup = a
	return a, nil
}

func (a *PendingBearerCleanup) Close(ctx context.Context) error {
	if a == nil || a.pool == nil || a.claim == nil {
		return ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c := a.claim
	c.mu.Lock()
	if a.waitOnly {
		pending := pendingClaimLocked(c)
		c.mu.Unlock()
		if pending {
			return ErrBusy
		}
		return nil
	}
	// A stale/repeated capability never touches a successor claim, even if the
	// same equipment was acquired again after this capability ACKed cleanup.
	if c.pendingCleanup != a {
		c.mu.Unlock()
		return nil
	}
	if c.busy || c.preparedAccess != nil {
		c.mu.Unlock()
		return ErrBusy
	}
	if c.storageFailed {
		c.mu.Unlock()
		return ErrJournal
	}
	if c.record.Token != a.token {
		c.mu.Unlock()
		return ErrChanged
	}
	if c.record.Pending != "" || c.record.SIMPending || c.record.SIMUncertain || c.record.RuntimePending || c.record.RuntimeUncertain || c.record.Network != nil || len(c.retained) != 0 || len(c.networkHolds) != 0 {
		c.mu.Unlock()
		return ErrUncertain
	}
	c.busy = true
	c.mu.Unlock()
	defer a.pool.finish(c)
	cleanup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The same strict known-object cleanup as Recover. It verifies the original
	// daemon/hardware/SIM binding before each confirmed PDN mutation.
	if err := a.pool.cleanup(cleanup, a.selection, c); err != nil {
		a.pool.uncertain(a.selection.key, c)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingCleanup != a || c.raw != nil || c.lock != nil || c.storageFailed {
		return ErrUncertain
	}
	c.pendingCleanup = nil
	return nil
}
