package sipgateway

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

// A final failure retires this branch's answer authority, not merely its
// retransmission timer. Another local fork must remain independently answerable.
func TestPeerCallsFailedChallengeCannotAcquireOtherForkAnswerOwner(t *testing.T) {
	for _, scenario := range []string{"wrong-realm", "second-challenge"} {
		t.Run(scenario, func(t *testing.T) {
			p := newFixedCallFixture(t, func(config *Config) {
				config.CarrierLines[0].IncomingExtensions = []string{"1001"}
			})
			local := registerMessagePeer(t, p.r, "1001")
			id := p.carrier.incoming("module-a", "+15550000001")
			if !p.manager.ForwardIncoming("module-a", id) {
				t.Fatal("local plus peer group admission failed")
			}
			initial := p.request(sip.INVITE)
			localInvite := readCallRequest(t, local, sip.INVITE)
			localMedia := testMediaSocket(t)
			writeCall(t, local, calleeResponse(t, local, localInvite, localMedia, 180))
			failedAttempt := initial
			if scenario == "wrong-realm" {
				p.send(p.challenge(initial, 401, "SHA-256", "foreign-realm"))
			} else {
				p.send(p.challenge(initial, 401, "SHA-256", "private-pbx"))
				p.request(sip.ACK)
				failedAttempt = p.request(sip.INVITE)
				if failedAttempt.CSeq().SeqNo != 2 {
					t.Fatal("retry did not have an independent transaction")
				}
				p.send(p.challenge(failedAttempt, 407, "SHA-256", "private-pbx"))
			}
			if ack := p.request(sip.ACK); ack.CSeq().SeqNo != failedAttempt.CSeq().SeqNo {
				t.Fatal("failure ACK targeted another attempt")
			}

			// The already-failed attempt sends an invalid late success while the
			// local fork is still ringing. It must get cleanup ACK/BYE, not media
			// ownership or a second authenticated INVITE.
			p.send(p.answer(failedAttempt, testMediaSocket(t)))
			if ack := p.request(sip.ACK); ack.CSeq().SeqNo != failedAttempt.CSeq().SeqNo {
				t.Fatal("late success ACK targeted another attempt")
			}
			bye := p.request(sip.BYE)
			if bye.CSeq().SeqNo != failedAttempt.CSeq().SeqNo+1 {
				t.Fatal("late success cleanup BYE used wrong sequence")
			}
			p.send(p.response(bye, 200))
			calls := p.r.status().Calls
			if p.carrier.answerCount() != 0 || p.carrier.hangupCount() != 0 || len(calls) != 1 || calls[0].Phase != "ringing" {
				t.Fatal("failed peer acquired/cancelled the local fork's answer owner")
			}

			answer := calleeResponse(t, local, localInvite, localMedia, 200)
			writeCall(t, local, answer)
			readCallRequest(t, local, sip.ACK)
			waitCarrier(t, func() bool {
				current := p.r.status().Calls
				return p.carrier.answerCount() == 1 && len(current) == 1 && current[0].Phase == "active"
			})
			p.r.mu.Lock()
			var current *sipCall
			for _, call := range p.r.calls {
				current = call
			}
			p.r.mu.Unlock()
			if current == nil {
				t.Fatal("local answer lost call owner")
			}
			current.mu.Lock()
			localWon := current.winner != nil && !current.winner.entry.peer && current.winner.target.Username == "1001"
			current.mu.Unlock()
			if !localWon {
				t.Fatal("failed peer was selected instead of registered local fork")
			}
			writeCall(t, local, dialogRequest(t, localInvite, answer, sip.BYE, false, 3))
			readCallFinal(t, local, 200)
			waitCalls(t, p.r, 0)
			waitCarrier(t, func() bool { return p.carrier.hangupCount() == 1 })
		})
	}
}
