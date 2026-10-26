package ims

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/vowifi"
)

var (
	ErrCallNotFound = errors.New("ims: call not found")
	ErrCallState    = errors.New("ims: call is not in the required state")
	ErrCallBusy     = errors.New("ims: this session already has a call")
)

const terminalCallRetention = 30 * time.Second

const (
	mmtelServiceURN = "urn:urn-7:3gpp-service.ims.icsi.mmtel"
	mmtelFeatureTag = "urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"
)

type imsCall struct {
	public          vowifi.Call
	callID          string
	target          string
	from            string
	to              string
	branch          string
	cseq            uint32
	inviteTarget    string
	invite          *sipRequest
	respond         func([]byte) error
	responses       chan *sipResponse
	remoteTag       string
	routes          []string
	terminated      bool
	media           *rtpMedia
	mediaContext    context.Context
	mediaCancel     context.CancelFunc
	mediaJobs       chan func()
	dialogOfferMu   sync.Mutex
	dialogOffers    map[dialogOfferKey]*dialogOfferTransaction
	mediaRemoteCSeq uint32
	pracked         map[string]bool
	sessionExpires  int
	sessionCancel   context.CancelFunc
	// callMu protects these fields; replyMu orders incoming final responses
	// without holding the session state lock across network I/O.
	replyMu            sync.Mutex
	answering          bool
	answerSent         bool
	inviteResponse     []byte
	inviteStatus       int
	invitePending      bool
	provisional        bool
	cancelSent         bool
	inviteFrom         string
	inviteTo           string
	inviteRoutes       []string
	invitePacket       []byte
	wake               chan struct{}
	byeStarted         bool
	remoteCSeq         uint32
	remoteByeVia       string
	answerAck          chan struct{}
	answerAcked        bool
	localOfferPending  bool
	localOfferDrains   int
	recordingAttempted bool
}

func (session *Session) Calls() []vowifi.Call {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	now := time.Now().UTC()
	calls := make([]vowifi.Call, 0, len(session.calls))
	for id, call := range session.calls {
		if call.media != nil && call.media.control != nil {
			quality := call.media.control.Statistics()
			call.public.MediaQuality = &quality
		}
		call.public.AnswerConfirmed = !call.terminated && call.public.State == "active" && (!call.answerSent || call.answerAcked)
		call.public.DTMFDigits = ""
		call.public.CanHold = !call.terminated && call.public.State == "active" && (!call.answerSent || call.answerAcked) && call.media != nil && call.media.ready()
		if call.public.CanHold && !call.public.Held && !call.localOfferPending {
			call.public.DTMFDigits = call.media.DTMFDigits()
		}
		if call.public.EndedAt != nil && !call.invitePending && now.Sub(*call.public.EndedAt) > terminalCallRetention &&
			(call.media == nil || !call.media.hasCellularRouteCleanup()) {
			delete(session.calls, id)
			continue
		}
		calls = append(calls, call.public)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].StartedAt.Before(calls[j].StartedAt) })
	return calls
}

func (session *Session) DialCall(ctx context.Context, number string) (vowifi.Call, error) {
	if err := ctx.Err(); err != nil {
		return vowifi.Call{}, err
	}
	if err := session.verifyCellularAccess(ctx); err != nil {
		return vowifi.Call{}, err
	}
	number = strings.TrimSpace(number)
	if !validCallNumber(number) {
		return vowifi.Call{}, errors.New("ims: invalid dial number")
	}
	callToken, err := randomHex(18)
	if err != nil {
		return vowifi.Call{}, err
	}
	branch, err := randomHex(12)
	if err != nil {
		return vowifi.Call{}, err
	}
	callID := callToken + "@" + addressHost(session.conn.LocalAddr())
	carrierProfile := vowifi.ResolveCarrierProfile(session.request.Identity)
	target := callTargetURI(number, session.identity.domain, carrierProfile)
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return vowifi.Call{}, net.ErrClosed
	}
	cseq := session.cseq
	session.cseq++
	routes := append([]string(nil), session.evidence.ServiceRoute...)
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	fromIdentity, preferredIdentity, identitySource := session.callOriginatingIdentitiesLocked(carrierProfile)
	session.mu.Unlock()
	media, err := newAccessRTPMedia(ctx, session.localMediaIP(), session.request.Cellular, openNativeAMR)
	if err != nil {
		return vowifi.Call{}, err
	}
	body := media.offerSDP(session.localMediaIP())
	transportUpper := strings.ToUpper(session.transport)
	from := "<" + fromIdentity + ">;tag=" + session.fromTag
	to := "<" + target + ">"
	lines := []string{
		"INVITE " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", transportUpper, session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	lines = append(lines, securityHeaders...)
	inviteRoutes := append([]string(nil), routes...)
	if len(routes) == 0 {
		inviteRoutes = []string{"<sip:" + session.endpoint.address() + ";transport=" + session.transport + ";lr>"}
		lines = append(lines, "Route: "+inviteRoutes[0])
	} else {
		for _, route := range routes {
			lines = append(lines, "Route: "+route)
		}
	}
	lines = append(lines,
		"From: "+from,
		"To: "+to,
		"Call-ID: "+callID,
		fmt.Sprintf("CSeq: %d INVITE", cseq),
		session.dialogContactHeader(),
		"P-Preferred-Identity: <"+preferredIdentity+">",
		"P-Preferred-Service: "+mmtelServiceURN,
		`Accept-Contact: *;+g.3gpp.icsi-ref="`+mmtelFeatureTag+`"`,
	)
	lines = session.appendPAccessNetworkInfoHeader(lines)
	lines = append(lines,
		"User-Agent: "+session.imsUserAgent(),
		"Allow: INVITE, ACK, CANCEL, BYE, OPTIONS, MESSAGE, PRACK, UPDATE, INFO",
		"Supported: 100rel, timer, replaces",
		"Session-Expires: 1800;refresher=uac",
		"Min-SE: 90",
		"Accept: application/sdp",
		"Content-Type: application/sdp",
		"Content-Length: "+strconv.Itoa(len(body)), "", "",
	)
	request := append([]byte(strings.Join(lines, "\r\n")), body...)
	responses := make(chan *sipResponse, 8)
	key := sipTransactionKey{callID: callID, cseq: cseq, method: "INVITE"}
	call := &imsCall{
		public: vowifi.Call{ID: callID, Number: number, Direction: "outgoing", State: "dialing", StartedAt: time.Now().UTC()},
		callID: callID, target: target, inviteTarget: target, from: from, to: to, branch: branch, cseq: cseq, responses: responses,
		routes: routes, media: media, pracked: make(map[string]bool),
		invitePending: true, inviteFrom: from, inviteTo: to, inviteRoutes: inviteRoutes,
		invitePacket: request, wake: make(chan struct{}, 1),
	}
	call.public.SignalingPending = true
	session.callMu.Lock()
	if !session.admitCallLocked() {
		session.callMu.Unlock()
		_ = media.Close()
		return vowifi.Call{}, ErrCallBusy
	}
	session.calls[callID] = call
	session.callMediaContextLocked(call)
	result := call.public
	session.callMu.Unlock()
	session.transactionsMu.Lock()
	if _, duplicate := session.transactions[key]; duplicate {
		session.transactionsMu.Unlock()
		_ = media.Close()
		session.callMu.Lock()
		delete(session.calls, callID)
		session.callMu.Unlock()
		return vowifi.Call{}, errors.New("ims: duplicate call transaction")
	}
	session.transactions[key] = responses
	session.transactionsMu.Unlock()
	if session.provider != nil && session.provider.config.Logger != nil {
		session.provider.config.Logger.Info("IMS call started",
			"category", "call",
			"device_id", session.request.DeviceID,
			"direction", "outgoing",
			"identity_source", identitySource,
			"target_scheme", strings.ToLower(strings.TrimSuffix(strings.SplitN(target, ":", 2)[0], ":")),
		)
	}
	err = session.writeRuntimePacket(ctx, request)
	if err != nil {
		// A cancelled/partial write does not prove INVITE was not delivered.
		// Keep its matching path, so a late 1xx is cancelled and 2xx ACK/BYEd.
		session.finishCall(callID, "failed", 0, "SIP INVITE delivery unknown")
		session.callMu.Lock()
		result = call.public
		session.callMu.Unlock()
		call.wake <- struct{}{}
		go session.watchOutgoingCall(call, key)
		return result, fmt.Errorf("ims: send SIP INVITE: %w", err)
	}
	go session.watchOutgoingCall(call, key)
	return result, nil
}

func (session *Session) watchOutgoingCall(call *imsCall, key sipTransactionKey) {
	timer := time.NewTimer(32 * time.Second)
	defer timer.Stop()
	finalSeen := false
	dialogs := make(map[string]bool) // bounded late/forked 2xx ACK/BYE drain
	var retransmit <-chan time.Time
	var retransmitTimer *time.Timer
	interval := 500 * time.Millisecond
	if session.transport == "udp" && len(call.invitePacket) > 0 {
		retransmitTimer = time.NewTimer(interval)
		retransmit = retransmitTimer.C
		defer retransmitTimer.Stop()
	}
	defer func() {
		session.callMu.Lock()
		call.invitePending = false
		call.public.SignalingPending = false
		session.callMu.Unlock()
		session.transactionsMu.Lock()
		delete(session.transactions, key)
		session.transactionsMu.Unlock()
	}()
	for {
		select {
		case <-session.callContext().Done():
			session.finishCall(call.callID, "ended", 0, "IMS session closed")
			return
		case <-call.wake:
			retransmit = nil // never resend INVITE after local cancellation
			if !finalSeen {
				timer.Reset(32 * time.Second)
			}
		case <-retransmit:
			if session.callWasTerminated(call.callID) {
				retransmit = nil
				continue
			}
			if err := session.writeRuntimePacket(session.callContext(), call.invitePacket); err != nil {
				session.finishCall(call.callID, "failed", 0, "SIP INVITE retransmission failed")
				retransmit = nil
				timer.Reset(32 * time.Second)
				continue // still drain a final response to the original send
			}
			interval *= 2
			if interval > 4*time.Second {
				interval = 4 * time.Second
			}
			retransmitTimer.Reset(interval)
		case <-timer.C:
			if finalSeen {
				return
			}
			if session.callWasTerminated(call.callID) {
				session.finishCall(call.callID, "ended", 0, "")
				return
			}
			session.finishCall(call.callID, "failed", 0, "SIP INVITE transaction timed out")
			return
		case response := <-call.responses:
			if response == nil {
				continue
			}
			diagnostic := callResponseDiagnostic(response)
			session.logCallResponse(response, diagnostic)
			if response.StatusCode < 200 {
				retransmit = nil
				if finalSeen {
					continue
				}
				session.callMu.Lock()
				firstProvisional := !call.provisional
				call.provisional = true
				session.callMu.Unlock()
				session.maybeCancelInvite(call)
				if session.callWasTerminated(call.callID) {
					continue
				}
				if firstProvisional {
					timer.Reset(2 * time.Minute)
				}
				session.setCallDiagnostic(call.callID, response.StatusCode, diagnostic)
				session.updateCallDialogFromResponse(call, response)
				if len(response.Body) > 0 {
					if mediaErr := session.configureCallMedia(call, response.Body); mediaErr == nil {
						session.setCallMediaReady(call.callID)
						session.setCallState(call.callID, "early_media")
					}
				} else if response.StatusCode >= 180 {
					session.setCallState(call.callID, "ringing")
				}
				if reliableProvisional(response) {
					go session.sendPRACK(call, response)
				}
				continue
			}
			firstFinal := !finalSeen
			retransmit = nil
			if firstFinal {
				finalSeen = true
				timer.Reset(32 * time.Second)
				session.callMu.Lock()
				call.invitePending = false
				call.public.SignalingPending = false
				session.callMu.Unlock()
			}
			if response.StatusCode < 300 {
				dialog := session.responseDialog(call, response)
				_ = session.sendACK(dialog) // retransmitted 2xx needs another ACK
				if _, seen := dialogs[dialog.remoteTag]; seen {
					continue
				}
				if len(dialogs) >= 8 {
					// Do not leave an excess accepted fork up, but also do not
					// allocate another response channel/goroutine for it.
					session.mu.Lock()
					cseq := session.cseq
					session.cseq++
					session.mu.Unlock()
					_ = session.writeRuntimePacket(session.callContext(), session.buildDialogRequest(dialog, "BYE", cseq))
					continue
				}
				dialogs[dialog.remoteTag] = true
				if !firstFinal || session.callWasTerminated(call.callID) {
					session.endAnsweredDialog(dialog)
					continue
				}
				mediaErr := session.configureCallMedia(call, response.Body)
				if mediaErr != nil {
					session.finishCall(call.callID, "failed", response.StatusCode, mediaErr.Error())
					session.endAnsweredDialog(dialog)
					continue
				}
				session.callMu.Lock()
				cancelled := call.terminated || call.public.EndedAt != nil
				if !cancelled {
					call.to, call.target, call.remoteTag, call.routes = dialog.to, dialog.target, dialog.remoteTag, dialog.routes
					call.public.State = "active"
					now := time.Now().UTC()
					call.public.AnsweredAt = &now
					call.public.MediaReady, call.public.Codec = call.media.ready(), call.media.Codec()
					session.beginCallRecordingLocked(call)
				}
				session.callMu.Unlock()
				if cancelled {
					session.endAnsweredDialog(dialog)
				} else {
					session.startSessionTimer(call, response.value("Session-Expires"))
				}
			} else {
				if ackErr := session.sendRejectedInviteACK(call, response); ackErr != nil && session.provider != nil && session.provider.config.Logger != nil {
					session.provider.config.Logger.Warn("IMS rejected INVITE ACK failed",
						"category", "call",
						"device_id", session.request.DeviceID,
						"carrier_profile", vowifi.ResolveCarrierProfile(session.request.Identity).ID,
						"sip_status", response.StatusCode,
						"error", safeSIPDiagnostic(ackErr.Error()),
					)
				}
				if !firstFinal {
					continue
				}
				if session.callWasTerminated(call.callID) {
					// CANCEL normally causes the pending INVITE transaction to finish
					// with 487 Request Terminated.  It is the expected response to our
					// local hang-up, not a new network rejection.
					session.finishCall(call.callID, "ended", response.StatusCode, diagnostic)
				} else {
					session.finishCall(call.callID, "failed", response.StatusCode, diagnostic)
				}
			}
		}
	}
}

func (session *Session) AnswerCall(ctx context.Context, id string) (vowifi.Call, error) {
	if err := ctx.Err(); err != nil {
		return vowifi.Call{}, err
	}
	if err := session.verifyCellularAccess(ctx); err != nil {
		return vowifi.Call{}, err
	}
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallNotFound
	}
	if call.public.Direction != "incoming" || call.public.State != "ringing" || call.terminated || call.answering || call.invite == nil || call.respond == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallState
	}
	request, respond := call.invite, call.respond
	call.answering = true
	mediaContext := session.callMediaContextLocked(call)
	session.callMu.Unlock()
	defer func() {
		session.callMu.Lock()
		call.answering = false
		session.callMu.Unlock()
	}()
	// WDS socket binding and route acquisition must never hold replyMu:
	// CANCEL needs that lock to commit 487 while an answer is being prepared.
	if session.request.Cellular != nil {
		if err := session.prepareCellularAnswerMedia(ctx, mediaContext, call); err != nil {
			session.rejectCellularAnswerSetup(call)
			return vowifi.Call{}, err
		}
	}
	call.replyMu.Lock()
	defer call.replyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return vowifi.Call{}, err
	}
	if err := mediaContext.Err(); err != nil {
		return vowifi.Call{}, err
	}
	if session.callWasTerminated(id) {
		return vowifi.Call{}, ErrCallState
	}
	response, err := buildSIPResponseWithBody(
		request,
		200,
		session.fromTag,
		call.media.answerSDP(session.localMediaIP()),
		session.dialogContactHeader(),
	)
	if err != nil {
		return vowifi.Call{}, err
	}
	session.callMu.Lock()
	call.answerSent = true
	call.inviteStatus = 200
	call.inviteResponse = append([]byte(nil), response...)
	session.callMu.Unlock()
	if err := respond(response); err != nil {
		// A write error does not prove the peer did not receive 200. Do not
		// allow a second owner to answer again or leave a possible dialog up.
		session.finishCall(id, "failed", 0, "IMS answer delivery unknown")
		session.endAnsweredDialog(call)
		return vowifi.Call{}, err
	}
	session.callMu.Lock()
	ended := call.terminated
	if !ended {
		call.public.State = "active"
		now := time.Now().UTC()
		call.public.AnsweredAt = &now
		call.public.MediaReady, call.public.Codec = call.media.ready(), call.media.Codec()
		session.beginCallRecordingLocked(call)
	}
	result := call.public
	session.callMu.Unlock()
	if ended {
		return vowifi.Call{}, ErrCallState // hangup owns the post-200 BYE
	}
	session.startSessionTimer(call, request.value("Session-Expires"))
	go session.recoverIncomingAnswer(call)
	return result, nil
}

func (session *Session) HangupCall(ctx context.Context, id string) error {
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return ErrCallNotFound
	}
	if call.terminated || call.public.EndedAt != nil {
		session.callMu.Unlock()
		return nil
	}
	state := call.public.State
	direction := call.public.Direction
	request, respond := call.invite, call.respond
	call.terminated = true
	session.callMu.Unlock()
	session.finishCall(id, "ended", 0, "") // local UI/media do not wait for a peer
	if direction == "incoming" && state == "ringing" && request != nil && respond != nil {
		call.replyMu.Lock()
		defer call.replyMu.Unlock()
		session.callMu.Lock()
		answered := call.answerSent
		final := call.inviteStatus >= 200
		session.callMu.Unlock()
		if answered {
			return session.hangupAnsweredDialog(ctx, call)
		}
		if final {
			return nil
		}
		response, err := buildSIPResponseWithBody(request, 486, session.fromTag, nil)
		if err != nil {
			return err
		}
		session.callMu.Lock()
		call.inviteResponse = append([]byte(nil), response...)
		call.inviteStatus = 486
		session.callMu.Unlock()
		return respond(response)
	}
	if direction == "outgoing" && (state == "dialing" || state == "ringing" || state == "early_media") {
		select {
		case call.wake <- struct{}{}:
		default:
		}
		session.maybeCancelInvite(call)
		return nil
	}
	err := session.hangupAnsweredDialog(ctx, call)
	// A remote endpoint may already have removed the dialog and answer BYE with
	// 481. The local call must still leave the active list after a hang-up.
	session.finishCall(id, "ended", 0, "")
	return err
}

func (session *Session) callWasTerminated(id string) bool {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	call := session.calls[id]
	return call != nil && call.terminated
}

func (session *Session) handleCallRequest(request *sipRequest, respond func([]byte) error) bool {
	switch request.Method {
	case "INVITE":
		return session.handleIncomingInvite(request, respond)
	case "PRACK":
		response, err := buildSIPResponseWithBody(request, 200, session.fromTag, nil)
		if err == nil {
			_ = respond(response)
		}
		return true
	case "UPDATE":
		callID := strings.TrimSpace(request.value("Call-ID"))
		session.callMu.Lock()
		call := session.calls[callID]
		session.callMu.Unlock()
		if call == nil {
			session.respondCall(request, respond, 481)
			return true
		}
		return session.handleDialogOffer(request, respond, call)
	case "ACK":
		return session.handleCallACK(request)
	case "CANCEL", "BYE":
		return session.handleCallTermination(request, respond)
	default:
		return false
	}
}

func (session *Session) sendACK(call *imsCall) error {
	request := session.buildDialogRequest(call, "ACK", call.cseq)
	return session.writeRuntimePacket(session.callContext(), request)
}

// sendRejectedInviteACK acknowledges a non-2xx final response using the
// original INVITE transaction branch and request URI. Unlike a 2xx ACK this is
// part of the INVITE transaction; sending a dialog-style ACK with a new branch
// leaves the P-CSCF retransmitting the rejection and leaking transaction state.
func (session *Session) sendRejectedInviteACK(call *imsCall, response *sipResponse) error {
	if call == nil || response == nil || response.StatusCode < 300 {
		return nil
	}
	if session == nil || session.conn == nil {
		return errors.New("ims: SIP connection unavailable for rejected INVITE ACK")
	}
	session.callMu.Lock()
	call = session.dialogSnapshotLocked(call)
	session.callMu.Unlock()
	target := call.inviteTarget
	if target == "" {
		target = call.target
	}
	to := strings.TrimSpace(response.value("To"))
	if to == "" {
		to = call.to
	}
	lines := []string{
		"ACK " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), call.branch),
		"Max-Forwards: 70",
	}
	session.mu.Lock()
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	lines = append(lines, securityHeaders...)
	routes := call.routes
	if call.inviteFrom != "" {
		routes = call.inviteRoutes
	}
	for _, route := range routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+call.from,
		"To: "+to,
		"Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d ACK", call.cseq),
	)
	lines = session.appendPAccessNetworkInfoHeader(lines)
	lines = append(lines,
		"User-Agent: "+session.imsUserAgent(),
		"Content-Length: 0", "", "",
	)
	return session.writeRuntimePacket(session.callContext(), []byte(strings.Join(lines, "\r\n")))
}

func reliableProvisional(response *sipResponse) bool {
	if response == nil || strings.TrimSpace(response.value("RSeq")) == "" {
		return false
	}
	for _, token := range strings.Split(strings.ToLower(response.value("Require")), ",") {
		if strings.TrimSpace(token) == "100rel" {
			return true
		}
	}
	return false
}

func (session *Session) updateCallDialogFromResponse(call *imsCall, response *sipResponse) {
	if call == nil || response == nil {
		return
	}
	session.callMu.Lock()
	defer session.callMu.Unlock()
	call.to = response.value("To")
	call.remoteTag = headerParameter(call.to, "tag")
	if contact := headerURI(response.value("Contact")); contact != "" {
		call.target = contact
	}
	if recordRoutes := response.values("Record-Route"); len(recordRoutes) > 0 {
		call.routes = reverseStrings(recordRoutes)
	}
}

func (session *Session) sendPRACK(call *imsCall, response *sipResponse) {
	rseq := strings.TrimSpace(response.value("RSeq"))
	inviteCSeq := strings.TrimSpace(response.value("CSeq"))
	if rseq == "" || inviteCSeq == "" {
		return
	}
	key := rseq + "|" + inviteCSeq
	session.callMu.Lock()
	if call.pracked == nil {
		call.pracked = make(map[string]bool)
	}
	if call.pracked[key] || call.public.EndedAt != nil {
		session.callMu.Unlock()
		return
	}
	call.pracked[key] = true
	target, to, from := call.target, call.to, call.from
	routes := append([]string(nil), call.routes...)
	session.callMu.Unlock()

	session.mu.Lock()
	cseq := session.cseq
	session.cseq++
	session.mu.Unlock()
	branch, _ := randomHex(12)
	lines := []string{
		"PRACK " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	session.mu.Lock()
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	lines = append(lines, securityHeaders...)
	for _, route := range routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+from, "To: "+to, "Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d PRACK", cseq), "RAck: "+rseq+" "+inviteCSeq,
	)
	lines = session.appendPAccessNetworkInfoHeader(lines)
	lines = append(lines,
		"User-Agent: "+session.imsUserAgent(),
		"Content-Length: 0", "", "",
	)
	ctx, cancel := context.WithTimeout(session.refreshContext, 10*time.Second)
	defer cancel()
	result, err := session.exchangeRuntime(ctx, []byte(strings.Join(lines, "\r\n")), sipTransactionKey{callID: call.callID, cseq: cseq, method: "PRACK"})
	if err != nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		reason := "reliable provisional response could not be acknowledged"
		if err != nil {
			reason = err.Error()
		}
		session.finishCall(call.callID, "failed", 0, reason)
	}
}

func (session *Session) handleDialogOffer(request *sipRequest, respond func([]byte) error, call *imsCall) bool {
	session.callMu.Lock()
	transaction, replay, status := session.admitDialogOfferLocked(call, request, respond)
	if transaction != nil && session.request.Cellular != nil {
		if !session.queueCellularMediaJobLocked(call, func() { session.handleDialogOfferNow(request, call, transaction) }) {
			session.callMu.Unlock()
			session.completeDialogOffer(call, transaction, request, 488, nil)
			return true
		}
		session.callMu.Unlock()
		return true
	}
	session.callMu.Unlock()
	if status != 0 {
		session.respondCall(request, respond, status)
		return true
	}
	if len(replay) != 0 {
		_ = respond(replay)
		return true
	}
	if transaction == nil {
		return true
	} // matching transaction is already pending
	return session.handleDialogOfferNow(request, call, transaction)
}

func (session *Session) handleDialogOfferNow(request *sipRequest, call *imsCall, transaction *dialogOfferTransaction) bool {
	// Non-cellular receive sockets may run concurrently too. Serialize their
	// media commits without ever holding callMu or the initial INVITE replyMu.
	call.dialogOfferMu.Lock()
	defer call.dialogOfferMu.Unlock()
	session.callMu.Lock()
	ended := call.terminated || call.public.EndedAt != nil
	stale := transaction.key.sequence < call.mediaRemoteCSeq
	session.callMu.Unlock()
	if ended {
		session.completeDialogOffer(call, transaction, request, 481, nil)
		return true
	}
	if stale {
		session.completeDialogOffer(call, transaction, request, 500, nil)
		return true
	}
	var body []byte
	if len(request.Body) > 0 {
		if err := session.configureCallMediaWithCommit(call, transaction.ctx, request.Body, transaction.commitMedia); err != nil {
			session.completeDialogOffer(call, transaction, request, 488, nil)
			return true
		}
		body = call.media.answerSDP(session.localMediaIP())
		session.setCallMediaReady(call.callID)
	}
	if session.callWasTerminated(call.callID) && !transaction.mediaCommitted() {
		session.completeDialogOffer(call, transaction, request, 481, nil)
		return true
	}
	session.callMu.Lock()
	call.mediaRemoteCSeq = transaction.key.sequence
	session.callMu.Unlock()
	session.completeDialogOffer(call, transaction, request, 200, body)
	session.startSessionTimer(call, request.value("Session-Expires"))
	return true
}

func (session *Session) startSessionTimer(call *imsCall, header string) {
	value := strings.TrimSpace(strings.Split(header, ";")[0])
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 90 || seconds > 86400 || call == nil {
		return
	}
	ctx, cancel := context.WithCancel(session.callContext())
	session.callMu.Lock()
	if call.terminated || call.public.EndedAt != nil {
		session.callMu.Unlock()
		cancel()
		return
	}
	if call.sessionCancel != nil {
		call.sessionCancel()
	}
	call.sessionExpires = seconds
	call.sessionCancel = cancel
	session.callMu.Unlock()
	go func() {
		interval := time.Duration(seconds) * time.Second / 2
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				refreshContext, cancelRefresh := context.WithTimeout(ctx, 15*time.Second)
				refreshErr := session.sendDialogRequest(refreshContext, call, "UPDATE")
				cancelRefresh()
				if refreshErr != nil {
					session.finishCall(call.callID, "failed", 0, "SIP session refresh failed")
					return
				}
				timer.Reset(interval)
			}
		}
	}()
}

func (session *Session) sendDialogRequest(ctx context.Context, call *imsCall, method string) error {
	cseq := call.cseq
	if method == "BYE" || method == "UPDATE" {
		session.mu.Lock()
		cseq = session.cseq
		session.cseq++
		session.mu.Unlock()
	}
	request := session.buildDialogRequest(call, method, cseq)
	if method == "ACK" {
		return session.writeRuntimePacket(ctx, request)
	}
	response, err := session.exchangeRuntime(ctx, request, sipTransactionKey{callID: call.callID, cseq: cseq, method: method})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ims: SIP %s rejected with %d", method, response.StatusCode)
	}
	return nil
}

func (session *Session) buildDialogRequest(call *imsCall, method string, cseq uint32) []byte {
	session.callMu.Lock()
	call = session.dialogSnapshotLocked(call)
	session.callMu.Unlock()
	branch, _ := randomHex(12)
	if method == "CANCEL" {
		branch = call.branch
	}
	target := call.target
	if method == "CANCEL" && call.inviteTarget != "" {
		target = call.inviteTarget
	}
	to := call.to
	from, routes := call.from, call.routes
	if method == "CANCEL" && call.inviteFrom != "" {
		from, to, routes = call.inviteFrom, call.inviteTo, call.inviteRoutes
	}
	if to == "" {
		to = "<" + call.target + ">"
	}
	lines := []string{
		method + " " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	session.mu.Lock()
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	lines = append(lines, securityHeaders...)
	for _, route := range routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+from,
		"To: "+to,
		"Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d %s", cseq, method),
		"Supported: 100rel, timer",
		"User-Agent: "+session.imsUserAgent(),
	)
	if method != "CANCEL" {
		lines = session.appendPAccessNetworkInfoHeader(lines)
	}
	if method == "UPDATE" || method == "INVITE" {
		lines = append(lines, session.dialogContactHeader())
	}
	lines = append(lines, "Content-Length: 0", "", "")
	if method == "UPDATE" && call.sessionExpires > 0 {
		lines = append(lines[:len(lines)-3], fmt.Sprintf("Session-Expires: %d;refresher=uac", call.sessionExpires), "Content-Length: 0", "", "")
	}
	return []byte(strings.Join(lines, "\r\n"))
}

func (session *Session) localMediaIP() net.IP {
	var localAddress net.Addr
	if session.conn != nil {
		localAddress = session.conn.LocalAddr()
	}
	return addressIP(localAddress)
}

func buildSIPResponseWithBody(request *sipRequest, status int, tag string, body []byte, extraHeaders ...string) ([]byte, error) {
	reasons := map[int]string{180: "Ringing", 200: "OK", 481: "Call/Transaction Does Not Exist", 486: "Busy Here", 487: "Request Terminated", 488: "Not Acceptable Here", 491: "Request Pending", 500: "Server Internal Error"}
	reason := reasons[status]
	if reason == "" {
		return nil, errors.New("ims: unsupported call response status")
	}
	via := request.values("Via")
	from, to := request.value("From"), request.value("To")
	callID, cseq := request.value("Call-ID"), request.value("CSeq")
	if len(via) == 0 || from == "" || to == "" || callID == "" || cseq == "" {
		return nil, errors.New("ims: call request omitted a mandatory response header")
	}
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + tag
	}
	lines := []string{fmt.Sprintf("SIP/2.0 %d %s", status, reason)}
	for _, value := range via {
		lines = append(lines, "Via: "+value)
	}
	lines = append(lines, "From: "+from, "To: "+to, "Call-ID: "+callID, "CSeq: "+cseq)
	for _, header := range extraHeaders {
		if strings.TrimSpace(header) != "" {
			lines = append(lines, header)
		}
	}
	if value := strings.TrimSpace(request.value("Session-Expires")); value != "" && status >= 200 && status < 300 && (request.Method == "INVITE" || request.Method == "UPDATE") {
		lines = append(lines, "Supported: timer", "Session-Expires: "+value)
	}
	if len(body) > 0 {
		lines = append(lines, "Content-Type: application/sdp")
	}
	lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)), "", "")
	return append([]byte(strings.Join(lines, "\r\n")), body...), nil
}

func (session *Session) dialogContactHeader() string {
	if session == nil || session.conn == nil || strings.TrimSpace(session.identity.user) == "" {
		return ""
	}
	if session.imsRegisterOptions().ContactFormat == vowifi.IMSContactFormatGSMA {
		contact := "Contact: <sip:" + session.contactAddress() + `>;+g.3gpp.icsi-ref="` + mmtelFeatureTag + `"`
		if strings.TrimSpace(session.instanceID) != "" {
			contact += `;+sip.instance="<` + session.instanceID + `>"`
		}
		return contact
	}
	contact := "Contact: <sip:" + session.identity.user + "@" + session.contactAddress() + ";transport=" + session.transport + ">"
	if strings.TrimSpace(session.instanceID) != "" {
		contact += `;+sip.instance="<` + session.instanceID + `>"`
	}
	return contact + `;audio;+g.3gpp.icsi-ref="` + mmtelFeatureTag + `"`
}

func callTargetURI(number, domain string, profile vowifi.CarrierProfile) string {
	domain = strings.TrimSpace(domain)
	if profile.IMSDialURIScheme == "sip" {
		target := "sip:" + number + "@" + domain
		if profile.IMSUserEqPhone {
			target += ";user=phone"
		}
		return target
	}
	if strings.HasPrefix(number, "+") {
		return "tel:" + number
	}
	return "tel:" + number + ";phone-context=" + domain
}

// callOriginatingIdentitiesLocked selects only a number that IMS explicitly
// associated with this registration. 3GPP originating sessions use that
// public identity in both From and P-Preferred-Identity; some TAS deployments
// accept an IMSI IMPU at the P-CSCF and then terminate the session immediately.
// The fallback deliberately remains the registered IMPU and never derives a
// telephone number from IMSI digits.
func (session *Session) callOriginatingIdentitiesLocked(profile vowifi.CarrierProfile) (from, preferred, source string) {
	if number, numberSource, ok := vowifi.ExtractAssociatedMSISDN(session.evidence); ok {
		from = "sip:" + number + "@" + session.identity.domain
		if profile.IMSUserEqPhone {
			from += ";user=phone"
		}
		return from, "tel:" + number, numberSource
	}
	return session.identity.public, session.identity.public, "registered_impu"
}

func (session *Session) pAccessNetworkInfo() string {
	if session == nil {
		return ""
	}
	session.paniMu.RLock()
	defer session.paniMu.RUnlock()
	if session.paniResolved {
		return session.pani
	}
	return resolveSessionPAccessNetworkInfo(session.request.Identity, session.imsLogger())
}

func (session *Session) setPANI(value string) {
	session.paniMu.Lock()
	defer session.paniMu.Unlock()
	session.pani = value
	session.paniResolved = true
}

func (session *Session) appendPAccessNetworkInfoHeader(lines []string) []string {
	if pani := session.pAccessNetworkInfo(); pani != "" {
		return append(lines, "P-Access-Network-Info: "+pani)
	}
	return lines
}

func callResponseDiagnostic(response *sipResponse) string {
	if response == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if reason := safeSIPDiagnostic(response.Reason); reason != "" {
		parts = append(parts, reason)
	}
	for _, name := range []string{"Reason", "Warning"} {
		for _, value := range response.values(name) {
			if value = safeSIPDiagnostic(value); value != "" {
				parts = append(parts, name+": "+value)
			}
		}
	}
	return safeSIPDiagnostic(strings.Join(parts, "; "))
}

func (session *Session) logCallResponse(response *sipResponse, diagnostic string) {
	if session == nil || session.provider == nil || session.provider.config.Logger == nil || response == nil {
		return
	}
	session.provider.config.Logger.Info("IMS call response",
		"category", "call",
		"device_id", session.request.DeviceID,
		"carrier_profile", vowifi.ResolveCarrierProfile(session.request.Identity).ID,
		"sip_status", response.StatusCode,
		"diagnostic", diagnostic,
		"content_type", safeSIPDiagnostic(response.value("Content-Type")),
		"body_bytes", len(response.Body),
	)
}

func (session *Session) setCallState(id, state string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil && !call.terminated && call.public.EndedAt == nil {
		call.public.State = state
		if state == "active" && call.public.AnsweredAt == nil {
			now := time.Now().UTC()
			call.public.AnsweredAt = &now
		}
		if state != "ended" && state != "failed" {
			call.public.EndedAt = nil
		}
	}
	session.callMu.Unlock()
}

func (session *Session) setCallDiagnostic(id string, code int, reason string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil {
		call.public.SIPCode = code
		call.public.Reason = safeSIPDiagnostic(reason)
	}
	session.callMu.Unlock()
}

func (session *Session) setCallMediaReady(id string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil && !call.terminated && call.public.EndedAt == nil && call.media != nil {
		call.public.MediaReady = call.media.ready()
		call.public.Codec = call.media.Codec()
		session.beginCallRecordingLocked(call)
	}
	session.callMu.Unlock()
}

func (session *Session) CallMedia(_ context.Context, id string) (vowifi.CallMedia, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	call := session.calls[id]
	if call == nil {
		return nil, ErrCallNotFound
	}
	if call.terminated || call.public.State != "active" || call.media == nil || !call.media.ready() {
		return nil, ErrCallState
	}
	return call.media, nil
}

func (session *Session) finishCall(id, state string, code int, reason string) {
	now := time.Now().UTC()
	var media *rtpMedia
	session.callMu.Lock()
	if call := session.calls[id]; call != nil {
		media = call.media
		call.terminated = true
		call.public.MediaReady = false
		call.public.CanHold = false
		call.public.HoldPending = false
		if call.public.EndedAt == nil {
			call.public.State = state
			call.public.EndedAt = &now
		}
		if code != 0 {
			call.public.SIPCode = code
		}
		if reason = safeSIPDiagnostic(reason); reason != "" && call.public.Reason == "" {
			call.public.Reason = reason
		}
		if call.sessionCancel != nil {
			call.sessionCancel()
			call.sessionCancel = nil
		}
		if call.mediaCancel != nil {
			call.mediaCancel()
		}
	}
	session.callMu.Unlock()
	if media != nil {
		if state == "failed" {
			media.stopRecording("call_failed")
		}
		_ = media.Close()
	}
}

func validCallNumber(value string) bool {
	if len(value) < 2 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if character >= '0' && character <= '9' || index == 0 && character == '+' || character == '*' || character == '#' {
			continue
		}
		return false
	}
	return true
}

func identityNumber(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			value = value[start+1 : start+end]
		}
	}
	value = strings.TrimPrefix(value, "sip:")
	value = strings.TrimPrefix(value, "tel:")
	if at := strings.Index(value, "@"); at >= 0 {
		value = value[:at]
	}
	return strings.TrimSpace(value)
}

func headerParameter(value, name string) string {
	needle := ";" + strings.ToLower(name) + "="
	lower := strings.ToLower(value)
	index := strings.Index(lower, needle)
	if index < 0 {
		return ""
	}
	value = value[index+len(needle):]
	if end := strings.IndexAny(value, ";,> \t"); end >= 0 {
		value = value[:end]
	}
	return strings.Trim(value, `"`)
}

func headerURI(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start+1:], ">"); end >= 0 {
			return strings.TrimSpace(value[start+1 : start+1+end])
		}
	}
	if end := strings.Index(value, ";"); end >= 0 {
		value = value[:end]
	}
	if strings.HasPrefix(strings.ToLower(value), "sip:") || strings.HasPrefix(strings.ToLower(value), "tel:") {
		return strings.TrimSpace(value)
	}
	return ""
}

func reverseStrings(values []string) []string {
	result := append([]string(nil), values...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

var _ vowifi.CallController = (*Session)(nil)
var _ vowifi.CallMediaController = (*Session)(nil)
