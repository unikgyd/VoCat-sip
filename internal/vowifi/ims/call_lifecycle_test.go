package ims

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

// All peers and identities in this file are synthetic. No modem or carrier is
// needed, including for the cancellation/late-answer exchange.
type callTestConn struct {
	mu         sync.Mutex
	packets    [][]byte
	onWrite    func([]byte)
	failInvite bool
}

func (c *callTestConn) Read([]byte) (int, error) { return 0, net.ErrClosed }
func (c *callTestConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.packets = append(c.packets, append([]byte(nil), b...))
	hook := c.onWrite
	fail := c.failInvite && strings.HasPrefix(string(b), "INVITE ")
	c.mu.Unlock()
	if hook != nil {
		hook(b)
	}
	if fail {
		return len(b), io.ErrUnexpectedEOF
	}
	return len(b), nil
}
func (c *callTestConn) Close() error { return nil }
func (c *callTestConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15060}
}
func (c *callTestConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15061}
}
func (c *callTestConn) SetDeadline(time.Time) error      { return nil }
func (c *callTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *callTestConn) SetWriteDeadline(time.Time) error { return nil }
func (c *callTestConn) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.packets {
		if strings.HasPrefix(string(b), method+" ") {
			n++
		}
	}
	return n
}

func (c *callTestConn) requests(method string) []*sipRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var requests []*sipRequest
	for _, b := range c.packets {
		packet, err := parseSIPPacket(b)
		if err == nil && packet.Request != nil && packet.Request.Method == method {
			requests = append(requests, packet.Request)
		}
	}
	return requests
}

const callTestSDP = "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 19000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n"

func acceptCallTestControls(s *Session, conn *callTestConn) {
	conn.onWrite = func(b []byte) {
		packet, err := parseSIPPacket(b)
		if err == nil && packet.Request != nil && (packet.Request.Method == "BYE" || packet.Request.Method == "CANCEL") {
			response, _ := buildSIPResponseWithBody(packet.Request, 200, "remote", nil)
			parsed, _ := parseSIPPacket(response)
			s.dispatchPacket(parsed, nil)
		}
	}
}

func waitCallTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("synthetic call event timed out")
	}
}

func incomingTestRequest(t *testing.T, invite *sipRequest, method string, seq uint32, tagged bool) *sipRequest {
	t.Helper()
	to := invite.value("To")
	if tagged {
		to += ";tag=local"
	}
	packet, err := parseSIPPacket([]byte(strings.Join([]string{
		method + " " + invite.URI + " SIP/2.0", "Via: " + invite.value("Via"),
		"From: " + invite.value("From"), "To: " + to, "Call-ID: " + invite.value("Call-ID"),
		fmt.Sprintf("CSeq: %d %s", seq, method), "Content-Length: 0", "", "",
	}, "\r\n")))
	if err != nil || packet.Request == nil {
		t.Fatalf("test %s: %v", method, err)
	}
	return packet.Request
}

func newCallTestSession(t *testing.T) (*Session, *callTestConn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn := &callTestConn{}
	s := &Session{
		provider: &Provider{config: Config{TransactionTimeout: 100 * time.Millisecond}},
		conn:     conn, transport: "udp", fromTag: "local",
		identity: identitySet{domain: "ims.example.test", user: "subscriber", public: "sip:subscriber@ims.example.test"},
		endpoint: pcscfEndpoint{host: "127.0.0.1", port: 15061},
		cseq:     1, calls: make(map[string]*imsCall), transactions: make(map[sipTransactionKey]chan *sipResponse),
		refreshContext: ctx,
	}
	t.Cleanup(func() {
		cancel()
		s.callMu.Lock()
		defer s.callMu.Unlock()
		for _, call := range s.calls {
			if call.media != nil {
				_ = call.media.Close()
			}
		}
	})
	return s, conn
}

func incomingTestInvite(t *testing.T, id string) *sipRequest {
	t.Helper()
	packet, err := parseSIPPacket([]byte(strings.Join([]string{
		"INVITE sip:subscriber@ims.example.test SIP/2.0",
		"Via: SIP/2.0/UDP 127.0.0.1:15061;branch=z9hG4bK-" + id,
		"From: <tel:+15550000001>;tag=remote", "To: <sip:subscriber@ims.example.test>",
		"Call-ID: " + id, "CSeq: 1 INVITE", "Content-Length: 0", "", "",
	}, "\r\n")))
	if err != nil || packet.Request == nil {
		t.Fatalf("test INVITE: %v", err)
	}
	return packet.Request
}

func TestTerminalCallCannotBeReactivated(t *testing.T) {
	s, _ := newCallTestSession(t)
	s.calls["done"] = &imsCall{public: vowifi.Call{ID: "done", State: "ringing"}}
	s.finishCall("done", "ended", 0, "local hangup")
	ended := *s.Calls()[0].EndedAt
	s.setCallState("done", "active")
	s.setCallMediaReady("done")
	s.finishCall("done", "failed", 500, "late error")
	call := s.Calls()[0]
	if call.State != "ended" || call.AnsweredAt != nil || call.EndedAt == nil || !call.EndedAt.Equal(ended) || call.MediaReady {
		t.Fatalf("terminal state changed after a late event: %#v", call)
	}
}

func TestIncomingInviteRetransmissionKeepsCallAndReplaysResponse(t *testing.T) {
	s, _ := newCallTestSession(t)
	req := incomingTestInvite(t, "repeat")
	var replies []string
	respond := func(b []byte) error { replies = append(replies, string(b)); return nil }
	s.handleCallRequest(req, respond)
	original := s.calls["repeat"]
	s.handleCallRequest(req, respond)
	if s.calls["repeat"] != original || len(replies) != 2 || replies[0] != replies[1] {
		t.Fatal("retransmitted INVITE replaced its call/media or response")
	}
	if _, err := s.AnswerCall(context.Background(), "repeat"); err != nil {
		t.Fatal(err)
	}
	s.handleCallRequest(req, respond)
	if len(replies) != 4 || replies[2] != replies[3] {
		t.Fatal("answered INVITE retransmission did not replay the same final response")
	}
}

func TestIncomingBusyIsPerSession(t *testing.T) {
	s, _ := newCallTestSession(t)
	other, _ := newCallTestSession(t)
	s.handleCallRequest(incomingTestInvite(t, "first"), func([]byte) error { return nil })
	var response string
	s.handleCallRequest(incomingTestInvite(t, "second"), func(b []byte) error { response = string(b); return nil })
	if !strings.HasPrefix(response, "SIP/2.0 486 ") || len(s.Calls()) != 1 {
		t.Fatalf("same module admitted another call: %s; %#v", response, s.Calls())
	}
	other.handleCallRequest(incomingTestInvite(t, "independent"), func([]byte) error { return nil })
	if len(other.Calls()) != 1 || other.Calls()[0].State != "ringing" {
		t.Fatal("busy on one module blocked a different module")
	}
}

func TestIncomingAnswerHasSingleOwner(t *testing.T) {
	s, _ := newCallTestSession(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	s.handleCallRequest(incomingTestInvite(t, "race"), func(b []byte) error {
		if strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			select {
			case entered <- struct{}{}:
			case <-release:
			}
			<-release
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, err := s.AnswerCall(context.Background(), "race"); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first answer did not enter writer")
	}
	second := make(chan error, 1)
	go func() { _, err := s.AnswerCall(context.Background(), "race"); second <- err }()
	select {
	case err := <-second:
		if !errors.Is(err, ErrCallState) {
			t.Fatalf("second answer = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("another answer was admitted or blocked behind network I/O")
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCancelledAnswerDoesNotSignal(t *testing.T) {
	s, _ := newCallTestSession(t)
	answers := 0
	s.handleCallRequest(incomingTestInvite(t, "cancel-answer"), func(b []byte) error {
		if strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			answers++
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.AnswerCall(ctx, "cancel-answer"); !errors.Is(err, context.Canceled) || answers != 0 {
		t.Fatalf("cancelled answer sent a response: err=%v answers=%d", err, answers)
	}
}

func TestCancelledOutgoingLateAnswerIsAckedAndHungUp(t *testing.T) {
	s, conn := newCallTestSession(t)
	conn.onWrite = func(b []byte) {
		packet, err := parseSIPPacket(b)
		if err == nil && packet.Request != nil && packet.Request.Method == "BYE" {
			response, _ := buildSIPResponseWithBody(packet.Request, 200, "remote", nil)
			parsed, _ := parseSIPPacket(response)
			s.dispatchPacket(parsed, nil)
		}
	}
	media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	call := &imsCall{
		public: vowifi.Call{ID: "late", State: "dialing", Direction: "outgoing"},
		callID: "late", cseq: 1, from: "<sip:subscriber@ims.example.test>;tag=local", to: "<tel:+15550000002>",
		target: "tel:+15550000002", inviteTarget: "tel:+15550000002", branch: "original", media: media,
		responses: make(chan *sipResponse, 8), terminated: true,
	}
	s.calls[call.callID] = call
	s.finishCall(call.callID, "ended", 0, "local hangup")
	key := sipTransactionKey{callID: call.callID, cseq: 1, method: "INVITE"}
	s.transactions[key] = call.responses
	go s.watchOutgoingCall(call, key)
	call.responses <- &sipResponse{StatusCode: 200, Headers: map[string][]string{
		"to": {"<tel:+15550000002>;tag=remote"}, "contact": {"<sip:peer@127.0.0.1:15061>"},
	}, Body: []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 19000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n")}
	deadline := time.Now().Add(time.Second)
	for conn.count("BYE") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if conn.count("ACK") != 1 || conn.count("BYE") != 1 || s.Calls()[0].State != "ended" || s.Calls()[0].MediaReady {
		t.Fatalf("late answer was not drained: ACK=%d BYE=%d calls=%#v", conn.count("ACK"), conn.count("BYE"), s.Calls())
	}
}

func TestConcurrentDialAdmitsOneCallPerModule(t *testing.T) {
	s, conn := newCallTestSession(t)
	start, results := make(chan struct{}), make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		go func() { <-start; _, err := s.DialCall(context.Background(), "+15550000002"); results <- err }()
	}
	close(start)
	success, busy := 0, 0
	for i := 0; i < cap(results); i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrCallBusy) {
			busy++
		} else {
			t.Fatal(err)
		}
	}
	requests := conn.requests("INVITE")
	if success != 1 || busy != 15 || len(requests) == 0 || len(s.Calls()) != 1 {
		t.Fatalf("admission success=%d busy=%d INVITE=%d", success, busy, conn.count("INVITE"))
	}
	// This is an unanswered UDP call. Under parallel-suite/race scheduling a
	// legitimate T1 retransmission may precede the last rejected goroutine.
	// Assert one admitted INVITE transaction, not a scheduling-dependent wire
	// count, while still rejecting a second dialog, branch, CSeq or SDP offer.
	first := requests[0]
	for _, request := range requests[1:] {
		if !sameInviteTransaction(first, request) || request.URI != first.URI ||
			request.value("From") != first.value("From") || request.value("To") != first.value("To") ||
			string(request.Body) != string(first.Body) {
			t.Fatal("concurrent dial admitted another transaction instead of retransmitting")
		}
	}
}

func TestCancelledDialDoesNotSignal(t *testing.T) {
	s, conn := newCallTestSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.DialCall(ctx, "+15550000002"); !errors.Is(err, context.Canceled) || conn.count("INVITE") != 0 || len(s.Calls()) != 0 {
		t.Fatalf("cancelled dial: %v, INVITE=%d calls=%#v", err, conn.count("INVITE"), s.Calls())
	}
}

func TestCancelWaitsForProvisionalAndPreservesOriginalInvite(t *testing.T) {
	s, conn := newCallTestSession(t)
	acceptCallTestControls(s, conn)
	public, err := s.DialCall(context.Background(), "+15550000002")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HangupCall(context.Background(), public.ID); err != nil {
		t.Fatal(err)
	}
	if conn.count("CANCEL") != 0 || s.Calls()[0].State != "ended" {
		t.Fatal("CANCEL sent before 1xx or local hangup waited")
	}
	time.Sleep(650 * time.Millisecond)
	if conn.count("INVITE") != 1 {
		t.Fatal("INVITE retransmitted after local cancellation")
	}
	s.callMu.Lock()
	call := s.calls[public.ID]
	s.callMu.Unlock()
	call.responses <- &sipResponse{StatusCode: 180, Headers: map[string][]string{
		"to": {"<tel:+15550000002>;tag=early"}, "contact": {"<sip:changed@127.0.0.1:19001>"},
		"record-route": {"<sip:changed-route.example.test;lr>"},
	}}
	waitCallTest(t, func() bool { return conn.count("CANCEL") == 1 })
	invite, cancel := conn.requests("INVITE")[0], conn.requests("CANCEL")[0]
	if invite.URI != cancel.URI {
		t.Fatal("CANCEL changed request URI")
	}
	for _, name := range []string{"Via", "From", "To", "Call-ID", "Route"} {
		if invite.value(name) != cancel.value(name) {
			t.Fatalf("CANCEL changed original %s: %q != %q", name, invite.value(name), cancel.value(name))
		}
	}
	call.responses <- &sipResponse{StatusCode: 487, Headers: map[string][]string{"to": {"<tel:+15550000002>;tag=early"}}}
	waitCallTest(t, func() bool { return conn.count("ACK") == 1 })
	if s.Calls()[0].State != "ended" {
		t.Fatal("487 changed cancelled call result")
	}
	if _, err := s.DialCall(context.Background(), "+15550000003"); err != nil {
		t.Fatalf("completed CANCEL did not release this module: %v", err)
	}
}

func TestOutgoingAnswerRetransmissionAndForkAreDrained(t *testing.T) {
	s, conn := newCallTestSession(t)
	acceptCallTestControls(s, conn)
	public, err := s.DialCall(context.Background(), "+15550000002")
	if err != nil {
		t.Fatal(err)
	}
	s.callMu.Lock()
	call := s.calls[public.ID]
	s.callMu.Unlock()
	answer := func(tag, target string) *sipResponse {
		return &sipResponse{StatusCode: 200,
			Headers: map[string][]string{"to": {"<tel:+15550000002>;tag=" + tag}, "contact": {"<sip:" + target + "@127.0.0.1:15061>"}}, Body: []byte(callTestSDP)}
	}
	call.responses <- answer("selected", "first")
	waitCallTest(t, func() bool { return len(s.Calls()) == 1 && s.Calls()[0].State == "active" })
	call.responses <- answer("selected", "first")
	call.responses <- answer("other", "second")
	call.responses <- answer("other", "second")
	waitCallTest(t, func() bool { return conn.count("ACK") == 4 && conn.count("BYE") == 1 })
	bye := conn.requests("BYE")[0]
	if bye.URI != "sip:second@127.0.0.1:15061" || headerParameter(bye.value("To"), "tag") != "other" {
		t.Fatalf("fork cleanup used selected dialog: %#v", bye)
	}
	if err := s.HangupCall(context.Background(), public.ID); err != nil {
		t.Fatal(err)
	}
	if conn.count("BYE") != 2 {
		t.Fatal("selected dialog did not hang up exactly once")
	}
	if err := s.HangupCall(context.Background(), public.ID); err != nil || conn.count("BYE") != 2 {
		t.Fatal("duplicate hangup signalled again")
	}
}

func TestIncomingCancelBeforeAnswerEndsInvite(t *testing.T) {
	s, _ := newCallTestSession(t)
	invite := incomingTestInvite(t, "cancel-incoming")
	var inviteReplies []string
	s.handleCallRequest(invite, func(b []byte) error { inviteReplies = append(inviteReplies, string(b)); return nil })
	cancel := incomingTestRequest(t, invite, "CANCEL", 1, false)
	var cancelReply string
	s.handleCallRequest(cancel, func(b []byte) error { cancelReply = string(b); return nil })
	if !strings.HasPrefix(cancelReply, "SIP/2.0 200 ") || len(inviteReplies) != 2 || !strings.HasPrefix(inviteReplies[1], "SIP/2.0 487 ") {
		t.Fatalf("CANCEL = %s; INVITE=%#v", cancelReply, inviteReplies)
	}
	if _, err := s.AnswerCall(context.Background(), "cancel-incoming"); !errors.Is(err, ErrCallState) {
		t.Fatalf("answered cancelled INVITE: %v", err)
	}
	s.handleCallRequest(invite, func(b []byte) error {
		if string(b) != inviteReplies[1] {
			t.Fatal("cancelled INVITE response changed")
		}
		return nil
	})
}

func TestIncomingCancelAfterAnswerDoesNotUndo200(t *testing.T) {
	s, _ := newCallTestSession(t)
	invite := incomingTestInvite(t, "answered-cancel")
	invite.Body = []byte(callTestSDP)
	var inviteReplies []string
	s.handleCallRequest(invite, func(b []byte) error { inviteReplies = append(inviteReplies, string(b)); return nil })
	if _, err := s.AnswerCall(context.Background(), "answered-cancel"); err != nil {
		t.Fatal(err)
	}
	s.handleCallRequest(incomingTestRequest(t, invite, "CANCEL", 1, false), func(b []byte) error {
		if !strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			t.Fatal(string(b))
		}
		return nil
	})
	if s.Calls()[0].State != "active" || len(inviteReplies) != 2 {
		t.Fatalf("late CANCEL undid accepted INVITE: %#v", s.Calls())
	}
	s.handleCallRequest(incomingTestRequest(t, invite, "ACK", 1, true), func([]byte) error { t.Fatal("ACK got a response"); return nil })
	s.callMu.Lock()
	acked := s.calls["answered-cancel"].answerAcked
	s.callMu.Unlock()
	if !acked {
		t.Fatal("valid ACK was not matched")
	}
}

func TestIncomingByeChecksTagsSequenceAndReplay(t *testing.T) {
	s, _ := newCallTestSession(t)
	invite := incomingTestInvite(t, "bye-check")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), "bye-check"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		seq    uint32
		tagged bool
		status int
	}{{1, true, 481}, {2, false, 481}, {2, true, 200}, {2, true, 200}} {
		request := incomingTestRequest(t, invite, "BYE", tc.seq, tc.tagged)
		s.handleCallRequest(request, func(b []byte) error {
			if !strings.HasPrefix(string(b), fmt.Sprintf("SIP/2.0 %d ", tc.status)) {
				t.Fatalf("BYE %v: %s", tc, b)
			}
			return nil
		})
		if tc.status == 481 && s.Calls()[0].State != "active" {
			t.Fatal("invalid BYE ended call")
		}
	}
	if s.Calls()[0].State != "ended" {
		t.Fatal("valid BYE did not end call")
	}
}

func TestIncomingAnswerFailureIsNotRetriedByAnotherOwner(t *testing.T) {
	s, conn := newCallTestSession(t)
	acceptCallTestControls(s, conn)
	answers := 0
	s.handleCallRequest(incomingTestInvite(t, "uncertain-answer"), func(b []byte) error {
		if strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			answers++
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if _, err := s.AnswerCall(context.Background(), "uncertain-answer"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if _, err := s.AnswerCall(context.Background(), "uncertain-answer"); !errors.Is(err, ErrCallState) || answers != 1 {
		t.Fatalf("unknown answer retried: %v count=%d", err, answers)
	}
	waitCallTest(t, func() bool { return conn.count("BYE") == 1 })
}

func TestCallWriteHonorsCancellationAndDoesNotPoisonNextWrite(t *testing.T) {
	s, _ := newCallTestSession(t)
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	s.conn = client
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.writeRuntimePacket(ctx, []byte("blocked")); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("blocked write: %v", err)
	}
	read := make(chan error, 1)
	go func() {
		b := make([]byte, 4)
		_, err := io.ReadFull(peer, b)
		if string(b) != "next" && err == nil {
			err = errors.New("bad packet")
		}
		read <- err
	}()
	if err := s.writeRuntimePacket(context.Background(), []byte("next")); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

func TestCallWriterLockHonorsCancellation(t *testing.T) {
	s, conn := newCallTestSession(t)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.writeRuntimePacket(ctx, []byte("must not send")); !errors.Is(err, context.DeadlineExceeded) || len(conn.packets) != 0 {
		t.Fatalf("writer lock wait: %v", err)
	}
}

func TestClosedCallMediaCannotBeReconfigured(t *testing.T) {
	media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := media.configureRemote([]byte(callTestSDP)); err != nil {
		t.Fatal(err)
	}
	_ = media.Close()
	if media.ready() || !errors.Is(media.configureRemote([]byte(callTestSDP)), io.EOF) {
		t.Fatal("closed RTP media was reactivated")
	}
}

func TestPendingInviteKeepsAdmissionAndHasBoundedHistory(t *testing.T) {
	s, _ := newCallTestSession(t)
	past := time.Now().Add(-time.Hour)
	s.calls["pending"] = &imsCall{public: vowifi.Call{ID: "pending", State: "ended", EndedAt: &past}, invitePending: true}
	if len(s.Calls()) != 1 {
		t.Fatal("pending INVITE was pruned before cleanup")
	}
	if _, err := s.DialCall(context.Background(), "+15550000002"); !errors.Is(err, ErrCallBusy) {
		t.Fatalf("uncertain module reused: %v", err)
	}
	delete(s.calls, "pending")
	now := time.Now()
	for i := 0; i < 256; i++ {
		if !s.admitCallLocked() {
			t.Fatal("terminal call blocked admission")
		}
		id := fmt.Sprint(i)
		s.calls[id] = &imsCall{public: vowifi.Call{ID: id, State: "ended", EndedAt: &now}}
	}
	if len(s.calls) > maxCallHistory {
		t.Fatalf("history grew without bound: %d", len(s.calls))
	}
}

func TestIncomingAnswerRetransmitsUntilMatchingAck(t *testing.T) {
	s, _ := newCallTestSession(t)
	invite := incomingTestInvite(t, "ack-recovery")
	invite.Body = []byte(callTestSDP)
	var answers atomic.Int32
	s.handleCallRequest(invite, func(b []byte) error {
		if strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			answers.Add(1)
		}
		return nil
	})
	if _, err := s.AnswerCall(context.Background(), "ack-recovery"); err != nil {
		t.Fatal(err)
	}
	s.handleCallACK(incomingTestRequest(t, invite, "ACK", 2, true)) // wrong CSeq
	waitCallTest(t, func() bool { return answers.Load() >= 2 })
	s.callMu.Lock()
	wrongAcked := s.calls["ack-recovery"].answerAcked
	s.callMu.Unlock()
	if wrongAcked {
		t.Fatal("wrong ACK suppressed recovery")
	}
	s.handleCallACK(incomingTestRequest(t, invite, "ACK", 1, true))
	afterAck := answers.Load()
	time.Sleep(1100 * time.Millisecond)
	if answers.Load() != afterAck || s.Calls()[0].State != "active" {
		t.Fatal("valid ACK did not stop answer retransmissions")
	}
}

func TestIncomingAnswerAckTimeoutReleasesMedia(t *testing.T) {
	s, conn := newCallTestSession(t)
	acceptCallTestControls(s, conn)
	invite := incomingTestInvite(t, "ack-timeout")
	invite.Body = []byte(callTestSDP)
	s.handleCallRequest(invite, func([]byte) error { return nil })
	if _, err := s.AnswerCall(context.Background(), "ack-timeout"); err != nil {
		t.Fatal(err)
	}
	s.callMu.Lock()
	call := s.calls["ack-timeout"]
	s.callMu.Unlock()
	s.recoverIncomingAnswerFor(call, 20*time.Millisecond, time.Second)
	waitCallTest(t, func() bool { return conn.count("BYE") == 1 })
	if s.Calls()[0].State != "failed" || s.Calls()[0].MediaReady || call.media.ready() {
		t.Fatal("ACK timeout retained live media/call")
	}
}

func TestLocalHangupDuringAnswerDoesNotResurrectCall(t *testing.T) {
	s, conn := newCallTestSession(t)
	acceptCallTestControls(s, conn)
	invite := incomingTestInvite(t, "answer-hangup")
	invite.Body = []byte(callTestSDP)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var rejected atomic.Int32
	s.handleCallRequest(invite, func(b []byte) error {
		if strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			close(entered)
			<-release
		}
		if strings.HasPrefix(string(b), "SIP/2.0 486 ") {
			rejected.Add(1)
		}
		return nil
	})
	answered, hungup := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.AnswerCall(context.Background(), "answer-hangup"); answered <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("answer did not enter writer")
	}
	go func() { hungup <- s.HangupCall(context.Background(), "answer-hangup") }()
	waitCallTest(t, func() bool { return s.Calls()[0].State == "ended" })
	once.Do(func() { close(release) })
	if err := <-answered; !errors.Is(err, ErrCallState) {
		t.Fatalf("answer/hangup result = %v", err)
	}
	if err := <-hungup; err != nil {
		t.Fatal(err)
	}
	if rejected.Load() != 0 || conn.count("BYE") != 1 || s.Calls()[0].State != "ended" {
		t.Fatal("answer/hangup race sent conflicting finals or resurrected call")
	}
}

func TestConcurrentIncomingRetransmissionNotifiesOnce(t *testing.T) {
	s, _ := newCallTestSession(t)
	var notifications, responses atomic.Int32
	s.provider.config.OnIncomingCall = func(context.Context, ReceivedCall) error { notifications.Add(1); return nil }
	invite := incomingTestInvite(t, "parallel-invite")
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			s.handleCallRequest(invite, func(b []byte) error {
				if strings.HasPrefix(string(b), "SIP/2.0 180 ") {
					responses.Add(1)
				}
				return nil
			})
		}()
	}
	group.Wait()
	waitCallTest(t, func() bool { return notifications.Load() != 0 })
	if notifications.Load() != 1 || responses.Load() != 16 || len(s.Calls()) != 1 {
		t.Fatalf("duplicate INVITE: notifications=%d responses=%d calls=%d", notifications.Load(), responses.Load(), len(s.Calls()))
	}
}

func TestCallControlUdpRetransmissionUsesSameTransaction(t *testing.T) {
	for _, method := range []string{"CANCEL", "BYE"} {
		t.Run(method, func(t *testing.T) {
			s, conn := newCallTestSession(t)
			s.provider.config.TransactionTimeout = 2 * time.Second
			conn.onWrite = func(b []byte) {
				if conn.count(method) < 2 {
					return
				} // lose first datagram
				packet, _ := parseSIPPacket(b)
				response, _ := buildSIPResponseWithBody(packet.Request, 200, "remote", nil)
				parsed, _ := parseSIPPacket(response)
				s.dispatchPacket(parsed, nil)
			}
			call := &imsCall{callID: "retrans-control", cseq: 1, target: "tel:+15550000002", branch: "original",
				from: "<sip:subscriber@ims.example.test>;tag=local", to: "<tel:+15550000002>;tag=remote"}
			if err := s.sendDialogRequest(context.Background(), call, method); err != nil {
				t.Fatal(err)
			}
			packets := conn.requests(method)
			if len(packets) != 2 || packets[0].value("Via") != packets[1].value("Via") || packets[0].value("CSeq") != packets[1].value("CSeq") {
				t.Fatal("retransmission created a new transaction")
			}
		})
	}
}

func TestIncomingCancelCannotReplaceExistingRejection(t *testing.T) {
	s, _ := newCallTestSession(t)
	invite := incomingTestInvite(t, "reject-then-cancel")
	var replies []string
	s.handleCallRequest(invite, func(b []byte) error { replies = append(replies, string(b)); return nil })
	if err := s.HangupCall(context.Background(), "reject-then-cancel"); err != nil {
		t.Fatal(err)
	}
	s.handleCallTermination(incomingTestRequest(t, invite, "CANCEL", 1, false), func(b []byte) error {
		if !strings.HasPrefix(string(b), "SIP/2.0 200 ") {
			t.Fatal(string(b))
		}
		return nil
	})
	if len(replies) != 2 || !strings.HasPrefix(replies[1], "SIP/2.0 486 ") {
		t.Fatalf("final rejection replaced: %#v", replies)
	}
}

func TestExcessAcceptedForksStillReceiveBestEffortBye(t *testing.T) {
	s, conn := newCallTestSession(t)
	acceptCallTestControls(s, conn)
	public, err := s.DialCall(context.Background(), "+15550000002")
	if err != nil {
		t.Fatal(err)
	}
	s.callMu.Lock()
	call := s.calls[public.ID]
	s.callMu.Unlock()
	for i := 0; i < 10; i++ {
		call.responses <- &sipResponse{StatusCode: 200, Headers: map[string][]string{
			"to":      {fmt.Sprintf("<tel:+15550000002>;tag=fork-%d", i)},
			"contact": {fmt.Sprintf("<sip:fork-%d@127.0.0.1:15061>", i)},
		}, Body: []byte(callTestSDP)}
	}
	waitCallTest(t, func() bool { return conn.count("ACK") == 10 && conn.count("BYE") == 9 })
	if s.Calls()[0].State != "active" {
		t.Fatal("excess fork replaced selected call")
	}
	for _, bye := range conn.requests("BYE") {
		if headerParameter(bye.value("To"), "tag") == "fork-0" {
			t.Fatal("selected fork was cleared")
		}
	}
}

func TestUncertainInviteWriteRetainsKnownIDAndLateAnswerDrain(t *testing.T) {
	s, conn := newCallTestSession(t)
	conn.failInvite = true
	acceptCallTestControls(s, conn)
	public, err := s.DialCall(context.Background(), "+15550000001")
	if !errors.Is(err, io.ErrUnexpectedEOF) || public.ID == "" || !public.SignalingPending || public.State != "failed" {
		t.Fatalf("uncertain submit lost known ID/reservation: %#v %v", public, err)
	}
	if _, err := s.DialCall(context.Background(), "+15550000002"); !errors.Is(err, ErrCallBusy) {
		t.Fatal("uncertain line reused", err)
	}
	s.callMu.Lock()
	call := s.calls[public.ID]
	s.callMu.Unlock()
	call.responses <- &sipResponse{StatusCode: 200, Headers: map[string][]string{"to": {"<tel:+15550000001>;tag=remote"}, "contact": {"<sip:peer@127.0.0.1:15061>"}}, Body: []byte(callTestSDP)}
	waitCallTest(t, func() bool { return conn.count("ACK") == 1 && conn.count("BYE") == 1 })
	if s.Calls()[0].State != "failed" || s.Calls()[0].SignalingPending {
		t.Fatal("late answer resurrected uncertain call or retained pending flag")
	}
}
