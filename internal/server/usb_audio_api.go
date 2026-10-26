package server

import (
	"context"
	"errors"
	"net/http"

	"vocat/internal/modemaudio"
)

type usbAudioInspector interface {
	USBAudioBinding(context.Context, string) (modemaudio.Info, error)
}

func (s *Server) handleUSBAudioBinding(w http.ResponseWriter, r *http.Request, id string) bool {
	if !requireMethod(w, r, http.MethodGet) {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	controller, ok := s.devices.(usbAudioInspector)
	if !ok {
		writeError(w, http.StatusNotImplemented, "usb_audio_unavailable", "USB audio binding inspection is unavailable")
		return true
	}
	info, err := controller.USBAudioBinding(r.Context(), id)
	if err != nil {
		if errors.Is(err, modemaudio.ErrChanged) {
			writeError(w, http.StatusConflict, "usb_audio_changed", "the physical device changed; rescan before using audio")
		} else {
			s.writeDeviceError(w, err)
		}
		return true
	}
	// No existing IMS/module call policy becomes usable merely by finding UAC.
	info.VoiceBridgeReady = false
	writeJSON(w, http.StatusOK, map[string]any{"data": info})
	return true
}
