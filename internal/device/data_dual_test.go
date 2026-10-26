//go:build linux

package device

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/modem"
)

type dualDataPart struct {
	fakeQMIDataSession
	family    uint8
	handle    uint32
	active    bool
	startErr  error
	starts    int
	stopCount int
}

func (s *dualDataPart) Start(_ context.Context, _, _, _ string, _, family uint8) (uint32, error) {
	s.family, s.starts, s.active = family, s.starts+1, true
	return s.handle, s.startErr
}
func (s *dualDataPart) Stop(_ context.Context, handle uint32) error {
	s.stopHandle, s.stopCount = handle, s.stopCount+1
	if s.stopErr == nil {
		s.active = false
	}
	return s.stopErr
}
func (s *dualDataPart) Connected(context.Context) (bool, error) { return s.active, nil }
func (*dualDataPart) RuntimeIPv4(context.Context) (qmiIPv4Settings, error) {
	return qmiIPv4Settings{Address: "192.0.2.2", Prefix: 24, Gateway: "192.0.2.1", DNS: []string{"192.0.2.53"}, MTU: 1428}, nil
}
func (*dualDataPart) RuntimeIPv6(context.Context) (qmiIPv6Settings, error) {
	return qmiIPv6Settings{Address: "2001:db8:1234::2", Prefix: 64, Gateway: "fe80::1", DNS: []string{"2001:db8:1234::53"}, MTU: 1428}, nil
}

func dualDataFixture() (*dualQMIDataSession, *dualDataPart, *dualDataPart) {
	v4, v6 := &dualDataPart{handle: 14}, &dualDataPart{handle: 26}
	return &dualQMIDataSession{parts: [2]qmiDataSession{v4, v6}}, v4, v6
}

func TestDualDataRetainsSeparateOriginalHandlesAndBothRuntimeFamilies(t *testing.T) {
	s, v4, v6 := dualDataFixture()
	if _, err := s.Start(context.Background(), "internet", "", "", 0, qmiDataDualFamily); err != nil {
		t.Fatal(err)
	}
	if v4.family != 4 || v6.family != 6 || v4.starts != 1 || v6.starts != 1 {
		t.Fatal("dual selector was sent as a protocol family")
	}
	if err := s.AllConnected(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RuntimeIPv4(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RuntimeIPv6(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.StopOwned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v4.stopHandle != 14 || v6.stopHandle != 26 || v4.stopAnyCalls != 0 || v6.stopAnyCalls != 0 {
		t.Fatal("cleanup did not use the two original handles")
	}
}

func TestDualDataAmbiguousSecondStartCleansKnownFirstWithoutForeignStop(t *testing.T) {
	s, v4, v6 := dualDataFixture()
	v6.handle, v6.startErr = 0, context.DeadlineExceeded
	if _, err := s.Start(context.Background(), "internet", "", "", 0, qmiDataDualFamily); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("ambiguous IPv6 start was accepted", err)
	}
	if err := s.StopOwned(context.Background()); !errors.Is(err, ErrDataCleanupPending) {
		t.Fatal("unknown connected IPv6 client was forgotten", err)
	}
	if v4.stopHandle != 14 || v6.stopCount != 0 || v6.stopAnyCalls != 0 || !s.attempted[1] || s.attempted[0] {
		t.Fatal("cleanup used a foreign/global handle or lost uncertainty")
	}
	if _, err := s.Start(context.Background(), "internet", "", "", 0, qmiDataDualFamily); !errors.Is(err, ErrDataCleanupPending) || v4.starts != 1 || v6.starts != 1 {
		t.Fatal("uncertain packet calls were replayed")
	}
	v6.active = false
	if err := s.StopOwned(context.Background()); err != nil {
		t.Fatal("confirmed disconnection did not settle original cleanup", err)
	}
}

func TestDualDataPartialDisconnectAndStopFailureRetainRemainingFamily(t *testing.T) {
	s, v4, v6 := dualDataFixture()
	_, _ = s.Start(context.Background(), "internet", "", "", 0, qmiDataDualFamily)
	v6.active = false
	if connected, err := s.Connected(context.Background()); err != nil || !connected {
		t.Fatal("live IPv4 owner would be discarded after IPv6 loss")
	}
	if err := s.AllConnected(context.Background()); err == nil {
		t.Fatal("partial service was called dual-stack ready")
	}
	v4.stopErr = context.DeadlineExceeded
	if err := s.StopOwned(context.Background()); !errors.Is(err, ErrDataCleanupPending) || v6.stopHandle != 26 || !s.attempted[0] {
		t.Fatal("one failed stop prevented cleanup of the other family", err)
	}
	v4.stopErr = nil
	if err := s.StopOwned(context.Background()); err != nil || v6.stopCount != 1 {
		t.Fatal("original retry replayed cleanup on a settled family", err)
	}
}

func TestDualDataSIMChangeAndCancellationPreventSecondFamilyWrite(t *testing.T) {
	for _, mode := range []string{"sim-change", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, v4, v6 := dualDataFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.verify = func(context.Context, qmiDataSession) error {
				calls++
				if calls == 2 {
					if mode == "cancel" {
						cancel()
					} else {
						return ErrSIMObservation
					}
				}
				return nil
			}
			if _, err := s.Start(ctx, "internet", "", "", 0, qmiDataDualFamily); err == nil || v4.starts != 1 || v6.starts != 0 {
				t.Fatal("changed/canceled subscription started another family", err)
			}
			if err := s.StopOwned(context.Background()); err != nil || v4.stopHandle != 14 || v6.stopCount != 0 {
				t.Fatal("original first family was not cleaned", err)
			}
		})
	}
}

func TestDualDataManagerRetainsHalfKnownTransportUntilBothPartsSettle(t *testing.T) {
	s, v4, v6 := dualDataFixture()
	v6.handle, v6.startErr = 0, context.DeadlineExceeded
	_, _ = s.Start(context.Background(), "internet", "", "", 0, qmiDataDualFamily)
	candidate := modem.Candidate{ID: "pair", QMIControl: "/dev/owned-pair"}
	state := &managedDevice{candidate: candidate, discovered: true, dataSession: s,
		dataOwner: &ownedNetwork{candidate: candidate, backend: "qmi", ipFamily: qmiDataDualFamily}}
	manager := &Manager{devices: map[string]*managedDevice{candidate.ID: state}}
	if err := manager.StopOwnedNetwork(context.Background(), candidate.ID); !errors.Is(err, ErrDataCleanupPending) {
		t.Fatal("manager forgot a half-known original client", err)
	}
	if state.dataOwner == nil || state.dataSession != s || v4.closed || v6.closed || v4.stopHandle != 14 || v6.stopCount != 0 {
		t.Fatal("manager closed a live uncertain transport or used a foreign handle")
	}
	v6.active = false
	if err := manager.StopOwnedNetwork(context.Background(), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if state.dataOwner != nil || state.dataSession != nil || !v4.closed || !v6.closed {
		t.Fatal("confirmed original cleanup did not close both transports")
	}
}
