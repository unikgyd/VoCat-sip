package ims

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"testing"
	"time"
	"vocat/internal/vowifi"
)

func TestCellularSessionCloseDrainsRetainedRoutesAfterCallerDeadline(t *testing.T) {
	peer := rtpTestPeer(t)
	route := &rtpTestRoute{}
	network := &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}
	network.acquire = func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) { return route, nil }
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := media.configureRemote(rtpTestSDP("127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	close(done)
	session := &Session{conn: client, refreshCancel: func() {}, refreshDone: done, calls: map[string]*imsCall{"call": {media: media}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	route.mu.Lock()
	calls := route.calls
	route.mu.Unlock()
	if calls != 1 || media.hasCellularRouteCleanup() {
		t.Fatal("expired SIP context suppressed local cleanup")
	}
}

func TestCellularSessionCloseReportsCleanupFailureAndCanRetry(t *testing.T) {
	route := &rtpTestRoute{err: errors.New("private owner state")}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	media.mu.Lock()
	media.route = route
	media.mu.Unlock()
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	close(done)
	session := &Session{conn: client, refreshCancel: func() {}, refreshDone: done, calls: map[string]*imsCall{"call": {media: media}}}
	if err := session.Close(context.Background()); !errors.Is(err, ErrCellularAccess) {
		t.Fatalf("cleanup loss was reported successful: %v", err)
	}
	if !media.hasCellularRouteCleanup() {
		t.Fatal("failed route handle discarded")
	}
	route.mu.Lock()
	route.err = nil
	route.mu.Unlock()
	if err := session.Close(context.Background()); err != nil {
		t.Fatal("repeat Close cannot retry", err)
	}
	if media.hasCellularRouteCleanup() {
		t.Fatal("route not reclaimed on retry")
	}
}

func TestCellularConcurrentCloseCannotReportUnfinishedCleanupAsSuccess(t *testing.T) {
	route := &rtpTestRoute{entered: make(chan struct{}, 1), release: make(chan struct{})}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	media.mu.Lock()
	media.route = route
	media.mu.Unlock()
	client, server := net.Pipe()
	defer server.Close()
	refreshDone := make(chan struct{})
	close(refreshDone)
	session := &Session{conn: client, refreshCancel: func() {}, refreshDone: refreshDone, calls: map[string]*imsCall{"call": {media: media}}}
	first := make(chan error, 1)
	go func() { first <- session.Close(context.Background()) }()
	waitRTPEvent(t, route.entered)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := session.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent Close reported early success: %v", err)
	}
	close(route.release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first Close did not finish")
	}
}
