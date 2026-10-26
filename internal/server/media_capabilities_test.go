package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"vocat/internal/vowifi/ims"
)

func TestMediaCapabilitySystemInfoNeedsSessionAndReportsLocalBackend(t *testing.T) {
	app := newTestApplication(t)
	response, err := app.client.Get(app.server.URL + "/api/system/info")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("anonymous media information exposed")
	}
	response, err = app.client.Post(app.server.URL+"/api/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = app.client.Get(app.server.URL + "/api/system/info")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Data struct {
			Media ims.MediaCapabilities `json:"media_capabilities"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("system media response", err)
	}
	expected := ims.ProbeMediaCapabilities()
	if body.Data.Media.RuntimeFlavor != expected.RuntimeFlavor || body.Data.Media.AMRNB != expected.AMRNB || body.Data.Media.AMRWB.Available {
		t.Fatalf("false backend capability %+v expected %+v", body, expected)
	}
}
