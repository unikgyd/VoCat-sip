package qmiport

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseKeepsPortOpenAndSerializesUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wwan0qmi0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	coordinator := newCoordinator(func(path string) (portHandle, error) {
		opens.Add(1)
		return openLeaseTestPort(path)
	})
	t.Cleanup(func() { _ = coordinator.close() })

	first, err := coordinator.acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := coordinator.acquire(ctx, path); err == nil {
		t.Fatal("second acquire succeeded before the first lease was released")
	}
	first.Release()

	second, err := coordinator.acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	second.Release()
	if got := opens.Load(); got != 1 {
		t.Fatalf("keepalive opens = %d, want 1", got)
	}
}

func TestLeaseReopensReplacedDeviceNode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "wwan0qmi0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	coordinator := newCoordinator(func(path string) (portHandle, error) {
		opens.Add(1)
		return openLeaseTestPort(path)
	})
	t.Cleanup(func() { _ = coordinator.close() })

	first, err := coordinator.acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	if got := opens.Load(); got != 2 {
		t.Fatalf("keepalive opens = %d, want 2 after node replacement", got)
	}
}

func TestProxySequenceSharesGateWithoutOpeningRawPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cdc-wdm-test")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	coordinator := newCoordinator(func(path string) (portHandle, error) { opens.Add(1); return openLeaseTestPort(path) })
	t.Cleanup(func() { _ = coordinator.close() })
	sequence, err := coordinator.acquireSequence(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 {
		t.Fatal("proxy-only sequence opened raw driver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := coordinator.acquire(ctx, path); err == nil || opens.Load() != 0 {
		t.Fatal("native user bypassed proxy sequence gate")
	}
	sequence.Release()
	lease, err := coordinator.acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if opens.Load() != 1 {
		t.Fatal("native keeper not opened after sequence release")
	}
}
func TestCancelledSequenceNeverOpensKeeper(t *testing.T) {
	var opens atomic.Int32
	coordinator := newCoordinator(func(path string) (portHandle, error) { opens.Add(1); return nil, os.ErrNotExist })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lease, err := coordinator.acquire(ctx, "/dev/cdc-wdm-not-opened"); err == nil {
		lease.Release()
		t.Fatal("cancelled caller acquired gate")
	}
	if opens.Load() != 0 {
		t.Fatal("cancelled caller opened raw port")
	}
}
