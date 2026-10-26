//go:build linux

package device

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"

	"vocat/internal/modem"
)

func dataFamilyFlags(family uint8) []string {
	if family == 6 {
		return []string{"-6"}
	}
	if family == qmiDataDualFamily {
		return []string{"-4", "-6"}
	}
	return []string{"-4"}
}

func validateIPv6DataHostClaim(name string, owner *ownedNetwork) error {
	info, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	addresses, err := info.Addrs()
	if err != nil {
		return err
	}
	for _, value := range addresses {
		address, _, err := net.ParseCIDR(value.String())
		if err == nil && address.IsGlobalUnicast() && !address.IsLinkLocalUnicast() && (owner == nil || value.String() != owner.ipv6Address && value.String() != owner.ipv4Address) {
			return errors.New("cellular interface already has an unowned data address")
		}
	}
	return nil
}

func dataIPv6SettingPath(name, setting string) (string, error) {
	if name == "" || len(name) > 15 || filepath.Base(name) != name || name == "." || name == ".." {
		return "", errors.New("invalid cellular interface")
	}
	return filepath.Join("/proc/sys/net/ipv6/conf", name, setting), nil
}

func prepareDataIPv6Settings(owner *ownedNetwork) error {
	if owner.ipv6Settings == nil {
		owner.ipv6Settings = make(map[string]string)
	}
	for _, setting := range []string{"accept_ra", "autoconf", "disable_ipv6"} {
		path, err := dataIPv6SettingPath(owner.candidate.NetworkInterface, setting)
		if err != nil {
			return err
		}
		previous, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, saved := owner.ipv6Settings[setting]; !saved {
			owner.ipv6Settings[setting] = strings.TrimSpace(string(previous))
		}
		if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func restoreDataIPv6Settings(owner *ownedNetwork) error {
	for setting, original := range owner.ipv6Settings {
		path, err := dataIPv6SettingPath(owner.candidate.NetworkInterface, setting)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		value := strings.TrimSpace(string(current))
		if value != "0" && value != original {
			return errors.New("cellular IPv6 settings changed during ownership")
		}
		if err := os.WriteFile(path, []byte(original+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func clearOwnedDataIPv6Address(ctx context.Context, ip, name, prefix string) error {
	return clearOwnedDataAddress(ctx, ip, name, "-6", prefix)
}

func clearOwnedDataAddress(ctx context.Context, ip, name, family, prefix string) error {
	if prefix == "" {
		return nil
	}
	info, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	addresses, err := info.Addrs()
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if address.String() == prefix {
			if out, err := exec.CommandContext(ctx, ip, family, "addr", "del", prefix, "dev", name).CombinedOutput(); err != nil {
				return fmt.Errorf("remove original data address: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

func clearOwnedDataRoutes(ctx context.Context, owner *ownedNetwork, family, table int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table, LinkIndex: owner.interfaceID}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.ENOENT) {
			return err
		}
	}
	return nil
}

func configureQMIIPv6Host(ctx context.Context, state *managedDevice, candidate modem.Candidate, ipCommand, apn, ipVersion, detail string) (result NetworkResult, err error) {
	owner := state.dataOwner
	if owner == nil || owner.interfaceID == 0 {
		return result, ErrDataCleanupPending
	}
	// Every configuration failure is rolled back through the original clients;
	// an uncertain stop keeps both clients and the cleanup fence available.
	defer func() {
		if err != nil {
			rollbackQMIDataHost(state, candidate, ipCommand)
		}
	}()
	if err = validateIPv6DataHostClaim(candidate.NetworkInterface, owner); err != nil {
		return result, err
	}
	reader, ok := state.dataSession.(qmiIPv6RuntimeSource)
	if !ok {
		return result, ErrDataBackendUnavailable
	}
	runtimeContext, cancel := context.WithTimeout(ctx, qmiDataStatusTimeout)
	v6, runtimeErr := reader.RuntimeIPv6(runtimeContext)
	cancel()
	if runtimeErr != nil {
		return result, runtimeErr
	}
	address := net.ParseIP(v6.Address)
	if address == nil || address.To4() != nil || !address.IsGlobalUnicast() || address.IsLinkLocalUnicast() || v6.Prefix < 1 || v6.Prefix > 128 {
		return result, errors.New("invalid IPv6 data runtime")
	}
	var v4 qmiIPv4Settings
	if ipVersion == "IPV4V6" {
		read, cancel := context.WithTimeout(ctx, qmiDataStatusTimeout)
		v4, err = state.dataSession.RuntimeIPv4(read)
		cancel()
		if err != nil {
			return result, err // a single family is not dual-stack readiness
		}
	}
	if err = prepareDataIPv6Settings(owner); err != nil {
		return result, err
	}
	if out, linkErr := exec.CommandContext(ctx, ipCommand, "link", "set", "dev", candidate.NetworkInterface, "up").CombinedOutput(); linkErr != nil {
		return result, fmt.Errorf("enable IPv6 cellular link: %w: %s", linkErr, strings.TrimSpace(string(out)))
	}
	mtu := v6.MTU
	if v4.MTU > 0 && (mtu == 0 || v4.MTU < mtu) {
		mtu = v4.MTU
	}
	if mtu != 0 {
		if mtu < 1280 || mtu > 65535 {
			return result, errors.New("cellular runtime MTU cannot carry IPv6")
		}
		if out, mtuErr := exec.CommandContext(ctx, ipCommand, "link", "set", "dev", candidate.NetworkInterface, "mtu", strconv.Itoa(mtu)).CombinedOutput(); mtuErr != nil {
			return result, fmt.Errorf("set IPv6 cellular MTU: %w: %s", mtuErr, strings.TrimSpace(string(out)))
		}
	}
	if ipVersion == "IPV4V6" {
		owner.ipv4Address = fmt.Sprintf("%s/%d", v4.Address, v4.Prefix)
		owner.ipv4Routes = true
		if _, err = configureExportProxyIPv4(ctx, ipCommand, candidate.NetworkInterface, v4.Address, v4.Prefix, v4.Gateway, v4.DNS); err != nil {
			return result, err
		}
	}
	prefix := fmt.Sprintf("%s/%d", address.String(), v6.Prefix)
	if owner.ipv6Address != prefix {
		if err = clearOwnedDataIPv6Address(ctx, ipCommand, candidate.NetworkInterface, owner.ipv6Address); err != nil {
			return result, err
		}
	}
	owner.ipv6Address = prefix // retain even an ambiguous kernel command result
	if out, addrErr := exec.CommandContext(ctx, ipCommand, "-6", "addr", "replace", prefix, "dev", candidate.NetworkInterface, "nodad").CombinedOutput(); addrErr != nil {
		return result, fmt.Errorf("configure IPv6 cellular address: %w: %s", addrErr, strings.TrimSpace(string(out)))
	}
	mark, table, priority := exportProxyRouteIdentity(candidate.NetworkInterface)
	owner.ipv6Routes = true
	args := []string{"-6", "route", "replace", "table", strconv.Itoa(table), "default"}
	if v6.Gateway != "" {
		gateway := net.ParseIP(v6.Gateway)
		if gateway == nil || gateway.To4() != nil || gateway.IsUnspecified() || gateway.IsMulticast() {
			return result, errors.New("invalid IPv6 data gateway")
		}
		args = append(args, "via", gateway.String())
	}
	args = append(args, "dev", candidate.NetworkInterface, "onlink")
	if out, routeErr := exec.CommandContext(ctx, ipCommand, args...).CombinedOutput(); routeErr != nil {
		return result, fmt.Errorf("configure IPv6 protected route: %w: %s", routeErr, strings.TrimSpace(string(out)))
	}
	_, _ = exec.CommandContext(ctx, ipCommand, "-6", "rule", "del", "priority", strconv.Itoa(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", strconv.Itoa(table)).CombinedOutput()
	if out, ruleErr := exec.CommandContext(ctx, ipCommand, "-6", "rule", "add", "priority", strconv.Itoa(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", strconv.Itoa(table)).CombinedOutput(); ruleErr != nil {
		return result, fmt.Errorf("configure IPv6 protected rule: %w: %s", ruleErr, strings.TrimSpace(string(out)))
	}
	dns := append(append([]string(nil), v6.DNS...), v4.DNS...)
	if len(dns) == 0 {
		dns = []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}
		if ipVersion == "IPV4V6" {
			dns = append(dns, "1.1.1.1", "8.8.8.8")
		}
	}
	if err = writeExportProxyDNS(candidate.NetworkInterface, dns); err != nil {
		return result, err
	}
	if err = qmiDataHostReadyForFamily(ctx, candidate.NetworkInterface, owner.ipFamily); err != nil {
		return result, err
	}
	return NetworkResult{Enabled: true, Backend: "qmi", Interface: candidate.NetworkInterface, ControlDevice: candidate.QMIControl, APN: apn, IPVersion: ipVersion, Detail: detail + "\nprotected " + ipVersion + " runtime configuration"}, nil
}
