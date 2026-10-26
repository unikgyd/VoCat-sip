//go:build linux

package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"vocat/internal/modem"
)

type fakeDurableDataClaim struct {
	calls int
	err   error
}

func (c *fakeDurableDataClaim) Release(context.Context) error { c.calls++; return c.err }

type connectedOwnedData struct {
	fakeQMIDataSession
	connected bool
}

func (s *connectedOwnedData) Connected(context.Context) (bool, error) { return s.connected, nil }

type pendingDataJournal struct{}

func (pendingDataJournal) Acquire(context.Context, modem.Candidate, string, string, string) (DataClaim, error) {
	return nil, ErrDataCleanupPending
}
func (pendingDataJournal) Pending(context.Context, modem.Candidate) (bool, error) { return true, nil }

func TestDataClaimRetainedUntilOriginalStopAndJournalACK(t *testing.T) {
	ctx := context.Background()
	candidate := modem.Candidate{ID: "original", USBGeneration: "1:7"}
	claim := &fakeDurableDataClaim{err: errors.New("journal unavailable")}
	session := &connectedOwnedData{fakeQMIDataSession: fakeQMIDataSession{stopErr: context.DeadlineExceeded}, connected: true}
	owner := &ownedNetwork{candidate: candidate, backend: "qmi", claim: claim, handleKnown: true}
	state := &managedDevice{candidate: candidate, discovered: true, dataOwner: owner, dataSession: session, dataSessionHandle: 42}
	m := &Manager{started: true, devices: map[string]*managedDevice{candidate.ID: state}}
	m.retainDataClaim(candidate, claim)
	if err := m.StopOwnedNetwork(ctx, candidate.ID); !errors.Is(err, ErrDataCleanupPending) || claim.calls != 0 || state.dataSession == nil {
		t.Fatal("uncertain stop lost original capability", err)
	}
	session.stopErr = nil
	if err := m.StopOwnedNetwork(ctx, candidate.ID); !errors.Is(err, ErrDataCleanupPending) || claim.calls != 1 || !owner.packetStopped || state.dataOwner == nil {
		t.Fatal("journal failure was acknowledged", err)
	}
	if session.stopHandle != 42 || session.stopAnyCalls != 0 {
		t.Fatal("original WDS handle not used")
	}
	claim.err = nil
	if err := m.StopOwnedNetwork(ctx, candidate.ID); err != nil || state.dataOwner != nil || m.OwnsDataClaim(candidate) || claim.calls != 2 {
		t.Fatal("journal retry did not finish acknowledged cleanup", err)
	}
}

func TestColdDataClaimCannotBeAcknowledgedByEmptyManager(t *testing.T) {
	m := &Manager{dataClaimJournal: pendingDataJournal{}, devices: map[string]*managedDevice{}}
	if err := m.StopOwnedNetwork(context.Background(), "original"); !errors.Is(err, ErrDataCleanupPending) {
		t.Fatal("cold orphan treated as released", err)
	}
	m.devices["original"] = &managedDevice{candidate: modem.Candidate{ID: "original"}, discovered: true}
	if err := m.StopOwnedNetwork(context.Background(), "original"); !errors.Is(err, ErrDataCleanupPending) {
		t.Fatal("inventory without WDS capability acknowledged orphan", err)
	}
}

type claimOrderingSession struct {
	fakeQMIDataSession
	starts int
}

func (s *claimOrderingSession) GetIMEI(context.Context) (string, error) {
	return "123456789012345", nil
}
func (s *claimOrderingSession) GetICCID(context.Context) (string, error) {
	return "1234567890123456789", nil
}
func (s *claimOrderingSession) GetIMSI(context.Context) (string, error) {
	return "123456789012345", nil
}
func (s *claimOrderingSession) Start(context.Context, string, string, string, uint8, uint8) (uint32, error) {
	s.starts++
	return 0, context.DeadlineExceeded
}

type orderingJournal struct {
	claim    *fakeDurableDataClaim
	acquired int
}

func (j *orderingJournal) Acquire(context.Context, modem.Candidate, string, string, string) (DataClaim, error) {
	j.acquired++
	if j.claim == nil {
		return nil, ErrDataCleanupPending
	}
	return j.claim, nil
}
func (j *orderingJournal) Pending(context.Context, modem.Candidate) (bool, error) { return false, nil }

func TestDataClaimIsCommittedBeforeStartAndRetainedOnTimeout(t *testing.T) {
	// No real host tools or netdev are used by this transaction-ordering test.
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "ip"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	for _, allow := range []bool{false, true} {
		journal := &orderingJournal{}
		if allow {
			journal.claim = &fakeDurableDataClaim{}
		}
		session := &claimOrderingSession{}
		candidate := modem.Candidate{ID: "ordering-test", USBGeneration: "1:7", QMIControl: "/dev/fixture", NetworkInterface: "vcatclaimtest"}
		state := &managedDevice{candidate: candidate, discovered: true}
		m := &Manager{started: true, devices: map[string]*managedDevice{candidate.ID: state}, dataClaimJournal: journal, qmiDataOpener: func(context.Context, string) (qmiDataSession, error) { return session, nil }}
		ctx, err := contextForSIMWrite(context.Background(), "1234567890123456789", "123456789012345")
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.setQMINetwork(ctx, state, candidate, true, "internet", "IP", "", "", "NONE")
		if err == nil || journal.acquired != 1 || session.stopAnyCalls != 0 {
			t.Fatal("unexpected transaction", allow, err, journal.acquired)
		}
		if !allow && (session.starts != 0 || !session.closed || state.dataOwner != nil) {
			t.Fatal("Start sent without durable ownership")
		}
		if allow && (session.starts != 1 || session.closed || state.dataOwner == nil || journal.claim.calls != 0 || !m.OwnsDataClaim(candidate)) {
			t.Fatal("Start timeout lost original capability")
		}
	}
}
