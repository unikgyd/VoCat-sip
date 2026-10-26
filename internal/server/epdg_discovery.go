package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/integration"
)

// Configuration only: no SIM APDU, DNS query, registration or mode switch.
// Existing authenticated/CSRF-protected device routing owns access control.
func (s *Server) handleEPDGDiscovery(w http.ResponseWriter, r *http.Request, config store.Device) bool {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or PUT")
		return true
	}
	entry, _, _ := s.physicalForConfig(config)
	iccid := strings.TrimSpace(snapshotString(entry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.ICCID }))
	if iccid == "" && s.vowifi != nil {
		if state, err := s.vowifi.State(config.ID); err == nil && state.SIMReady {
			iccid = strings.TrimSpace(state.ICCID)
		}
	}
	if iccid == "" {
		writeError(w, http.StatusConflict, "verified_sim_required", "A verified SIM is required before configuring ePDG discovery")
		return true
	}
	settings := integration.EPDGStore{Store: s.store}
	fingerprint := sha256.Sum256([]byte(iccid))
	token := hex.EncodeToString(fingerprint[:])
	type view struct {
		vowifi.EPDGDiscoveryOptions
		SIMToken string `json:"sim_token"`
	}
	if r.Method == http.MethodPut {
		var request view
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return true
		}
		if request.SIMToken != token {
			writeError(w, http.StatusConflict, "sim_changed", "SIM changed; reload the ePDG settings before saving")
			return true
		}
		options := request.EPDGDiscoveryOptions
		if _, err := options.Normalize(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_epdg", err.Error())
			return true
		}
		saved, err := settings.SetConfig(r.Context(), iccid, options)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "epdg_save_failed", "ePDG settings could not be saved")
			return true
		}
		s.recordAudit(r.Context(), "admin", "device.vowifi.discovery", "device", config.ID, "success", "ePDG discovery settings updated; effective on next connection")
		writeJSON(w, http.StatusOK, map[string]any{"data": view{saved, token}})
		return true
	}
	options, err := settings.Config(r.Context(), iccid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "epdg_load_failed", "ePDG settings could not be read")
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": view{options, token}})
	return true
}
