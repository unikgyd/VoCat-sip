//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"vocat/internal/modem"
	"vocat/internal/store"
)

func allowOwnedDataRecovery(ctx context.Context, database *store.Store, candidate modem.Candidate, iccid string) bool {
	configs, err := database.ListDevices(ctx)
	return err == nil && ownedDataRecoveryPolicy(configs, candidate, iccid)
}

func ownedDataRecoveryPolicy(configs []store.Device, candidate modem.Candidate, iccid string) bool {
	if candidate.ID == "" || iccid == "" {
		return false
	}
	matched := 0
	for _, config := range configs {
		if !candidateMatchesTelephonyConfig(candidate, config) {
			if (store.HostIMSConfigured(config) || config.VoWiFiEnabled || store.AccessEnabled(config, store.IMSModeVoWiFi)) && config.USBPath == "" && config.ControlDevice == "" && config.ATPort == "" {
				return false
			}
			continue
		}
		matched++
		if !config.NetworkEnabled || config.Telephony == nil || config.Telephony.ICCID != iccid ||
			store.CellularMode(config) != store.IMSModeModule || store.HostIMSConfigured(config) ||
			config.VoWiFiEnabled || store.AccessEnabled(config, store.IMSModeVoWiFi) {
			return false
		}
	}
	return matched == 1
}

func parseDataOwnerRecoveryPorts(raw string) (map[string]string, error) {
	invalid := errors.New("VOCAT_DATA_OWNER_RECOVERY_PORTS must map exact physical IDs to distinct secondary /dev AT ports")
	if len(raw) > 32768 {
		return nil, invalid
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalid
	}
	ports := map[string]string{}
	for decoder.More() {
		token, err := decoder.Token()
		id, ok := token.(string)
		if err != nil || !ok || id == "" || len(id) > 1024 || strings.ContainsAny(id, "\r\n\x00") || len(ports) >= 32 {
			return nil, invalid
		}
		if _, duplicate := ports[id]; duplicate {
			return nil, invalid
		}
		var port string
		if decoder.Decode(&port) != nil || !filepath.IsAbs(port) || filepath.Clean(port) != port ||
			!strings.HasPrefix(port, "/dev/") || strings.ContainsAny(port, "\r\n\x00") {
			return nil, invalid
		}
		for _, other := range ports {
			if other == port {
				return nil, invalid
			}
		}
		ports[id] = port
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, invalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid
	}
	return ports, nil
}
