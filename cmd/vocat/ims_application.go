package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"vocat/internal/developer"
	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/linelock"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
	"vocat/internal/vowifi/integration"
)

func configureHostIMSRuntime(ctx context.Context, logger *slog.Logger, database *store.Store,
	devices *device.Manager, developerEnabled bool, incoming func(context.Context, ims.ReceivedCall) error,
) (*durableHostIMSController, func(context.Context) error, error) {
	factory, err := buildHostIMSFactory(ctx, hostIMSFactoryOptions{
		Store: database, Devices: devices,
		IMSConfig: func(ctx context.Context, config store.Device) (ims.Config, error) {
			return applicationIMSConfig(ctx, config, database, logger.With("category", "host_ims", "device_id", config.ID), incoming, store.IMSModeHost)
		},
		// This is application admission, not a substitute for compliance with
		// LICENSE. Preserve existing regional and configured-device limits.
		Authorize: func(ctx context.Context, selection hostims.Selection) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			imsi := strings.TrimSpace(selection.SubscriberIMSI())
			if len(imsi) < 5 || device.IsPlaceholderIMSI(imsi) || device.RegionBlockReason(imsi) != "" {
				return errors.New("Host IMS admission denied")
			}
			configs, err := database.ListDevices(ctx)
			if err != nil {
				return err
			}
			if len(configs) > developer.DeviceLimit(ctx, database, developerEnabled) {
				return errors.New("configured device limit exceeded")
			}
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	manager := newDurableHostIMSController(hostimsruntime.New(hostimsruntime.Options{
		Logger: logger.With("category", "host_ims"), Factory: factory.Factory,
		OnState: func(ctx context.Context, state vowifi.State) error {
			// Never project Host IMS into the Wi-Fi runtime table (or label its
			// cellular bearer as an ePDG tunnel). The API reads its live owner.
			if state.PhoneNumber == "" || state.ICCID == "" {
				return nil
			}
			return (integration.PhoneStore{Store: database, DeviceID: state.DeviceID}).SaveAssociatedNumber(ctx, vowifi.PhoneRecord{
				ICCID: state.ICCID, Number: state.PhoneNumber, Source: state.PhoneNumberSource, UpdatedAt: state.UpdatedAt,
			})
		},
	}), database)
	if observer, ok := factory.(interface {
		Subscription(context.Context, string) (vowifi.SIMIdentity, error)
	}); ok {
		manager.readSubscription = observer.Subscription
	}
	closeAll := func(ctx context.Context) error {
		// Do not close shared DBus underneath a late session/access cleanup.
		if err := manager.Close(ctx); err != nil {
			return err
		}
		return factory.Close(ctx)
	}
	configs, err := database.ListDevices(ctx)
	if err != nil {
		return manager, closeAll, err
	}
	for _, config := range configs {
		if !store.HostIMSConfigured(config) {
			continue
		}
		if err := manager.Ensure(ctx, config.ID); err != nil {
			return manager, closeAll, err
		}
		if store.AccessEnabled(config, store.IMSModeHost) {
			if _, err := manager.RequestEnabled(config.ID, true); err != nil {
				return manager, closeAll, err
			}
		}
	}
	return manager, closeAll, nil
}

// Physical refresh must not perform automatic registration recovery, reboot,
// or RF-off card defaults beneath an explicitly managed IMS line. Region
// enforcement is deliberately separate and always remains applicable.
func explicitIMSOwnsPhysical(ctx context.Context, database *store.Store, devices *device.Manager, physicalID string) bool {
	configs, err := database.ListDevices(ctx)
	if err != nil {
		return true
	} // cannot prove the modem is unowned
	mapper := integration.ATMapper{Store: database, Devices: devices}
	for _, config := range configs {
		if config.IMSMode == "" {
			continue
		}
		if entry, err := mapper.Get(config.ID); err == nil && entry.ID == physicalID {
			return true
		}
	}
	return false
}

const hostIMSShutdownTimeout = 20 * time.Second

// Read the current row only after taking the same per-line lock used by the
// mode-changing API. A stale ListDevices row is never authority for RF/I/O.
func lockIMSLineRead(ctx context.Context, database *store.Store, id string) (store.Device, func(), error) {
	gate := linelock.For(id)
	gate.RLock()
	config, err := database.Device(ctx, id)
	if err != nil {
		gate.RUnlock()
		return store.Device{}, nil, err
	}
	return config, gate.RUnlock, nil
}

// Return only logical IDs that may address this physical target, not every
// line in the fleet. Port hints may add an alias to the lock set, but can never
// establish Host IMS ownership: that always requires the factory's strong map.
func legacyPhysicalGateIDs(configs []store.Device, entries []device.Device, physicalID string) ([]string, bool) {
	return legacyPhysicalGateIDsWith(configs, entries, physicalID, defaultHostIMSPhysicalGuard())
}
func legacyPhysicalGateIDsWith(configs []store.Device, entries []device.Device, physicalID string, guard hostIMSPhysicalGuard) ([]string, bool) {
	if guard.resolve == nil || guard.disjoint == nil {
		return nil, false
	}
	var target *device.Device
	for i := range entries {
		if entries[i].ID == physicalID && entries[i].Discovered {
			if target != nil {
				return nil, false
			}
			target = &entries[i]
		}
	}
	if target == nil {
		return nil, false
	}
	ids := make(map[string]bool)
	for _, config := range configs {
		strongID, strongErr := guard.resolve(config, entries)
		if store.HostIMSConfigured(config) {
			if strongErr == nil && strongID == physicalID || strongErr != nil && !guard.disjoint(config, entries, physicalID) {
				return nil, false
			}
			continue
		}
		candidate := target.Candidate
		match := strongErr == nil && strongID == physicalID || config.ID == physicalID ||
			config.USBPath != "" && config.USBPath == candidate.USBPath ||
			config.ModemIMEI != "" && target.Snapshot != nil && config.ModemIMEI == target.Snapshot.IMEI ||
			config.ATPort != "" && (config.ATPort == candidate.ATPort.Path || config.ATPort == candidate.ATPort.OpenPath()) ||
			config.ControlDevice != "" && (config.ControlDevice == candidate.QMIControl || config.ControlDevice == candidate.ATPort.OpenPath() || config.ControlDevice == candidate.ReaderName)
		if match {
			ids[config.ID] = true
		}
	}
	// Unconfigured inventory is handled before provisioning; once configured,
	// an unresolvable physical target must not be probed by guessing.
	if len(ids) == 0 && len(configs) != 0 {
		return nil, false
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, true
}

func lockLegacyPhysicalRead(ctx context.Context, database *store.Store, inventory hostIMSDeviceInventory, physicalID string) (func(), bool) {
	configs, err := database.ListDevices(ctx)
	if err != nil {
		return nil, false
	}
	ids, ok := legacyPhysicalGateIDs(configs, inventory.List(), physicalID)
	if !ok {
		return nil, false
	}
	for _, id := range ids {
		linelock.For(id).RLock()
	}
	unlock := func() {
		for i := len(ids) - 1; i >= 0; i-- {
			linelock.For(ids[i]).RUnlock()
		}
	}
	configs, err = database.ListDevices(ctx)
	current, ok := legacyPhysicalGateIDs(configs, inventory.List(), physicalID)
	if err != nil || ctx.Err() != nil || !ok || len(current) != len(ids) || strings.Join(current, "\x00") != strings.Join(ids, "\x00") {
		unlock()
		return nil, false
	}
	return unlock, true
}

func lockLegacyConfiguredRead(ctx context.Context, database *store.Store, devices *device.Manager, id string) (store.Device, device.Device, func(), bool) {
	mapper := integration.ATMapper{Store: database, Devices: devices}
	before, err := mapper.Get(id) // metadata only; never authority for I/O
	if err != nil {
		return store.Device{}, device.Device{}, nil, false
	}
	unlock, ok := lockLegacyPhysicalRead(ctx, database, devices, before.ID)
	if !ok {
		return store.Device{}, device.Device{}, nil, false
	}
	config, err := database.Device(ctx, id)
	current, mapErr := mapper.Get(id)
	if err != nil || mapErr != nil || current.ID != before.ID || store.HostIMSConfigured(config) {
		unlock()
		return store.Device{}, device.Device{}, nil, false
	}
	return config, current, unlock, true
}

func hostIMSOwnsPhysical(ctx context.Context, database *store.Store, devices *device.Manager, physicalID string) bool {
	return hostIMSOwnsPhysicalWith(ctx, database, devices.List(), physicalID, defaultHostIMSPhysicalGuard())
}
func hostIMSOwnsPhysicalWith(ctx context.Context, database *store.Store, entries []device.Device, physicalID string, guard hostIMSPhysicalGuard) bool {
	if guard.resolve == nil || guard.disjoint == nil {
		return true
	}
	configs, err := database.ListDevices(ctx)
	if err != nil {
		return true
	}
	for _, config := range configs {
		if !store.HostIMSConfigured(config) {
			continue
		}
		selectedID, err := guard.resolve(config, entries)
		if err != nil {
			if !guard.disjoint(config, entries, physicalID) {
				return true
			}
			continue
		} // Unresolved owner: do not probe another port by guessing.
		if selectedID == physicalID {
			return true
		}
	}
	return false
}
