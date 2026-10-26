package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"vocat/internal/callrecord"
	"vocat/internal/store"
)

func (s *Server) routeRecordingsAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	settings := path == "settings/recordings"
	if !settings && path != "recordings" && !strings.HasPrefix(path, "recordings/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.recordings == nil {
		writeError(w, 503, "recordings_unavailable", "call recording storage is unavailable; check server logs and directory permissions")
		return true
	}
	if settings {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, map[string]any{"data": s.recordings.Status()})
		case http.MethodPut:
			var requested callrecord.Config
			if err := s.decodeJSON(w, r, &requested); err != nil {
				writeError(w, 400, "invalid_settings", err.Error())
				return true
			}
			if err := requested.Validate(); err != nil {
				writeError(w, 400, "invalid_settings", err.Error())
				return true
			}
			s.recordingsMu.Lock()
			defer s.recordingsMu.Unlock()
			raw, err := json.Marshal(requested)
			if err != nil {
				s.writeStoreError(w, err)
				return true
			}
			if err = s.store.UpsertAppSetting(r.Context(), store.AppSetting{Key: callrecord.SettingKey, Value: raw}); err != nil {
				s.writeStoreError(w, err)
				return true
			}
			if err = s.recordings.Configure(requested); err != nil {
				writeError(w, 503, "recording_retention_failed", "settings were saved; recording cleanup failed, check storage permissions")
				return true
			}
			s.recordAudit(r.Context(), "admin", "call.recording.settings", "settings", callrecord.SettingKey, "success", "")
			writeJSON(w, 200, map[string]any{"data": s.recordings.Status()})
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, 405, "method_not_allowed", "method not allowed")
		}
		return true
	}
	if path == "recordings" {
		switch r.Method {
		case http.MethodGet:
			device := r.URL.Query().Get("device_id")
			if len(device) > 256 {
				writeError(w, 400, "invalid_device", "invalid device ID")
				return true
			}
			writeJSON(w, 200, map[string]any{"data": map[string]any{"recordings": s.recordings.List(device), "status": s.recordings.Status()}})
		case http.MethodDelete:
			var body struct {
				AllCompleted bool `json:"all_completed"`
			}
			if s.decodeJSON(w, r, &body) != nil || !body.AllCompleted {
				writeError(w, 400, "invalid_cleanup", "all_completed must be explicitly true")
				return true
			}
			s.recordingsMu.Lock()
			defer s.recordingsMu.Unlock()
			removed, failed := 0, 0
			for _, m := range s.recordings.List("") {
				if m.State == "recording" {
					continue
				}
				if err := s.recordings.Delete(m.ID); err != nil {
					failed++
				} else {
					removed++
				}
			}
			outcome := "success"
			if failed > 0 {
				outcome = "partial"
			}
			s.recordAudit(r.Context(), "admin", "call.recording.clear", "recordings", "", outcome, "")
			writeJSON(w, 200, map[string]any{"data": map[string]any{"removed": removed, "failed": failed}})
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, 405, "method_not_allowed", "method not allowed")
		}
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "recordings/"), "/")
	if len(parts) == 2 && parts[1] == "audio" {
		if !requireMethod(w, r, http.MethodGet) {
			return true
		}
		file, m, err := s.recordings.Open(parts[0])
		if err != nil {
			s.writeRecordingError(w, err)
			return true
		}
		defer file.Close()
		w.Header().Set("Content-Type", "audio/wav")
		disposition := "inline"
		if r.URL.Query().Get("download") == "1" {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition", disposition+`; filename="call_`+m.ID+`.wav"`)
		http.ServeContent(w, r, m.ID+".wav", m.StartedAt, file)
		return true
	}
	if len(parts) == 1 {
		if !requireMethod(w, r, http.MethodDelete) {
			return true
		}
		s.recordingsMu.Lock()
		defer s.recordingsMu.Unlock()
		if err := s.recordings.Delete(parts[0]); err != nil {
			s.writeRecordingError(w, err)
			return true
		}
		s.recordAudit(r.Context(), "admin", "call.recording.delete", "recording", parts[0], "success", "")
		writeJSON(w, 200, map[string]any{"data": map[string]any{"deleted": true}})
		return true
	}
	writeError(w, 404, "not_found", "recording endpoint not found")
	return true
}
func (s *Server) writeRecordingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, callrecord.ErrActive):
		writeError(w, 409, "recording_active", "the exact recording is still active")
	case errors.Is(err, os.ErrNotExist):
		writeError(w, 404, "recording_not_found", "recording not found")
	default:
		writeError(w, 503, "recording_storage_failed", "recording storage operation failed; check server storage")
	}
}
