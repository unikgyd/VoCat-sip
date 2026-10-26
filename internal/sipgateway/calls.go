package sipgateway

import (
	"context"
	"mime"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/rtcpmedia"
)

type CallStatus struct {
	ID            string                `json:"id"`
	Caller        string                `json:"caller"`
	Callee        string                `json:"callee"`
	Phase         string                `json:"phase"`
	StartedAt     time.Time             `json:"started_at"`
	CallerPackets uint64                `json:"caller_packets"`
	CalleePackets uint64                `json:"callee_packets"`
	CallerQuality *rtcpmedia.Statistics `json:"caller_quality,omitempty"`
	CalleeQuality *rtcpmedia.Statistics `json:"callee_quality,omitempty"`
	DeviceID      string                `json:"device_id,omitempty"`
	Transport     string                `json:"transport,omitempty"`
}
type callLeg struct {
	call                            *sipCall
	branch                          *callBranch // nil is the incoming caller leg
	source, id, localTag, remoteTag string
	inviteSeq, remoteSeq            uint32
	remoteBranch                    string
	remoteMethod                    sip.RequestMethod
}
type callEvent struct {
	branch      *callBranch
	response    *sip.Response
	description audioDescription
	failure     int
}
type sipCall struct {
	r              *runtime
	id             string
	caller, callee Account
	peerIngress    bool // peer auth principal is not a one-call local extension
	incoming       *sip.Request
	transaction    sip.ServerTransaction
	callerLeg      *callLeg
	media          *callMedia
	description    audioDescription
	branches       []*callBranch
	started        time.Time
	mu             sync.Mutex
	phase          string
	final          int
	winner         *callBranch
	stopped        bool
	endedBy        string
	stop           chan struct{}
	ack            chan struct{}
	acked          bool
	acceptedAt     time.Time
	accepted       *sip.Response
	events         chan callEvent
	branchWork     sync.WaitGroup
	cleanupWork    sync.WaitGroup
	carrier        *carrierLeg
	inbound        *carrierIncoming
	infoRequests   map[infoRequestKey]*infoReceipt
}
type callBranch struct {
	call          *sipCall
	entry         deliveryTarget
	target        Account // incoming carrier group may span several accounts
	mu            sync.Mutex
	media         *callMedia
	invite        *sip.Request
	response      *sip.Response
	wire          *callWire
	stop          chan struct{}
	stopOnce      sync.Once
	selected      atomic.Bool
	remoteEnded   atomic.Bool
	provisional   atomic.Bool
	finalResponse atomic.Bool
}

func (b *callBranch) stopBranch() { b.stopOnce.Do(func() { close(b.stop) }) }
func (b *callBranch) isStopped() bool {
	select {
	case <-b.stop:
		return true
	default:
		return false
	}
}
func (c *sipCall) stopCall(by string) {
	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		c.endedBy = by
		if c.carrier != nil && c.carrier.cancel != nil {
			c.carrier.cancel()
		}
		if c.inbound != nil {
			c.inbound.cancel()
		}
		close(c.stop)
	}
	c.mu.Unlock()
}
func (c *sipCall) snapshot() CallStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := CallStatus{ID: c.id, Caller: c.caller.Username, Callee: c.callee.Username, Phase: c.phase, StartedAt: c.started}
	if c.media != nil {
		status.CallerPackets = c.media.packets.Load()
		if c.media.control != nil {
			quality := c.media.control.Statistics()
			status.CallerQuality = &quality
		}
	}
	if c.inbound != nil {
		status.DeviceID, status.Transport = c.inbound.line.DeviceID, "carrier_incoming"
		if c.winner != nil {
			status.Callee = c.winner.target.Username
		}
		if bridge := c.inbound.bridge.Load(); bridge != nil {
			status.CallerPackets = bridge.sent.Load()
		}
	}
	if c.carrier != nil {
		status.Callee = c.carrier.number
		status.DeviceID, status.Transport = c.carrier.line.DeviceID, "carrier_outgoing"
		if bridge := c.carrier.bridge.Load(); bridge != nil {
			status.CalleePackets = bridge.sent.Load()
		}
	}
	if c.winner != nil {
		c.winner.mu.Lock()
		if c.winner.media != nil {
			status.CalleePackets = c.winner.media.packets.Load()
			if c.winner.media.control != nil {
				quality := c.winner.media.control.Statistics()
				status.CalleeQuality = &quality
			}
		}
		c.winner.mu.Unlock()
	}
	return status
}

func (r *runtime) invite(req *sip.Request, tx sip.ServerTransaction) {
	if !r.config.Config.CallsEnabled {
		r.respond(req, tx, 405, "Calls Disabled")
		return
	}
	principal, ok := r.authorizeBusiness(req, tx)
	if !ok {
		return
	}
	if req.To().Params.Has("tag") {
		r.respond(req, tx, 501, "Re-INVITE Not Implemented")
		return
	}
	caller := principal.account
	callee, exists := r.account(req.Recipient.User)
	// The first relay scope is peer <-> explicitly permitted carrier lines,
	// not an arbitrary PBX proxy or a reflection path back to the same peer.
	if principal.peer && exists {
		r.respond(req, tx, 403, "Peer Local Routing Not Permitted")
		return
	}
	if !caller.AllowCalls || exists && !callee.AllowCalls {
		r.respond(req, tx, 403, "Calls Not Permitted")
		return
	}
	var line CarrierLine
	var carrierNumber string
	if !exists {
		var code int
		line, carrierNumber, code = carrierRoute(r.config.Config, caller, req.Recipient.User)
		if code != 0 {
			r.respond(req, tx, code, "No Permitted Destination")
			return
		}
		callee = Account{Username: req.Recipient.User, DisplayName: line.Label}
	}
	if req.To().Address.User != callee.Username || req.From().Params.GetOr("tag", "") == "" || len(req.From().Params.GetOr("tag", "")) > 128 || req.MaxForwards() == nil || req.MaxForwards().Val() == 0 {
		r.respond(req, tx, 400, "Invalid Call")
		return
	}
	contacts := req.GetHeaders("Contact")
	if len(contacts) != 1 {
		r.respond(req, tx, 400, "Contact Required")
		return
	}
	contact, valid := contacts[0].(*sip.ContactHeader)
	if !valid || !safeCallContact(contact) {
		r.respond(req, tx, 400, "Invalid Contact")
		return
	}
	types := req.GetHeaders("Content-Type")
	mediaType := ""
	if len(types) == 1 {
		mediaType, _, _ = mime.ParseMediaType(types[0].Value())
	}
	if mediaType != "application/sdp" || len(req.GetHeaders("Content-Encoding")) != 0 {
		r.respond(req, tx, 415, "SDP Required")
		return
	}
	description, err := parseCallSDP(req.Body())
	if err != nil {
		r.respond(req, tx, 488, "G711 Audio Required")
		return
	}
	media, err := newCallMedia(req.Source())
	if err != nil {
		r.respond(req, tx, 503, "Media Unavailable")
		return
	}
	if err = media.configure(req.Source(), description); err != nil {
		media.close()
		r.respond(req, tx, 503, "Media Unavailable")
		return
	}
	r.mu.Lock()
	r.pruneLocked(time.Now())
	code := 0
	if r.closed.Load() || len(r.calls) >= 16 || len(r.callWires) > 480 {
		code = 503
	} else if r.callLegs[req.CallID().Value()] != nil {
		code = 482
	} else if !principal.peer && r.callBusy[caller.Username] != nil || exists && (caller.Username == callee.Username || r.callBusy[callee.Username] != nil) || !exists && r.carrierBusy[line.DeviceID] != nil {
		code = 486
	}
	bySource := map[string]registration{}
	for _, entry := range r.registrations[callee.Username] {
		previous, found := bySource[entry.source]
		if !found || entry.order > previous.order {
			bySource[entry.source] = entry
		}
	}
	if code == 0 && exists && len(bySource) == 0 {
		code = 480
	}
	if code == 0 && exists && r.branchSlots+len(bySource) > 480 {
		code = 503
	}
	if code != 0 {
		r.mu.Unlock()
		media.close()
		r.respond(req, tx, code, "Call Unavailable")
		return
	}
	c := &sipCall{r: r, id: randomToken(), caller: caller, callee: callee, peerIngress: principal.peer, incoming: req, transaction: tx, media: media, description: description, started: time.Now().UTC(), phase: "ringing", stop: make(chan struct{}), ack: make(chan struct{}), events: make(chan callEvent, 32)}
	c.callerLeg = &callLeg{call: c, source: req.Source(), id: req.CallID().Value(), localTag: randomToken(), remoteTag: req.From().Params.GetOr("tag", ""), inviteSeq: req.CSeq().SeqNo, remoteSeq: req.CSeq().SeqNo}
	if !exists {
		c.carrier = &carrierLeg{call: c, line: line, number: carrierNumber, events: make(chan carrierEvent, 16), activate: make(chan struct{})}
		r.carrierBusy[line.DeviceID] = c.carrier
	}
	for _, entry := range bySource {
		c.branches = append(c.branches, &callBranch{call: c, entry: localDeliveryTarget(entry), stop: make(chan struct{})})
	}
	r.branchSlots += len(c.branches)
	r.calls[c.id] = c
	r.callLegs[c.callerLeg.id] = c.callerLeg
	if !principal.peer {
		r.callBusy[caller.Username] = c
	}
	if exists {
		r.callBusy[callee.Username] = c
	}
	r.work.Add(1)
	r.mu.Unlock()
	defer r.work.Done()
	c.run()
}
func safeCallContact(contact *sip.ContactHeader) bool {
	return contact != nil && contact.Address.Scheme == "sip" && !contact.Address.Wildcard && contact.Address.Host != "" && contact.Address.Password == "" && len(contact.Address.Headers) == 0 && len(contact.Value()) <= 2048
}
func (c *sipCall) callerResponse(code int, reason string, body []byte) *sip.Response {
	response := sip.NewResponseFromRequest(c.incoming, code, reason, body)
	response.RemoveHeader("Record-Route")
	response.SetDestination(c.callerLeg.source)
	if code > 100 {
		response.To().Params.Add("tag", c.callerLeg.localTag)
	}
	if code == 200 {
		contact, _ := c.r.callContact(c.callerLeg.source)
		response.AppendHeader(contact)
		response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	return response
}
func (c *sipCall) run() {
	defer c.finish()
	if c.inbound != nil {
		c.runIncoming()
		return
	}
	if c.carrier != nil {
		c.runCarrier()
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	_ = c.transaction.Respond(c.callerResponse(100, "Trying", nil))
	_ = c.transaction.Respond(c.callerResponse(180, "Ringing", nil))
	c.mu.Unlock()
	for _, branch := range c.branches {
		c.branchWork.Add(1)
		go branch.run()
	}
	ring := time.NewTimer(c.r.callRingTimeout)
	var ringDeadline <-chan time.Time = ring.C
	defer ring.Stop()
	var ackTimeout, retransmission *time.Timer
	var ackDeadline, retransmit <-chan time.Time
	defer func() {
		if ackTimeout != nil {
			ackTimeout.Stop()
		}
		if retransmission != nil {
			retransmission.Stop()
		}
	}()
	ack := c.ack
	remaining := len(c.branches)
	lastFailure := 480
	var accepted *sip.Response
	delay := 500 * time.Millisecond
	for {
		select {
		case event := <-c.events:
			if event.response != nil && event.branch.isStopped() {
				event.response = nil
				event.failure = 480
			}
			if event.response == nil {
				remaining--
				if event.failure == 486 || event.failure == 488 && lastFailure != 486 || event.failure == 503 && lastFailure == 480 {
					lastFailure = event.failure
				}
				c.mu.Lock()
				if remaining == 0 && c.final == 0 && !c.stopped {
					c.final = lastFailure
					_ = c.transaction.Respond(c.callerResponse(lastFailure, "No Extension Answered", nil))
					c.mu.Unlock()
					c.stopCall("failed")
					return
				}
				c.mu.Unlock()
				continue
			}
			c.mu.Lock()
			if c.stopped || c.final != 0 || event.branch.isStopped() {
				c.mu.Unlock()
				event.branch.stopBranch()
				continue
			}
			c.winner = event.branch
			c.final = 200
			c.phase = "awaiting_ack"
			event.branch.selected.Store(true)
			ring.Stop()
			ringDeadline = nil
			event.branch.mu.Lock()
			winnerMedia := event.branch.media
			event.branch.mu.Unlock()
			// The two dialogs may use different payload numbers, but only
			// mutually negotiated digits can cross an internal/peer call.
			mask := c.description.events.Mask & event.description.events.Mask
			c.description.events.Mask = mask
			c.media.mu.Lock()
			c.media.events.Mask = mask
			c.media.mu.Unlock()
			winnerMedia.mu.Lock()
			winnerMedia.events.Mask = mask
			winnerMedia.mu.Unlock()
			c.media.destination.Store(winnerMedia)
			winnerMedia.destination.Store(c.media)
			for _, branch := range c.branches {
				if branch != event.branch {
					branch.stopBranch()
				}
			}
			accepted = c.callerResponse(200, "OK", makeCallAnswer(c.description, c.media.address(), c.media.controlAddress()))
			c.acceptedAt, c.accepted = time.Now(), accepted
			err := c.transaction.Respond(accepted)
			c.mu.Unlock()
			if err != nil {
				c.stopCall("transport")
				return
			}
			ackTimeout = time.NewTimer(c.r.callAckTimeout)
			ackDeadline = ackTimeout.C
			retransmission = time.NewTimer(delay)
			retransmit = retransmission.C
		case <-ack:
			ack = nil
			if ackTimeout != nil {
				ackTimeout.Stop()
			}
			ackDeadline = nil
			if retransmission != nil {
				retransmission.Stop()
			}
			retransmit = nil
			c.mu.Lock()
			if !c.stopped && c.final == 200 {
				c.phase = "active"
			}
			c.mu.Unlock()
		case <-retransmit:
			if accepted != nil {
				_ = c.transaction.Respond(accepted)
			}
			delay = min(2*delay, 4*time.Second)
			retransmission.Reset(delay)
		case <-ackDeadline:
			c.stopCall("ack_timeout")
			return
		case <-ringDeadline:
			c.mu.Lock()
			pending := c.final == 0
			if pending {
				c.final = 480
			}
			if pending {
				_ = c.transaction.Respond(c.callerResponse(480, "Call Timed Out", nil))
			}
			c.mu.Unlock()
			c.stopCall("timeout")
			return
		case <-c.stop:
			return
		case <-c.r.ctx.Done():
			c.stopCall("shutdown")
			return
		}
	}
}
func (c *sipCall) finish() {
	if c.inbound != nil {
		c.inbound.cancel()
		if bridge := c.inbound.bridge.Load(); bridge != nil {
			bridge.close()
		}
	}
	if c.carrier != nil {
		if c.carrier.cancel != nil {
			c.carrier.cancel()
		}
		if !c.carrier.started.Load() {
			c.carrier.release()
		}
		if bridge := c.carrier.bridge.Load(); bridge != nil {
			bridge.close()
		}
	}
	c.mu.Lock()
	c.phase = "ended"
	status, endedBy := c.final, c.endedBy
	c.mu.Unlock()
	for _, branch := range c.branches {
		branch.stopBranch()
		branch.mu.Lock()
		media := branch.media
		branch.mu.Unlock()
		if media != nil {
			media.close()
		}
	}
	if c.media != nil {
		c.media.close()
	}
	r := c.r
	r.mu.Lock()
	delete(r.calls, c.id)
	if r.callBusy[c.caller.Username] == c {
		delete(r.callBusy, c.caller.Username)
	}
	if r.callBusy[c.callee.Username] == c {
		delete(r.callBusy, c.callee.Username)
	}
	if c.inbound != nil {
		for _, branch := range c.branches {
			if r.callBusy[branch.target.Username] == c {
				delete(r.callBusy, branch.target.Username)
			}
		}
	}
	r.mu.Unlock()
	if c.inbound == nil && status == 200 && endedBy != "caller" {
		// A UAS waits for ACK (or its deadline) before sending BYE. This wait
		// never holds an active-call slot, RTP socket, or the runtime shutdown.
		c.waitCallerAck()
		if r.ctx.Err() == nil {
			c.sendCallerBye()
		}
	}
	c.branchWork.Wait()
	c.cleanupWork.Wait()
	r.mu.Lock()
	r.branchSlots -= len(c.branches)
	for id, leg := range r.callLegs {
		if leg.call == c {
			delete(r.callLegs, id)
		}
	}
	r.mu.Unlock()
}
func (c *sipCall) waitCallerAck() {
	c.mu.Lock()
	remaining, accepted := time.Until(c.acceptedAt.Add(c.r.callAckTimeout)), c.accepted
	c.mu.Unlock()
	if remaining <= 0 {
		return
	}
	deadline := time.NewTimer(remaining)
	defer deadline.Stop()
	retry := time.NewTicker(500 * time.Millisecond)
	defer retry.Stop()
	for {
		select {
		case <-c.ack:
			return
		case <-deadline.C:
			return
		case <-c.r.ctx.Done():
			return
		case <-retry.C:
			if accepted != nil {
				_ = c.transaction.Respond(accepted)
			}
		}
	}
}
func (c *sipCall) sendCallerBye() {
	request, err := c.callerBye()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.ctx, 3*time.Second)
	defer cancel()
	_ = c.r.callDo(ctx, request, c.callerLeg.source)
}
func (c *sipCall) callerBye() (*sip.Request, error) {
	contact := c.incoming.Contact()
	from := c.incoming.To().AsFrom()
	from.Params = from.Params.Clone()
	from.Params.Add("tag", c.callerLeg.localTag)
	to := c.incoming.From().AsTo()
	return c.r.buildCallRequest(sip.BYE, contact.Address, c.callerLeg.source, &from, &to, c.callerLeg.id, 1, "", nil)
}
func (r *runtime) callDialog(req *sip.Request, source string, ack bool) *callLeg {
	return r.checkCallDialog(req, source, ack, true)
}
func (r *runtime) checkCallDialog(req *sip.Request, source string, ack, commit bool) *callLeg {
	for _, name := range []string{"From", "To", "Call-ID", "CSeq"} {
		if len(req.GetHeaders(name)) != 1 {
			return nil
		}
	}
	if _, ok := req.GetHeaders("From")[0].(*sip.FromHeader); !ok {
		return nil
	}
	if _, ok := req.GetHeaders("To")[0].(*sip.ToHeader); !ok {
		return nil
	}
	if req.From() == nil || req.To() == nil || req.Via() == nil || req.CallID() == nil || req.CSeq() == nil || req.CSeq().SeqNo >= 1<<31 {
		return nil
	}
	r.mu.Lock()
	leg := r.callLegs[req.CallID().Value()]
	r.mu.Unlock()
	if leg == nil || source != leg.source {
		return nil
	}
	c := leg.call
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.From().Params.GetOr("tag", "") != leg.remoteTag || req.To().Params.GetOr("tag", "") != leg.localTag || req.CSeq().MethodName != req.Method {
		return nil
	}
	var from, to string
	if leg.branch != nil {
		from, to = leg.branch.invite.To().Address.String(), leg.branch.invite.From().Address.String()
	} else if c.incoming != nil {
		from, to = c.incoming.From().Address.String(), c.incoming.To().Address.String()
	} else {
		return nil
	}
	if req.From().Address.String() != from || req.To().Address.String() != to {
		return nil
	}
	if ack {
		if leg.branch != nil || req.CSeq().SeqNo != leg.inviteSeq || c.final != 200 {
			return nil
		}
		if !c.acked {
			c.acked = true
			close(c.ack)
		}
	} else {
		branch, _ := req.Via().Params.Get("branch")
		if req.CSeq().SeqNo < leg.remoteSeq || req.CSeq().SeqNo == leg.remoteSeq && (branch != leg.remoteBranch || leg.remoteMethod != "" && leg.remoteMethod != req.Method) {
			return nil
		}
		if commit {
			leg.remoteSeq = req.CSeq().SeqNo
			leg.remoteBranch = branch
			leg.remoteMethod = req.Method
		}
	}
	return leg
}
func (r *runtime) bye(req *sip.Request, tx sip.ServerTransaction) {
	leg := r.callDialog(req, req.Source(), false)
	if leg == nil {
		r.respond(req, tx, 481, "Unknown Dialog")
		return
	}
	r.respond(req, tx, 200, "OK")
	if leg.branch == nil {
		leg.call.stopCall("caller")
		return
	}
	leg.branch.remoteEnded.Store(true)
	leg.call.mu.Lock()
	selected := leg.call.winner == leg.branch
	leg.call.mu.Unlock()
	if selected {
		leg.call.stopCall("callee")
	} else {
		leg.branch.stopBranch()
	}
}
func (r *runtime) cancelCall(req *sip.Request, source string) {
	for _, name := range []string{"From", "To", "Via", "Call-ID", "CSeq"} {
		if len(req.GetHeaders(name)) != 1 {
			return
		}
	}
	if _, ok := req.GetHeaders("From")[0].(*sip.FromHeader); !ok {
		return
	}
	if _, ok := req.GetHeaders("To")[0].(*sip.ToHeader); !ok {
		return
	}
	code := 481
	r.mu.Lock()
	var c *sipCall
	if req.CallID() != nil {
		if leg := r.callLegs[req.CallID().Value()]; leg != nil && leg.branch == nil {
			c = leg.call
		}
	}
	r.mu.Unlock()
	if c != nil && req.CSeq() != nil && req.From() != nil && req.Via() != nil {
		branch, _ := req.Via().Params.Get("branch")
		expected, _ := c.incoming.Via().Params.Get("branch")
		if source == c.callerLeg.source && branch == expected && req.CSeq().MethodName == sip.CANCEL && req.CSeq().SeqNo == c.callerLeg.inviteSeq && req.From().Params.GetOr("tag", "") == c.callerLeg.remoteTag && req.From().Address.String() == c.incoming.From().Address.String() && req.To().Address.String() == c.incoming.To().Address.String() && req.To().Params.GetOr("tag", "") == "" && req.Recipient.String() == c.incoming.Recipient.String() {
			code = 200
			c.mu.Lock()
			cancel := c.final == 0 && !c.stopped
			if cancel {
				c.final = 487
				if c.carrier != nil && c.carrier.cancel != nil {
					c.carrier.cancel()
				}
				r.statelessCallResponse(req, source, 200)
				_ = c.transaction.Respond(c.callerResponse(487, "Request Terminated", nil))
				c.stopped = true
				c.endedBy = "cancel"
				close(c.stop)
			}
			c.mu.Unlock()
			if cancel {
				return
			}
		}
	}
	r.statelessCallResponse(req, source, code)
}
func (r *runtime) statelessCallResponse(req *sip.Request, source string, code int) {
	if req.Via() == nil || req.From() == nil || req.To() == nil || req.CSeq() == nil || req.CallID() == nil {
		return
	}
	response := sip.NewResponseFromRequest(req, code, "Call Result", nil)
	if remote, err := net.ResolveUDPAddr("udp", source); err == nil {
		_, _ = r.conn.WriteTo([]byte(response.String()), remote)
	}
}

// Local shutdown sends a single best-effort termination, then runtime.close
// cancels workers and closes sockets. It never waits for a remote SIP endpoint.
func (r *runtime) closeCalls() {
	_ = r.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	r.mu.Lock()
	calls := make([]*sipCall, 0, len(r.calls))
	for _, c := range r.calls {
		calls = append(calls, c)
	}
	r.mu.Unlock()
	for _, c := range calls {
		c.mu.Lock()
		pending, established := c.inbound == nil && c.final == 0, c.inbound == nil && c.final == 200 && c.acked
		if pending {
			c.final = 503
		}
		if !c.stopped {
			c.stopped = true
			c.endedBy = "shutdown"
			if c.carrier != nil && c.carrier.cancel != nil {
				c.carrier.cancel()
			}
			if c.inbound != nil {
				c.inbound.cancel()
			}
			close(c.stop)
		}
		if pending {
			_ = c.transaction.Respond(c.callerResponse(503, "Service Stopping", nil))
		}
		c.mu.Unlock()
		if established {
			if bye, err := c.callerBye(); err == nil {
				_ = r.sendCallPacket(bye, c.callerLeg.source)
			}
		}
		for _, b := range c.branches {
			b.mu.Lock()
			invite, response := b.invite, b.response
			b.mu.Unlock()
			if response != nil && !b.remoteEnded.Load() {
				if bye, err := b.byeRequest(response); err == nil {
					_ = r.sendCallPacket(bye, b.entry.source)
				}
			} else if invite != nil && b.provisional.Load() && !b.finalResponse.Load() {
				branch, _ := invite.Via().Params.Get("branch")
				if cancel, err := r.buildCallRequest(sip.CANCEL, invite.Recipient, b.entry.source, invite.From(), invite.To(), invite.CallID().Value(), invite.CSeq().SeqNo, branch, nil); err == nil {
					_ = r.sendCallPacket(cancel, b.entry.source)
				}
			}
		}
	}
}
