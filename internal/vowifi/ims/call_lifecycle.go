package ims

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"vocat/internal/vowifi"
)

const maxCallHistory = 128

// A single IMS media session cannot hold or mix two calls. The lock and the
// admission decision are per Session (one module), never package-global.
func (session *Session) admitCallLocked() bool {
	now := time.Now().UTC()
	for id, call := range session.calls {
		if call.public.EndedAt == nil || call.invitePending {
			return false
		}
		if now.Sub(*call.public.EndedAt) > terminalCallRetention && (call.media == nil || !call.media.hasCellularRouteCleanup()) {
			delete(session.calls, id)
		}
	}
	if len(session.calls) >= maxCallHistory {
		oldest := ""
		for id, call := range session.calls {
			if call.media != nil && call.media.hasCellularRouteCleanup() {
				continue
			}
			if oldest == "" || call.public.EndedAt.Before(*session.calls[oldest].public.EndedAt) {
				oldest = id
			}
		}
		if oldest == "" {
			return false
		}
		delete(session.calls, oldest)
	}
	return true
}

func (session *Session) callContext() context.Context {
	if session.refreshContext != nil {
		return session.refreshContext
	}
	return context.Background()
}

// Runtime writes include the time spent waiting for another writer. A blocked
// TCP peer must not indefinitely hold SIP cancellation, SMS, or local shutdown.
// The AfterFunc is joined before clearing the deadline so it cannot interrupt
// an unrelated later request using the same connection.
func (session *Session) writeRuntimePacket(ctx context.Context, packet []byte) error {
	if session.conn == nil && !session.canRedialProtectedTCP() {
		return net.ErrClosed
	}
	return writeSIPPacket(
		ctx,
		&session.writeMu,
		func(deadline time.Time) error {
			if session.canRedialProtectedTCP() && (session.conn == nil || session.mainConnDisconnected) {
				if err := session.reconnectProtectedTCPLocked(ctx); err != nil {
					return err
				}
			}
			if session.conn == nil {
				return net.ErrClosed
			}
			return session.conn.SetWriteDeadline(deadline)
		},
		func(data []byte) (int, error) {
			n, err := session.conn.Write(data)
			if err != nil && session.canRedialProtectedTCP() && ctx.Err() == nil {
				if reconnErr := session.reconnectProtectedTCPLocked(ctx); reconnErr == nil {
					if deadline, ok := ctx.Deadline(); ok {
						_ = session.conn.SetWriteDeadline(deadline)
					}
					n, err = session.conn.Write(data)
				}
			}
			if err == nil {
				session.lastMainActivity = time.Now()
			}
			return n, err
		},
		packet,
	)
}

func writeSIPPacket(ctx context.Context, writeMu *sync.Mutex, setDeadline func(time.Time) error, write func([]byte) (int, error), packet []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for !writeMu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := setDeadline(deadline); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = setDeadline(time.Now())
		close(done)
	})
	n, err := write(packet)
	if !stop() {
		<-done
	}
	_ = setDeadline(time.Time{})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	if err == nil && n != len(packet) {
		err = io.ErrShortWrite
	}
	return err
}

func (session *Session) maybeCancelInvite(call *imsCall) {
	session.callMu.Lock()
	if !call.terminated || !call.invitePending || !call.provisional || call.cancelSent {
		session.callMu.Unlock()
		return
	}
	call.cancelSent = true
	session.callMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(session.callContext(), 5*time.Second)
		defer cancel()
		_ = session.sendDialogRequest(ctx, call, "CANCEL")
	}()
}

// Copy only dialog values, not locks or mutable media state. Each fork's late
// 2xx is ACKed/BYEd with its own To-tag/Contact/routes, not the selected dialog.
func (session *Session) responseDialog(call *imsCall, response *sipResponse) *imsCall {
	session.callMu.Lock()
	dialog := session.dialogSnapshotLocked(call)
	session.callMu.Unlock()
	dialog.to = response.value("To")
	dialog.remoteTag = headerParameter(dialog.to, "tag")
	if contact := headerURI(response.value("Contact")); contact != "" {
		dialog.target = contact
	}
	if routes := response.values("Record-Route"); len(routes) > 0 {
		dialog.routes = reverseStrings(routes)
	}
	return dialog
}

func (session *Session) dialogSnapshotLocked(call *imsCall) *imsCall {
	return &imsCall{
		callID: call.callID, target: call.target, from: call.from, to: call.to,
		branch: call.branch, cseq: call.cseq, inviteTarget: call.inviteTarget, remoteTag: call.remoteTag,
		routes: append([]string(nil), call.routes...), sessionExpires: call.sessionExpires,
		inviteFrom: call.inviteFrom, inviteTo: call.inviteTo, inviteRoutes: append([]string(nil), call.inviteRoutes...),
	}
}

func (session *Session) endAnsweredDialog(dialog *imsCall) {
	go func() {
		ctx, cancel := context.WithTimeout(session.callContext(), 5*time.Second)
		defer cancel()
		_ = session.hangupAnsweredDialog(ctx, dialog)
	}()
}

func (session *Session) hangupAnsweredDialog(ctx context.Context, call *imsCall) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	session.callMu.Lock()
	if call.byeStarted {
		session.callMu.Unlock()
		return nil
	}
	call.byeStarted = true
	dialog := session.dialogSnapshotLocked(call)
	session.callMu.Unlock()
	return session.sendDialogRequest(ctx, dialog, "BYE")
}

func sameInviteTransaction(a, b *sipRequest) bool {
	if a == nil || b == nil {
		return false
	}
	aSeq, aMethod, aErr := cseqNumber(a.value("CSeq"))
	bSeq, bMethod, bErr := cseqNumber(b.value("CSeq"))
	return aErr == nil && bErr == nil && aMethod == "INVITE" && bMethod == b.Method &&
		(a.Method == b.Method || b.Method == "CANCEL") && aSeq == bSeq && a.URI == b.URI &&
		a.value("Call-ID") == b.value("Call-ID") &&
		headerParameter(a.value("Via"), "branch") != "" &&
		headerParameter(a.value("Via"), "branch") == headerParameter(b.value("Via"), "branch") &&
		headerURI(a.value("From")) == headerURI(b.value("From")) &&
		headerParameter(a.value("From"), "tag") == headerParameter(b.value("From"), "tag") &&
		headerURI(a.value("To")) == headerURI(b.value("To")) &&
		headerParameter(a.value("To"), "tag") == headerParameter(b.value("To"), "tag")
}

func (session *Session) respondCall(request *sipRequest, respond func([]byte) error, status int) {
	if response, err := buildSIPResponseWithBody(request, status, session.fromTag, nil); err == nil {
		_ = respond(response)
	}
}

func (session *Session) handleIncomingInvite(request *sipRequest, respond func([]byte) error) bool {
	callID := strings.TrimSpace(request.value("Call-ID"))
	if callID == "" || len(callID) > 256 {
		return true
	}
	session.callMu.Lock()
	existing := session.calls[callID]
	session.callMu.Unlock()
	if existing != nil {
		if sameInviteTransaction(existing.invite, request) {
			existing.replyMu.Lock()
			session.callMu.Lock()
			response := append([]byte(nil), existing.inviteResponse...)
			session.callMu.Unlock()
			if len(response) > 0 {
				_ = respond(response)
			}
			existing.replyMu.Unlock()
			return true
		}
		session.callMu.Lock()
		active := existing.public.State == "active" && !existing.terminated
		remoteTag := headerParameter(existing.to, "tag")
		session.callMu.Unlock()
		if active && headerParameter(request.value("To"), "tag") == session.fromTag &&
			headerParameter(request.value("From"), "tag") == remoteTag {
			return session.handleDialogOffer(request, respond, existing)
		}
		session.respondCall(request, respond, 481)
		return true
	}
	if headerParameter(request.value("To"), "tag") != "" {
		session.respondCall(request, respond, 481)
		return true
	}
	number := identityNumber(request.value("From"))
	target := headerURI(request.value("Contact"))
	if target == "" {
		target = request.URI
	}
	// Cellular media binding does WDS verification. Defer it to AnswerCall,
	// not this receive loop, so an immediately following CANCEL is readable.
	var media *rtpMedia
	var err error
	if session.request.Cellular == nil {
		media, err = newRTPMedia(session.localMediaIP())
		if err == nil && len(request.Body) > 0 {
			err = media.configureRemote(request.Body)
		}
	} else if len(request.Body) > 0 {
		_, _, _, _, err = parseAudioSDP(request.Body)
	}
	if err != nil {
		if media != nil {
			_ = media.Close()
		}
		session.respondCall(request, respond, 488)
		return true
	}
	response, err := buildSIPResponseWithBody(request, 180, session.fromTag, nil)
	if err != nil {
		if media != nil {
			_ = media.Close()
		}
		return true
	}
	call := &imsCall{
		public: vowifi.Call{ID: callID, Number: number, Direction: "incoming", State: "ringing", StartedAt: time.Now().UTC()},
		callID: callID, target: target, inviteTarget: request.URI, from: request.value("To") + ";tag=" + session.fromTag,
		to: request.value("From"), invite: request, respond: respond, routes: request.values("Record-Route"), media: media,
		pracked: make(map[string]bool), inviteResponse: response, inviteStatus: 180,
		answerAck: make(chan struct{}),
	}
	call.remoteCSeq, _, _ = cseqNumber(request.value("CSeq"))
	call.replyMu.Lock()
	session.callMu.Lock()
	if session.calls[callID] != nil {
		// Another protected receive socket may have admitted this very same
		// INVITE while media was being allocated. Replay it, not a false busy.
		session.callMu.Unlock()
		call.replyMu.Unlock()
		if media != nil {
			_ = media.Close()
		}
		return session.handleIncomingInvite(request, respond)
	}
	if !session.admitCallLocked() {
		session.callMu.Unlock()
		call.replyMu.Unlock()
		if media != nil {
			_ = media.Close()
		}
		session.respondCall(request, respond, 486)
		return true
	}
	session.calls[callID] = call
	session.callMediaContextLocked(call)
	session.callMu.Unlock()
	_ = respond(response)
	call.replyMu.Unlock()
	if session.provider != nil && session.provider.config.Logger != nil {
		session.provider.config.Logger.Info("IMS incoming call received", "category", "call", "device_id", session.request.DeviceID)
	}
	if session.provider != nil && session.provider.config.OnIncomingCall != nil {
		called := identityNumber(request.value("To"))
		if called == "" {
			called = session.identity.public
		}
		notification := ReceivedCall{DeviceID: session.request.DeviceID, IMSI: session.sourceIMSI(), CallID: callID,
			Caller: number, Called: called, Timestamp: time.Now().UTC()}
		go func() {
			ctx, cancel := context.WithTimeout(session.callContext(), 10*time.Second)
			defer cancel()
			_ = session.provider.config.OnIncomingCall(ctx, notification)
		}()
	}
	return true
}

func (session *Session) handleCallTermination(request *sipRequest, respond func([]byte) error) bool {
	id := strings.TrimSpace(request.value("Call-ID"))
	session.callMu.Lock()
	call := session.calls[id]
	session.callMu.Unlock()
	if call == nil {
		session.respondCall(request, respond, 481)
		return true
	}
	if request.Method == "CANCEL" && session.handleDialogOfferCancel(call, request, respond) {
		return true
	}
	call.replyMu.Lock()
	defer call.replyMu.Unlock()
	if request.Method == "CANCEL" {
		if !sameInviteTransaction(call.invite, request) {
			session.respondCall(request, respond, 481)
			return true
		}
		// A matching CANCEL gets 200, but cannot undo an INVITE's final 200.
		session.respondCall(request, respond, 200)
		session.callMu.Lock()
		final := call.inviteStatus >= 200
		session.callMu.Unlock()
		if final {
			return true
		}
		if terminated, err := buildSIPResponseWithBody(call.invite, 487, session.fromTag, nil); err == nil {
			session.callMu.Lock()
			call.inviteResponse = append([]byte(nil), terminated...)
			call.inviteStatus = 487
			session.callMu.Unlock()
			session.finishCall(id, "ended", 0, "")
			_ = call.respond(terminated)
		}
		return true
	}
	seq, method, err := cseqNumber(request.value("CSeq"))
	via := request.value("Via")
	session.callMu.Lock()
	valid := err == nil && method == "BYE" &&
		headerURI(request.value("From")) == headerURI(call.to) &&
		headerURI(request.value("To")) == headerURI(call.from) &&
		headerParameter(request.value("From"), "tag") != "" &&
		headerParameter(request.value("From"), "tag") == headerParameter(call.to, "tag") &&
		headerParameter(request.value("To"), "tag") == headerParameter(call.from, "tag") &&
		(seq > call.remoteCSeq || seq == call.remoteCSeq && call.remoteByeVia == via)
	if valid {
		call.remoteCSeq, call.remoteByeVia = seq, via
	}
	session.callMu.Unlock()
	if !valid {
		session.respondCall(request, respond, 481)
		return true
	}
	session.finishCall(id, "ended", 0, "")
	session.respondCall(request, respond, 200)
	return true
}

func (session *Session) handleCallACK(request *sipRequest) bool {
	id := strings.TrimSpace(request.value("Call-ID"))
	session.callMu.Lock()
	call := session.calls[id]
	seq, method, err := cseqNumber(request.value("CSeq"))
	var inviteSeq uint32
	if call != nil && call.invite != nil {
		inviteSeq, _, _ = cseqNumber(call.invite.value("CSeq"))
	}
	valid := call != nil && call.answerSent && !call.answerAcked && !call.terminated &&
		err == nil && method == "ACK" && seq == inviteSeq &&
		headerURI(request.value("From")) == headerURI(call.to) &&
		headerURI(request.value("To")) == headerURI(call.from) &&
		headerParameter(request.value("From"), "tag") == headerParameter(call.to, "tag") &&
		headerParameter(request.value("To"), "tag") == headerParameter(call.from, "tag")
	if valid {
		call.answerAcked = true
		close(call.answerAck)
		session.beginCallRecordingLocked(call)
	}
	session.callMu.Unlock()
	if !valid {
		return true // ACK never receives a response
	}
	if len(call.invite.Body) == 0 && call.media != nil {
		complete := func() { session.completeCallACKMedia(call, request) }
		if session.request.Cellular != nil {
			if !session.queueCellularMediaJob(call, complete) {
				session.finishCall(id, "failed", 0, "IMS media update queue is full")
				session.endAnsweredDialog(call)
			}
		} else {
			complete()
		}
	}
	return true
}

func (session *Session) completeCallACKMedia(call *imsCall, request *sipRequest) {
	id := call.callID
	if len(request.Body) == 0 {
		session.finishCall(id, "failed", 0, "IMS ACK omitted SDP answer")
		session.endAnsweredDialog(call)
	} else if err := session.configureCallMedia(call, request.Body); err != nil {
		session.finishCall(id, "failed", 0, err.Error())
		session.endAnsweredDialog(call)
	} else {
		session.setCallMediaReady(id)
	}
}

func (session *Session) recoverIncomingAnswer(call *imsCall) {
	session.recoverIncomingAnswerFor(call, 32*time.Second, 500*time.Millisecond)
}

func (session *Session) recoverIncomingAnswerFor(call *imsCall, timeout, interval time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-session.callContext().Done():
			return
		case <-call.answerAck:
			return
		case <-deadline.C:
			session.callMu.Lock()
			if call.answerAcked || call.terminated {
				session.callMu.Unlock()
				return
			}
			call.terminated = true
			session.callMu.Unlock()
			session.finishCall(call.callID, "failed", 0, "IMS answer ACK timed out")
			session.endAnsweredDialog(call)
			return
		case <-timer.C:
			call.replyMu.Lock()
			session.callMu.Lock()
			ended := call.terminated || call.answerAcked
			response := append([]byte(nil), call.inviteResponse...)
			session.callMu.Unlock()
			if ended {
				call.replyMu.Unlock()
				return
			}
			if session.transport == "udp" {
				_ = call.respond(response)
			}
			call.replyMu.Unlock()
			interval *= 2
			if interval > 4*time.Second {
				interval = 4 * time.Second
			}
			timer.Reset(interval)
		}
	}
}

// This assertion also prevents a future media-less replacement from silently
// satisfying the controller used by a SIP carrier adapter.
var _ vowifi.CallController = (*Session)(nil)
var _ vowifi.CallMediaController = (*Session)(nil)
