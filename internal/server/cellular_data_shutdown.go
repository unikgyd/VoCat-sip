package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vocat/internal/device"
)

// CloseCellularData drains user-plane work before device.Stop can discard the
// original WDS client. It preserves durable intent for the next startup and
// never substitutes a newly opened client or a global modem reset for cleanup.
// The caller first cancels background producers; this fence also rejects late
// callbacks and new proxy connections while existing socket owners exit.
func (s *Server) CloseCellularData(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runtime := s.cellularDataRuntime()
	runtime.mu.Lock()
	runtime.closing = true
	if runtime.quiescing == nil {
		runtime.quiescing = make(map[string]bool)
	}
	ids := make(map[string]struct{})
	for id := range runtime.entries {
		ids[id] = struct{}{}
	}
	for id := range runtime.physicalTargets {
		ids[id] = struct{}{}
	}
	for id, workers := range runtime.workers {
		ids[id] = struct{}{}
		for worker := range workers {
			if worker.cancel != nil {
				worker.cancel()
			}
		}
	}
	for transfer := range runtime.transfers {
		ids[transfer.id] = struct{}{}
		transfer.cancel()
	}
	for id := range ids {
		runtime.quiescing[id] = true
	}
	runtime.mu.Unlock()

	// Independent lines drain concurrently, so an unresponsive modem cannot
	// consume every other line's shutdown budget.
	completed := make(chan error, len(ids))
	for id := range ids {
		go func() {
			gate := s.imsModeLock(id)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for !gate.TryLock() {
				select {
				case <-ctx.Done():
					completed <- errors.Join(device.ErrDataCleanupPending, ctx.Err())
					return
				case <-ticker.C:
				}
			}
			defer gate.Unlock()
			targets, err := runtime.quiesce(ctx, id)
			if err == nil {
				controller, capable := s.devices.(ownedNetworkController)
				if len(targets) != 0 && !capable {
					err = device.ErrDataBackendUnavailable
				} else {
					for _, target := range targets {
						err = errors.Join(err, controller.StopOwnedNetwork(ctx, target))
					}
				}
			}
			runtime.finishQuiesce(id, err)
			if err != nil {
				err = fmt.Errorf("stop cellular data for %s: %w", id, err)
			}
			completed <- err
		}()
	}
	var failures []error
	for range ids {
		if err := <-completed; err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
