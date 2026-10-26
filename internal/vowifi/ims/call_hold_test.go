package ims

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func holdTestSession(t *testing.T) (*Session, *callTestConn, *imsCall) {
	t.Helper()
	s, conn := newCallTestSession(t)
	m, _ := dtmfMedia(t)
	call := &imsCall{callID: "exact-dialog", target: "sip:peer@ims.example.test", from: "<sip:subscriber@ims.example.test>;tag=local", to: "<sip:peer@ims.example.test>;tag=remote", media: m, public: vowifi.Call{ID: "exact-dialog", State: "active", MediaReady: true, StartedAt: time.Now()}}
	s.calls[call.callID] = call
	return s, conn, call
}

func TestHoldResumeReinviteCommitsOnlyMatchingDialogAndACK(t *testing.T) {
	s, conn, call := holdTestSession(t)
	conn.onWrite = func(wire []byte) {
		packet, err := parseSIPPacket(wire)
		if err != nil || packet.Request == nil || packet.Request.Method != "INVITE" {
			return
		}
		r := packet.Request
		direction, err := audioSDPDirection(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		answer := []byte(callTestSDP)
		if direction == "sendonly" {
			answer = replaceSDPDirection(answer, "recvonly")
		}
		response, err := buildSIPResponseWithBody(r, 200, "remote", answer)
		if err != nil {
			t.Error(err)
			return
		}
		parsed, err := parseSIPPacket(response)
		if err != nil {
			t.Error(err)
			return
		}
		forged := *parsed.Response
		forged.Headers = make(map[string][]string)
		for k, v := range parsed.Response.Headers {
			forged.Headers[k] = append([]string(nil), v...)
		}
		forged.Headers["to"] = []string{"<sip:peer@ims.example.test>;tag=other"}
		s.dispatchPacket(sipPacket{Response: &forged}, nil)
		s.dispatchPacket(parsed, nil)
	}
	for _, held := range []bool{true, false} {
		result, err := s.SetCallHold(context.Background(), call.callID, held)
		if err != nil || result.Held != held || result.HoldPending || result.State != "active" || !result.MediaReady {
			t.Fatal("hold/resume did not preserve active media", result, err)
		}
		call.media.mu.RLock()
		localHeld := call.media.localHeld
		call.media.mu.RUnlock()
		if localHeld != held {
			t.Fatal("media state diverged from ACKed dialog")
		}
	}
	if conn.count("INVITE") != 2 || conn.count("ACK") != 2 {
		t.Fatal("missing exact dialog re-INVITE/ACK")
	}
	for _, request := range conn.requests("ACK") {
		if request.value("Call-ID") != call.callID || !strings.Contains(request.value("To"), "tag=remote") {
			t.Fatal("ACK escaped original dialog")
		}
	}
	if _, err := s.SetCallHold(context.Background(), "different-dialog", true); !errors.Is(err, ErrCallNotFound) {
		t.Fatal(err)
	}
}

func TestHoldRejectedOfferPreservesExistingCallAndMedia(t *testing.T) {
	s, conn, call := holdTestSession(t)
	conn.onWrite = func(wire []byte) {
		packet, err := parseSIPPacket(wire)
		if err != nil || packet.Request == nil || packet.Request.Method != "INVITE" {
			return
		}
		response, _ := buildSIPResponseWithBody(packet.Request, 491, "remote", nil)
		parsed, _ := parseSIPPacket(response)
		s.dispatchPacket(parsed, nil)
	}
	if _, err := s.SetCallHold(context.Background(), call.callID, true); err == nil {
		t.Fatal("rejected offer committed")
	}
	if got := s.Calls()[0]; got.State != "active" || got.Held || got.HoldPending || !got.MediaReady {
		t.Fatal("rejected re-INVITE damaged existing call", got)
	}
	if conn.count("ACK") != 1 {
		t.Fatal("failed re-INVITE was not ACKed")
	}
}

func TestActiveMediaDrainDoesNotWaitForLiveRTCPToClose(t *testing.T) {
	m, _ := dtmfMedia(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.DrainCellularRoutes(ctx); err != nil {
		t.Fatal("active route drain waited for call termination", err)
	}
	if m.control.Closing() || !m.ready() {
		t.Fatal("draining retired routes closed active control/media")
	}
}
