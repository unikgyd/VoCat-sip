package hostims

import (
	"context"
	"errors"
	"testing"
)

func selectionDiscoveryFixture(t *testing.T) (*BearerPool, *fakeBearerBackend, ModemTarget) {
	t.Helper()
	pool, backend, _, _ := bearerFixture(t)
	const path = "/org/freedesktop/ModemManager1/Modem/1"
	backend.mu.Lock()
	snapshot := backend.modems[path]
	snapshot.QMIControl = "/dev/cdc-wdm1"
	backend.modems[path] = snapshot
	backend.mu.Unlock()
	b := snapshot.Binding
	return pool, backend, ModemTarget{Equipment: b.Equipment, Physical: b.Physical, QMIControl: snapshot.QMIControl, ICCID: b.ICCID, IMSI: b.IMSI}
}

func TestHostIMSSelectionTargetNeverChoosesFirstModem(t *testing.T) {
	pool, _, target := selectionDiscoveryFixture(t)
	selection, err := pool.SelectTarget(context.Background(), target)
	if err != nil || selection.snapshot.Path != "/org/freedesktop/ModemManager1/Modem/1" {
		t.Fatal("wrong MM target", err)
	}
}

func TestHostIMSSelectionColdTargetPinsLiveMMSIMWithoutCachedSIMProof(t *testing.T) {
	pool, _, target := selectionDiscoveryFixture(t)
	target.ICCID, target.IMSI = "", ""
	selection, err := pool.SelectTarget(context.Background(), target)
	if err != nil || selection.SubscriberIMSI() == "" || !selection.snapshot.Binding.valid() {
		t.Fatal("MM did not supply a verified live SIM binding", err)
	}
}

func TestHostIMSSelectionRejectsConflictingLiveIdentity(t *testing.T) {
	for _, mode := range []string{"equipment", "iccid", "imsi", "qmi", "physical", "duplicate", "traversal", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			pool, backend, target := selectionDiscoveryFixture(t)
			ctx := context.Background()
			switch mode {
			case "equipment":
				target.Equipment = "123456789012349"
			case "iccid":
				target.ICCID = "8901000000000000099"
			case "imsi":
				target.IMSI = "310260123456799"
			case "qmi":
				target.QMIControl = "/dev/cdc-wdm0"
			case "physical":
				target.Physical = "/sys/devices/virtual/other-modem"
			case "traversal":
				target.Physical = "/sys/devices/virtual/../modem-test"
			case "duplicate":
				backend.mu.Lock()
				s := backend.modems["/org/freedesktop/ModemManager1/Modem/0"]
				s.Binding.Equipment = target.Equipment
				backend.modems[s.Path] = s
				backend.mu.Unlock()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := pool.SelectTarget(ctx, target); err == nil {
				t.Fatal("stale/ambiguous target accepted")
			}
		})
	}
}

func TestHostIMSSelectionClaimsKeepApplicationTransportAlive(t *testing.T) {
	pool, _, target := selectionDiscoveryFixture(t)
	if pool.HasPendingClaims() {
		t.Fatal("empty pool has pending claims")
	}
	selection, err := pool.SelectTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !pool.HasPendingClaims() {
		t.Fatal("active PDN would lose its transport")
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool.HasPendingClaims() {
		t.Fatal("released claim still blocks idle shutdown")
	}
	lease.claim.mu.Lock()
	lease.claim.retained = append(lease.claim.retained, struct{}{})
	lease.claim.mu.Unlock()
	if !pool.HasPendingClaims() {
		t.Fatal("retained unknown resource did not keep transport alive")
	}
	if _, err := pool.SelectTarget(context.Background(), ModemTarget{}); !errors.Is(err, ErrSelection) {
		t.Fatal(err)
	}
}
