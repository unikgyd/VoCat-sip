package smsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/smsquery"
	"vocat/internal/store"
)

const testToken = "synthetic_token_0123456789_ABCDEFGH"

func database(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
func request(t *testing.T, base, method, path, token string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, base+strings.TrimPrefix(path, "/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		if key == "Host" {
			req.Host = value
		} else {
			req.Header.Set(key, value)
		}
	}
	client := &http.Client{Timeout: 6 * time.Second}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body), res.Header
}
func start(t *testing.T, db Repository) (*Service, Update, Status) {
	t.Helper()
	service := New(context.Background(), db)
	t.Cleanup(service.Close)
	u := Update{Enabled: true, BindAddress: "127.0.0.1", Port: freePort(t), Token: testToken}
	status, err := service.Apply(context.Background(), u)
	if err != nil || !status.Running {
		t.Fatalf("start %+v %v", status, err)
	}
	return service, u, status
}

func TestSMSAPILiveHTTPReadOnlyAndIndependentCredential(t *testing.T) {
	db := database(t)
	_, _, status := start(t, db)
	_, err := db.SaveSMSMessage(context.Background(), store.SMSMessage{DeviceID: "saved-line", Peer: "BANK", Direction: "inbound", Body: "登录验证码 123456", IMSI: "secret-imsi", ICCID: "secret-iccid", Extra: []byte(`{"pdu":"secret-pdu"}`), Timestamp: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		Name, Method, Path, Token string
		Headers                   map[string]string
		Status                    int
	}{
		{"no-token", "GET", "/api/v1/sms", "", nil, 401},
		{"wrong-token", "GET", "/api/v1/sms", "wrong", nil, 401},
		{"cookie-not-auth", "GET", "/api/v1/sms", "", map[string]string{"Cookie": "vocat_session=fake"}, 401},
		{"post", "POST", "/api/v1/sms", testToken, nil, 405},
		{"delete", "DELETE", "/api/v1/sms", testToken, nil, 405},
		{"admin", "GET", "/api/settings/sip", testToken, nil, 404},
		{"duplicate", "GET", "/api/v1/sms?limit=1&limit=2", testToken, nil, 400},
		{"query-token", "GET", "/api/v1/sms?token=secret", testToken, nil, 400},
		{"origin", "GET", "/api/v1/sms", testToken, map[string]string{"Origin": ""}, 403},
		{"foreign-host", "GET", "/api/v1/sms", testToken, map[string]string{"Host": "attacker.example"}, 403},
		{"forwarded", "GET", "/api/v1/sms", testToken, map[string]string{"X-Forwarded-For": "203.0.113.4"}, 200},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			code, body, h := request(t, status.BaseURL, tc.Method, tc.Path, tc.Token, tc.Headers)
			if code != tc.Status {
				t.Fatalf("status %d: %s", code, body)
			}
			if h.Get("Cache-Control") != "no-store, private" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("unsafe headers %v", h)
			}
		})
	}
	code, body, _ := request(t, status.BaseURL, "GET", "/api/v1/otp", testToken, nil)
	if code != 200 || !strings.Contains(body, `"code":"123456"`) || !strings.Contains(body, `"slotId":"saved-line"`) || strings.Contains(body, "secret-") {
		t.Fatalf("unsafe response %d %s", code, body)
	}
	code, body, _ = request(t, status.BaseURL, "GET", "/api/v1/otp?sender=OTHER", testToken, nil)
	if code != 200 || !strings.Contains(body, `"found":false`) || !strings.Contains(body, `"message":null`) {
		t.Fatalf("empty response %s", body)
	}
}

func TestSMSAPISettingsPersistenceRotationDisableAndRestart(t *testing.T) {
	ctx := context.Background()
	db := database(t)
	service, u, status := start(t, db)
	saved, err := db.AppSetting(ctx, SettingKey)
	if err != nil || !saved.Sensitive || strings.Contains(string(saved.Value), testToken) {
		t.Fatalf("credential leaked %v", err)
	}
	u.Token = strings.Repeat("B", 32)
	status, err = service.Apply(ctx, u)
	if err != nil || !status.Running {
		t.Fatalf("rotate %+v %v", status, err)
	}
	code, _, _ := request(t, status.BaseURL, "GET", "/api/v1/sms", testToken, nil)
	if code != 401 {
		t.Fatal("old token still active")
	}
	code, _, _ = request(t, status.BaseURL, "GET", "/api/v1/sms", u.Token, nil)
	if code != 200 {
		t.Fatal("new token rejected")
	}
	service.Close()
	service.Close()
	restarted := New(ctx, db)
	defer restarted.Close()
	if !restarted.Status().Running {
		t.Fatalf("restart %+v", restarted.Status())
	}
	u.Enabled = false
	u.Token = ""
	status, err = restarted.Apply(ctx, u)
	if err != nil || status.Running || !status.TokenConfigured {
		t.Fatalf("disable %+v %v", status, err)
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", u.Port))
	if err != nil {
		t.Fatalf("disabled API retained listener: %v", err)
	}
	listener.Close()
	for _, tc := range []Update{{Enabled: true, BindAddress: "0.0.0.0", Port: u.Port, Token: testToken}, {Enabled: true, BindAddress: "8.8.8.8", Port: u.Port, AllowLAN: true, Token: testToken}, {Enabled: true, BindAddress: "10.0.0.2", Port: u.Port, Token: testToken}, {Enabled: true, BindAddress: "127.0.0.1", Port: u.Port, Token: "short"}, {Enabled: true, BindAddress: "127.0.0.1", Port: 80, Token: testToken}} {
		if _, err = restarted.Apply(ctx, tc); err == nil {
			t.Fatalf("unsafe update %+v", tc)
		}
	}
	if restarted.Status() != status {
		t.Fatal("invalid update mutated running settings")
	}
}

func TestSMSAPISynchronousPortReleaseDuringRapidStartup(t *testing.T) {
	ctx := context.Background()
	service := New(ctx, database(t))
	defer service.Close()
	u := Update{Enabled: true, BindAddress: "127.0.0.1", Port: freePort(t), Token: testToken}
	for i := 0; i < 64; i++ {
		u.Enabled = true
		if status, err := service.Apply(ctx, u); err != nil || !status.Running {
			t.Fatalf("start %d: %+v %v", i, status, err)
		}
		u.Enabled = false
		if status, err := service.Apply(ctx, u); err != nil || status.Running {
			t.Fatalf("stop %d: %+v %v", i, status, err)
		}
		listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", u.Port))
		if err != nil {
			t.Fatalf("port not synchronously released at %d: %v", i, err)
		}
		listener.Close()
	}
}

func TestSMSAPIFailClosedCorruptSettingsBindFailureAndRateLimit(t *testing.T) {
	ctx := context.Background()
	db := database(t)
	if err := db.UpsertAppSetting(ctx, store.AppSetting{Key: SettingKey, Value: []byte(`{"version":99}`), Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	service := New(ctx, db)
	defer service.Close()
	if s := service.Status(); s.Running || s.Enabled || s.Error == "" {
		t.Fatalf("corrupt load %+v", s)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	u := Update{Enabled: true, BindAddress: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Token: testToken}
	status, err := service.Apply(ctx, u)
	if err != nil || !status.Enabled || status.Running || status.Error == "" {
		t.Fatalf("bind failure status %+v %v", status, err)
	}
	listener.Close()
	status, err = service.Apply(ctx, u)
	if err != nil || !status.Running {
		t.Fatalf("retry %+v %v", status, err)
	}
	for i := 0; i < 121; i++ {
		code, _, h := request(t, status.BaseURL, "GET", "/api/v1/sms", "wrong", nil)
		expected := 401
		if i == 120 {
			expected = 429
			if h.Get("Retry-After") == "" {
				t.Fatal("no retry")
			}
		}
		if code != expected {
			t.Fatalf("request %d=%d", i, code)
		}
	}
}

type pausedRepository struct {
	*store.Store
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	failSave bool
}

func (p *pausedRepository) QueryReadOnlySMS(ctx context.Context, q smsquery.Query) (smsquery.Result, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return p.Store.QueryReadOnlySMS(ctx, q)
	case <-ctx.Done():
		return smsquery.Result{}, ctx.Err()
	}
}
func (p *pausedRepository) UpsertAppSetting(ctx context.Context, s store.AppSetting) error {
	if p.failSave {
		return errors.New("synthetic private failure")
	}
	return p.Store.UpsertAppSetting(ctx, s)
}
func TestSMSAPICapacityCancellationAndFailedSavePreservesRuntime(t *testing.T) {
	ctx := context.Background()
	db := &pausedRepository{Store: database(t), entered: make(chan struct{}), release: make(chan struct{})}
	service, u, status := start(t, db)
	db.failSave = true
	u.Token = strings.Repeat("C", 32)
	unchanged, err := service.Apply(ctx, u)
	if err == nil || unchanged != status || strings.Contains(err.Error(), "private failure") {
		t.Fatal("failed save changed runtime or leaked detail")
	}
	db.failSave = false
	var clients sync.WaitGroup
	for i := 0; i < 8; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			client := &http.Client{Timeout: 6 * time.Second}
			req, _ := http.NewRequest("GET", status.BaseURL+"api/v1/sms", nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			r, err := client.Do(req)
			if err == nil {
				r.Body.Close()
			}
			client.CloseIdleConnections()
		}()
	}
	select {
	case <-db.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("query not entered")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(service.snapshot.capacity) < 8 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(service.snapshot.capacity) != 8 {
		t.Fatal("capacity did not fill")
	}
	code, _, _ := request(t, status.BaseURL, "GET", "/api/v1/sms", testToken, nil)
	if code != 503 {
		t.Fatal("capacity not bounded")
	}
	start := time.Now()
	service.Close()
	clients.Wait()
	if time.Since(start) > time.Second {
		t.Fatal("shutdown waited for blocked DB query")
	}
	if _, err := service.Apply(ctx, u); err == nil {
		t.Fatal("closed service restarted")
	}
}

func TestSMSAPIPrivateAddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		Address string
		Allowed bool
	}{{"127.0.0.1", true}, {"10.0.0.2", true}, {"100.64.0.2", true}, {"172.16.0.2", true}, {"192.168.0.2", true}, {"::1", true}, {"fd00::1", true}, {"0.0.0.0", false}, {"::", false}, {"8.8.8.8", false}, {"fe80::1", false}, {"169.254.0.1", false}, {"2001:db8::1", false}} {
		var s Settings = defaults()
		s.BindAddress = tc.Address
		s.AllowLAN = true
		_, err := s.endpoint()
		if (err == nil) != tc.Allowed {
			t.Errorf("%s=%v", tc.Address, err)
		}
	}
	// The status representation never includes a digest, even if manually serialized.
	s := New(context.Background(), database(t))
	defer s.Close()
	raw, _ := json.Marshal(s.Status())
	if strings.Contains(string(raw), "token_hash") {
		t.Fatal("digest leaked")
	}
}
