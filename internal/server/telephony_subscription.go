package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func (s *Server) observedTelephonySubscription(ctx context.Context, config store.Device) (vowifi.SIMIdentity, error) {
	if s.readSubscription != nil {
		return s.readSubscription(ctx, config.ID)
	}
	entry, _, present := s.physicalForConfig(config)
	if !present || entry.Snapshot == nil {
		return vowifi.SIMIdentity{}, nil
	}
	return vowifi.SIMIdentity{ICCID: strings.TrimSpace(entry.Snapshot.ICCID), IMSI: strings.TrimSpace(entry.Snapshot.IMSI)}, nil
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
		for _, config := range configs {
			if config.Telephony == nil {
				continue
			}
			func() {
				gate := s.imsModeLock(config.ID)
				if !gate.TryLock() {
					return
				}
				defer gate.Unlock()
				current, err := s.store.Device(ctx, config.ID)
				if err != nil || current.Telephony == nil {
					return
				}
				operation, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				identity, err := s.observedTelephonySubscription(operation, current)
				if err != nil {
					return
				} // Unavailable metadata is not evidence of another SIM.
				entry, _, present := s.physicalForConfig(current)
				key := identity.ICCID + "\x00" + entry.Candidate.USBGeneration
				if !present || identity.ICCID == "" {
					key = "missing"
				}
				if observed[current.ID] == key {
					return
				}
				if _, seen := observed[current.ID]; !seen && identity.ICCID == current.Telephony.ICCID && key != "missing" {
					observed[current.ID] = key // Startup managers already apply this saved subscription.
					return
				}
				if err := s.reconcileTelephonySubscription(operation, current, identity); err != nil {
					if s.logger != nil {
						s.logger.Warn("subscription access reconciliation deferred", "device_id", current.ID, "error", err)
					}
					return
				}
				observed[current.ID] = key
			}()
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

// Caller holds the line writer fence. Only confirmed target identity may be
// persisted; an uncertain or missing SIM keeps the old intent saved and stopped.
func (s *Server) reconcileTelephonySubscription(ctx context.Context, config store.Device, identity vowifi.SIMIdentity) error {
	entry, physical, present := s.physicalForConfig(config)
	controllers, err := s.imsControllers(config)
	if err != nil {
		return err
	}
	for _, controller := range controllers {
		if err := stopIMSController(ctx, controller, config.ID); err != nil {
			return err
		}
	}
	for _, controller := range controllers {
		defer endIMSMaintenance(controller, config.ID)
	}
	if s.moduleCalls != nil && s.moduleCalls.Busy(config.ID) {
		// Its lease observes the changed ICCID/generation and releases its own
		// audio reservation. Never issue a new SIM's CLCC hangup here.
		return errors.New("previous module call is still releasing its reservation")
	}
	if !present || identity.ICCID == "" {
		return nil
	}
	if identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) || device.RegionBlockReason(identity.IMSI) != "" {
		return errors.New("the replacement subscription is not eligible for access")
	}
	card, err := s.store.CardPolicy(ctx, identity.ICCID)
	if errors.Is(err, store.ErrNotFound) {
		card = defaultCardPolicy(identity.ICCID)
	} else if err != nil {
		return err
	}
	policy, err := s.store.TelephonyForCard(ctx, config, card)
	if err != nil {
		return err
	}
	again, err := s.observedTelephonySubscription(ctx, config)
	latest, latestPhysical, stillPresent := s.physicalForConfig(config)
	if err != nil || !stillPresent || physical != latestPhysical || latest.Candidate.USBGeneration != entry.Candidate.USBGeneration ||
		again.ICCID != identity.ICCID || again.IMSI != identity.IMSI {
		return errors.New("subscription changed during access cleanup")
	}
	card.VoWiFiEnabled = false
	config.NetworkEnabled = false // A previous SIM's data intent cannot migrate.
	config.APN = card.APN
	if err := s.store.SaveTelephonyPolicy(ctx, config, policy, card); err != nil {
		return err
	}
	config.Telephony = &policy
	for _, controller := range controllers {
		endIMSMaintenance(controller, config.ID)
	}
	return s.startTelephonyAccess(ctx, config, physical)
}

func (s *Server) startTelephonyAccess(ctx context.Context, config store.Device, physical string) error {
	if store.AccessEnabled(config, store.IMSModeModule) {
		if _, err := s.devices.SetFlight(ctx, physical, false); err != nil {
			return err
		}
	}
	for _, access := range []string{store.IMSModeHost, store.IMSModeVoWiFi} {
		if !store.AccessEnabled(config, access) {
			continue
		}
		controller := s.controllerForMode(access)
		if controller == nil {
			return fmt.Errorf("%s provider unavailable", access)
		}
		endIMSMaintenance(controller, config.ID)
		if _, err := controller.RequestEnabled(config.ID, true); err != nil {
			return err
		}
	}
	return nil
}
