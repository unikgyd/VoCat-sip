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
)

func newOwnedQMINetwork(candidateOwner *ownedNetwork) *ownedNetwork {
	if info, err := net.InterfaceByName(candidateOwner.candidate.NetworkInterface); err == nil {
		candidateOwner.interfaceID = info.Index
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
		state.dataOwner = nil
		return nil
	}
	if owner.radioStopped {
		closeQMIDataSession(state)
		state.dataOwner = nil
		return nil
	}
	if state.dataSession == nil {
		return ErrDataCleanupPending
	}
	var err error
	if owner.handleKnown {
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
	state.dataOwner = nil
	return nil
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
	_, _ = exec.CommandContext(ctx, ip, "rule", "del", "priority", strconv.Itoa(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", strconv.Itoa(table)).CombinedOutput()
	rules, err := exec.CommandContext(ctx, ip, "rule", "show", "priority", strconv.Itoa(priority)).CombinedOutput()
	if err != nil || strings.Contains(string(rules), fmt.Sprintf("fwmark 0x%x", mark)) {
		return errors.New("original cellular routing rule cleanup is not confirmed")
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
	for _, args := range [][]string{
		{"-4", "route", "flush", "table", strconv.Itoa(table), "dev", name},
		{"-4", "addr", "flush", "dev", name, "scope", "global"},
		{"link", "set", "dev", name, "down"},
	} {
		if _, err := exec.CommandContext(ctx, ip, args...).CombinedOutput(); err != nil {
			return errors.New("original cellular host configuration cleanup failed")
		}
	}
	owner.hostTouched = false
	return nil
}
