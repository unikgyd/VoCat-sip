package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// These fakes never open a modem or send SIP/SMS. They model two independently
// owned managers, including an explicit, possibly failed cleanup ACK.
type dispatchTestController struct {
	mu                                                                sync.Mutex
	state                                                             vowifi.State
	calls                                                             []vowifi.Call
	callsErr                                                          error
	dialed, hungup, answered, messages, boundMessages, stops, enabled int
	stopErr                                                           error
	stopStarted, stopContinue                                         chan struct{}
}

func (c *dispatchTestController) State(string) (vowifi.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, nil
}
func (c *dispatchTestController) RequestEnabled(_ string, enabled bool) (vowifi.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if enabled {
		c.enabled++
	}
	return c.state, nil
}
func (c *dispatchTestController) RequestReconnect(string) (vowifi.State, error) { return c.State("") }
func (c *dispatchTestController) Calls(string) ([]vowifi.Call, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]vowifi.Call(nil), c.calls...), c.callsErr
}
func (c *dispatchTestController) DialCall(_ context.Context, id, number string) (vowifi.Call, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dialed++
	call := vowifi.Call{ID: id + "-call", State: "dialing"}
	c.calls = append(c.calls, call)
	return call, nil
}
func (c *dispatchTestController) AnswerCall(_ context.Context, _ string, id string) (vowifi.Call, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answered++
	return vowifi.Call{ID: id, State: "active"}, nil
}
func (c *dispatchTestController) HangupCall(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hungup++
	return nil
}
func (c *dispatchTestController) CallMedia(context.Context, string, string) (vowifi.CallMedia, error) {
	return nil, nil
}
func (c *dispatchTestController) SendSMS(context.Context, string, vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages++
	return vowifi.SMSSubmitResult{PartsAttempted: 1}, nil
}
func (c *dispatchTestController) SendSMSForSubscription(context.Context, string, string, string, vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.boundMessages++
	return vowifi.SMSSubmitResult{PartsAttempted: 1}, nil
}
func (c *dispatchTestController) StopDevice(ctx context.Context, _ string) error {
	c.mu.Lock()
	c.stops++
	started, proceed, err := c.stopStarted, c.stopContinue, c.stopErr
	c.mu.Unlock()
	if started != nil {
		close(started)
	}
	if proceed != nil {
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
func (*dispatchTestController) BeginMaintenance(string) error { return nil }
func (*dispatchTestController) EndMaintenance(string)         {}

type dispatchTestHardware struct {
	fakeDeviceController
	at, sms, refresh, reboot, esim, flight atomic.Int32
	atReply                                modem.Response
	atFailure                              error
}

func (h *dispatchTestHardware) ExecuteAT(context.Context, string, string) (modem.Response, error) {
	h.at.Add(1)
	if h.atFailure != nil {
		return modem.Response{}, h.atFailure
	}
	if h.atReply.Final != "" {
		return h.atReply, nil
	}
	return modem.Response{Final: "OK"}, nil
}
func (h *dispatchTestHardware) SendSMS(context.Context, string, string, string) (device.SMSSendResult, error) {
	h.sms.Add(1)
	return device.SMSSendResult{}, nil
}
func (h *dispatchTestHardware) Refresh(context.Context, string) (device.Snapshot, error) {
	h.refresh.Add(1)
	return device.Snapshot{}, nil
}
func (h *dispatchTestHardware) Reboot(context.Context, string) error { h.reboot.Add(1); return nil }
func (h *dispatchTestHardware) ESIMInventory(context.Context, string) ([]device.EsimInventoryEntry, error) {
	h.esim.Add(1)
	return nil, nil
}
func (h *dispatchTestHardware) SetFlight(context.Context, string, bool) (device.FlightResult, error) {
	h.flight.Add(1)
	return device.FlightResult{}, nil
}

func newDispatchTestServer(t *testing.T, configs ...store.Device) (*Server, *dispatchTestController, *dispatchTestController, *dispatchTestHardware) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, config := range configs {
		if err := db.UpsertDevice(context.Background(), config); err != nil {
			t.Fatal(err)
		}
	}
	ready := vowifi.State{Enabled: true, SIMReady: true, IMSReady: true, SMSReady: true, Phase: vowifi.PhaseIMSReady, IMSI: "001010000000001", ICCID: "synthetic-sim"}
	wifi, host := &dispatchTestController{state: ready}, &dispatchTestController{state: ready}
	hardware := &dispatchTestHardware{fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "line", Discovered: true, Snapshot: &device.Snapshot{ICCID: "synthetic-sim", IMSI: ready.IMSI}}}}
	s := &Server{store: db, wifiIMS: wifi, hostIMS: host, devices: hardware, logger: regionTestLogger(), maxRequestBodyBytes: 4096}
	s.vowifi = &imsServiceDispatch{server: s}
	return s, wifi, host, hardware
}
func dispatchConfig(id, mode string) store.Device {
	return store.Device{ID: id, Name: id, IMSMode: mode, IMSEnabled: mode != store.IMSModeModule, SMSEnabled: true}
}
func dispatchModeRequest(s *Server, config store.Device, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/devices/"+config.ID+"/ims", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	s.handleIMSMode(w, r, config, true)
	return w
}

func TestIMSDispatchHostReadyRoutesSIPAndDirectServices(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, wifi, host, hardware := newDispatchTestServer(t, config)
	backend := sipCarrierBackend{s}
	call, err := backend.Dial(context.Background(), "line", "+15550000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Hangup(context.Background(), "line", call.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vowifi.(imsSMSController).SendSMS(context.Background(), "line", vowifi.SMSSubmitRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vowifi.(boundIMSSMSController).SendSMSForSubscription(context.Background(), "line", "imsi", "iccid", vowifi.SMSSubmitRequest{}); err != nil {
		t.Fatal(err)
	}
	if host.dialed != 1 || host.hungup != 1 || host.messages != 1 || host.boundMessages != 1 || wifi.dialed+wifi.messages+wifi.boundMessages != 0 || hardware.at.Load()+hardware.sms.Load() != 0 {
		t.Fatal("service crossed selected owner")
	}
}
func TestIMSDispatchHostNotReadyNeverFallsBack(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, wifi, host, hardware := newDispatchTestServer(t, config)
	host.state.IMSReady = false
	host.state.SMSReady = false
	if _, err := (sipCarrierBackend{s}).Dial(context.Background(), "line", "+15550000001"); !errors.Is(err, sipgateway.ErrCarrierUnavailable) {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleCallAction(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"number":"+15550000001"}`)), config, "line", "dial")
	if w.Code < 400 {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleSMSSend(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"device_id":"line","phone":"+15550000001","message":"synthetic"}`)))
	if w.Code != 503 {
		t.Fatalf("SMS %d %s", w.Code, w.Body.String())
	}
	if host.dialed+host.messages+wifi.dialed+wifi.messages != 0 || hardware.at.Load()+hardware.sms.Load() != 0 {
		t.Fatal("not-ready Host submitted through another transport")
	}
	if !s.vowifi.(VoWiFiSMSSyncController).ModemSMSSyncBlocked("line") {
		t.Fatal("Host setup did not block AT SMS synchronization")
	}
}
func TestIMSDispatchConcurrentLinesAndLegacyCompatibility(t *testing.T) {
	s, wifi, host, _ := newDispatchTestServer(t, dispatchConfig("wifi", store.IMSModeVoWiFi), dispatchConfig("host", store.IMSModeHost), store.Device{ID: "legacy", Name: "legacy"}, dispatchConfig("module", store.IMSModeModule))
	var wg sync.WaitGroup
	for _, id := range []string{"wifi", "host", "legacy"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := s.vowifi.(VoWiFiCallController).DialCall(context.Background(), id, "+15550000001"); err != nil {
				t.Errorf("%s: %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	if wifi.dialed != 2 || host.dialed != 1 {
		t.Fatal(wifi.dialed, host.dialed)
	}
	if _, err := s.vowifi.(VoWiFiCallController).DialCall(context.Background(), "module", "+15550000001"); err == nil {
		t.Fatal("explicit module borrowed stale IMS")
	}
	if s.callTransport("module") != "cellular" || s.callTransport("host") != store.IMSModeHost || s.callTransport("legacy") != store.IMSModeVoWiFi {
		t.Fatal("wrong transport")
	}
}
func TestIMSDispatchDisabledHostCannotSubmitEvenStaleReady(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	config.IMSEnabled = false
	s, _, host, hardware := newDispatchTestServer(t, config)
	if _, err := s.vowifi.(VoWiFiCallController).DialCall(context.Background(), "line", "+15550000001"); err == nil {
		t.Fatal("disabled line dialed")
	}
	if _, err := s.vowifi.(imsSMSController).SendSMS(context.Background(), "line", vowifi.SMSSubmitRequest{}); !errors.Is(err, vowifi.ErrSMSNotReady) {
		t.Fatal(err)
	}
	if host.dialed+host.messages != 0 || hardware.at.Load()+hardware.sms.Load() != 0 {
		t.Fatal("disabled line submitted")
	}
}
func TestIMSDispatchPinsLateHangupToCreatingController(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, wifi, host, _ := newDispatchTestServer(t, config)
	call, err := s.vowifi.(VoWiFiCallController).DialCall(context.Background(), "line", "+15550000001")
	if err != nil {
		t.Fatal(err)
	}
	config.IMSMode = store.IMSModeVoWiFi
	if err := s.store.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if err := s.vowifi.(VoWiFiCallController).HangupCall(context.Background(), "line", call.ID); err != nil {
		t.Fatal(err)
	}
	if host.hungup != 1 || wifi.hungup != 0 {
		t.Fatal("late hangup crossed controller")
	}
}
func TestIMSModeSwitchRequiresExactCleanupACK(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeVoWiFi)
	s, wifi, host, hardware := newDispatchTestServer(t, config)
	wifi.stopErr = errors.New("unknown ACK")
	if w := dispatchModeRequest(s, config, `{"mode":"host_ims","enabled":true}`); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, _ := s.store.Device(context.Background(), "line")
	if saved.IMSMode != store.IMSModeVoWiFi || host.enabled != 0 {
		t.Fatal("owner lost before cleanup ACK")
	}
	wifi.stopErr = nil
	wifi.stopStarted = make(chan struct{})
	wifi.stopContinue = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- dispatchModeRequest(s, config, `{"mode":"host_ims","enabled":true}`) }()
	select {
	case <-wifi.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup not reached")
	}
	saved, _ = s.store.Device(context.Background(), "line")
	if saved.IMSMode != store.IMSModeVoWiFi || host.enabled != 0 {
		t.Fatal("switched before ACK")
	}
	if w := dispatchModeRequest(s, config, `{"mode":"module","enabled":false}`); w.Code != 409 {
		t.Fatal("parallel transition accepted", w.Code)
	}
	close(wifi.stopContinue)
	w := <-done
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, _ = s.store.Device(context.Background(), "line")
	if saved.IMSMode != store.IMSModeHost || !saved.IMSEnabled || saved.VoWiFiEnabled || host.enabled != 1 || hardware.flight.Load() != 0 {
		t.Fatal("bad mode transition", saved)
	}
}
func TestIMSModeSwitchRejectsActiveCallAndMissingOwner(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, _, host, _ := newDispatchTestServer(t, config)
	host.calls = []vowifi.Call{{ID: "active", State: "active"}}
	if w := dispatchModeRequest(s, config, `{"mode":"module","enabled":false}`); w.Code != 409 || host.stops != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.hostIMS = nil
	if w := dispatchModeRequest(s, config, `{"mode":"module","enabled":false}`); w.Code != 409 {
		t.Fatal("missing owner assumed cleaned", w.Code)
	}
}
func TestIMSHostStatusAndLegacyBypassIsolation(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, _, _, hardware := newDispatchTestServer(t, config)
	summary := s.configuredDeviceSummary(config, &hardware.entry)
	if summary["ims_mode"] != store.IMSModeHost || summary["ims_runtime"].(map[string]any)["ims_ready"] != true || summary["vowifi_runtime"].(map[string]any)["ims_ready"] == true {
		t.Fatal("Host mislabeled as WiFi", summary)
	}
	for _, path := range []string{"actions/refresh", "actions/at", "actions/reboot", "network/apns", "cellular-ims", "cells", "operator_selection/scan", "esim"} {
		method := http.MethodGet
		if strings.HasPrefix(path, "actions/") {
			method = http.MethodPost
		}
		w := httptest.NewRecorder()
		s.handleDevicePath(w, httptest.NewRequest(method, "/", strings.NewReader(`{}`)), "line", strings.Split(path, "/"))
		if w.Code != 409 {
			t.Fatalf("%s %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.handleVoWiFiEnabled(w, httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"enabled":true}`)), config, true)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := (&telegramBot{server: s}).executeATCommand(context.Background(), "line", "AT+CGSN"); err == nil {
		t.Fatal("bot bypassed Host")
	}
	if hardware.at.Load()+hardware.refresh.Load()+hardware.reboot.Load()+hardware.esim.Load()+hardware.flight.Load() != 0 {
		t.Fatal("Host side channel reached legacy hardware")
	}
}

func TestIMSHostOnlyControllerAndGeneralConfigCannotEraseMode(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, _, host, hardware := newDispatchTestServer(t, config)
	s.wifiIMS = nil
	if _, err := s.vowifi.(VoWiFiCallController).DialCall(context.Background(), "line", "+15550000001"); err != nil || host.dialed != 1 {
		t.Fatal("Host-only server did not route", err)
	}
	config, err := s.store.Device(context.Background(), "line")
	if err != nil {
		t.Fatal(err)
	}
	payload := storedDeviceConfig(config)
	payload["name"], payload["ims_mode"], payload["ims_enabled"] = "Renamed", store.IMSModeModule, false
	body, err := json.Marshal(map[string]any{"config": payload})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleDevicePath(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(string(body))), "line", nil)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, _ := s.store.Device(context.Background(), "line")
	if saved.Name != "Renamed" || saved.IMSMode != store.IMSModeHost || !saved.IMSEnabled {
		t.Fatal("general save erased mode", saved)
	}
	payload["control_device"] = "/dev/fake-other"
	body, _ = json.Marshal(map[string]any{"config": payload})
	w = httptest.NewRecorder()
	s.handleDevicePath(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(string(body))), "line", nil)
	if w.Code != http.StatusConflict {
		t.Fatal("live Host remapped", w.Code, w.Body.String())
	}
	if hardware.at.Load()+hardware.refresh.Load()+hardware.flight.Load() != 0 {
		t.Fatal("config save touched hardware")
	}
}

func TestIMSModeSwitchDoesNotCrossAnInFlightSubmission(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, _, host, _ := newDispatchTestServer(t, config)
	lock := s.imsModeLock("line")
	lock.RLock() // An admitted call/SMS operation owns this same per-line gate.
	w := dispatchModeRequest(s, config, `{"mode":"module","enabled":false}`)
	lock.RUnlock()
	if w.Code != http.StatusConflict || host.stops != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestIMSModeModuleCallMustBeProvenIdle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   modem.Response
		failure error
		want    int
	}{
		{"active", modem.Response{Final: "OK", Lines: []string{`+CLCC: 1,0,0,0,0,"+15550000001",145`}}, nil, 409},
		{"unknown", modem.Response{}, errors.New("control unavailable"), 409},
		{"rejected", modem.Response{Final: "ERROR"}, nil, 409},
		{"idle", modem.Response{Final: "OK"}, nil, 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := dispatchConfig("line", store.IMSModeModule)
			s, _, host, hardware := newDispatchTestServer(t, config)
			hardware.atReply, hardware.atFailure = tc.reply, tc.failure
			w := dispatchModeRequest(s, config, `{"mode":"host_ims","enabled":true}`)
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
			if tc.want != 202 && host.enabled != 0 {
				t.Fatal("Host started while module call status unproved")
			}
		})
	}
}

func TestIMSHostKnownConfigCannotFallBackWhenDatabaseFails(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, _, _, hardware := newDispatchTestServer(t, config)
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	if s.callTransport("line") != "unavailable" || s.callTransportForConfig(config) != store.IMSModeHost {
		t.Fatal("DB failure became module transport")
	}
	for _, action := range []string{"dial", "answer", "hangup"} {
		w := httptest.NewRecorder()
		s.handleCallAction(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"number":"+15550000001","call_id":"old"}`)), config, "line", action)
		if w.Code < 400 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.handleCalls(w, httptest.NewRequest(http.MethodGet, "/", nil), config, "line")
	if w.Code < 400 || hardware.at.Load() != 0 {
		t.Fatal("DB error reached AT", w.Code, hardware.at.Load())
	}
	w = httptest.NewRecorder()
	s.handleESIM(w, httptest.NewRequest(http.MethodGet, "/", nil), nil, "line", true, "line")
	if w.Code < 400 || hardware.esim.Load() != 0 {
		t.Fatal("eSIM DB failure reached UIM")
	}
}

func TestIMSModuleDelayedHangupIsScopedToModePhysicalAndGeneration(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeModule)
	s, _, _, hardware := newDispatchTestServer(t, config)
	scope := s.beginModuleCallScope(config, "line")
	s.hangupModuleScope(context.Background(), "line", scope)
	if hardware.at.Load() != 1 {
		t.Fatal("known scope cleanup not sent")
	}
	_ = s.beginModuleCallScope(config, "line")
	s.hangupModuleScope(context.Background(), "line", scope)
	if hardware.at.Load() != 1 {
		t.Fatal("old call generation hung up a newer call")
	}
	scope = s.beginModuleCallScope(config, "line")
	hardware.entry.Candidate.USBGeneration = "new-usb-epoch"
	s.hangupModuleScope(context.Background(), "line", scope)
	if hardware.at.Load() != 1 {
		t.Fatal("old physical epoch used")
	}
	scope = s.beginModuleCallScope(config, "line")
	config.IMSMode, config.IMSEnabled = store.IMSModeHost, true
	if err := s.store.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	s.hangupModuleScope(context.Background(), "line", scope)
	if hardware.at.Load() != 1 {
		t.Fatal("old ATH sent into Host owner")
	}
}

func TestIMSModeFailedHostCanStopButCleanupFailureRetainsOwner(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	s, _, host, _ := newDispatchTestServer(t, config)
	host.state = vowifi.State{Enabled: true, Phase: vowifi.PhaseFailed}
	host.callsErr = errors.New("not registered")
	host.stopErr = errors.New("unknown cleanup ACK")
	w := dispatchModeRequest(s, config, `{"mode":"module","enabled":false}`)
	if w.Code != 409 || host.stops != 1 {
		t.Fatal("failed runtime did not reach cleanup", w.Code, host.stops)
	}
	saved, _ := s.store.Device(context.Background(), "line")
	if saved.IMSMode != store.IMSModeHost {
		t.Fatal("unknown ACK dropped owner")
	}
	host.stopErr = nil
	w = dispatchModeRequest(s, config, `{"mode":"module","enabled":false}`)
	if w.Code != 202 || host.stops != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, _ = s.store.Device(context.Background(), "line")
	if saved.IMSMode != store.IMSModeModule {
		t.Fatal("failed Host cannot stop")
	}
}

func TestIMSPhysicalAliasConfigurationCannotBorrowHostHardware(t *testing.T) {
	config := dispatchConfig("line", store.IMSModeHost)
	config.USBPath = "usb-a"
	s, _, _, hardware := newDispatchTestServer(t, config)
	hardware.entry.Candidate.USBPath = "usb-a"
	config, _ = s.store.Device(context.Background(), "line")
	alias := storedDeviceConfig(config)
	alias["id"], alias["name"] = "alias", "Alias"
	body, _ := json.Marshal(map[string]any{"config": alias})
	w := httptest.NewRecorder()
	s.handleDevices(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))))
	if w.Code != 409 || hardware.flight.Load() != 0 || hardware.at.Load() != 0 {
		t.Fatal("alias reached hardware", w.Code, w.Body.String())
	}
	if _, err := s.store.Device(context.Background(), "alias"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("alias saved", err)
	}
	other := dispatchConfig("other", store.IMSModeModule)
	other.USBPath = "usb-b"
	if err := s.store.UpsertDevice(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	alias["id"], alias["name"] = "other", "Other"
	body, _ = json.Marshal(map[string]any{"config": alias})
	w = httptest.NewRecorder()
	s.handleDevicePath(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(string(body))), "other", nil)
	if w.Code != 409 {
		t.Fatal("PUT created physical alias", w.Code, w.Body.String())
	}
	saved, _ := s.store.Device(context.Background(), "other")
	if saved.USBPath != "usb-b" {
		t.Fatal("rejected alias replaced config")
	}
	dashboard := s.dashboardDevices()
	if len(dashboard) < 1 || dashboard[0]["ims_mode"] == nil || dashboard[0]["ims_enabled"] == nil || dashboard[0]["ims_runtime"] == nil {
		t.Fatal("dashboard omitted selected mode")
	}
}

func TestIMSPhysicalAliasReservationIsAtomicAcrossAdds(t *testing.T) {
	s, _, _, _ := newDispatchTestServer(t)
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) {
			config := dispatchConfig(id, store.IMSModeModule)
			config.USBPath = "same-physical-usb"
			results <- s.saveDeviceWithPhysicalGuard(context.Background(), config, true)
		}(id)
	}
	var accepted, rejected int
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			accepted++
		} else if errors.Is(err, errPhysicalDeviceAlias) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatal("physical identity was not atomically reserved", accepted, rejected)
	}
}
