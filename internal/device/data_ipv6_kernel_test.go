//go:build linux

package device

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"vocat/internal/modem"
)

func requireDataKernelNamespace(t *testing.T) string {
	t.Helper()
	if os.Getenv("VOCAT_DATA_KERNEL_TESTS") != "1" {
		t.Skip("opt-in private network/mount namespaces required")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil || self == init {
			t.Fatal("kernel data test refuses the original host namespace", kind, err)
		}
	}
	ip, err := exec.LookPath("ip")
	if err != nil {
		t.Fatal(err)
	}
	return ip
}

func dataKernelCommand(t *testing.T, ip string, args ...string) string {
	t.Helper()
	out, err := exec.Command(ip, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("private ip %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func dataKernelFixture(t *testing.T, family uint8) (string, *managedDevice, modem.Candidate) {
	t.Helper()
	ip := requireDataKernelNamespace(t)
	name := fmt.Sprintf("vcd%d", family)
	dataKernelCommand(t, ip, "link", "add", name, "type", "dummy")
	t.Cleanup(func() { _ = exec.Command(ip, "link", "del", name).Run() })
	if family == 6 {
		dataKernelCommand(t, ip, "link", "set", "dev", name, "up")
	}
	candidate := modem.Candidate{ID: name, NetworkInterface: name}
	session, _, _ := dualDataFixture()
	var source qmiDataSession = session
	if family == 6 {
		source = session.parts[1]
	}
	owner := newOwnedQMINetwork(&ownedNetwork{candidate: candidate, backend: "qmi", ipFamily: family, hostTouched: true})
	return ip, &managedDevice{candidate: candidate, discovered: true, dataOwner: owner, dataSession: source}, candidate
}

func TestQMIIPv6DataKernelRoutesDNSAndOriginalSettings(t *testing.T) {
	requireDataKernelNamespace(t)
	for _, family := range []uint8{6, qmiDataDualFamily} {
		t.Run(fmt.Sprint(family), func(t *testing.T) {
			ip, state, candidate := dataKernelFixture(t, family)
			version := "IPV6"
			if family == qmiDataDualFamily {
				version = "IPV4V6"
			}
			before := map[string]string{}
			for _, setting := range []string{"accept_ra", "autoconf", "disable_ipv6"} {
				path, _ := dataIPv6SettingPath(candidate.NetworkInterface, setting)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[setting] = strings.TrimSpace(string(raw))
			}
			originalMTU, originalLinkUp := state.dataOwner.hostMTU, state.dataOwner.hostLinkUp
			default4 := dataKernelCommand(t, ip, "-4", "route", "show", "table", "main", "default")
			default6 := dataKernelCommand(t, ip, "-6", "route", "show", "table", "main", "default")
			if result, err := configureQMIDataHost(context.Background(), state, candidate, ip, "internet", version, "private kernel fixture"); err != nil || !result.Enabled {
				t.Fatal("requested IP families were not configured", result, err)
			}
			if err := qmiDataHostReadyForFamily(context.Background(), candidate.NetworkInterface, family); err != nil {
				t.Fatal(err)
			}
			dns, err := os.ReadFile(exportProxyDNSPath(candidate.NetworkInterface))
			if err != nil || !strings.Contains(string(dns), "2001:db8:1234::53") || family == qmiDataDualFamily && !strings.Contains(string(dns), "192.0.2.53") {
				t.Fatal("requested DNS families missing", err)
			}
			if err := clearOwnedQMIHost(context.Background(), state.dataOwner, true); err != nil {
				t.Fatal("original host cleanup failed", err)
			}
			info, _ := net.InterfaceByName(candidate.NetworkInterface)
			if info.MTU != originalMTU || (info.Flags&net.FlagUp != 0) != originalLinkUp {
				t.Fatal("original link state or MTU was not restored")
			}
			addresses, _ := info.Addrs()
			for _, a := range addresses {
				ip, _, _ := net.ParseCIDR(a.String())
				if ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() {
					t.Fatal("owned data address remained", a)
				}
			}
			if _, err := os.Stat(exportProxyDNSPath(candidate.NetworkInterface)); !os.IsNotExist(err) {
				t.Fatal("private DNS file remained", err)
			}
			for setting, original := range before {
				path, _ := dataIPv6SettingPath(candidate.NetworkInterface, setting)
				raw, err := os.ReadFile(path)
				if err != nil || strings.TrimSpace(string(raw)) != original {
					t.Fatal("original IPv6 setting was not restored", setting, err)
				}
			}
			if default4 != dataKernelCommand(t, ip, "-4", "route", "show", "table", "main", "default") || default6 != dataKernelCommand(t, ip, "-6", "route", "show", "table", "main", "default") {
				t.Fatal("the main default route changed")
			}
		})
	}
}

func TestQMIIPv6DataKernelPreservesOtherInterfaceRoutes(t *testing.T) {
	ip, state, candidate := dataKernelFixture(t, qmiDataDualFamily)
	foreign := "vcforeign"
	dataKernelCommand(t, ip, "link", "add", foreign, "type", "dummy")
	t.Cleanup(func() { _ = exec.Command(ip, "link", "del", foreign).Run() })
	dataKernelCommand(t, ip, "link", "set", "dev", foreign, "up")
	_, table, _ := exportProxyRouteIdentity(candidate.NetworkInterface)
	for flag, destination := range map[string]string{"-4": "198.51.100.0/24", "-6": "2001:db8:feed::/64"} {
		dataKernelCommand(t, ip, flag, "route", "add", "table", fmt.Sprint(table), destination, "dev", foreign)
	}
	if _, err := configureQMIDataHost(context.Background(), state, candidate, ip, "internet", "IPV4V6", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := clearOwnedQMIHost(context.Background(), state.dataOwner, true); err != nil {
		t.Fatal(err)
	}
	for flag, destination := range map[string]string{"-4": "198.51.100.0/24", "-6": "2001:db8:feed::/64"} {
		if !strings.Contains(dataKernelCommand(t, ip, flag, "route", "show", "table", fmt.Sprint(table), "dev", foreign), destination) {
			t.Fatal("another interface's route was removed", flag)
		}
	}
}

func TestQMIIPv6DataKernelRefusesForeignAddressesAndReplacement(t *testing.T) {
	ip, state, candidate := dataKernelFixture(t, 6)
	dataKernelCommand(t, ip, "-6", "addr", "add", "2001:db8:ffff::2/64", "dev", candidate.NetworkInterface, "nodad")
	if err := validateIPv6DataHostClaim(candidate.NetworkInterface, nil); err == nil {
		t.Fatal("foreign address was accepted as a fresh host claim")
	}
	state.dataOwner.ipv6Address = "2001:db8:1234::2/64"
	if err := clearOwnedQMIHost(context.Background(), state.dataOwner, true); err == nil {
		t.Fatal("foreign address did not fence current-netdev cleanup")
	}
	if err := clearOwnedQMIHost(context.Background(), state.dataOwner, false); err != nil {
		t.Fatal("confirmed physical disappearance tried to mutate the replacement", err)
	}
	if !strings.Contains(dataKernelCommand(t, ip, "-6", "addr", "show", "dev", candidate.NetworkInterface), "2001:db8:ffff::2") {
		t.Fatal("replacement address was removed")
	}
}
