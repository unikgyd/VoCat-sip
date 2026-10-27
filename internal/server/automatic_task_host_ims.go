package server

import (
	"context"
	"errors"
	"time"

	"vocat/internal/device"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func (s *Server) stopAutomaticTaskController(ctx context.Context, id, access string) error {
	if access != store.IMSModeHost {
		return s.stopTelephonyController(ctx, id, access)
	}
	cleanup, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var pending error
	for {
		if err := cleanup.Err(); err != nil {
			return errors.Join(pending, err)
		}
		err := s.stopTelephonyController(cleanup, id, access)
		if !errors.Is(err, hostimsruntime.ErrCleanup) {
			return err
		}
		pending = err
		// Retry only the same retained owner, never the task's business action.
		// Unknown owners and a deadline still leave both cleanup fences intact.
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-cleanup.Done():
			timer.Stop()
			return errors.Join(err, cleanup.Err())
		case <-timer.C:
		}
	}
}

type hostIMSTaskSubscriptionReader interface {
	Subscription(context.Context, string) (vowifi.SIMIdentity, error)
}

func (s *Server) hasHostTaskMetadata() bool {
	_, ok := s.hostIMS.(hostIMSTaskSubscriptionReader)
	return ok
}

// The Host IMS metadata owner verifies equipment, topology and the live SIM.
// Do not open the legacy AT session merely to re-observe an unchanged profile.
func (s *Server) hostTaskSubscription(ctx context.Context, id, iccid string) (vowifi.SIMIdentity, error) {
	reader, ok := s.hostIMS.(hostIMSTaskSubscriptionReader)
	if !ok {
		return vowifi.SIMIdentity{}, errIMSUnavailable
	}
	identity, err := reader.Subscription(ctx, id)
	if err != nil || identity.ICCID != iccid || identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) {
		return vowifi.SIMIdentity{}, errors.Join(err, device.ErrSIMObservation)
	}
	return identity, ctx.Err()
}

func (journal *automaticTaskJournal) usesHostTaskMetadata(ctx context.Context, current store.Device) bool {
	p := journal.payload
	if !journal.server.hasHostTaskMetadata() || p.SwitchAttempted || p.Original.Card.ICCID != p.Target.Card.ICCID ||
		!store.HostIMSConfigured(p.Original.Projection.apply(current)) {
		return false
	}
	environment := p.Environment
	if environment == "" {
		// Version-1 journals predate the environment field. A surviving exact
		// Host IMS task can identify this case; missing/changed tasks retain
		// the original conservative recovery path.
		task, err := journal.server.store.AutomaticTask(ctx, journal.record.TaskID)
		if err != nil || task.DeviceID != current.ID || task.ProfileICCID != p.Original.Card.ICCID {
			return false
		}
		environment = task.Environment
	}
	return environment == store.IMSModeHost
}

// Wait on the selected cellular owner, never the dispatcher's fallback or a
// stale AT snapshot. No legacy radio, operator or packet-data I/O follows start.
func (s *Server) waitAutomaticHostIMS(ctx context.Context, id, iccid string, requireSMS bool) error {
	controller := s.controllerForMode(store.IMSModeHost)
	if controller == nil {
		return errIMSUnavailable
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		config, err := s.store.Device(ctx, id)
		if err != nil {
			return err
		}
		if !store.HostIMSConfigured(config) || !store.AccessEnabled(config, store.IMSModeHost) ||
			s.callTransportForConfig(config) != store.IMSModeHost || config.Telephony == nil || config.Telephony.ICCID != iccid {
			return errors.New("Host IMS task selection changed before registration")
		}
		state, stateErr := controller.State(id)
		if stateErr == nil && state.ICCID != "" && state.ICCID != iccid {
			return device.ErrSIMObservation
		}
		if stateErr == nil && state.Enabled && state.Active && state.SIMReady && state.IMSReady && (!requireSMS || state.SMSReady) {
			identity, err := s.observedTelephonySubscription(ctx, config)
			if err != nil || identity.ICCID != iccid || state.ICCID != iccid || identity.IMSI == "" || state.IMSI != identity.IMSI || device.IsPlaceholderIMSI(identity.IMSI) {
				return errors.Join(err, device.ErrSIMObservation)
			}
			return ctx.Err()
		}
		if stateErr == nil && !state.Enabled && !state.Active && state.LastError != "" {
			return errIMSUnavailable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
