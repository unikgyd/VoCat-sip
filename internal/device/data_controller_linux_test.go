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

type handoverDataController struct {
	controllerFixture
	afterStop func()
}

func (c *handoverDataController) Stop(ctx context.Context, id string) error {
	if err := c.controllerFixture.Stop(ctx, id); err != nil {
		return err
	}
	if c.afterStop != nil {
		c.afterStop()
	}
	return nil
}

type handoverDataJournal struct{ pending bool }

func (j handoverDataJournal) Pending(context.Context, modem.Candidate) (bool, error) {
	return j.pending, nil
}
func (j handoverDataJournal) Acquire(context.Context, modem.Candidate, string, string, string) (DataClaim, error) {
	return nil, ErrDataCleanupPending
}

func TestDelegatedChangedGenerationJoinsOnlyAcknowledgedOriginalPeer(t *testing.T) {
	for _, scenario := range []string{"acknowledged", "stop pending", "journal pending", "changed during acknowledgement"} {
		t.Run(scenario, func(t *testing.T) {
			original := modem.Candidate{ID: "fixture", USBGeneration: "1:7", QMIControl: "/dev/fixture", NetworkInterface: "fixture0"}
			current := original
			current.USBGeneration = "1:8"
			state := &managedDevice{candidate: current, discovered: true}
			ctrl := &handoverDataController{controllerFixture: controllerFixture{candidate: original, active: true}}
			if scenario == "stop pending" {
				ctrl.stopErr = ErrDataCleanupPending
			}
			if scenario == "changed during acknowledgement" {
				ctrl.afterStop = func() { state.candidate.USBGeneration = "1:9" }
			}
			m := &Manager{started: true, devices: map[string]*managedDevice{current.ID: state}, dataNetwork: ctrl, dataClaimJournal: handoverDataJournal{pending: scenario == "journal pending"}}
			_, err := m.SetNetworkForSIM(context.Background(), current.ID, "1234567890123456789", "123456789012345", NetworkRequest{Enabled: true, APN: "internet", IPVersion: "IPV4V6", Backend: "qmi"})
			if ctrl.stops != 1 {
				t.Fatal("old original peer was not joined", err)
			}
			if scenario == "acknowledged" {
				if err != nil || ctrl.sets != 1 || !ctrl.Owns(current) {
					t.Fatal("confirmed cleanup did not allow a fresh session", err)
				}
			} else if err == nil || ctrl.sets != 0 {
				t.Fatal("new session bypassed unresolved or changed ownership", err)
			}
		})
	}
}
