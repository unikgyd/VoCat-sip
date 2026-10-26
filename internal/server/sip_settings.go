package server

import (
	"net/http"

	"vocat/internal/sipgateway"
)

func (s *Server) handleSIPSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": s.sipGateway.Snapshot()})
	case http.MethodPut:
		var request sipgateway.Config
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid SIP configuration")
			return
		}
		view, err := s.sipGateway.Apply(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "sip_config_failed", err.Error())
			return
		}
		s.recordAudit(r.Context(), "admin", "settings.sip", "settings", "sip", "success", "SIP configuration updated")
		writeJSON(w, http.StatusOK, map[string]any{"data": view})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}
