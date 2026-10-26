package main

import (
	"path"
	"strings"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

type hostIMSPhysicalGuard struct {
	resolve  func(store.Device, []device.Device) (string, error)
	disjoint func(store.Device, []device.Device, string) bool
}

func defaultHostIMSPhysicalGuard() hostIMSPhysicalGuard {
	return hostIMSPhysicalGuard{resolve: hostIMSInventoryPhysicalID, disjoint: hostIMSPhysicalDisjoint}
}

// This is only negative ownership evidence for legacy maintenance on another
// modem. It never establishes a Host SIM/bearer, nor relaxes factory admission.
func hostIMSPhysicalDisjointWith(config store.Device, entries []device.Device, targetID string, canonical func(string) (string, error)) bool {
	if canonical == nil || config.USBPath == "" {
		return false
	}
	valid := func(value string) bool {
		return strings.HasPrefix(value, "/sys/devices/") && path.Clean(value) == value
	}
	anchor, err := canonical(config.USBPath)
	if err != nil || !valid(anchor) {
		return false
	}
	var owner, target *device.Device
	for i := range entries {
		entry := &entries[i]
		if !entry.Discovered {
			continue
		}
		if entry.ID == targetID {
			if target != nil {
				return false
			}
			target = entry
		}
		physical, err := canonical(entry.Candidate.USBPath)
		if err != nil || !valid(physical) {
			if endpointInCandidate(entry.Candidate, config.ControlDevice) {
				return false
			}
			continue
		}
		if physical == anchor {
			if owner != nil {
				return false
			}
			owner = entry
		}
		if config.DeviceBackend == "qmi" && config.ControlDevice != "" && config.ControlDevice == entry.Candidate.QMIControl && physical != anchor {
			return false
		}
	}
	if owner == nil || target == nil {
		return false
	}
	other, err := canonical(target.Candidate.USBPath)
	if err != nil || !valid(other) || anchor == other || strings.HasPrefix(anchor, other+"/") || strings.HasPrefix(other, anchor+"/") {
		return false
	}
	if config.DeviceBackend == "qmi" && config.ControlDevice != "" && owner.Candidate.QMIControl != config.ControlDevice {
		return false
	}
	if endpointInCandidate(target.Candidate, config.ControlDevice) || endpointInCandidate(target.Candidate, config.ATPort) {
		return false
	}
	ownerEndpoints := []string{owner.Candidate.QMIControl, owner.Candidate.ATPort.Path, owner.Candidate.ATPort.OpenPath(), owner.Candidate.ReaderName}
	for _, port := range owner.Candidate.Ports {
		ownerEndpoints = append(ownerEndpoints, port.Path, port.OpenPath())
	}
	for _, endpoint := range ownerEndpoints {
		if endpointInCandidate(target.Candidate, endpoint) {
			return false
		}
	}
	if config.ModemIMEI != "" {
		if owner.Snapshot != nil && owner.Snapshot.IMEI != "" && owner.Snapshot.IMEI != config.ModemIMEI {
			return false
		}
		if target.Snapshot != nil && target.Snapshot.IMEI == config.ModemIMEI {
			return false
		}
	}
	if owner.Snapshot != nil && target.Snapshot != nil && owner.Snapshot.IMEI != "" && owner.Snapshot.IMEI == target.Snapshot.IMEI {
		return false
	}
	return true
}

// Cached names alone cannot exclude a hotplugged/reused AT or QMI device.
// Production verifies both actual legacy endpoints using sysfs char metadata.
// This remains a bounded negative pre-I/O check, not a new SIM/bearer proof.
func hostIMSPhysicalDisjointVerifiedWith(config store.Device, entries []device.Device, targetID string,
	canonical func(string) (string, error), endpointUnder func(string, string) bool, generation func(modem.Candidate, string) bool) bool {
	if endpointUnder == nil || generation == nil || !hostIMSPhysicalDisjointWith(config, entries, targetID, canonical) {
		return false
	}
	for _, entry := range entries {
		if !entry.Discovered || entry.ID != targetID {
			continue
		}
		physical, err := canonical(entry.Candidate.USBPath)
		if err != nil || !generation(entry.Candidate, physical) {
			return false
		}
		count := 0
		for _, endpoint := range []string{entry.Candidate.QMIControl, entry.Candidate.ATPort.OpenPath()} {
			if endpoint == "" {
				continue
			}
			count++
			if !endpointUnder(endpoint, physical) {
				return false
			}
		}
		return count > 0 && generation(entry.Candidate, physical)
	}
	return false
}

func endpointInCandidate(candidate modem.Candidate, endpoint string) bool {
	if endpoint == "" {
		return false
	}
	if endpoint == candidate.QMIControl || endpoint == candidate.ATPort.Path || endpoint == candidate.ATPort.OpenPath() || endpoint == candidate.ReaderName {
		return true
	}
	for _, port := range candidate.Ports {
		if endpoint == port.Path || endpoint == port.OpenPath() {
			return true
		}
	}
	return false
}
