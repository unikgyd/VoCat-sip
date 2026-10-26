package server

import (
	"context"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

type isolatedCallPollDevices struct {
	fakeDeviceController
	entries    []device.Device
	slow, fast chan struct{}
}

func (d *isolatedCallPollDevices) List() []device.Device { return d.entries }
func (d *isolatedCallPollDevices) Get(id string) (device.Device, error) {
	for _, entry := range d.entries {
		if entry.ID == id {
			return entry, nil
		}
	}
	return device.Device{}, device.ErrNotFound
}
func (d *isolatedCallPollDevices) ExecuteAT(ctx context.Context, id, command string) (modem.Response, error) {
	if command != "AT+CLCC" {
		panic("unexpected hardware operation in synthetic test")
	}
	if id == "a-slow" {
		close(d.slow)
		<-ctx.Done()
		return modem.Response{}, ctx.Err()
	}
	close(d.fast)
	return modem.Response{Final: "OK"}, nil
}

func TestCellularCallPollSlowModemDoesNotDelayAnotherOrHoldModeLock(t *testing.T) {
	s, _ := newPhoneTest(t)
	// Use explicit module mode, independently of the synthetic IMS fixture.
	for _, id := range []string{"a-slow", "b-fast"} {
		if err := s.store.UpsertDevice(context.Background(), store.Device{ID: id, Name: id, IMSMode: store.IMSModeModule}); err != nil {
			t.Fatal(err)
		}
	}
	// Remove the fixture's original line so only the two physical test modems poll.
	if err := s.store.DeleteDevice(context.Background(), "module-a"); err != nil {
		t.Fatal(err)
	}
	d := &isolatedCallPollDevices{entries: []device.Device{{ID: "a-slow", Discovered: true}, {ID: "b-fast", Discovered: true}}, slow: make(chan struct{}), fast: make(chan struct{})}
	s.devices = d
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.pollCellularCalls(ctx) }()
	select {
	case <-d.slow:
	case <-time.After(time.Second):
		t.Fatal("slow modem was not polled")
	}
	select {
	case <-d.fast:
	case <-time.After(time.Second):
		t.Fatal("slow modem blocked the independent modem")
	}
	lock := s.imsModeLock("b-fast")
	locked := make(chan struct{})
	go func() { lock.Lock(); lock.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("the idle modem mode is held by another modem's poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll workers did not stop on cancellation")
	}
}

func TestCellularCallPollCanceledModeLockWaitDoesNotDelayShutdown(t *testing.T) {
	s, _ := newPhoneTest(t)
	lock := s.imsModeLock("mode-transition")
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.collectCellularCalls(ctx, "mode-transition") }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled call poll still waits for the mode transition lock")
	}
}
