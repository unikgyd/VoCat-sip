package sipgateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/modulecall"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

type usbWireAudio struct {
	*syntheticCarrierMedia
	done chan struct{}
	once sync.Once
}

func (a *usbWireAudio) Codec() string         { return "PCM" }
func (a *usbWireAudio) Start() error          { return nil }
func (a *usbWireAudio) Close() error          { a.once.Do(func() { close(a.done) }); return nil }
func (a *usbWireAudio) Done() <-chan struct{} { return a.done }

type usbWireLine struct {
	mu      sync.Mutex
	current bool
	rows    []modulecall.Record
	audio   *usbWireAudio
	order   []string
	digits  []byte
}

func (l *usbWireLine) Current() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.current }
func (l *usbWireLine) Close()        { l.mu.Lock(); l.current = false; l.mu.Unlock() }
func (l *usbWireLine) Read(context.Context) ([]modulecall.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]modulecall.Record(nil), l.rows...), nil
}
func (l *usbWireLine) OpenAudio(context.Context, func() bool) (modulecall.Audio, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, "audio")
	return l.audio, nil
}
func (l *usbWireLine) Execute(_ context.Context, a modulecall.Action) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, a.Kind)
	if a.Kind == "dtmf" {
		l.digits = append(l.digits, a.Digit)
	}
	if a.Kind == "dial" {
		l.rows = []modulecall.Record{{Index: 1, Number: a.Number}}
	} else if a.Kind == "answer" {
		l.rows[0].State = 0
	} else if a.Kind == "hangup" {
		l.rows = nil
		l.current = false
	}
	return true, nil
}

type usbWireSource struct{ line *usbWireLine }

func (b usbWireSource) Available(context.Context, string) error { return nil }
func (b usbWireSource) Acquire(context.Context, string) (modulecall.Lease, error) {
	b.line.mu.Lock()
	defer b.line.mu.Unlock()
	if b.line.current {
		return nil, modulecall.ErrBusy
	}
	b.line.current = true
	return b.line, nil
}

type usbWireCarrier struct{ manager *modulecall.Manager }

func (b usbWireCarrier) Ready(ctx context.Context, id string) error {
	if b.manager.Busy(id) {
		return ErrCarrierBusy
	}
	return b.manager.Available(ctx, id)
}
func (b usbWireCarrier) Calls(_ context.Context, id string) ([]vowifi.Call, error) {
	return b.manager.Calls(id)
}
func (b usbWireCarrier) Dial(ctx context.Context, id, number string) (vowifi.Call, error) {
	return b.manager.DialCall(ctx, id, number)
}
func (b usbWireCarrier) Hangup(ctx context.Context, id, cid string) error {
	return b.manager.HangupCall(ctx, id, cid)
}
func (b usbWireCarrier) Media(ctx context.Context, id, cid string) (vowifi.CallMedia, error) {
	return b.manager.CallMedia(ctx, id, cid)
}
func (b usbWireCarrier) Incoming(ctx context.Context, id, cid string) (vowifi.Call, error) {
	calls, err := b.Calls(ctx, id)
	if err != nil {
		return vowifi.Call{}, err
	}
	for _, call := range calls {
		if call.ID == cid && call.Direction == "incoming" && call.State == "ringing" && call.EndedAt == nil {
			return call, nil
		}
	}
	return vowifi.Call{}, ErrCarrierBusy
}
func (b usbWireCarrier) Answer(ctx context.Context, id, cid string) (vowifi.Call, error) {
	call, err := b.manager.AnswerCall(ctx, id, cid)
	if errors.Is(err, modulecall.ErrNotAttempted) {
		err = errors.Join(ErrCarrierAnswerNotAttempted, err)
	}
	return call, err
}

func TestUSBModuleSIPIncomingFirstAnswerAndBothPCMDirections(t *testing.T) {
	audio := &usbWireAudio{syntheticCarrierMedia: &syntheticCarrierMedia{up: make(chan []int16, 8), down: make(chan []int16, 8)}, done: make(chan struct{})}
	line := &usbWireLine{audio: audio, rows: []modulecall.Record{{Index: 2, Direction: 1, State: 4, Number: "+15550000001"}}}
	manager := modulecall.New(context.Background(), usbWireSource{line: line})
	config := carrierTestConfig(t)
	config.Config.CarrierLines[0].AllowIncoming = true
	config.Config.CarrierLines[0].IncomingExtensions = []string{"1001", "1002"}
	backend := usbWireCarrier{manager: manager}
	r, err := start(config, runtimeOptions{carrier: backend, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})
	m := &Manager{runtime: r, carrier: backend}
	one, two := registerMessagePeer(t, r, "1001"), registerMessagePeer(t, r, "1002")
	calls, err := manager.Poll(context.Background(), "module-a")
	if err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
	id := calls[0].ID
	if !m.ForwardIncoming("module-a", id) {
		t.Fatal("module incoming not forwarded")
	}
	first, second := readCallRequest(t, one, sip.INVITE), readCallRequest(t, two, sip.INVITE)
	pcm := testMediaSocket(t)
	writeCall(t, two, calleeResponse(t, two, second, pcm, 180))
	answer := calleeResponse(t, one, first, pcm, 200)
	writeCall(t, one, answer)
	readCallRequest(t, one, sip.ACK)
	replyCallOK(t, two, readCallRequest(t, two, sip.CANCEL))
	writeCall(t, two, calleeResponse(t, two, second, pcm, 200))
	readCallRequest(t, two, sip.ACK)
	replyCallOK(t, two, readCallRequest(t, two, sip.BYE))
	waitCarrier(t, func() bool { return len(r.status().Calls) == 1 && r.status().Calls[0].Phase == "active" })
	line.mu.Lock()
	order := append([]string(nil), line.order...)
	line.mu.Unlock()
	if len(order) != 2 || order[0] != "audio" || order[1] != "answer" {
		t.Fatal("answer was duplicated or preceded duplex audio", order)
	}
	if !m.CarrierOwnsMedia("module-a", id) || !errors.Is(m.ClaimLocalAnswer("module-a", id), ErrCarrierBusy) {
		t.Fatal("SIP winner did not exclude local answer")
	}
	description, err := parseCallSDP(first.Body())
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x7f}, 160)
	packet := append([]byte{0x80, 0, 0, 1, 0, 0, 0, 160, 1, 2, 3, 4}, payload...)
	if _, err = pcm.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: description.port}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-audio.up:
		expected, _ := ims.DecodeG711(payload, "PCMU")
		if !bytes.Equal(pcmBytes(frame), pcmBytes(expected)) {
			t.Fatal("incoming SIP-to-module PCM changed")
		}
	case <-time.After(time.Second):
		t.Fatal("incoming SIP-to-module PCM missing")
	}
	frame := make([]int16, 160)
	for i := range frame {
		frame[i] = -1234
	}
	audio.down <- frame
	_ = pcm.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := pcm.ReadFromUDP(buffer)
	expected, _ := ims.EncodeG711(frame, "PCMU")
	if err != nil || n != 172 || buffer[1]&127 != 0 || !bytes.Equal(buffer[12:n], expected) {
		t.Fatal("incoming module-to-SIP RTP changed", err)
	}
	writeCall(t, one, dialogRequest(t, first, answer, sip.BYE, false, 2))
	readCallFinal(t, one, 200)
	waitCalls(t, r, 0)
	select {
	case <-audio.Done():
	case <-time.After(time.Second):
		t.Fatal("incoming SIP hangup retained module audio")
	}
}

func TestUSBModuleSIPWireOwnsPreparedAudioAndBridgesBothPCMDirections(t *testing.T) {
	audio := &usbWireAudio{syntheticCarrierMedia: &syntheticCarrierMedia{up: make(chan []int16, 8), down: make(chan []int16, 8)}, done: make(chan struct{})}
	line := &usbWireLine{audio: audio}
	manager := modulecall.New(context.Background(), usbWireSource{line: line})
	r, err := start(carrierTestConfig(t), runtimeOptions{carrier: usbWireCarrier{manager: manager}, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})
	caller, cm, invite := prepareCaller(t, r, "7700900001")
	writeCall(t, caller, invite)
	answer := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	waitCarrier(t, func() bool {
		r.mu.Lock()
		leg := r.callLegs[invite.CallID().Value()]
		r.mu.Unlock()
		if leg == nil {
			return false
		}
		bridge := leg.call.carrier.bridge.Load()
		return bridge != nil && leg.call.media.pcm.Load() == bridge
	})
	line.mu.Lock()
	if len(line.order) < 2 || line.order[0] != "audio" || line.order[1] != "dial" {
		t.Fatal("serial call preceded audio preparation", line.order)
	}
	line.mu.Unlock()
	description, err := parseCallSDP(answer.Body())
	if err != nil {
		t.Fatal(err)
	}
	relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: description.port}
	payload := bytes.Repeat([]byte{0xd5}, 160)
	packet := append([]byte{0x80, 8, 0, 1, 0, 0, 0, 160, 1, 2, 3, 4}, payload...)
	if _, err = cm.WriteToUDP(packet, relay); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-audio.up:
		expected, _ := ims.DecodeG711(payload, "PCMA")
		if !bytes.Equal(pcmBytes(frame), pcmBytes(expected)) {
			t.Fatal("SIP-to-module PCM changed")
		}
	case <-time.After(time.Second):
		t.Fatal("SIP-to-module media missing")
	}
	samples := make([]int16, 160)
	for i := range samples {
		samples[i] = 1234
	}
	audio.down <- samples
	_ = cm.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := cm.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := ims.EncodeG711(samples, "PCMA")
	if n != 172 || buffer[1]&127 != 8 || !bytes.Equal(buffer[12:n], expected) {
		t.Fatal("module-to-SIP RTP changed")
	}
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	waitCalls(t, r, 0)
	select {
	case <-audio.Done():
	case <-time.After(time.Second):
		t.Fatal("SIP hangup retained module audio")
	}
}
func pcmBytes(samples []int16) []byte {
	result := make([]byte, len(samples)*2)
	for i, s := range samples {
		result[i*2], result[i*2+1] = byte(s), byte(uint16(s)>>8)
	}
	return result
}
