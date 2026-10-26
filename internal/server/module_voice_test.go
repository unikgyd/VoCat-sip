package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/modulecall"
	"vocat/internal/store"
)

type moduleTestAudio struct {
	*phoneTestMedia
	mu              sync.Mutex
	done            chan struct{}
	started, closed bool
}

func (a *moduleTestAudio) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started = true
	return nil
}
func (a *moduleTestAudio) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.done)
	}
	return nil
}
func (a *moduleTestAudio) Done() <-chan struct{} { return a.done }
func (a *moduleTestAudio) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.done:
		return nil, modulecall.ErrClosed
	case frame := <-a.down:
		return frame, nil
	}
}

type moduleTestDevices struct {
	fakeDeviceController
	mu             sync.Mutex
	active         *moduleTestLease
	rows           []modulecall.Record
	actions        []string
	acquired       []string
	openError      error
	availableError error
}

func (d *moduleTestDevices) ModuleVoiceAvailable(context.Context, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.availableError
}
func (d *moduleTestDevices) AcquireModuleVoice(_ context.Context, id string) (modulecall.Lease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		return nil, modulecall.ErrBusy
	}
	line := &moduleTestLease{d: d}
	d.active = line
	d.acquired = append(d.acquired, id)
	return line, nil
}

type moduleTestLease struct {
	d      *moduleTestDevices
	closed bool
	audio  *moduleTestAudio
}

func (l *moduleTestLease) Current() bool {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	return !l.closed && l.d.active == l
}
func (l *moduleTestLease) Close() {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	l.closed = true
	if l.d.active == l {
		l.d.active = nil
	}
}
func (l *moduleTestLease) Read(ctx context.Context) ([]modulecall.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	if l.closed {
		return nil, modulecall.ErrChanged
	}
	return append([]modulecall.Record(nil), l.d.rows...), nil
}
func (l *moduleTestLease) OpenAudio(context.Context, func() bool) (modulecall.Audio, error) {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	l.d.actions = append(l.d.actions, "audio:open")
	if l.d.openError != nil {
		return nil, l.d.openError
	}
	l.audio = &moduleTestAudio{phoneTestMedia: &phoneTestMedia{down: make(chan []int16, 8), up: make(chan []int16, 8)}, done: make(chan struct{})}
	return l.audio, nil
}
func (l *moduleTestLease) Execute(ctx context.Context, a modulecall.Action) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	if l.closed {
		return false, modulecall.ErrChanged
	}
	l.d.actions = append(l.d.actions, a.Kind)
	switch a.Kind {
	case "dial":
		l.d.rows = []modulecall.Record{{Index: 1, Direction: 0, Number: a.Number}}
	case "answer":
		l.d.rows[0].State = 0
	case "hangup":
		l.d.rows = nil
	}
	return true, nil
}
func newModulePhone(t *testing.T) (*Server, *moduleTestDevices, *phoneTestController) {
	t.Helper()
	s, ims := newPhoneTest(t)
	if err := s.store.UpsertDevice(context.Background(), store.Device{ID: "module-a", Name: "USB module", IMSMode: store.IMSModeModule, USBPath: "/synthetic/usb/a"}); err != nil {
		t.Fatal(err)
	}
	d := &moduleTestDevices{fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "physical-a", Discovered: true, Snapshot: &device.Snapshot{ICCID: "89880000000000000001", IMSI: "515660000000001"}}}}
	d.entry.Candidate.USBPath = "/synthetic/usb/a"
	d.atHandler = func(string) (response modem.Response, err error) {
		t.Error("owned module phone escaped to raw AT")
		return response, modulecall.ErrState
	}
	s.devices = d
	s.moduleCalls = modulecall.New(context.Background(), moduleVoiceBackend{s})
	return s, d, ims
}
func waitModulePhone(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		calls, _ := s.moduleCalls.Calls("module-a")
		if len(calls) == 1 && calls[0].ID == id && calls[0].MediaReady {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("owned USB module audio did not start")
}

func TestUSBModulePhoneActualWebSocketAndPinnedPhysicalSource(t *testing.T) {
	s, d, ims := newModulePhone(t)
	w := phoneRequest(t, s, "GET", "", phoneClientA, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"transport":"cellular"`) {
		t.Fatal(w.Code, w.Body)
	}
	d.mu.Lock()
	if len(d.acquired) != 0 {
		t.Fatal("status opened audio or issued modem commands")
	}
	d.mu.Unlock()
	w = phoneRequest(t, s, "POST", "/dial", phoneClientA, dialRequest)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	token := readPhoneToken(t, w)
	var reply struct {
		Data struct {
			Call struct {
				ID string `json:"id"`
			} `json:"call"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	id := reply.Data.Call.ID
	waitModulePhone(t, s, id)
	d.mu.Lock()
	line := d.active
	audio := line.audio
	if len(d.acquired) != 1 || d.acquired[0] != "physical-a" || len(d.actions) < 2 || d.actions[0] != "audio:open" || d.actions[1] != "dial" {
		t.Fatal(d.acquired, d.actions)
	}
	d.mu.Unlock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.routePhoneAPI(w, r) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/phone/media?device_id=module-a&call_id=" + id
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{"vocat-pcm", "lease-" + token}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	audio.down <- []int16{-1234, 1234}
	_, data, err := conn.Read(ctx)
	if err != nil || len(data) != 4 || int16(binary.LittleEndian.Uint16(data)) != -1234 {
		t.Fatal(data, err)
	}
	up := make([]byte, 320)
	binary.LittleEndian.PutUint16(up, 4321)
	if err = conn.Write(ctx, websocket.MessageBinary, up); err != nil {
		t.Fatal(err)
	}
	select {
	case samples := <-audio.up:
		if samples[0] != 4321 {
			t.Fatal(samples)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	w = phoneRequest(t, s, "POST", "/dtmf", phoneClientA, `{"device_id":"module-a","call_id":"`+id+`","digit":"5"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = phoneRequest(t, s, "POST", "/hangup", phoneClientA, `{"device_id":"module-a","call_id":"`+id+`"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	if ims.dials != 0 || ims.hangs != 0 {
		t.Fatal("module call touched an IMS runtime")
	}
}

func TestUSBModulePhoneAudioFailureAndModeMaintenanceAdmission(t *testing.T) {
	s, d, _ := newModulePhone(t)
	d.openError = errors.New("synthetic audio refusal")
	w := phoneRequest(t, s, "POST", "/dial", phoneClientA, dialRequest)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	d.mu.Lock()
	for _, action := range d.actions {
		if action == "dial" {
			t.Fatal("audio failure still dialed")
		}
	}
	d.openError = nil
	d.mu.Unlock()
	call, err := (sipCarrierBackend{s}).Dial(context.Background(), "module-a", "+15550000002")
	if err != nil {
		t.Fatal(err)
	}
	config, err := s.store.Device(context.Background(), "module-a")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.requireModuleCallsIdle(context.Background(), config); err == nil {
		t.Fatal("a preparing module call allowed mode/SIM maintenance")
	}
	if _, err = (sipCarrierBackend{s}).Dial(context.Background(), "module-a", "+15550000003"); err == nil {
		t.Fatal("a second caller acquired the same module")
	}
	if call.ID == "" {
		t.Fatal("module call has no original identity")
	}
}

func TestUSBModulePhoneIncomingFirstBrowserAndTelegramShareOwner(t *testing.T) {
	s, d, ims := newModulePhone(t)
	d.mu.Lock()
	d.rows = []modulecall.Record{{Index: 2, Direction: 1, State: 4, Number: "+15550000002"}}
	d.mu.Unlock()
	calls, err := s.moduleCalls.Poll(context.Background(), "module-a")
	if err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
	id := calls[0].ID
	body := `{"device_id":"module-a","call_id":"` + id + `"}`
	w := phoneRequest(t, s, "POST", "/answer", phoneClientA, body)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	readPhoneToken(t, w)
	if w = phoneRequest(t, s, "POST", "/answer", phoneClientB, body); w.Code != 409 {
		t.Fatal("second browser answered", w.Code, w.Body)
	}
	waitModulePhone(t, s, id)
	config, err := s.store.Device(context.Background(), "module-a")
	if err != nil {
		t.Fatal(err)
	}
	bot := &telegramBot{server: s}
	transport, controller, err := bot.telegramCallTransport(config, d.entry)
	if err != nil || transport != "module_usb" || controller != s.moduleCalls {
		t.Fatal(transport, err)
	}
	if _, err = bot.executeSimpleVoWiFiCallAction(context.Background(), config.ID, "answer", controller); err == nil {
		t.Fatal("Telegram answered the browser's active call again")
	}
	result, err := bot.executeSimpleVoWiFiCallAction(context.Background(), config.ID, "hangup", controller)
	if err != nil || !strings.Contains(result, "模组 USB 语音") {
		t.Fatal(result, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Join(d.actions, ",") != "audio:open,answer,hangup" || ims.answers != 0 || ims.hangs != 0 {
		t.Fatal(d.actions, "wrong or duplicated call owner")
	}
}

func TestUSBModuleSourceReadCancellationDoesNotReleaseReservation(t *testing.T) {
	s, d, _ := newModulePhone(t)
	lease, err := (moduleVoiceBackend{s}).Acquire(context.Background(), "module-a")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = lease.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled source read did not preserve cancellation", err)
	}
	if !lease.Current() {
		t.Fatal("canceled database read invalidated a live physical source")
	}
	if _, err = d.AcquireModuleVoice(context.Background(), "physical-a"); !errors.Is(err, modulecall.ErrBusy) {
		t.Fatal("canceled read released the live physical reservation", err)
	}
}

func TestUSBModuleRebindingStopsMediaAndStaleCleanupCannotReachIMS(t *testing.T) {
	s, d, ims := newModulePhone(t)
	call, err := (sipCarrierBackend{s}).Dial(context.Background(), "module-a", "+15550000001")
	if err != nil {
		t.Fatal(err)
	}
	waitModulePhone(t, s, call.ID)
	d.mu.Lock()
	audio := d.active.audio
	d.mu.Unlock()
	config, err := s.store.Device(context.Background(), "module-a")
	if err != nil {
		t.Fatal(err)
	}
	config.IMSMode = store.IMSModeVoWiFi
	if err = s.store.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	select {
	case <-audio.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("changed source retained media")
	}
	if err = (sipCarrierBackend{s}).Hangup(context.Background(), "module-a", call.ID); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Join(d.actions, ",") != "audio:open,dial" || ims.hangs != 0 {
		t.Fatal("old cleanup reached a changed source", d.actions)
	}
}

func TestUSBModuleTelegramPreservesSignalingOnlyToolsButNeverEscapesLiveOwner(t *testing.T) {
	s, d, _ := newModulePhone(t)
	config, err := s.store.Device(context.Background(), "module-a")
	if err != nil {
		t.Fatal(err)
	}
	bot := &telegramBot{server: s}
	d.mu.Lock()
	d.availableError = modulecall.ErrUnavailable // A modem without a paired UAC.
	d.mu.Unlock()
	transport, controller, err := bot.telegramCallTransport(config, d.entry)
	if err != nil || transport != "cellular" || controller != nil {
		t.Fatal("legacy signaling-only Telegram tools were removed", transport, err)
	}
	d.mu.Lock()
	d.availableError = nil
	d.mu.Unlock()
	call, err := s.moduleCalls.DialCall(context.Background(), "module-a", "+15550000001")
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.availableError = modulecall.ErrUnavailable
	d.mu.Unlock()
	transport, controller, err = bot.telegramCallTransport(config, d.entry)
	if err != nil || transport != "module_usb" || controller != s.moduleCalls {
		t.Fatal("a live USB call escaped to raw AT after readiness changed", transport, err)
	}
	if err = controller.HangupCall(context.Background(), "module-a", call.ID); err != nil {
		t.Fatal(err)
	}
}
