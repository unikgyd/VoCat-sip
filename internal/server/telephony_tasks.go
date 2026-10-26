package server

import (
	"context"
	"errors"
	"fmt"

	"vocat/internal/store"
)

func (s *Server) stopTelephonyAccess(ctx context.Context, config store.Device) error {
	controllers, err := s.imsControllers(config)
	if err != nil {
		return err
	}
	if err := s.requireModuleCallsIdle(ctx, config); err != nil {
		return err
	}
	for _, controller := range controllers {
		if err := imsActiveCalls(controller, config.ID); err != nil {
			return err
		}
	}
	for _, access := range s.telephonyControllerAccesses(config) {
		controller := s.controllerForMode(access)
		if err := s.stopTelephonyController(ctx, config.ID, access); err != nil {
			return err
		}
		defer endIMSMaintenance(controller, config.ID)
	}
	return nil
}

// A scheduled task temporarily changes the active device projection. The SIM's
// durable preferences stay intact and are restored by the task's finalizer.
func (s *Server) prepareIndependentTaskEnvironment(ctx context.Context, config *store.Device, physical string, task store.AutomaticTask, progress automaticTaskProgress) error {
	identity, err := s.observedTelephonySubscription(ctx, *config)
	if err != nil || identity.ICCID != config.Telephony.ICCID || identity.ICCID != task.ProfileICCID {
		return errors.New("task subscription does not match the active SIM")
	}
	if err := s.stopTelephonyAccess(ctx, *config); err != nil {
		return err
	}
	if err := s.quiesceOwnedData(ctx, *config); err != nil {
		return fmt.Errorf("stop original data before preparing task: %w", err)
	}
	policy := *config.Telephony
	if task.Environment == "vowifi" {
		if task.TaskType == "public_ip" {
			return errors.New("public IP tasks require cellular data")
		}
		progress("正在准备独立 Wi-Fi 通话执行环境")
		policy.AutoConnectWiFi = true
		policy.CellularMode = store.IMSModeModule
		policy.AutoConnectCellular = false
		policy.WiFiRadioPolicy = "exclusive"
		policy.DefaultRoute = store.IMSModeVoWiFi
	} else {
		progress("正在准备蜂窝执行环境")
		policy.CellularMode, policy.DefaultRoute = store.IMSModeModule, store.IMSModeModule
		policy.AutoConnectCellular, policy.WiFiRadioPolicy = true, "coexist"
		policy.AutoConnectWiFi = false
	}
	config.Telephony = &policy
	config.NetworkEnabled = task.TaskType == "public_ip"
	if err := s.store.UpsertDevice(ctx, *config); err != nil {
		return err
	}
	activation := *config
	activation.NetworkEnabled = false // data starts only after task registration is ready
	if task.Environment == "vowifi" {
		s.cellularDataRuntime().resume(config.ID)
	}
	if err := s.startTelephonyAccess(ctx, activation, physical); err != nil {
		return err
	}
	if task.Environment == "vowifi" {
		return s.waitAutomaticVoWiFi(ctx, config.ID, policy.ICCID, task.TaskType == "sms")
	}
	if _, err := s.devices.SetOperatorSelection(ctx, physical, true, "", nil); err != nil {
		return err
	}
	if _, err := s.devices.ReRegisterOperator(ctx, physical); err != nil {
		return err
	}
	if err := s.waitAutomaticCellular(ctx, physical, task.TaskType == "public_ip"); err != nil {
		return err
	}
	s.cellularDataRuntime().resume(config.ID)
	if task.TaskType == "public_ip" {
		card, err := s.store.CardPolicy(ctx, policy.ICCID)
		if err != nil {
			return err
		}
		if _, err := s.applyCellularData(ctx, config.ID, physical, s.cardNetworkRequest(ctx, physical, *config, card, true)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) restoreIndependentTaskEnvironment(ctx context.Context, physical string, snapshot automaticTaskEnvironmentSnapshot) error {
	config, card := snapshot.config, snapshot.policy
	current, err := s.store.Device(ctx, config.ID)
	if err != nil {
		return err
	}
	if err := s.stopTelephonyAccess(ctx, current); err != nil {
		return err
	}
	if err := s.quiesceOwnedData(ctx, current); err != nil {
		return fmt.Errorf("stop temporary task data before restoring access: %w", err)
	}
	identity, err := s.observedTelephonySubscription(ctx, current)
	if err != nil || identity.ICCID != config.Telephony.ICCID {
		return errors.New("task restoration subscription could not be verified")
	}
	// Independent tasks edit only the active projection. Reload durable card
	// settings so a concurrent APN/settings edit is not overwritten by a snapshot.
	fresh, err := s.taskCardSnapshot(ctx, config, identity)
	if err != nil {
		return err
	}
	config, card = fresh.config, fresh.policy
	card.VoWiFiEnabled = false
	if err := s.store.SaveTelephonyPolicy(ctx, config, *config.Telephony, card); err != nil {
		return err
	}
	s.cellularDataRuntime().resume(config.ID)
	if err := s.startTelephonyAccess(ctx, config, physical); err != nil {
		return err
	}
	if !store.HostIMSConfigured(config) && config.NetworkEnabled {
		if _, err := s.applyCellularData(ctx, config.ID, physical, s.cardNetworkRequest(ctx, physical, config, card, config.NetworkEnabled)); err != nil {
			return fmt.Errorf("restore saved cellular data intent: %w", err)
		}
	}
	return nil
}
