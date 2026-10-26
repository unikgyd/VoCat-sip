package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// Exercise the public administrator boundary, not a new package symbol: on the
// previous product this compiles and fails because the settings route is absent.
func TestSMSTokenAPISettingsDefaultAndPermissionBoundary(t *testing.T) {
	app := newTestApplication(t)
	response, err := app.client.Get(app.server.URL + "/api/settings/sms-api")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous settings = %d", response.StatusCode)
	}
	response, err = app.client.Post(app.server.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = app.client.Get(app.server.URL + "/api/settings/sms-api")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("settings status = %d: %s", response.StatusCode, body)
	}
	var body struct {
		Data struct {
			Enabled         bool   `json:"enabled"`
			Running         bool   `json:"running"`
			TokenConfigured bool   `json:"token_configured"`
			BindAddress     string `json:"bind_address"`
			Port            int    `json:"port"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Enabled || body.Data.Running || body.Data.TokenConfigured || body.Data.BindAddress != "127.0.0.1" || body.Data.Port != 8787 {
		t.Fatalf("unsafe defaults: %+v", body)
	}
	request, _ := http.NewRequest(http.MethodPut, app.server.URL+"/api/settings/sms-api", strings.NewReader(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = app.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF = %d", response.StatusCode)
	}
}

func TestSMSTokenAPIAdminApplyAndBearerCannotAdminister(t *testing.T) {
	app := newTestApplication(t)
	response, err := app.client.Post(app.server.URL+"/api/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	token := "synthetic-admin-boundary-0123456789"
	body := fmt.Sprintf(`{"enabled":true,"bind_address":"127.0.0.1","port":%d,"token":%q,"allow_lan_access":false}`, port, token)
	req, _ := http.NewRequest("PUT", app.server.URL+"/api/settings/sms-api", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, login.Data.CSRF)
	response, err = app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"running":true`) || strings.Contains(string(raw), token) || strings.Contains(string(raw), "token_hash") {
		t.Fatalf("apply=%d %s", response.StatusCode, raw)
	}
	client := &http.Client{}
	defer client.CloseIdleConnections()
	req, _ = http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/v1/sms", port), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("API=%d %s", response.StatusCode, raw)
	}
	req, _ = http.NewRequest("GET", app.server.URL+"/api/settings/sms-api", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("SMS token authenticated administrator")
	}
}
