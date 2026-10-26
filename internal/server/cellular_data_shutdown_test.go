package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/device"
)

type shutdownDataController struct {
	fakeDeviceController
	stops chan string
	stop  func(context.Context, string) error
}

func (c *shutdownDataController) StopOwnedNetwork(ctx context.Context, id string) error {
	c.stops <- id
	if c.stop != nil {
		return c.stop(ctx, id)
	}
	return nil
}

func shutdownDataServer() (*Server, *cellularDataRuntime, *shutdownDataController) {
	r, _ := transferRuntime()
	r.entries["a"].physicalID = "physical-a"
	r.entries["a"].changed = make(chan struct{})
	r.physicalTargets["a"] = map[string]struct{}{"physical-a": {}, "superseded-a": {}}
	c := &shutdownDataController{stops: make(chan string, 8)}
	s := &Server{devices: c}
	s.cellularDataOnce.Do(func() { s.cellularData = r })
	return s, r, c
}

func TestCellularShutdownJoinsSocketsAndLateWorkersBeforeOriginalStops(t *testing.T) {
	s, r, c := shutdownDataServer()
	session := r.entries["a"].session
	transfer, release, err := r.acquireTransfer(context.Background(), "a", session.physicalID, session.identity, cellularDataTransferBinding{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	workerCanceled := make(chan struct{}, 1)
	worker := &cellularDataWorker{done: make(chan struct{}), cancel: func() { workerCanceled <- struct{}{} }}
	r.workers["a"] = map[*cellularDataWorker]struct{}{worker: {}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- s.CloseCellularData(ctx) }()
	select {
	case <-transfer.Done():
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel the original socket owner")
	}
	select {
	case <-workerCanceled:
	case <-ctx.Done():
		t.Fatal("late data worker was not canceled")
	}
	if status := r.request("new-line", "new-physical", device.NetworkRequest{Enabled: true}); status.Phase != "failed" {
		t.Fatal("late startup callback admitted during process shutdown", status)
	}
	if len(c.stops) != 0 {
		t.Fatal("WDS stopped before the original socket owner released")
	}
	release()
	if len(c.stops) != 0 {
		t.Fatal("superseded worker was not joined")
	}
	close(worker.done)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	stopped := map[string]bool{<-c.stops: true, <-c.stops: true}
	if !stopped["physical-a"] || !stopped["superseded-a"] || len(c.stops) != 0 {
		t.Fatal("cleanup did not retain both original physical targets", stopped)
	}
	r.resume("a") // an old task finalizer cannot reopen process admission
	if _, _, err := r.acquireTransfer(context.Background(), "a", session.physicalID, session.identity, cellularDataTransferBinding{}); err == nil {
		t.Fatal("late task finalizer reopened proxy traffic")
	}
}

func TestCellularShutdownRetainsCleanupUntilLateSocketOwnerReleases(t *testing.T) {
	s, r, c := shutdownDataServer()
	session := r.entries["a"].session
	_, release, err := r.acquireTransfer(context.Background(), "a", session.physicalID, session.identity, cellularDataTransferBinding{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.CloseCellularData(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unjoined socket was acknowledged as cleaned", err)
	}
	if len(c.stops) != 0 || len(r.physicalTargets["a"]) != 2 || r.entries["a"].status.MaintenancePhase != "data_cleanup_pending" {
		t.Fatal("uncertain cleanup discarded the original capabilities")
	}
	release()
	if err := s.CloseCellularData(context.Background()); err != nil || len(c.stops) != 2 {
		t.Fatal("confirmed late release could not finish original cleanup", err)
	}
}

func TestCellularShutdownBlockedLineDoesNotStarveAnotherModem(t *testing.T) {
	s, r, c := shutdownDataServer()
	r.entries["b"] = &cellularDataRuntimeEntry{physicalID: "physical-b", changed: make(chan struct{})}
	r.physicalTargets["b"] = map[string]struct{}{"physical-b": {}}
	gate := s.imsModeLock("a")
	gate.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- s.CloseCellularData(ctx) }()
	select {
	case id := <-c.stops:
		if id != "physical-b" {
			t.Fatal("cleanup crossed an active lifecycle writer", id)
		}
	case <-ctx.Done():
		gate.Unlock()
		t.Fatal("the independent modem was starved by another line")
	}
	err := <-completed
	gate.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("active lifecycle owner was discarded at shutdown", err)
	}
}

func TestCellularShutdownFailedOriginalStopRemainsPending(t *testing.T) {
	s, r, c := shutdownDataServer()
	c.stop = func(context.Context, string) error { return device.ErrDataCleanupPending }
	if err := s.CloseCellularData(context.Background()); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("failed WDS cleanup was acknowledged", err)
	}
	if len(r.physicalTargets["a"]) != 2 || r.entries["a"].status.Phase != "failed" {
		t.Fatal("failed original capability was forgotten")
	}
	c.stop = nil
	if err := s.CloseCellularData(context.Background()); err != nil {
		t.Fatal("original stop could not be retried", err)
	}
}
