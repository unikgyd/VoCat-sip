package server

import (
	"context"
	"errors"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type cellularDataIdentityKey struct{}
type cellularDataIdentity struct{ config, physical, iccid string }

type ownedNetworkController interface {
	StopOwnedNetwork(context.Context, string) error
}

// Keep every live worker, including workers whose entry was superseded by
// invalidate. Replacing a presentation record is not a hardware cleanup ACK.
func (r *cellularDataRuntime) quiesce(ctx context.Context, id string) ([]string, error) {
	r.mu.Lock()
	if r.quiescing == nil {
		r.quiescing = make(map[string]bool)
	}
	r.quiescing[id] = true
	done := r.cancelTransfersLocked(id, nil)
	for worker := range r.workers[id] {
		if worker.cancel != nil {
			worker.cancel()
		}
		done = append(done, worker.done)
	}
	var targets []string
	for target := range r.physicalTargets[id] {
		if target != "" {
			targets = append(targets, target)
		}
	}
	if entry := r.entries[id]; entry != nil {
		if entry.physicalID != "" && len(targets) == 0 {
			targets = append(targets, entry.physicalID)
		}
		if entry.cancel != nil {
			entry.cancel()
		}
		r.beginClosingLocked(entry)
		entry.request.Enabled = false
		entry.status.DesiredEnabled = false
		entry.status.Connected = false
		entry.status.Revision++
		entry.status.Phase = "stopping"
		entry.status.MaintenancePhase = "data_cleanup_pending"
		entry.status.UpdatedAt = time.Now().UTC()
		r.signalLocked(entry)
	}
	r.mu.Unlock()
	for _, completed := range done {
		select {
		case <-completed:
		case <-ctx.Done():
			r.finishQuiesce(id, ctx.Err())
			return targets, ctx.Err()
		}
	}
	return targets, nil
}

func (r *cellularDataRuntime) finishQuiesce(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[id]
	if entry == nil {
		entry = &cellularDataRuntimeEntry{changed: make(chan struct{})}
		r.entries[id] = entry
	}
	entry.request.Enabled = false
	entry.status.DesiredEnabled = false
	entry.status.Connected = false
	entry.status.UpdatedAt = time.Now().UTC()
	if err != nil {
		entry.status.Phase = "failed"
		entry.status.LastError = err.Error()
		entry.status.MaintenancePhase = "data_cleanup_pending"
	} else {
		r.closeSessionLocked(entry)
		entry.status.Phase = "disabled"
		entry.status.LastError = ""
		// Keep admission fenced until the caller persists the target policy.
		entry.status.MaintenancePhase = "data_cleanup_pending"
		delete(r.physicalTargets, id)
	}
	r.signalLocked(entry)
}

func (r *cellularDataRuntime) resume(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.quiescing, id)
	if entry := r.entries[id]; entry != nil && entry.status.Phase == "disabled" {
		entry.status.MaintenancePhase = ""
		r.signalLocked(entry)
	}
}

// Caller retains its lifecycle fence. This deliberately invokes the opaque
// owned-session cleanup capability directly after joining the data workers;
// queuing generic SetNetwork(false) under a writer would wait for itself.
func (s *Server) quiesceOwnedData(ctx context.Context, config store.Device) error {
	runtime := s.cellularDataRuntime()
	targets, err := runtime.quiesce(ctx, config.ID)
	if err != nil {
		return err
	}
	s.clearPublicIP(config.ID)
	_, physical, _ := s.physicalForConfig(config)
	if physical != "" {
		found := false
		for _, target := range targets {
			found = found || target == physical
		}
		if !found {
			targets = append(targets, physical)
		}
	}
	controller, capable := s.devices.(ownedNetworkController)
	if len(targets) != 0 && !capable {
		err = device.ErrDataBackendUnavailable
	} else {
		var failures []error
		for _, target := range targets {
			if stopErr := controller.StopOwnedNetwork(ctx, target); stopErr != nil {
				failures = append(failures, stopErr)
			}
		}
		err = errors.Join(failures...)
	}
	runtime.finishQuiesce(config.ID, err)
	return err
}

func (s *Server) startTelephonyData(ctx context.Context, config store.Device, physical string, identity vowifi.SIMIdentity) error {
	if !config.NetworkEnabled || store.HostIMSConfigured(config) || store.AccessEnabled(config, store.IMSModeVoWiFi) && store.WiFiExclusive(config) {
		return nil
	}
	entry, current, present := s.physicalForConfig(config)
	if !present || current != physical || entry.Snapshot == nil || config.Telephony == nil || identity.ICCID != config.Telephony.ICCID {
		return device.ErrSIMObservation
	}
	runtime := s.cellularDataRuntime()
	runtime.mu.Lock()
	if runtime.closing || runtime.quiescing[config.ID] {
		runtime.mu.Unlock()
		return device.ErrDataCleanupPending
	}
	existing := runtime.entries[config.ID]
	alreadyQueued := existing != nil && existing.physicalID == physical && existing.identity == identity.ICCID && existing.status.DesiredEnabled &&
		(existing.running || existing.status.Connected)
	runtime.mu.Unlock()
	if !alreadyQueued {
		request := s.cellularNetworkRequest(ctx, config, entry.Snapshot)
		request.Enabled = true
		runtime.requestWithIdentityContext(ctx, config.ID, physical, request, identity.ICCID)
	}
	return nil
}
