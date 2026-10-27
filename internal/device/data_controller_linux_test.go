//go:build linux

package device

import (
	"context"
	"errors"
	"testing"
	"vocat/internal/modem"
)

type controllerFixture struct {
	candidate   modem.Candidate
	sets, stops int
	active      bool
	stopErr     error
}

func (c *controllerFixture) Admitted(id string) bool { return c.active && c.candidate.ID == id }

func (c *controllerFixture) Set(_ context.Context, candidate modem.Candidate, iccid, imsi string, r NetworkRequest) (NetworkResult, error) {
	if iccid != "1234567890123456789" || imsi != "123456789012345" {
		return NetworkResult{}, ErrSIMObservation
	}
	c.sets++
	c.active = true
	c.candidate = candidate
	return NetworkResult{Enabled: true, Backend: "qmi", Interface: candidate.NetworkInterface, ControlDevice: candidate.QMIControl}, nil
}
func (c *controllerFixture) Stop(context.Context, string) error {
	c.stops++
	if c.stopErr != nil {
		return c.stopErr
	}
	c.active = false
	return nil
}
func (c *controllerFixture) Status(context.Context, string) (NetworkStatus, error) {
	return NetworkStatus{Connected: c.active, Backend: "qmi", Interface: c.candidate.NetworkInterface}, nil
}
func (c *controllerFixture) Owns(candidate modem.Candidate) bool {
	return c.active && SameDataPhysical(c.candidate, candidate)
}
func (c *controllerFixture) Close(context.Context) error { return nil }

func TestDelegatedDataUsesOriginalSIMWithoutOpeningMainWDS(t *testing.T) {
	ctrl := &controllerFixture{}
	candidate := modem.Candidate{ID: "fixture", USBGeneration: "1:7", QMIControl: "/dev/fixture", NetworkInterface: "fixture0"}
	state := &managedDevice{candidate: candidate, discovered: true}
	m := &Manager{started: true, devices: map[string]*managedDevice{candidate.ID: state}, dataNetwork: ctrl, qmiDataOpener: func(context.Context, string) (qmiDataSession, error) {
		t.Fatal("main opened a second WDS owner")
		return nil, ErrDataBackendUnavailable
	}}
	_, err := m.SetNetworkForSIM(context.Background(), candidate.ID, "1234567890123456789", "123456789012345", NetworkRequest{Enabled: true, APN: "internet", IPVersion: "IPV4V6", Backend: "qmi"})
	if err != nil || ctrl.sets != 1 || !m.OwnsDataClaim(candidate) {
		t.Fatal("delegation not admitted", err)
	}
	if s, err := m.NetworkStatus(context.Background(), candidate.ID); err != nil || !s.Connected {
		t.Fatal("original status not used", err)
	}
	ctrl.stopErr = ErrDataCleanupPending
	if err := m.StopOwnedNetwork(context.Background(), candidate.ID); !errors.Is(err, ErrDataCleanupPending) || !ctrl.active {
		t.Fatal("uncertain delegated stop acknowledged", err)
	}
	ctrl.stopErr = nil
	if err := m.StopOwnedNetwork(context.Background(), candidate.ID); err != nil || ctrl.active {
		t.Fatal("delegated original stop not joined", err)
	}
}

func TestDelegatedDataPreservesLocalATOwner(t *testing.T) {
	for _, control := range []string{"", "/dev/fixture"} {
		candidate := modem.Candidate{ID: "local-at", QMIControl: control, NetworkInterface: "fixture0"}
		ctrl := &controllerFixture{stopErr: ErrDataCleanupPending}
		state := &managedDevice{candidate: candidate, discovered: true, dataOwner: &ownedNetwork{candidate: candidate, backend: "at", radioStopped: true}}
		m := &Manager{started: true, devices: map[string]*managedDevice{candidate.ID: state}, dataNetwork: ctrl}
		if err := m.StopOwnedNetwork(context.Background(), candidate.ID); err != nil || state.dataOwner != nil || ctrl.stops != 0 {
			t.Fatal("local AT owner was delegated or left active", control, err)
		}
		if control == "" {
			if err := m.stopDelegatedData(context.Background(), candidate.ID); err != nil || ctrl.stops != 0 {
				t.Fatal("AT-only RF depended on QMI owner availability", err)
			}
		}
	}
}

func TestDelegatedDataCannotReplaceUnreleasedLocalATOwner(t *testing.T) {
	candidate := modem.Candidate{ID: "local-at", QMIControl: "/dev/fixture", NetworkInterface: "fixture0"}
	ctrl := &controllerFixture{}
	state := &managedDevice{candidate: candidate, discovered: true, dataOwner: &ownedNetwork{candidate: candidate, backend: "at"}}
	m := &Manager{started: true, devices: map[string]*managedDevice{candidate.ID: state}, dataNetwork: ctrl}
	_, err := m.SetNetworkForSIM(context.Background(), candidate.ID, "1234567890123456789", "123456789012345", NetworkRequest{Enabled: true, APN: "internet", Backend: "qmi"})
	if !errors.Is(err, ErrDataCleanupPending) || ctrl.sets != 0 || state.dataOwner == nil {
		t.Fatal("QMI delegate bypassed local AT cleanup", err)
	}
}
