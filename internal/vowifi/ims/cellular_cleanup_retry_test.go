package ims

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func newCleanupRetryMedia(t *testing.T) *rtpMedia {
	t.Helper()
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = media.Close() })
	return media
}

func awaitCleanupRetryWorker(t *testing.T, media *rtpMedia) {
	t.Helper()
	media.cleanupMu.Lock()
	done := media.cleanupDone
	media.cleanupMu.Unlock()
	if done == nil {
		t.Fatal("cleanup worker was not started")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded cleanup worker did not finish")
	}
}

func TestCellularEndedMediaRetriesOriginalRouteWithoutAnotherRequest(t *testing.T) {
	media := newCleanupRetryMedia(t)
	route := &rtpTestRoute{err: errors.New("original owner temporarily unavailable"), entered: make(chan struct{}, 1)}
	media.mu.Lock()
	media.route = route
	media.mu.Unlock()
	_ = media.Close() // A fast first rejection may already be visible here.
	waitRTPEvent(t, route.entered)
	route.mu.Lock()
	route.err = nil
	route.mu.Unlock()
	// No Calls, Drain, second Close, new bearer or new call drives the retry.
	awaitCleanupRetryWorker(t, media)
	route.mu.Lock()
	calls := route.calls
	route.mu.Unlock()
	if calls != 2 || media.hasCellularRouteCleanup() {
		t.Fatalf("original route did not recover automatically: attempts=%d", calls)
	}
}

func TestCellularAutomaticCleanupExhaustionRetainsOriginalCapability(t *testing.T) {
	media := newCleanupRetryMedia(t)
	route := &rtpTestRoute{err: errors.New("unconfirmed original cleanup")}
	media.mu.Lock()
	media.route = route
	media.mu.Unlock()
	_ = media.Close()
	awaitCleanupRetryWorker(t, media)
	route.mu.Lock()
	calls := route.calls
	route.mu.Unlock()
	if calls != cellularRouteCleanupPasses || !media.hasCellularRouteCleanup() || !errors.Is(media.cellularRouteCleanupError(), ErrCellularAccess) {
		t.Fatalf("failed cleanup was lost or unbounded: attempts=%d", calls)
	}
	// A later explicit drain must still hold and release that exact capability.
	route.mu.Lock()
	route.err = nil
	route.mu.Unlock()
	if err := media.DrainCellularRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	route.mu.Lock()
	calls = route.calls
	route.mu.Unlock()
	if calls != cellularRouteCleanupPasses+1 || media.hasCellularRouteCleanup() {
		t.Fatal("explicit drain could not release the retained original route")
	}
}

func TestCellularCleanupRetryDoesNotStrandLateRetiredRoute(t *testing.T) {
	media := newCleanupRetryMedia(t)
	first := &rtpTestRoute{err: errors.New("temporary original rejection"), entered: make(chan struct{}, 1)}
	second := &rtpTestRoute{}
	media.retireCellularRoute(first)
	waitRTPEvent(t, first.entered)
	first.mu.Lock()
	first.err = nil
	first.mu.Unlock()
	media.retireCellularRoute(second)
	awaitCleanupRetryWorker(t, media)
	first.mu.Lock()
	firstCalls := first.calls
	first.mu.Unlock()
	second.mu.Lock()
	secondCalls := second.calls
	second.mu.Unlock()
	if firstCalls != 2 || secondCalls != 1 || media.hasCellularRouteCleanup() {
		t.Fatalf("late original route stranded: attempts=%d/%d", firstCalls, secondCalls)
	}
}
