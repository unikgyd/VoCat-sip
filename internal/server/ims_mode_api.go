package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type imsDeviceStopper interface {
	StopDevice(context.Context, string) error
}

func stopIMSController(ctx context.Context, controller VoWiFiController, id string) error {
	if controller == nil {
		return nil
	}
	if stopper, ok := controller.(imsDeviceStopper); ok {
		return stopper.StopDevice(ctx, id)
	}
	// Older embedders can prove an already idle instance; an asynchronous
	// disable followed by a momentarily idle State is NOT a cleanup ACK.
	state, err := controller.State(id)
	if err == nil && !state.Enabled && !state.Active && !state.IMSReady && state.Phase == vowifi.PhaseIdle && len(state.CleanupErrors) == 0 {
		return nil
	}
	return errors.New("IMS controller cannot acknowledge completed cleanup")
}
func endIMSMaintenance(controller VoWiFiController, id string) {
	if maintenance, ok := controller.(VoWiFiMaintenanceController); ok {
		maintenance.EndMaintenance(id)
	}
}
func imsActiveCalls(controller VoWiFiController, id string) error {
	calls, ok := controller.(VoWiFiCallController)
	if !ok {
		return nil
	}
	active, err := calls.Calls(id)
	if err != nil {
		state, stateErr := controller.State(id)
		if stateErr == nil && !state.Active && !state.AccessReady && !state.IMSReady && (state.Phase == vowifi.PhaseIdle || state.Phase == vowifi.PhaseFailed) {
			return nil
		}
		return err
	}
	for _, call := range active {
		if call.EndedAt == nil && call.State != "ended" && call.State != "failed" || call.SignalingPending {
			return errors.New("finish the active IMS call before changing line mode")
		}
	}
	return nil
}
func (s *Server) imsModeResponse(config store.Device) map[string]any {
	data := map[string]any{"mode": store.EffectiveIMSMode(config), "enabled": store.EffectiveIMSEnabled(config)}
	if controller := s.controllerForMode(store.EffectiveIMSMode(config)); controller != nil {
		if state, err := controller.State(config.ID); err == nil {
			data["runtime"] = state
		}
	}
	return data
}
func (s *Server) handleIMSMode(w http.ResponseWriter, r *http.Request, config store.Device, physicalPresent bool) bool {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"data": s.imsModeResponse(config)})
		return true
	}
	if config.Telephony != nil {
		writeError(w, 409, "independent_telephony_policy", "use the telephony policy endpoint for independently configured access")
		return true
	}
	if !requireMethod(w, r, http.MethodPatch) {
		return true
	}
	var request struct {
		Mode    string `json:"mode"`
		Enabled *bool  `json:"enabled"`
	}
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return true
	}
	request.Mode = strings.ToLower(strings.TrimSpace(request.Mode))
	if request.Enabled == nil || request.Mode != store.IMSModeModule && request.Mode != store.IMSModeVoWiFi && request.Mode != store.IMSModeHost || request.Mode == store.IMSModeModule && *request.Enabled {
		writeError(w, http.StatusBadRequest, "invalid_ims_mode", "mode must be module, vowifi or host_ims with an explicit enabled flag; module is not an IMS runtime")
		return true
	}
	unlock, gateErr := s.acquireLineWrite(r.Context(), config.ID)
	if gateErr != nil {
		writeError(w, http.StatusConflict, "ims_busy", gateErr.Error())
		return true
	}
	defer unlock()
	current, err := s.store.Device(r.Context(), config.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return true
	}
	oldMode := store.EffectiveIMSMode(current)
	old, next := s.controllerForMode(oldMode), s.controllerForMode(request.Mode)
	if current.IMSMode == "" && s.wifiController() != nil {
		old = s.wifiController()
	}
	if old == nil && oldMode != store.IMSModeModule && store.EffectiveIMSEnabled(current) {
		writeError(w, http.StatusConflict, "ims_cleanup_pending", "the current IMS owner is unavailable; completed cleanup cannot be assumed")
		return true
	}
	if *request.Enabled {
		if !s.requirePhysicalDevice(w, physicalPresent) {
			return true
		}
		if next == nil {
			writeError(w, http.StatusServiceUnavailable, "ims_unavailable", "the selected IMS provider is unavailable")
			return true
		}
		if current.NetworkEnabled {
			writeError(w, http.StatusConflict, "network_enabled", "disable the existing data session before enabling an IMS line")
			return true
		}
		if request.Mode == store.IMSModeHost && s.legacyDataOwnerBusy(current.ID) {
			writeError(w, http.StatusConflict, "network_cleanup_pending", "the previous data owner has not completed cleanup; retry before enabling Host IMS")
			return true
		}
		entry, _, _ := s.physicalForConfig(current)
		if reason := device.RegionBlockReason(snapshotString(entry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMSI })); reason != "" {
			writeError(w, http.StatusForbidden, "region_blocked", reason)
			return true
		}
	}
	if oldMode == store.IMSModeModule && request.Mode != store.IMSModeModule && s.callTransportForConfig(current) == "cellular" {
		if err := s.requireModuleCallsIdle(r.Context(), current); err != nil {
			writeError(w, http.StatusConflict, "module_call_state_unknown_or_active", err.Error())
			return true
		}
	}
	if err := imsActiveCalls(old, current.ID); err != nil {
		writeError(w, http.StatusConflict, "ims_call_active", "the current line has an active call or unconfirmed signaling")
		return true
	}
	if err := stopIMSController(r.Context(), old, current.ID); err != nil {
		writeError(w, http.StatusConflict, "ims_cleanup_pending", "the previous line has not acknowledged complete cleanup; retry without switching")
		return true
	}
	defer endIMSMaintenance(old, current.ID)
	s.moduleCallScopes.Delete(current.ID)
	current.IMSMode, current.IMSEnabled = request.Mode, *request.Enabled
	current.VoWiFiEnabled = request.Mode == store.IMSModeVoWiFi && *request.Enabled
	if err := s.store.UpsertDevice(r.Context(), current); err != nil {
		s.writeStoreError(w, err)
		return true
	}
	// Entering Host IMS never calls SetFlight/EnterVoWiFiRFOff. The cellular
	// radio/registration prerequisite is proved by its normal owned factory.
	if *request.Enabled {
		endIMSMaintenance(next, current.ID)
		if _, err := next.RequestEnabled(current.ID, true); err != nil {
			writeError(w, http.StatusServiceUnavailable, "ims_start_failed", "the selected IMS line could not be queued; saved policy is retained for retry")
			return true
		}
	}
	s.recordAudit(r.Context(), "admin", "device.ims.mode", "device", current.ID, "success", request.Mode)
	writeJSON(w, http.StatusAccepted, map[string]any{"data": s.imsModeResponse(current)})
	return true
}

func (s *Server) requireModuleCallsIdle(ctx context.Context, config store.Device) error {
	if s.moduleCalls != nil && s.moduleCalls.Busy(config.ID) {
		return errors.New("finish the owned module call and confirmed cleanup before changing the line")
	}
	if store.HostIMSConfigured(config) {
		return nil
	}
	typeOf := store.NormalizeDeviceType(config.DeviceType)
	if typeOf == store.DeviceTypeUSBSIMReader || typeOf == store.DeviceTypeWiFi410 {
		return nil
	} // These expose no module voice-call API.
	_, physicalID, present := s.physicalForConfig(config)
	if !present || s.devices == nil {
		return errors.New("module call state is unavailable; stop the module call before switching")
	}
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	response, err := s.devices.ExecuteAT(query, physicalID, "AT+CLCC")
	if err != nil || !response.OK() {
		return errors.New("the module did not confirm an idle call state; retry without switching")
	}
	if len(parseCLCC(response)) != 0 {
		return errors.New("finish the active module call before changing line mode")
	}
	for _, line := range response.Lines {
		if strings.HasPrefix(strings.TrimSpace(line), "+CLCC:") {
			return errors.New("the module returned an unrecognized call record; idle state cannot be assumed")
		}
	}
	return nil
}
