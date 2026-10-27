package device

import (
	"context"
	"vocat/internal/modem"
)

// DataNetworkController keeps the original data capability in a separately
// supervised process. It must never adopt a foreign CID or acknowledge an
// unknown owner. Owns is metadata-only and cannot perform I/O or take device locks.
type DataNetworkController interface {
	Set(context.Context, modem.Candidate, string, string, NetworkRequest) (NetworkResult, error)
	Stop(context.Context, string) error
	Status(context.Context, string) (NetworkStatus, error)
	Owns(modem.Candidate) bool
	Admitted(string) bool // includes admission/cleanup awaiting acknowledgement
	Close(context.Context) error
}

func SameDataPhysical(a, b modem.Candidate) bool {
	return sameSIMWritePhysical(a, b) && a.NetworkInterface == b.NetworkInterface
}

// HasOwnedNetwork is only for disposing this manager's original live capability.
// A durable record without that capability does not make this true.
func (m *Manager) HasOwnedNetwork(id string) bool {
	m.mu.RLock()
	state := m.devices[id]
	m.mu.RUnlock()
	if state == nil {
		return false
	}
	state.dataMu.Lock()
	defer state.dataMu.Unlock()
	return state.dataOwner != nil || state.dataSession != nil
}

func (m *Manager) stopDelegatedData(ctx context.Context, id string) error {
	if m.dataNetwork == nil {
		return nil
	}
	m.mu.RLock()
	state := m.devices[id]
	atOnly := state != nil && state.candidate.QMIControl == ""
	m.mu.RUnlock()
	if atOnly && !m.dataNetwork.Admitted(id) {
		return m.requireNoPendingDataClaim(ctx, modem.Candidate{ID: id})
	}
	if err := m.dataNetwork.Stop(ctx, id); err != nil {
		return err
	}
	return m.requireNoPendingDataClaim(ctx, modem.Candidate{ID: id})
}
