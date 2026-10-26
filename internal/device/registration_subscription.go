package device

import (
	"context"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// Keep the original raw identity reader even when the NAS surface is wrapped
// to prohibit shared-owner radio cycling. Every mutation, including rollback,
// has to prove the original SIM and current application ownership again.
type boundNativeRegistration struct {
	nativeQMIRegistrationSession
	identity any
	guard    *simWriteGuard
}

func (s *boundNativeRegistration) SetOperatingMode(ctx context.Context, mode qmi.OperatingMode) error {
	if err := s.guard.verify(ctx, s.identity, true); err != nil {
		return err
	}
	if mode != qmi.ModeOnline && s.guard.manager.allowAutomaticSIMRecovery != nil && !s.guard.manager.allowAutomaticSIMRecovery(ctx, s.guard.binding.candidate) {
		return ErrDataBackendUnavailable
	}
	return s.nativeQMIRegistrationSession.SetOperatingMode(ctx, mode)
}

func (s *boundNativeRegistration) SetSystemSelectionPreference(ctx context.Context, pref qmi.SystemSelectionPreference) error {
	if err := s.guard.verify(ctx, s.identity, true); err != nil {
		return err
	}
	return s.nativeQMIRegistrationSession.SetSystemSelectionPreference(ctx, pref)
}

func (s *boundNativeRegistration) InitiateNetworkRegister(ctx context.Context, request qmi.NASInitiateNetworkRegisterRequest) error {
	if err := s.guard.verify(ctx, s.identity, true); err != nil {
		return err
	}
	return s.nativeQMIRegistrationSession.InitiateNetworkRegister(ctx, request)
}

func (s *boundNativeRegistration) ForceNetworkSearch(ctx context.Context) error {
	if err := s.guard.verify(ctx, s.identity, true); err != nil {
		return err
	}
	return s.nativeQMIRegistrationSession.ForceNetworkSearch(ctx)
}

func (s *boundNativeRegistration) AttachDetach(ctx context.Context, attach bool) error {
	if err := s.guard.verify(ctx, s.identity, true); err != nil {
		return err
	}
	return s.nativeQMIRegistrationSession.AttachDetach(ctx, attach)
}

func (s *boundNativeRegistration) GetServingSystem(ctx context.Context) (*qmi.ServingSystem, error) {
	if err := s.guard.verify(ctx, s.identity, true); err != nil {
		return nil, err
	}
	result, err := s.nativeQMIRegistrationSession.GetServingSystem(ctx)
	if err == nil {
		err = s.guard.current(ctx)
	}
	return result, err
}
