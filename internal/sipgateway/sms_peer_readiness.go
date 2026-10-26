package sipgateway

import (
	"context"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Readiness belongs to one immutable runtime/endpoint/credential generation,
// not to any SMS or SIM. Only one bounded owner and one cached result exist.
// All mutable fields are protected by runtime.mu; done publishes ready once.
type smsPeerProbe struct {
	done        chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	deadline    time.Time
	hasDeadline bool
	ready       bool
	wire        *smsPeerProbeWire
}

type smsPeerProbeWire struct {
	request *sip.Request
	expect  responseExpectation
	wake    chan struct{}
	sent    bool
	final   *sip.Response
}

// Each waiter retains its OWN total message budget. Cancelling one waiter
// never kills the shared runtime-owned one-second probe for other waiters.
func (r *runtime) smsPeerReady(ctx context.Context) bool {
	if ctx.Err() != nil || r.closed.Load() {
		return false
	}
	peer := r.config.Config.Peer
	if peer == nil || !peer.Enabled || !peer.AllowSMSToPeer {
		return false
	}
	if _, valid := usablePeerEndpoint(peer.Endpoint); !valid {
		return false
	}
	r.mu.Lock()
	if r.closed.Load() || ctx.Err() != nil {
		r.mu.Unlock()
		return false
	}
	if time.Now().Before(r.smsPeerProbeUntil) {
		ready := r.smsPeerProbeReady
		r.mu.Unlock()
		return ready && ctx.Err() == nil && !r.closed.Load()
	}
	probe, owner := r.smsPeerProbe, false
	if probe == nil {
		probeCtx, cancel := context.WithTimeout(r.ctx, time.Second)
		probe = &smsPeerProbe{done: make(chan struct{}), ctx: probeCtx, cancel: cancel}
		probe.deadline, probe.hasDeadline = probeCtx.Deadline()
		r.smsPeerProbe = probe
		// runtime.close sets closed before taking this same mutex, so no owner
		// can be added after shutdown begins waiting for owned work.
		r.work.Add(1)
		owner = true
	}
	r.mu.Unlock()
	if owner {
		go r.runSMSPeerProbe(probe)
	}
	select {
	case <-probe.done:
		return probe.ready && ctx.Err() == nil && !r.closed.Load()
	case <-ctx.Done():
		return false
	case <-r.ctx.Done():
		return false
	}
}

func (r *runtime) runSMSPeerProbe(probe *smsPeerProbe) {
	ready := false
	defer func() {
		probe.cancel()
		r.mu.Lock()
		probe.ready = ready
		probe.wire = nil // Late replies cannot complete a later probe/MT branch.
		if r.smsPeerProbe == probe {
			r.smsPeerProbe = nil
			r.smsPeerProbeReady = ready
			r.smsPeerProbeUntil = time.Now().Add(time.Second)
		}
		close(probe.done)
		r.mu.Unlock()
		r.work.Done()
	}()
	peer := r.config.Config.Peer
	if peer == nil || !peer.Enabled || !peer.AllowSMSToPeer {
		return
	}
	endpoint, valid := usablePeerEndpoint(peer.Endpoint)
	if !valid || probe.ctx.Err() != nil || r.closed.Load() {
		return
	}
	remote := net.UDPAddrFromAddrPort(endpoint)
	via, err := r.callVia(endpoint.String(), "z9hG4bK-"+randomToken())
	if err != nil {
		return
	}
	uri := sip.Uri{Scheme: "sip", User: "keepalive", Host: endpoint.Addr().String(), Port: int(endpoint.Port())}
	request := sip.NewRequest(sip.OPTIONS, uri)
	request.SetTransport("UDP")
	request.SetDestination(endpoint.String())
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "gateway", Host: r.identityDomain()}}
	from.Params.Add("tag", randomToken())
	to := &sip.ToHeader{Address: uri}
	callID := sip.CallIDHeader(randomToken())
	forward := sip.MaxForwardsHeader(20)
	request.AppendHeader(via)
	request.AppendHeader(from)
	request.AppendHeader(to)
	request.AppendHeader(&callID)
	request.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.OPTIONS})
	request.AppendHeader(&forward)
	request.AppendHeader(sip.NewHeader("User-Agent", "VoCat"))
	request.SetBody(nil)
	data := []byte(request.String())
	if len(data) > 1300 {
		return
	}
	wire := &smsPeerProbeWire{request: request, wake: make(chan struct{}, 1), expect: responseExpectation{
		source: endpoint.String(), callID: callID.Value(), sequence: 1, from: from.Address.String(), to: to.Address.String(), tag: from.Params.GetOr("tag", "")}}
	r.mu.Lock()
	if r.smsPeerProbe != probe || r.closed.Load() || probe.ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	probe.wire = wire
	r.mu.Unlock()
	final := func() *sip.Response {
		r.mu.Lock()
		response := wire.final
		r.mu.Unlock()
		return response
	}
	send := func() bool {
		r.mu.Lock()
		allowed := r.smsPeerProbe == probe && probe.wire == wire && !r.closed.Load() && probe.ctx.Err() == nil &&
			(!probe.hasDeadline || time.Now().Before(probe.deadline)) && wire.final == nil
		if allowed {
			wire.sent = true
		}
		r.mu.Unlock()
		if !allowed || r.closed.Load() || probe.ctx.Err() != nil {
			return false
		}
		_, err := r.conn.WriteTo(data, remote)
		return err == nil
	}
	if !send() {
		return
	}
	delay := sip.Timer_A
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-wire.wake:
		case <-timer.C:
			if response := final(); response != nil {
				ready = r.smsPeerProbeAccepted(response)
				return
			}
			if !send() {
				if response := final(); response != nil {
					ready = r.smsPeerProbeAccepted(response)
				}
				return
			}
			delay = min(2*delay, sip.T2)
			timer.Reset(delay)
		case <-probe.ctx.Done():
			if response := final(); response != nil {
				ready = r.smsPeerProbeAccepted(response)
			}
			return
		}
		if response := final(); response != nil {
			ready = r.smsPeerProbeAccepted(response)
			return
		}
	}
}

func (r *runtime) smsPeerProbeAccepted(response *sip.Response) bool {
	if response.StatusCode == 200 {
		return true
	}
	_, usable := r.pinnedPeerChallenge(response)
	return usable
}

// responseFilter performs singleton/framing validation first. No response is
// handed to a client FSM and no DB/credential proof is used in this route.
func (r *runtime) routeSMSPeerProbeResponse(response *sip.Response, source string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	probe := r.smsPeerProbe
	if probe == nil || probe.wire == nil {
		return false
	}
	wire := probe.wire
	branch := response.Via().Params.GetOr("branch", "")
	if branch != wire.request.Via().Params.GetOr("branch", "") {
		return false
	}
	expect := wire.expect
	if !wire.sent || source != expect.source || response.CallID().Value() != expect.callID ||
		response.CSeq().MethodName != sip.OPTIONS || response.CSeq().SeqNo != expect.sequence ||
		response.From().Address.String() != expect.from || response.To().Address.String() != expect.to ||
		response.From().Value() != wire.request.From().Value() || !matchingPeerVia(wire.request.Via(), response.Via()) {
		return true
	}
	if response.IsProvisional() || wire.final != nil {
		return true
	}
	if r.closed.Load() || probe.ctx.Err() != nil || probe.hasDeadline && !time.Now().Before(probe.deadline) {
		return true
	}
	wire.final = response
	select {
	case wire.wake <- struct{}{}:
	default:
	}
	return true
}
