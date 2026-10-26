package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type phoneTestMedia struct {
	down chan []int16
	up   chan []int16
}

func (*phoneTestMedia) Codec() string { return "PCMA" }
func (m *phoneTestMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s := <-m.down:
		return s, nil
	}
}
func (m *phoneTestMedia) WritePCM(s []int16) error {
	select {
	case m.up <- s:
		return nil
	default:
		return errors.New("full")
	}
}

type phoneTestController struct {
	fakeVoWiFiController
	mu                    sync.Mutex
	calls                 []vowifi.Call
	dials, answers, hangs int
	dialError             error
	dialWait              <-chan struct{}
	media                 *phoneTestMedia
}

func (c *phoneTestController) Calls(string) ([]vowifi.Call, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]vowifi.Call(nil), c.calls...), nil
}
func (c *phoneTestController) DialCall(ctx context.Context, _ string, number string) (vowifi.Call, error) {
	c.mu.Lock()
	c.dials++
	wait := c.dialWait
	c.mu.Unlock()
	if wait != nil {
		<-wait
	} // Deliberately return a known ID even after cancellation.
	c.mu.Lock()
	defer c.mu.Unlock()
	call := vowifi.Call{ID: "synthetic-outgoing", Number: number, Direction: "outgoing", State: "active", MediaReady: true, StartedAt: time.Now()}
	c.calls = []vowifi.Call{call}
	return call, c.dialError
}
func (c *phoneTestController) AnswerCall(context.Context, string, string) (vowifi.Call, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers++
	if len(c.calls) == 0 || c.calls[0].State != "ringing" {
		return vowifi.Call{}, errors.New("already answered")
	}
	c.calls[0].State = "active"
	c.calls[0].MediaReady = true
	return c.calls[0], nil
}
func (c *phoneTestController) HangupCall(_ context.Context, _ string, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hangs++
	for i := range c.calls {
		if c.calls[i].ID == id {
			now := time.Now()
			c.calls[i].EndedAt = &now
			c.calls[i].State = "ended"
		}
	}
	return nil
}
func (c *phoneTestController) CallMedia(_ context.Context, _, id string) (vowifi.CallMedia, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		if call.ID == id && call.State == "active" {
			return c.media, nil
		}
	}
	return nil, errors.New("not active")
}
func newPhoneTest(t *testing.T) (*Server, *phoneTestController) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.UpsertDevice(context.Background(), store.Device{ID: "module-a", Name: "Synthetic A"}); err != nil {
		t.Fatal(err)
	}
	c := &phoneTestController{fakeVoWiFiController: fakeVoWiFiController{state: vowifi.State{Enabled: true, SIMReady: true, IMSReady: true, IMSI: "001010000000001", ICCID: "synthetic"}}, media: &phoneTestMedia{make(chan []int16, 8), make(chan []int16, 8)}}
	s := &Server{store: db, vowifi: c, maxRequestBodyBytes: 4096, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), devices: fakeDeviceController{entry: device.Device{ID: "module-a", Discovered: true, Snapshot: &device.Snapshot{ICCID: "synthetic"}}, atHandler: func(string) (modem.Response, error) {
		t.Error("web phone must never use AT")
		return modem.Response{}, errors.New("AT forbidden")
	}}}
	s.sipGateway = sipgateway.New(context.Background(), db, sipgateway.WithCarrierBackend(sipCarrierBackend{s}))
	s.webPhone = newWebPhone(s)
	t.Cleanup(s.CloseSIPGateway)
	return s, c
}
func phoneRequest(t *testing.T, s *Server, method, path, client, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/api/phone"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-VoCat-Phone-Client", client)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "synthetic-session"})
	w := httptest.NewRecorder()
	if !s.routePhoneAPI(w, r) {
		t.Fatal("not routed")
	}
	return w
}

const phoneClientA = "browser_a_0000000000000001"
const phoneClientB = "browser_b_0000000000000002"
const dialRequest = `{"device_id":"module-a","number":"+15550000001","operation_id":"operation_000000000000001"}`

func readPhoneToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var reply struct {
		Data struct {
			AudioToken string `json:"audio_token"`
		} `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Data.AudioToken == "" {
		t.Fatalf("missing token %s", w.Body.String())
	}
	return reply.Data.AudioToken
}
func TestWebPhoneStrictIMSAndNoAutomaticNumberOrATFallback(t *testing.T) {
	s, c := newPhoneTest(t)
	for _, number := range []string{"1001", "15550000001", "+0", "+1555;ATH"} {
		w := phoneRequest(t, s, "POST", "/dial", phoneClientA, strings.Replace(dialRequest, "+15550000001", number, 1))
		if w.Code != 400 {
			t.Fatalf("accepted %s: %d", number, w.Code)
		}
	}
	c.state.IMSReady = false
	if w := phoneRequest(t, s, "POST", "/dial", phoneClientA, dialRequest); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := phoneRequest(t, s, "GET", "", phoneClientA, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"ready":false`)) {
		t.Fatal(w.Body.String())
	}
	if c.dials != 0 {
		t.Fatal("unregistered dial submitted")
	}
}
func TestWebPhoneDurableOperationAndExclusiveBrowserOwnership(t *testing.T) {
	s, c := newPhoneTest(t)
	w := phoneRequest(t, s, "POST", "/dial", phoneClientA, dialRequest)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	token := readPhoneToken(t, w)
	if w = phoneRequest(t, s, "GET", "", phoneClientA, ""); !bytes.Contains(w.Body.Bytes(), []byte(`"audio_owner":"this_browser"`)) || bytes.Contains(w.Body.Bytes(), []byte(token)) {
		t.Fatal(w.Body.String())
	}
	if w = phoneRequest(t, s, "GET", "", phoneClientB, ""); !bytes.Contains(w.Body.Bytes(), []byte(`"audio_owner":"other_browser"`)) {
		t.Fatal(w.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, ok := s.webPhone.attach("module-a", "synthetic-outgoing", "wrong", cancel); ok {
		t.Fatal("bad token")
	}
	l, ok := s.webPhone.attach("module-a", "synthetic-outgoing", token, cancel)
	if !ok {
		t.Fatal("lease missing")
	}
	if _, ok = s.webPhone.attach("module-a", "synthetic-outgoing", token, cancel); ok {
		t.Fatal("two audio consumers")
	}
	if _, err := s.webPhone.reserve("module-b", l.client); err == nil {
		t.Fatal("one tab got two audio calls")
	}
	second, err := s.webPhone.reserve("module-b", "another-client")
	if err != nil {
		t.Fatal("modules coupled", err)
	}
	s.webPhone.drop(second, false)
	s.webPhone.drop(l, true)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("media not cancelled")
	}
	waitPhoneHang(t, c, 1)
	// A listener restart and a different tab must not replay the paid operation.
	s.sipGateway.Close()
	s.sipGateway = sipgateway.New(context.Background(), s.store)
	w = phoneRequest(t, s, "POST", "/dial", phoneClientB, dialRequest)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dials != 1 {
		t.Fatal("duplicate carrier Dial", c.dials)
	}
}
func waitPhoneHang(t *testing.T, c *phoneTestController, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := c.hangs
		c.mu.Unlock()
		if n >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("hangup missing")
}
func TestWebPhoneConcurrentAnswerAndDisconnectCleanup(t *testing.T) {
	s, c := newPhoneTest(t)
	c.calls = []vowifi.Call{{ID: "incoming", Direction: "incoming", State: "ringing", StartedAt: time.Now()}}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, client := range []string{phoneClientA, phoneClientB} {
		wg.Add(1)
		go func(client string) {
			defer wg.Done()
			w := phoneRequest(t, s, "POST", "/answer", client, `{"device_id":"module-a","call_id":"incoming"}`)
			codes <- w.Code
		}(client)
	}
	wg.Wait()
	close(codes)
	accepted := 0
	for code := range codes {
		if code == 202 {
			accepted++
		} else if code != 409 {
			t.Fatal(code)
		}
	}
	if accepted != 1 || c.answers != 1 {
		t.Fatal("duplicate answer", accepted, c.answers)
	}
	s.webPhone.mu.Lock()
	l := s.webPhone.leases["module-a"]
	s.webPhone.mu.Unlock()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, ok := s.webPhone.attach(l.device, l.call, l.token, cancel); !ok {
		t.Fatal("attach failed")
	}
	s.webPhone.detach(l)
	s.webPhone.sweep(time.Now().Add(4 * time.Second))
	waitPhoneHang(t, c, 1)
	// Expired old lease must never affect a replacement call's ownership.
	newLease, err := s.webPhone.reserve("module-a", "next")
	if err != nil {
		t.Fatal(err)
	}
	s.webPhone.drop(l, true)
	s.webPhone.mu.Lock()
	if s.webPhone.leases["module-a"] != newLease {
		t.Error("old call removed new lease")
	}
	s.webPhone.mu.Unlock()
}
func TestWebPhoneCancelledDialCleansLateKnownCall(t *testing.T) {
	s, c := newPhoneTest(t)
	gate := make(chan struct{})
	c.dialWait = gate
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/api/phone/dial", strings.NewReader(dialRequest)).WithContext(ctx)
	r.Header.Set("X-VoCat-Phone-Client", phoneClientA)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "synthetic-session"})
	done := make(chan struct{})
	go func() { defer close(done); s.routePhoneAPI(httptest.NewRecorder(), r) }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		n := c.dials
		c.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dial not reached")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	closedAt := time.Now()
	s.CloseSIPGateway()
	if time.Since(closedAt) > time.Second {
		t.Fatal("shutdown waited for an unresponsive Dial")
	}
	close(gate)
	<-done
	waitPhoneHang(t, c, 1)
	s.webPhone.mu.Lock()
	defer s.webPhone.mu.Unlock()
	if len(s.webPhone.leases) != 0 {
		t.Fatal("lease leaked")
	}
}
func TestWebPhoneMediaWebSocketPCMAndLeaseFence(t *testing.T) {
	s, c := newPhoneTest(t)
	w := phoneRequest(t, s, "POST", "/dial", phoneClientA, dialRequest)
	token := readPhoneToken(t, w)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.routePhoneAPI(w, r) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/phone/media?device_id=module-a&call_id=synthetic-outgoing"
	for _, protocols := range [][]string{{"vocat-pcm"}, {"vocat-pcm", "lease-wrong"}} {
		conn, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: protocols})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || res.StatusCode != 409 {
			t.Fatal("lease bypass")
		}
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{"vocat-pcm", "lease-" + token}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != "vocat-pcm" {
		t.Fatal("token echoed as protocol")
	}
	if second, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{"vocat-pcm", "lease-" + token}}); err == nil || res.StatusCode != 409 {
		if second != nil {
			second.CloseNow()
		}
		t.Fatal("duplicate audio consumer")
	}
	c.media.down <- []int16{-32768, 123, 32767}
	kind, down, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || len(down) != 6 || int16(binary.LittleEndian.Uint16(down)) != -32768 {
		t.Fatal("downlink", down, err)
	}
	up := make([]byte, 320)
	binary.LittleEndian.PutUint16(up, 1234)
	if err = conn.Write(ctx, websocket.MessageBinary, up); err != nil {
		t.Fatal(err)
	}
	select {
	case pcm := <-c.media.up:
		if pcm[0] != 1234 || len(pcm) != 160 {
			t.Fatal("uplink")
		}
	case <-ctx.Done():
		t.Fatal("no uplink")
	}
	if err = conn.Write(ctx, websocket.MessageBinary, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = conn.Read(ctx); err == nil {
		t.Fatal("invalid PCM accepted")
	}
	// The peer can observe EOF before the handler's deferred detach has run.
	// Wait for that actual ownership transition, not a guessed sleep duration.
	detachDeadline := time.Now().Add(time.Second)
	for {
		s.webPhone.mu.Lock()
		lease := s.webPhone.leases["module-a"]
		detached := lease == nil || !lease.attached
		s.webPhone.mu.Unlock()
		if detached {
			break
		}
		if time.Now().After(detachDeadline) {
			t.Fatal("media handler did not detach")
		}
		time.Sleep(time.Millisecond)
	}
	s.webPhone.sweep(time.Now().Add(5 * time.Second))
	waitPhoneHang(t, c, 1)
}

func TestWebPhoneRequiresSessionAndCSRF(t *testing.T) {
	app := newTestApplication(t)
	for _, path := range []string{"/api/phone", "/api/phone/media?device_id=missing&call_id=missing"} {
		response, err := app.client.Get(app.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatal("unauthenticated phone", response.StatusCode)
		}
	}
	response, err := app.client.Post(app.server.URL+"/api/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	request, _ := http.NewRequest("POST", app.server.URL+"/api/phone/dial", strings.NewReader(dialRequest))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-VoCat-Phone-Client", phoneClientA)
	response, err = app.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("no CSRF accepted", response.StatusCode)
	}
	request, _ = http.NewRequest("POST", app.server.URL+"/api/phone/dial", strings.NewReader(dialRequest))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-VoCat-Phone-Client", phoneClientA)
	request.Header.Set(csrfHeaderName, login.Data.CSRFToken)
	response, err = app.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatal("strict no-modem guard not reached", response.StatusCode)
	}
}
