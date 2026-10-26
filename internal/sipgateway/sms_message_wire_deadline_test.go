package sipgateway

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Drive the production response read-filter with bytes received on a REAL UDP
// socket. start() installs that filter internally and has no after-route hook;
// this small receive driver supplies an explicit processed barrier without
// fabricating a final or depending on wire.final becoming non-nil in GREEN.
func smsMessageDeadlineRuntime(t *testing.T, ctx context.Context) (*runtime, <-chan error) {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{config: testConfig(t, "SHA-256"), conn: connection, ctx: ctx,
		pendingResponses: map[string]responseExpectation{}, mtMessageWires: map[string]*mtMessageWire{}}
	processed, finished := make(chan error, 4), make(chan struct{})
	filter := r.responseFilter()
	go func() {
		defer close(finished)
		buffer := make([]byte, 16*1024)
		for {
			n, source, err := connection.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			forwarded, err := filter(sip.TransportReadProps{RemoteAddr: source}, buffer[:n])
			if err == nil && len(forwarded) != 0 {
				err = errors.New("guarded MT response escaped its production read-filter")
			}
			select {
			case processed <- err:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = connection.Close()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("real deadline response reader did not stop")
		}
	})
	return r, processed
}

func smsMessageDeadlineProcessed(t *testing.T, ctx context.Context, processed <-chan error) {
	t.Helper()
	select {
	case err := <-processed:
		if err != nil {
			t.Fatal("real UDP final did not traverse the production response filter:", err)
		}
	case <-ctx.Done():
		t.Fatal("real UDP final was not processed before releasing the guard:", ctx.Err())
	}
}

func smsMessageDeadlineBlockedGuard(ctx context.Context, gate *smsCASRelease, reached chan<- struct{}) func() bool {
	var checks atomic.Int32
	return func() bool {
		if checks.Add(1) != 2 {
			return true
		}
		close(reached)
		// Deliberately model a slow DB operation's cancellation/ROLLBACK
		// cleanup. Only the parent test budget, not the expired wire budget,
		// releases this guard early. No guard result is synthesized as valid.
		select {
		case <-gate.ch:
		case <-ctx.Done():
		}
		return false
	}
}

func TestSMSMessageWireLateFinalCannotReviveExpiredAttempt(t *testing.T) {
	for _, cutoff := range []string{"deadline", "cancel", "stop"} {
		t.Run(cutoff, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			r, processed := smsMessageDeadlineRuntime(t, ctx)
			peer := newMessagePeerSocket(t)
			budget := 3 * time.Second
			if cutoff == "deadline" {
				budget = 900 * time.Millisecond
			}
			delivery, stop := context.WithTimeout(ctx, budget)
			defer stop()
			gate := newSMSCASRelease()
			defer gate.close()
			reached := make(chan struct{})
			result := smsMessageWireDelivery(t, delivery, r, peer, smsMessageDeadlineBlockedGuard(ctx, gate, reached))
			request, source, err := readFixedPeerMessage(peer)
			if err != nil {
				t.Fatal(err)
			}
			smsCASWait(t, ctx, reached, 1, "second actual-datagram DB guard")
			switch cutoff {
			case "deadline":
				select {
				case <-delivery.Done():
				case <-ctx.Done():
					t.Fatal("original delivery deadline did not expire")
				}
			case "cancel":
				stop()
				if delivery.Err() == nil {
					t.Fatal("delivery cancellation was not visible before the late response")
				}
			case "stop":
				// Same atomic fence set first by runtime.close, while the live
				// receive socket and delivery context intentionally remain usable.
				r.closed.Store(true)
				if delivery.Err() != nil {
					t.Fatal("stop case did not isolate the closed fence")
				}
			}
			if err := fixedPeerResponse(peer, request, source, 200); err != nil {
				t.Fatal(err)
			}
			// This is after the real packet's parser + identity + route path,
			// not merely after WriteTo and not a sleep-based scheduling guess.
			smsMessageDeadlineProcessed(t, ctx, processed)
			gate.close()
			smsMessageWireResult(t, ctx, result, 504)
			smsWireRetransmitNoUDP(t, peer)
			smsMessageWireReleased(t, r)
		})
	}
}

func TestSMSMessageWireTimelyFinalSurvivesLaterDeadlineAndVeto(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, processed := smsMessageDeadlineRuntime(t, ctx)
	peer := newMessagePeerSocket(t)
	delivery, stop := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer stop()
	gate := newSMSCASRelease()
	defer gate.close()
	reached := make(chan struct{})
	result := smsMessageWireDelivery(t, delivery, r, peer, smsMessageDeadlineBlockedGuard(ctx, gate, reached))
	request, source, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	smsCASWait(t, ctx, reached, 1, "second actual-datagram DB guard before timely final")
	if err := fixedPeerResponse(peer, request, source, 200); err != nil {
		t.Fatal(err)
	}
	smsMessageDeadlineProcessed(t, ctx, processed)
	deadline, hasDeadline := delivery.Deadline()
	if delivery.Err() != nil || !hasDeadline || !time.Now().Before(deadline) {
		t.Fatal("positive control's exact final was not actually processed before the original deadline")
	}
	select {
	case <-delivery.Done():
	case <-ctx.Done():
		t.Fatal("positive control's original deadline did not expire")
	}
	gate.close()
	// The already saved, timely final wins even though the later DB guard
	// returns false and the caller's original wire budget has now expired.
	smsMessageWireResult(t, ctx, result, 200)
	smsWireRetransmitNoUDP(t, peer)
	smsMessageWireReleased(t, r)
}
