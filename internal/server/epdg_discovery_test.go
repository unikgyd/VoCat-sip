package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func TestEPDGConfigurationAPIRequiresVerifiedSIMAndBindsWritesToIt(t *testing.T) {
	test := newSettingsAPITest(t)
	config := store.Device{ID: "ec20"}
	controller := &fakeVoWiFiController{}
	test.server.vowifi = controller
	request := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/devices/ec20/vowifi/discovery", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		test.server.handleEPDGDiscovery(w, r, config)
		return w
	}
	if got := request(http.MethodGet, ""); got.Code != 409 {
		t.Fatalf("missing SIM %d", got.Code)
	}
	controller.state = vowifi.State{SIMReady: true, ICCID: "8944100000000000001"}
	get := request(http.MethodGet, "")
	if get.Code != 200 {
		t.Fatal(get.Body.String())
	}
	var initial struct {
		Data map[string]any `json:"data"`
	}
	json.Unmarshal(get.Body.Bytes(), &initial)
	token := initial.Data["sim_token"].(string)
	body := `{"sim_token":"` + token + `","preferred_addresses":["MANUAL.example."],"additional_plmns":["515-66"],"manual_only":true}`
	if got := request(http.MethodPut, body); got.Code != 200 || !strings.Contains(got.Body.String(), "manual.example") {
		t.Fatalf("save %d %s", got.Code, got.Body)
	}
	if len(controller.enabled) != 0 || controller.reconnects != 0 {
		t.Fatal("saving changed the live runtime")
	}
	if got := request(http.MethodPut, `{"sim_token":"`+token+`","manual_only":true}`); got.Code != 400 {
		t.Fatal("invalid manual-only accepted")
	}
	controller.state.ICCID = "8944100000000000002"
	if got := request(http.MethodPut, body); got.Code != 409 {
		t.Fatal("stale SIM token saved to another SIM")
	}
	get = request(http.MethodGet, "")
	if strings.Contains(get.Body.String(), "manual.example") {
		t.Fatal("configuration crossed cards")
	}
	if got := request(http.MethodDelete, ""); got.Code != 405 || got.Header().Get("Allow") != "GET, PUT" {
		t.Fatal("wrong method handling")
	}
}
func TestEPDGConfigurationRouteRequiresSessionAndCSRF(t *testing.T) {
	app := newTestApplication(t)
	response, err := app.client.Get(app.server.URL + "/api/devices/ec20/vowifi/discovery")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unauthenticated discovery status %d", response.StatusCode)
	}
	response, err = app.client.Post(app.server.URL+"/api/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	req, _ := http.NewRequest(http.MethodPut, app.server.URL+"/api/devices/ec20/vowifi/discovery", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	response, err = app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("write without CSRF status %d", response.StatusCode)
	}
}
