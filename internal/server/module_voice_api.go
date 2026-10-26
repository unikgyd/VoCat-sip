package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func (s *Server) useModuleVoice(ctx context.Context, config store.Device, callID string) bool {
	return s.moduleVoiceCall(config.ID, callID) || s.moduleVoiceSelected(ctx, config.ID) && (s.moduleCalls.Busy(config.ID) || s.moduleCalls.Available(ctx, config.ID) == nil)
}
func (s *Server) handleModuleVoiceAction(w http.ResponseWriter, r *http.Request, config store.Device, action, number, callID string, duration time.Duration) bool {
	backend := sipCarrierBackend{s}
	var call vowifi.Call
	var err error
	switch action {
	case "dial":
		call, err = backend.Dial(r.Context(), config.ID, number)
	case "answer":
		callID, err = resolveVoWiFiCallID(s.moduleCalls, config.ID, callID, "ringing")
		if err == nil {
			err = s.claimLocalSIPAnswer(config.ID, callID)
		}
		if err == nil {
			call, err = backend.Answer(r.Context(), config.ID, callID)
		}
	case "hangup":
		callID, err = resolveVoWiFiCallID(s.moduleCalls, config.ID, callID, "")
		if err == nil {
			err = backend.Hangup(r.Context(), config.ID, callID)
		}
	default:
		err = errors.New("unsupported module voice action")
	}
	if call.ID != "" {
		callID = call.ID
	}
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "module_call_failed", "message": "module call action failed or remains unconfirmed; check the exact call before retrying"}, "data": map[string]any{"call_id": callID, "call": call, "transport": "cellular"}})
		return true
	}
	if action == "dial" && duration > 0 {
		go s.hangupOwnedModuleAfter(config.ID, callID, duration)
	}
	s.recordAudit(r.Context(), "admin", "call."+action, "device", config.ID, "success", "module_usb")
	writeJSON(w, 202, map[string]any{"data": map[string]any{"accepted": true, "action": action, "call_id": callID, "call": call, "transport": "cellular", "duration_seconds": int(duration / time.Second)}})
	return true
}
func (s *Server) hangupOwnedModuleAfter(id, callID string, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	<-timer.C
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = (sipCarrierBackend{s}).Hangup(ctx, id, callID)
}
