package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func (s *Server) requireAutomaticTaskCallsIdle(ctx context.Context, config store.Device) error {
	// Startup deliberately leaves journaled lines disabled. Register inert
	// entries before querying their call state; absence is not a cleanup ACK.
	for _, access := range s.telephonyControllerAccesses(config) {
		if ensure, ok := s.controllerForMode(access).(interface {
			Ensure(context.Context, string) error
		}); ok {
			if err := ensure.Ensure(ctx, config.ID); err != nil {
				return err
			}
		}
	}
	if err := s.requireModuleCallsIdle(ctx, config); err != nil {
		return err
	}
	controllers, err := s.imsControllers(config)
	if err != nil {
		return err
	}
	for _, controller := range controllers {
		if err := imsActiveCalls(controller, config.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) taskCardSnapshot(ctx context.Context, original store.Device, identity vowifi.SIMIdentity) (automaticTaskEnvironmentSnapshot, error) {
	card, err := s.store.CardPolicy(ctx, identity.ICCID)
	if errors.Is(err, store.ErrNotFound) {
		card = defaultCardPolicy(identity.ICCID)
	} else if err != nil {
		return automaticTaskEnvironmentSnapshot{}, err
	}
	config := original
	config.APN = card.APN
	config.NetworkEnabled = card.NetworkEnabled && !card.AirplaneEnabled && !card.VoWiFiEnabled
	config.VoWiFiEnabled = card.VoWiFiEnabled
	if original.Telephony != nil {
		policy, err := s.store.TelephonyForCard(ctx, original, card)
		if err != nil {
			return automaticTaskEnvironmentSnapshot{}, err
		}
		config.Telephony = &policy
		config.NetworkEnabled = card.NetworkEnabled && policy.CellularMode == store.IMSModeModule &&
			!(policy.AutoConnectWiFi && policy.WiFiRadioPolicy == "exclusive") && (!card.AirplaneEnabled || policy.AutoConnectCellular)
	}
	return automaticTaskEnvironmentSnapshot{config: config, policy: card}, nil
}

// Preparation can fail after RF/data cleanup or after a committed eSIM switch,
// before the ordinary execution finalizer exists. Restore only a positively
// observed original/target card; never switch an unrelated replacement SIM.
func (s *Server) restoreFailedAutomaticTaskPreparation(ctx context.Context, task store.AutomaticTask, plan automaticTaskRevertState) error {
	if !plan.modified {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	config, err := s.store.Device(cleanup, task.DeviceID)
	if err != nil {
		return err
	}
	_, physical, present := s.physicalForConfig(config)
	if !present || physical != plan.physicalID {
		return device.ErrSIMObservation
	}
	snapshot, err := s.devices.Refresh(cleanup, physical)
	if err != nil {
		return err
	}
	identity := vowifi.SIMIdentity{ICCID: strings.TrimSpace(snapshot.ICCID), IMSI: strings.TrimSpace(snapshot.IMSI)}
	if identity.ICCID == plan.initialICCID {
		return s.restoreAutomaticTaskEnvironment(physical, plan.snapshot, cleanup)
	}
	if identity.ICCID != task.ProfileICCID || !plan.switchAttempted {
		return device.ErrSIMObservation
	}
	if task.RevertProfile {
		if plan.initialEID == "" {
			return device.ErrSIMObservation
		}
		storage, err := s.taskProfileStorage(cleanup, physical, plan.initialICCID, plan.initialEID)
		if err != nil {
			return err
		}
		if config.Telephony != nil && config.Telephony.ICCID != identity.ICCID {
			applied, _, err := s.bindTelephonySubscription(cleanup, config, identity)
			if err != nil {
				return err
			}
			if applied == nil {
				return device.ErrSIMObservation
			}
			config = *applied
		}
		if config.Telephony != nil {
			if err := s.stopTelephonyAccess(cleanup, config); err != nil {
				return err
			}
			if err := s.quiesceOwnedData(cleanup, config); err != nil {
				return err
			}
		} else if err := s.quiesceVoWiFiForProfileSwitch(cleanup, config.ID); err != nil {
			return err
		}
		if _, err := s.setSubscriptionFlight(cleanup, physical, identity, true); err != nil {
			return err
		}
		switchErr := s.devices.ESIMSwitchProfile(cleanup, physical, plan.initialICCID, storage.AID)
		actual, verifyErr := s.devices.Refresh(cleanup, physical)
		if verifyErr != nil || strings.TrimSpace(actual.ICCID) != plan.initialICCID {
			return errors.Join(switchErr, verifyErr, device.ErrSIMObservation)
		}
		return s.restoreAutomaticTaskEnvironment(physical, plan.snapshot, cleanup)
	}
	target, err := s.taskCardSnapshot(cleanup, plan.snapshot.config, identity)
	if err != nil {
		return err
	}
	return s.restoreAutomaticTaskEnvironment(physical, target, cleanup)
}

// A failed revert may already have committed. A fresh observation chooses the
// matching saved environment; it never applies the requested target blindly.
func (s *Server) restoreObservedAutomaticTaskEnvironment(ctx context.Context, physical string, candidates ...automaticTaskEnvironmentSnapshot) (string, error) {
	actual, err := s.devices.Refresh(ctx, physical)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if candidate.policy.ICCID != "" && strings.TrimSpace(actual.ICCID) == candidate.policy.ICCID {
			return candidate.policy.ICCID, s.restoreAutomaticTaskEnvironment(physical, candidate, ctx)
		}
	}
	return "", device.ErrSIMObservation
}
