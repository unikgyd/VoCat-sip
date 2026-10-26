package sipgateway

import (
	"context"
	"mime"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

type peerCallAttempt struct {
	request     *sip.Request
	wire        *callWire
	provisional bool
	final       bool
	cancelSent  bool
	acks        map[string]*sip.Request
}
type peerCallResponse struct {
	attempt  *peerCallAttempt
	response *sip.Response
}

// The peer has independent INVITE attempts, not a second call or a renewed ring
// deadline. Retired challenge transactions remain routable until this branch's
// bounded late-response drain ends. They can never acquire the answer owner.
func (b *callBranch) runPeer() {
	c, r := b.call, b.call.r
	defer c.branchWork.Done()
	reported := false
	fail := func(code int) {
		if !reported {
			reported = true
			select {
			case c.events <- callEvent{branch: b, failure: code}:
			case <-r.ctx.Done():
			}
		}
	}
	media, err := newCallMedia(b.entry.source)
	if err != nil {
		fail(503)
		return
	}
	b.mu.Lock()
	b.media = media
	b.mu.Unlock()
	defer media.close()
	from := &sip.FromHeader{DisplayName: c.caller.DisplayName, Address: sip.Uri{Scheme: "sip", User: c.caller.Username, Host: r.identityDomain()}}
	from.Params.Add("tag", randomToken())
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: b.target.Username, Host: r.identityDomain()}}
	initial, err := r.buildCallRequest(sip.INVITE, b.entry.contact.Address, b.entry.source, from, to, randomToken(), 1, "", makeCallOffer(media.address(), media.controlAddress()))
	if err != nil {
		fail(503)
		return
	}
	forwardCtx, stopForward := context.WithCancel(r.ctx)
	var forwardWork sync.WaitGroup
	responses := make(chan peerCallResponse, 64)
	attempts := make([]*peerCallAttempt, 0, 2)
	defer func() {
		stopForward()
		forwardWork.Wait()
		for _, attempt := range attempts {
			r.removeCallWire(attempt.wire)
		}
	}()
	startAttempt := func(request *sip.Request) (*peerCallAttempt, error) {
		wire, e := r.trackCallRequest(request, b.entry.source)
		if e != nil {
			return nil, e
		}
		a := &peerCallAttempt{request: request, wire: wire, acks: map[string]*sip.Request{}}
		attempts = append(attempts, a)
		b.mu.Lock()
		b.invite, b.wire = request, wire
		b.mu.Unlock()
		b.provisional.Store(false)
		b.finalResponse.Store(false)
		forwardWork.Add(1)
		go func() {
			defer forwardWork.Done()
			for {
				select {
				case response := <-wire.responses:
					select {
					case responses <- peerCallResponse{a, response}:
					case <-forwardCtx.Done():
						return
					}
				case <-forwardCtx.Done():
					return
				}
			}
		}()
		return a, nil
	}
	current, err := startAttempt(initial)
	if err != nil {
		fail(503)
		return
	}
	if b.isStopped() || r.ctx.Err() != nil {
		return
	}
	if err = r.sendCallPacket(initial, b.entry.source); err != nil {
		fail(503)
		return
	}
	delay := 500 * time.Millisecond
	resend := time.NewTimer(delay)
	defer resend.Stop()
	var resendC <-chan time.Time = resend.C
	var grace *time.Timer
	var graceC <-chan time.Time
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	startGrace := func() {
		if grace == nil {
			grace = time.NewTimer(r.callLateTimeout)
			graceC = grace.C
		}
	}
	stop := b.stop
	abandoned, retried := false, false
	selectedTag := ""
	// Dialog tags are scoped by this branch's stable Call-ID/From tag. A
	// malformed old-attempt 2xx cannot send BYE to an already selected dialog.
	closedTags := map[string]bool{}
	sendBye := func(a *peerCallAttempt, response *sip.Response) {
		tag := response.To().Params.GetOr("tag", "")
		if b.remoteEnded.Load() || tag == "" || selectedTag == tag || closedTags[tag] || len(closedTags) >= 16 {
			return
		}
		closedTags[tag] = true
		request, e := b.byeForInvite(a.request, response)
		if e != nil {
			return
		}
		c.cleanupWork.Add(1)
		go func() {
			defer c.cleanupWork.Done()
			ctx, cancel := context.WithTimeout(r.ctx, 3*time.Second)
			defer cancel()
			_ = r.callDo(ctx, request, b.entry.source)
		}()
	}
	sendCancel := func(a *peerCallAttempt) {
		if a.cancelSent || !a.provisional || a.final || len(a.acks) != 0 {
			return
		}
		a.cancelSent = true
		invite := a.request
		branch, _ := invite.Via().Params.Get("branch")
		request, e := r.buildCallRequest(sip.CANCEL, invite.Recipient, b.entry.source, invite.From(), invite.To(), invite.CallID().Value(), invite.CSeq().SeqNo, branch, nil)
		if e != nil {
			return
		}
		c.cleanupWork.Add(1)
		go func() {
			defer c.cleanupWork.Done()
			ctx, cancel := context.WithTimeout(r.ctx, 3*time.Second)
			defer cancel()
			_ = r.callDo(ctx, request, b.entry.source)
		}()
	}
	abandon := func() {
		abandoned, stop = true, nil
		resend.Stop()
		resendC = nil
		startGrace()
		sendCancel(current)
		b.mu.Lock()
		accepted := b.response
		b.mu.Unlock()
		if accepted != nil {
			// The selected dialog now belongs to cleanup, no longer active media.
			selectedTag = ""
			sendBye(current, accepted)
		}
	}
	for {
		select {
		case event := <-responses:
			a, response := event.attempt, event.response
			if response.StatusCode < 200 {
				if a != current || a.final {
					continue
				}
				a.provisional = true
				b.provisional.Store(true)
				resend.Stop()
				resendC = nil
				if abandoned || b.isStopped() {
					if !abandoned {
						abandon()
					}
					sendCancel(a)
				}
				continue
			}
			a.final = true
			if a == current {
				resend.Stop()
				resendC = nil
				b.finalResponse.Store(true)
			}
			if response.StatusCode >= 300 {
				if ack, e := b.ackForInvite(a.request, response, false); e == nil {
					_ = r.sendCallPacket(ack, b.entry.source)
				}
				if a != current || reported {
					continue
				}
				if b.isStopped() && !abandoned {
					abandon()
				}
				if !abandoned && !retried && (response.StatusCode == 401 || response.StatusCode == 407) {
					retried = true
					if name, value, ok := r.peerAuthorization(a.request, response, b.entry.source); ok && r.ctx.Err() == nil && !b.isStopped() {
						retry, e := r.buildCallRequest(sip.INVITE, a.request.Recipient, b.entry.source, a.request.From(), a.request.To(), a.request.CallID().Value(), a.request.CSeq().SeqNo+1, "", a.request.Body())
						if e == nil {
							retry.AppendHeader(sip.NewHeader(name, value))
							var next *peerCallAttempt
							next, e = startAttempt(retry)
							if e == nil {
								current = next
							}
						}
						if e == nil && !b.isStopped() && r.ctx.Err() == nil {
							e = r.sendCallPacket(retry, b.entry.source)
						}
						if e == nil {
							delay = 500 * time.Millisecond
							resend.Reset(delay)
							resendC = resend.C
							continue
						}
					}
				}
				fail(response.StatusCode)
				// A failed attempt is cleanup-only even while another branch
				// keeps the call ringing. A late 2xx may be ACKed/BYEd but
				// must never acquire answer ownership after its failure.
				abandon()
				continue
			}
			tag := response.To().Params.GetOr("tag", "")
			ack := a.acks[tag]
			if ack == nil {
				ack, err = b.ackForInvite(a.request, response, true)
				if err != nil {
					fail(488)
					b.stopBranch()
					continue
				}
			}
			_ = r.sendCallPacket(ack, b.entry.source)
			if b.isStopped() && !abandoned {
				abandon()
			}
			if a != current || abandoned || selectedTag != "" || closedTags[tag] || len(a.acks) >= 8 {
				if tag != selectedTag || abandoned {
					sendBye(a, response)
				}
				continue
			}
			a.acks[tag] = ack
			content := response.GetHeaders("Content-Type")
			mediaType := ""
			if len(content) == 1 {
				mediaType, _, _ = mime.ParseMediaType(content[0].Value())
			}
			description, e := parseCallSDP(response.Body())
			if mediaType != "application/sdp" || e != nil || len(description.blocks) != 1 || description.selected != 0 || description.format.pt != 0 && description.format.pt != 8 || description.events.Mask != 0 && description.events.Payload != 101 || len(response.GetHeaders("Contact")) != 1 || !safeCallContact(response.Contact()) || tag == "" || len(tag) > 128 {
				sendBye(a, response)
				fail(488)
				b.stopBranch()
				continue
			}
			if err = media.configure(b.entry.source, description); err != nil {
				sendBye(a, response)
				fail(488)
				b.stopBranch()
				continue
			}
			selectedTag = tag
			b.mu.Lock()
			b.response = response
			b.mu.Unlock()
			leg := &callLeg{call: c, branch: b, source: b.entry.source, id: a.request.CallID().Value(), localTag: a.request.From().Params.GetOr("tag", ""), remoteTag: tag, inviteSeq: a.request.CSeq().SeqNo}
			r.mu.Lock()
			r.callLegs[leg.id] = leg
			r.mu.Unlock()
			reported = true
			select {
			case c.events <- callEvent{branch: b, response: response, description: description}:
			case <-r.ctx.Done():
				return
			}
		case <-resendC:
			if !abandoned && !b.isStopped() {
				if e := r.sendCallPacket(current.request, b.entry.source); e != nil {
					fail(503)
					b.stopBranch()
				}
				delay = min(2*delay, 4*time.Second)
				resend.Reset(delay)
			}
		case <-stop:
			abandon()
		case <-graceC:
			return
		case <-r.ctx.Done():
			return
		}
	}
}
