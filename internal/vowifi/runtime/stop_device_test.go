package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestStopDeviceJoinsQueuedWorkerAndFencesReconnect(t *testing.T) {
	manager := New(Options{OperationTimeout: time.Second})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if err := manager.Register(testOrchestrator(t, "a")); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(testOrchestrator(t, "b")); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	if _, err := manager.startOperation("a", false, func(context.Context, *vowifi.Orchestrator) error { close(entered); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := manager.StopDevice(ctx, "a")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop did not join running worker: %v", err)
	}
	// The other modem is usable while A is still winding down.
	if _, err := manager.RequestEnabled("b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestReconnect("a"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	if manager.entries["a"].desiredEnabled || manager.entries["a"].reconnectPending {
		manager.mu.Unlock()
		t.Fatal("reconnect crossed maintenance fence")
	}
	manager.mu.Unlock()
	close(release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.StopDevice(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	state, _ := manager.State("a")
	if state.Enabled || state.Active || state.IMSReady {
		t.Fatal("stop returned with live owner")
	}
	manager.EndMaintenance("a")
	if _, err := manager.RequestEnabled("a", true); err != nil {
		t.Fatal(err)
	}
}
