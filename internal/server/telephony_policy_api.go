package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"vocat/internal/device"
	"vocat/internal/store"
)

func (s *Server) telephonyPolicyResponse(config store.Device, iccid string) map[string]any {
	p := config.Telephony
	if p != nil && (iccid == "" || p.ICCID != iccid) {
		p = nil
	}
	if config.Telephony == nil && iccid != "" {
		legacy := store.LegacyTelephonyPolicy(config, iccid)
		if entry, _, present := s.physicalForConfig(config); present && entry.Snapshot != nil && entry.Snapshot.FlightMode && legacy.CellularMode == store.IMSModeModule {
			legacy.AutoConnectCellular = false
		}
		p = &legacy
	}
	result := map[string]any{"policy": p, "legacy": config.Telephony == nil, "subscription_pending": p == nil}
	if c := s.wifiController(); c != nil {
		if state, err := c.State(config.ID); err == nil {
			result["wifi_runtime"] = state
		}
	}
	if store.HostIMSConfigured(config) && s.hostIMS != nil {
		if state, err := s.hostIMS.State(config.ID); err == nil {
			result["cellular_runtime"] = state
		}
	}
	return result
}

func (s *Server) handleTelephonyPolicy(w http.ResponseWriter, r *http.Request, config store.Device) bool {
	entry, physical, present := s.physicalForConfig(config)
	iccid := snapshotString(entry.Snapshot, func(v *device.Snapshot) string { return strings.TrimSpace(v.ICCID) })
	if r.Method == http.MethodGet {
		if identity, err := s.observedTelephonySubscription(r.Context(), config); err == nil {
			iccid = identity.ICCID
		} else {
			iccid = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": s.telephonyPolicyResponse(config, iccid)})
		return true
	}
	if !requireMethod(w, r, http.MethodPatch) {
		return true
	}
	var requested store.TelephonyPolicy
	if err := s.decodeJSON(w, r, &requested); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return true
	}
	if err := requested.Validate(); err != nil {
		writeError(w, 400, "invalid_telephony_policy", err.Error())
		return true
	}
	gate := s.imsModeLock(config.ID)
	if !gate.TryLock() {
		writeError(w, 409, "ims_busy", "a line operation is in progress")
		return true
	}
	defer gate.Unlock()
	current, err := s.store.Device(r.Context(), config.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return true
	}
	entry, physical, present = s.physicalForConfig(current)
	if !s.requirePhysicalDevice(w, present) {
		return true
	}
	identity, identityErr := s.observedTelephonySubscription(r.Context(), current)
	iccid = identity.ICCID
	if identityErr != nil {
		writeError(w, 409, "sim_identity_unavailable", "the live subscription could not be verified")
		return true
	}
	if iccid == "" || requested.ICCID != iccid || current.Telephony != nil && current.Telephony.ICCID != iccid {
		writeError(w, 409, "sim_identity_changed", "the observed SIM must match the active subscription before editing its policy")
		return true
	}
	if current.DeviceType == store.DeviceTypeUSBSIMReader && requested.AutoConnectCellular && (current.Telephony == nil || !current.Telephony.AutoConnectCellular || current.Telephony.CellularMode != requested.CellularMode) {
		writeError(w, 400, "cellular_unavailable", "this SIM reader has no cellular access")
		return true
	}
	if requested.AutoConnectCellular || requested.AutoConnectWiFi {
		if identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) {
			writeError(w, 409, "sim_identity_unavailable", "a verified IMSI is required to enable access")
			return true
		}
		if reason := device.RegionBlockReason(identity.IMSI); reason != "" {
			writeError(w, 403, "region_blocked", reason)
			return true
		}
	}
	previous := store.LegacyTelephonyPolicy(current, iccid)
	if current.Telephony == nil && entry.Snapshot != nil && entry.Snapshot.FlightMode && previous.CellularMode == store.IMSModeModule {
		previous.AutoConnectCellular = false
	}
	if current.Telephony != nil {
		previous = *current.Telephony
	}
	cellChanged := previous.CellularMode != requested.CellularMode || previous.AutoConnectCellular != requested.AutoConnectCellular
	wifiChanged := previous.AutoConnectWiFi != requested.AutoConnectWiFi || previous.WiFiRadioPolicy != requested.WiFiRadioPolicy
	if requested.AutoConnectWiFi && s.wifiController() == nil || requested.AutoConnectCellular && requested.CellularMode == store.IMSModeHost && s.hostIMS == nil {
		writeError(w, 503, "ims_unavailable", "the requested access provider is unavailable")
		return true
	}
	if requested.AutoConnectWiFi && requested.WiFiRadioPolicy == "exclusive" && current.NetworkEnabled {
		writeError(w, 409, "network_enabled", "disable the data session before starting exclusive Wi-Fi access")
		return true
	}
	if cellChanged && requested.AutoConnectCellular && requested.CellularMode == store.IMSModeHost && current.NetworkEnabled {
		writeError(w, 409, "network_cleanup_pending", "the previous cellular data owner must finish cleanup before Host IMS")
		return true
	}
	if cellChanged && previous.CellularMode == store.IMSModeModule || wifiChanged && (previous.WiFiRadioPolicy == "exclusive" || requested.WiFiRadioPolicy == "exclusive") {
		if err := s.requireModuleCallsIdle(r.Context(), current); err != nil {
			writeError(w, 409, "module_call_state_unknown_or_active", err.Error())
			return true
		}
	}
	if cellChanged && previous.CellularMode == store.IMSModeHost {
		if s.hostIMS == nil && previous.AutoConnectCellular {
			writeError(w, 409, "ims_cleanup_pending", "the previous Host IMS owner is unavailable")
			return true
		}
		if err := imsActiveCalls(s.hostIMS, current.ID); err != nil {
			writeError(w, 409, "ims_call_active", err.Error())
			return true
		}
	}
	if wifiChanged {
		if err := imsActiveCalls(s.wifiController(), current.ID); err != nil {
			writeError(w, 409, "ims_call_active", err.Error())
			return true
		}
	}
	// A route-only edit reaches none of these lifecycle operations. Its active
	// calls and media leases retain their original control and physical owner.
	if wifiChanged {
		if err := s.stopTelephonyController(r.Context(), current.ID, store.IMSModeVoWiFi); err != nil {
			writeError(w, 409, "ims_cleanup_pending", "Wi-Fi cleanup has not been acknowledged")
			return true
		}
		defer endIMSMaintenance(s.wifiController(), current.ID)
	}
	if cellChanged && previous.CellularMode == store.IMSModeHost {
		if err := s.stopTelephonyController(r.Context(), current.ID, store.IMSModeHost); err != nil {
			writeError(w, 409, "ims_cleanup_pending", "Host IMS cleanup has not been acknowledged")
			return true
		}
		defer endIMSMaintenance(s.hostIMS, current.ID)
	}
	dataHandover := cellChanged && requested.CellularMode == store.IMSModeHost || wifiChanged && requested.AutoConnectWiFi && requested.WiFiRadioPolicy == "exclusive"
	if dataHandover {
		if err := s.quiesceOwnedData(r.Context(), current); err != nil {
			writeError(w, 409, "network_cleanup_pending", "the previous data owner's cleanup has not been confirmed")
			return true
		}
		current.NetworkEnabled = false
	}
	latest, latestPhysical, found := s.physicalForConfig(current)
	again, identityErr := s.observedTelephonySubscription(r.Context(), current)
	if identityErr != nil || !found || latestPhysical != physical || latest.Candidate.USBGeneration != entry.Candidate.USBGeneration || again.ICCID != iccid || again.IMSI != identity.IMSI {
		writeError(w, 409, "sim_identity_changed", "hardware or SIM changed while stopping access; no new policy was saved")
		return true
	}
	card, err := s.store.CardPolicy(r.Context(), iccid)
	if errors.Is(err, store.ErrNotFound) {
		card = store.CardPolicy{ICCID: iccid, IPVersion: "IPV4V6", Source: "telephony"}
	} else if err != nil {
		s.writeStoreError(w, err)
		return true
	}
	card.VoWiFiEnabled = false // independent access intent now lives in its own per-SIM policy
	if cellChanged && requested.AutoConnectCellular {
		card.AirplaneEnabled = false
	}
	if err := s.store.SaveTelephonyPolicy(r.Context(), current, requested, card); err != nil {
		s.writeStoreError(w, err)
		return true
	}
	current.Telephony = &requested
	if dataHandover {
		s.cellularDataRuntime().resume(current.ID)
	}
	if wifiChanged {
		endIMSMaintenance(s.wifiController(), current.ID)
	}
	if cellChanged {
		endIMSMaintenance(s.hostIMS, current.ID)
	}
	if cellChanged || wifiChanged {
		if err := s.startTelephonyAccess(r.Context(), current, physical); err != nil {
			writeError(w, 503, "access_start_pending", "policy was saved; one or more access starts remain pending and will be retried independently")
			return true
		}
	}
	s.recordAudit(context.Background(), "admin", "device.telephony.policy", "device", current.ID, "success", requested.DefaultRoute)
	writeJSON(w, http.StatusAccepted, map[string]any{"data": s.telephonyPolicyResponse(current, iccid)})
	return true
}

func (s *Server) handleTelephonyReconnect(w http.ResponseWriter, r *http.Request, config store.Device, access string) bool {
	if !requireMethod(w, r, http.MethodPost) {
		return true
	}
	if access == "" {
		var request struct {
			Access string `json:"access"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return true
		}
		access = request.Access
	}
	if access != store.IMSModeModule && access != store.IMSModeHost && access != store.IMSModeVoWiFi {
		writeError(w, 400, "invalid_access", "access must be module, host_ims or vowifi")
		return true
	}
	unlock, gateErr := s.acquireLineRead(r.Context(), config.ID)
	if gateErr != nil {
		writeError(w, 409, "line_busy", gateErr.Error())
		return true
	}
	defer unlock()
	current, err := s.store.Device(r.Context(), config.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return true
	}
	if current.Telephony == nil || !store.AccessEnabled(current, access) {
		writeError(w, 409, "access_disabled", "enable the requested access before reconnecting")
		return true
	}
	_, physical, present := s.physicalForConfig(current)
	if !s.requirePhysicalDevice(w, present) {
		return true
	}
	identity, err := s.observedTelephonySubscription(r.Context(), current)
	if err != nil || identity.ICCID != current.Telephony.ICCID {
		writeError(w, 409, "sim_identity_changed", "the active SIM no longer matches the saved access policy")
		return true
	}
	if access == store.IMSModeModule {
		if err := s.requireModuleCallsIdle(r.Context(), current); err != nil {
			writeError(w, 409, "module_call_state_unknown_or_active", err.Error())
			return true
		}
		if _, err := s.setSubscriptionFlight(r.Context(), physical, identity, false); err != nil {
			s.writeDeviceError(w, err)
			return true
		}
		writeJSON(w, 202, map[string]any{"data": map[string]any{"accepted": true, "access": access}})
		return true
	}
	controller := s.controllerForMode(access)
	if controller == nil {
		writeError(w, 503, "ims_unavailable", "the access provider is unavailable")
		return true
	}
	if err := imsActiveCalls(controller, current.ID); err != nil {
		writeError(w, 409, "ims_call_active", err.Error())
		return true
	}
	state, err := controller.RequestReconnect(current.ID)
	if err != nil {
		s.writeVoWiFiError(w, err)
		return true
	}
	writeJSON(w, 202, map[string]any{"data": map[string]any{"accepted": true, "access": access, "runtime": state}})
	return true
}
