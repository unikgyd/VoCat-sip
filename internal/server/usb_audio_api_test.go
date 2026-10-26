package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"
	"vocat/internal/auth"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/modemaudio"
	"vocat/internal/store"
)

type usbAudioAPIDevice struct {
	fakeDeviceController
	err       error
	inspected []string
}

func usbAudioAPIRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "127.0.0.1:12345"
	return r
}

func (d *usbAudioAPIDevice) USBAudioBinding(_ context.Context, id string) (modemaudio.Info, error) {
	d.inspected = append(d.inspected, id)
	return modemaudio.Info{Pairing: "bound", Pair: &modemaudio.PairInfo{Card: 2, CaptureDevice: 0, PlaybackDevice: 1}, BackendAvailable: true, VoiceBridgeReady: true, SampleRate: 8000, Channels: 1}, d.err
}

func newUSBAudioAPI(t *testing.T, controller DeviceController) (*Server, *http.Cookie, string) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.UpsertDevice(context.Background(), store.Device{ID: "configured", Name: "configured", USBPath: "/synthetic/usb/1-2", IMSMode: store.IMSModeHost, IMSEnabled: true}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(db, auth.Options{SessionTTL: time.Hour, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.EnsureAdmin(context.Background(), "synthetic-admin", "synthetic-password"); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Store: db, Auth: a, Devices: controller, Logger: regionTestLogger(), MaxRequestBodyBytes: 4096, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>synthetic</html>")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSIPGateway)
	w := httptest.NewRecorder()
	r := usbAudioAPIRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"synthetic-admin","password":"synthetic-password"}`))
	r.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			return s, c, response.Data.CSRF
		}
	}
	t.Fatal("no session cookie")
	return nil, nil, ""
}

func usbAudioAPIPhysical() fakeDeviceController {
	return fakeDeviceController{entry: device.Device{ID: "physical", Discovered: true, Candidate: modem.Candidate{USBPath: "/synthetic/usb/1-2"}}, atHandler: func(string) (modem.Response, error) { panic("read-only USB audio API issued AT") }}
}

func TestUSBAudioAPIActualRouteRequiresAuthAndMapsPhysicalDevice(t *testing.T) {
	controller := &usbAudioAPIDevice{fakeDeviceController: usbAudioAPIPhysical()}
	s, cookie, csrf := newUSBAudioAPI(t, controller)
	public := usbAudioAPIRequest(http.MethodGet, "/api/devices/configured/usb-audio", nil)
	public.RemoteAddr = "198.51.100.1:12345"
	public.AddCookie(cookie)
	denied := httptest.NewRecorder()
	s.ServeHTTP(denied, public)
	if denied.Code != http.StatusForbidden || len(controller.inspected) != 0 {
		t.Fatal("public network guard changed", denied.Code, controller.inspected)
	}
	for _, authenticated := range []bool{false, true} {
		r := usbAudioAPIRequest(http.MethodGet, "/api/devices/configured/usb-audio", nil)
		if authenticated {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if !authenticated {
			if w.Code != http.StatusUnauthorized || len(controller.inspected) != 0 {
				t.Fatal(w.Code, controller.inspected, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
		data := decodeData(t, w)
		if data["voice_bridge_ready"] != false || data["backend_available"] != true || data["pairing"] != "bound" {
			t.Fatal(data)
		}
		if len(controller.inspected) != 1 || controller.inspected[0] != "physical" {
			t.Fatal(controller.inspected)
		}
		for _, sensitive := range []string{"/synthetic/usb", "/dev/snd", "imsi", "iccid", "at_port"} {
			if strings.Contains(w.Body.String(), sensitive) {
				t.Fatal("private binding leaked", w.Body.String())
			}
		}
	}
	// Metadata is permitted during Host IMS ownership; maintenance operations
	// remain forbidden and neither request may issue an AT command.
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodPost, "/api/devices/configured/usb-audio", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/devices/configured/actions/at", http.StatusConflict},
		{http.MethodGet, "/api/devices/unknown/usb-audio", http.StatusNotFound},
	} {
		r := usbAudioAPIRequest(tc.method, tc.path, strings.NewReader(`{"command":"AT"}`))
		r.AddCookie(cookie)
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
		r.Header.Set(csrfHeaderName, csrf)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
		if len(controller.inspected) != 1 {
			t.Fatal("non-read request inspected audio", controller.inspected)
		}
	}
}

func TestUSBAudioAPIUnavailableOfflineAndStaleAreNotReadiness(t *testing.T) {
	for _, name := range []string{"unsupported", "offline", "changed", "failure"} {
		t.Run(name, func(t *testing.T) {
			base := usbAudioAPIPhysical()
			controller := &usbAudioAPIDevice{fakeDeviceController: base}
			var boundary DeviceController = controller
			want := http.StatusBadGateway
			switch name {
			case "unsupported":
				boundary = &base
				want = http.StatusNotImplemented
			case "offline":
				controller.entry.Discovered = false
				want = http.StatusServiceUnavailable
			case "changed":
				controller.err = modemaudio.ErrChanged
				want = http.StatusConflict
			case "failure":
				controller.err = errors.New("synthetic inspection failure")
			}
			s, cookie, _ := newUSBAudioAPI(t, boundary)
			r := usbAudioAPIRequest(http.MethodGet, "/api/devices/configured/usb-audio", nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != want || strings.Contains(w.Body.String(), `"voice_bridge_ready":true`) {
				t.Fatal(name, w.Code, w.Body.String())
			}
			if name == "offline" && len(controller.inspected) != 0 {
				t.Fatal("offline audio inspected")
			}
		})
	}
}
