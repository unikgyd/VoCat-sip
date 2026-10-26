//go:build linux

package main

import (
	"context"
	"strings"

	"vocat/internal/hostims"
	"vocat/internal/store"
)

// CleanupPending uses metadata selection and the private journal only. It
// neither enables RF, opens UIM/WDS, creates a PDN nor submits IMS registration.
func (f *linuxHostIMSFactory) CleanupPending(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	f.mu.Lock()
	if f.closed || f.closing {
		f.mu.Unlock()
		return errHostIMSFactory
	}
	f.preparing++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.preparing--; f.mu.Unlock() }()
	config, err := f.opts.Store.Device(ctx, strings.TrimSpace(id))
	if err != nil || !store.HostIMSConfigured(config) {
		return hostims.ErrDenied
	}
	entries := hostIMSMetadataInventory(config, f.opts.Devices.List())
	target, err := hostIMSPhysicalTarget(config, entries)
	if err != nil {
		return err
	}
	target.ICCID, target.IMSI = "", ""
	pool, err := f.sharedPool(ctx)
	if err != nil {
		return err
	}
	selection, err := pool.SelectTarget(ctx, target)
	if err != nil {
		return err
	}
	_, err = f.recoverSelected(ctx, config.ID, pool, selection)
	return err
}

func (f *linuxHostIMSFactory) recoverSelected(ctx context.Context, id string, pool *hostims.BearerPool, selection hostims.Selection) (*factoryHostIMSAccess, error) {
	f.mu.Lock()
	owner := f.recoveries[id]
	f.mu.Unlock()
	if owner == nil {
		pending, err := pool.RecoveryCleanup(ctx, selection)
		if err != nil {
			return nil, err
		}
		if pending == nil {
			return nil, nil
		}
		candidate := &factoryHostIMSAccess{lease: pending, factory: f, recoveryID: id}
		f.mu.Lock()
		if f.recoveries == nil {
			f.recoveries = make(map[string]*factoryHostIMSAccess)
		}
		owner = f.recoveries[id]
		if owner == nil {
			owner = candidate
			f.recoveries[id] = owner
			if f.held == nil {
				f.held = make(map[*factoryHostIMSAccess]struct{})
			}
			f.held[owner] = struct{}{}
		}
		f.mu.Unlock()
	}
	if err := owner.Close(ctx); err != nil {
		return owner, err
	}
	return nil, nil
}
