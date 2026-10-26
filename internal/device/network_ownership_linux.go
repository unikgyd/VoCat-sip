//go:build linux

package device

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

func newOwnedQMINetwork(candidateOwner *ownedNetwork) *ownedNetwork {
	if info, err := net.InterfaceByName(candidateOwner.candidate.NetworkInterface); err == nil {
		candidateOwner.interfaceID = info.Index
		candidateOwner.hostMTU = info.MTU
		candidateOwner.hostLinkUp = info.Flags&net.FlagUp != 0
	}
	return candidateOwner
}

// Only the original transport's PDH can be stopped. Unknown Start replies
// retain that transport and may be cleared by a confirmed disconnected status;
// a connected session with no returned PDH requires explicit recovery.
func (m *Manager) stopOwnedQMINetwork(ctx context.Context, state *managedDevice, owner *ownedNetwork) error {
	current := m.ownedNetworkPhysicalCurrent(state, owner)
	if err := clearOwnedQMIHost(ctx, owner, current); err != nil {
		return errors.Join(ErrDataCleanupPending, err)
	}
	if !current {
		// Discovery proved the old generation disappeared. Never send the old
		// PDH through a newly opened client at the recycled control pathname.
		closeQMIDataSession(state)
		owner.packetStopped = true
		return m.releaseDataOwner(ctx, state)
	}
	if owner.radioStopped || owner.packetStopped {
		closeQMIDataSession(state)
		return m.releaseDataOwner(ctx, state)
	}
	if state.dataSession == nil {
		return ErrDataCleanupPending
	}
	var err error
	if owned, ok := state.dataSession.(qmiOwnedDataStopper); ok {
		err = owned.StopOwned(ctx)
	} else if owner.handleKnown {
		err = state.dataSession.Stop(ctx, state.dataSessionHandle)
		if stoppedQMIDataError(err) {
			err = nil
		}
	} else {
		err = ErrDataCleanupPending
	}
	if err != nil {
		connected, observeErr := state.dataSession.Connected(ctx)
		if observeErr != nil || connected {
			return errors.Join(ErrDataCleanupPending, err)
		}
	}
	closeQMIDataSession(state)
	owner.packetStopped = true
	return m.releaseDataOwner(ctx, state)
}

// The lifecycle fence prevents a new VoCat data owner during this operation.
// Only that owner's mark/rule, interface-specific routes and original netdev
// are cleared; a replacement netdev is never flushed or brought down.
func clearOwnedQMIHost(ctx context.Context, owner *ownedNetwork, physicalCurrent bool) error {
	if !owner.hostTouched {
		return nil
	}
	name := owner.candidate.NetworkInterface
	ip, err := exec.LookPath("ip")
	if err != nil {
		return err
	}
	mark, table, priority := exportProxyRouteIdentity(name)
	if owner.ipv6Routes {
		_, _ = exec.CommandContext(ctx, ip, "-6", "rule", "del", "priority", strconv.Itoa(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", strconv.Itoa(table)).CombinedOutput()
		rules, err := exec.CommandContext(ctx, ip, "-6", "rule", "show", "priority", strconv.Itoa(priority)).CombinedOutput()
		if err != nil || strings.Contains(string(rules), fmt.Sprintf("fwmark 0x%x", mark)) {
			return errors.New("original IPv6 cellular routing rule cleanup is not confirmed")
		}
	}
	if owner.ipFamily == 0 || owner.ipFamily == 4 || owner.ipv4Routes {
		_, _ = exec.CommandContext(ctx, ip, "rule", "del", "priority", strconv.Itoa(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", strconv.Itoa(table)).CombinedOutput()
		rules, err := exec.CommandContext(ctx, ip, "rule", "show", "priority", strconv.Itoa(priority)).CombinedOutput()
		if err != nil || strings.Contains(string(rules), fmt.Sprintf("fwmark 0x%x", mark)) {
			return errors.New("original cellular routing rule cleanup is not confirmed")
		}
	}
	_ = os.Remove(exportProxyDNSPath(name))
	if !physicalCurrent {
		return nil // the original kernel netdev/routes disappeared with USB
	}
	info, err := net.InterfaceByName(name)
	if err != nil {
		return errors.New("original cellular netdev cannot be verified")
	}
	if owner.interfaceID == 0 || info.Index != owner.interfaceID {
		return errors.New("cellular netdev ownership changed during cleanup")
	}
	if owner.ipFamily != 0 && owner.ipFamily != 4 {
		if err := validateIPv6DataHostClaim(name, owner); err != nil {
			return err
		}
		if owner.ipv6Routes {
			if err := clearOwnedDataRoutes(ctx, owner, netlink.FAMILY_V6, table); err != nil {
				return err
			}
		}
		if err := clearOwnedDataIPv6Address(ctx, ip, name, owner.ipv6Address); err != nil {
			return err
		}
	}
	commands := [][]string{{"link", "set", "dev", name, "down"}}
	if owner.ipFamily == 0 || owner.ipFamily == 4 {
		commands = append([][]string{{"-4", "route", "flush", "table", strconv.Itoa(table), "dev", name}, {"-4", "addr", "flush", "dev", name, "scope", "global"}}, commands...)
	} else {
		if owner.ipv4Routes {
			if err := clearOwnedDataRoutes(ctx, owner, netlink.FAMILY_V4, table); err != nil {
				return err
			}
		}
		if err := clearOwnedDataAddress(ctx, ip, name, "-4", owner.ipv4Address); err != nil {
			return err
		}
	}
	for _, args := range commands {
		if _, err := exec.CommandContext(ctx, ip, args...).CombinedOutput(); err != nil {
			return errors.New("original cellular host configuration cleanup failed")
		}
	}
	if err := restoreDataIPv6Settings(owner); err != nil {
		return err
	}
	if owner.ipFamily != 0 && owner.ipFamily != 4 {
		if owner.hostMTU > 0 {
			if _, err := exec.CommandContext(ctx, ip, "link", "set", "dev", name, "mtu", strconv.Itoa(owner.hostMTU)).CombinedOutput(); err != nil {
				return err
			}
		}
		if owner.hostLinkUp {
			if _, err := exec.CommandContext(ctx, ip, "link", "set", "dev", name, "up").CombinedOutput(); err != nil {
				return err
			}
		}
	}
	owner.hostTouched = false
	return nil
}
