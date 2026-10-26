package server

import (
	"context"
	"errors"
	"strings"

	"vocat/internal/device"
	"vocat/internal/modulecall"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

// Deliberately separate from callTransport: that HTTP convenience path may
// choose AT calling. A SIP media bridge must never silently do so.
type sipCarrierBackend struct{ server *Server }

func (b sipCarrierBackend) Ready(ctx context.Context, id string) error {
	if err := b.registered(ctx, id); err != nil {
		return err
	}
	calls, err := b.Calls(ctx, id)
	if err != nil {
		return sipgateway.ErrCarrierUnavailable
	}
	for _, call := range calls {
		if call.EndedAt == nil || call.SignalingPending {
			return sipgateway.ErrCarrierBusy
		}
	}
	return nil
}

func (b sipCarrierBackend) registered(ctx context.Context, id string) error {
	s := b.server
	if s.moduleVoiceSelected(ctx, id) {
		return s.moduleCalls.Available(ctx, id)
	}
	if err := s.verifiedCarrierIMS(ctx, id); err != nil {
		return err
	}
	if _, ok := s.vowifi.(VoWiFiCallController); !ok {
		return sipgateway.ErrCarrierUnavailable
	}
	if _, ok := s.vowifi.(VoWiFiCallMediaController); !ok {
		return sipgateway.ErrCarrierMedia
	}
	return nil
}

func (s *Server) verifiedCarrierIMS(ctx context.Context, id string) error {
	config, err := s.store.Device(ctx, id)
	if err != nil || config.IMSMode != "" && (store.EffectiveIMSMode(config) == store.IMSModeModule || !store.EffectiveIMSEnabled(config)) {
		return sipgateway.ErrCarrierUnavailable
	}
	return s.verifiedCarrierIMSOwner(ctx, id, s.vowifi)
}

func (s *Server) verifiedCarrierIMSOwner(ctx context.Context, id string, controller VoWiFiController) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if controller == nil {
		return sipgateway.ErrCarrierUnavailable
	}
	config, err := s.store.Device(ctx, id)
	if err != nil {
		return sipgateway.ErrCarrierUnavailable
	}
	entry, _, present := s.physicalForConfig(config)
	if !present {
		return sipgateway.ErrCarrierUnavailable
	}
	state, err := controller.State(id)
	if err != nil || !state.Enabled || !state.SIMReady || !state.IMSReady || strings.TrimSpace(state.IMSI) == "" || strings.TrimSpace(state.ICCID) == "" {
		return sipgateway.ErrCarrierUnavailable
	}
	if device.IsPlaceholderIMSI(state.IMSI) {
		return sipgateway.ErrCarrierUnavailable
	}
	if config.Telephony != nil && config.Telephony.ICCID != state.ICCID {
		return sipgateway.ErrCarrierUnavailable
	}
	if config.NetworkEnabled && store.WiFiExclusive(config) && store.EffectiveIMSMode(config) == store.IMSModeVoWiFi {
		return sipgateway.ErrCarrierForbidden
	}
	// Retain the same region guard as the existing modem/VoWiFi paths. Both
	// verified runtime identity and available cached physical identity count.
	if device.RegionBlockReason(state.IMSI) != "" {
		return sipgateway.ErrCarrierForbidden
	}
	if entry.Snapshot != nil {
		if device.RegionBlockReason(entry.Snapshot.IMSI) != "" {
			return sipgateway.ErrCarrierForbidden
		}
		if iccid := strings.TrimSpace(entry.Snapshot.ICCID); iccid != "" && iccid != state.ICCID {
			return sipgateway.ErrCarrierUnavailable
		}
	}
	return nil
}

func (b sipCarrierBackend) Incoming(ctx context.Context, id, callID string) (vowifi.Call, error) {
	config, configErr := b.server.store.Device(ctx, id)
	independent := configErr == nil && config.Telephony != nil
	if !independent {
		if err := b.registered(ctx, id); err != nil {
			return vowifi.Call{}, err
		}
	}
	calls, err := b.Calls(ctx, id)
	if err != nil {
		return vowifi.Call{}, err
	}
	for _, call := range calls {
		if call.ID == callID && call.Direction == "incoming" && call.State == "ringing" && call.EndedAt == nil {
			if independent && !b.server.moduleVoiceCall(id, callID) {
				if err := b.server.verifiedCarrierIMSOwner(ctx, id, b.server.ownerIMSCall(id, callID)); err != nil {
					return vowifi.Call{}, err
				}
			}
			return call, nil
		}
	}
	return vowifi.Call{}, sipgateway.ErrCarrierBusy
}

func (b sipCarrierBackend) Answer(ctx context.Context, id, callID string) (vowifi.Call, error) {
	if _, err := b.Incoming(ctx, id, callID); err != nil {
		return vowifi.Call{}, errors.Join(sipgateway.ErrCarrierAnswerNotAttempted, err)
	}
	var call vowifi.Call
	var err error
	if b.server.moduleVoiceCall(id, callID) {
		lock := b.server.imsModeLock(id)
		lock.RLock()
		defer lock.RUnlock()
		call, err = b.server.moduleCalls.AnswerCall(ctx, id, callID)
		if errors.Is(err, modulecall.ErrNotAttempted) {
			err = errors.Join(sipgateway.ErrCarrierAnswerNotAttempted, err)
		}
	} else {
		call, err = b.server.vowifi.(VoWiFiCallController).AnswerCall(ctx, id, callID)
	}
	if errors.Is(err, ims.ErrCallState) || errors.Is(err, ims.ErrCallBusy) {
		err = sipgateway.ErrCarrierBusy
	}
	return call, err
}

// Keep web answering and SIP group ringing on the same per-call ownership path.
func (s *Server) claimLocalSIPAnswer(id, callID string) error {
	if s.sipGateway != nil {
		return s.sipGateway.ClaimLocalAnswer(id, callID)
	}
	return nil
}

func (s *Server) ForwardIMSIncomingCall(id, callID string) bool {
	return s.sipGateway != nil && s.sipGateway.ForwardIncoming(id, callID)
}
func (b sipCarrierBackend) Calls(ctx context.Context, id string) ([]vowifi.Call, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if b.server.store != nil {
		config, err := b.server.store.Device(ctx, id)
		if err != nil {
			return nil, err
		}
		if config.Telephony != nil {
			result := []vowifi.Call{}
			if c, ok := b.server.vowifi.(VoWiFiCallController); ok {
				entries, err := c.Calls(id)
				if err != nil {
					return nil, err
				}
				result = append(result, entries...)
			}
			if b.server.moduleCalls != nil && (b.server.moduleVoiceEnabled(config) || b.server.moduleCalls.Busy(id)) {
				entries, err := b.server.moduleCalls.Calls(id)
				if err != nil {
					return nil, err
				}
				result = append(result, entries...)
			}
			return result, nil
		}
	}
	if b.server.moduleVoiceSelected(ctx, id) || b.server.moduleCalls != nil && b.server.moduleCalls.Busy(id) {
		return b.server.moduleCalls.Calls(id)
	}
	controller, ok := b.server.vowifi.(VoWiFiCallController)
	if !ok {
		return nil, sipgateway.ErrCarrierUnavailable
	}
	return controller.Calls(id)
}
func (b sipCarrierBackend) Dial(ctx context.Context, id, number string) (vowifi.Call, error) {
	if err := b.Ready(ctx, id); err != nil {
		return vowifi.Call{}, err
	}
	var call vowifi.Call
	var err error
	if b.server.moduleVoiceSelected(ctx, id) {
		lock := b.server.imsModeLock(id)
		lock.RLock()
		defer lock.RUnlock()
		call, err = b.server.moduleCalls.DialCall(ctx, id, number)
	} else {
		call, err = b.server.vowifi.(VoWiFiCallController).DialCall(ctx, id, number)
	}
	if errors.Is(err, ims.ErrCallBusy) {
		err = sipgateway.ErrCarrierBusy
	}
	return call, err
}
func (b sipCarrierBackend) Hangup(ctx context.Context, id, callID string) error {
	if b.server.moduleVoiceCall(id, callID) {
		return b.server.moduleCalls.HangupCall(ctx, id, callID)
	}
	// Cleanup stays possible when the SIM disappears or policy changes; only
	// this known IMS call is affected, never AT or another module's session.
	controller, ok := b.server.vowifi.(VoWiFiCallController)
	if !ok {
		return sipgateway.ErrCarrierUnavailable
	}
	return controller.HangupCall(ctx, id, callID)
}
func (b sipCarrierBackend) Media(ctx context.Context, id, callID string) (vowifi.CallMedia, error) {
	if b.server.moduleVoiceCall(id, callID) {
		return b.server.moduleCalls.CallMedia(ctx, id, callID)
	}
	controller, ok := b.server.vowifi.(VoWiFiCallMediaController)
	if !ok {
		return nil, sipgateway.ErrCarrierMedia
	}
	return controller.CallMedia(ctx, id, callID)
}

func (b sipCarrierBackend) SendDTMF(ctx context.Context, id, callID string, digit byte) error {
	if b.server.moduleVoiceCall(id, callID) {
		return b.server.moduleCalls.SendDTMF(ctx, id, callID, digit)
	}
	controller, ok := b.server.vowifi.(VoWiFiCallDTMFController)
	if !ok {
		return sipgateway.ErrCarrierUnavailable
	}
	return controller.SendDTMF(ctx, id, callID, digit)
}

var _ sipgateway.CarrierBackend = sipCarrierBackend{}
var _ sipgateway.CarrierIncomingBackend = sipCarrierBackend{}
var _ sipgateway.CarrierDTMFBackend = sipCarrierBackend{}
