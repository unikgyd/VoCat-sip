package smsapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSMSAPIQueryDeadlineAndIPv6SocketBoundary(t *testing.T) {
	db := &pausedRepository{Store: database(t), entered: make(chan struct{}), release: make(chan struct{})}
	service, _, status := start(t, db)
	started := time.Now()
	code, body, _ := request(t, status.BaseURL, "GET", "/api/v1/sms", testToken, nil)
	if code != 503 || !strings.Contains(body, "temporarily_unavailable") || time.Since(started) < 4500*time.Millisecond || time.Since(started) > 6*time.Second {
		t.Fatalf("deadline status=%d elapsed=%v body=%s", code, time.Since(started), body)
	}
	service.Close()
	// A socket-derived IPv6 local endpoint is accepted, but a public remote
	// stays forbidden even if X-Forwarded-For pretends to be private.
	endpoint := netip.MustParseAddrPort("[::1]:8787")
	p := &snapshot{database: db.Store, endpoint: endpoint, capacity: make(chan struct{}, 8)}
	p.active.Store(true)
	for _, remote := range []string{"[fd00::2]:15000", "[2001:db8::2]:15000"} {
		r := httptest.NewRequest("GET", "http://[::1]:8787/api/v1/sms", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8787}))
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		expected := 401
		if strings.HasPrefix(remote, "[2001") {
			expected = 403
		}
		if w.Code != expected {
			t.Fatalf("IPv6 %s = %d", remote, w.Code)
		}
	}
}
