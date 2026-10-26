package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handlePhoneHold(w http.ResponseWriter, r *http.Request) bool {
	if !requireMethod(w, r, http.MethodPost) {
		return true
	}
	client := phoneClient(r)
	var body struct {
		DeviceID string `json:"device_id"`
		CallID   string `json:"call_id"`
		Held     *bool  `json:"held"`
	}
	if client == "" || s.decodeJSON(w, r, &body) != nil || body.DeviceID == "" || body.CallID == "" || len(body.DeviceID) > 256 || len(body.CallID) > 256 || body.Held == nil {
		writeError(w, 400, "invalid_hold_request", "an exact device/call and an explicit held flag are required")
		return true
	}
	s.webPhone.mu.Lock()
	lease := s.webPhone.leases[body.DeviceID]
	owned := !s.webPhone.closed && lease != nil && lease.call == body.CallID && lease.client == client && lease.attached
	var lifetime context.Context
	if owned {
		lifetime = lease.controlContext
	}
	s.webPhone.mu.Unlock()
	if !owned || s.sipGateway != nil && s.sipGateway.CarrierOwnsMedia(body.DeviceID, body.CallID) {
		writeError(w, 409, "call_not_owned", "the connected browser must own this exact call")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()
	if lifetime != nil {
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
	}
	call, err := (sipCarrierBackend{s}).SetCallHold(ctx, body.DeviceID, body.CallID, *body.Held)
	if err != nil {
		writeError(w, 409, "hold_change_failed", "hold change failed or is unconfirmed; inspect the call state before another action")
		return true
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"call": call}})
	return true
}
