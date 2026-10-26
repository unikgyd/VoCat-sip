package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type phoneDTMFTestController struct {
	*phoneTestController
	keys []string
	err  error
}

func (c *phoneDTMFTestController) SendDTMF(ctx context.Context, device, call string, digit byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, v := range c.calls {
		if v.ID == call && v.State == "active" && v.EndedAt == nil {
			c.keys = append(c.keys, device+"/"+call+"/"+string(digit))
			return c.err
		}
	}
	return errors.New("not active")
}

func phoneDTMFOwner(t *testing.T, s *Server, device, call, client string) *phoneLease {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/phone/dtmf", nil)
	r.Header.Set("X-VoCat-Phone-Client", client)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "synthetic-session"})
	l, err := s.webPhone.reserve(device, phoneClient(r))
	if err != nil {
		t.Fatal(err)
	}
	s.webPhone.mu.Lock()
	l.call = call
	l.attached = true
	l.controlContext, l.controlCancel = context.WithCancel(context.Background())
	s.webPhone.mu.Unlock()
	return l
}

func TestWebPhoneDTMFOwnershipExactCallAndUncertainFailure(t *testing.T) {
	s, base := newPhoneTest(t)
	c := &phoneDTMFTestController{phoneTestController: base}
	s.vowifi = c
	c.calls = []vowifi.Call{{ID: "call-a", State: "active", StartedAt: time.Now()}}
	client := "browser_aaaaaaaaaaaaaaaaaaaa"
	l := phoneDTMFOwner(t, s, "module-a", "call-a", client)
	body := `{"device_id":"module-a","call_id":"call-a","digit":"#"}`
	for _, tc := range []struct {
		name, client, body string
		status             int
	}{
		{"owner", client, body, 200},
		{"other browser", "browser_bbbbbbbbbbbbbbbbbbbb", body, 409},
		{"no client", "", body, 400},
		{"wrong call", client, strings.Replace(body, "call-a", "next-call", 1), 409},
		{"wrong module", client, strings.Replace(body, "module-a", "module-b", 1), 409},
		{"command injection", client, strings.Replace(body, `"#"`, `"1;ATH"`, 1), 400},
		{"multiple digits", client, strings.Replace(body, `"#"`, `"12"`, 1), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := phoneRequest(t, s, "POST", "/dtmf", tc.client, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
	if len(c.keys) != 1 || c.keys[0] != "module-a/call-a/#" {
		t.Fatalf("unexpected calls: %v", c.keys)
	}
	c.err = errors.New("private carrier or subscriber detail")
	w := phoneRequest(t, s, "POST", "/dtmf", client, body)
	if w.Code != 409 || strings.Contains(w.Body.String(), "private carrier") || len(c.keys) != 2 {
		t.Fatalf("uncertain failure %d %s keys=%v", w.Code, w.Body, c.keys)
	}
	s.webPhone.mu.Lock()
	l.attached = false
	s.webPhone.mu.Unlock()
	w = phoneRequest(t, s, "POST", "/dtmf", client, body)
	if w.Code != 409 || len(c.keys) != 2 {
		t.Fatalf("disconnected browser sent event: %d", w.Code)
	}
}

func TestWebPhoneDTMFRemainsPinnedToOriginalRuntime(t *testing.T) {
	s, base := newPhoneTest(t)
	old := &phoneDTMFTestController{phoneTestController: base}
	other := &phoneDTMFTestController{phoneTestController: &phoneTestController{fakeVoWiFiController: base.fakeVoWiFiController}}
	base.calls = []vowifi.Call{{ID: "old-dialog", State: "active", StartedAt: time.Now()}}
	s.pinIMSCall("module-a", base.calls[0], old)
	s.wifiIMS = other
	s.vowifi = &imsServiceDispatch{server: s}
	client := "browser_aaaaaaaaaaaaaaaaaaaa"
	phoneDTMFOwner(t, s, "module-a", "old-dialog", client)
	w := phoneRequest(t, s, "POST", "/dtmf", client, `{"device_id":"module-a","call_id":"old-dialog","digit":"5"}`)
	if w.Code != 200 || len(old.keys) != 1 || len(other.keys) != 0 {
		t.Fatalf("wrong runtime: %d %s old=%v new=%v", w.Code, w.Body, old.keys, other.keys)
	}
}

func TestWebPhoneDTMFCapabilityFromRealCallSnapshot(t *testing.T) {
	s, c := newPhoneTest(t)
	c.calls = []vowifi.Call{{ID: "capability", State: "active", StartedAt: time.Now(), DTMFDigits: "0123456789*#"}}
	w := phoneRequest(t, s, "GET", "", "browser_aaaaaaaaaaaaaaaaaaaa", "")
	var body struct {
		Data struct {
			Lines []phoneLineView `json:"lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(body.Data.Lines) != 1 || body.Data.Lines[0].Calls[0].DTMFDigits != "0123456789*#" {
		t.Fatalf("capability response %d %s", w.Code, w.Body)
	}
}
