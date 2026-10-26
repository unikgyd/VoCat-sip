package sipgateway

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"vocat/internal/vowifi/ims"
)

func testCallRuntime(t *testing.T, changes ...func(*Config)) *runtime {
	t.Helper()
	config := testConfig(t, "SHA-256")
	config.Config.CallsEnabled = true
	config.Config.IdentityDomain = "voice.example.test"
	for i := range config.Config.Accounts {
		config.Config.Accounts[i].AllowCalls = true
	}
	for _, change := range changes {
		if change != nil {
			change(&config.Config)
		}
	}
	r, err := start(config)
	if err != nil {
		t.Fatal(err)
	}
	// The listener is live; every future call admission takes r.mu before
	// reading these immutable-per-call fixture settings.
	r.mu.Lock()
	r.callLateTimeout = 600 * time.Millisecond
	r.callRingTimeout = 2 * time.Second
	r.callAckTimeout = time.Second
	r.mu.Unlock()
	t.Cleanup(r.close)
	return r
}
func testMediaSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func readCallPacket(t *testing.T, c *wireClient) sip.Message {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 16384)
	n, err := c.conn.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := registrarParser().ParseSIP(buffer[:n])
	if err != nil {
		t.Fatal(err)
	}
	return msg
}
func readCallRequest(t *testing.T, c *wireClient, method sip.RequestMethod) *sip.Request {
	t.Helper()
	msg := readCallPacket(t, c)
	req, ok := msg.(*sip.Request)
	if !ok || req.Method != method {
		t.Fatalf("expected %s, got %s", method, msg.String())
	}
	return req
}
func readCallFinal(t *testing.T, c *wireClient, code int) *sip.Response {
	t.Helper()
	for {
		msg := readCallPacket(t, c)
		res, ok := msg.(*sip.Response)
		if !ok {
			t.Fatalf("expected response, got %s", msg.String())
		}
		if res.StatusCode < 200 {
			continue
		}
		if res.StatusCode != code {
			t.Fatalf("status %d want %d: %s", res.StatusCode, code, res.String())
		}
		return res
	}
}
func writeCall(t *testing.T, c *wireClient, msg sip.Message) {
	t.Helper()
	if _, err := c.conn.Write([]byte(msg.String())); err != nil {
		t.Fatal(err)
	}
}
func newCallerInvite(t *testing.T, c *wireClient, media *net.UDPConn, target, auth string) *sip.Request {
	t.Helper()
	c.serial++
	body := string(makeCallOffer(media.LocalAddr().(*net.UDPAddr)))
	// Hostnames and display name deliberately disagree with our server's
	// configured identity. They must never control routing or caller identity.
	packet := fmt.Sprintf("INVITE sip:%s@old-vpn.invalid SIP/2.0\r\nVia: SIP/2.0/UDP 192.0.2.90:9;branch=z9hG4bK-call-%s-%d;rport\r\nFrom: \"Spoofed\" <sip:1001@old-lan.invalid>;tag=caller-tag\r\nTo: <sip:%s@old-domain.invalid>\r\nCall-ID: %s\r\nCSeq: 2 INVITE\r\nMax-Forwards: 70\r\nContact: <sip:caller@unreachable.invalid:9>\r\nContent-Type: application/sdp\r\n%sContent-Length: %d\r\n\r\n%s", target, c.branch, c.serial, target, c.callID, auth, len(body), body)
	msg, err := registrarParser().ParseSIP([]byte(packet))
	if err != nil {
		t.Fatal(err)
	}
	return msg.(*sip.Request)
}
func prepareCaller(t *testing.T, r *runtime, target string) (*wireClient, *net.UDPConn, *sip.Request) {
	return prepareCallerWithChallenge(t, r, target, false)
}
func prepareCallerWithChallenge(t *testing.T, r *runtime, target string, registrationChallenge bool) (*wireClient, *net.UDPConn, *sip.Request) {
	t.Helper()
	c := newWireClient(t, r)
	c.callID = "call-" + randomToken()
	media := testMediaSocket(t)
	initial := newCallerInvite(t, c, media, target, "")
	challengePacket := initial.String()
	if registrationChallenge {
		// Permission fixtures install immutable policy before listener startup.
		// REGISTER remains available when calls are disabled; its same-peer
		// nonce still authenticates the INVITE whose permission result we test.
		challengePacket = c.packet("REGISTER", "1001", 1, "", "")
	}
	res := c.exchange(challengePacket)
	requireStatus(t, res, 401)
	parsed, err := registrarParser().ParseSIP([]byte(res.raw))
	if err != nil {
		t.Fatal(err)
	}
	if !registrationChallenge {
		ack := dialogRequest(t, initial, parsed.(*sip.Response), sip.ACK, true, 2)
		ack.RemoveHeader("Via")
		ack.PrependHeader(sip.HeaderClone(initial.Via()))
		ack.Recipient = *initial.Recipient.Clone()
		writeCall(t, c, ack)
	}
	challenge, err := digest.ParseChallenge(res.headers["www-authenticate"][0])
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := digest.Digest(challenge, digest.Options{Method: "INVITE", URI: initial.Recipient.String(), Username: "1001", Password: testPassword, Count: 1, Cnonce: randomToken()})
	if err != nil {
		t.Fatal(err)
	}
	return c, media, newCallerInvite(t, c, media, target, "Authorization: "+credentials.String()+"\r\n")
}
func calleeResponse(t *testing.T, c *wireClient, invite *sip.Request, media *net.UDPConn, code int) *sip.Response {
	t.Helper()
	res := sip.NewResponseFromRequest(invite, code, "Test", nil)
	res.To().Params.Add("tag", "callee-"+c.branch)
	if code == 200 {
		body := fmt.Sprintf("v=0\r\no=test 1 1 IN IP4 203.0.113.88\r\ns=test\r\nc=IN IP4 203.0.113.88\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n", media.LocalAddr().(*net.UDPAddr).Port)
		res.SetBody([]byte(body))
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "device", Host: "unreachable.invalid", Port: 9}})
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	return res
}
func dialogRequest(t *testing.T, invite *sip.Request, answer *sip.Response, method sip.RequestMethod, caller bool, seq uint32) *sip.Request {
	t.Helper()
	req := sip.NewRequest(method, sip.Uri{Scheme: "sip", User: "gateway", Host: "unreachable.invalid"})
	via := sip.HeaderClone(invite.Via()).(*sip.ViaHeader)
	via.Params.Add("branch", "z9hG4bK-dialog-"+randomToken())
	req.AppendHeader(via)
	if caller {
		req.AppendHeader(sip.HeaderClone(invite.From()))
		req.AppendHeader(sip.HeaderClone(answer.To()))
	} else {
		from := answer.To().AsFrom()
		to := invite.From().AsTo()
		req.AppendHeader(&from)
		req.AppendHeader(&to)
	}
	req.AppendHeader(sip.HeaderClone(invite.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})
	h := sip.MaxForwardsHeader(70)
	req.AppendHeader(&h)
	req.SetBody(nil)
	return req
}
func cancelRequest(invite *sip.Request) *sip.Request {
	req := sip.NewRequest(sip.CANCEL, *invite.Recipient.Clone())
	for _, h := range []sip.Header{invite.Via(), invite.From(), invite.To(), invite.CallID()} {
		req.AppendHeader(sip.HeaderClone(h))
	}
	req.AppendHeader(&sip.CSeqHeader{SeqNo: invite.CSeq().SeqNo, MethodName: sip.CANCEL})
	req.SetBody(nil)
	return req
}
func replyCallOK(t *testing.T, c *wireClient, req *sip.Request) {
	t.Helper()
	writeCall(t, c, sip.NewResponseFromRequest(req, 200, "OK", nil))
}
func waitCalls(t *testing.T, r *runtime, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(r.status().Calls) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("calls not %d: %+v", n, r.status().Calls)
}

type connectedCall struct {
	caller, callee           *wireClient
	callerMedia, calleeMedia *net.UDPConn
	incoming, outgoing       *sip.Request
	answer, remoteAnswer     *sip.Response
}

func connectCall(t *testing.T, r *runtime, ack bool) connectedCall {
	t.Helper()
	callee := registerMessagePeer(t, r, "1002")
	caller, cm, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	out := readCallRequest(t, callee, sip.INVITE)
	if out.From().Address.String() != "sip:1001@voice.example.test" || out.Recipient.User != "device-1002" || len(out.GetHeaders("Authorization")) != 0 || len(out.GetHeaders("Route")) != 0 {
		t.Fatal("untrusted caller identity/routing forwarded")
	}
	dm := testMediaSocket(t)
	remoteAnswer := calleeResponse(t, callee, out, dm, 200)
	writeCall(t, callee, remoteAnswer)
	readCallRequest(t, callee, sip.ACK)
	answer := readCallFinal(t, caller, 200)
	if ack {
		writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			s := r.status()
			if len(s.Calls) == 1 && s.Calls[0].Phase == "active" {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	return connectedCall{caller, callee, cm, dm, invite, out, answer, remoteAnswer}
}
func TestCallWireBidirectionalMediaAndCallerHangup(t *testing.T) {
	r := testCallRuntime(t)
	c := connectCall(t, r, true)
	callerSDP, err := parseCallSDP(c.answer.Body())
	if err != nil {
		t.Fatal(err)
	}
	calleeSDP, err := parseCallSDP(c.outgoing.Body())
	if err != nil {
		t.Fatal(err)
	}
	callerRelay := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: callerSDP.port}
	calleeRelay := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: calleeSDP.port}
	for _, d := range []struct {
		from, to           *net.UDPConn
		relay              *net.UDPAddr
		pt                 byte
		fromCodec, toCodec string
		wantPT             byte
	}{{c.callerMedia, c.calleeMedia, callerRelay, 8, "PCMA", "PCMU", 0}, {c.calleeMedia, c.callerMedia, calleeRelay, 0, "PCMU", "PCMA", 8}} {
		packet := append([]byte{0x80, 0x80 | d.pt, 0, 17, 0, 0, 0, 160, 1, 2, 3, 4}, bytes.Repeat([]byte{0xd5}, 160)...)
		if _, err := d.from.WriteToUDP(packet, d.relay); err != nil {
			t.Fatal(err)
		}
		d.to.SetReadDeadline(time.Now().Add(time.Second))
		buffer := make([]byte, 2048)
		n, source, err := d.to.ReadFromUDP(buffer)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := ims.TranscodeG711(packet[12:], d.fromCodec, d.toCodec)
		if n != 172 || buffer[1] != 0x80|d.wantPT || !bytes.Equal(buffer[2:12], packet[2:12]) || !bytes.Equal(buffer[12:n], want) || source.Port == d.relay.Port {
			t.Fatal("RTP forwarding/codec/source socket mismatch")
		}
	}
	s := r.status()
	// Receiving the forwarded UDP datagram does not synchronize with the
	// sender goroutine's subsequent successful-write counter increment. Keep
	// the exact one-packet assertions, but wait for that asynchronous telemetry.
	deadline := time.Now().Add(time.Second)
	for len(s.Calls) == 1 && (s.Calls[0].CallerPackets == 0 || s.Calls[0].CalleePackets == 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		s = r.status()
	}
	if len(s.Calls) != 1 || s.Calls[0].CallerPackets != 1 || s.Calls[0].CalleePackets != 1 {
		t.Fatalf("packet stats %+v", s.Calls)
	}
	bad := dialogRequest(t, c.incoming, c.answer, sip.BYE, true, 3)
	bad.To().Params.Add("tag", "wrong")
	writeCall(t, c.caller, bad)
	readCallFinal(t, c.caller, 481)
	waitCalls(t, r, 1)
	writeCall(t, c.caller, dialogRequest(t, c.incoming, c.answer, sip.BYE, true, 4))
	readCallFinal(t, c.caller, 200)
	bye := readCallRequest(t, c.callee, sip.BYE)
	replyCallOK(t, c.callee, bye)
	waitCalls(t, r, 0)
	for _, address := range []*net.UDPAddr{callerRelay, calleeRelay} {
		socket, err := net.ListenUDP("udp", address)
		if err != nil {
			t.Fatalf("media port not released: %v", err)
		}
		socket.Close()
	}
}
func TestCallCalleeHangupAndSpoofedBye(t *testing.T) {
	r := testCallRuntime(t)
	c := connectCall(t, r, true)
	bye := dialogRequest(t, c.outgoing, c.remoteAnswer, sip.BYE, false, 2)
	spoof := newWireClient(t, r)
	writeCall(t, spoof, bye)
	readCallFinal(t, spoof, 481)
	waitCalls(t, r, 1)
	writeCall(t, c.callee, bye)
	readCallFinal(t, c.callee, 200)
	request := readCallRequest(t, c.caller, sip.BYE)
	replyCallOK(t, c.caller, request)
	waitCalls(t, r, 0)
}
func TestCallCancelBeforeAnswerRejectsSpoofAndCleansLate200(t *testing.T) {
	r := testCallRuntime(t)
	callee := registerMessagePeer(t, r, "1002")
	caller, _, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	out := readCallRequest(t, callee, sip.INVITE)
	dm := testMediaSocket(t)
	writeCall(t, callee, calleeResponse(t, callee, out, dm, 180))
	cancel := cancelRequest(invite)
	spoof := newWireClient(t, r)
	writeCall(t, spoof, cancel)
	readCallFinal(t, spoof, 481)
	waitCalls(t, r, 1)
	writeCall(t, caller, cancel)
	first := readCallFinal(t, caller, 200)
	if first.CSeq().MethodName != sip.CANCEL {
		t.Fatal("CANCEL response must precede 487")
	}
	readCallFinal(t, caller, 487)
	remoteCancel := readCallRequest(t, callee, sip.CANCEL)
	replyCallOK(t, callee, remoteCancel)
	late := calleeResponse(t, callee, out, dm, 200)
	writeCall(t, callee, late)
	readCallRequest(t, callee, sip.ACK)
	bye := readCallRequest(t, callee, sip.BYE)
	replyCallOK(t, callee, bye)
	waitCalls(t, r, 0)
	writeCall(t, callee, late)
	readCallRequest(t, callee, sip.ACK)
}
func TestCallGroupFirstAnswerCancelsOtherAndLateAnswerGetsBye(t *testing.T) {
	r := testCallRuntime(t)
	one := registerMessagePeer(t, r, "1002")
	two := registerMessagePeer(t, r, "1002")
	caller, _, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	first := readCallRequest(t, one, sip.INVITE)
	second := readCallRequest(t, two, sip.INVITE)
	media := testMediaSocket(t)
	writeCall(t, two, calleeResponse(t, two, second, media, 180))
	writeCall(t, one, calleeResponse(t, one, first, media, 200))
	readCallRequest(t, one, sip.ACK)
	answer := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	cancel := readCallRequest(t, two, sip.CANCEL)
	replyCallOK(t, two, cancel)
	writeCall(t, two, calleeResponse(t, two, second, media, 200))
	readCallRequest(t, two, sip.ACK)
	bye := readCallRequest(t, two, sip.BYE)
	replyCallOK(t, two, bye)
	waitCalls(t, r, 1)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	bye = readCallRequest(t, one, sip.BYE)
	replyCallOK(t, one, bye)
	waitCalls(t, r, 0)
}
func TestCallShutdownDoesNotWaitForPeers(t *testing.T) {
	r := testCallRuntime(t)
	c := connectCall(t, r, true)
	before := time.Now()
	r.close()
	if time.Since(before) > time.Second {
		t.Fatal("shutdown waited for remote peer")
	}
	readCallRequest(t, c.caller, sip.BYE)
	readCallRequest(t, c.callee, sip.BYE)
}
func TestCallAckLossRetransmitsAndEndsBothLegs(t *testing.T) {
	r := testCallRuntime(t)
	r.mu.Lock()
	r.callAckTimeout = 650 * time.Millisecond
	r.mu.Unlock()
	c := connectCall(t, r, false)
	readCallFinal(t, c.caller, 200)
	bye := readCallRequest(t, c.callee, sip.BYE)
	replyCallOK(t, c.callee, bye)
	bye = readCallRequest(t, c.caller, sip.BYE)
	replyCallOK(t, c.caller, bye)
	waitCalls(t, r, 0)
}
func TestCallPermissionsAndNoCarrierFallback(t *testing.T) {
	for _, item := range []struct {
		name, target string
		change       func(*Config)
		code         int
	}{{"global", "1002", func(c *Config) { c.CallsEnabled = false }, 405}, {"caller", "1002", func(c *Config) { c.Accounts[0].AllowCalls = false }, 403}, {"callee", "1002", func(c *Config) { c.Accounts[1].AllowCalls = false }, 403}, {"offline", "1002", nil, 480}, {"external", "17387799413", nil, 404}} {
		t.Run(item.name, func(t *testing.T) {
			r := testCallRuntime(t, item.change)
			caller, _, invite := prepareCallerWithChallenge(t, r, item.target, true)
			writeCall(t, caller, invite)
			readCallFinal(t, caller, item.code)
			waitCalls(t, r, 0)
		})
	}
}
func TestCallActiveSettingsRequireExplicitDisable(t *testing.T) {
	ctx := context.Background()
	m := New(ctx, &memoryRepository{})
	defer m.Close()
	config := managerConfig(t)
	config.IdentityDomain = "voice.example.test"
	config.Accounts[1].Password = testPassword + "-two"
	config.CallsEnabled = true
	for i := range config.Accounts {
		config.Accounts[i].AllowCalls = true
	}
	if _, err := m.Apply(ctx, config); err != nil {
		t.Fatal(err)
	}
	c := connectCall(t, m.runtime, true)
	_ = c
	update := m.Snapshot().Config
	update.IdentityDomain = "new.example.test"
	if _, err := m.Apply(ctx, update); err == nil || !strings.Contains(err.Error(), "active calls") {
		t.Fatal("active call silently interrupted")
	}
	update.Enabled = false
	if _, err := m.Apply(ctx, update); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Status.Running {
		t.Fatal("disable retained listener")
	}
}

func TestCallCancelBeforeProvisionalStopsInviteRetransmission(t *testing.T) {
	r := testCallRuntime(t)
	r.mu.Lock()
	r.callLateTimeout = 1500 * time.Millisecond
	r.mu.Unlock()
	callee := registerMessagePeer(t, r, "1002")
	caller, _, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	out := readCallRequest(t, callee, sip.INVITE)
	writeCall(t, caller, cancelRequest(invite))
	readCallFinal(t, caller, 200)
	readCallFinal(t, caller, 487)
	waitCalls(t, r, 0)
	callee.conn.SetReadDeadline(time.Now().Add(650 * time.Millisecond))
	if _, err := callee.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("INVITE retransmitted or CANCEL sent before provisional response")
	}
	media := testMediaSocket(t)
	writeCall(t, callee, calleeResponse(t, callee, out, media, 180))
	cancel := readCallRequest(t, callee, sip.CANCEL)
	replyCallOK(t, callee, cancel)
	writeCall(t, callee, calleeResponse(t, callee, out, media, 487))
	readCallRequest(t, callee, sip.ACK)
}

func TestCallByeRetransmissionReturnsCached200(t *testing.T) {
	r := testCallRuntime(t)
	c := connectCall(t, r, true)
	bye := dialogRequest(t, c.incoming, c.answer, sip.BYE, true, 3)
	writeCall(t, c.caller, bye)
	readCallFinal(t, c.caller, 200)
	remote := readCallRequest(t, c.callee, sip.BYE)
	replyCallOK(t, c.callee, remote)
	writeCall(t, c.caller, bye)
	readCallFinal(t, c.caller, 200)
}

func TestCallResponseGuardsIgnoreSpoofAndDuplicateTags(t *testing.T) {
	r := testCallRuntime(t)
	callee := registerMessagePeer(t, r, "1002")
	caller, _, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	out := readCallRequest(t, callee, sip.INVITE)
	media := testMediaSocket(t)
	answer := calleeResponse(t, callee, out, media, 200)
	spoof := newWireClient(t, r)
	writeCall(t, spoof, answer)
	broken := strings.Replace(answer.String(), ";tag="+answer.To().Params.GetOr("tag", ""), ";tag=evil;tag="+answer.To().Params.GetOr("tag", ""), 1)
	callee.conn.Write([]byte(broken))
	callee.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := callee.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("forged response produced ACK")
	}
	waitCalls(t, r, 1)
	writeCall(t, callee, answer)
	readCallRequest(t, callee, sip.ACK)
	final := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, final, sip.ACK, true, 2))
	writeCall(t, caller, dialogRequest(t, invite, final, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	bye := readCallRequest(t, callee, sip.BYE)
	replyCallOK(t, callee, bye)
}

func TestCallCalleeHangupBeforeCallerAckRetains200Recovery(t *testing.T) {
	r := testCallRuntime(t)
	r.mu.Lock()
	r.callAckTimeout = 1500 * time.Millisecond
	r.mu.Unlock()
	c := connectCall(t, r, false)
	writeCall(t, c.callee, dialogRequest(t, c.outgoing, c.remoteAnswer, sip.BYE, false, 2))
	readCallFinal(t, c.callee, 200)
	waitCalls(t, r, 0)
	readCallFinal(t, c.caller, 200)
	writeCall(t, c.caller, dialogRequest(t, c.incoming, c.answer, sip.ACK, true, 2))
	bye := readCallRequest(t, c.caller, sip.BYE)
	replyCallOK(t, c.caller, bye)
}

func TestCallByeResponseRequiresDialogToTag(t *testing.T) {
	r := testCallRuntime(t)
	c := connectCall(t, r, true)
	writeCall(t, c.caller, dialogRequest(t, c.incoming, c.answer, sip.BYE, true, 3))
	readCallFinal(t, c.caller, 200)
	bye := readCallRequest(t, c.callee, sip.BYE)
	wrong := sip.NewResponseFromRequest(bye, 200, "Wrong To Tag", nil)
	wrong.To().Params.Add("tag", "wrong")
	writeCall(t, c.callee, wrong)
	readCallRequest(t, c.callee, sip.BYE)
	replyCallOK(t, c.callee, bye)
	waitCalls(t, r, 0)
}

func TestCallRejectedBranchAndRingDeadline(t *testing.T) {
	for _, code := range []int{486, 488, 503, 0} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := testCallRuntime(t)
			r.mu.Lock()
			r.callRingTimeout = 100 * time.Millisecond
			r.mu.Unlock()
			callee := registerMessagePeer(t, r, "1002")
			caller, _, invite := prepareCaller(t, r, "1002")
			writeCall(t, caller, invite)
			out := readCallRequest(t, callee, sip.INVITE)
			media := testMediaSocket(t)
			if code != 0 {
				writeCall(t, callee, calleeResponse(t, callee, out, media, code))
				readCallRequest(t, callee, sip.ACK)
				readCallFinal(t, caller, code)
			} else {
				writeCall(t, callee, calleeResponse(t, callee, out, media, 180))
				readCallFinal(t, caller, 480)
				cancel := readCallRequest(t, callee, sip.CANCEL)
				replyCallOK(t, callee, cancel)
			}
			waitCalls(t, r, 0)
		})
	}
}
