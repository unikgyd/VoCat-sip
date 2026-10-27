//go:build linux

package main

import (
	"strings"
	"testing"

	"vocat/internal/modem"
	"vocat/internal/store"
)

func TestDataOwnerRecoveryPortConfiguration(t *testing.T) {
	for _, raw := range []string{"", "  ", "{}", `{"physical-a":"/dev/serial/by-id/secondary","physical-b":"/dev/ttyUSB7"}`} {
		ports, err := parseDataOwnerRecoveryPorts(raw)
		if err != nil {
			t.Fatalf("valid explicit mapping: %v", err)
		}
		if strings.Contains(raw, "physical-a") && ports["physical-a"] != "/dev/serial/by-id/secondary" {
			t.Fatal("configured port changed")
		}
		if ports["unconfigured-device"] != "" {
			t.Fatal("an unconfigured modem acquired recovery permission")
		}
	}
}

func TestDataRecoveryRequiresOneCurrentDataOnlyOriginalSIMPolicy(t *testing.T) {
	candidate := modem.Candidate{ID: "physical", USBPath: "/synthetic/usb/1-2"}
	for _, scenario := range []string{"data only", "another modem WiFi", "no configuration", "disabled data", "changed card", "Host IMS", "WiFi intent", "legacy WiFi flag", "legacy card", "ambiguous alias", "unbound protected modem"} {
		t.Run(scenario, func(t *testing.T) {
			policy := &store.TelephonyPolicy{ICCID: "8901000000000000001", CellularMode: store.IMSModeModule, DefaultRoute: store.IMSModeModule, WiFiRadioPolicy: "coexist"}
			config := store.Device{ID: "line", USBPath: candidate.USBPath, NetworkEnabled: true, Telephony: policy}
			switch scenario {
			case "disabled data":
				config.NetworkEnabled = false
			case "changed card":
				policy.ICCID = "8901000000000000002"
			case "Host IMS":
				policy.CellularMode = store.IMSModeHost
			case "WiFi intent":
				policy.AutoConnectWiFi = true
			case "legacy WiFi flag":
				config.VoWiFiEnabled = true
			case "legacy card":
				config.Telephony = nil
			}
			configs := []store.Device{config}
			switch scenario {
			case "no configuration":
				configs = nil
			case "ambiguous alias":
				alias := config
				alias.ID = "alias"
				configs = append(configs, alias)
			case "unbound protected modem":
				configs = append(configs, store.Device{ID: "unbound", IMSMode: store.IMSModeVoWiFi, VoWiFiEnabled: true})
			case "another modem WiFi":
				configs = append(configs, store.Device{ID: "other", USBPath: "/synthetic/usb/1-4", IMSMode: store.IMSModeVoWiFi, VoWiFiEnabled: true})
			}
			want := scenario == "data only" || scenario == "another modem WiFi"
			if got := ownedDataRecoveryPolicy(configs, candidate, "8901000000000000001"); got != want {
				t.Fatalf("recovery allowed %v, want %v", got, want)
			}
		})
	}
}

func TestDataOwnerRecoveryRejectsAmbiguousOrInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{"":"/dev/ttyUSB3"}`, `{"a":null}`, `{"a":4}`,
		`{"a":"relative"}`, `{"a":"/etc/passwd"}`, `{"a":"/dev/../etc/passwd"}`,
		`{"a":"/dev/ttyUSB3","a":"/dev/ttyUSB4"}`,
		`{"a":"/dev/ttyUSB3","b":"/dev/ttyUSB3"}`, `{"a":"/dev/ttyUSB3"} {}`,
		`{"a\n":"/dev/ttyUSB3"}`, `{"a":"/dev/ttyUSB3\n"}`, strings.Repeat(" ", 32769),
	} {
		if ports, err := parseDataOwnerRecoveryPorts(raw); err == nil || ports != nil {
			t.Fatalf("invalid recovery mapping accepted: %q", raw)
		}
	}
}
