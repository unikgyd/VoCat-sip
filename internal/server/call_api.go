package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

const maxCallDuration = 10 * time.Minute

func (s *Server) handleCalls(w http.ResponseWriter, r *http.Request, config store.Device, physicalID string) bool {
	if !requireMethod(w, r, http.MethodGet) {
		return true
	}
	if config.Telephony != nil {
		calls, err := (sipCarrierBackend{s}).Calls(r.Context(), config.ID)
		if err != nil {
			writeError(w, 503, "call_state_unavailable", err.Error())
			return true
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"device_id": config.ID, "transport": s.callTransportForConfig(config), "calls": calls}})
		return true
	}
	transport := s.callTransportForConfig(config)
	if transport == "unavailable" {
		writeError(w, http.StatusServiceUnavailable, "ims_unavailable", "the selected line policy is unavailable")
		return true
	}
	if transport == "cellular" && s.moduleCalls != nil && s.moduleCalls.Busy(config.ID) {
		calls, err := s.moduleCalls.Calls(config.ID)
		if err != nil {
			writeError(w, 503, "module_call_unavailable", "module call state is unavailable")
			return true
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"device_id": config.ID, "transport": "cellular", "calls": calls}})
		return true
	}
	if transport == "vowifi" || transport == "host_ims" {
		controller, ok := s.vowifi.(VoWiFiCallController)
		if !ok {
			writeError(w, http.StatusNotImplemented, "vowifi_voice_unavailable", "the active VoWiFi IMS session does not expose voice-call signalling")
			return true
		}
		calls, err := controller.Calls(config.ID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "vowifi_call_failed", err.Error())
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"device_id": config.ID, "transport": transport, "calls": calls,
		}})
		return true
	}
	response, err := s.devices.ExecuteAT(r.Context(), physicalID, "AT+CLCC")
	if err != nil {
		s.writeDeviceError(w, err)
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"device_id": config.ID,
			"transport": transport,
			"calls":     parseCLCC(response),
			"raw":       response.Text(),
		},
	})
	return true
}

func (s *Server) handleCallAction(w http.ResponseWriter, r *http.Request, config store.Device, physicalID, action string) bool {
	if !requireMethod(w, r, http.MethodPost) {
		return true
	}
	command := ""
	duration := time.Duration(0)
	number := ""
	callID := ""
	switch action {
	case "dial":
		var request struct {
			Number          string `json:"number"`
			DurationSeconds int    `json:"duration_seconds"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return true
		}
		number = strings.TrimSpace(request.Number)
		if !validDialNumber(number) {
			writeError(w, http.StatusBadRequest, "invalid_number", "phone number is invalid")
			return true
		}
		duration = time.Duration(request.DurationSeconds) * time.Second
		if duration < 0 || duration > maxCallDuration {
			writeError(w, http.StatusBadRequest, "invalid_duration", "duration_seconds must be 0 (no automatic hang-up) or between 1 and 600")
			return true
		}
		command = "ATD" + number + ";"
	case "answer":
		var request struct {
			CallID string `json:"call_id"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return true
		}
		callID = strings.TrimSpace(request.CallID)
		command = "ATA"
	case "hangup":
		var request struct {
			CallID string `json:"call_id"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return true
		}
		callID = strings.TrimSpace(request.CallID)
		command = "ATH"
	default:
		writeError(w, http.StatusNotFound, "not_found", "call action not found")
		return true
	}

	transport := s.callTransportForConfig(config)
	if config.Telephony != nil && action != "dial" {
		if callID == "" {
			writeError(w, 400, "call_id_required", "independent access requires an exact call_id")
			return true
		}
		if s.moduleVoiceCall(config.ID, callID) {
			return s.handleModuleVoiceAction(w, r, config, action, number, callID, duration)
		}
		calls, err := (sipCarrierBackend{s}).Calls(r.Context(), config.ID)
		found := false
		for _, call := range calls {
			if call.ID == callID {
				found = true
				break
			}
		}
		owner := s.ownerIMSCall(config.ID, callID)
		if err != nil || !found || owner == nil {
			writeError(w, 409, "call_not_found", "the exact call owner is unavailable or ambiguous")
			return true
		}
		transport = store.IMSModeVoWiFi
		if owner == s.hostIMS {
			transport = store.IMSModeHost
		}
	}
	if transport == "unavailable" {
		writeError(w, http.StatusServiceUnavailable, "ims_unavailable", "the selected line policy is unavailable")
		return true
	}
	if transport == "vowifi" || transport == "host_ims" {
		controller, ok := s.vowifi.(VoWiFiCallController)
		if !ok {
			writeError(w, http.StatusNotImplemented, "vowifi_voice_unavailable", "the active VoWiFi IMS session does not expose voice-call signalling")
			return true
		}
		var result any
		var err error
		switch action {
		case "dial":
			result, err = controller.DialCall(r.Context(), config.ID, number)
		case "answer":
			callID, err = resolveVoWiFiCallID(controller, config.ID, callID, "ringing")
			if err == nil {
				err = s.claimLocalSIPAnswer(config.ID, callID)
			}
			if err == nil {
				result, err = (sipCarrierBackend{s}).Answer(r.Context(), config.ID, callID)
			}
		case "hangup":
			callID, err = resolveVoWiFiCallID(controller, config.ID, callID, "")
			if err == nil {
				err = controller.HangupCall(r.Context(), config.ID, callID)
			}
		}
		if err != nil {
			s.logger.Warn("VoWiFi call operation failed",
				"category", "call", "event", "call."+action,
				"device_id", config.ID, "number", number, "call_id", callID,
				"transport", transport, "raw_error", err,
			)
			writeError(w, http.StatusBadGateway, "vowifi_call_failed", err.Error())
			return true
		}
		if action == "dial" {
			if call, ok := result.(vowifi.Call); ok {
				callID = call.ID
			}
			if duration > 0 {
				go s.hangupVoWiFiAfter(config.ID, callID, duration)
			}
		}
		s.recordAudit(r.Context(), "admin", "call."+action, "device", config.ID, "success", transport)
		writeJSON(w, http.StatusAccepted, map[string]any{"data": map[string]any{
			"accepted": true, "action": action, "number": number, "call_id": callID,
			"duration_seconds": int(duration / time.Second), "transport": transport, "call": result,
		}})
		return true
	}
	if s.useModuleVoice(r.Context(), config, callID) {
		return s.handleModuleVoiceAction(w, r, config, action, number, callID, duration)
	}
	operationContext, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	response, err := s.devices.ExecuteAT(operationContext, physicalID, command)
	cancel()
	if err != nil {
		s.writeDeviceError(w, err)
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(response.Final), "OK") {
		s.logger.Warn("cellular call operation rejected",
			"category", "call", "event", "call."+action,
			"device_id", config.ID, "number", number, "transport", transport,
			"modem_final", response.Final, "raw_response", response.Text(),
		)
		writeError(w, http.StatusBadGateway, "call_rejected", "modem did not accept the call action")
		return true
	}
	if action == "dial" {
		scope := s.beginModuleCallScope(config, physicalID)
		if duration > 0 {
			go s.hangupAfter(config.ID, scope, duration)
		}
	} else {
		s.moduleCallScopes.Delete(config.ID)
	}
	s.recordAudit(r.Context(), "admin", "call."+action, "device", config.ID, "success", transport)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"data": map[string]any{
			"accepted": true, "action": action, "number": number,
			"duration_seconds": int(duration / time.Second), "transport": transport,
		},
	})
	return true
}

func resolveVoWiFiCallID(controller VoWiFiCallController, deviceID, id, requiredState string) (string, error) {
	if id != "" {
		return id, nil
	}
	calls, err := controller.Calls(deviceID)
	if err != nil {
		return "", err
	}
	for _, call := range calls {
		if call.State != "ended" && call.State != "failed" && (requiredState == "" || call.State == requiredState) {
			return call.ID, nil
		}
	}
	return "", errors.New("no matching active call")
}

func (s *Server) hangupVoWiFiAfter(deviceID, callID string, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	<-timer.C
	controller, ok := s.vowifi.(VoWiFiCallController)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := controller.HangupCall(ctx, deviceID, callID); err != nil {
		s.logger.Warn("automatic VoWiFi call hangup failed", "device_id", deviceID, "call_id", callID, "error", err)
	}
}

func (s *Server) callTransport(deviceID string) string {
	if s.store != nil {
		config, err := s.store.Device(context.Background(), deviceID)
		if err != nil {
			return "unavailable"
		}
		return s.callTransportForConfig(config)
	}
	return s.callTransportForConfig(store.Device{ID: deviceID})
}
func (s *Server) callTransportForConfig(config store.Device) string {
	if config.Telephony != nil {
		mode := store.EffectiveIMSMode(config)
		if !store.AccessEnabled(config, mode) {
			return "unavailable"
		}
		if mode == store.IMSModeModule {
			return "cellular"
		}
		return mode
	}
	if config.IMSMode != "" {
		switch config.IMSMode {
		case store.IMSModeHost, store.IMSModeVoWiFi:
			return store.EffectiveIMSMode(config) // No unavailable IMS -> AT fallback.
		case store.IMSModeModule:
			return "cellular"
		default:
			return "unavailable"
		}
	}
	if s.vowifi != nil {
		// Enabled is only the desired card policy. Calls can use IMS only after
		// registration has actually completed; otherwise keep using the modem's
		// circuit-switched call path instead of routing into an unavailable IMS
		// session.
		if state, err := s.vowifi.State(config.ID); err == nil && state.IMSReady {
			return "vowifi"
		}
	}
	return "cellular"
}

type moduleCallScope struct {
	physicalID, usbGeneration, controlDevice, atPort, usbPath, imei string
}

func (s *Server) beginModuleCallScope(config store.Device, physicalID string) *moduleCallScope {
	entry, _, _ := s.physicalForConfig(config)
	scope := &moduleCallScope{physicalID: physicalID, usbGeneration: entry.Candidate.USBGeneration, controlDevice: config.ControlDevice, atPort: config.ATPort, usbPath: config.USBPath, imei: config.ModemIMEI}
	s.moduleCallScopes.Store(config.ID, scope)
	return scope
}
func (s *Server) hangupAfter(deviceID string, scope *moduleCallScope, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	<-timer.C
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.hangupModuleScope(ctx, deviceID, scope)
}
func (s *Server) hangupModuleScope(ctx context.Context, deviceID string, scope *moduleCallScope) {
	unlock, gateErr := s.acquireLineRead(ctx, deviceID)
	if gateErr != nil {
		return
	}
	defer unlock()
	current, ok := s.moduleCallScopes.Load(deviceID)
	if !ok || current != scope || scope == nil || s.store == nil {
		return
	}
	config, err := s.store.Device(ctx, deviceID)
	if err != nil || s.callTransportForConfig(config) != "cellular" || config.ControlDevice != scope.controlDevice || config.ATPort != scope.atPort || config.USBPath != scope.usbPath || config.ModemIMEI != scope.imei {
		return
	}
	entry, physicalID, present := s.physicalForConfig(config)
	if !present || physicalID != scope.physicalID || entry.Candidate.USBGeneration != scope.usbGeneration {
		return
	}
	if _, err := s.devices.ExecuteAT(ctx, physicalID, "ATH"); err != nil {
		s.logger.Warn("automatic call hangup failed", "device_id", deviceID, "error", err)
	}
}

func validDialNumber(value string) bool {
	if len(value) < 2 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if character >= '0' && character <= '9' || (index == 0 && character == '+') || character == '*' || character == '#' {
			continue
		}
		return false
	}
	return true
}

func parseCLCC(response modem.Response) []map[string]any {
	result := make([]map[string]any, 0)
	for _, line := range response.Lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "+CLCC:") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+CLCC:")), ",")
		if len(fields) < 5 {
			continue
		}
		integer := func(index int) int {
			value, _ := strconv.Atoi(strings.TrimSpace(fields[index]))
			return value
		}
		call := map[string]any{
			"index": integer(0), "direction": integer(1), "state": integer(2),
			"mode": integer(3), "multiparty": integer(4), "raw": line,
		}
		if len(fields) > 5 {
			call["number"] = strings.Trim(strings.TrimSpace(fields[5]), `"`)
		}
		result = append(result, call)
	}
	return result
}
