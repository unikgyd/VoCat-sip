package sipgateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

type incomingSyntheticCarrier struct {
	*syntheticCarrier
	answers           int
	answerError       error
	answerEntered     chan struct{}
	answerPause       <-chan struct{}
	activePollEntered chan struct{}
	activePollRelease <-chan struct{}
}

func (b *incomingSyntheticCarrier) Calls(ctx context.Context, id string) ([]vowifi.Call, error) {
	calls, err := b.syntheticCarrier.Calls(ctx, id)
	b.mu.Lock()
	entered, release := b.activePollEntered, b.activePollRelease
	b.mu.Unlock()
	if len(calls) > 0 && calls[0].State == "active" && entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	return calls, err
}

func (b *incomingSyntheticCarrier) incoming(id, number string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	call := vowifi.Call{ID: randomToken(), Number: number, Direction: "incoming", State: "ringing", StartedAt: time.Now().UTC()}
	b.calls[id] = call
	b.media[call.ID] = &syntheticCarrierMedia{up: make(chan []int16, 16), down: make(chan []int16, 16)}
	return call.ID
}
func (b *incomingSyntheticCarrier) Incoming(ctx context.Context, id, callID string) (vowifi.Call, error) {
	calls, err := b.Calls(ctx, id)
	if err != nil {
		return vowifi.Call{}, err
	}
	for _, call := range calls {
		if call.ID == callID && call.Direction == "incoming" && call.State == "ringing" && call.EndedAt == nil {
			return call, nil
		}
	}
	return vowifi.Call{}, ErrCarrierBusy
}
func (b *incomingSyntheticCarrier) Answer(ctx context.Context, id, callID string) (vowifi.Call, error) {
	if ctx.Err() != nil {
		return vowifi.Call{}, ctx.Err()
	}
	b.mu.Lock()
	b.answers++
	pause, entered, err := b.answerPause, b.answerEntered, b.answerError
	call := b.calls[id]
	if call.ID != callID || call.State != "ringing" {
		err = ErrCarrierBusy
	}
	if err == nil {
		call.State, call.MediaReady = "active", true
		b.calls[id] = call
	}
	b.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if pause != nil {
		<-pause
	} // intentionally ignore cancellation for exit tests
	return call, err
}
func (b *incomingSyntheticCarrier) answerCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answers
}
func incomingRuntime(t *testing.T, mutate func(*Config)) (*Manager, *runtime, *incomingSyntheticCarrier) {
	t.Helper()
	config := carrierTestConfig(t)
	for i := range config.Config.CarrierLines {
		config.Config.CarrierLines[i].AllowIncoming = true
		config.Config.CarrierLines[i].IncomingExtensions = []string{"1001", "1002"}
	}
	if mutate != nil {
		mutate(&config.Config)
	}
	b := &incomingSyntheticCarrier{syntheticCarrier: newSyntheticCarrier()}
	r, err := start(config, runtimeOptions{carrier: b, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	// Incoming admission also takes r.mu before starting its call worker.
	r.mu.Lock()
	r.callRingTimeout, r.callLateTimeout = time.Second, 150*time.Millisecond
	r.mu.Unlock()
	t.Cleanup(r.close)
	m := &Manager{runtime: r, carrier: b}
	return m, r, b
}
func pendingIncoming(t *testing.T, m *Manager, r *runtime, b *incomingSyntheticCarrier, device string) (string, *wireClient, *sip.Request) {
	t.Helper()
	peer := registerMessagePeer(t, r, "1001")
	id := b.incoming(device, "+15550000001")
	if !m.ForwardIncoming(device, id) {
		t.Fatal("incoming not admitted")
	}
	invite := readCallRequest(t, peer, sip.INVITE)
	return id, peer, invite
}

func TestCarrierIncomingPermissionsAndConfigCopy(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.CarrierLines[0].IncomingExtensions = []string{"missing"} },
		func(c *Config) { c.CarrierLines[0].IncomingExtensions = []string{"1001", "1001"} },
	} {
		c := carrierTestConfig(t).Config
		mutate(&c)
		if prepareCarrierConfig(&c) == nil {
			t.Fatal("invalid targets accepted")
		}
	}
	c := carrierTestConfig(t).Config
	c.CarrierLines[0].IncomingExtensions = []string{"1001"}
	prepared, err := prepare(c, stored{}, true)
	if err != nil { // Existing hashes live in the old stored configuration.
		prepared, err = prepare(c, carrierTestConfig(t), true)
	}
	if err != nil {
		t.Fatal(err)
	}
	c.CarrierLines[0].IncomingExtensions[0] = "changed"
	copy := cloneConfig(prepared.Config)
	copy.CarrierLines[0].IncomingExtensions[0] = "also changed"
	if prepared.Config.CarrierLines[0].IncomingExtensions[0] != "1001" {
		t.Fatal("incoming target aliases escaped")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"global", func(c *Config) { c.CarrierEnabled = false }},
		{"calls", func(c *Config) { c.CallsEnabled = false }},
		{"line", func(c *Config) { c.CarrierLines[0].AllowIncoming = false }},
		{"empty", func(c *Config) { c.CarrierLines[0].IncomingExtensions = nil }},
		{"account", func(c *Config) {
			for i := range c.Accounts {
				c.Accounts[i].AllowCalls = false
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, b := incomingRuntime(t, tc.mutate)
			registerMessagePeer(t, r, "1001")
			id := b.incoming("module-a", "+15550000001")
			if m.ForwardIncoming("module-a", id) {
				t.Fatal("unauthorized group admitted")
			}
			if b.answerCount() != 0 || b.hangupCount() != 0 {
				t.Fatal("disabled forwarding touched carrier")
			}
		})
	}
}

func TestCarrierIncomingGroupAcrossAccountsFirstAnswerAndPCM(t *testing.T) {
	m, r, b := incomingRuntime(t, nil)
	one, two := registerMessagePeer(t, r, "1001"), registerMessagePeer(t, r, "1002")
	id := b.incoming("module-a", "+15550000001")
	if !m.ForwardIncoming("module-a", id) || m.ForwardIncoming("module-a", id) {
		t.Fatal("duplicate admission")
	}
	first, second := readCallRequest(t, one, sip.INVITE), readCallRequest(t, two, sip.INVITE)
	if first.From().Address.String() != "sip:+15550000001@voice.example.test" || first.To().Address.User != "1001" || second.To().Address.User != "1002" || first.CallID().Value() == second.CallID().Value() {
		t.Fatal("identity or independent dialog lost")
	}
	pcmSocket := testMediaSocket(t)
	writeCall(t, two, calleeResponse(t, two, second, pcmSocket, 180))
	answer := calleeResponse(t, one, first, pcmSocket, 200)
	writeCall(t, one, answer)
	readCallRequest(t, one, sip.ACK)
	cancel := readCallRequest(t, two, sip.CANCEL)
	replyCallOK(t, two, cancel)
	// A simultaneous losing 200 is ACKed/BYEd, never answered at the carrier.
	writeCall(t, two, calleeResponse(t, two, second, pcmSocket, 200))
	readCallRequest(t, two, sip.ACK)
	replyCallOK(t, two, readCallRequest(t, two, sip.BYE))
	waitCarrier(t, func() bool {
		return b.answerCount() == 1 && len(r.status().Calls) == 1 && r.status().Calls[0].Phase == "active"
	})
	if !m.CarrierOwnsMedia("module-a", id) || m.CarrierOwnsMedia("module-b", id) || !errors.Is(m.ClaimLocalAnswer("module-a", id), ErrCarrierBusy) {
		t.Fatal("SIP audio/answer ownership not fenced")
	}
	desc, err := parseCallSDP(first.Body())
	if err != nil {
		t.Fatal(err)
	}
	relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: desc.port}
	payload := bytes.Repeat([]byte{0x7f}, 160)
	packet := append([]byte{0x80, 0, 0, 1, 0, 0, 0, 160, 1, 2, 3, 4}, payload...)
	_, _ = pcmSocket.WriteToUDP(packet, relay)
	b.mu.Lock()
	media := b.media[id]
	b.mu.Unlock()
	select {
	case samples := <-media.up:
		want, _ := ims.DecodeG711(payload, "PCMU")
		if !bytes.Equal(int16Bytes(samples), int16Bytes(want)) {
			t.Fatal("incoming uplink PCM corrupt")
		}
	case <-time.After(time.Second):
		t.Fatal("no incoming PCM uplink")
	}
	samples := make([]int16, 160)
	for i := range samples {
		samples[i] = 1500
	}
	media.down <- samples
	pcmSocket.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := pcmSocket.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := ims.EncodeG711(samples, "PCMU")
	if n != 172 || buffer[1]&127 != 0 || !bytes.Equal(buffer[12:n], want) {
		t.Fatal("incoming downlink RTP corrupt")
	}
	// Winner BYE has no synthetic SIP caller leg; it ends the exact IMS call.
	writeCall(t, one, dialogRequest(t, first, answer, sip.BYE, false, 2))
	readCallFinal(t, one, 200)
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { return b.hangupCount() == 1 })
	if b.dialCount() != 0 || b.answerCount() != 1 {
		t.Fatal("inbound caused redial or duplicate answer")
	}
	port, err := net.ListenUDP("udp", relay)
	if err != nil {
		t.Fatal("RTP port retained", err)
	}
	port.Close()
}

func int16Bytes(samples []int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, v := range samples {
		b[2*i] = byte(v)
		b[2*i+1] = byte(uint16(v) >> 8)
	}
	return b
}

func TestCarrierIncomingLocalAnswerCancelsAndDrainsLateSIP200(t *testing.T) {
	m, r, b := incomingRuntime(t, nil)
	id, peer, invite := pendingIncoming(t, m, r, b, "module-a")
	media := testMediaSocket(t)
	writeCall(t, peer, calleeResponse(t, peer, invite, media, 180))
	if err := m.ClaimLocalAnswer("module-a", id); err != nil {
		t.Fatal(err)
	}
	b.activate("module-a") // separate web audio owner, not the SIP backend
	replyCallOK(t, peer, readCallRequest(t, peer, sip.CANCEL))
	writeCall(t, peer, calleeResponse(t, peer, invite, media, 200))
	readCallRequest(t, peer, sip.ACK)
	replyCallOK(t, peer, readCallRequest(t, peer, sip.BYE))
	waitCalls(t, r, 0)
	time.Sleep(180 * time.Millisecond)
	if b.answerCount() != 0 || b.hangupCount() != 0 || m.CarrierOwnsMedia("module-a", id) || m.ForwardIncoming("module-a", id) {
		t.Fatal("local answer was stolen, hung up or re-rang")
	}
}

func TestCarrierIncomingCarrierEndsBeforeAndAfterAnswer(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "ringing", true: "active"}[answered], func(t *testing.T) {
			m, r, b := incomingRuntime(t, nil)
			id, peer, invite := pendingIncoming(t, m, r, b, "module-a")
			media := testMediaSocket(t)
			code := 180
			if answered {
				code = 200
			}
			writeCall(t, peer, calleeResponse(t, peer, invite, media, code))
			if answered {
				readCallRequest(t, peer, sip.ACK)
				waitCarrier(t, func() bool { return b.answerCount() == 1 })
			}
			if err := b.Hangup(context.Background(), "module-a", id); err != nil {
				t.Fatal(err)
			}
			method := sip.CANCEL
			if answered {
				method = sip.BYE
			}
			replyCallOK(t, peer, readCallRequest(t, peer, method))
			waitCalls(t, r, 0)
		})
	}
}

func TestCarrierIncomingUnansweredGroupsLeaveLocalCallAvailable(t *testing.T) {
	for _, code := range []int{0, 486, 603} {
		t.Run(map[int]string{0: "timeout", 486: "busy", 603: "decline"}[code], func(t *testing.T) {
			m, r, b := incomingRuntime(t, nil)
			r.mu.Lock()
			r.callRingTimeout = 150 * time.Millisecond
			r.mu.Unlock()
			_, peer, invite := pendingIncoming(t, m, r, b, "module-a")
			media := testMediaSocket(t)
			if code == 0 {
				writeCall(t, peer, calleeResponse(t, peer, invite, media, 180))
				replyCallOK(t, peer, readCallRequest(t, peer, sip.CANCEL))
			} else {
				writeCall(t, peer, calleeResponse(t, peer, invite, media, code))
				readCallRequest(t, peer, sip.ACK)
			}
			waitCalls(t, r, 0)
			if code == 603 {
				waitCarrier(t, func() bool { return b.hangupCount() == 1 })
			} else {
				time.Sleep(180 * time.Millisecond)
				if b.hangupCount() != 0 {
					t.Fatal("unanswered SIP group hung up local receiver")
				}
			}
			if b.answerCount() != 0 || b.dialCount() != 0 {
				t.Fatal("unanswered call answered or redialled")
			}
		})
	}
}

func TestCarrierIncomingAnswerUncertainAndOtherOwner(t *testing.T) {
	for _, ownedElsewhere := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "other owner"}[ownedElsewhere], func(t *testing.T) {
			m, r, b := incomingRuntime(t, nil)
			b.answerError = errors.New("synthetic unknown delivery")
			if ownedElsewhere {
				b.answerError = ErrCarrierBusy
			}
			_, peer, invite := pendingIncoming(t, m, r, b, "module-a")
			media := testMediaSocket(t)
			writeCall(t, peer, calleeResponse(t, peer, invite, media, 200))
			readCallRequest(t, peer, sip.ACK)
			replyCallOK(t, peer, readCallRequest(t, peer, sip.BYE))
			waitCalls(t, r, 0)
			if !ownedElsewhere {
				waitCarrier(t, func() bool { return b.hangupCount() == 1 })
			} else {
				time.Sleep(180 * time.Millisecond)
				if b.hangupCount() != 0 {
					t.Fatal("other owner's call was hung up")
				}
			}
		})
	}
}

func TestCarrierIncomingShutdownDoesNotWaitForIMSAnswer(t *testing.T) {
	m, r, b := incomingRuntime(t, nil)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	b.answerEntered, b.answerPause = entered, release
	_, peer, invite := pendingIncoming(t, m, r, b, "module-a")
	media := testMediaSocket(t)
	writeCall(t, peer, calleeResponse(t, peer, invite, media, 200))
	readCallRequest(t, peer, sip.ACK)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("answer not entered")
	}
	before := time.Now()
	r.close()
	if time.Since(before) > time.Second {
		t.Fatal("shutdown waited for IMS answer")
	}
	close(release)
	waitCarrier(t, func() bool { return b.hangupCount() == 1 })
}

func TestCarrierIncomingTwoModulesIndependent(t *testing.T) {
	m, r, b := incomingRuntime(t, func(c *Config) {
		c.CarrierLines[0].IncomingExtensions = []string{"1001"}
		c.CarrierLines[1].IncomingExtensions = []string{"1002"}
	})
	one, two := registerMessagePeer(t, r, "1001"), registerMessagePeer(t, r, "1002")
	a, bb := b.incoming("module-a", "+15550000001"), b.incoming("module-b", "+15550000002")
	if !m.ForwardIncoming("module-a", a) || !m.ForwardIncoming("module-b", bb) {
		t.Fatal("parallel module blocked")
	}
	i, j := readCallRequest(t, one, sip.INVITE), readCallRequest(t, two, sip.INVITE)
	media := testMediaSocket(t)
	writeCall(t, one, calleeResponse(t, one, i, media, 180))
	writeCall(t, two, calleeResponse(t, two, j, media, 180))
	if err := m.ClaimLocalAnswer("module-a", a); err != nil {
		t.Fatal(err)
	}
	replyCallOK(t, one, readCallRequest(t, one, sip.CANCEL))
	answer := calleeResponse(t, two, j, media, 200)
	writeCall(t, two, answer)
	readCallRequest(t, two, sip.ACK)
	waitCarrier(t, func() bool {
		calls := r.status().Calls
		return len(calls) == 1 && calls[0].DeviceID == "module-b" && calls[0].Phase == "active"
	})
	writeCall(t, two, dialogRequest(t, j, answer, sip.BYE, false, 2))
	readCallFinal(t, two, 200)
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { return b.hangupCount() == 1 })
	b.mu.Lock()
	state := b.calls["module-a"].State
	b.mu.Unlock()
	if state != "ringing" {
		t.Fatal("other module was modified")
	}
}

func TestCarrierIncomingObservedWebAnswerWinsQueuedSIP200(t *testing.T) {
	m, r, b := incomingRuntime(t, nil)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	b.activePollEntered, b.activePollRelease = entered, release
	id, peer, invite := pendingIncoming(t, m, r, b, "module-a")
	media := testMediaSocket(t)
	writeCall(t, peer, calleeResponse(t, peer, invite, media, 180))
	b.activate("module-a") // web answer happened before the SIP 200
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("active poll not entered")
	}
	writeCall(t, peer, calleeResponse(t, peer, invite, media, 200))
	readCallRequest(t, peer, sip.ACK)
	waitCarrier(t, func() bool { return m.CarrierOwnsMedia("module-a", id) })
	close(release)
	replyCallOK(t, peer, readCallRequest(t, peer, sip.BYE))
	waitCalls(t, r, 0)
	time.Sleep(180 * time.Millisecond)
	if b.answerCount() != 0 || b.hangupCount() != 0 {
		t.Fatal("queued SIP answer hung up the earlier web-owned call")
	}
}

func TestCarrierIncomingLocalClaimBeforeNotificationFencesAdmission(t *testing.T) {
	m, r, b := incomingRuntime(t, nil)
	registerMessagePeer(t, r, "1001")
	id := b.incoming("module-a", "+15550000001")
	if err := m.ClaimLocalAnswer("module-a", id); err != nil {
		t.Fatal(err)
	}
	if m.ForwardIncoming("module-a", id) {
		t.Fatal("delayed notification stole local answer")
	}
	if b.hangupCount() != 0 || b.answerCount() != 0 {
		t.Fatal("claim itself touched IMS")
	}
}

func TestCarrierPCMClosedSIPMediaCannotBeResurrected(t *testing.T) {
	for i := 0; i < 20; i++ {
		media, err := newCallMedia("127.0.0.1:15060")
		if err != nil {
			t.Fatal(err)
		}
		pcm := &syntheticCarrierMedia{up: make(chan []int16, 1), down: make(chan []int16, 1)}
		bridge, err := newCarrierPCMBridge(context.Background(), media, pcm)
		if err != nil {
			t.Fatal(err)
		}
		media.close()
		bridge.start()
		if media.pcm.Load() != nil || bridge.ctx.Err() == nil {
			t.Fatal("closed SIP socket acquired a new PCM consumer")
		}
		if _, err := newCarrierPCMBridge(context.Background(), media, pcm); err == nil {
			t.Fatal("closed media accepted a new bridge")
		}
	}
}

func TestCarrierIncomingPendingForkBudgetRejectsWholeGroup(t *testing.T) {
	m, r, b := incomingRuntime(t, nil)
	registerMessagePeer(t, r, "1001")
	id := b.incoming("module-a", "+15550000001")
	r.mu.Lock()
	r.branchSlots = 480
	r.mu.Unlock()
	if m.ForwardIncoming("module-a", id) {
		t.Fatal("pending branches not counted in admission")
	}
	if b.answerCount() != 0 || b.hangupCount() != 0 || len(r.status().Calls) != 0 {
		t.Fatal("budget rejection partially took over the call")
	}
	r.mu.Lock()
	r.branchSlots = 0
	r.mu.Unlock()
}
