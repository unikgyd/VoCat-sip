//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
	"vocat/internal/vowifi/ims"
)

type hostIMSInertInventory struct{}

func (hostIMSInertInventory) List() []device.Device { return nil }

func TestHostIMSFactoryInventoryGuardUsesStrongPhysicalMapping(t *testing.T) {
	// Synthetic discovery rows; these existing sysfs directories are only
	// canonicalization fixtures. No device port, bus, or carrier is contacted.
	const physical = "/sys/devices/virtual"
	if _, err := os.Stat(physical); err != nil {
		t.Skip("sysfs unavailable")
	}
	config, wanted, _ := hostIMSMappingFixture()
	config.DeviceBackend, config.USBPath = "at", physical
	config.ATPort, config.ControlDevice = "/dev/ttyUSB9", "/dev/ttyUSB9"
	wanted.Snapshot, wanted.Candidate.USBPath = nil, physical
	other := wanted
	other.ID, other.Candidate.USBPath = "stale-port-device", "/sys/devices/nonexistent-vocat-fixture"
	other.Candidate.QMIControl, other.Candidate.ATPort.Path = "/dev/cdc-wdm1", config.ATPort
	id, err := hostIMSInventoryPhysicalID(config, []device.Device{other, wanted})
	if err != nil || id != wanted.ID {
		t.Fatal("legacy stale port prevented protecting actual Host IMS modem", err)
	}
	if _, err := hostIMSInventoryPhysicalID(config, []device.Device{wanted, wanted}); err == nil {
		t.Fatal("ambiguous Host IMS guard did not fail closed")
	}
	if _, err := hostIMSInventoryPhysicalID(config, nil); err == nil {
		t.Fatal("missing Host IMS guard did not fail closed")
	}
}

type fakeHostIMSPendingCleanup struct {
	fail      atomic.Bool
	closes    atomic.Int32
	once      sync.Once
	attempted chan struct{}
}

func (c *fakeHostIMSPendingCleanup) Close(context.Context) error {
	c.closes.Add(1)
	c.once.Do(func() { close(c.attempted) })
	if c.fail.Load() {
		return hostims.ErrUncertain
	}
	return nil
}

func TestHostIMSFactoryAcquireErrorRetainsPendingOwnerUntilManagerStopACK(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	f := &linuxHostIMSFactory{lifetime: lifetime, cancel: cancel, held: map[*factoryHostIMSAccess]struct{}{}}
	pending := &fakeHostIMSPendingCleanup{attempted: make(chan struct{})}
	pending.fail.Store(true)
	runtime, err := hostimsruntime.NewRuntime(hostimsruntime.Config{DeviceID: "pending-line", Prepare: func(ctx context.Context) (hostimsruntime.Access, error) {
		_, owner, err := f.acquireBearer(ctx, hostims.Selection{}, hostims.BearerOptions{},
			func(context.Context, hostims.Selection, hostims.BearerOptions) (*hostims.BearerLease, error) {
				return nil, hostims.ErrUncertain
			},
			func(hostims.Selection) (hostIMSCleanup, error) { return pending, nil })
		if owner == nil {
			return nil, errors.New("failed acquisition lost pending owner")
		}
		return owner, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	manager := hostimsruntime.New(hostimsruntime.Options{PollInterval: time.Millisecond, RetryInitial: time.Hour, RetryMaximum: time.Hour,
		Factory: func(context.Context, string) (*hostimsruntime.Runtime, error) { return runtime, nil }})
	t.Cleanup(func() {
		pending.fail.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
		_ = f.Close(ctx)
	})
	if err := manager.Ensure(context.Background(), "pending-line"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestEnabled("pending-line", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending.attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("failed Start did not attempt retained cleanup")
	}
	ctx, cancelStop := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStop()
	mode := store.IMSModeHost
	if err := manager.StopDevice(ctx, "pending-line"); err == nil {
		mode = store.IMSModeModule
	}
	if mode != store.IMSModeHost {
		t.Fatal("unacknowledged pending PDN allowed mode switch")
	}
	if err := f.Close(ctx); !errors.Is(err, hostims.ErrBusy) || lifetime.Err() != nil {
		t.Fatal("pending owner released MM connection", err)
	}
	// A different logical line with no resources can stop independently.
	if err := manager.Ensure(ctx, "other-line"); err != nil {
		t.Fatal(err)
	}
	if err := manager.StopDevice(ctx, "other-line"); err != nil {
		t.Fatal("pending line blocked another line", err)
	}
	pending.fail.Store(false)
	if err := manager.StopDevice(ctx, "pending-line"); err != nil {
		t.Fatal("same pending owner could not retry cleanup", err)
	}
	f.mu.Lock()
	remaining := len(f.held)
	f.mu.Unlock()
	if remaining != 0 || pending.closes.Load() < 2 {
		t.Fatal("pending cleanup owner was abandoned")
	}
}

func TestHostIMSFactoryConstructionAndRuntimeFactoryDoNotOpenSystemBus(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "line", Name: "Line"}); err != nil {
		t.Fatal(err)
	}
	opts := hostIMSFactoryOptions{Store: database, Devices: hostIMSInertInventory{}, IMSConfig: func(context.Context, store.Device) (ims.Config, error) { return ims.Config{}, nil }, Authorize: func(context.Context, hostims.Selection) error { return hostims.ErrDenied }}
	factory, err := buildHostIMSFactory(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	f := factory.(*linuxHostIMSFactory)
	if _, err := factory.Factory(context.Background(), "line"); err != nil {
		t.Fatal(err)
	}
	if f.connection != nil || f.pool != nil || f.initializing || len(f.held) != 0 {
		t.Fatal("inert registration initialized a transport")
	}
	if _, err := f.prepare(context.Background(), "line"); !errors.Is(err, hostims.ErrDenied) {
		t.Fatal("non-Host config reached preparation", err)
	}
	if f.connection != nil || f.pool != nil || f.initializing {
		t.Fatal("non-Host request touched MM")
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Factory(context.Background(), "line"); err == nil {
		t.Fatal("closed factory accepted new line")
	}
}

func TestHostIMSFactoryShutdownRetainsPendingPrepareAndAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &linuxHostIMSFactory{lifetime: ctx, cancel: cancel, preparing: 1, held: map[*factoryHostIMSAccess]struct{}{}}
	if err := f.Close(context.Background()); !errors.Is(err, hostims.ErrBusy) || ctx.Err() != nil {
		t.Fatal("shutdown released a pending worker transport", err)
	}
	f.mu.Lock()
	f.preparing = 0
	access := &factoryHostIMSAccess{}
	f.held[access] = struct{}{}
	f.mu.Unlock()
	if err := f.Close(context.Background()); !errors.Is(err, hostims.ErrBusy) || ctx.Err() != nil {
		t.Fatal("shutdown released retained cleanup", err)
	}
	f.mu.Lock()
	delete(f.held, access)
	f.mu.Unlock()
	if err := f.Close(context.Background()); err != nil || ctx.Err() == nil {
		t.Fatal("idle lifetime not retired", err)
	}
}

type hostIMSFailedLease struct {
	fail   bool
	closes int
}

func (l *hostIMSFailedLease) Close(context.Context) error {
	l.closes++
	if l.fail {
		return hostims.ErrUncertain
	}
	return nil
}

func TestHostIMSFactoryFailedPreparationLeaseRetainsCleanupOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &linuxHostIMSFactory{lifetime: ctx, cancel: cancel, held: map[*factoryHostIMSAccess]struct{}{}}
	lease := &hostIMSFailedLease{fail: true}
	owner := &factoryHostIMSAccess{factory: f, lease: lease}
	f.held[owner] = struct{}{}
	if owner.Evidence().Established {
		t.Fatal("cleanup-only lease owner pretended to be ready")
	}
	if err := owner.Close(context.Background()); !errors.Is(err, hostims.ErrUncertain) {
		t.Fatal(err)
	}
	if err := f.Close(context.Background()); !errors.Is(err, hostims.ErrBusy) || ctx.Err() != nil {
		t.Fatal("failed cleanup released MM lifetime", err)
	}
	lease.fail = false
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(context.Background()); err != nil || ctx.Err() == nil {
		t.Fatal("completed cleanup did not allow idle close", err)
	}
	if lease.closes != 2 {
		t.Fatal("cleanup retry did not use original lease")
	}
}
