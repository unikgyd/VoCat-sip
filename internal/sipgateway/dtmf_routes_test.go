package sipgateway

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/modulecall"
	"vocat/internal/vowifi"
)

func (b usbWireCarrier) SendDTMF(ctx context.Context, id, cid string, digit byte) error {
	return b.manager.SendDTMF(ctx, id, cid, digit)
}

func TestSIPDTMFUSBOutgoingUsesOneOriginalModuleAction(t *testing.T) {
	audio := &usbWireAudio{syntheticCarrierMedia: &syntheticCarrierMedia{up: make(chan []int16, 8), down: make(chan []int16, 8)}, done: make(chan struct{})}
	line := &usbWireLine{audio: audio}
	manager := modulecall.New(context.Background(), usbWireSource{line: line})
	r, err := start(carrierTestConfig(t), runtimeOptions{carrier: usbWireCarrier{manager}, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		manager.Close(ctx)
	})
	caller, cm, invite := prepareCaller(t, r, "7700900001")
	invite.SetBody(eventSDP(cm.LocalAddr().(*net.UDPAddr).Port, 110, "0-15"))
	writeCall(t, caller, invite)
	answer := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	desc, _ := parseCallSDP(answer.Body())
	if desc.events.Mask != 0xffff {
		t.Fatal("module keypad unavailable", desc.events)
	}
	waitCarrier(t, func() bool {
		r.mu.Lock()
		leg := r.callLegs[invite.CallID().Value()]
		r.mu.Unlock()
		return leg != nil && leg.call.media.pcm.Load() != nil
	})
	address := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: desc.port}
	for i := 0; i < 3; i++ {
		cm.WriteToUDP(eventPacket(110, 7, uint16(i+1), 1000, true), address)
	}
	waitCarrier(t, func() bool { line.mu.Lock(); defer line.mu.Unlock(); return len(line.digits) == 1 })
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	waitCalls(t, r, 0)
	line.mu.Lock()
	defer line.mu.Unlock()
	if string(line.digits) != "7" {
		t.Fatal("redundant reports became repeated AT actions", line.digits)
	}
}

type dtmfSubmission struct {
	device, call string
	digit        byte
}
type dtmfRouteMedia struct {
	vowifi.CallMedia
	digits chan byte
}

func (m *dtmfRouteMedia) ReadDTMF(ctx context.Context) (byte, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case digit := <-m.digits:
		return digit, nil
	}
}
func (m *dtmfRouteMedia) FlushDTMF() {
	for i := 0; i < cap(m.digits); i++ {
		select {
		case <-m.digits:
		default:
			return
		}
	}
}

type dtmfIncomingCarrier struct {
	*incomingSyntheticCarrier
	mediaMu     sync.Mutex
	keyMedia    map[string]*dtmfRouteMedia
	submissions chan dtmfSubmission
}

func newDTMFIncomingCarrier() *dtmfIncomingCarrier {
	return &dtmfIncomingCarrier{incomingSyntheticCarrier: &incomingSyntheticCarrier{syntheticCarrier: newSyntheticCarrier()}, keyMedia: map[string]*dtmfRouteMedia{}, submissions: make(chan dtmfSubmission, 8)}
}
func (b *dtmfIncomingCarrier) incomingWithKeys(device string) string {
	id := b.incoming(device, "+15550000001")
	b.mu.Lock()
	call := b.calls[device]
	call.DTMFDigits = "0123456789*#ABCD"
	b.calls[device] = call
	b.mu.Unlock()
	return id
}

func (b *dtmfIncomingCarrier) Dial(ctx context.Context, device, number string) (vowifi.Call, error) {
	call, err := b.incomingSyntheticCarrier.Dial(ctx, device, number)
	if err != nil {
		return call, err
	}
	b.mu.Lock()
	call = b.calls[device]
	call.DTMFDigits = "0123456789*#ABCD"
	b.calls[device] = call
	b.mu.Unlock()
	return call, nil
}
func (b *dtmfIncomingCarrier) Media(ctx context.Context, device, cid string) (vowifi.CallMedia, error) {
	pcm, err := b.incomingSyntheticCarrier.Media(ctx, device, cid)
	if err != nil {
		return nil, err
	}
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	media := b.keyMedia[cid]
	if media == nil {
		media = &dtmfRouteMedia{CallMedia: pcm, digits: make(chan byte, 8)}
		b.keyMedia[cid] = media
	}
	return media, nil
}
func (b *dtmfIncomingCarrier) SendDTMF(ctx context.Context, device, cid string, digit byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b.mu.Lock()
	call := b.calls[device]
	active := call.ID == cid && call.State == "active" && call.EndedAt == nil
	b.mu.Unlock()
	if !active {
		return ErrCarrierBusy
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.submissions <- dtmfSubmission{device, cid, digit}:
		return nil
	}
}

func TestSIPDTMFIncomingPeerUsesOriginalCallAfterDigestRetry(t *testing.T) {
	b := newDTMFIncomingCarrier()
	p := newFixedCallFixtureWithBackend(t, nil, b)
	id := b.incomingWithKeys("module-a")
	if !p.manager.ForwardIncoming("module-a", id) {
		t.Fatal("incoming not forwarded")
	}
	initial := p.request(sip.INVITE)
	p.send(p.challenge(initial, 407, "SHA-256", "private-pbx"))
	p.request(sip.ACK)
	retry := p.request(sip.INVITE)
	assertPeerCallDigest(t, retry, "SHA-256")
	rtp := testMediaSocket(t)
	answer := p.answer(retry, rtp)
	answer.SetBody(eventSDP(rtp.LocalAddr().(*net.UDPAddr).Port, 101, "0-15"))
	p.send(answer)
	p.request(sip.ACK)
	waitCarrier(t, func() bool { return len(p.r.status().Calls) == 1 && p.r.status().Calls[0].Phase == "active" })
	desc, _ := parseCallSDP(retry.Body())
	address := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: desc.port}
	for i := 0; i < 3; i++ {
		rtp.WriteToUDP(eventPacket(101, 11, uint16(i+1), 9000, true), address)
	}
	select {
	case got := <-b.submissions:
		if got != (dtmfSubmission{"module-a", id, '#'}) {
			t.Fatal("wrong original carrier call", got)
		}
	case <-time.After(time.Second):
		t.Fatal("peer digit missing")
	}
	b.mediaMu.Lock()
	media := b.keyMedia[id]
	b.mediaMu.Unlock()
	media.digits <- '8'
	assertWireEvent(t, rtp, 101, 8)
	select {
	case got := <-b.submissions:
		t.Fatal("peer event was submitted twice", got)
	default:
	}
	p.send(dialogRequest(t, retry, answer, sip.BYE, false, 4))
	if response, ok := p.read().(*sip.Response); !ok || response.StatusCode != 200 {
		t.Fatal("BYE rejected")
	}
	waitCalls(t, p.r, 0)
	waitCarrier(t, func() bool { return b.hangupCount() == 1 })
	for i := 0; i < 3; i++ {
		rtp.WriteToUDP(eventPacket(101, 1, uint16(100+i), 10000, true), address)
	}
	select {
	case got := <-b.submissions:
		t.Fatal("ended peer still controlled carrier", got)
	case <-time.After(60 * time.Millisecond):
	}
}

func TestSIPDTMFIncomingOnlyWinningExtensionControlsCarrier(t *testing.T) {
	b := newDTMFIncomingCarrier()
	config := carrierTestConfig(t)
	config.Config.CarrierLines[0].AllowIncoming = true
	config.Config.CarrierLines[0].IncomingExtensions = []string{"1001", "1002"}
	r, err := start(config, runtimeOptions{carrier: b, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	m := &Manager{runtime: r, carrier: b}
	one, two := registerMessagePeer(t, r, "1001"), registerMessagePeer(t, r, "1002")
	id := b.incomingWithKeys("module-a")
	if !m.ForwardIncoming("module-a", id) {
		t.Fatal("admission")
	}
	first, second := readCallRequest(t, one, sip.INVITE), readCallRequest(t, two, sip.INVITE)
	winner, loser := testMediaSocket(t), testMediaSocket(t)
	writeCall(t, two, calleeResponse(t, two, second, loser, 180))
	answer := calleeResponse(t, one, first, winner, 200)
	answer.SetBody(eventSDP(winner.LocalAddr().(*net.UDPAddr).Port, 101, "0-15"))
	writeCall(t, one, answer)
	readCallRequest(t, one, sip.ACK)
	replyCallOK(t, two, readCallRequest(t, two, sip.CANCEL))
	late := calleeResponse(t, two, second, loser, 200)
	late.SetBody(eventSDP(loser.LocalAddr().(*net.UDPAddr).Port, 101, "0-15"))
	writeCall(t, two, late)
	readCallRequest(t, two, sip.ACK)
	replyCallOK(t, two, readCallRequest(t, two, sip.BYE))
	waitCarrier(t, func() bool { return len(r.status().Calls) == 1 && r.status().Calls[0].Phase == "active" })
	desc, _ := parseCallSDP(first.Body())
	lost, _ := parseCallSDP(second.Body())
	loser.WriteToUDP(eventPacket(101, 9, 1, 1000, true), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: lost.port})
	winner.WriteToUDP(eventPacket(101, 8, 1, 2000, true), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: desc.port})
	select {
	case got := <-b.submissions:
		if got != (dtmfSubmission{"module-a", id, '8'}) {
			t.Fatal("losing extension controlled the call", got)
		}
	case <-time.After(time.Second):
		t.Fatal("winning digit missing")
	}
	writeCall(t, one, dialogRequest(t, first, answer, sip.BYE, false, 2))
	readCallFinal(t, one, 200)
	waitCalls(t, r, 0)
}

func TestSIPDTMFPeerIngressTwoModulesDoNotShareKeyOwner(t *testing.T) {
	b := newDTMFIncomingCarrier()
	p := newFixedCallFixtureWithBackend(t, nil, b)
	rtp := []*net.UDPConn{testMediaSocket(t), testMediaSocket(t)}
	requests := []*sip.Request{p.ingress("+15550000011"), p.ingress("82+15550000012")}
	for i, req := range requests {
		req.SetBody(eventSDP(rtp[i].LocalAddr().(*net.UDPAddr).Port, 110+i, "0-15"))
		p.send(req)
	}
	waitCarrier(t, func() bool { return b.dialCount() == 2 })
	b.activate("module-a")
	b.activate("module-b")
	answers := map[string]*sip.Response{}
	for len(answers) < 2 {
		response, ok := p.read().(*sip.Response)
		if !ok {
			t.Fatal("unexpected peer message")
		}
		if response.StatusCode < 200 {
			continue
		}
		if response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
		answers[response.CallID().Value()] = response
	}
	for _, req := range requests {
		p.send(dialogRequest(t, req, answers[req.CallID().Value()], sip.ACK, true, 2))
	}
	waitCarrier(t, func() bool {
		calls := p.r.status().Calls
		if len(calls) != 2 || calls[0].Phase != "active" || calls[1].Phase != "active" {
			return false
		}
		p.r.mu.Lock()
		defer p.r.mu.Unlock()
		for _, call := range p.r.calls {
			if call.media == nil || call.media.pcm.Load() == nil {
				return false
			}
		}
		return true
	})
	for i, req := range requests {
		desc, _ := parseCallSDP(answers[req.CallID().Value()].Body())
		rtp[i].WriteToUDP(eventPacket(byte(110+i), byte(i+1), 1, uint32(1000+i), true), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: desc.port})
	}
	seen := map[string]dtmfSubmission{}
	for i := 0; i < 2; i++ {
		select {
		case got := <-b.submissions:
			seen[got.device] = got
		case <-time.After(time.Second):
			t.Fatal("peer key absent")
		}
	}
	b.mu.Lock()
	a, c := b.calls["module-a"].ID, b.calls["module-b"].ID
	b.mu.Unlock()
	if seen["module-a"] != (dtmfSubmission{"module-a", a, '1'}) || seen["module-b"] != (dtmfSubmission{"module-b", c, '2'}) {
		t.Fatal("peer keys crossed module calls", seen)
	}
	for _, req := range requests {
		p.send(dialogRequest(t, req, answers[req.CallID().Value()], sip.BYE, true, 3))
	}
	for range requests {
		response, ok := p.read().(*sip.Response)
		if !ok || response.StatusCode != 200 {
			t.Fatal("BYE rejected")
		}
	}
	waitCalls(t, p.r, 0)
}

type blockedDTMFCarrier struct {
	*dtmfIncomingCarrier
	entered  chan dtmfSubmission
	exited   chan struct{}
	attempts atomic.Int32
}

func (b *blockedDTMFCarrier) SendDTMF(ctx context.Context, device, cid string, digit byte) error {
	b.attempts.Add(1)
	b.entered <- dtmfSubmission{device, cid, digit}
	<-ctx.Done()
	close(b.exited)
	return ctx.Err()
}
func TestSIPDTMFSlowCarrierDoesNotQueueKeysOrBlockPCM(t *testing.T) {
	peer := testMediaSocket(t)
	media, err := newCallMedia(peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer media.close()
	desc, _ := parseCallSDP(eventSDP(peer.LocalAddr().(*net.UDPAddr).Port, 110, "0-15"))
	media.configure(peer.LocalAddr().String(), desc)
	pcm := &syntheticCarrierMedia{up: make(chan []int16, 4), down: make(chan []int16, 4)}
	b := &blockedDTMFCarrier{dtmfIncomingCarrier: newDTMFIncomingCarrier(), entered: make(chan dtmfSubmission, 4), exited: make(chan struct{})}
	bridge, err := newCarrierPCMBridge(context.Background(), media, pcm)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	bridge.bindDTMF(b, "module-a", "original-call", "0123456789")
	bridge.start()
	peer.WriteToUDP(eventPacket(110, 1, 1, 1000, true), media.address())
	select {
	case got := <-b.entered:
		if got != (dtmfSubmission{"module-a", "original-call", '1'}) {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("first key not submitted")
	}
	peer.WriteToUDP(eventPacket(110, 2, 2, 2000, true), media.address())
	voice := make([]byte, 172)
	voice[0], voice[1] = 0x80, 8
	voice[3] = 3
	peer.WriteToUDP(voice, media.address())
	select {
	case samples := <-pcm.up:
		if len(samples) != 160 {
			t.Fatal(samples)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked DTMF stopped PCM forwarding")
	}
	bridge.close()
	select {
	case <-b.exited:
	case <-time.After(time.Second):
		t.Fatal("closed SIP call did not cancel carrier key")
	}
	if b.attempts.Load() != 1 || bridge.dtmfDropped.Load() == 0 {
		t.Fatal("key was queued behind a blocked carrier", b.attempts.Load(), bridge.dtmfDropped.Load())
	}
}
