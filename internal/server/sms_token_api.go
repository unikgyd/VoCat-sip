package server

import (
	"net/http"
	"vocat/internal/smsapi"
)

// The settings route stays behind the administrator session + CSRF. The
// independent listener does not share this router or its cookies.
func (s *Server) handleSMSTokenAPISettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private")
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"data": s.smsAPI.Status()})
	case http.MethodPut:
		var update smsapi.Update
		if err := s.decodeJSON(w, r, &update); err != nil {
			writeError(w, 400, "invalid_request", "Invalid SMS API settings.")
			return
		}
		status, err := s.smsAPI.Apply(r.Context(), update)
		if err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return
		}
		s.recordAudit(r.Context(), "admin", "sms_api.settings.update", "settings", smsapi.SettingKey, "success", "read-only SMS API settings updated")
		writeJSON(w, 200, map[string]any{"data": status})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, 405, "method_not_allowed", "Method not allowed.")
	}
}
