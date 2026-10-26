package ims

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"vocat/internal/vowifi"
)

// SetCallHold changes one established dialog. Held remains a property of the
// active call, so existing PCM owners and cleanup leases stay attached.
func (session *Session) SetCallHold(ctx context.Context, id string, held bool) (vowifi.Call, error) {
	if err := ctx.Err(); err != nil {
		return vowifi.Call{}, err
	}
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallNotFound
	}
	if call.terminated || call.public.EndedAt != nil || call.public.State != "active" || call.media == nil || !call.media.ready() {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallState
	}
	if call.answerSent && !call.answerAcked {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallBusy
	}
	if call.localOfferPending || call.localOfferDrains >= 8 || !call.dialogOfferMu.TryLock() {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallBusy
	}
	if !call.media.dtmfMu.TryLock() {
		call.dialogOfferMu.Unlock()
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallBusy
	}
	defer call.media.dtmfMu.Unlock()
	for _, transaction := range call.dialogOffers {
		if transaction.pending {
			call.dialogOfferMu.Unlock()
			session.callMu.Unlock()
			return vowifi.Call{}, ErrCallBusy
		}
	}
	if call.public.Held == held {
		result := call.public
		call.dialogOfferMu.Unlock()
		session.callMu.Unlock()
		return result, nil
	}
	call.localOfferPending, call.public.HoldPending = true, true
	lifetime := session.callMediaContextLocked(call)
	dialog := session.dialogSnapshotLocked(call)
	session.callMu.Unlock()
	defer func() {
		session.callMu.Lock()
		call.localOfferPending, call.public.HoldPending = false, false
		session.callMu.Unlock()
		call.dialogOfferMu.Unlock()
	}()
	operation, cancel := context.WithTimeout(ctx, 15*time.Second)
	stop := context.AfterFunc(lifetime, cancel)
	defer func() { stop(); cancel() }()
	if err := session.verifyCellularAccess(operation); err != nil {
		return vowifi.Call{}, err
	}
	body := call.media.holdOfferSDP(session.localMediaIP(), held)
	session.mu.Lock()
	sequence := session.cseq
	if sequence == 0 || sequence >= 1<<31 {
		session.mu.Unlock()
		return vowifi.Call{}, ErrCallState
	}
	session.cseq++
	session.mu.Unlock()
	dialog.cseq = sequence
	packet := session.buildDialogRequest(dialog, "INVITE", sequence)
	packet = []byte(strings.Replace(string(packet), "Content-Length: 0\r\n\r\n", "Content-Type: application/sdp\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+string(body), 1))
	parsed, err := parseSIPPacket(packet)
	if err != nil || parsed.Request == nil {
		return vowifi.Call{}, ErrCallState
	}
	request := parsed.Request
	dialog.branch = strings.TrimPrefix(headerParameter(request.value("Via"), "branch"), "z9hG4bK")
	dialog.inviteTarget, dialog.inviteFrom, dialog.inviteTo = dialog.target, dialog.from, dialog.to
	dialog.inviteRoutes = append([]string(nil), dialog.routes...)
	key := sipTransactionKey{callID: id, cseq: sequence, method: "INVITE"}
	responses := make(chan *sipResponse, 8)
	session.transactionsMu.Lock()
	if session.transactions[key] != nil {
		session.transactionsMu.Unlock()
		return vowifi.Call{}, ErrCallBusy
	}
	session.transactions[key] = responses
	session.transactionsMu.Unlock()
	session.callMu.Lock()
	call.localOfferDrains++
	session.callMu.Unlock()
	// Retain transaction identity after returning to ACK duplicate or late finals.
	// A late acceptance after a canceled/unknown change ends this exact dialog.
	accepted := false
	defer func() { go session.drainHoldResponses(call, dialog, request, key, responses, accepted) }()
	if err := session.writeRuntimePacket(operation, packet); err != nil {
		session.failHoldDialog(call, "re-INVITE delivery is unconfirmed")
		return vowifi.Call{}, err
	}
	retransmit := time.NewTimer(500 * time.Millisecond)
	defer retransmit.Stop()
	interval, provisional := 500*time.Millisecond, false
	var earlyAnswer []byte
	for {
		select {
		case <-operation.Done():
			session.failHoldDialog(call, "re-INVITE result is unconfirmed")
			return vowifi.Call{}, operation.Err()
		case <-retransmit.C:
			if session.transport == "udp" && !provisional {
				if err := session.writeRuntimePacket(operation, packet); err != nil {
					session.failHoldDialog(call, "re-INVITE retransmission failed")
					return vowifi.Call{}, err
				}
				interval = min(interval*2, 4*time.Second)
				retransmit.Reset(interval)
			}
		case response := <-responses:
			if !matchesHoldResponse(request, response) {
				continue
			}
			if response.StatusCode < 200 {
				provisional = true
				if reliableProvisional(response) {
					if len(response.Body) != 0 && validHoldSDPContentType(response) {
						earlyAnswer = append([]byte(nil), response.Body...)
					}
					session.sendPRACK(dialog, response)
				}
				continue
			}
			if response.StatusCode >= 300 {
				_ = session.sendRejectedInviteACK(dialog, response)
				if response.StatusCode == 408 || response.StatusCode == 481 {
					session.failHoldDialog(call, "IMS dialog no longer available")
				}
				return vowifi.Call{}, fmt.Errorf("ims: hold change rejected with %d", response.StatusCode)
			}
			accepted = true
			ackErr := session.ackHoldResponse(dialog, response)
			session.callMu.Lock()
			call.target = dialog.target // A successful target refresh also governs cleanup after bad SDP.
			session.callMu.Unlock()
			if err := ackErr; err != nil {
				session.failHoldDialog(call, "re-INVITE ACK failed")
				return vowifi.Call{}, err
			}
			answer := response.Body
			if len(answer) != 0 && !validHoldSDPContentType(response) {
				session.failHoldDialog(call, "re-INVITE answer has an invalid content type")
				return vowifi.Call{}, ErrCallState
			}
			if len(answer) == 0 {
				answer = earlyAnswer
			}
			if err := validateHoldAnswer(body, answer, held); err != nil {
				session.failHoldDialog(call, "re-INVITE SDP answer is invalid")
				return vowifi.Call{}, err
			}
			if err := session.configureCallMediaWithCommit(call, operation, answer, nil); err != nil {
				session.failHoldDialog(call, "re-INVITE media could not be committed")
				return vowifi.Call{}, err
			}
			call.media.setLocalHold(held)
			session.callMu.Lock()
			if call.terminated || operation.Err() != nil {
				session.callMu.Unlock()
				session.failHoldDialog(call, "call ended during re-INVITE")
				return vowifi.Call{}, ErrCallState
			}
			call.target = dialog.target
			call.public.Held, call.public.HoldPending = held, false
			call.public.CanHold, call.public.MediaReady = true, call.media.ready()
			result := call.public
			session.callMu.Unlock()
			session.startSessionTimer(call, response.value("Session-Expires"))
			return result, nil
		}
	}
}

func matchesHoldResponse(request *sipRequest, response *sipResponse) bool {
	if response == nil {
		return false
	}
	for _, name := range []string{"From", "To", "Call-ID", "CSeq"} {
		if len(response.values(name)) != 1 {
			return false
		}
	}
	return response.value("Call-ID") == request.value("Call-ID") && response.value("CSeq") == request.value("CSeq") &&
		headerURI(response.value("From")) == headerURI(request.value("From")) && headerURI(response.value("To")) == headerURI(request.value("To")) &&
		headerParameter(response.value("From"), "tag") == headerParameter(request.value("From"), "tag") &&
		headerParameter(response.value("To"), "tag") == headerParameter(request.value("To"), "tag") &&
		headerParameter(response.value("Via"), "branch") == headerParameter(request.value("Via"), "branch")
}

func validHoldSDPContentType(response *sipResponse) bool {
	return len(response.values("Content-Type")) == 1 && len(response.values("Content-Encoding")) == 0 &&
		strings.EqualFold(strings.TrimSpace(strings.Split(response.value("Content-Type"), ";")[0]), "application/sdp")
}

func (session *Session) ackHoldResponse(dialog *imsCall, response *sipResponse) error {
	if contact := headerURI(response.value("Contact")); len(response.values("Contact")) == 1 && contact != "" {
		dialog.target = contact
	}
	ctx, cancel := context.WithTimeout(session.callContext(), 5*time.Second)
	defer cancel()
	return session.writeRuntimePacket(ctx, session.buildDialogRequest(dialog, "ACK", dialog.cseq))
}

func (session *Session) failHoldDialog(call *imsCall, reason string) {
	session.finishCall(call.callID, "failed", 0, reason)
	session.endAnsweredDialog(call)
}

func (session *Session) drainHoldResponses(call, dialog *imsCall, request *sipRequest, key sipTransactionKey, responses chan *sipResponse, accepted bool) {
	defer func() {
		session.transactionsMu.Lock()
		if session.transactions[key] == responses {
			delete(session.transactions, key)
		}
		session.transactionsMu.Unlock()
		session.callMu.Lock()
		call.localOfferDrains--
		session.callMu.Unlock()
	}()
	timer := time.NewTimer(32 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-session.callContext().Done():
			return
		case <-timer.C:
			return
		case response := <-responses:
			if !matchesHoldResponse(request, response) || response.StatusCode < 200 {
				continue
			}
			if response.StatusCode >= 300 {
				_ = session.sendRejectedInviteACK(dialog, response)
				continue
			}
			_ = session.ackHoldResponse(dialog, response)
			if !accepted {
				session.endAnsweredDialog(dialog)
			}
		}
	}
}

func (media *rtpMedia) holdOfferSDP(local net.IP, held bool) []byte {
	body := media.answerSDP(local)
	direction := "sendrecv"
	if held {
		direction = "sendonly"
	}
	return replaceSDPDirection(body, direction)
}

func validateHoldAnswer(offer, answer []byte, held bool) error {
	_, _, formats, mappings, err := parseAudioSDP(answer)
	if err != nil {
		return err
	}
	_, _, offered, offeredMaps, err := parseAudioSDP(offer)
	if err != nil {
		return err
	}
	for _, format := range formats {
		found := false
		for _, expected := range offered {
			if expected == format {
				found = true
				break
			}
		}
		pt, _ := strconv.Atoi(format)
		mapping := func(values map[int]string) string {
			value := values[pt]
			if value == "" && pt == 0 {
				value = "PCMU/8000"
			}
			if value == "" && pt == 8 {
				value = "PCMA/8000"
			}
			return strings.ToUpper(strings.TrimSuffix(value, "/1"))
		}
		if !found || mapping(mappings) != mapping(offeredMaps) {
			return errors.New("ims: hold answer changed an unoffered format")
		}
	}
	direction, err := audioSDPDirection(answer)
	if err != nil {
		return err
	}
	if held && direction != "recvonly" && direction != "inactive" {
		return errors.New("ims: hold answer has an incompatible direction")
	}
	return nil
}

func audioSDPDirection(body []byte) (string, error) {
	selected, err := selectAudioSDP(body)
	if err != nil {
		return "", err
	}
	session, media := "", ""
	inMedia := false
	for _, line := range strings.Split(string(selected), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "m=") {
			inMedia = true
		}
		if line != "a=sendrecv" && line != "a=sendonly" && line != "a=recvonly" && line != "a=inactive" {
			continue
		}
		target := &session
		if inMedia {
			target = &media
		}
		if *target != "" {
			return "", errors.New("ims: duplicate SDP direction")
		}
		*target = strings.TrimPrefix(line, "a=")
	}
	if media != "" {
		return media, nil
	}
	if session != "" {
		return session, nil
	}
	return "sendrecv", nil
}

func replaceSDPDirection(body []byte, direction string) []byte {
	for _, old := range []string{"sendrecv", "sendonly", "recvonly", "inactive"} {
		if strings.Contains(string(body), "a="+old+"\r\n") {
			return []byte(strings.Replace(string(body), "a="+old+"\r\n", "a="+direction+"\r\n", 1))
		}
	}
	return body
}

func (media *rtpMedia) setLocalHold(held bool) {
	media.writeMu.Lock()
	media.mu.Lock()
	media.localHeld, media.pending = held, nil
	media.mu.Unlock()
	for i := 0; i < cap(media.downlink); i++ {
		select {
		case <-media.downlink:
		default:
			media.writeMu.Unlock()
			media.FlushDTMF()
			return
		}
	}
	media.writeMu.Unlock()
	media.FlushDTMF()
}

var _ vowifi.CallHoldController = (*Session)(nil)
