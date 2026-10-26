package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func (s *Server) observedTelephonySubscription(ctx context.Context, config store.Device) (identity vowifi.SIMIdentity, err error) {
	entry, physical, present := s.physicalForConfig(config)
	defer func() {
		observation := subscriptionObservation{identity: identity, physical: physical, generation: entry.Candidate.USBGeneration, available: present && err == nil, at: time.Now()}
		s.subscriptionObservations.Store(config.ID, observation)
		if store.HostIMSConfigured(config) && observation.available {
			s.subscriptionTransitions.Store(config.ID, observation)
		}
	}()
	if !present {
		return vowifi.SIMIdentity{}, nil
	}
	if s.awaitingLegacySubscription(config, &entry) {
		return vowifi.SIMIdentity{}, device.ErrSIMObservation
	}
	if s.readSubscription != nil {
		return s.readSubscription(ctx, config.ID)
	}
	iccid, imsi, err := device.SnapshotSubscription(entry.Snapshot)
	return vowifi.SIMIdentity{ICCID: iccid, IMSI: imsi}, err
}

// ReconcileTelephonyPolicies follows physical insertion and removal. It never
// starts a replacement SIM while the old owners still have unconfirmed cleanup.
// The caller owns the lifetime context, including server shutdown.
func (s *Server) ReconcileTelephonyPolicies(ctx context.Context) {
	observed := make(map[string]string)
	reconcile := func() {
		configs, err := s.store.ListDevices(ctx)
		if err != nil {
			return
		}
		type job struct{ id, previous string }
		type result struct {
			id, key string
			changed bool
		}
		jobs := make(chan job)
		results := make(chan result, len(configs))
		var workers sync.WaitGroup
		for i := 0; i < min(4, len(configs)); i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for task := range jobs {
					key, changed := s.reconcileTelephonyLine(ctx, task.id, task.previous)
					results <- result{id: task.id, key: key, changed: changed}
				}
			}()
		}
		for _, config := range configs {
			if config.Telephony == nil {
				continue
			}
			select {
			case jobs <- job{id: config.ID, previous: observed[config.ID]}:
			case <-ctx.Done():
			}
		}
		close(jobs)
		workers.Wait()
		close(results)
		for result := range results {
			if result.changed {
				observed[result.id] = result.key
			}
		}
		for id := range observed {
			found := false
			for _, config := range configs {
				if config.ID == id {
					found = true
					break
				}
			}
			if !found {
				delete(observed, id)
				s.subscriptionObservations.Delete(id)
				s.subscriptionTransitions.Delete(id)
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func (s *Server) reconcileTelephonyLine(ctx context.Context, id, previous string) (key string, changed bool) {
	gate := s.imsModeLock(id)
	if !gate.TryLock() {
		return "", false
	}
	defer gate.Unlock()
	current, err := s.store.Device(ctx, id)
	if err != nil || current.Telephony == nil {
		return "", false
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	identity, err := s.observedTelephonySubscription(operation, current)
	entry, physical, present := s.physicalForConfig(current)
	if err != nil {
		// A confirmed new USB generation invalidates old owners even while MM
		// is still enumerating its SIM. Never bind a new policy from that error.
		parts := strings.Split(previous, "\x00")
		if len(parts) == 4 && (parts[2] != physical || parts[3] != entry.Candidate.USBGeneration) {
			if _, _, stopErr := s.bindTelephonySubscription(operation, current, vowifi.SIMIdentity{}); stopErr != nil {
				s.telephonyReconcileError(id, stopErr)
				return "", false
			}
			return "missing", true
		}
		for _, access := range s.telephonyControllerAccesses(current) {
			if _, pending := s.telephonyCleanup.Load(telephonyCleanupKey{id, access}); pending {
				if stopErr := s.stopTelephonyController(operation, id, access); stopErr != nil {
					s.telephonyReconcileError(id, stopErr)
				}
			}
		}
		return "", false
	}
	key = identity.ICCID + "\x00" + identity.IMSI + "\x00" + physical + "\x00" + entry.Candidate.USBGeneration
	if !present || identity.ICCID == "" {
		key = "missing"
	}
	if key == "missing" && previous == key {
		return key, false
	}
	if key != previous && !(previous == "" && identity.ICCID == current.Telephony.ICCID && key != "missing") {
		var applied *store.Device
		applied, physical, err = s.bindTelephonySubscription(operation, current, identity)
		if err != nil {
			s.telephonyReconcileError(id, err)
			return key, false
		}
		if applied == nil {
			return key, true
		}
		current = *applied
	}
	// A failed cellular start must not repeatedly stop an already queued Wi-Fi
	// owner. Runtime intent distinguishes backoff from a missing enable request.
	err = s.startTelephonyAccess(operation, current, physical, true)
	if err != nil {
		s.telephonyReconcileError(id, err)
	}
	return key, true
}

func (s *Server) telephonyReconcileError(id string, err error) {
	if s.logger != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("subscription access reconciliation deferred", "device_id", id, "error", err)
	}
}

// Caller holds the line writer fence. Only confirmed target identity may be
// persisted; an uncertain or missing SIM keeps the old intent saved and stopped.
func (s *Server) reconcileTelephonySubscription(ctx context.Context, config store.Device, identity vowifi.SIMIdentity) error {
	applied, physical, err := s.bindTelephonySubscription(ctx, config, identity)
	if err != nil || applied == nil {
		return err
	}
	return s.startTelephonyAccess(ctx, *applied, physical)
}

func (s *Server) bindTelephonySubscription(ctx context.Context, config store.Device, identity vowifi.SIMIdentity) (*store.Device, string, error) {
	entry, physical, present := s.physicalForConfig(config)
	controllers, err := s.imsControllers(config)
	if err != nil {
		return nil, physical, err
	}
	for _, access := range s.telephonyControllerAccesses(config) {
		if err := s.stopTelephonyController(ctx, config.ID, access); err != nil {
			return nil, physical, err
		}
	}
	for _, controller := range controllers {
		defer endIMSMaintenance(controller, config.ID)
	}
	if s.moduleCalls != nil && s.moduleCalls.Busy(config.ID) {
		// Its lease observes the changed ICCID/generation and releases its own
		// audio reservation. Never issue a new SIM's CLCC hangup here.
		return nil, physical, errors.New("previous module call is still releasing its reservation")
	}
	if !present || identity.ICCID == "" {
		return nil, physical, nil
	}
	if identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) || device.RegionBlockReason(identity.IMSI) != "" {
		return nil, physical, errors.New("the replacement subscription is not eligible for access")
	}
	card, err := s.store.CardPolicy(ctx, identity.ICCID)
	if errors.Is(err, store.ErrNotFound) {
		card = defaultCardPolicy(identity.ICCID)
	} else if err != nil {
		return nil, physical, err
	}
	policy, err := s.store.TelephonyForCard(ctx, config, card)
	if err != nil {
		return nil, physical, err
	}
	again, err := s.observedTelephonySubscription(ctx, config)
	latest, latestPhysical, stillPresent := s.physicalForConfig(config)
	if err != nil || !stillPresent || physical != latestPhysical || latest.Candidate.USBGeneration != entry.Candidate.USBGeneration ||
		again.ICCID != identity.ICCID || again.IMSI != identity.IMSI {
		return nil, physical, errors.New("subscription changed during access cleanup")
	}
	card.VoWiFiEnabled = false
	config.NetworkEnabled = false // A previous SIM's data intent cannot migrate.
	config.APN = card.APN
	if err := s.store.SaveTelephonyPolicy(ctx, config, policy, card); err != nil {
		return nil, physical, err
	}
	config.Telephony = &policy
	for _, controller := range controllers {
		endIMSMaintenance(controller, config.ID)
	}
	return &config, physical, nil
}

func (s *Server) startTelephonyAccess(ctx context.Context, config store.Device, physical string, startup ...bool) error {
	var failures []error
	for _, access := range []string{store.IMSModeHost, store.IMSModeVoWiFi} {
		if _, pending := s.telephonyCleanup.Load(telephonyCleanupKey{config.ID, access}); !pending {
			continue
		}
		if err := s.stopTelephonyController(ctx, config.ID, access); err != nil {
			failures = append(failures, err)
			continue
		}
		endIMSMaintenance(s.controllerForMode(access), config.ID)
	}
	if config.Telephony != nil && !config.Telephony.AutoConnectCellular && !config.Telephony.AutoConnectWiFi {
		return errors.Join(failures...)
	}
	identity, err := s.observedTelephonySubscription(ctx, config)
	if err != nil || config.Telephony == nil || identity.ICCID != config.Telephony.ICCID || identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) || device.RegionBlockReason(identity.IMSI) != "" {
		return errors.New("the active subscription is not eligible for access")
	}
	if store.AccessEnabled(config, store.IMSModeModule) {
		entry, live, found := s.physicalForConfig(config)
		if !found || live != physical {
			failures = append(failures, errors.New("cellular physical identity changed"))
		} else if entry.Snapshot == nil || !entry.Snapshot.ModeKnown || entry.Snapshot.FlightMode || entry.Snapshot.RadioOff {
			if err := s.requireModuleCallsIdle(ctx, config); err != nil {
				failures = append(failures, err)
			} else if _, err := s.devices.SetFlight(ctx, physical, false); err != nil {
				failures = append(failures, fmt.Errorf("activate module radio: %w", err))
			}
		}
	}
	for _, access := range []string{store.IMSModeHost, store.IMSModeVoWiFi} {
		controller := s.controllerForMode(access)
		if _, pending := s.telephonyCleanup.Load(telephonyCleanupKey{config.ID, access}); pending {
			continue
		}
		if !store.AccessEnabled(config, access) {
			continue
		}
		if controller == nil {
			failures = append(failures, fmt.Errorf("%s provider unavailable", access))
			continue
		}
		if desired, ok := controller.(interface{ DesiredEnabled(string) (bool, error) }); ok {
			queued, err := desired.DesiredEnabled(config.ID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if queued {
				continue
			}
		} else if state, err := controller.State(config.ID); err == nil && state.Enabled {
			continue
		}
		if access == store.IMSModeHost && (config.NetworkEnabled || s.legacyDataOwnerBusy(config.ID)) {
			failures = append(failures, errors.New("legacy data cleanup must finish before Host IMS"))
			continue
		}
		if access == store.IMSModeVoWiFi && len(startup) == 1 && startup[0] && config.DeviceType == store.DeviceTypeWiFi410 && time.Now().Before(s.telephonyStartupNotBefore) {
			continue
		}
		endIMSMaintenance(controller, config.ID)
		if _, err := controller.RequestEnabled(config.ID, true); err != nil {
			failures = append(failures, fmt.Errorf("queue %s access: %w", access, err))
		}
	}
	return errors.Join(failures...)
}

type telephonyCleanupKey struct{ device, access string }

func (s *Server) telephonyControllerAccesses(config store.Device) []string {
	var result []string
	if s.wifiController() != nil {
		result = append(result, store.IMSModeVoWiFi)
	}
	if store.HostIMSConfigured(config) && s.hostIMS != nil {
		result = append(result, store.IMSModeHost)
	}
	return result
}

func (s *Server) stopTelephonyController(ctx context.Context, id, access string) error {
	key := telephonyCleanupKey{id, access}
	s.telephonyCleanup.Store(key, true)
	controller := s.controllerForMode(access)
	if controller == nil {
		return errIMSUnavailable
	}
	if err := stopIMSController(ctx, controller, id); err != nil {
		return err
	}
	s.telephonyCleanup.Delete(key)
	return nil
}
