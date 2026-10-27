package sipgateway

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func fastDialogProbes(r *runtime) {
	r.mu.Lock()
	r.callProbeInterval, r.callProbeTimeout = 60*time.Millisecond, 220*time.Millisecond
	r.mu.Unlock()
}

func assertOriginalProbe(t *testing.T, request, invite *sip.Request, answer *sip.Response, caller bool) {
	t.Helper()
	from, to := invite.From().Address.String(), answer.To().Address.String()
	localTag, remoteTag := invite.From().Params.GetOr("tag", ""), answer.To().Params.GetOr("tag", "")
	if caller {
		from, to = to, from
		localTag, remoteTag = remoteTag, localTag
	}
	if request.Method != sip.OPTIONS || request.CallID().Value() != invite.CallID().Value() ||
		request.From().Address.String() != from || request.To().Address.String() != to ||
		request.From().Params.GetOr("tag", "") != localTag || request.To().Params.GetOr("tag", "") != remoteTag ||
		len(request.Body()) != 0 || len(request.GetHeaders("Authorization")) != 0 || len(request.GetHeaders("Proxy-Authorization")) != 0 {
		t.Fatal("probe escaped original dialog or carried business/authentication data")
	}
}

// A real surviving UA keeps answering OPTIONS while another leg's transaction
// reaches its deadline. Keep one absolute read budget instead of assuming BYE
// is the next datagram after the first successful probe.
func readDialogByeAnsweringProbes(t *testing.T, peer *wireClient, original *sip.Request) (*sip.Request, uint32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := original.CSeq().SeqNo
	buffer := make([]byte, 16384)
	for {
		peer.conn.SetReadDeadline(deadline)
		n, err := peer.conn.Read(buffer)
		if err != nil {
			t.Fatal("original dialog did not terminate:", err)
		}
		message, err := registrarParser().ParseSIP(buffer[:n])
		if err != nil {
			t.Fatal(err)
		}
		request, ok := message.(*sip.Request)
		if !ok || request.CallID().Value() != original.CallID().Value() || request.From().Value() != original.From().Value() || request.To().Value() != original.To().Value() {
			t.Fatal("unexpected message escaped the original surviving dialog")
		}
		if request.Method == sip.BYE {
			return request, last
		}
		if request.Method != sip.OPTIONS || len(request.Body()) != 0 || request.CSeq().SeqNo < last {
			t.Fatal("unexpected dialog operation while waiting for cleanup")
		}
		last = request.CSeq().SeqNo
		replyCallOK(t, peer, request)
	}
}

func TestDialogProbesPreserveSilentCallAndBYESequence(t *testing.T) {
	r := testCallRuntime(t)
	fastDialogProbes(r)
	call := connectCall(t, r, true)
	var lastCaller, lastCallee uint32
	for i, code := range []int{200, 405, 302} {
		caller := readCallRequest(t, call.caller, sip.OPTIONS)
		callee := readCallRequest(t, call.callee, sip.OPTIONS)
		assertOriginalProbe(t, caller, call.incoming, call.answer, true)
		assertOriginalProbe(t, callee, call.outgoing, call.remoteAnswer, false)
		if caller.CSeq().SeqNo <= lastCaller || callee.CSeq().SeqNo <= lastCallee {
			t.Fatal("probe sequence reused")
		}
		lastCaller, lastCallee = caller.CSeq().SeqNo, callee.CSeq().SeqNo
		replyCallOK(t, call.caller, caller)
		response := sip.NewResponseFromRequest(callee, code, "Reachable", nil)
		if i == 2 {
			response.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: "redirect.invalid", Port: 9}})
			response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			response.SetBody([]byte("not a media renegotiation"))
		}
		writeCall(t, call.callee, response)
	}
	// No RTP was sent. The opposite direction's CSeq is independent of probes.
	writeCall(t, call.caller, dialogRequest(t, call.incoming, call.answer, sip.BYE, true, 3))
	readCallFinal(t, call.caller, 200)
	bye := readCallRequest(t, call.callee, sip.BYE)
	if bye.CSeq().SeqNo <= lastCallee {
		t.Fatal("BYE reused a probe CSeq")
	}
	replyCallOK(t, call.callee, bye)
	waitCalls(t, r, 0)
}

func TestDialogProbesTerminateEitherDisappearedEndpoint(t *testing.T) {
	for _, dead := range []string{"caller", "callee"} {
		t.Run(dead, func(t *testing.T) {
			r := testCallRuntime(t)
			fastDialogProbes(r)
			call := connectCall(t, r, true)
			lost, alive := call.caller, call.callee
			if dead == "callee" {
				lost, alive = alive, lost
			}
			lost.conn.Close() // Actual UDP endpoint disappears without sending BYE.
			probe := readCallRequest(t, alive, sip.OPTIONS)
			replyCallOK(t, alive, probe)
			bye, last := readDialogByeAnsweringProbes(t, alive, probe)
			if bye.CSeq().SeqNo <= last {
				t.Fatal("cleanup sequence did not advance")
			}
			replyCallOK(t, alive, bye)
			waitCalls(t, r, 0)
		})
	}
}

func TestDialogProbesIgnoreUnownedFinalResponses(t *testing.T) {
	for _, mutation := range []string{"source", "to-tag", "from-tag", "call-id", "sequence", "method", "duplicate-to"} {
		t.Run(mutation, func(t *testing.T) {
			r := testCallRuntime(t)
			fastDialogProbes(r)
			call := connectCall(t, r, true)
			one := readCallRequest(t, call.caller, sip.OPTIONS)
			two := readCallRequest(t, call.callee, sip.OPTIONS)
			replyCallOK(t, call.caller, one)
			forged := sip.NewResponseFromRequest(two, 200, "Unowned", nil)
			switch mutation {
			case "to-tag":
				forged.To().Params.Add("tag", "foreign")
			case "from-tag":
				forged.From().Params.Add("tag", "foreign")
			case "call-id":
				forged.RemoveHeader("Call-ID")
				id := sip.CallIDHeader("foreign")
				forged.AppendHeader(&id)
			case "sequence":
				forged.CSeq().SeqNo++
			case "method":
				forged.CSeq().MethodName = sip.INFO
			case "duplicate-to":
				forged.AppendHeader(sip.HeaderClone(forged.To()))
			}
			if mutation == "source" {
				writeCall(t, newWireClient(t, r), forged)
			} else {
				writeCall(t, call.callee, forged)
			}
			// A matching-looking response cannot renew the original missing transaction.
			bye, last := readDialogByeAnsweringProbes(t, call.caller, one)
			if bye.CSeq().SeqNo <= last {
				t.Fatal("cleanup did not follow healthy probe sequence")
			}
			replyCallOK(t, call.caller, bye)
			waitCalls(t, r, 0)
		})
	}
}

func TestDialogProbesEndOnlyOriginalMissingDialog(t *testing.T) {
	for _, code := range []int{408, 481} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			r := testCallRuntime(t)
			fastDialogProbes(r)
			call := connectCall(t, r, true)
			caller := readCallRequest(t, call.caller, sip.OPTIONS)
			callee := readCallRequest(t, call.callee, sip.OPTIONS)
			replyCallOK(t, call.caller, caller)
			writeCall(t, call.callee, sip.NewResponseFromRequest(callee, code, "Gone", nil))
			replyCallOK(t, call.caller, readCallRequest(t, call.caller, sip.BYE))
			waitCalls(t, r, 0)
		})
	}
}

func TestDialogProbesShutdownCancelsAndJoinsPendingTransactions(t *testing.T) {
	r := testCallRuntime(t)
	fastDialogProbes(r)
	r.mu.Lock()
	r.callProbeTimeout = 5 * time.Second
	r.mu.Unlock()
	call := connectCall(t, r, true)
	readCallRequest(t, call.caller, sip.OPTIONS)
	readCallRequest(t, call.callee, sip.OPTIONS)
	done := make(chan struct{})
	go func() { r.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown waited for the probe peer deadline")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, wire := range r.callWires {
		if wire.request.Method == sip.OPTIONS {
			t.Fatal("probe transaction survived runtime shutdown")
		}
	}
}

func TestDialogOPTIONSRejectsForgeryBeforeTransactionCache(t *testing.T) {
	r := testCallRuntime(t)
	call := connectCall(t, r, true)
	original := dialogRequest(t, call.incoming, call.answer, sip.OPTIONS, true, 3)
	forged := original.Clone()
	forged.To().Params.Add("tag", "foreign")
	writeCall(t, call.caller, forged)
	readCallFinal(t, call.caller, 481)
	writeCall(t, call.caller, original)
	readCallFinal(t, call.caller, 200)
	writeCall(t, call.caller, dialogRequest(t, call.incoming, call.answer, sip.BYE, true, 4))
	readCallFinal(t, call.caller, 200)
	replyCallOK(t, call.callee, readCallRequest(t, call.callee, sip.BYE))
	waitCalls(t, r, 0)
	// Ended legs retained for late-answer cleanup are not living dialogs.
	late := dialogRequest(t, call.incoming, call.answer, sip.OPTIONS, true, 5)
	writeCall(t, call.caller, late)
	readCallFinal(t, call.caller, 481)
}

func TestDialogProbesOutgoingCarrierTimeoutEndsOriginalCallOnce(t *testing.T) {
	backend := newSyntheticCarrier()
	r := startCarrierTest(t, carrierTestConfig(t), backend, carrierTestStore(t))
	fastDialogProbes(r)
	caller, _, invite := prepareCaller(t, r, "7700900001")
	writeCall(t, caller, invite)
	waitCarrier(t, func() bool { return backend.dialCount() == 1 })
	backend.activate("module-a")
	answer := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	probe := readCallRequest(t, caller, sip.OPTIONS)
	assertOriginalProbe(t, probe, invite, answer, true)
	replyCallOK(t, caller, readCallRequest(t, caller, sip.BYE))
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
	if backend.dialCount() != 1 {
		t.Fatal("liveness failure redialed the carrier")
	}
}

func TestDialogProbesIncomingCarrierTimeoutEndsOriginalCallOnce(t *testing.T) {
	manager, r, backend := incomingRuntime(t, nil)
	fastDialogProbes(r)
	_, peer, invite := pendingIncoming(t, manager, r, backend, "module-a")
	answer := calleeResponse(t, peer, invite, testMediaSocket(t), 200)
	writeCall(t, peer, answer)
	readCallRequest(t, peer, sip.ACK)
	probe := readCallRequest(t, peer, sip.OPTIONS)
	assertOriginalProbe(t, probe, invite, answer, false)
	replyCallOK(t, peer, readCallRequest(t, peer, sip.BYE))
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
	if backend.answerCount() != 1 || backend.dialCount() != 0 {
		t.Fatal("liveness failure repeated carrier business")
	}
}

func TestDialogProbesPeerChallengeKeepsSequenceAndDoesNotAuthenticateOPTIONS(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	fastDialogProbes(p.r)
	id := p.carrier.incoming("module-a", "+15550000001")
	if !p.manager.ForwardIncoming("module-a", id) {
		t.Fatal("incoming refused")
	}
	initial := p.request(sip.INVITE)
	p.send(p.challenge(initial, 407, "SHA-256", "private-pbx"))
	p.request(sip.ACK)
	invite := p.request(sip.INVITE)
	assertPeerCallDigest(t, invite, "SHA-256")
	answer := p.answer(invite, testMediaSocket(t))
	p.send(answer)
	p.request(sip.ACK)
	probe := p.request(sip.OPTIONS)
	assertOriginalProbe(t, probe, invite, answer, false)
	if probe.CSeq().SeqNo != invite.CSeq().SeqNo+1 {
		t.Fatal("probe did not follow authenticated INVITE sequence")
	}
	p.send(p.challenge(probe, 407, "SHA-256", "private-pbx"))
	// A challenge is a reachable response, not authority to export credentials.
	next := p.request(sip.OPTIONS)
	if len(next.GetHeaders("Proxy-Authorization")) != 0 || len(next.GetHeaders("Authorization")) != 0 || next.CSeq().SeqNo <= probe.CSeq().SeqNo {
		t.Fatal("OPTIONS authenticated or reused its sequence")
	}
	p.send(p.response(next, 481))
	bye := p.request(sip.BYE)
	if bye.CSeq().SeqNo <= next.CSeq().SeqNo || !strings.Contains(bye.To().Value(), answer.To().Params.GetOr("tag", "")) {
		t.Fatal("peer cleanup lost original dialog or sequence")
	}
	p.send(p.response(bye, 200))
	waitCalls(t, p.r, 0)
	waitCarrier(t, func() bool { return p.carrier.hangupCount() == 1 })
}

func TestDialogProbesSlowLegDoesNotPostponeOtherLeg(t *testing.T) {
	r := testCallRuntime(t)
	fastDialogProbes(r)
	call := connectCall(t, r, true)
	first := readCallRequest(t, call.caller, sip.OPTIONS)
	slow := readCallRequest(t, call.callee, sip.OPTIONS)
	replyCallOK(t, call.caller, first)
	// The callee transaction remains in progress while the caller's next probe
	// must still run. Both legs remain silent in RTP throughout this exercise.
	next := readCallRequest(t, call.caller, sip.OPTIONS)
	if next.CSeq().SeqNo <= first.CSeq().SeqNo {
		t.Fatal("fast leg sequence reused")
	}
	replyCallOK(t, call.caller, next)
	replyCallOK(t, call.callee, slow)
	writeCall(t, call.caller, dialogRequest(t, call.incoming, call.answer, sip.BYE, true, 3))
	readCallFinal(t, call.caller, 200)
	replyCallOK(t, call.callee, readCallRequest(t, call.callee, sip.BYE))
	waitCalls(t, r, 0)
}
