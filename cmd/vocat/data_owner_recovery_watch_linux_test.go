//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/device"
)

func TestDataOwnerRecoveryWatchOutlivesRequestAndJoinsBeforeCleanup(t *testing.T) {
	request, cancel := context.WithCancel(context.Background())
	cancel() // the successful Set RPC has already completed
	var w dataOwnerRecoveryWatch
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	w.start(time.Millisecond, func(ctx context.Context) {
		if calls.Add(1) != 1 || ctx.Err() != nil || request.Err() == nil {
			t.Error("original owner watch inherited the completed request or overlapped")
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release // simulate the original serial operation still unwinding
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = w.stop(context.Background())
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("watch never observed the original transport independently")
	}
	w.start(time.Millisecond, func(context.Context) { t.Error("duplicate original-owner watch") })
	stop, stopCancel := context.WithCancel(context.Background())
	stopCancel()
	if err := w.stop(stop); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("unfinished original operation falsely acknowledged cleanup", err)
	}
	<-cancelled
	w.start(time.Millisecond, func(context.Context) { t.Error("uncertain stop started a replacement watch") })
	releaseOnce.Do(func() { close(release) })
	if err := w.stop(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatal("original watch did not join exactly once", err)
	}
	if err := w.stop(context.Background()); err != nil {
		t.Fatal("acknowledged stop is not idempotent", err)
	}
}

func TestDataOwnerRecoveryWatchCanRestartOnlyAfterAcknowledgedStop(t *testing.T) {
	var w dataOwnerRecoveryWatch
	t.Cleanup(func() { _ = w.stop(context.Background()) })
	for i := 0; i < 2; i++ {
		seen := make(chan struct{}, 1)
		w.start(time.Millisecond, func(context.Context) {
			select {
			case seen <- struct{}{}:
			default:
			}
		})
		select {
		case <-seen:
		case <-time.After(time.Second):
			t.Fatal("successful later admission did not start its own watch")
		}
		if err := w.stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
