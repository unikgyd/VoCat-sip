//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"vocat/internal/device"
)

// The original owner observes its own retained transport. A main-process SIM
// refresh can fail before it sends another RPC after proxy loss; recovery must
// not depend on that refresh admitting another Set request.
func (d *dataOwnerDriver) checkRecoveryTransport(ctx context.Context) {
	probe, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if d.refresh(probe) != nil {
		return
	}
	if _, err := d.manager.NetworkStatus(probe, d.open.Candidate.ID); err != nil && probe.Err() == nil {
		_, _ = d.manager.RecoverOwnedQMITransport(probe, d.open.Candidate.ID, d.recoveryPort)
	}
}

type dataOwnerRecoveryWatch struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *dataOwnerRecoveryWatch) start(interval time.Duration, check func(context.Context)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.cancel, w.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() == nil {
					check(ctx)
				}
			}
		}
	}()
}

// Cancellation is joined before the original driver's stop/close path. A
// timed-out join keeps that capability retained; it cannot authorize handover.
func (w *dataOwnerRecoveryWatch) stop(ctx context.Context) error {
	w.mu.Lock()
	if w.cancel == nil {
		w.mu.Unlock()
		return nil
	}
	w.cancel()
	done := w.done
	w.mu.Unlock()
	select {
	case <-done:
		w.mu.Lock()
		if w.done == done {
			w.cancel, w.done = nil, nil
		}
		w.mu.Unlock()
		return nil
	case <-ctx.Done():
		return errors.Join(device.ErrDataCleanupPending, ctx.Err())
	}
}
