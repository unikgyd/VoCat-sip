package hostims

import "context"

// RecoveryCleanup loads an existing journal under its equipment/process lock.
// It creates no bearer and grants no usable access. Existing live owners remain
// owned by their original capability; uncertain records remain fenced by Close.
func (p *BearerPool) RecoveryCleanup(ctx context.Context, selection Selection) (*PendingBearerCleanup, error) {
	if p == nil || selection.pool != p || selection.key == "" {
		return nil, ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := p.enter(ctx, selection, false)
	if err == nil {
		// No journal exists. Release the absence probe's process lock without
		// writing a claim or reserving this equipment against other processes.
		c.mu.Lock()
		if c.raw == nil && c.lock != nil {
			_ = c.lock.Close()
			c.lock = nil
		}
		c.busy = false
		c.mu.Unlock()
		return nil, nil
	}
	if selection.pool != p {
		return nil, ErrSelection
	}
	p.mu.Lock()
	c = p.claims[selection.key]
	p.mu.Unlock()
	if c == nil {
		return nil, err
	}
	c.mu.Lock()
	adopted := c.adopted && c.raw != nil && c.lock != nil && !c.busy && !c.storageFailed && c.preparedAccess == nil
	c.mu.Unlock()
	if !adopted {
		return nil, err
	}
	return p.PendingCleanup(selection)
}
