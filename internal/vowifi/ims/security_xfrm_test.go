package ims

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestXFRMServerRepliesCoverTCPAndUDPWithoutOpeningOtherPeers(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		t.Run(fmt.Sprint(ipv6), func(t *testing.T) {
			config := testIPSecSAConfig()
			prefix := "/32"
			if ipv6 {
				config.LocalIP = net.ParseIP("2001:db8:10::2")
				config.RemoteIP = net.ParseIP("2001:db8:10::3")
				prefix = "/128"
			}
			install, err := buildXFRMInstallPlan(config)
			if err != nil {
				t.Fatal(err)
			}
			found := make(map[string]bool)
			for _, operation := range install {
				if !strings.HasPrefix(operation.description, "UE-server to P-CSCF-client ") {
					continue
				}
				args := operation.arguments
				protocol := argumentAfter(t, args, "proto")
				if protocol != "6" && protocol != "17" || found[protocol] {
					t.Fatalf("unexpected server-reply protocol: %q", protocol)
				}
				found[protocol] = true
				if argumentAfter(t, args, "src") != config.LocalIP.String()+prefix ||
					argumentAfter(t, args, "dst") != config.RemoteIP.String()+prefix ||
					argumentAfter(t, args, "sport") != fmt.Sprint(config.UEServerPort) ||
					argumentAfter(t, args, "dir") != "out" {
					t.Fatalf("reply selector is not limited to the UE server and proven P-CSCF: %v", args)
				}
				// The inbound filters accept a proven P-CSCF's ephemeral port.
				// An absent destination-port selector covers that port as well as
				// negotiated port-c, without broadening either address or sport.
				if _, fixedPort := optionalArgumentAfter(args, "dport"); fixedPort {
					t.Fatalf("reply selector excludes ephemeral P-CSCF ports: %v", args)
				}
				if argumentAfter(t, args, "spi") != fmt.Sprintf("0x%08x", config.PCSCFClientSPI) ||
					argumentAfter(t, args, "reqid") != fmt.Sprint(serverPairReqID(config)) ||
					argumentAfter(t, args, "level") != "required" {
					t.Fatalf("reply policy does not require the server-pair ESP SA: %v", args)
				}
			}
			if len(found) != 2 {
				t.Fatalf("server-reply policies = %v, want TCP and UDP", found)
			}
			deleted := make(map[string]bool)
			for _, operation := range buildXFRMCleanupPlan(config) {
				if !strings.HasPrefix(operation.description, "delete UE-server to P-CSCF-client ") {
					continue
				}
				args := operation.arguments
				protocol := argumentAfter(t, args, "proto")
				if !found[protocol] || deleted[protocol] ||
					argumentAfter(t, args, "src") != config.LocalIP.String()+prefix ||
					argumentAfter(t, args, "dst") != config.RemoteIP.String()+prefix ||
					argumentAfter(t, args, "sport") != fmt.Sprint(config.UEServerPort) ||
					argumentAfter(t, args, "dir") != "out" {
					t.Fatalf("cleanup selector differs from installed reply policy: %v", args)
				}
				if _, fixedPort := optionalArgumentAfter(args, "dport"); fixedPort {
					t.Fatalf("cleanup cannot remove the wildcard reply policy: %v", args)
				}
				deleted[protocol] = true
			}
			if len(deleted) != 2 {
				t.Fatalf("server-reply cleanup policies = %v, want TCP and UDP", deleted)
			}
		})
	}
}
