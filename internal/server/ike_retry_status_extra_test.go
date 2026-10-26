package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func TestIKERetryStatusMalformedStoredObservationIsNotAValidCountdown(t *testing.T) {
	for _, value := range []string{`{"source":"operator","after_seconds":-1}`, `{"source":"operator","after_seconds":1116001}`, `{"source":"operator","scope":"raw-private-identity"}`, `{"source":"operator","after_seconds":"600"}`, `["not-a-status"]`} {
		got := retryStatusJSON(t, storedVoWiFiRuntime(store.VoWiFiRuntime{Extra: json.RawMessage(`{"ike_retry":` + value + `}`)}))
		if got["source"] != "status-unavailable" || got["until_card_event"] != true {
			t.Fatalf("invalid observation became a countdown: %#v", got)
		}
	}
}

func TestIKERetryStatusSerializationCarriesLiveObservationButNotPrivateDeadline(t *testing.T) {
	state := retryStatusState()
	recorder := httptest.NewRecorder()
	writeJSON(recorder, 200, map[string]any{"data": liveVoWiFiRuntime(state)})
	if recorder.Code != 200 {
		t.Fatal(recorder.Code)
	}
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := retryStatusJSON(t, response.Data)
	if got["source"] != "operator" || got["after_seconds"] != float64(600) || got["not_before"] != nil {
		t.Fatalf("HTTP serialization lost the observation: %#v", got)
	}
}

func TestIKERetryStatusAnotherDeviceCannotSupplyTheSelectedLineObservation(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	state := retryStatusState()
	s := &Server{store: database, vowifi: &fakeVoWiFiController{state: state}}
	entry := &device.Device{ID: "physical", Snapshot: &device.Snapshot{ICCID: state.ICCID}}
	response := s.configuredDeviceSummary(store.Device{ID: "other-device", VoWiFiEnabled: true}, entry)
	if response["vowifi_runtime"].(map[string]any)["ike_retry"] != nil {
		t.Fatal("another runtime supplied this line's retry state")
	}
}

func TestIKERetryStatusTableKeepsAllPauseAndTimerSourcesDistinct(t *testing.T) {
	for _, source := range []string{"operator", "implementation-missing", "implementation-invalid", "uncertain-attempt", "store-unavailable", "binding-unavailable"} {
		state := retryStatusState()
		state.IKERetry.Source = source
		state.IKERetry.UntilCardEvent = source == "uncertain-attempt" || source == "store-unavailable" || source == "binding-unavailable"
		got := retryStatusJSON(t, liveVoWiFiRuntime(state))
		if got["source"] != source {
			t.Fatalf("%s became %#v", source, got)
		}
	}
	state := retryStatusState()
	state.IKERetry = &vowifi.IKERetryStatus{Source: "operator", UntilCardEvent: true}
	state.UpdatedAt = time.Time{}
	if got := retryStatusJSON(t, liveVoWiFiRuntime(state)); got["observed_at"] != nil || got["until_card_event"] != true {
		t.Fatalf("invented timestamp or lost pause: %#v", got)
	}
}
