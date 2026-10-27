//go:build linux

package dataowner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

type fixtureDriver struct {
	candidate         modem.Candidate
	sets              atomic.Int32
	closed            chan struct{}
	closing           chan struct{}
	block             <-chan struct{}
	set               func(context.Context) error
	once, closingOnce sync.Once
}

func (d *fixtureDriver) Set(ctx context.Context, r device.NetworkRequest) (device.NetworkResult, error) {
	d.sets.Add(1)
	if d.set != nil {
		if err := d.set(ctx); err != nil {
			return device.NetworkResult{}, err
		}
	}
	return device.NetworkResult{Enabled: r.Enabled, Backend: "qmi", Interface: d.candidate.NetworkInterface, ControlDevice: d.candidate.QMIControl}, nil
}
func (d *fixtureDriver) Status(context.Context) (device.NetworkStatus, error) {
	return device.NetworkStatus{Connected: true, Backend: "qmi", Interface: d.candidate.NetworkInterface}, nil
}
func (d *fixtureDriver) Stop(ctx context.Context) error { return d.Close(ctx) }
func (d *fixtureDriver) Close(ctx context.Context) error {
	d.closingOnce.Do(func() { close(d.closing) })
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return device.ErrDataCleanupPending
		}
	}
	d.once.Do(func() { close(d.closed) })
	return nil
}
func newFixtureDriver(candidate modem.Candidate) *fixtureDriver {
	return &fixtureDriver{candidate: candidate, closed: make(chan struct{}), closing: make(chan struct{})}
}
func candidateFixture(id string) modem.Candidate {
	return modem.Candidate{ID: id, USBPath: "/sys/devices/fixture/" + id, USBGeneration: "1:7", QMIControl: "/dev/fixture-" + id, NetworkInterface: "fixture0"}
}
func fixtureFiles(t *testing.T, dir string, lock bool) *os.File {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("fixture only"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if lock {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
func fixtureServer(t *testing.T, dir string, factory func(context.Context, Open) (Driver, error), idle func(context.Context, string) error) *Server {
	t.Helper()
	if idle == nil {
		idle = func(context.Context, string) error { return nil }
	}
	s, err := Listen(ServerOptions{SocketPath: filepath.Join(dir, "owner.sock"), InstanceLockPath: filepath.Join(dir, "server.lock"), DatabasePath: filepath.Join(dir, "data.db"), Factory: factory, Idle: idle})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := s.Close(cleanup); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return s
}
func fixtureClient(t *testing.T, dir string, instance *os.File) *Client {
	t.Helper()
	c, err := NewClient(filepath.Join(dir, "owner.sock"), filepath.Join(dir, "data.db"), instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition not acknowledged")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func fixtureSet(c *Client, candidate modem.Candidate) (device.NetworkResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Set(ctx, candidate, "1234567890123456789", "123456789012345", device.NetworkRequest{Enabled: true, Backend: "qmi", APN: "internet", IPVersion: "IPV4V6"})
}

func TestDataOwnerLifecycleAndPhysicalBinding(t *testing.T) {
	dir := t.TempDir()
	instance := fixtureFiles(t, dir, true)
	candidate := candidateFixture("first")
	driver := newFixtureDriver(candidate)
	fixtureServer(t, dir, func(context.Context, Open) (Driver, error) { return driver, nil }, nil)
	client := fixtureClient(t, dir, instance)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureSet(client, candidate); err != nil {
		t.Fatal(err)
	}
	if !client.Owns(candidate) {
		t.Fatal("original owner missing")
	}
	replacement := candidate
	replacement.USBGeneration = "1:8"
	if client.Owns(replacement) {
		t.Fatal("replacement inherited owner")
	}
	if _, err := fixtureSet(client, replacement); !errors.Is(err, device.ErrSIMObservation) {
		t.Fatal("replacement admitted", err)
	}
	if state, err := client.Status(context.Background(), candidate.ID); err != nil || !state.Connected {
		t.Fatal(state, err)
	}
	if err := client.Stop(context.Background(), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if client.Owns(candidate) || driver.sets.Load() != 1 {
		t.Fatal("original was not released")
	}
	if err := client.Stop(context.Background(), candidate.ID); err != nil {
		t.Fatal("idle retry", err)
	}
}

func TestDataOwnerSIGKILLRetainsInstanceFlockUntilCleanupACK(t *testing.T) {
	if dir := os.Getenv("VOCAT_DATA_OWNER_CHILD_DIR"); dir != "" {
		file, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(filepath.Join(dir, "owner.sock"), filepath.Join(dir, "data.db"), file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixtureSet(client, candidateFixture("first")); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
		select {}
	}
	dir := t.TempDir()
	_ = fixtureFiles(t, dir, false)
	driver := newFixtureDriver(candidateFixture("first"))
	gate := make(chan struct{})
	driver.block = gate
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	fixtureServer(t, dir, func(context.Context, Open) (Driver, error) { return driver, nil }, nil)
	child := exec.Command(os.Args[0], "-test.run=^TestDataOwnerSIGKILLRetainsInstanceFlockUntilCleanupACK$")
	child.Env = append(os.Environ(), "VOCAT_DATA_OWNER_CHILD_DIR="+dir)
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); release.Do(func() { close(gate) }) })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(pipe).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatal("child did not acknowledge ownership")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("child readiness timeout")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("expected actual SIGKILL")
	}
	select {
	case <-driver.closing:
	case <-time.After(5 * time.Second):
		t.Fatal("EOF did not initiate original cleanup")
	}
	fd, err := unix.Open(filepath.Join(dir, "server.lock"), unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("new main acquired lock before cleanup ACK")
	}
	release.Do(func() { close(gate) })
	waitFor(t, func() bool { return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil })
	select {
	case <-driver.closed:
	default:
		t.Fatal("lock released without cleanup")
	}
}

func TestDataOwnerCancellationJoinsStartAndIsolatesOtherDevice(t *testing.T) {
	dir := t.TempDir()
	instance := fixtureFiles(t, dir, true)
	started := make(chan struct{})
	first := newFixtureDriver(candidateFixture("first"))
	second := newFixtureDriver(candidateFixture("second"))
	first.set = func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }
	fixtureServer(t, dir, func(_ context.Context, open Open) (Driver, error) {
		if open.Candidate.ID == "first" {
			return first, nil
		}
		return second, nil
	}, nil)
	client := fixtureClient(t, dir, instance)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Set(ctx, first.candidate, "1234567890123456789", "123456789012345", device.NetworkRequest{Enabled: true})
		done <- err
	}()
	<-started
	if _, err := fixtureSet(client, second.candidate); err != nil {
		t.Fatal("another device was blocked", err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled Start acknowledged")
	}
	select {
	case <-first.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled original worker not joined")
	}
	if client.Owns(first.candidate) {
		t.Fatal("dead connection retained policy permission")
	}
	if err := client.Stop(context.Background(), second.candidate.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDataOwnerRejectsForeignDescriptorsAndColdOrphans(t *testing.T) {
	dir := t.TempDir()
	instance := fixtureFiles(t, dir, true)
	var factories atomic.Int32
	fixtureServer(t, dir, func(_ context.Context, open Open) (Driver, error) {
		factories.Add(1)
		return newFixtureDriver(open.Candidate), nil
	}, func(_ context.Context, id string) error {
		if id == "orphan" {
			return device.ErrDataCleanupPending
		}
		return nil
	})
	client := fixtureClient(t, dir, instance)
	if err := client.Stop(context.Background(), "orphan"); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("orphan acknowledged", err)
	}
	wrong, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	foreign, err := NewClient(filepath.Join(dir, "owner.sock"), filepath.Join(dir, "data.db"), wrong)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := foreign.Ping(ctx); err == nil {
		t.Fatal("unlocked open description accepted")
	}
	otherDB := filepath.Join(dir, "other.db")
	if err := os.WriteFile(otherDB, []byte("different store"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign, err = NewClient(filepath.Join(dir, "owner.sock"), otherDB, instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Ping(ctx); err == nil {
		t.Fatal("different store accepted")
	}
	if factories.Load() != 0 {
		t.Fatal("rejected request constructed a hardware driver")
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal("rejection damaged original main flock", err)
	}
}

func TestDataOwnerCloseDuringOpenNeverStartsLateDriver(t *testing.T) {
	dir := t.TempDir()
	instance := fixtureFiles(t, dir, true)
	entered := make(chan struct{})
	release := make(chan struct{})
	driver := newFixtureDriver(candidateFixture("first"))
	fixtureServer(t, dir, func(ctx context.Context, open Open) (Driver, error) {
		close(entered)
		select {
		case <-release:
			return driver, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil)
	client := fixtureClient(t, dir, instance)
	done := make(chan error, 1)
	go func() { _, err := fixtureSet(client, driver.candidate); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_ = client.Close(ctx)
	cancel()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("late open survived close")
	}
	waitFor(t, func() bool {
		select {
		case <-driver.closed:
			return true
		default:
			return false
		}
	})
	if driver.sets.Load() != 0 || client.Owns(driver.candidate) {
		t.Fatal("late driver started after close")
	}
}

func TestDataOwnerServerLossRevokesPolicyPermission(t *testing.T) {
	dir := t.TempDir()
	instance := fixtureFiles(t, dir, true)
	driver := newFixtureDriver(candidateFixture("first"))
	server := fixtureServer(t, dir, func(context.Context, Open) (Driver, error) { return driver, nil }, nil)
	client := fixtureClient(t, dir, instance)
	if _, err := fixtureSet(client, driver.candidate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !client.Owns(driver.candidate) })
	if err := client.Stop(ctx, driver.candidate.ID); err == nil {
		t.Fatal("missing owner service acknowledged cleanup")
	}
}

func TestDataOwnerDatabaseAttestationPreservesSQLiteWAL(t *testing.T) {
	if dir := os.Getenv("VOCAT_DATA_OWNER_SQLITE_CHILD"); dir != "" {
		db, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		before, err := db.AppSetting(context.Background(), "fixture.parent")
		if err != nil || string(before.Value) != `"parent"` {
			t.Fatal("parent WAL transaction missing", err)
		}
		if err := db.UpsertAppSetting(context.Background(), store.AppSetting{Key: "fixture.child", Value: json.RawMessage(`"child"`)}); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	instance := fixtureFiles(t, dir, true)
	// fixtureFiles uses a non-SQLite dummy for protocol-only tests.
	if err := os.Remove(filepath.Join(dir, "data.db")); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertAppSetting(context.Background(), store.AppSetting{Key: "fixture.parent", Value: json.RawMessage(`"parent"`)}); err != nil {
		t.Fatal(err)
	}
	wal := filepath.Join(dir, "data.db-wal")
	original, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	fixtureServer(t, dir, func(_ context.Context, open Open) (Driver, error) { return newFixtureDriver(open.Candidate), nil }, nil)
	client := fixtureClient(t, dir, instance)
	for range 3 {
		if err := client.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestDataOwnerDatabaseAttestationPreservesSQLiteWAL$")
	child.Env = append(os.Environ(), "VOCAT_DATA_OWNER_SQLITE_CHILD="+dir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child WAL transaction: %v: %s", err, output)
	}
	current, err := os.Stat(wal)
	if err != nil || !os.SameFile(original, current) {
		t.Fatal("RPC database attestation released SQLite locks and lost the live WAL", err)
	}
	value, err := db.AppSetting(context.Background(), "fixture.child")
	if err != nil || string(value.Value) != `"child"` {
		t.Fatal("independent transaction was not visible", err)
	}
}
