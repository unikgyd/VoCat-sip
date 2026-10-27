package device

import (
	"context"
	"sync"

	"vocat/internal/modem"
)

// A journal is a crash fence. It cannot reconstruct a WDS client or authorize
// stopping an unknown modem-wide call. Only the original live owner releases it.
type DataClaim interface{ Release(context.Context) error }
type DataClaimJournal interface {
	Acquire(context.Context, modem.Candidate, string, string, string) (DataClaim, error)
	Pending(context.Context, modem.Candidate) (bool, error)
}

type dataClaims struct {
	sync.RWMutex
	active map[string]DataClaim
}

// OwnsDataClaim is metadata-only and safe inside the policy callback. It is
// positive evidence only for this process's retained journal capability.
func (m *Manager) OwnsDataClaim(candidate modem.Candidate) bool {
	if m.dataNetwork != nil && m.dataNetwork.Owns(candidate) {
		return true
	}
	m.dataClaims.RLock()
	defer m.dataClaims.RUnlock()
	_, ok := m.dataClaims.active[dataClaimPhysicalKey(candidate)]
	return ok
}

func dataClaimPhysicalKey(c modem.Candidate) string {
	return c.ID + "\x00" + c.USBPath + "\x00" + c.USBGeneration + "\x00" + c.QMIControl + "\x00" + c.NetworkInterface
}

func (m *Manager) retainDataClaim(candidate modem.Candidate, claim DataClaim) {
	m.dataClaims.Lock()
	defer m.dataClaims.Unlock()
	if m.dataClaims.active == nil {
		m.dataClaims.active = make(map[string]DataClaim)
	}
	m.dataClaims.active[dataClaimPhysicalKey(candidate)] = claim
}

func (m *Manager) releaseDataOwner(ctx context.Context, state *managedDevice) error {
	owner := state.dataOwner
	if owner != nil && owner.claim != nil {
		if err := owner.claim.Release(ctx); err != nil {
			return ErrDataCleanupPending
		}
		m.dataClaims.Lock()
		delete(m.dataClaims.active, dataClaimPhysicalKey(owner.candidate))
		m.dataClaims.Unlock()
	}
	state.dataOwner = nil
	return nil
}

func (m *Manager) requireNoPendingDataClaim(ctx context.Context, candidate modem.Candidate) error {
	if m.dataClaimJournal == nil {
		return nil
	}
	pending, err := m.dataClaimJournal.Pending(ctx, candidate)
	if err != nil || pending {
		return ErrDataCleanupPending
	}
	return nil
}
