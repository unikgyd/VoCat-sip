package main

import (
	"context"
	"errors"
	"path"
	"strings"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
	"vocat/internal/vowifi/ims"
)

var errHostIMSFactory = errors.New("host IMS: application factory unavailable or target identity ambiguous")

type hostIMSDeviceInventory interface{ List() []device.Device }

// Saved equipment plus physical topology lets MM supply the live SIM. Do not
// let an intentionally unrefreshed AT SIM snapshot veto that metadata lookup,
// or mutate a caller's inventory slice while projecting hardware-only rows.
func hostIMSMetadataInventory(config store.Device, entries []device.Device) []device.Device {
	result := append([]device.Device(nil), entries...)
	if hostIMSDigits(config.ModemIMEI, 15, 15) && strings.Trim(config.ModemIMEI, "0") != "" && config.USBPath != "" {
		for i := range result {
			result[i].Snapshot = nil
		}
	}
	return result
}

type hostIMSFactoryOptions struct {
	Store     *store.Store
	Devices   hostIMSDeviceInventory
	IMSConfig func(context.Context, store.Device) (ims.Config, error)
	// Mandatory deployment/product authorization, independently rechecked by
	// the bearer owner. HTTP admin login is not a licence/evaluation decision.
	Authorize     hostims.AuthorizeBearer
	BearerOptions func(context.Context, store.Device) (hostims.BearerOptions, error)
}

type hostIMSRuntimeFactory interface {
	Factory(context.Context, string) (*hostimsruntime.Runtime, error)
	Close(context.Context) error
}

func hostIMSModeEnabled(config store.Device) bool {
	return store.AccessEnabled(config, store.IMSModeHost)
}

func sameHostIMSPrepareConfig(before, after store.Device) bool {
	return hostIMSModeEnabled(before) && hostIMSModeEnabled(after) && store.SameSubscription(before, after) && before.ID == after.ID &&
		before.DeviceType == after.DeviceType && before.ModemIMEI == after.ModemIMEI && before.USBPath == after.USBPath &&
		before.ControlDevice == after.ControlDevice && before.DeviceBackend == after.DeviceBackend
}

func hostIMSDigits(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func hostIMSControlPath(value string) bool {
	return strings.HasPrefix(value, "/dev/") && path.Dir(value) == "/dev" && path.Clean(value) == value && len(path.Base(value)) > 0
}

// No ATMapper score or logical-ID fallback: every saved strong anchor must
// agree, and exactly one live discovery row must satisfy the full identity.
// canonical is a metadata-only sysfs resolver in production, injected in tests.
func resolveHostIMSInventory(config store.Device, entries []device.Device, canonical func(string) (string, error)) (device.Device, string, error) {
	if config.ID == "" || canonical == nil || config.DeviceType == store.DeviceTypeUSBSIMReader ||
		config.ModemIMEI == "" && config.USBPath == "" || config.ModemIMEI != "" && !hostIMSDigits(config.ModemIMEI, 15, 15) {
		return device.Device{}, "", errHostIMSFactory
	}
	physicalConfig := ""
	if config.USBPath != "" {
		var err error
		physicalConfig, err = canonical(config.USBPath)
		if err != nil || !strings.HasPrefix(physicalConfig, "/sys/devices/") || path.Clean(physicalConfig) != physicalConfig {
			return device.Device{}, "", errHostIMSFactory
		}
	}
	var selected device.Device
	physicalSelected := ""
	for _, entry := range entries {
		s := entry.Snapshot
		if !entry.Discovered || !hostIMSControlPath(entry.Candidate.QMIControl) {
			continue
		}
		if s == nil {
			// A cold-start sysfs inventory has no SIM snapshot. Only both saved
			// immutable equipment and physical anchors can reach MM selection;
			// this is not a fabricated SIM/Established observation.
			if !hostIMSDigits(config.ModemIMEI, 15, 15) || strings.Trim(config.ModemIMEI, "0") == "" || physicalConfig == "" {
				continue
			}
		} else {
			if !s.SIMReady || !hostIMSDigits(s.IMEI, 15, 15) || strings.Trim(s.IMEI, "0") == "" ||
				!hostIMSDigits(s.ICCID, 18, 20) || !hostIMSDigits(s.IMSI, 14, 16) || device.IsPlaceholderIMSI(s.IMSI) || device.RegionBlockReason(s.IMSI) != "" {
				continue
			}
			if config.ModemIMEI != "" && config.ModemIMEI != s.IMEI {
				continue
			}
		}
		if config.DeviceBackend == "qmi" && config.ControlDevice != "" && config.ControlDevice != entry.Candidate.QMIControl {
			continue
		}
		physical, err := canonical(entry.Candidate.USBPath)
		if err != nil || !strings.HasPrefix(physical, "/sys/devices/") || path.Clean(physical) != physical {
			continue
		}
		if physicalConfig != "" && physicalConfig != physical {
			continue
		}
		if selected.ID != "" {
			return device.Device{}, "", errHostIMSFactory
		}
		selected, physicalSelected = entry, physical
	}
	if selected.ID == "" {
		return device.Device{}, "", errHostIMSFactory
	}
	return selected, physicalSelected, nil
}
