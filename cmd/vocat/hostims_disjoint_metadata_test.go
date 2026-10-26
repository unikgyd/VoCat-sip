package main

import (
	"testing"

	"vocat/internal/modem"
)

func TestIMSHostDisjointRequiresCurrentTargetEndpointMetadata(t *testing.T) {
	for _, name := range []string{"current", "qmi-reused-by-host", "at-reused-by-host", "stable-at-reused-by-host", "missing-qmi-char", "missing-at-char", "no-endpoint", "changed-generation", "generation-changes-during-check", "missing-metadata-proof"} {
		t.Run(name, func(t *testing.T) {
			configs, entries, canonical, _ := hostIMSDisjointFixture()
			const a = "/sys/devices/pci0000:00/usb1/1-1"
			const b = "/sys/devices/pci0000:00/usb1/1-2"
			entries[1].Candidate.ATPort = modem.Port{Path: "/dev/ttyUSB9"}
			entries[1].Candidate.USBGeneration = "1:2"
			current := map[string]string{"/dev/cdc-wdm1": b, "/dev/ttyUSB9": b}
			epoch := "1:2"
			switch name {
			case "qmi-reused-by-host":
				current["/dev/cdc-wdm1"] = a
			case "at-reused-by-host":
				current["/dev/ttyUSB9"] = a
			case "stable-at-reused-by-host":
				entries[1].Candidate.ATPort.StablePath = "/dev/serial/by-id/stale-b"
				current["/dev/serial/by-id/stale-b"] = a
			case "missing-qmi-char":
				delete(current, "/dev/cdc-wdm1")
			case "missing-at-char":
				delete(current, "/dev/ttyUSB9")
			case "no-endpoint":
				entries[1].Candidate.QMIControl = ""
				entries[1].Candidate.ATPort = modem.Port{}
			case "changed-generation":
				epoch = "1:3"
			}
			checks := 0
			endpointUnder := func(endpoint, physical string) bool {
				checks++
				if name == "generation-changes-during-check" {
					epoch = "1:3"
				}
				return current[endpoint] == physical
			}
			if name == "missing-metadata-proof" {
				endpointUnder = nil
			}
			generation := func(candidate modem.Candidate, _ string) bool { return candidate.USBGeneration == epoch }
			got := hostIMSPhysicalDisjointVerifiedWith(configs[0], entries, "physical-b", canonical, endpointUnder, generation)
			if got != (name == "current") {
				t.Fatal("cached endpoint escaped current physical proof", got, checks)
			}
		})
	}
}
