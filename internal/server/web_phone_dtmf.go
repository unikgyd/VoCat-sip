package server

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handlePhoneDTMF(w http.ResponseWriter, r *http.Request) bool {
	if !requireMethod(w, r, http.MethodPost) {
		return true
	}
	client := phoneClient(r)
	var body struct {
		DeviceID string `json:"device_id"`
		CallID   string `json:"call_id"`
		Digit    string `json:"digit"`
	}
	if client == "" || s.decodeJSON(w, r, &body) != nil || body.DeviceID == "" || body.CallID == "" || len(body.DeviceID) > 256 || len(body.CallID) > 256 || len(body.Digit) != 1 || !strings.ContainsAny(body.Digit, "0123456789*#ABCD") {
		writeError(w, 400, "invalid_dtmf_request", "one digit and an exact device/call are required")
		return true
	}
	s.webPhone.mu.Lock()
	l := s.webPhone.leases[body.DeviceID]
	owned := !s.webPhone.closed && l != nil && l.call == body.CallID && l.client == client && l.attached
	var controlContext context.Context
	if owned {
		controlContext = l.controlContext
	}
	s.webPhone.mu.Unlock()
	if !owned {
		writeError(w, 409, "call_not_owned", "the connected browser must own this exact call")
		return true
	}
	if s.sipGateway != nil && s.sipGateway.CarrierOwnsMedia(body.DeviceID, body.CallID) {
		writeError(w, 409, "call_not_owned", "the call belongs to a SIP extension")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if controlContext != nil {
		stop := context.AfterFunc(controlContext, cancel)
		defer stop()
	}
	if err := (sipCarrierBackend{s}).SendDTMF(ctx, body.DeviceID, body.CallID, body.Digit[0]); err != nil {
		writeError(w, 409, "dtmf_failed", "key submission failed or is uncertain; it will not be retried automatically")
		return true
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"submitted": true}})
	return true
}
