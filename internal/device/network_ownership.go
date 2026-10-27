package device

import (
	"context"
	"errors"
	"strings"

	"vocat/internal/modem"
)

var ErrDataCleanupPending = errors.New("original cellular data cleanup has not been confirmed")

// dataMu owns this capability. It is not reconstructed from a reused control
// basename, current SIM, APN, or the current desired device configuration.
type ownedNetwork struct {
	claim          DataClaim
	hostReceipt    *DataHostReceipt
	candidate      modem.Candidate
	backend        string
	guard          *simWriteGuard
	recoveryClient modem.Client // retained after restart submission until old-generation cleanup
	interfaceID    int
	hostTouched    bool
	handleKnown    bool
	radioStopped   bool // confirmed RF-off ended this generation's packet calls
	packetStopped  bool // original WDS stop ACK; journal deletion may still need retry
	ipFamily       uint8
	requestKey     string
	ipv6Address    string // exact installed address; never flush foreign IPv6 addresses
	ipv4Address    string
	ipv6Settings   map[string]string
	ipv4Routes     bool
	ipv6Routes     bool
	hostMTU        int
	hostLinkUp     bool
}

func (m *Manager) SetNetworkForSIM(ctx context.Context, id, iccid, imsi string, request NetworkRequest) (NetworkResult, error) {
	bound, err := contextForSIMWrite(ctx, iccid, imsi)
	if err != nil {
		return NetworkResult{}, err
	}
	if !request.Enabled {
		return NetworkResult{}, m.StopOwnedNetwork(bound, id)
	}
	return m.SetNetwork(bound, id, request)
}

func (m *Manager) ownedNetworkPhysicalCurrent(state *managedDevice, owner *ownedNetwork) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.devices[owner.candidate.ID] == state && state.discovered && sameSIMWritePhysical(owner.candidate, state.candidate) && owner.candidate.NetworkInterface == state.candidate.NetworkInterface
}

// StopOwnedNetwork never opens a new WDS client or sends StopAny. The line
// coordinator first joins its workers; these device locks then serialize the
// original capability against direct RF/data operations as well.
func (m *Manager) StopOwnedNetwork(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	state := m.devices[id] // retained inventory also holds disconnected owners
	m.mu.RUnlock()
	if state == nil {
		if m.dataNetwork != nil {
			return m.stopDelegatedData(ctx, id)
		}
		return m.requireNoPendingDataClaim(ctx, modem.Candidate{ID: id})
	}
	if err := lockMutexContext(ctx, &state.opMu); err != nil {
		return err
	}
	defer state.opMu.Unlock()
	if err := lockMutexContext(ctx, &state.dataMu); err != nil {
		return err
	}
	defer state.dataMu.Unlock()
	owner := state.dataOwner
	if owner == nil {
		if m.dataNetwork != nil && (m.candidateFor(state).QMIControl != "" || m.dataNetwork.Admitted(id)) {
			return m.stopDelegatedData(ctx, id)
		}
		if state.dataSession != nil {
			return ErrDataCleanupPending
		}
		return m.requireNoPendingDataClaim(ctx, m.candidateFor(state))
	}
	if owner.backend == "qmi" {
		return m.stopOwnedQMINetwork(ctx, state, owner)
	}
	if owner.radioStopped {
		state.dataOwner = nil
		return nil
	}
	if !m.ownedNetworkPhysicalCurrent(state, owner) {
		state.dataOwner = nil // the original USB generation is gone; no new AT I/O
		return nil
	}
	if owner.guard == nil || owner.guard.binding.iccid == "" {
		return ErrDataCleanupPending
	}
	cleanup := *owner.guard
	if m.legacyWriteICCID != nil {
		current, err := m.legacyWriteICCID(ctx, owner.candidate)
		if err != nil {
			return errors.Join(ErrDataCleanupPending, err)
		}
		// Cleanup may follow an application-policy change but remains fixed
		// to the original actual SIM. The current Host owner still denies AT.
		cleanup.binding.policyICCID = strings.TrimSpace(current)
		cleanup.binding.policyPinned = true
	}
	client, err := m.clientLocked(ctx, state, owner.candidate)
	if err != nil {
		return err
	}
	if err := cleanup.verify(ctx, atSIMWriteReader{client}, false); err != nil {
		// A replacement card's CID 1 must never be deactivated. A read-only
		// inactive observation can prove there is no remaining original PDP.
		if !errors.Is(err, ErrSIMObservation) || !atPDPInactive(ctx, client) {
			return errors.Join(ErrDataCleanupPending, err)
		}
		state.dataOwner = nil
		return nil
	}
	if _, err := m.command(ctx, client, "AT+CGACT=0,1"); err != nil {
		return errors.Join(ErrDataCleanupPending, err)
	}
	state.dataOwner = nil
	return nil
}

func atPDPInactive(ctx context.Context, client modem.Client) bool {
	response, err := client.Execute(ctx, "AT+CGACT?")
	if err != nil || !response.OK() {
		return false
	}
	for _, line := range response.Lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CGACT:") {
			continue
		}
		parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+CGACT:")), ",")
		if len(parts) != 2 {
			return false
		}
		if strings.TrimSpace(parts[0]) == "1" {
			return strings.TrimSpace(parts[1]) == "0"
		}
	}
	return false // absence of a parsed context is not an inactive ACK
}
