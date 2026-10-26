package server

import (
	"context"
	"errors"
	"sync"

	"vocat/internal/linelock"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

var errIMSUnavailable = errors.New("the selected IMS runtime is unavailable or not registered")

// The legacy field remains the common service boundary, not a Wi-Fi fallback.
// Every operation selects exactly one persisted line mode. In-flight calls are
// pinned to their creating controller; a later configuration edit cannot send
// their BYE or media request through another module/runtime.
type imsServiceDispatch struct{ server *Server }

func (s *Server) wifiController() VoWiFiController {
	if s.wifiIMS != nil {
		return s.wifiIMS
	}
	if _, routed := s.vowifi.(*imsServiceDispatch); !routed {
		return s.vowifi // Existing embedders/tests can still supply the legacy field.
	}
	return nil
}
func (s *Server) controllerForMode(mode string) VoWiFiController {
	switch mode {
	case store.IMSModeHost:
		return s.hostIMS
	case store.IMSModeVoWiFi:
		return s.wifiController()
	default:
		return nil
	}
}
func (s *Server) selectedIMS(id string) (VoWiFiController, string, error) {
	if s.store == nil {
		return s.wifiController(), store.IMSModeVoWiFi, nil
	}
	config, err := s.store.Device(context.Background(), id)
	if err != nil {
		return nil, "", err
	}
	if config.IMSMode == "" {
		// The old API selected its already-registered runtime by evidence even
		// when the legacy desired flag was false. Explicit modes are strict.
		return s.wifiController(), store.IMSModeVoWiFi, nil
	}
	mode := store.EffectiveIMSMode(config)
	return s.controllerForMode(mode), mode, nil
}
func (s *Server) imsModeLock(id string) *sync.RWMutex {
	return linelock.For(id)
}
func (d *imsServiceDispatch) outboundIMS(id string) (VoWiFiController, error) {
	c, mode, err := d.server.selectedIMS(id)
	if err != nil || c == nil || mode == store.IMSModeModule {
		return nil, errIMSUnavailable
	}
	if d.server.store != nil {
		config, err := d.server.store.Device(context.Background(), id)
		if err != nil || config.Telephony != nil && !store.AccessEnabled(config, mode) || config.Telephony == nil && config.IMSMode != "" && !store.EffectiveIMSEnabled(config) {
			return nil, errIMSUnavailable
		}
	}
	return c, nil
}
func (s *Server) pinIMSCall(id string, call vowifi.Call, controller VoWiFiController) {
	if call.ID == "" || controller == nil {
		return
	}
	s.imsCallsMu.Lock()
	defer s.imsCallsMu.Unlock()
	if s.imsCallOwners == nil {
		s.imsCallOwners = make(map[string]VoWiFiController)
	}
	key := id + "\x00" + call.ID
	if call.EndedAt != nil && !call.SignalingPending {
		if owner, known := s.imsCallOwners[key]; !known || owner == controller {
			delete(s.imsCallOwners, key)
		}
	} else {
		if owner, known := s.imsCallOwners[key]; known && owner != controller {
			s.imsCallOwners[key] = nil
		} else {
			s.imsCallOwners[key] = controller
		}
	}
}
func (s *Server) ownerIMSCall(id, callID string) VoWiFiController {
	s.imsCallsMu.Lock()
	controller, known := s.imsCallOwners[id+"\x00"+callID]
	s.imsCallsMu.Unlock()
	if known {
		return controller
	}
	if s.store != nil {
		if config, err := s.store.Device(context.Background(), id); err != nil || config.Telephony != nil {
			return nil
		}
	}
	controller, _, _ = s.selectedIMS(id)
	return controller
}

func (s *Server) imsControllers(config store.Device) ([]VoWiFiController, error) {
	if config.Telephony == nil {
		c, _, err := s.selectedIMS(config.ID)
		if c == nil {
			return nil, err
		}
		return []VoWiFiController{c}, err
	}
	var result []VoWiFiController
	if c := s.wifiController(); c != nil {
		result = append(result, c)
	} else if store.AccessEnabled(config, store.IMSModeVoWiFi) {
		return nil, errIMSUnavailable
	}
	if store.HostIMSConfigured(config) {
		if s.hostIMS != nil {
			result = append(result, s.hostIMS)
		} else if store.AccessEnabled(config, store.IMSModeHost) {
			return nil, errIMSUnavailable
		}
	}
	return result, nil
}
func (d *imsServiceDispatch) State(id string) (vowifi.State, error) {
	c, mode, err := d.server.selectedIMS(id)
	if err != nil {
		return vowifi.State{}, err
	}
	if mode == store.IMSModeModule {
		return vowifi.State{DeviceID: id, Phase: vowifi.PhaseIdle}, nil
	}
	if c == nil {
		return vowifi.State{}, errIMSUnavailable
	}
	return c.State(id)
}
func (d *imsServiceDispatch) RequestEnabled(id string, enabled bool) (vowifi.State, error) {
	if d.server.store != nil {
		config, err := d.server.store.Device(context.Background(), id)
		if err != nil {
			return vowifi.State{}, err
		}
		if config.Telephony != nil {
			if wifi := d.server.wifiController(); wifi != nil {
				if _, err := wifi.RequestEnabled(id, enabled && store.AccessEnabled(config, store.IMSModeVoWiFi)); err != nil {
					return vowifi.State{}, err
				}
			}
			if store.HostIMSConfigured(config) && d.server.hostIMS != nil {
				if _, err := d.server.hostIMS.RequestEnabled(id, enabled && store.AccessEnabled(config, store.IMSModeHost)); err != nil {
					return vowifi.State{}, err
				}
			}
			return d.State(id)
		}
	}
	c, mode, err := d.server.selectedIMS(id)
	if err == nil && mode == store.IMSModeModule && !enabled {
		c = d.server.wifiController() // Legacy toggle persisted false before teardown.
	}
	if err != nil || c == nil {
		return vowifi.State{}, errIMSUnavailable
	}
	return c.RequestEnabled(id, enabled)
}
func (d *imsServiceDispatch) StopDevice(ctx context.Context, id string) error {
	if d.server.store != nil {
		config, err := d.server.store.Device(ctx, id)
		if err != nil {
			return err
		}
		if config.Telephony != nil {
			controllers, err := d.server.imsControllers(config)
			if err != nil {
				return err
			}
			for _, c := range controllers {
				if err := stopIMSController(ctx, c, id); err != nil {
					return err
				}
			}
			return nil
		}
	}
	c, _, err := d.server.selectedIMS(id)
	if err != nil {
		return err
	}
	return stopIMSController(ctx, c, id)
}
func (d *imsServiceDispatch) RequestReconnect(id string) (vowifi.State, error) {
	c, _, err := d.server.selectedIMS(id)
	if err != nil || c == nil {
		return vowifi.State{}, errIMSUnavailable
	}
	return c.RequestReconnect(id)
}
func (d *imsServiceDispatch) Calls(id string) ([]vowifi.Call, error) {
	if d.server.store != nil {
		config, err := d.server.store.Device(context.Background(), id)
		if err != nil {
			return nil, err
		}
		if config.Telephony != nil {
			controllers, err := d.server.imsControllers(config)
			if err != nil {
				return nil, err
			}
			result := []vowifi.Call{}
			for _, c := range controllers {
				calls, ok := c.(VoWiFiCallController)
				if !ok {
					continue
				}
				entries, err := calls.Calls(id)
				if err != nil {
					state, stateErr := c.State(id)
					if stateErr != nil || state.IMSReady || state.Phase == vowifi.PhaseStopping || len(state.CleanupErrors) != 0 {
						return nil, err
					}
					continue
				}
				for _, call := range entries {
					d.server.pinIMSCall(id, call, c)
					result = append(result, call)
				}
			}
			return result, nil
		}
	}
	c, _, err := d.server.selectedIMS(id)
	controller, ok := c.(VoWiFiCallController)
	if err != nil || !ok {
		return nil, errIMSUnavailable
	}
	calls, err := controller.Calls(id)
	if err == nil {
		for _, call := range calls {
			d.server.pinIMSCall(id, call, c)
		}
	}
	return calls, err
}
func (d *imsServiceDispatch) DialCall(ctx context.Context, id, number string) (vowifi.Call, error) {
	lock := d.server.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	c, err := d.outboundIMS(id)
	controller, ok := c.(VoWiFiCallController)
	if err != nil || !ok {
		return vowifi.Call{}, errIMSUnavailable
	}
	state, err := c.State(id)
	if err != nil || !state.IMSReady {
		return vowifi.Call{}, errIMSUnavailable
	}
	call, err := controller.DialCall(ctx, id, number)
	d.server.pinIMSCall(id, call, c)
	return call, err
}
func (d *imsServiceDispatch) AnswerCall(ctx context.Context, id, callID string) (vowifi.Call, error) {
	lock := d.server.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	c := d.server.ownerIMSCall(id, callID)
	controller, ok := c.(VoWiFiCallController)
	if !ok {
		return vowifi.Call{}, errIMSUnavailable
	}
	call, err := controller.AnswerCall(ctx, id, callID)
	d.server.pinIMSCall(id, call, c)
	return call, err
}
func (d *imsServiceDispatch) HangupCall(ctx context.Context, id, callID string) error {
	controller, ok := d.server.ownerIMSCall(id, callID).(VoWiFiCallController)
	if !ok {
		return errIMSUnavailable
	}
	return controller.HangupCall(ctx, id, callID)
}
func (d *imsServiceDispatch) CallMedia(ctx context.Context, id, callID string) (vowifi.CallMedia, error) {
	controller, ok := d.server.ownerIMSCall(id, callID).(VoWiFiCallMediaController)
	if !ok {
		return nil, errIMSUnavailable
	}
	return controller.CallMedia(ctx, id, callID)
}

func (d *imsServiceDispatch) SendDTMF(ctx context.Context, id, callID string, digit byte) error {
	controller, ok := d.server.ownerIMSCall(id, callID).(VoWiFiCallDTMFController)
	if !ok {
		return errIMSUnavailable
	}
	return controller.SendDTMF(ctx, id, callID, digit)
}
func (d *imsServiceDispatch) SendSMS(ctx context.Context, id string, request vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	lock := d.server.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	c, err := d.outboundIMS(id)
	controller, ok := c.(imsSMSController)
	if err != nil || !ok {
		return vowifi.SMSSubmitResult{}, vowifi.ErrSMSNotReady
	}
	return controller.SendSMS(ctx, id, request)
}
func (d *imsServiceDispatch) SendSMSForSubscription(ctx context.Context, id, imsi, iccid string, request vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	lock := d.server.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	c, err := d.outboundIMS(id)
	controller, ok := c.(boundIMSSMSController)
	if err != nil || !ok {
		return vowifi.SMSSubmitResult{}, vowifi.ErrSMSNotReady
	}
	return controller.SendSMSForSubscription(ctx, id, imsi, iccid, request)
}
func (d *imsServiceDispatch) SendUSSI(ctx context.Context, id string, request vowifi.USSISubmitRequest) (vowifi.USSISubmitResult, error) {
	lock := d.server.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	c, err := d.outboundIMS(id)
	if request.SessionID != "" {
		c, err = d.server.ownerIMSDialog(id, request.SessionID)
	}
	controller, ok := c.(imsUSSIController)
	if err != nil || !ok {
		return vowifi.USSISubmitResult{}, vowifi.ErrUSSINotReady
	}
	result, err := controller.SendUSSI(ctx, id, request)
	session := request.SessionID
	if session == "" {
		session = result.SessionID
	}
	if session != "" {
		terminal := err == nil && !result.Continueable || errors.Is(err, vowifi.ErrUSSISessionNotFound)
		d.server.pinIMSDialog(id, session, c, terminal)
	}
	return result, err
}
func (d *imsServiceDispatch) CancelUSSI(ctx context.Context, id, session string) error {
	lock := d.server.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	c, err := d.server.ownerIMSDialog(id, session)
	controller, ok := c.(imsUSSICanceler)
	if err != nil || !ok {
		return vowifi.ErrUSSINotReady
	}
	err = controller.CancelUSSI(ctx, id, session)
	if err == nil || errors.Is(err, vowifi.ErrUSSISessionNotFound) {
		d.server.pinIMSDialog(id, session, c, true)
	}
	return err
}

func (s *Server) ownerIMSDialog(id, session string) (VoWiFiController, error) {
	s.imsCallsMu.Lock()
	owner, known := s.imsDialogOwners[id+"\x00"+session]
	s.imsCallsMu.Unlock()
	if known {
		if owner == nil {
			return nil, vowifi.ErrUSSISessionNotFound
		}
		return owner, nil
	}
	if s.store != nil {
		config, err := s.store.Device(context.Background(), id)
		if err != nil {
			return nil, err
		}
		if config.Telephony != nil {
			return nil, vowifi.ErrUSSISessionNotFound
		}
	}
	owner, _, err := s.selectedIMS(id)
	return owner, err
}

func (s *Server) pinIMSDialog(id, session string, owner VoWiFiController, terminal bool) {
	s.imsCallsMu.Lock()
	defer s.imsCallsMu.Unlock()
	if s.imsDialogOwners == nil {
		s.imsDialogOwners = make(map[string]VoWiFiController)
	}
	key := id + "\x00" + session
	old, known := s.imsDialogOwners[key]
	if terminal {
		if !known || old == owner {
			delete(s.imsDialogOwners, key)
		}
	} else if known && old != owner {
		s.imsDialogOwners[key] = nil // Colliding network IDs must never change the owner.
	} else {
		s.imsDialogOwners[key] = owner
	}
}
func (d *imsServiceDispatch) BeginMaintenance(id string) error {
	if d.server.store != nil {
		if config, err := d.server.store.Device(context.Background(), id); err != nil {
			return err
		} else if config.Telephony != nil {
			controllers, err := d.server.imsControllers(config)
			if err != nil {
				return err
			}
			for _, c := range controllers {
				if maintenance, ok := c.(VoWiFiMaintenanceController); ok {
					if err := maintenance.BeginMaintenance(id); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	c, _, err := d.server.selectedIMS(id)
	if err != nil {
		return err
	}
	if controller, ok := c.(VoWiFiMaintenanceController); ok {
		return controller.BeginMaintenance(id)
	}
	return nil
}
func (d *imsServiceDispatch) EndMaintenance(id string) {
	if d.server.store != nil {
		if config, err := d.server.store.Device(context.Background(), id); err == nil && config.Telephony != nil {
			controllers, _ := d.server.imsControllers(config)
			for _, c := range controllers {
				endIMSMaintenance(c, id)
			}
			return
		}
	}
	c, _, _ := d.server.selectedIMS(id)
	if controller, ok := c.(VoWiFiMaintenanceController); ok {
		controller.EndMaintenance(id)
	}
}
func (d *imsServiceDispatch) ModemSMSSyncBlocked(id string) bool {
	if d.server.store != nil {
		if config, err := d.server.store.Device(context.Background(), id); err != nil || store.HostIMSConfigured(config) {
			return true
		} else if config.Telephony != nil {
			if controller, ok := d.server.wifiController().(VoWiFiSMSSyncController); ok {
				return controller.ModemSMSSyncBlocked(id)
			}
			return false
		}
	}
	c, mode, err := d.server.selectedIMS(id)
	if err != nil || mode == store.IMSModeHost {
		return true // Host owns UIM/PDN even before registration succeeds.
	}
	if controller, ok := c.(VoWiFiSMSSyncController); ok {
		return controller.ModemSMSSyncBlocked(id)
	}
	return false
}
func (d *imsServiceDispatch) WithSIM(ctx context.Context, id string, fn func(vowifi.SIMIdentity, vowifi.AKAProvider) error) error {
	c, mode, err := d.server.selectedIMS(id)
	if d.server.store != nil {
		if config, loadErr := d.server.store.Device(ctx, id); loadErr != nil {
			return loadErr
		} else if config.Telephony != nil {
			c, mode = d.server.wifiController(), store.IMSModeVoWiFi
		}
	}
	controller, ok := c.(e911SIMController)
	if err != nil || mode != store.IMSModeVoWiFi || !ok {
		return errIMSUnavailable
	}
	return controller.WithSIM(ctx, id, fn)
}
