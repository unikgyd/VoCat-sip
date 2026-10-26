package sipgateway

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func localFailureCall(t *testing.T, r *runtime) *sipCall {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		t.Fatal("expected one owned synthetic SIP call")
	}
	for _, call := range r.calls {
		return call
	}
	panic("unreachable")
}
func localFailureBranch(t *testing.T, call *sipCall, source string) *callBranch {
	t.Helper()
	for _, branch := range call.branches {
		if branch.entry.source == source {
			return branch
		}
	}
	t.Fatal("expected local branch source missing")
	return nil
}

func TestLocalBranchFailedFinalCannotAcquireAnotherForkAnswerOwner(t *testing.T) {
	r := testCallRuntime(t)
	failed := registerMessagePeer(t, r, "1002")
	winner := registerMessagePeer(t, r, "1002")
	caller, _, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	failedInvite := readCallRequest(t, failed, sip.INVITE)
	winnerInvite := readCallRequest(t, winner, sip.INVITE)
	media := testMediaSocket(t)
	writeCall(t, winner, calleeResponse(t, winner, winnerInvite, media, 180))
	call := localFailureCall(t, r)
	failedBranch := localFailureBranch(t, call, failed.conn.LocalAddr().String())
	writeCall(t, failed, calleeResponse(t, failed, failedInvite, media, 486))
	readCallRequest(t, failed, sip.ACK)
	writeCall(t, failed, calleeResponse(t, failed, failedInvite, media, 200))
	readCallRequest(t, failed, sip.ACK)
	// The actor installs b.response before transmitting a successful ACK.
	// Thus ACK receipt is a deterministic barrier: a failed branch must not
	// have configured its answer/leg even if the parent event loop is delayed.
	failedBranch.mu.Lock()
	installed := failedBranch.response
	failedBranch.mu.Unlock()
	if installed != nil || failedBranch.selected.Load() {
		t.Fatal("failed local branch reacquired answer authority after its final failure")
	}
	bye := readCallRequest(t, failed, sip.BYE)
	replyCallOK(t, failed, bye)
	if current := r.status().Calls; len(current) != 1 || current[0].Phase != "ringing" {
		t.Fatal("failed local branch ended or answered the independent fork")
	}

	answer := calleeResponse(t, winner, winnerInvite, media, 200)
	writeCall(t, winner, answer)
	readCallRequest(t, winner, sip.ACK)
	accepted := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, accepted, sip.ACK, true, 2))
	waitCarrier(t, func() bool { current := r.status().Calls; return len(current) == 1 && current[0].Phase == "active" })
	call.mu.Lock()
	won := call.winner != nil && call.winner.entry.source == winner.conn.LocalAddr().String()
	call.mu.Unlock()
	if !won {
		t.Fatal("independent local fork did not acquire sole answer owner")
	}
	writeCall(t, caller, dialogRequest(t, invite, accepted, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	replyCallOK(t, winner, readCallRequest(t, winner, sip.BYE))
	waitCalls(t, r, 0)
}

func TestLocalBranchContradictoryFinalAfterAnswerCannotRetireActiveMedia(t *testing.T) {
	r := testCallRuntime(t)
	r.mu.Lock()
	r.callLateTimeout = 60 * time.Millisecond
	r.mu.Unlock()
	c := connectCall(t, r, true)
	waitCarrier(t, func() bool { current := r.status().Calls; return len(current) == 1 && current[0].Phase == "active" })
	call := localFailureCall(t, r)
	branch := localFailureBranch(t, call, c.callee.conn.LocalAddr().String())
	branch.mu.Lock()
	media := branch.media
	branch.mu.Unlock()
	if media == nil {
		t.Fatal("active branch media missing")
	}
	writeCall(t, c.callee, calleeResponse(t, c.callee, c.outgoing, c.calleeMedia, 486))
	if ack := readCallRequest(t, c.callee, sip.ACK); ack.CSeq().SeqNo != c.outgoing.CSeq().SeqNo {
		t.Fatal("contradictory final was not transaction-ACKed")
	}

	// Wait past the bounded failure-drain period using the media completion
	// signal, not a polling sleep. The contradictory response must not arm it.
	deadline := time.NewTimer(2 * 60 * time.Millisecond)
	defer deadline.Stop()
	select {
	case <-media.done:
		t.Fatal("contradictory final response retired active media")
	case <-deadline.C:
	}
	if current := r.status().Calls; len(current) != 1 || current[0].Phase != "active" {
		t.Fatal("contradictory final ended selected local call")
	}
	callerDescription, err := parseCallSDP(c.answer.Body())
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x7f}, 160)
	packet := append([]byte{0x80, 8, 0, 1, 0, 0, 0, 160, 1, 2, 3, 4}, payload...)
	if _, err = c.callerMedia.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: callerDescription.port}); err != nil {
		t.Fatal(err)
	}
	c.calleeMedia.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := c.calleeMedia.ReadFromUDP(buffer)
	if err != nil || n != 172 || buffer[1]&127 != 0 {
		t.Fatal("selected local media stopped forwarding after contradictory final", err)
	}
	writeCall(t, c.caller, dialogRequest(t, c.incoming, c.answer, sip.BYE, true, 3))
	readCallFinal(t, c.caller, 200)
	replyCallOK(t, c.callee, readCallRequest(t, c.callee, sip.BYE))
	waitCalls(t, r, 0)
}
