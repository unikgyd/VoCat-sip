package ims

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestCellularDialogSDPIdentitySequenceAndRetransmission(t *testing.T) {
	for _, method := range []string{"UPDATE", "INVITE"} {
		t.Run(method, func(t *testing.T) {
			var mu sync.Mutex
			acquisitions := 0
			entered, release := make(chan struct{}), make(chan struct{})
			network := &rtpTestNetwork{acquire: func(ctx context.Context, _, _ netip.Addr) (vowifi.CellularMediaRoute, error) {
				mu.Lock()
				acquisitions++
				count := acquisitions
				mu.Unlock()
				if count == 2 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return &rtpTestRoute{}, nil
			}}
			s, _ := cellularRTPTestSession(t, network)
			invite := incomingTestInvite(t, "dialog-"+method)
			invite.Body = []byte(callTestSDP)
			s.handleCallRequest(invite, func([]byte) error { return nil })
			if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
				t.Fatal(err)
			}
			s.callMu.Lock()
			call := s.calls[invite.value("Call-ID")]
			s.callMu.Unlock()
			responses := make(chan string, 32)
			respond := func(b []byte) error { responses <- string(b); return nil }
			newer := cellularDialogTestRequest(t, call, method, 11)
			newer.Body = rtpTestSDP("127.0.0.1", 19011)
			// The two entry invocations model the independent protected receive paths.
			s.handleCallRequest(newer, respond)
			waitRTPEvent(t, entered)
			s.handleCallRequest(newer, respond) // pending retransmission must not enqueue another acquire
			older := cellularDialogTestRequest(t, call, method, 10)
			older.Body = rtpTestSDP("127.0.0.1", 19010)
			s.handleCallRequest(older, respond)
			if response := <-responses; !strings.HasPrefix(response, "SIP/2.0 500 ") {
				t.Fatal("lower CSeq admitted", response)
			}
			wrongTag := cellularDialogTestRequest(t, call, method, 12)
			wrongTag.Headers["from"] = []string{strings.ReplaceAll(call.to, "tag=remote", "tag=wrong")}
			s.handleCallRequest(wrongTag, respond)
			if response := <-responses; !strings.HasPrefix(response, "SIP/2.0 481 ") {
				t.Fatal("wrong dialog tag admitted", response)
			}
			wrongMethod := cellularDialogTestRequest(t, call, method, 12)
			wrongMethod.Headers["cseq"] = []string{"12 BYE"}
			s.handleCallRequest(wrongMethod, respond)
			if response := <-responses; !strings.HasPrefix(response, "SIP/2.0 500 ") {
				t.Fatal("mismatched CSeq method admitted", response)
			}
			latest := cellularDialogTestRequest(t, call, method, 12)
			latest.Body = rtpTestSDP("127.0.0.1", 19012)
			s.handleCallRequest(latest, respond)
			// Same CSeq under a different Via transaction must not overwrite it.
			conflict := cellularDialogTestRequest(t, call, method, 12)
			conflict.Headers["via"] = []string{"SIP/2.0/UDP 127.0.0.1:15061;branch=z9hG4bK-other"}
			s.handleCallRequest(conflict, respond)
			if response := <-responses; !strings.HasPrefix(response, "SIP/2.0 500 ") {
				t.Fatal("same CSeq different transaction admitted", response)
			}
			mu.Lock()
			before := acquisitions
			mu.Unlock()
			if before != 2 {
				t.Fatal("duplicate/old/forged SDP acquired media", before)
			}
			close(release)
			waitCallTest(t, func() bool {
				call.media.mu.RLock()
				defer call.media.mu.RUnlock()
				return call.media.remote != nil && call.media.remote.Port == 19012
			})
			waitCallTest(t, func() bool { return len(responses) >= 3 })
			first, duplicate, last := <-responses, <-responses, <-responses
			if first != duplicate || !strings.HasPrefix(last, "SIP/2.0 200 ") {
				t.Fatal("pending transaction did not reuse response")
			}
			s.handleCallRequest(newer, respond) // completed older transaction replays its cached response
			if replay := <-responses; replay != first {
				t.Fatal("completed retransmission changed response")
			}
			s.handleCallRequest(latest, respond)
			if replay := <-responses; replay != last {
				t.Fatal("latest retransmission changed response")
			}
			mu.Lock()
			after := acquisitions
			mu.Unlock()
			if after != 3 {
				t.Fatal("retransmission reacquired media", after)
			}
			call.media.mu.RLock()
			port := call.media.remote.Port
			call.media.mu.RUnlock()
			if port != 19012 {
				t.Fatal("older retransmission rolled media back", port)
			}
			s.finishCall(call.callID, "ended", 0, "")
			if err := call.media.DrainCellularRoutes(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCellularDialogOfferCacheAndPendingRepliesAreBounded(t *testing.T) {
	network := &rtpTestNetwork{}
	s, _ := cellularRTPTestSession(t, network)
	invite := incomingTestInvite(t, "bounded-dialog")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
		t.Fatal(err)
	}
	call := s.calls[invite.value("Call-ID")]
	for sequence := uint32(2); sequence < 24; sequence++ {
		request := cellularDialogTestRequest(t, call, "UPDATE", sequence)
		done := make(chan struct{}, 1)
		s.handleCallRequest(request, func([]byte) error { done <- struct{}{}; return nil })
		waitRTPEvent(t, done)
	}
	s.callMu.Lock()
	size := len(call.dialogOffers)
	s.callMu.Unlock()
	if size != maxDialogOfferTransactions {
		t.Fatal("dialog transaction history unbounded", size)
	}
	request := cellularDialogTestRequest(t, call, "UPDATE", 24)
	s.callMu.Lock()
	transaction, _, status := s.admitDialogOfferLocked(call, request, func([]byte) error { return nil })
	for index := 0; index < 100; index++ {
		s.admitDialogOfferLocked(call, request, func([]byte) error { return nil })
	}
	responders := len(transaction.responders)
	s.callMu.Unlock()
	if status != 0 || responders != maxDialogOfferResponders {
		t.Fatal(fmt.Sprintf("pending reply references unbounded: %d %d", status, responders))
	}
	s.completeDialogOffer(call, transaction, request, 488, nil)
	s.finishCall(call.callID, "ended", 0, "")
}

func TestCellularReinviteCANCELPreservesActiveCallAndOldRoute(t *testing.T) {
	entered := make(chan struct{})
	oldRoute := &rtpTestRoute{}
	lateRoute := &rtpTestRoute{entered: make(chan struct{}, 1)}
	var mu sync.Mutex
	acquisitions := 0
	network := &rtpTestNetwork{acquire: func(ctx context.Context, _, _ netip.Addr) (vowifi.CellularMediaRoute, error) {
		mu.Lock()
		acquisitions++
		count := acquisitions
		mu.Unlock()
		if count == 1 {
			return oldRoute, nil
		}
		if count == 2 {
			close(entered)
			<-ctx.Done()
			return lateRoute, nil
		}
		return &rtpTestRoute{}, nil
	}}
	s, _ := cellularRTPTestSession(t, network)
	invite := incomingTestInvite(t, "cancel-reinvite")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
		t.Fatal(err)
	}
	call := s.calls[invite.value("Call-ID")]
	reinvite := cellularDialogTestRequest(t, call, "INVITE", 2)
	reinvite.Body = rtpTestSDP("127.0.0.1", 19002)
	replies := make(chan string, 8)
	s.handleCallRequest(reinvite, func(b []byte) error { replies <- string(b); return nil })
	waitRTPEvent(t, entered)
	cancel := incomingTestRequest(t, reinvite, "CANCEL", 2, false)
	cancelReplies := make(chan string, 4)
	returned := make(chan struct{})
	go func() {
		s.handleCallRequest(cancel, func(b []byte) error { cancelReplies <- string(b); return nil })
		close(returned)
	}()
	waitRTPEvent(t, returned)
	if response := <-cancelReplies; !strings.HasPrefix(response, "SIP/2.0 200 ") {
		t.Fatal("re-INVITE CANCEL not acknowledged", response)
	}
	if response := <-replies; !strings.HasPrefix(response, "SIP/2.0 487 ") || !strings.Contains(response, "CSeq: 2 INVITE") {
		t.Fatal("wrong re-INVITE final response", response)
	}
	waitRTPEvent(t, lateRoute.entered)
	if err := call.media.DrainCellularRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	call.media.mu.RLock()
	port := call.media.remote.Port
	call.media.mu.RUnlock()
	oldRoute.mu.Lock()
	oldClosed := oldRoute.calls
	oldRoute.mu.Unlock()
	if port != 19000 || oldClosed != 0 || s.Calls()[0].State != "active" || !s.Calls()[0].MediaReady {
		t.Fatal("CANCEL changed the original active media", port, oldClosed, s.Calls())
	}
	s.handleCallRequest(reinvite, func(b []byte) error { replies <- string(b); return nil })
	if replay := <-replies; !strings.HasPrefix(replay, "SIP/2.0 487 ") {
		t.Fatal("canceled transaction was retried", replay)
	}
	update := cellularDialogTestRequest(t, call, "UPDATE", 3)
	update.Body = rtpTestSDP("127.0.0.1", 19003)
	s.handleCallRequest(update, func(b []byte) error { replies <- string(b); return nil })
	waitCallTest(t, func() bool { return len(replies) > 0 })
	if response := <-replies; !strings.HasPrefix(response, "SIP/2.0 200 ") {
		t.Fatal("CANCEL ended the dialog", response)
	}
	mu.Lock()
	count := acquisitions
	mu.Unlock()
	if count != 3 {
		t.Fatal("CANCEL/retransmission acquired unexpected routes", count)
	}
	s.finishCall(call.callID, "ended", 0, "")
}

func TestCellularReinviteCommittedMediaCannotBeCancelled(t *testing.T) {
	network := &rtpTestNetwork{}
	s, _ := cellularRTPTestSession(t, network)
	invite := incomingTestInvite(t, "committed-reinvite")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
		t.Fatal(err)
	}
	call := s.calls[invite.value("Call-ID")]
	reinvite := cellularDialogTestRequest(t, call, "INVITE", 2)
	reinvite.Body = rtpTestSDP("127.0.0.1", 19002)
	writing, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	replies := make(chan string, 4)
	s.handleCallRequest(reinvite, func(b []byte) error { replies <- string(b); close(writing); <-release; return nil })
	waitRTPEvent(t, writing) // route/target and final 200 are committed; response write is deliberately slow
	cancel := incomingTestRequest(t, reinvite, "CANCEL", 2, false)
	done := make(chan struct{})
	var cancelResponse string
	go func() {
		s.handleCallRequest(cancel, func(b []byte) error { cancelResponse = string(b); return nil })
		close(done)
	}()
	waitRTPEvent(t, done)
	if !strings.HasPrefix(cancelResponse, "SIP/2.0 200 ") {
		t.Fatal("post-commit CANCEL not acknowledged", cancelResponse)
	}
	if response := <-replies; !strings.HasPrefix(response, "SIP/2.0 200 ") {
		t.Fatal("committed answer was replaced", response)
	}
	select {
	case response := <-replies:
		t.Fatal("CANCEL sent a second final response", response)
	case <-time.After(20 * time.Millisecond):
	}
	call.media.mu.RLock()
	port := call.media.remote.Port
	call.media.mu.RUnlock()
	if port != 19002 || s.Calls()[0].State != "active" {
		t.Fatal("post-commit CANCEL rolled media back", port, s.Calls())
	}
	s.finishCall(call.callID, "ended", 0, "")
}

func TestCellularReinviteCANCELDoesNotWaitForRTPWriter(t *testing.T) {
	acquired := make(chan struct{})
	staged := &rtpTestRoute{entered: make(chan struct{}, 1)}
	old := &rtpTestRoute{}
	count := 0
	network := &rtpTestNetwork{acquire: func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
		count++
		if count == 1 {
			return old, nil
		}
		close(acquired)
		return staged, nil
	}}
	s, _ := cellularRTPTestSession(t, network)
	invite := incomingTestInvite(t, "writer-reinvite")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
		t.Fatal(err)
	}
	call := s.calls[invite.value("Call-ID")]
	call.media.writeMu.Lock()
	defer call.media.writeMu.Unlock()
	reinvite := cellularDialogTestRequest(t, call, "INVITE", 2)
	reinvite.Body = rtpTestSDP("127.0.0.1", 19002)
	replies := make(chan string, 4)
	s.handleCallRequest(reinvite, func(b []byte) error { replies <- string(b); return nil })
	waitRTPEvent(t, acquired)
	cancel := incomingTestRequest(t, reinvite, "CANCEL", 2, false)
	done := make(chan struct{})
	var response string
	go func() {
		s.handleCallRequest(cancel, func(b []byte) error { response = string(b); return nil })
		close(done)
	}()
	waitRTPEvent(t, done) // writeMu remains held throughout; this must not depend on the RTP sender.
	if !strings.HasPrefix(response, "SIP/2.0 200 ") {
		t.Fatal("CANCEL blocked behind writer", response)
	}
	if response := <-replies; !strings.HasPrefix(response, "SIP/2.0 487 ") {
		t.Fatal("staged media was committed", response)
	}
	waitRTPEvent(t, staged.entered) // canceled waiter retires the lease without acquiring writeMu
	call.media.mu.RLock()
	port := call.media.remote.Port
	call.media.mu.RUnlock()
	old.mu.Lock()
	oldClosed := old.calls
	old.mu.Unlock()
	if port != 19000 || oldClosed != 0 || s.Calls()[0].State != "active" {
		t.Fatal("writer-wait cancellation lost old media", port, oldClosed)
	}
	s.finishCall(call.callID, "ended", 0, "")
}

func TestCellularDialogConcurrentReceiversKeepRouteOrder(t *testing.T) {
	var mu sync.Mutex
	var destinations []byte
	network := &rtpTestNetwork{acquire: func(_ context.Context, _, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
		mu.Lock()
		destinations = append(destinations, remote.As4()[3])
		mu.Unlock()
		return &rtpTestRoute{}, nil
	}}
	s, _ := cellularRTPTestSession(t, network)
	invite := incomingTestInvite(t, "concurrent-dialog")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
		t.Fatal(err)
	}
	call := s.calls[invite.value("Call-ID")]
	start := make(chan struct{})
	responses := make(chan struct{}, 32)
	var senders sync.WaitGroup
	for sequence := uint32(2); sequence < 34; sequence++ {
		request := cellularDialogTestRequest(t, call, "UPDATE", sequence)
		request.Body = rtpTestSDP(fmt.Sprintf("127.0.0.%d", sequence), 19000+int(sequence))
		senders.Add(1)
		go func() {
			defer senders.Done()
			<-start
			s.handleCallRequest(request, func([]byte) error { responses <- struct{}{}; return nil })
		}()
	}
	close(start)
	senders.Wait()
	for index := 0; index < 32; index++ {
		waitRTPEvent(t, responses)
	}
	mu.Lock()
	observed := append([]byte(nil), destinations...)
	mu.Unlock()
	for index := 1; index < len(observed); index++ {
		if observed[index] <= observed[index-1] {
			t.Fatal("concurrent receive paths reordered SDP route commits", observed)
		}
	}
	s.finishCall(call.callID, "ended", 0, "")
}
