package sipgateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"vocat/internal/vowifi/ims"
)

const callPeerPassword = "synthetic-outbound-peer-secret"
const callPeerInboundPassword = "synthetic-inbound-peer-secret"

type fixedCallFixture struct {
	t       *testing.T
	conn    *net.UDPConn
	r       *runtime
	manager *Manager
	carrier *incomingSyntheticCarrier
}

func newFixedCallFixture(t *testing.T, change func(*Config)) *fixedCallFixture {
	return newFixedCallFixtureWithBackend(t, change, nil)
}
func newFixedCallFixtureWithBackend(t *testing.T, change func(*Config), custom CarrierBackend) *fixedCallFixture {
	t.Helper()
	conn := testMediaSocket(t)
	config := carrierTestConfig(t)
	for i := range config.Config.CarrierLines {
		config.Config.CarrierLines[i].AllowIncoming = true
		config.Config.CarrierLines[i].IncomingExtensions = nil
	}
	config.Config.Peer = &PeerConfig{Enabled: true, Endpoint: conn.LocalAddr().String(),
		InboundAuth:        PeerInboundAuth{Username: "pbx-in", Password: callPeerInboundPassword},
		OutboundAuth:       PeerOutboundAuth{Username: "gateway-out", Realm: "private-pbx", Password: callPeerPassword, Algorithms: []string{"SHA-256", "MD5"}},
		AllowCallsFromPeer: true, AllowCallsToPeer: true, OutgoingLines: []string{"module-a", "module-b"},
		IncomingRoutes: []PeerIncomingRoute{{DeviceID: "module-a", CallTarget: "600"}, {DeviceID: "module-b", CallTarget: "600"}}}
	if change != nil {
		change(&config.Config)
	}
	prepared, err := prepare(config.Config, config, true)
	if err != nil {
		t.Fatal(err)
	}
	backend := &incomingSyntheticCarrier{syntheticCarrier: newSyntheticCarrier()}
	carrier := CarrierBackend(backend)
	if custom != nil {
		carrier = custom
	}
	r, err := start(prepared, runtimeOptions{carrier: carrier, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.callRingTimeout, r.callLateTimeout, r.callAckTimeout = 2*time.Second, 250*time.Millisecond, time.Second
	r.mu.Unlock()
	t.Cleanup(r.close)
	return &fixedCallFixture{t, conn, r, &Manager{current: prepared, runtime: r, carrier: carrier}, backend}
}
func (p *fixedCallFixture) read() sip.Message {
	p.t.Helper()
	p.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 16384)
	n, source, err := p.conn.ReadFromUDP(buffer)
	if err != nil {
		p.t.Fatal(err)
	}
	if source.String() != p.r.conn.LocalAddr().String() {
		p.t.Fatal("signaling escaped its pinned gateway")
	}
	message, err := registrarParser().ParseSIP(buffer[:n])
	if err != nil {
		p.t.Fatal(err)
	}
	return message
}
func (p *fixedCallFixture) request(method sip.RequestMethod) *sip.Request {
	p.t.Helper()
	message := p.read()
	request, ok := message.(*sip.Request)
	if !ok || request.Method != method {
		p.t.Fatalf("want %s, got %s", method, message.String())
	}
	return request
}
func (p *fixedCallFixture) send(message sip.Message) {
	p.t.Helper()
	if _, err := p.conn.WriteToUDP([]byte(message.String()), p.r.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		p.t.Fatal(err)
	}
}
func (p *fixedCallFixture) response(req *sip.Request, status int) *sip.Response {
	response := sip.NewResponseFromRequest(req, status, "Synthetic", nil)
	if response.To().Params.GetOr("tag", "") == "" {
		response.To().Params.Add("tag", "peer-answer")
	}
	return response
}
func (p *fixedCallFixture) challenge(req *sip.Request, status int, algorithm, realm string) *sip.Response {
	res := p.response(req, status)
	name := "WWW-Authenticate"
	if status == 407 {
		name = "Proxy-Authenticate"
	}
	res.AppendHeader(sip.NewHeader(name, fmt.Sprintf(`Digest realm="%s",nonce="loopback-nonce",algorithm=%s,qop="auth"`, realm, algorithm)))
	// Real UAS responses add these routing observations to the original Via.
	res.Via().Params.Add("received", "127.0.0.1")
	res.Via().Params.Add("rport", strconv.Itoa(p.r.conn.LocalAddr().(*net.UDPAddr).Port))
	return res
}
func assertPeerCallDigest(t *testing.T, req *sip.Request, algorithm string) {
	t.Helper()
	header := req.GetHeader("Proxy-Authorization")
	if header == nil {
		header = req.GetHeader("Authorization")
	}
	if header == nil {
		t.Fatal("peer authentication absent")
	}
	fields, err := parseDigest(header.Value())
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := digest.ParseChallenge(fmt.Sprintf(`Digest realm="private-pbx",nonce="loopback-nonce",algorithm=%s,qop="auth"`, algorithm))
	if err != nil {
		t.Fatal(err)
	}
	want, err := digest.Digest(challenge, digest.Options{Method: string(req.Method), URI: req.Recipient.String(), Username: "gateway-out", Password: callPeerPassword, Count: 1, Cnonce: fields["cnonce"]})
	if err != nil {
		t.Fatal(err)
	}
	wantFields, err := parseDigest(want.String())
	if err != nil || fields["response"] != wantFields["response"] || fields["username"] != "gateway-out" || fields["nc"] != "00000001" {
		t.Fatal("outbound proof differs from independent digest client")
	}
}
func (p *fixedCallFixture) answer(req *sip.Request, media *net.UDPConn) *sip.Response {
	response := p.response(req, 200)
	response.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "dialog", Host: "contact.invalid", Port: 9999}})
	response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	response.SetBody(makeCallOffer(media.LocalAddr().(*net.UDPAddr)))
	return response
}
func TestPeerCallsAuthenticatedIncomingAndBidirectionalPCM(t *testing.T) {
	for _, algorithm := range []string{"MD5", "SHA-256"} {
		t.Run(algorithm, func(t *testing.T) {
			p := newFixedCallFixture(t, nil)
			id := p.carrier.incoming("module-a", "+15550000001")
			if !p.manager.ForwardIncoming("module-a", id) {
				t.Fatal("peer-only incoming target not admitted")
			}
			initial := p.request(sip.INVITE)
			if initial.From().Address.User != "+15550000001" || len(p.r.status().Bindings) != 0 {
				t.Fatal("caller lost or peer masquerades as registration")
			}
			p.send(p.challenge(initial, 407, algorithm, "private-pbx"))
			ack := p.request(sip.ACK)
			retry := p.request(sip.INVITE)
			if ack.CSeq().SeqNo != 1 || retry.CSeq().SeqNo != 2 || retry.CallID().Value() != initial.CallID().Value() || retry.Via().Params.GetOr("branch", "") == initial.Via().Params.GetOr("branch", "") {
				t.Fatal("challenge transaction identity not advanced independently")
			}
			assertPeerCallDigest(t, retry, algorithm)
			media := testMediaSocket(t)
			answer := p.answer(retry, media)
			p.send(answer)
			if p.request(sip.ACK).CSeq().SeqNo != 2 {
				t.Fatal("2xx ACK used original challenge CSeq")
			}
			waitCarrier(t, func() bool { calls := p.r.status().Calls; return len(calls) == 1 && calls[0].Phase == "active" })
			description, err := parseCallSDP(retry.Body())
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte{0x7f}, 160)
			packet := append([]byte{0x80, 8, 0, 1, 0, 0, 0, 160, 1, 2, 3, 4}, payload...)
			if _, err = media.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: description.port}); err != nil {
				t.Fatal(err)
			}
			p.carrier.mu.Lock()
			pcm := p.carrier.media[id]
			p.carrier.mu.Unlock()
			select {
			case samples := <-pcm.up:
				want, _ := ims.DecodeG711(payload, "PCMA")
				if !bytes.Equal(int16Bytes(samples), int16Bytes(want)) {
					t.Fatal("peer uplink corrupted")
				}
			case <-time.After(time.Second):
				t.Fatal("peer uplink absent")
			}
			samples := make([]int16, 160)
			for i := range samples {
				samples[i] = 1500
			}
			pcm.down <- samples
			media.SetReadDeadline(time.Now().Add(time.Second))
			buffer := make([]byte, 2048)
			n, _, err := media.ReadFromUDP(buffer)
			want, _ := ims.EncodeG711(samples, "PCMA")
			if err != nil || n != 172 || !bytes.Equal(buffer[12:n], want) {
				t.Fatal("peer downlink corrupted", err)
			}
			p.send(dialogRequest(t, retry, answer, sip.BYE, false, 4))
			if res, ok := p.read().(*sip.Response); !ok || res.StatusCode != 200 {
				t.Fatal("peer BYE not accepted")
			}
			waitCalls(t, p.r, 0)
			waitCarrier(t, func() bool { return p.carrier.hangupCount() == 1 })
		})
	}
}

func TestPeerCallsCancelAfterChallengeUsesCurrentAttemptAndCleansLate200(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	id := p.carrier.incoming("module-a", "+15550000001")
	if !p.manager.ForwardIncoming("module-a", id) {
		t.Fatal("admission")
	}
	initial := p.request(sip.INVITE)
	p.send(p.challenge(initial, 401, "SHA-256", "private-pbx"))
	p.request(sip.ACK)
	retry := p.request(sip.INVITE)
	p.send(p.response(retry, 180))
	if err := p.manager.ClaimLocalAnswer("module-a", id); err != nil {
		t.Fatal(err)
	}
	p.carrier.activate("module-a") // A separately owned web answer.
	cancel := p.request(sip.CANCEL)
	if cancel.CSeq().SeqNo != 2 || cancel.Via().Params.GetOr("branch", "") != retry.Via().Params.GetOr("branch", "") {
		t.Fatal("cancel targeted retired attempt")
	}
	p.send(p.response(cancel, 200))
	answer := p.answer(retry, testMediaSocket(t))
	p.send(answer)
	if p.request(sip.ACK).CSeq().SeqNo != 2 {
		t.Fatal("late answer ACK sequence")
	}
	bye := p.request(sip.BYE)
	if bye.CSeq().SeqNo != 3 {
		t.Fatal("late answer BYE sequence")
	}
	p.send(p.response(bye, 200))
	waitCalls(t, p.r, 0)
	if p.carrier.answerCount() != 0 || p.carrier.hangupCount() != 0 {
		t.Fatal("peer cleanup touched independently owned carrier call")
	}
}

func TestPeerCallsBYEAuthenticatesUnderOneBudget(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "gateway", Host: "voice.test"}}
	from.Params.Add("tag", "local")
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: "600", Host: "voice.test"}}
	to.Params.Add("tag", "remote")
	req, err := p.r.buildCallRequest(sip.BYE, sip.Uri{Scheme: "sip", User: "dialog", Host: "different-contact.invalid"}, p.conn.LocalAddr().String(), from, to, "bye-test", 3, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.r.callDo(ctx, req, p.conn.LocalAddr().String()) }()
	first := p.request(sip.BYE)
	p.send(p.challenge(first, 401, "SHA-256", "private-pbx"))
	retry := p.request(sip.BYE)
	if retry.CSeq().SeqNo != 4 || retry.Via().Params.GetOr("branch", "") == first.Via().Params.GetOr("branch", "") {
		t.Fatal("BYE auth reused transaction")
	}
	assertPeerCallDigest(t, retry, "SHA-256")
	p.send(p.response(retry, 200))
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestPeerCallsWrongRealmDoesNotDiscloseCredentialsOrAnswer(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	id := p.carrier.incoming("module-a", "+15550000001")
	if !p.manager.ForwardIncoming("module-a", id) {
		t.Fatal("admission")
	}
	initial := p.request(sip.INVITE)
	p.send(p.challenge(initial, 401, "SHA-256", "untrusted-realm"))
	p.request(sip.ACK)
	waitCalls(t, p.r, 0)
	p.conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	buf := make([]byte, 4096)
	if _, _, err := p.conn.ReadFromUDP(buf); err == nil {
		t.Fatal("unexpected authenticated peer request")
	}
	if p.carrier.answerCount() != 0 || p.carrier.hangupCount() != 0 {
		t.Fatal("wrong realm touched carrier")
	}
}

func (p *fixedCallFixture) ingress(target string) *sip.Request {
	p.t.Helper()
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "+15550000001", Host: "presented.invalid"}}
	from.Params.Add("tag", randomToken())
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: target, Host: "old-network.invalid"}}
	req, err := p.r.buildCallRequest(sip.INVITE, to.Address, p.conn.LocalAddr().String(), from, to, randomToken(), 2, "", makeCallOffer(testMediaSocket(p.t).LocalAddr().(*net.UDPAddr)))
	if err != nil {
		p.t.Fatal(err)
	}
	req.AppendHeader(sip.NewHeader("Authorization", peerTestCredential(p.t, p.r, req, "pbx-in", callPeerInboundPassword, p.conn.LocalAddr().String())))
	return req
}

func TestPeerCallsIngressTwoModulesShareAuthorityNotBusySlot(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	requests := []*sip.Request{p.ingress("+15550000011"), p.ingress("82+15550000012")}
	for _, req := range requests {
		p.send(req)
	}
	waitCarrier(t, func() bool { return p.carrier.dialCount() == 2 })
	p.carrier.activate("module-a")
	p.carrier.activate("module-b")
	answers := map[string]*sip.Response{}
	for len(answers) < 2 {
		res, ok := p.read().(*sip.Response)
		if !ok {
			t.Fatal("unexpected request during carrier ingress")
		}
		if res.StatusCode < 200 {
			continue
		}
		if res.StatusCode != 200 {
			t.Fatalf("peer carrier call rejected: %d", res.StatusCode)
		}
		answers[res.CallID().Value()] = res
	}
	for _, req := range requests {
		p.send(dialogRequest(t, req, answers[req.CallID().Value()], sip.ACK, true, 2))
	}
	waitCarrier(t, func() bool {
		calls := p.r.status().Calls
		return len(calls) == 2 && calls[0].Phase == "active" && calls[1].Phase == "active"
	})
	for _, req := range requests {
		p.send(dialogRequest(t, req, answers[req.CallID().Value()], sip.BYE, true, 3))
	}
	for i := 0; i < 2; i++ {
		res, ok := p.read().(*sip.Response)
		if !ok || res.StatusCode != 200 || res.CSeq().MethodName != sip.BYE {
			t.Fatal("ingress BYE not accepted")
		}
	}
	waitCalls(t, p.r, 0)
	waitCarrier(t, func() bool { return p.carrier.hangupCount() == 2 })
	if len(p.r.status().Bindings) != 0 {
		t.Fatal("ingress peer became a registered account")
	}
}

func TestPeerCallsIngressNeverRoutesKnownLocalExtensionOrUnpermittedLine(t *testing.T) {
	for _, target := range []string{"1001", "82+15550000012"} {
		t.Run(target, func(t *testing.T) {
			p := newFixedCallFixture(t, func(c *Config) { c.Peer.OutgoingLines = []string{"module-a"} })
			p.send(p.ingress(target))
			for {
				res, ok := p.read().(*sip.Response)
				if !ok {
					t.Fatal("peer reflected into an extension")
				}
				if res.StatusCode < 200 {
					continue
				}
				if res.StatusCode != 403 {
					t.Fatalf("want explicit denial, got %d", res.StatusCode)
				}
				break
			}
			if p.carrier.dialCount() != 0 {
				t.Fatal("rejected peer touched a carrier")
			}
		})
	}
}

func TestPeerCallsTwoIncomingModulesSameTargetStayIndependent(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	for _, device := range []string{"module-a", "module-b"} {
		id := p.carrier.incoming(device, "+15550000001")
		if !p.manager.ForwardIncoming(device, id) {
			t.Fatal("peer target incorrectly shares a local busy slot")
		}
	}
	invites := []*sip.Request{p.request(sip.INVITE), p.request(sip.INVITE)}
	if invites[0].CallID().Value() == invites[1].CallID().Value() {
		t.Fatal("modules share a dialog")
	}
	answers := []*sip.Response{p.answer(invites[0], testMediaSocket(t)), p.answer(invites[1], testMediaSocket(t))}
	for _, answer := range answers {
		p.send(answer)
	}
	p.request(sip.ACK)
	p.request(sip.ACK)
	waitCarrier(t, func() bool { return p.carrier.answerCount() == 2 })
	for i, invite := range invites {
		p.send(dialogRequest(t, invite, answers[i], sip.BYE, false, 2))
	}
	for i := 0; i < 2; i++ {
		res, ok := p.read().(*sip.Response)
		if !ok || res.StatusCode != 200 {
			t.Fatal("independent peer BYE failed")
		}
	}
	waitCalls(t, p.r, 0)
	waitCarrier(t, func() bool { return p.carrier.hangupCount() == 2 })
}

func TestPeerCallsRetiredAttemptCannotWinAndDuplicateAnswerIsIdempotent(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	id := p.carrier.incoming("module-a", "+15550000001")
	if !p.manager.ForwardIncoming("module-a", id) {
		t.Fatal("admission")
	}
	initial := p.request(sip.INVITE)
	p.send(p.challenge(initial, 407, "SHA-256", "private-pbx"))
	p.request(sip.ACK)
	retry := p.request(sip.INVITE)
	retired := p.answer(initial, testMediaSocket(t))
	retired.To().Params.Add("tag", "retired-dialog")
	p.send(retired)
	if p.request(sip.ACK).CSeq().SeqNo != 1 {
		t.Fatal("retired response used new ACK sequence")
	}
	cleanup := p.request(sip.BYE)
	if cleanup.To().Params.GetOr("tag", "") != "retired-dialog" || cleanup.CSeq().SeqNo != 2 {
		t.Fatal("retired response cleanup targeted current attempt")
	}
	p.send(p.response(cleanup, 200))
	if p.carrier.answerCount() != 0 {
		t.Fatal("retired attempt stole the answer")
	}
	answer := p.answer(retry, testMediaSocket(t))
	p.send(answer)
	p.request(sip.ACK)
	waitCarrier(t, func() bool { return p.carrier.answerCount() == 1 })
	p.send(answer)
	if p.request(sip.ACK).CSeq().SeqNo != 2 {
		t.Fatal("duplicate 2xx was not ACKed")
	}
	p.send(retired)
	if p.request(sip.ACK).CSeq().SeqNo != 1 {
		t.Fatal("retired duplicate was not ACKed")
	}
	p.send(dialogRequest(t, retry, answer, sip.BYE, false, 4))
	if res, ok := p.read().(*sip.Response); !ok || res.StatusCode != 200 {
		t.Fatal("active dialog was incorrectly BYEd by duplicate responses")
	}
	waitCalls(t, p.r, 0)
	if p.carrier.answerCount() != 1 {
		t.Fatal("duplicate response answered twice")
	}
}

func TestPeerCallsBYEChallengeDoesNotRenewCleanupDeadline(t *testing.T) {
	p := newFixedCallFixture(t, nil)
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "gateway", Host: "voice.test"}}
	from.Params.Add("tag", "local")
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: "600", Host: "voice.test"}}
	to.Params.Add("tag", "remote")
	req, err := p.r.buildCallRequest(sip.BYE, to.Address, p.conn.LocalAddr().String(), from, to, randomToken(), 3, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() { result <- p.r.callDo(ctx, req, p.conn.LocalAddr().String()) }()
	first := p.request(sip.BYE)
	time.Sleep(250 * time.Millisecond)
	p.send(p.challenge(first, 401, "SHA-256", "private-pbx"))
	retry := p.request(sip.BYE)
	if retry.CSeq().SeqNo != 4 {
		t.Fatal("challenge not retried")
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 850*time.Millisecond {
		t.Fatal("authentication renewed cleanup deadline", err)
	}
}
