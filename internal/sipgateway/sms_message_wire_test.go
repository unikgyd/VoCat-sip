package sipgateway

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// These wire-focused tests use real loopback UDP and the actual runtime read
// filter. The Store/Manager binding and committed Unknown cases are separately
// covered by the frozen actual dual-Store retransmission RED regressions.
func smsMessageWireDelivery(t *testing.T, ctx context.Context, r *runtime, phone *net.UDPConn, guards ...func() bool) <-chan messageOutcome {
	t.Helper()
	max := sip.MaxForwardsHeader(70)
	incoming := sip.NewRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", Host: r.identityDomain()})
	incoming.AppendHeader(&max)
	target := deliveryTarget{contact: &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "1001", Host: "unreachable.invalid"}}, source: phone.LocalAddr().String()}
	result := make(chan messageOutcome, 1)
	go func() {
		result <- r.deliverMessage(ctx, incoming, Account{Username: "synthetic-source"}, Account{Username: "1001"}, target, []byte("synthetic private guarded wire body"), guards...)
	}()
	return result
}

func smsMessageWireResult(t *testing.T, ctx context.Context, result <-chan messageOutcome, expected int) {
	t.Helper()
	select {
	case outcome := <-result:
		if outcome.code != expected {
			t.Fatalf("guarded MT wire outcome=%d, expected=%d", outcome.code, expected)
		}
	case <-ctx.Done():
		t.Fatal("guarded MT wire did not finish:", ctx.Err())
	}
}

func smsMessageWireReleased(t *testing.T, r *runtime) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.mtMessageWires) != 0 || len(r.pendingResponses) != 0 {
		t.Fatal("completed MT wire retained a response owner or leaked into sipgo transactions")
	}
}

func smsMessageWireWaitFinal(t *testing.T, ctx context.Context, r *runtime) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		r.mu.Lock()
		found := false
		for _, wire := range r.mtMessageWires {
			found = found || wire.final != nil
		}
		r.mu.Unlock()
		if found {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("valid final could not enter the independent MT wire while its guard was blocked")
		}
	}
}

func TestSMSMessageWireFinalDuringGuardWaitWinsWithoutAnotherDatagram(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := testMessageRuntime(t)
	phone := newMessagePeerSocket(t)
	gate := newSMSCASRelease()
	defer gate.close()
	reached := make(chan struct{})
	var checks atomic.Int32
	result := smsMessageWireDelivery(t, ctx, r, phone, func() bool {
		if checks.Add(1) == 2 {
			close(reached)
			select {
			case <-gate.ch:
			case <-ctx.Done():
			}
			return false // A later source veto cannot undo the acknowledged send.
		}
		return true
	})
	request, source, err := readFixedPeerMessage(phone)
	if err != nil {
		t.Fatal(err)
	}
	smsCASWait(t, ctx, reached, 1, "per-retransmission source guard")
	// A flood of provisional responses must not fill a bounded channel and
	// hide the genuine final. The read filter can run while the guard waits.
	for index := 0; index < 48; index++ {
		if err := fixedPeerResponse(phone, request, source, 183); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixedPeerResponse(phone, request, source, 200); err != nil {
		t.Fatal(err)
	}
	smsMessageWireWaitFinal(t, ctx, r)
	gate.close()
	smsMessageWireResult(t, ctx, result, 200)
	smsCaptureWireNoUDP(t, phone)
	smsMessageWireReleased(t, r)
}

func TestSMSMessageWireProvisionalKeepsActualNonInviteRetransmissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := testMessageRuntime(t)
	phone := newMessagePeerSocket(t)
	var checks atomic.Int32
	result := smsMessageWireDelivery(t, ctx, r, phone, func() bool { checks.Add(1); return true })
	first, source, err := readFixedPeerMessage(phone)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixedPeerResponse(phone, first, source, 100); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		repeated, remote, err := readFixedPeerMessage(phone)
		if err != nil || repeated == nil || repeated.String() != first.String() || remote.String() != source.String() {
			t.Fatal("non-INVITE provisional response disabled or changed the actual guarded retransmission:", err)
		}
		if index == 0 {
			if err := fixedPeerResponse(phone, repeated, remote, 183); err != nil {
				t.Fatal(err)
			}
		} else if err := fixedPeerResponse(phone, repeated, remote, 200); err != nil {
			t.Fatal(err)
		}
	}
	smsMessageWireResult(t, ctx, result, 200)
	if checks.Load() != 3 {
		t.Fatal("each actual UDP datagram did not execute its own guard")
	}
	smsMessageWireReleased(t, r)
}

func TestSMSMessageWireOverallDeadlineStopsAndReleasesRetransmissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivery, stop := context.WithTimeout(ctx, 1400*time.Millisecond)
	defer stop()
	r := testMessageRuntime(t)
	phone := newMessagePeerSocket(t)
	var checks atomic.Int32
	result := smsMessageWireDelivery(t, delivery, r, phone, func() bool { checks.Add(1); return true })
	first, _, err := readFixedPeerMessage(phone)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := readFixedPeerMessage(phone)
	if err != nil || first.String() != second.String() {
		t.Fatal("the deadline fixture did not exercise a real permitted retransmission:", err)
	}
	smsMessageWireResult(t, ctx, result, 504)
	if delivery.Err() == nil || checks.Load() != 2 {
		t.Fatal("the guarded wire changed the original total deadline or retried after it")
	}
	smsMessageWireReleased(t, r)
	smsWireRetransmitNoUDP(t, phone)
}

func TestSMSMessageWireRejectsForeignOrMismatchedFinalIdentity(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*sip.Response)
		foreign bool
	}{
		{name: "foreign-source", foreign: true},
		{name: "branch", mutate: func(response *sip.Response) { response.Via().Params.Add("branch", "z9hG4bK-foreign") }},
		{name: "call-id", mutate: func(response *sip.Response) {
			// Pinned sipgo treats Call-ID as immutable: headerClone returns
			// the original pointer. Replace it, never mutate the request's ID.
			response.RemoveHeader("Call-ID")
			foreignID := sip.CallIDHeader("foreign-call")
			response.AppendHeader(&foreignID)
		}},
		{name: "sequence", mutate: func(response *sip.Response) { response.CSeq().SeqNo++ }},
		{name: "method", mutate: func(response *sip.Response) { response.CSeq().MethodName = sip.OPTIONS }},
		{name: "from-tag", mutate: func(response *sip.Response) { response.From().Params.Add("tag", "foreign-tag") }},
		{name: "from-address", mutate: func(response *sip.Response) { response.From().Address.User = "foreign-sender" }},
		{name: "to-address", mutate: func(response *sip.Response) { response.To().Address.User = "foreign-recipient" }},
		{name: "duplicate-from", mutate: func(response *sip.Response) { response.AppendHeader(sip.HeaderClone(response.From())) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r := testMessageRuntime(t)
			phone, foreign := newMessagePeerSocket(t), newMessagePeerSocket(t)
			result := smsMessageWireDelivery(t, ctx, r, phone, func() bool { return true })
			request, source, err := readFixedPeerMessage(phone)
			if err != nil {
				t.Fatal(err)
			}
			response := sip.NewResponseFromRequest(request, 200, "Synthetic identity test", nil)
			if test.mutate != nil {
				test.mutate(response)
			}
			connection := phone
			if test.foreign {
				connection = foreign
			}
			if _, err := connection.WriteToUDP([]byte(response.String()), source); err != nil {
				t.Fatal(err)
			}
			select {
			case outcome := <-result:
				t.Fatal("an invalid MT final identity completed the wire:", outcome.code)
			case <-time.After(75 * time.Millisecond):
			}
			if err := fixedPeerResponse(phone, request, source, 200); err != nil {
				t.Fatal(err)
			}
			smsMessageWireResult(t, ctx, result, 200)
			smsMessageWireReleased(t, r)
		})
	}
}
