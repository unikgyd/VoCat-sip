package server

import (
	"context"
	"errors"
	"time"
)

var errLineBusy = errors.New("a line operation is in progress")
var errTaskLeaseExpired = errors.New("the automatic task no longer owns this line")

type automaticTaskLeaseKey struct{}
type automaticTaskLease struct {
	server         *Server
	id             string
	finishRecovery func() error
}

func (s *Server) beginAutomaticTask(ctx context.Context, id string) (context.Context, func() error, error) {
	return s.beginAutomaticTaskRecovery(ctx, id, "")
}

func (s *Server) beginAutomaticTaskRecovery(ctx context.Context, id, token string) (context.Context, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	gate := s.imsModeLock(id)
	if !gate.TryLock() {
		return nil, nil, errLineBusy
	}
	if s.store != nil {
		if token == "" {
			if err := s.store.RequireAutomaticTaskRecovered(ctx, id); err != nil {
				gate.Unlock()
				return nil, nil, err
			}
		} else {
			pending, err := s.store.AutomaticTaskRecovery(ctx, id)
			if err != nil || pending.Token != token {
				gate.Unlock()
				return nil, nil, errors.Join(err, errTaskLeaseExpired)
			}
		}
	}
	lease := &automaticTaskLease{server: s, id: id}
	s.automaticTaskOwners.Store(id, lease)
	return context.WithValue(ctx, automaticTaskLeaseKey{}, lease), func() error {
		runtime := s.cellularDataRuntime()
		runtime.mu.Lock()
		wasQuiescing := runtime.quiescing[id]
		runtime.quiescing[id] = true
		done := runtime.cancelTransfersLocked(id, nil)
		for worker := range runtime.workers[id] {
			if worker.cancel != nil {
				worker.cancel()
			}
			done = append(done, worker.done)
		}
		runtime.mu.Unlock()
		finish := func() error {
			var recoveryErr error
			var publicIPPhysical string
			var publicIPRevision uint64
			if lease.finishRecovery != nil {
				recoveryErr = lease.finishRecovery()
			}
			runtime.mu.Lock()
			if entry := runtime.entries[id]; entry != nil && entry.taskLease == lease {
				entry.taskLease = nil
				if entry.status.Connected && entry.status.DesiredEnabled && entry.status.Phase == "connected" {
					publicIPPhysical, publicIPRevision = entry.physicalID, entry.status.Revision
				}
			}
			if !wasQuiescing {
				delete(runtime.quiescing, id)
			}
			runtime.mu.Unlock()
			s.automaticTaskOwners.Delete(id)
			gate.Unlock()
			if recoveryErr == nil && publicIPPhysical != "" {
				// The transition callback ran while this writer/journal still
				// fenced ordinary traffic. Detect the restored exit after release.
				s.schedulePublicIPDetection(id, publicIPPhysical, publicIPRevision)
			}
			return recoveryErr
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		for i, stopped := range done {
			select {
			case <-stopped:
			case <-cleanup.Done():
				// Keep the writer capability until late child I/O actually exits.
				go func(remaining []<-chan struct{}) {
					for _, child := range remaining {
						<-child
					}
					_ = finish()
				}(done[i:])
				return errors.New("automatic task data workers are still releasing the line")
			}
		}
		return finish()
	}, nil
}

// Task children borrow their own writer capability, never a process-wide
// bypass. An expired context cannot operate after restoration or a new task.
// Other callers fail promptly while the selected line is exclusively owned.
func (s *Server) acquireLineRead(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lease, ok := ctx.Value(automaticTaskLeaseKey{}).(*automaticTaskLease); ok {
		if lease.server != s || lease.id != id {
			return nil, errTaskLeaseExpired
		}
		if owner, active := s.automaticTaskOwners.Load(id); active && owner == lease {
			return func() {}, nil
		}
		return nil, errTaskLeaseExpired
	}
	gate := s.imsModeLock(id)
	if !gate.TryRLock() {
		return nil, errLineBusy
	}
	if s.store != nil {
		if err := s.store.RequireAutomaticTaskRecovered(ctx, id); err != nil {
			gate.RUnlock()
			return nil, err
		}
	}
	return gate.RUnlock, nil
}

func (s *Server) acquireLineWrite(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gate := s.imsModeLock(id)
	if !gate.TryLock() {
		return nil, errLineBusy
	}
	if s.store != nil {
		if err := s.store.RequireAutomaticTaskRecovered(ctx, id); err != nil {
			gate.Unlock()
			return nil, err
		}
	}
	return gate.Unlock, nil
}
