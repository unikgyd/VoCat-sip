package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"vocat/internal/sipgateway"
)

func TestSIPSettingsPublicViewAndSensitivePersistence(t *testing.T) {
	test := newDurableSettingsAPITest(t)
	test.server.sipGateway = sipgateway.New(context.Background(), test.database)
	t.Cleanup(test.server.CloseSIPGateway)
	config := sipgateway.DefaultConfig()
	config.MessagesEnabled = true
	config.CallsEnabled = true
	config.IdentityDomain = "voice.example.test"
	config.Accounts = []sipgateway.Account{{Username: "1001", Password: "ui-test-secret-2026", AllowMessages: true, AllowCalls: true}}
	body, _ := json.Marshal(config)
	recorder := test.request(t, http.MethodPut, "/api/settings/sip", string(body))
	if recorder.Code != 200 {
		t.Fatalf("PUT %d %s", recorder.Code, recorder.Body)
	}
	for _, recorder := range []*bytes.Buffer{recorder.Body, test.request(t, http.MethodGet, "/api/settings/sip", "").Body} {
		if strings.Contains(recorder.String(), "ui-test-secret") || strings.Contains(recorder.String(), "password_sha256") {
			t.Fatal("SIP API exposed secrets")
		}
	}
	setting, err := test.database.AppSetting(context.Background(), sipgateway.SettingKey)
	if err != nil || !setting.Sensitive || !strings.Contains(string(setting.Value), "password_sha256") {
		t.Fatal("secret config not marked sensitive")
	}
	if string(setting.Public().Value) != "null" {
		t.Fatal("generic settings export leaks SIP config")
	}
	var saved struct {
		Data sipgateway.View `json:"data"`
	}
	if err := json.Unmarshal(test.request(t, http.MethodGet, "/api/settings/sip", "").Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Data.Config.MessagesEnabled || !saved.Data.Config.Accounts[0].AllowMessages || saved.Data.Config.IdentityDomain != "voice.example.test" || saved.Data.Status.Capabilities.Messages {
		t.Fatal("message settings not persisted or disabled service advertised as active")
	}
	if !saved.Data.Config.CallsEnabled || !saved.Data.Config.Accounts[0].AllowCalls || saved.Data.Status.Capabilities.Calls {
		t.Fatal("call permissions not persisted or disabled listener advertised as active")
	}
}
func TestSIPSettingsRequiresSessionAndCSRF(t *testing.T) {
	app := newTestApplication(t)
	response, err := app.client.Get(app.server.URL + "/api/settings/sip")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("SIP config accessible without login")
	}
	response, err = app.client.Post(app.server.URL+"/api/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	json.NewDecoder(response.Body).Decode(&login)
	response.Body.Close()
	if login.Data.CSRFToken == "" {
		t.Fatal("login failed")
	}
	config := sipgateway.DefaultConfig()
	config.Peer = sipSettingsPeerFixture()
	body, _ := json.Marshal(config)
	request, _ := http.NewRequest(http.MethodPut, app.server.URL+"/api/settings/sip", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err = app.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("SIP mutation missing CSRF accepted")
	}
	request, _ = http.NewRequest(http.MethodPut, app.server.URL+"/api/settings/sip", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(csrfHeaderName, login.Data.CSRFToken)
	response, err = app.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("authenticated SIP save failed")
	}
	response, err = app.client.Get(app.server.URL + "/api/settings/sip")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(data, []byte(`"enabled":false`)) {
		t.Fatal("SIP optional defaults missing")
	}
	if bytes.Contains(data, []byte("peer-secret")) || bytes.Contains(data, []byte("password_sha256")) || bytes.Contains(data, []byte("password_md5")) {
		t.Fatal("authenticated SIP API exposed peer passwords or HA1")
	}
}

func sipSettingsPeerFixture() *sipgateway.PeerConfig {
	return &sipgateway.PeerConfig{
		Enabled: true, Endpoint: "127.0.0.1:9",
		InboundAuth:  sipgateway.PeerInboundAuth{Username: "pbx-in", Password: "inbound-peer-secret-2026"},
		OutboundAuth: sipgateway.PeerOutboundAuth{Username: "gateway-out", Realm: "pbx-realm", Password: "outbound-peer-secret-2026", Algorithms: []string{"SHA-256", "MD5"}},
	}
}

func TestSIPSettingsPeerOptionalDefaultAndSecretRoundTrip(t *testing.T) {
	test := newDurableSettingsAPITest(t)
	test.server.sipGateway = sipgateway.New(context.Background(), test.database)
	t.Cleanup(test.server.CloseSIPGateway)
	read := func() sipgateway.View {
		t.Helper()
		response := test.request(t, http.MethodGet, "/api/settings/sip", "")
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET: %d %s", response.Code, response.Body)
		}
		var result struct {
			Data sipgateway.View `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"inbound-peer-secret", "outbound-peer-secret", "password_sha256", "password_md5", "credential_epoch"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("SIP API exposed peer secret: %s", secret)
			}
		}
		return result.Data
	}
	initial := read()
	if initial.Config.Peer != nil || initial.Status.Running || len(initial.Config.Accounts) != 0 {
		t.Fatal("peer default was not absent and inert")
	}
	config := sipgateway.DefaultConfig()
	config.Listen = "127.0.0.1:15060"
	config.Accounts = []sipgateway.Account{{Username: "1001", Password: "local-extension-secret"}}
	config.CarrierLines = []sipgateway.CarrierLine{{DeviceID: "modem-a", Label: "A", AllowOutgoing: true, AllowIncoming: true, AllowSMSSend: true, AllowSMSReceive: true}, {DeviceID: "modem-b", Label: "B"}}
	config.Peer = sipSettingsPeerFixture()
	config.Peer.AllowCallsFromPeer, config.Peer.AllowSMSFromPeer, config.Peer.AllowCallsToPeer, config.Peer.AllowSMSToPeer = true, true, true, true
	config.Peer.OutgoingLines, config.Peer.SMSSendLines = []string{"modem-a", "modem-b"}, []string{"modem-a"}
	config.Peer.IncomingRoutes = []sipgateway.PeerIncomingRoute{{DeviceID: "modem-a", CallTarget: "600", SMSTarget: "messages"}, {DeviceID: "modem-b", CallTarget: "601"}}
	put := func(config sipgateway.Config, expected int) {
		t.Helper()
		body, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		response := test.request(t, http.MethodPut, "/api/settings/sip", string(body))
		if response.Code != expected {
			t.Fatalf("PUT: %d want %d: %s", response.Code, expected, response.Body)
		}
		for _, secret := range []string{"inbound-peer-secret", "outbound-peer-secret", "local-extension-secret", "password_sha256", "password_md5", "credential_epoch"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("PUT exposed secret: %s", secret)
			}
		}
	}
	put(config, http.StatusOK)
	saved := read()
	peer := saved.Config.Peer
	if peer == nil || !peer.Enabled || !peer.InboundAuth.HasPassword || !peer.OutboundAuth.HasPassword || peer.InboundAuth.Password != "" || peer.OutboundAuth.Password != "" {
		t.Fatal("peer credentials were not write-only")
	}
	if !reflect.DeepEqual(peer.OutgoingLines, config.Peer.OutgoingLines) || !reflect.DeepEqual(peer.SMSSendLines, config.Peer.SMSSendLines) || !reflect.DeepEqual(peer.IncomingRoutes, config.Peer.IncomingRoutes) || !reflect.DeepEqual(peer.OutboundAuth.Algorithms, config.Peer.OutboundAuth.Algorithms) {
		t.Fatal("nested peer configuration was lost")
	}
	if !peer.AllowCallsFromPeer || !peer.AllowSMSFromPeer || !peer.AllowCallsToPeer || !peer.AllowSMSToPeer || saved.Status.Running {
		t.Fatal("peer permissions or disabled service state changed")
	}
	setting, err := test.database.AppSetting(context.Background(), sipgateway.SettingKey)
	if err != nil || !setting.Sensitive || !strings.Contains(string(setting.Value), "password_sha256") || string(setting.Public().Value) != "null" {
		t.Fatal("peer private setting was not protected")
	}
	if strings.Contains(string(setting.Value), "inbound-peer-secret") || strings.Contains(string(setting.Value), "outbound-peer-secret") {
		t.Fatal("plaintext peer password persisted")
	}
	// An ordinary full-form save retains both secrets and all nested peer fields.
	saved.Config.IdentityDomain = "voice.example.test"
	put(saved.Config, http.StatusOK)
	after := read()
	if !reflect.DeepEqual(after.Config.Peer, peer) {
		t.Fatal("blank-password edit changed peer configuration")
	}
	// Renaming a peer principal without a replacement password cannot inherit it.
	after.Config.Peer.InboundAuth.Username = "replacement"
	put(after.Config, http.StatusBadRequest)
	if read().Config.Peer.InboundAuth.Username != "pbx-in" {
		t.Fatal("invalid peer edit replaced saved credentials")
	}
	// A new manager loads the same sanitized, complete peer without sending SIP.
	test.server.CloseSIPGateway()
	test.server.sipGateway = sipgateway.New(context.Background(), test.database)
	if !reflect.DeepEqual(read().Config.Peer, peer) {
		t.Fatal("persisted peer was not restored")
	}
	after = read()
	after.Config.Peer = nil
	put(after.Config, http.StatusOK)
	if read().Config.Peer != nil {
		t.Fatal("explicit peer removal did not persist")
	}
}

func TestSIPSettingsPeerOnlyListenerDoesNotRequireLocalAccountOrProbePeer(t *testing.T) {
	test := newDurableSettingsAPITest(t)
	test.server.sipGateway = sipgateway.New(context.Background(), test.database)
	t.Cleanup(test.server.CloseSIPGateway)
	// Only synthetic loopback sockets are involved. The remote socket must receive
	// nothing merely because a peer is saved/enabled.
	remote, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := listener.LocalAddr().String()
	listener.Close()
	config := sipgateway.DefaultConfig()
	config.Enabled, config.Listen = true, listenAddress
	config.Peer = &sipgateway.PeerConfig{Enabled: true, Endpoint: remote.LocalAddr().String()}
	body, _ := json.Marshal(config)
	response := test.request(t, http.MethodPut, "/api/settings/sip", string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("peer-only PUT: %d %s", response.Code, response.Body)
	}
	view := test.server.sipGateway.Snapshot()
	if !view.Status.Running || len(view.Config.Accounts) != 0 || len(view.Status.Bindings) != 0 || view.Config.Peer == nil {
		t.Fatal("fixed peer invented a local account or registration")
	}
	if view.Config.Peer.AllowCallsFromPeer || view.Config.Peer.AllowCallsToPeer || view.Config.Peer.AllowSMSFromPeer || view.Config.Peer.AllowSMSToPeer {
		t.Fatal("peer-only setup granted business permissions")
	}
	if err := remote.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := remote.ReadFrom(make([]byte, 1024)); err == nil {
		t.Fatal("saving peer unexpectedly sent a SIP probe")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("read peer: %v", err)
	}
}
