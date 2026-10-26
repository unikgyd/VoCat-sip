package sipgateway

import (
	"context"
	"mime"
	"time"

	"github.com/emiago/sipgo/sip"
)

func (b *callBranch) run() {
	if b.entry.peer {
		b.runPeer()
		return
	}
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
	target := c.callee
	if b.target.Username != "" {
		target = b.target
	}
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: target.Username, Host: r.identityDomain()}}
	invite, err := r.buildCallRequest(sip.INVITE, b.entry.contact.Address, b.entry.source, from, to, randomToken(), 1, "", makeCallOffer(media.address(), media.controlAddress()))
	if err != nil {
		fail(503)
		return
	}
	wire, err := r.trackCallRequest(invite, b.entry.source)
	if err != nil {
		fail(503)
		return
	}
	defer r.removeCallWire(wire)
	b.mu.Lock()
	b.invite = invite
	b.wire = wire
	b.mu.Unlock()
	if b.isStopped() {
		return
	}
	if err = r.sendCallPacket(invite, b.entry.source); err != nil {
		fail(503)
		return
	}
	resend := time.NewTimer(500 * time.Millisecond)
	defer resend.Stop()
	delay := 500 * time.Millisecond
	var resendC <-chan time.Time = resend.C
	var grace *time.Timer
	var graceC <-chan time.Time
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	stop := b.stop
	abandoned, provisional, cancelSent := false, false, false
	finalReceived := false
	acknowledged := map[string]*sip.Request{}
	byes := map[string]bool{}
	selectedTag := ""
	sendCancel := func() {
		if cancelSent || !provisional || finalReceived || len(acknowledged) > 0 {
			return
		}
		cancelSent = true
		branch, _ := invite.Via().Params.Get("branch")
		request, err := r.buildCallRequest(sip.CANCEL, invite.Recipient, b.entry.source, invite.From(), invite.To(), invite.CallID().Value(), 1, branch, nil)
		if err != nil {
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
	sendBye := func(response *sip.Response) {
		if b.remoteEnded.Load() {
			return
		}
		tag := response.To().Params.GetOr("tag", "")
		if byes[tag] {
			return
		}
		byes[tag] = true
		c.cleanupWork.Add(1)
		go func() { defer c.cleanupWork.Done(); b.sendBye(response) }()
	}
	for {
		select {
		case response := <-wire.responses:
			if response.StatusCode < 200 {
				provisional = true
				b.provisional.Store(true)
				resend.Stop()
				resendC = nil
				if abandoned {
					sendCancel()
				}
				continue
			}
			resend.Stop()
			resendC = nil
			finalReceived = true
			b.finalResponse.Store(true)
			if response.StatusCode >= 300 {
				ack, err := b.ackRequest(response, false)
				if err == nil {
					_ = r.sendCallPacket(ack, b.entry.source)
				}
				if !reported {
					// A failed branch is cleanup-only even while other forks ring.
					// Conversely, a contradictory non-2xx after an accepted answer
					// is ACKed above but cannot retire its active media/ownership.
					abandoned = true
					fail(response.StatusCode)
					if grace == nil {
						grace = time.NewTimer(r.callLateTimeout)
						graceC = grace.C
					}
				}
				continue
			}
			tag := response.To().Params.GetOr("tag", "")
			if ack := acknowledged[tag]; ack != nil {
				_ = r.sendCallPacket(ack, b.entry.source)
				if abandoned {
					sendBye(response)
				}
				continue
			}
			ack, err := b.ackRequest(response, true)
			if err != nil {
				fail(488)
				b.stopBranch()
				continue
			}
			// The UAC must ACK all 2xx, including losing or invalid media answers.
			if len(acknowledged) >= 8 {
				_ = r.sendCallPacket(ack, b.entry.source)
				sendBye(response)
				b.stopBranch()
				continue
			}
			acknowledged[tag] = ack
			if abandoned || selectedTag != "" {
				_ = r.sendCallPacket(ack, b.entry.source)
				sendBye(response)
				continue
			}
			content := response.GetHeaders("Content-Type")
			mediaType := ""
			if len(content) == 1 {
				mediaType, _, _ = mime.ParseMediaType(content[0].Value())
			}
			description, err := parseCallSDP(response.Body())
			if mediaType != "application/sdp" || err != nil || len(description.blocks) != 1 || description.selected != 0 || description.format.pt != 0 && description.format.pt != 8 || description.events.Mask != 0 && description.events.Payload != 101 || len(response.GetHeaders("Contact")) != 1 || !safeCallContact(response.Contact()) || tag == "" || len(tag) > 128 {
				_ = r.sendCallPacket(ack, b.entry.source)
				sendBye(response)
				fail(488)
				b.stopBranch()
				continue
			}
			if err = media.configure(b.entry.source, description); err != nil {
				_ = r.sendCallPacket(ack, b.entry.source)
				sendBye(response)
				fail(488)
				b.stopBranch()
				continue
			}
			selectedTag = tag
			b.mu.Lock()
			b.response = response
			b.mu.Unlock()
			leg := &callLeg{call: c, branch: b, source: b.entry.source, id: invite.CallID().Value(), localTag: invite.From().Params.GetOr("tag", ""), remoteTag: tag, inviteSeq: 1}
			r.mu.Lock()
			r.callLegs[leg.id] = leg
			r.mu.Unlock()
			_ = r.sendCallPacket(ack, b.entry.source)
			reported = true
			select {
			case c.events <- callEvent{branch: b, response: response, description: description}:
			case <-r.ctx.Done():
				return
			}
		case <-resendC:
			if err = r.sendCallPacket(invite, b.entry.source); err != nil {
				fail(503)
				b.stopBranch()
			}
			delay = min(2*delay, 4*time.Second)
			resend.Reset(delay)
		case <-stop:
			stop = nil
			abandoned = true
			resend.Stop()
			resendC = nil
			if grace == nil {
				grace = time.NewTimer(r.callLateTimeout)
				graceC = grace.C
			}
			sendCancel()
			b.mu.Lock()
			accepted := b.response
			b.mu.Unlock()
			if accepted != nil {
				sendBye(accepted)
			}
		case <-graceC:
			return
		case <-r.ctx.Done():
			return
		}
	}
}
func (b *callBranch) ackRequest(response *sip.Response, success bool) (*sip.Request, error) {
	b.mu.Lock()
	invite := b.invite
	b.mu.Unlock()
	return b.ackForInvite(invite, response, success)
}
func (b *callBranch) ackForInvite(invite *sip.Request, response *sip.Response, success bool) (*sip.Request, error) {
	recipient := invite.Recipient
	branch, _ := invite.Via().Params.Get("branch")
	if success {
		branch = ""
		if contact := response.Contact(); safeCallContact(contact) {
			recipient = contact.Address
		}
	}
	return b.call.r.buildCallRequest(sip.ACK, recipient, b.entry.source, invite.From(), response.To(), invite.CallID().Value(), invite.CSeq().SeqNo, branch, nil)
}
func (b *callBranch) sendBye(response *sip.Response) {
	request, err := b.byeRequest(response)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(b.call.r.ctx, 3*time.Second)
	defer cancel()
	_ = b.call.r.callDo(ctx, request, b.entry.source)
}
func (b *callBranch) byeRequest(response *sip.Response) (*sip.Request, error) {
	b.mu.Lock()
	invite := b.invite
	b.mu.Unlock()
	return b.byeForInvite(invite, response)
}
func (b *callBranch) byeForInvite(invite *sip.Request, response *sip.Response) (*sip.Request, error) {
	recipient := invite.Recipient
	if contact := response.Contact(); safeCallContact(contact) {
		recipient = contact.Address
	}
	return b.call.r.buildCallRequest(sip.BYE, recipient, b.entry.source, invite.From(), response.To(), invite.CallID().Value(), invite.CSeq().SeqNo+1, "", nil)
}
