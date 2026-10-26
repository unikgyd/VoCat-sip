package sipgateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

type syntheticCarrierMedia struct {
	down chan []int16
	up   chan []int16
}

func (m *syntheticCarrierMedia) Codec() string { return "AMR" } // PCM boundary only; not a real codec acceptance test
func (m *syntheticCarrierMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case b := <-m.down:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (m *syntheticCarrierMedia) WritePCM(b []int16) error {
	select {
	case m.up <- append([]int16(nil), b...):
		return nil
	default:
		return errors.New("test media queue full")
	}
}

type syntheticCarrier struct {
	mu         sync.Mutex
	calls      map[string]vowifi.Call
	media      map[string]*syntheticCarrierMedia
	dials      [][2]string
	hangups    int
	readyError error
	pauseDial  <-chan struct{}
	entered    chan struct{}
}

func newSyntheticCarrier() *syntheticCarrier {
	return &syntheticCarrier{calls: map[string]vowifi.Call{}, media: map[string]*syntheticCarrierMedia{}}
}
func (b *syntheticCarrier) Ready(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readyError
}
func (b *syntheticCarrier) Calls(ctx context.Context, id string) ([]vowifi.Call, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c, ok := b.calls[id]; ok {
		return []vowifi.Call{c}, nil
	}
	return nil, nil
}
func (b *syntheticCarrier) Dial(ctx context.Context, id, number string) (vowifi.Call, error) {
	if ctx.Err() != nil {
		return vowifi.Call{}, ctx.Err()
	}
	b.mu.Lock()
	b.dials = append(b.dials, [2]string{id, number})
	call := vowifi.Call{ID: randomToken(), Number: number, Direction: "outgoing", State: "ringing", StartedAt: time.Now()}
	b.calls[id] = call
	b.media[call.ID] = &syntheticCarrierMedia{down: make(chan []int16, 16), up: make(chan []int16, 16)}
	pause, entered := b.pauseDial, b.entered
	b.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if pause != nil {
		<-pause
	} // simulate a late known ID, even after cancellation
	return call, nil
}
func (b *syntheticCarrier) Hangup(_ context.Context, id, callID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	call := b.calls[id]
	if call.ID != callID {
		return errors.New("wrong module or call")
	}
	now := time.Now()
	call.State, call.EndedAt, call.MediaReady = "ended", &now, false
	b.calls[id] = call
	b.hangups++
	return nil
}
func (b *syntheticCarrier) Media(_ context.Context, id, callID string) (vowifi.CallMedia, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls[id].ID != callID {
		return nil, ErrCarrierMedia
	}
	return b.media[callID], nil
}
func (b *syntheticCarrier) activate(id string) *syntheticCarrierMedia {
	b.mu.Lock()
	defer b.mu.Unlock()
	call := b.calls[id]
	call.State, call.MediaReady = "active", true
	b.calls[id] = call
	return b.media[call.ID]
}
func (b *syntheticCarrier) dialCount() int   { b.mu.Lock(); defer b.mu.Unlock(); return len(b.dials) }
func (b *syntheticCarrier) hangupCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.hangups }
func waitCarrier(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("synthetic carrier event timed out")
	}
}

func carrierTestConfig(t *testing.T) stored {
	c := testConfig(t, "SHA-256")
	c.Config.CallsEnabled, c.Config.CarrierEnabled = true, true
	c.Config.IdentityDomain = "voice.example.test"
	c.Config.CarrierLines = []CarrierLine{{DeviceID: "module-a", Label: "Line A", Default: true, AllowOutgoing: true, CountryCode: "44", NationalNumberLength: 10}, {DeviceID: "module-b", Label: "Line B", Prefix: "82", AllowOutgoing: true}}
	for i := range c.Config.Accounts {
		c.Config.Accounts[i].AllowCalls = true
		c.Config.Accounts[i].OutgoingLines = []string{"module-a", "module-b"}
	}
	return c
}
func carrierTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "carrier-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func startCarrierTest(t *testing.T, config stored, backend *syntheticCarrier, db repository) *runtime {
	t.Helper()
	r, err := start(config, runtimeOptions{carrier: backend, journal: &callJournal{db: db}})
	if err != nil {
		t.Fatal(err)
	}
	// Listener goroutines already exist. Publish fixture timers through the
	// same mutex every subsequent INVITE admission takes before running a call.
	r.mu.Lock()
	r.callRingTimeout, r.callAckTimeout, r.callLateTimeout = 2*time.Second, time.Second, 100*time.Millisecond
	r.mu.Unlock()
	t.Cleanup(r.close)
	return r
}

func TestCarrierRouteExplicitPermissionAndNormalization(t *testing.T) {
	c := carrierTestConfig(t).Config
	for _, tc := range []struct {
		dial, line, number string
		code               int
	}{
		{"7700900001", "module-a", "+447700900001", 0}, {"+15550000001", "module-a", "+15550000001", 0},
		{"82+15550000002", "module-b", "+15550000002", 0}, {"1003", "", "", 484}, {"112", "", "", 484},
		{"82", "", "", 484}, {"+0000000", "", "", 484}, {"7700;BYE", "", "", 484},
	} {
		t.Run(tc.dial, func(t *testing.T) {
			line, number, code := carrierRoute(c, c.Accounts[0], tc.dial)
			if line.DeviceID != tc.line || number != tc.number || code != tc.code {
				t.Fatalf("route=%s %s %d", line.DeviceID, number, code)
			}
		})
	}
	account := c.Accounts[0]
	account.OutgoingLines = nil
	if _, _, code := carrierRoute(c, account, "+15550000001"); code != 403 {
		t.Fatal("default line implied permission")
	}
	c.CarrierEnabled = false
	if _, _, code := carrierRoute(c, c.Accounts[0], "+15550000001"); code != 404 {
		t.Fatal("disabled external calls routed")
	}
}

func TestCarrierConfigValidationAndDeepCopy(t *testing.T) {
	base := carrierTestConfig(t).Config
	for _, mutate := range []func(*Config){
		func(c *Config) { c.CarrierLines[1].Default = true }, func(c *Config) { c.CarrierLines[1].DeviceID = c.CarrierLines[0].DeviceID },
		func(c *Config) { c.CarrierLines[0].Prefix = "82" }, func(c *Config) { c.CarrierLines[0].CountryCode = "+44" },
		func(c *Config) { c.CarrierLines[0].NationalNumberLength = 3 }, func(c *Config) { c.Accounts[0].OutgoingLines = []string{"missing"} },
		func(c *Config) { c.Accounts[0].OutgoingLines = []string{"module-a", "module-a"} },
	} {
		c := cloneConfig(base)
		mutate(&c)
		if prepareCarrierConfig(&c) == nil {
			t.Fatal("invalid carrier config accepted")
		}
	}
	copy := cloneConfig(base)
	copy.CarrierLines[0].Label = "changed"
	copy.Accounts[0].OutgoingLines[0] = "changed"
	if base.CarrierLines[0].Label != "Line A" || base.Accounts[0].OutgoingLines[0] != "module-a" {
		t.Fatal("nested configuration aliases retained")
	}
}

func TestCarrierJournalPersistsBeforeDialAndRejectsChangedRetry(t *testing.T) {
	db := carrierTestStore(t)
	j := &callJournal{db: db}
	if code, err := j.reserve(context.Background(), "realm", "1001", "same-id", "module-a", "+15550000001"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	reopened := &callJournal{db: db}
	if code, _ := reopened.reserve(context.Background(), "realm", "1001", "same-id", "module-a", "+15550000001"); code != 482 {
		t.Fatal("listener restart lost paid-operation reservation")
	}
	if code, _ := reopened.reserve(context.Background(), "realm", "1001", "same-id", "module-b", "+15550000002"); code != 400 {
		t.Fatal("same call changed target")
	}
	value, err := db.AppSetting(context.Background(), CallOperationSettingKey)
	if err != nil || !value.Sensitive || bytes.Contains(value.Value, []byte("1555000")) || bytes.Contains(value.Value, []byte("module-a")) || bytes.Contains(value.Value, []byte("same-id")) {
		t.Fatal("operation store contains raw identity/number or is not private")
	}
}

func TestCarrierWireBidirectionalPCMAndCallerHangup(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	r.mu.Lock()
	r.callRingTimeout = 400 * time.Millisecond
	r.mu.Unlock()
	caller, cm, invite := prepareCaller(t, r, "7700900001")
	writeCall(t, caller, invite)
	waitCarrier(t, func() bool { return backend.dialCount() == 1 })
	backend.mu.Lock()
	dial := backend.dials[0]
	backend.mu.Unlock()
	if dial != [2]string{"module-a", "+447700900001"} {
		t.Fatalf("dial=%v", dial)
	}
	media := backend.activate("module-a")
	answer := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	// Signaling and RTP use different UDP sockets: receiving 200 and writing
	// ACK does not mean the ACK or the asynchronously attached PCM consumer
	// has been observed. This test sends exactly one speech packet, so wait
	// for both real readiness conditions rather than racing an intentional
	// pre-bridge RTP drop (a live source would keep sending later packets).
	waitCarrier(t, func() bool {
		r.mu.Lock()
		leg := r.callLegs[invite.CallID().Value()]
		r.mu.Unlock()
		if leg == nil {
			return false
		}
		call := leg.call
		call.mu.Lock()
		ready := call.acked && call.phase == "active" && !call.stopped
		call.mu.Unlock()
		bridge := call.carrier.bridge.Load()
		return ready && bridge != nil && call.media.pcm.Load() == bridge
	})
	description, err := parseCallSDP(answer.Body())
	if err != nil {
		t.Fatal(err)
	}
	relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: description.port}
	payload := bytes.Repeat([]byte{0xd5}, 160)
	packet := append([]byte{0x80, 8, 0, 1, 0, 0, 0, 160, 1, 2, 3, 4}, payload...)
	_, _ = cm.WriteToUDP(packet, relay)
	select {
	case pcm := <-media.up:
		expected, _ := ims.DecodeG711(payload, "PCMA")
		if fmt.Sprint(pcm) != fmt.Sprint(expected) {
			t.Fatal("uplink PCM corrupted")
		}
	case <-time.After(time.Second):
		t.Fatal("no SIP-to-carrier PCM")
	}
	// Duplicate RTP must not replay the caller's speech into the carrier leg.
	_, _ = cm.WriteToUDP(packet, relay)
	select {
	case <-media.up:
		t.Fatal("duplicate RTP replayed")
	case <-time.After(30 * time.Millisecond):
	}
	samples := make([]int16, 320)
	for i := range samples {
		samples[i] = 1000
	}
	media.down <- samples
	cm.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, source, err := cm.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	first := append([]byte(nil), buffer[:n]...)
	n, _, err = cm.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := ims.EncodeG711(samples[:160], "PCMA")
	if n != 172 || source.Port != relay.Port || buffer[1]&127 != 8 || !bytes.Equal(buffer[12:n], expected) || binary.BigEndian.Uint16(buffer[2:4]) != binary.BigEndian.Uint16(first[2:4])+1 || binary.BigEndian.Uint32(buffer[4:8]) != binary.BigEndian.Uint32(first[4:8])+160 {
		t.Fatal("carrier-to-SIP RTP framing or PCM corrupted")
	}
	time.Sleep(450 * time.Millisecond)
	if calls := r.status().Calls; len(calls) != 1 || calls[0].Phase != "active" {
		t.Fatal("stopped ring timer ended an active carrier call")
	}
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
	port, err := net.ListenUDP("udp", relay)
	if err != nil {
		t.Fatal("SIP media port retained after hangup", err)
	}
	_ = port.Close()
}

func TestCarrierUnavailableDoesNotFallBackToAnotherModule(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	backend.readyError = ErrCarrierUnavailable
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	caller, _, invite := prepareCaller(t, r, "82+15550000002")
	writeCall(t, caller, invite)
	readCallFinal(t, caller, 503)
	waitCalls(t, r, 0)
	if backend.dialCount() != 0 {
		t.Fatal("offline line dialled another module")
	}
}

func TestCarrierKnownExtensionAlwaysWinsOverNationalRules(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	caller, _, invite := prepareCaller(t, r, "1002")
	writeCall(t, caller, invite)
	readCallFinal(t, caller, 480)
	if backend.dialCount() != 0 {
		t.Fatal("unregistered known extension leaked into carrier")
	}
}

func TestCarrierCallerCancelAndLateDialReturnAreCleaned(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	paused, entered := make(chan struct{}), make(chan struct{}, 1)
	backend.pauseDial, backend.entered = paused, entered
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	caller, _, invite := prepareCaller(t, r, "+15550000001")
	writeCall(t, caller, invite)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dial not entered")
	}
	writeCall(t, caller, cancelRequest(invite))
	readCallFinal(t, caller, 200)
	readCallFinal(t, caller, 487)
	waitCalls(t, r, 0)
	close(paused)
	waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
	if backend.dialCount() != 1 {
		t.Fatal("cancelled carrier attempt retried")
	}
}

func TestCarrierShutdownDoesNotWaitForDialOrModemRestore(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	paused, entered := make(chan struct{}), make(chan struct{}, 1)
	backend.pauseDial, backend.entered = paused, entered
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	caller, _, invite := prepareCaller(t, r, "+15550000001")
	writeCall(t, caller, invite)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dial not entered")
	}
	start := time.Now()
	r.close()
	if time.Since(start) > time.Second {
		t.Fatal("SIP shutdown waited for carrier")
	}
	close(paused)
	waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
}

func TestCarrierJournalCorruptionFailsBeforeDial(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	_ = db.UpsertAppSetting(context.Background(), store.AppSetting{Key: CallOperationSettingKey, Value: json.RawMessage(`{"version":99}`), Sensitive: true})
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	caller, _, invite := prepareCaller(t, r, "+15550000001")
	writeCall(t, caller, invite)
	readCallFinal(t, caller, 503)
	if backend.dialCount() != 0 {
		t.Fatal("dial escaped failed durable reservation")
	}
}

func prepareNamedCarrierCaller(t *testing.T, r *runtime, user, target string) (*wireClient, *net.UDPConn, *sip.Request) {
	t.Helper()
	c, media := newWireClient(t, r), testMediaSocket(t)
	c.callID = "carrier-" + randomToken()
	initial := newCallerInvite(t, c, media, target, "")
	initial.From().Address.User = user
	res := c.exchange(initial.String())
	requireStatus(t, res, 401)
	parsed, err := registrarParser().ParseSIP([]byte(res.raw))
	if err != nil {
		t.Fatal(err)
	}
	ack := dialogRequest(t, initial, parsed.(*sip.Response), sip.ACK, true, 2)
	ack.RemoveHeader("Via")
	ack.PrependHeader(sip.HeaderClone(initial.Via()))
	ack.Recipient = *initial.Recipient.Clone()
	writeCall(t, c, ack)
	challenge, err := digest.ParseChallenge(res.headers["www-authenticate"][0])
	if err != nil {
		t.Fatal(err)
	}
	password := testPassword
	if user == "1002" {
		password += "-two"
	}
	auth, err := digest.Digest(challenge, digest.Options{Method: "INVITE", URI: initial.Recipient.String(), Username: user, Password: password, Count: 1, Cnonce: randomToken()})
	if err != nil {
		t.Fatal(err)
	}
	invite := newCallerInvite(t, c, media, target, "Authorization: "+auth.String()+"\r\n")
	invite.From().Address.User = user
	return c, media, invite
}

func TestCarrierMultipleModulesParallelAndSameModuleBusy(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	r := startCarrierTest(t, carrierTestConfig(t), backend, db)
	a, _, aInvite := prepareNamedCarrierCaller(t, r, "1001", "+15550000001")
	writeCall(t, a, aInvite)
	waitCarrier(t, func() bool { return backend.dialCount() == 1 })
	busy, _, busyInvite := prepareNamedCarrierCaller(t, r, "1002", "+15550000002")
	writeCall(t, busy, busyInvite)
	readCallFinal(t, busy, 486)
	b, _, bInvite := prepareNamedCarrierCaller(t, r, "1002", "82+15550000002")
	writeCall(t, b, bInvite)
	waitCarrier(t, func() bool { return backend.dialCount() == 2 })
	backend.activate("module-a")
	backend.activate("module-b")
	aAnswer, bAnswer := readCallFinal(t, a, 200), readCallFinal(t, b, 200)
	writeCall(t, a, dialogRequest(t, aInvite, aAnswer, sip.ACK, true, 2))
	writeCall(t, b, dialogRequest(t, bInvite, bAnswer, sip.ACK, true, 2))
	waitCalls(t, r, 2)
	writeCall(t, a, dialogRequest(t, aInvite, aAnswer, sip.BYE, true, 3))
	readCallFinal(t, a, 200)
	waitCalls(t, r, 1)
	backend.mu.Lock()
	other := backend.calls["module-b"]
	backend.mu.Unlock()
	if other.State != "active" {
		t.Fatal("one module's hangup ended the other module")
	}
	writeCall(t, b, dialogRequest(t, bInvite, bAnswer, sip.BYE, true, 3))
	readCallFinal(t, b, 200)
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { return backend.hangupCount() == 2 })
}

func TestCarrierRetryAfterListenerRestartDoesNotRedial(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	config := carrierTestConfig(t)
	r := startCarrierTest(t, config, backend, db)
	a, _, invite := prepareCaller(t, r, "+15550000001")
	writeCall(t, a, invite)
	waitCarrier(t, func() bool { return backend.dialCount() == 1 })
	backend.activate("module-a")
	answer := readCallFinal(t, a, 200)
	writeCall(t, a, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	writeCall(t, a, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, a, 200)
	waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
	r.close()
	restarted := startCarrierTest(t, config, backend, db)
	retry, _, request := prepareCaller(t, restarted, "+15550000001")
	*request.CallID() = sip.CallIDHeader(invite.CallID().Value())
	writeCall(t, retry, request)
	readCallFinal(t, retry, 482)
	if backend.dialCount() != 1 {
		t.Fatal("listener restart repeated paid call")
	}
}

func TestCarrierAccountPermissionFailsBeforeDial(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	config := carrierTestConfig(t)
	config.Config.Accounts[0].OutgoingLines = nil
	r := startCarrierTest(t, config, backend, db)
	caller, _, invite := prepareCaller(t, r, "+15550000001")
	writeCall(t, caller, invite)
	readCallFinal(t, caller, 403)
	if backend.dialCount() != 0 {
		t.Fatal("account without line permission dialled")
	}
}

func TestCarrierRemoteHangupAndLostCallerAckEndOtherLeg(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(fmt.Sprint(lostAck), func(t *testing.T) {
			backend, db := newSyntheticCarrier(), carrierTestStore(t)
			r := startCarrierTest(t, carrierTestConfig(t), backend, db)
			r.mu.Lock()
			r.callAckTimeout = 150 * time.Millisecond
			r.mu.Unlock()
			caller, _, invite := prepareCaller(t, r, "+15550000001")
			writeCall(t, caller, invite)
			waitCarrier(t, func() bool { return backend.dialCount() == 1 })
			backend.activate("module-a")
			answer := readCallFinal(t, caller, 200)
			if !lostAck {
				writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
				backend.mu.Lock()
				call := backend.calls["module-a"]
				now := time.Now()
				call.State, call.EndedAt = "ended", &now
				backend.calls["module-a"] = call
				backend.mu.Unlock()
			}
			bye := readCallRequest(t, caller, sip.BYE)
			replyCallOK(t, caller, bye)
			waitCalls(t, r, 0)
			waitCarrier(t, func() bool { return backend.hangupCount() == 1 })
		})
	}
}

func TestCarrierManagerRestoresOnlyExplicitSavedPermissions(t *testing.T) {
	backend, db := newSyntheticCarrier(), carrierTestStore(t)
	m := New(context.Background(), db, WithCarrierBackend(backend))
	t.Cleanup(m.Close)
	if m.Snapshot().Config.CarrierEnabled || len(m.Snapshot().Config.CarrierLines) != 0 {
		t.Fatal("carrier access enabled by default")
	}
	config := carrierTestConfig(t).Config
	config.Listen = unusedAddress(t)
	for i := range config.Accounts {
		config.Accounts[i].Password = testPassword
		if config.Accounts[i].Username == "1002" {
			config.Accounts[i].Password += "-two"
		}
	}
	if _, err := m.Apply(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	view := m.Snapshot()
	view.Config.Accounts[0].OutgoingLines[0] = "changed"
	view.Config.CarrierLines[0].DeviceID = "changed"
	if m.Snapshot().Config.Accounts[0].OutgoingLines[0] != "module-a" || m.Snapshot().Config.CarrierLines[0].DeviceID != "module-a" {
		t.Fatal("snapshot mutated running carrier permissions")
	}
	m.Close()
	reopened := New(context.Background(), db, WithCarrierBackend(backend))
	defer reopened.Close()
	if view := reopened.Snapshot(); !view.Status.Running || !view.Status.Capabilities.CarrierOutgoing || !view.Config.CarrierEnabled || len(view.Config.Accounts[0].OutgoingLines) != 2 {
		t.Fatalf("carrier config did not restore: %+v", view.Status)
	}
	if backend.dialCount() != 0 {
		t.Fatal("saving or restoring carrier settings made a call")
	}
}

func TestCarrierStopCancelsBackendContextSynchronously(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := &sipCall{stop: make(chan struct{}), carrier: &carrierLeg{cancel: cancel}}
	call.stopCall("caller")
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("backend could continue submission until the SIP worker is scheduled")
	}
}
