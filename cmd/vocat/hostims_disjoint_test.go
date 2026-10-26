package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func hostIMSDisjointFixture() ([]store.Device, []device.Device, func(string) (string, error), hostIMSPhysicalGuard) {
	const a = "/sys/devices/pci0000:00/usb1/1-1"
	const b = "/sys/devices/pci0000:00/usb1/1-2"
	configs := []store.Device{
		{ID: "host-a", Name: "Host A", DeviceType: store.DeviceTypePCIeEC20EC25, IMSMode: store.IMSModeHost, IMSEnabled: true, USBPath: "/sys/bus/usb/devices/1-1", ModemIMEI: "123456789012340", DeviceBackend: "qmi", ControlDevice: "/dev/cdc-wdm0"},
		{ID: "module-b", Name: "Module B", DeviceType: store.DeviceTypePCIeEC20EC25, IMSMode: store.IMSModeModule, USBPath: "/sys/bus/usb/devices/1-2", ModemIMEI: "123456789012341", DeviceBackend: "qmi", ControlDevice: "/dev/cdc-wdm1"},
	}
	entries := []device.Device{
		{ID: "physical-a", Discovered: true, Candidate: modem.Candidate{USBPath: a, QMIControl: "/dev/cdc-wdm0"}, Snapshot: &device.Snapshot{SIMReady: false, IMEI: configs[0].ModemIMEI}},
		{ID: "physical-b", Discovered: true, Candidate: modem.Candidate{USBPath: b, QMIControl: "/dev/cdc-wdm1"}, Snapshot: &device.Snapshot{SIMReady: true, IMEI: configs[1].ModemIMEI, ICCID: "8901000000000000001", IMSI: "001010000000001"}},
	}
	canonical := func(value string) (string, error) {
		switch value {
		case "/sys/bus/usb/devices/1-1":
			return a, nil
		case "/sys/bus/usb/devices/1-2":
			return b, nil
		}
		if strings.HasPrefix(value, "/sys/devices/") {
			return value, nil
		}
		return "", errors.New("synthetic missing sysfs anchor")
	}
	guard := hostIMSPhysicalGuard{
		resolve: func(config store.Device, entries []device.Device) (string, error) {
			entry, _, err := resolveHostIMSInventory(config, entries, canonical)
			return entry.ID, err
		},
		disjoint: func(config store.Device, entries []device.Device, target string) bool {
			return hostIMSPhysicalDisjointWith(config, entries, target, canonical)
		},
	}
	return configs, entries, canonical, guard
}

func TestIMSHostUnreadySIMDoesNotBlockDisjointLegacyPhysical(t *testing.T) {
	configs, entries, canonical, guard := hostIMSDisjointFixture()
	if _, _, err := resolveHostIMSInventory(configs[0], entries, canonical); err == nil {
		t.Fatal("failed SIM was accepted by the real prepare gate")
	}
	if !hostIMSPhysicalDisjointWith(configs[0], entries, "physical-b", canonical) {
		t.Fatal("known disjoint modem blocked by failed SIM")
	}
	ids, ok := legacyPhysicalGateIDsWith(configs, entries, "physical-b", guard)
	if !ok || len(ids) != 1 || ids[0] != "module-b" {
		t.Fatal("other line could not enter its own read gate", ids, ok)
	}
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, config := range configs {
		if err := db.UpsertDevice(context.Background(), config); err != nil {
			t.Fatal(err)
		}
	}
	if hostIMSOwnsPhysicalWith(context.Background(), db, entries, "physical-b", guard) {
		t.Fatal("other physical target falsely claimed by Host")
	}
	if !hostIMSOwnsPhysicalWith(context.Background(), db, entries, "physical-a", guard) {
		t.Fatal("failed SIM released its own physical guard")
	}
	if _, ok := legacyPhysicalGateIDsWith(configs, entries, "physical-a", guard); ok {
		t.Fatal("failed Host's own radio became probeable")
	}
}

func TestIMSHostDisjointGuardRejectsUnknownTopologyAndAliases(t *testing.T) {
	for _, name := range []string{"missing-saved-usb", "unknown-saved-usb", "unclean-usb", "missing-owner", "missing-target", "duplicate-owner", "duplicate-target", "same-usb", "owner-ancestor", "target-ancestor", "saved-imei-is-target", "saved-imei-contradicts-owner", "snapshot-imei-alias", "qmi-alias", "saved-qmi-is-target", "legacy-at-alias", "non-sysfs-canonical", "canonical-error"} {
		t.Run(name, func(t *testing.T) {
			configs, entries, canonical, _ := hostIMSDisjointFixture()
			switch name {
			case "missing-saved-usb":
				configs[0].USBPath = ""
			case "unknown-saved-usb":
				configs[0].USBPath = "/synthetic/unknown"
			case "unclean-usb":
				configs[0].USBPath = "/sys/devices/../devices/usb-a"
			case "missing-owner":
				entries[0].Discovered = false
			case "missing-target":
				entries[1].Discovered = false
			case "duplicate-owner":
				duplicate := entries[0]
				duplicate.ID = "duplicate-a"
				entries = append(entries, duplicate)
			case "duplicate-target":
				entries = append(entries, entries[1])
			case "same-usb":
				entries[1].Candidate.USBPath = entries[0].Candidate.USBPath
			case "owner-ancestor":
				entries[1].Candidate.USBPath = entries[0].Candidate.USBPath + "/1-1.2"
			case "target-ancestor":
				entries[1].Candidate.USBPath = "/sys/devices/pci0000:00/usb1"
			case "saved-imei-is-target":
				configs[0].ModemIMEI = entries[1].Snapshot.IMEI
			case "saved-imei-contradicts-owner":
				configs[0].ModemIMEI = "123456789012349"
			case "snapshot-imei-alias":
				entries[0].Snapshot.IMEI = entries[1].Snapshot.IMEI
				configs[0].ModemIMEI = ""
			case "qmi-alias":
				entries[0].Candidate.QMIControl = entries[1].Candidate.QMIControl
				configs[0].ControlDevice = ""
			case "saved-qmi-is-target":
				configs[0].ControlDevice = entries[1].Candidate.QMIControl
			case "legacy-at-alias":
				configs[0].ATPort = "/dev/ttyUSB9"
				entries[1].Candidate.ATPort = modem.Port{Path: "/dev/ttyUSB9"}
			case "non-sysfs-canonical":
				canonical = func(string) (string, error) { return "/tmp/not-physical", nil }
			case "canonical-error":
				canonical = func(string) (string, error) { return "", errors.New("synthetic unavailable") }
			}
			if hostIMSPhysicalDisjointWith(configs[0], entries, "physical-b", canonical) {
				t.Fatal("unproved physical disjointness accepted")
			}
		})
	}
}

func TestIMSHostDisjointUSBPathComparisonUsesComponentBoundary(t *testing.T) {
	configs, entries, canonical, _ := hostIMSDisjointFixture()
	entries[1].Candidate.USBPath = "/sys/devices/pci0000:00/usb1/1-10"
	if !hostIMSPhysicalDisjointWith(configs[0], entries, "physical-b", canonical) {
		t.Fatal("1-1 and 1-10 incorrectly treated as ancestors")
	}
}
