package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"vocat/internal/device"
	"vocat/internal/exportproxy"
	"vocat/internal/store"
)

type cachedPublicIP struct {
	ICCID      string
	Info       exportproxy.PublicIPInfo
	Generation uint64
}

type publicIPResponse struct {
	Detected bool `json:"detected"`
	exportproxy.PublicIPInfo
}

func (s *Server) clearPublicIP(deviceID string) {
	s.publicIPMu.Lock()
	delete(s.publicIPs, strings.TrimSpace(deviceID))
	s.publicIPMu.Unlock()
}

func (s *Server) loadPublicIP(deviceID, iccid string) (exportproxy.PublicIPInfo, bool) {
	deviceID = strings.TrimSpace(deviceID)
	iccid = strings.TrimSpace(iccid)
	s.publicIPMu.RLock()
	entry, ok := s.publicIPs[deviceID]
	s.publicIPMu.RUnlock()
	if !ok {
		return exportproxy.PublicIPInfo{}, false
	}
	// A missing live ICCID means the modem is resetting or no card is present.
	// A different ICCID means the SIM/eSIM profile changed. Either transition
	// invalidates the old cellular exit immediately.
	if iccid == "" || !strings.EqualFold(strings.TrimSpace(entry.ICCID), iccid) ||
		entry.Generation == 0 || s.cellularDataRuntime().activeDataGeneration(deviceID, iccid) != entry.Generation {
		s.clearPublicIP(deviceID)
		return exportproxy.PublicIPInfo{}, false
	}
	return entry.Info, true
}

func (s *Server) savePublicIP(deviceID, iccid string, info exportproxy.PublicIPInfo, generations ...uint64) {
	generation := s.cellularDataRuntime().activeDataGeneration(deviceID, iccid)
	if generation == 0 || len(generations) > 0 && generation != generations[0] {
		return
	}
	s.publicIPMu.Lock()
	if s.publicIPs == nil {
		s.publicIPs = make(map[string]cachedPublicIP)
	}
	s.publicIPs[strings.TrimSpace(deviceID)] = cachedPublicIP{
		ICCID:      strings.TrimSpace(iccid),
		Info:       info,
		Generation: generation,
	}
	s.publicIPMu.Unlock()
}

func (s *Server) lookupCellularPublicIP(ctx context.Context, configID, networkInterface string) (exportproxy.PublicIPInfo, uint64, error) {
	operation, release, err := s.acquireCellularTransfer(ctx, configID, networkInterface)
	if err != nil {
		return exportproxy.PublicIPInfo{}, 0, err
	}
	defer release()
	config, err := s.store.Device(operation, configID)
	if err != nil {
		return exportproxy.PublicIPInfo{}, 0, err
	}
	entry, _, present := s.physicalForConfig(config)
	if !present || entry.Snapshot == nil {
		return exportproxy.PublicIPInfo{}, 0, device.ErrSIMObservation
	}
	generation := s.cellularDataRuntime().activeDataGeneration(configID, strings.TrimSpace(entry.Snapshot.ICCID))
	if generation == 0 {
		return exportproxy.PublicIPInfo{}, 0, device.ErrDataCleanupPending
	}
	lookup := s.lookupPublicIP
	if lookup == nil {
		lookup = exportproxy.LookupPublicIP
	}
	info, err := lookup(operation, networkInterface)
	return info, generation, errors.Join(err, operation.Err())
}

func (s *Server) overviewPublicIP(config store.Device, entry *device.Device, developerActive bool) map[string]any {
	result := map[string]any{"detected": false}
	if !developerActive || !store.CellularDataEnabled(config) || entry == nil || entry.Snapshot == nil {
		return result
	}
	iccid := strings.TrimSpace(entry.Snapshot.ICCID)
	if info, ok := s.loadPublicIP(config.ID, iccid); ok {
		result["detected"] = true
		result["ip"] = info.IP
		result["country_code"] = info.CountryCode
		result["region"] = info.Region
		result["city"] = info.City
		result["organization"] = info.Organization
	}
	return result
}

// schedulePublicIPDetection runs after the cellular runtime has completed a
// successful enable/recovery transition. Keeping this on the backend covers
// manual toggles, modem reboots, IMS-triggered reboots, and service startup;
// the web UI only reads the resulting cache.
func (s *Server) schedulePublicIPDetection(configID, physicalID string, revision uint64) {
	if s == nil || s.store == nil || s.devices == nil || !s.developerActive(context.Background()) {
		return
	}
	runtime := s.cellularDataRuntime()
	go func() {
		if !runtime.isCurrent(configID, revision, "connected") {
			return
		}
		ctx, cancel := context.WithTimeout(runtime.rootContext(), 15*time.Second)
		defer cancel()

		config, err := s.store.Device(ctx, configID)
		if err != nil || !store.CellularDataEnabled(config) || strings.TrimSpace(config.Interface) == "" {
			return
		}
		entry, err := s.devices.Get(physicalID)
		if err != nil || entry.Snapshot == nil {
			return
		}
		iccid := strings.TrimSpace(entry.Snapshot.ICCID)
		if !validICCID(iccid) {
			return
		}

		info, generation, err := s.lookupCellularPublicIP(ctx, config.ID, config.Interface)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("automatic roaming public IP detection failed", "device_id", configID, "interface", config.Interface, "error", err)
			}
			return
		}
		// Do not repopulate a cache after the user disabled data or changed the
		// active SIM while the lookup was in flight.
		if !runtime.isCurrent(configID, revision, "connected") {
			return
		}
		latest, err := s.store.Device(ctx, configID)
		if err != nil || !store.CellularDataEnabled(latest) || latest.Interface != config.Interface {
			return
		}
		latestEntry, err := s.devices.Get(physicalID)
		if err != nil || latestEntry.Snapshot == nil || !strings.EqualFold(strings.TrimSpace(latestEntry.Snapshot.ICCID), iccid) {
			return
		}
		s.savePublicIP(configID, iccid, info, generation)
		if s.logger != nil {
			s.logger.Info("automatic roaming public IP detected", "device_id", configID, "interface", config.Interface)
		}
	}()
}

func (s *Server) handleCellularPublicIP(w http.ResponseWriter, r *http.Request, config store.Device, iccid string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return true
	}
	if !s.developerActive(r.Context()) {
		writeError(w, http.StatusForbidden, "developer_mode_required", "public IP detection through roaming data is available only in developer mode")
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		if !store.CellularDataEnabled(config) {
			// A cached result describes the previous cellular session. Once the
			// desired data state is off, never expose that stale exit again.
			s.clearPublicIP(config.ID)
			writeJSON(w, http.StatusOK, map[string]any{"data": publicIPResponse{}})
			return true
		}
		info, ok := s.loadPublicIP(config.ID, iccid)
		writeJSON(w, http.StatusOK, map[string]any{"data": publicIPResponse{Detected: ok, PublicIPInfo: info}})
		return true
	}
	if !store.CellularDataEnabled(config) {
		writeError(w, http.StatusConflict, "cellular_data_disabled", "enable roaming data before detecting its public IP")
		return true
	}
	if strings.TrimSpace(iccid) == "" {
		writeError(w, http.StatusConflict, "sim_identity_unavailable", "the modem has no current ICCID; refresh it before detecting the public IP")
		return true
	}
	if strings.TrimSpace(config.Interface) == "" {
		writeError(w, http.StatusConflict, "cellular_interface_missing", "the device has no cellular network interface")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info, generation, err := s.lookupCellularPublicIP(ctx, config.ID, config.Interface)
	if err != nil {
		if errors.Is(err, device.ErrDataCleanupPending) || errors.Is(err, device.ErrDataBackendUnavailable) || errors.Is(err, device.ErrSIMObservation) ||
			errors.Is(err, errLineBusy) || errors.Is(err, store.ErrAutomaticTaskRecoveryPending) {
			writeError(w, http.StatusConflict, "cellular_data_unconfirmed", "wait for the selected SIM's owned data session to finish connecting or restoring")
			return true
		}
		s.logger.Warn("detect roaming public IP failed", "device_id", config.ID, "interface", config.Interface, "error", err)
		writeError(w, http.StatusBadGateway, "public_ip_lookup_failed", err.Error())
		return true
	}
	s.savePublicIP(config.ID, iccid, info, generation)
	writeJSON(w, http.StatusOK, map[string]any{"data": publicIPResponse{Detected: true, PublicIPInfo: info}})
	return true
}
