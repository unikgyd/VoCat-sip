package server

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/device"
)

func transferRuntime() (*cellularDataRuntime, *cellularDataSession) {
	session := &cellularDataSession{owned: true, physicalID: "physical-a", identity: "8901000000000000000", phase: cellularDataSessionActive}
	r := newCellularDataRuntime(nil, nil)
	r.entries["a"] = &cellularDataRuntimeEntry{session: session, status: cellularDataRuntimeStatus{DesiredEnabled: true, Connected: true, Phase: "connected", Revision: 7}}
	return r, session
}

func TestCellularTransferRequiresAcknowledgedSessionAndOriginalBinding(t *testing.T) {
	for _, mode := range []string{"unowned", "running", "closing", "quiescing", "shutdown", "maintenance", "disconnected", "physical", "sim"} {
		t.Run(mode, func(t *testing.T) {
			r, s := transferRuntime()
			physical, iccid := "physical-a", s.identity
			e := r.entries["a"]
			switch mode {
			case "unowned":
				s.owned = false
			case "running":
				e.running = true
			case "closing":
				s.phase = cellularDataSessionClosing
			case "quiescing":
				r.quiescing["a"] = true
			case "shutdown":
				r.closing = true
			case "maintenance":
				e.status.MaintenancePhase = "pending"
			case "disconnected":
				e.status.Connected = false
			case "physical":
				physical = "other"
			case "sim":
				iccid = "other"
			}
			if _, release, err := r.acquireTransfer(context.Background(), "a", physical, iccid, cellularDataTransferBinding{}); err == nil || release != nil {
				t.Fatal("unowned transfer admitted")
			}
			if r.activeDataGeneration("a", iccid) != 0 && mode != "physical" {
				t.Fatal("stale IP lookup generation exposed")
			}
		})
	}
	r, s := transferRuntime()
	binding := cellularDataTransferBinding{Interface: "wwan0", Index: 5, USBGeneration: "first"}
	_, release, err := r.acquireTransfer(context.Background(), "a", s.physicalID, s.identity, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	binding.USBGeneration = "replacement"
	if _, _, err := r.acquireTransfer(context.Background(), "a", s.physicalID, s.identity, binding); !errors.Is(err, device.ErrSIMObservation) {
		t.Fatal("USB replacement borrowed original data session", err)
	}
}

func TestCellularTransferCancellationRetainsSupersededSessionUntilRelease(t *testing.T) {
	r, s := transferRuntime()
	ctx, release, err := r.acquireTransfer(context.Background(), "a", s.physicalID, s.identity, cellularDataTransferBinding{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	r.mu.Lock()
	r.entries["a"] = &cellularDataRuntimeEntry{}
	done := r.cancelTransfersLocked("a", nil)
	r.mu.Unlock()
	if len(done) != 1 || ctx.Err() != context.Canceled {
		t.Fatal("superseded transfer escaped cancellation")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitCellularTransfers(canceled, done); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("cleanup acknowledged before socket owner exited", err)
	}
	release()
	release()
	if err := waitCellularTransfers(context.Background(), done); err != nil || len(r.transfers) != 0 {
		t.Fatal("release did not join all transfers", err)
	}
}
