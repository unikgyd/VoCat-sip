package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handleAutomaticTakeover(w http.ResponseWriter, r *http.Request) bool {
	if !requireMethod(w, r, http.MethodPost) {
		return true
	}
	client := phoneClient(r)
	var body struct {
		DeviceID string `json:"device_id"`
		CallID   string `json:"call_id"`
	}
	if client == "" || s.decodeJSON(w, r, &body) != nil || body.DeviceID == "" || body.CallID == "" || len(body.DeviceID) > 256 || len(body.CallID) > 256 {
		writeError(w, 400, "invalid_takeover", "an exact call and browser client are required")
		return true
	}
	if s.autoAnswer == nil {
		writeError(w, 409, "takeover_unavailable", "automatic answering is unavailable")
		return true
	}
	s.webPhone.mu.Lock()
	lease := s.webPhone.leases[body.DeviceID]
	ready := lease != nil && lease.call == body.CallID && lease.kind == "auto_answer" && (lease.stage == "playing" || lease.stage == "awaiting_takeover")
	s.webPhone.mu.Unlock()
	if !ready {
		writeError(w, 409, "takeover_unavailable", "the automatic call is not ready for takeover")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	result := s.autoAnswer.takeover(ctx, body.DeviceID, body.CallID, client)
	if result.err != nil {
		writeError(w, 409, "takeover_failed", "takeover failed or the original call ended; check call status")
		return true
	}
	writeJSON(w, 202, map[string]any{"data": map[string]any{"call": result.call, "audio_token": result.lease.token}})
	return true
}
