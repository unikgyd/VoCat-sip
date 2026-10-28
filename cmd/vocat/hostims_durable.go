package main

import (
	"context"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// The runtime manager can acknowledge its in-memory owners, but cannot infer
// that a previous process left no private durable claim. This wrapper adds a
// presence fence before mode switches/deletion release ownership. A factory
// recovery callback may clean a confirmed previous-process journal first.
type durableHostIMSController struct {
	*hostimsruntime.Manager
	database            *store.Store
	readSubscription    func(context.Context, string) (vowifi.SIMIdentity, error)
	observeRadio        func(context.Context, string) (device.Snapshot, error)
	reconcileInhibition func(context.Context) error
	recoverPending      func(context.Context, string) error
}

func (c *durableHostIMSController) Subscription(ctx context.Context, id string) (vowifi.SIMIdentity, error) {
	if c == nil || c.readSubscription == nil {
		return vowifi.SIMIdentity{}, errHostIMSFactory
	}
	return c.readSubscription(ctx, id)
}

func (c *durableHostIMSController) ObserveRadio(ctx context.Context, id string) (device.Snapshot, error) {
	if c == nil || c.observeRadio == nil {
		return device.Snapshot{}, errHostIMSFactory
	}
	return c.observeRadio(ctx, id)
}

func (c *durableHostIMSController) ReconcileDeviceInhibition(ctx context.Context) error {
	if c == nil || c.reconcileInhibition == nil {
		return nil
	}
	return c.reconcileInhibition(ctx)
}

func newDurableHostIMSController(manager *hostimsruntime.Manager, database *store.Store) *durableHostIMSController {
	return &durableHostIMSController{Manager: manager, database: database}
}

func (c *durableHostIMSController) StopDevice(ctx context.Context, id string) error {
	if c == nil || c.Manager == nil || c.database == nil {
		return hostims.ErrUncertain
	}
	if err := c.Manager.StopDevice(ctx, id); err != nil {
		return err
	}
	config, err := c.database.Device(ctx, id)
	if err != nil {
		return hostims.ErrUncertain
	}
	validEquipment := hostIMSDigits(config.ModemIMEI, 15, 15) && config.ModemIMEI != "000000000000000"
	if !validEquipment {
		// A warm inventory can acquire using its live IMEI plus saved USB even
		// before ModemIMEI is persisted. Missing saved IMEI is not "never owned".
		// Only an empty durable namespace proves no leaked resources here. A
		// present row cannot be attributed safely; save a reliable IMEI to regain
		// the normal independent per-equipment check instead of guessing.
		pending, err := (hostims.SQLiteClaimJournal{Database: c.database}).HasAnyClaim(ctx)
		if err != nil || pending {
			return hostims.ErrUncertain
		}
		return nil
	}
	pending, err := (hostims.SQLiteClaimJournal{Database: c.database}).HasEquipmentClaim(ctx, config.ModemIMEI)
	if err != nil {
		return hostims.ErrUncertain
	}
	if pending && c.recoverPending != nil {
		if err := c.recoverPending(ctx, id); err != nil {
			return err
		}
		pending, err = (hostims.SQLiteClaimJournal{Database: c.database}).HasEquipmentClaim(ctx, config.ModemIMEI)
	}
	if err != nil || pending {
		return hostims.ErrUncertain
	}
	return nil
}
