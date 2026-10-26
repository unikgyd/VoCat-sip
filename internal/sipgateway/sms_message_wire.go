package sipgateway

import (
	"context"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Only source-bound MT MESSAGE transactions are owned here. Ordinary extension
// MESSAGE, REGISTER and all voice transactions retain their existing path.
// A DB read must never run inside sipgo's client FSM or a transport write hook:
// either would prevent a valid final response from arriving during that read.
type mtMessageWire struct {
	expect responseExpectation
	toTag  string
	wake   chan struct{}
	// Immutable admission budget for this exact wire transaction. Authentication
	// retries receive the same caller context, never a fresh timeout window.
	ctx         context.Context
	deadline    time.Time
	hasDeadline bool
	// All mutable fields below are protected by runtime.mu. Store the first
	// final separately, so an arbitrary number of provisionals cannot fill a
	// response queue and discard a genuine completed transaction.
	sent  bool
	final *sip.Response
}

func (r *runtime) trackMTMessageWire(ctx context.Context, request *sip.Request, source string) *mtMessageWire {
	branch := request.Via().Params.GetOr("branch", "")
	wire := &mtMessageWire{
		expect: responseExpectation{source: source, callID: request.CallID().Value(), sequence: request.CSeq().SeqNo,
			from: request.From().Address.String(), to: request.To().Address.String(), tag: request.From().Params.GetOr("tag", "")},
		toTag: request.To().Params.GetOr("tag", ""), wake: make(chan struct{}, 1), ctx: ctx,
	}
	wire.deadline, wire.hasDeadline = ctx.Deadline()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() || branch == "" || len(r.mtMessageWires) >= 512 || r.mtMessageWires[branch] != nil || r.pendingResponses[branch].callID != "" {
		return nil
	}
	if r.mtMessageWires == nil {
		r.mtMessageWires = map[string]*mtMessageWire{}
	}
	r.mtMessageWires[branch] = wire
	return wire
}

func (r *runtime) removeMTMessageWire(branch string, wire *mtMessageWire) {
	r.mu.Lock()
	if r.mtMessageWires[branch] == wire {
		delete(r.mtMessageWires, branch)
	}
	r.mu.Unlock()
}

func (r *runtime) mtMessageFinal(wire *mtMessageWire) *sip.Response {
	r.mu.Lock()
	final := wire.final
	r.mu.Unlock()
	return final
}

// responseFilter has already validated the singleton required headers, body
// framing and response status. Consume both valid and invalid replies for an
// owned branch, never exposing this transaction to sipgo's unrelated FSM.
func (r *runtime) routeMTMessageResponse(response *sip.Response, source string) bool {
	branch := response.Via().Params.GetOr("branch", "")
	r.mu.Lock()
	defer r.mu.Unlock()
	wire := r.mtMessageWires[branch]
	if wire == nil {
		return false
	}
	expect := wire.expect
	if !wire.sent || source != expect.source || response.CallID().Value() != expect.callID ||
		response.CSeq().MethodName != sip.MESSAGE || response.CSeq().SeqNo != expect.sequence ||
		response.From().Address.String() != expect.from || response.To().Address.String() != expect.to ||
		response.From().Params.GetOr("tag", "") != expect.tag ||
		wire.toTag != "" && response.To().Params.GetOr("tag", "") != wire.toTag {
		return true
	}
	// Pinned sipgo's non-INVITE transaction keeps retransmitting after 1xx;
	// provisionals do not reset its timer or the caller's overall deadline.
	if response.IsProvisional() || wire.final != nil {
		return true
	}
	// Guard cancellation/ROLLBACK may still be finishing after the original
	// budget ends. A newly arriving final must not revive that retired attempt.
	// Check the absolute deadline too: delivery of ctx.Done can be scheduled late.
	// An already saved timely final remains authoritative despite a later veto.
	if r.closed.Load() || wire.ctx == nil || wire.ctx.Err() != nil ||
		wire.hasDeadline && !time.Now().Before(wire.deadline) {
		return true
	}
	wire.final = response
	select {
	case wire.wake <- struct{}{}:
	default:
	}
	return true
}

func (r *runtime) guardedMessageResponse(ctx context.Context, request *sip.Request, source string, guards ...func() bool) (*sip.Response, int) {
	data := []byte(request.String())
	if len(data) > 1300 {
		return nil, 513
	}
	if ctx.Err() != nil || r.closed.Load() {
		return nil, 504
	}
	// Resolve before the final source DB read, not after it. Configured peer
	// destinations and observed registration sources retain their exact socket.
	remote, err := net.ResolveUDPAddr("udp", source)
	if err != nil {
		return nil, 504
	}
	wire := r.trackMTMessageWire(ctx, request, source)
	if wire == nil {
		return nil, 504
	}
	branch := request.Via().Params.GetOr("branch", "")
	defer r.removeMTMessageWire(branch, wire)

	// A source/config read can block while a prior datagram gets a real final
	// response. Final takes precedence over both retransmission and a later
	// veto; the already acknowledged send must not be falsely made Unknown.
	send := func() (*sip.Response, bool) {
		if final := r.mtMessageFinal(wire); final != nil {
			return final, true
		}
		if ctx.Err() != nil || r.closed.Load() {
			return nil, false
		}
		for _, allowed := range guards {
			valid := allowed != nil && allowed()
			if final := r.mtMessageFinal(wire); final != nil {
				return final, true
			}
			if !valid {
				return nil, false
			}
		}
		// No DB operation follows the guards. Their final source snapshot is
		// followed only by synchronous Contact/stop checks and this send fence.
		r.mu.Lock()
		final := wire.final
		valid := !r.closed.Load() && ctx.Err() == nil && r.mtMessageWires[branch] == wire
		if final == nil && valid {
			wire.sent = true
		}
		r.mu.Unlock()
		if final != nil {
			return final, true
		}
		if !valid {
			return nil, false
		}
		// Recheck cancellation immediately next to the actual datagram write.
		if ctx.Err() != nil || r.closed.Load() {
			return nil, false
		}
		_, err := r.conn.WriteTo(data, remote)
		return nil, err == nil
	}
	if final, sent := send(); final != nil {
		return final, 0
	} else if !sent {
		return nil, 504
	}
	delay := sip.Timer_A
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-wire.wake:
			if final := r.mtMessageFinal(wire); final != nil {
				return final, 0
			}
		case <-timer.C:
			if final, sent := send(); final != nil {
				return final, 0
			} else if !sent {
				return nil, 504
			}
			delay = min(2*delay, sip.T2)
			timer.Reset(delay)
		case <-ctx.Done():
			if final := r.mtMessageFinal(wire); final != nil {
				return final, 0
			}
			return nil, 504
		}
	}
}
