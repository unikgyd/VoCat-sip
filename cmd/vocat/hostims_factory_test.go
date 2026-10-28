package main

import (
	"errors"
	"testing"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func hostIMSMappingFixture() (store.Device, device.Device, func(string) (string, error)) {
	const usb = "/sys/devices/pci0000:00/usb1/1-1"
	config := store.Device{ID: "logical-line", DeviceType: store.DeviceTypePCIeEC20EC25, ModemIMEI: "123456789012340", USBPath: "/sys/bus/usb/devices/1-1", DeviceBackend: "qmi", ControlDevice: "/dev/cdc-wdm0"}
	entry := device.Device{ID: "discovered-new-id", Discovered: true, Candidate: modem.Candidate{USBPath: usb, QMIControl: "/dev/cdc-wdm0"}, Snapshot: &device.Snapshot{SIMReady: true, IMEI: config.ModemIMEI, ICCID: "8901000000000000000", IMSI: "310260123456780"}}
	canonical := func(value string) (string, error) {
		if value == usb || value == config.USBPath {
			return usb, nil
		}
		if value == "/sys/devices/pci0000:00/usb1/1-2" {
			return value, nil
		}
		return "", errors.New("synthetic missing sysfs identity")
	}
	return config, entry, canonical
}

func TestHostIMSMetadataProjectionKeepsInventoryAndHardwareAnchors(t *testing.T) {
	config, entry, canonical := hostIMSMappingFixture()
	entry.Snapshot.SIMReady = false
	entries := []device.Device{entry}
	projected := hostIMSMetadataInventory(config, entries)
	if projected[0].Snapshot != nil || entries[0].Snapshot != entry.Snapshot || entry.Snapshot.SIMReady {
		t.Fatal("metadata projection mutated discovery inventory")
	}
	if _, _, err := resolveHostIMSInventory(config, projected, canonical); err != nil {
		t.Fatal(err)
	}
	projected[0].Candidate.QMIControl = "/dev/cdc-wdm1"
	if _, _, err := resolveHostIMSInventory(config, projected, canonical); err == nil {
		t.Fatal("metadata projection bypassed endpoint anchor")
	}
	config.ModemIMEI = ""
	if hostIMSMetadataInventory(config, entries)[0].Snapshot == nil {
		t.Fatal("incomplete hardware anchors discarded SIM proof")
	}
}

func TestHostIMSMappingUsesUniquePhysicalIdentityNotLogicalID(t *testing.T) {
	config, wanted, canonical := hostIMSMappingFixture()
	other := wanted
	other.ID = config.ID
	snapshot := *wanted.Snapshot
	snapshot.IMEI = "123456789012341"
	other.Snapshot = &snapshot
	other.Candidate.USBPath = "/sys/devices/pci0000:00/usb1/1-2"
	other.Candidate.QMIControl = "/dev/cdc-wdm1"
	selected, physical, err := resolveHostIMSInventory(config, []device.Device{other, wanted}, canonical)
	if err != nil || selected.ID != wanted.ID || physical != wanted.Candidate.USBPath {
		t.Fatal("logical ID overrode physical identity", err)
	}
}

func TestHostIMSMappingRejectsMissingAmbiguousAndStaleAnchors(t *testing.T) {
	for _, mode := range []string{"absent", "duplicate", "undiscovered", "no-sim", "stale-imei", "stale-usb", "stale-qmi", "missing-qmi", "placeholder", "blocked-region", "bad-imsi", "no-anchor", "reader", "bad-physical"} {
		t.Run(mode, func(t *testing.T) {
			config, entry, canonical := hostIMSMappingFixture()
			entries := []device.Device{entry}
			switch mode {
			case "absent":
				entries = nil
			case "duplicate":
				duplicate := entry
				duplicate.ID = "duplicate"
				entries = append(entries, duplicate)
			case "undiscovered":
				entries[0].Discovered = false
			case "no-sim":
				entry.Snapshot.SIMReady = false
			case "stale-imei":
				config.ModemIMEI = "123456789012349"
			case "stale-usb":
				config.USBPath = "/sys/devices/pci0000:00/usb1/1-2"
			case "stale-qmi":
				config.ControlDevice = "/dev/cdc-wdm1"
			case "missing-qmi":
				entries[0].Candidate.QMIControl = ""
			case "placeholder":
				entry.Snapshot.IMSI = "310000000000000"
			case "blocked-region":
				device.BlockedMCCs["999"] = "测试"
				t.Cleanup(func() { delete(device.BlockedMCCs, "999") })
				entry.Snapshot.IMSI = "999001234567890"
			case "bad-imsi":
				entry.Snapshot.IMSI = "unknown"
			case "no-anchor":
				config.ModemIMEI, config.USBPath = "", ""
				entries[0].ID = config.ID
			case "reader":
				config.DeviceType = store.DeviceTypeUSBSIMReader
			case "bad-physical":
				canonical = func(string) (string, error) { return "/sys/devices/../class", nil }
			}
			if _, _, err := resolveHostIMSInventory(config, entries, canonical); !errors.Is(err, errHostIMSFactory) {
				t.Fatal("unsafe target accepted", err)
			}
		})
	}
}

func TestHostIMSMappingAllowsVerifiedSysfsAnchorBeforeIMEIIsPersisted(t *testing.T) {
	config, entry, canonical := hostIMSMappingFixture()
	config.ModemIMEI = ""
	if selected, _, err := resolveHostIMSInventory(config, []device.Device{entry}, canonical); err != nil || selected.Snapshot.IMEI == "" {
		t.Fatal("trusted physical anchor was not usable", err)
	}
	config.USBPath = ""
	if _, _, err := resolveHostIMSInventory(config, []device.Device{entry}, canonical); err == nil {
		t.Fatal("missing persisted anchor accepted")
	}
}

func TestHostIMSMappingDoesNotConfuseSiblingPhysicalPaths(t *testing.T) {
	config, entry, _ := hostIMSMappingFixture()
	canonical := func(value string) (string, error) {
		if value == config.USBPath {
			return "/sys/devices/usb1/1-1", nil
		}
		return "/sys/devices/usb1/1-10", nil
	}
	if _, _, err := resolveHostIMSInventory(config, []device.Device{entry}, canonical); err == nil {
		t.Fatal("prefix sibling accepted as same USB device")
	}
}

func TestHostIMSMappingColdStartNeedsSavedHardwareAndDoesNotInventSIM(t *testing.T) {
	config, entry, canonical := hostIMSMappingFixture()
	entry.Snapshot = nil
	selected, _, err := resolveHostIMSInventory(config, []device.Device{entry}, canonical)
	if err != nil || selected.Snapshot != nil {
		t.Fatal("cold hardware mapping failed or invented a SIM snapshot", err)
	}
	config.ModemIMEI = ""
	if _, _, err := resolveHostIMSInventory(config, []device.Device{entry}, canonical); err == nil {
		t.Fatal("cold target without saved IMEI accepted")
	}
	config, entry, canonical = hostIMSMappingFixture()
	entry.Snapshot = nil
	config.USBPath = ""
	if _, _, err := resolveHostIMSInventory(config, []device.Device{entry}, canonical); err == nil {
		t.Fatal("cold target without saved physical anchor accepted")
	}
}

func TestHostIMSMappingGuardIgnoresStaleLegacyATPort(t *testing.T) {
	config, wanted, canonical := hostIMSMappingFixture()
	config.DeviceBackend = "at"
	config.ATPort, config.ControlDevice = "/dev/ttyUSB9", "/dev/ttyUSB9"
	wanted.Snapshot = nil
	other := wanted
	other.ID = "stale-port-device"
	other.Candidate.USBPath = "/sys/devices/pci0000:00/usb1/1-2"
	other.Candidate.QMIControl = "/dev/cdc-wdm1"
	other.Candidate.ATPort.Path = config.ATPort
	selected, _, err := resolveHostIMSInventory(config, []device.Device{other, wanted}, canonical)
	if err != nil || selected.ID != wanted.ID {
		t.Fatal("stale AT port overrode strong cold-start USB mapping", err)
	}
	if _, _, err := resolveHostIMSInventory(config, []device.Device{wanted, wanted}, canonical); err == nil {
		t.Fatal("ambiguous guard mapping accepted")
	}
}

func TestHostIMSMappingPrepareConfigRequiresExplicitEnabledMode(t *testing.T) {
	config, _, _ := hostIMSMappingFixture()
	config.VoWiFiEnabled = true
	if hostIMSModeEnabled(config) {
		t.Fatal("legacy WiFi preference authorized Host IMS")
	}
	config.IMSMode, config.IMSEnabled = store.IMSModeHost, true
	if !sameHostIMSPrepareConfig(config, config) {
		t.Fatal("unchanged explicit configuration rejected")
	}
	for _, field := range []string{"mode", "enabled", "imei", "physical", "qmi", "backend"} {
		next := config
		switch field {
		case "mode":
			next.IMSMode = store.IMSModeVoWiFi
		case "enabled":
			next.IMSEnabled = false
		case "imei":
			next.ModemIMEI = "123456789012349"
		case "physical":
			next.USBPath = "/sys/devices/other"
		case "qmi":
			next.ControlDevice = "/dev/cdc-wdm1"
		case "backend":
			next.DeviceBackend = "at"
		}
		if sameHostIMSPrepareConfig(config, next) {
			t.Fatal("stale prepare config accepted", field)
		}
	}
}
