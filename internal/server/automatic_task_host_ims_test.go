package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type hostTaskHardware struct {
	*revertTestController
	host     *dispatchTestController
	noLegacy bool
}

func (h *hostTaskHardware) ExecuteAT(ctx context.Context, id, command string) (modem.Response, error) {
	state, _ := h.host.State("")
	if state.Active || h.noLegacy {
		return modem.Response{}, errors.New("legacy AT used under active Host IMS")
	}
	return h.revertTestController.ExecuteAT(ctx, id, command)
}
func (h *hostTaskHardware) Refresh(ctx context.Context, id string) (device.Snapshot, error) {
	state, _ := h.host.State("")
	if state.Active || h.noLegacy {
		return device.Snapshot{}, errors.New("legacy refresh used under active Host IMS")
	}
	return h.revertTestController.Refresh(ctx, id)
}
func (*hostTaskHardware) SendSMS(context.Context, string, string, string) (device.SMSSendResult, error) {
	return device.SMSSendResult{}, errors.New("Host IMS task fell back to module SMS")
}

type hostTaskIMS struct {
	*dispatchTestController
	hardware    *hostTaskHardware
	sendFailure error
}

func (h *hostTaskIMS) Subscription(context.Context, string) (vowifi.SIMIdentity, error) {
	return vowifi.SIMIdentity{ICCID: h.hardware.currentICCID, IMSI: "310260123456789"}, nil
}

func TestAutomaticHostIMSTaskCurrentCardAndRecoveryAvoidLegacyIO(t *testing.T) {
	for _, scenario := range []string{"normal", "old-journal", "new-journal", "changed-card"} {
		t.Run(scenario, func(t *testing.T) {
			s := blockedRegionServer(t, "310260123456789")
			core := &dispatchTestController{}
			hardware := &hostTaskHardware{host: core, noLegacy: true, revertTestController: &revertTestController{
				currentICCID: "8901000000000000000", registration: 1,
				fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "dev1", Discovered: true, Snapshot: automaticTaskTestSnapshot("8901000000000000000")}},
			}}
			host := &hostTaskIMS{dispatchTestController: core, hardware: hardware}
			s.devices, s.hostIMS, s.wifiIMS = hardware, host, &dispatchTestController{}
			s.vowifi = &imsServiceDispatch{server: s}
			s.readSubscription = host.Subscription
			ctx := context.Background()
			config := store.Device{ID: "dev1", Name: "Modem", SMSEnabled: true, IMSMode: store.IMSModeHost,
				Telephony: &store.TelephonyPolicy{ICCID: hardware.currentICCID, CellularMode: store.IMSModeHost, DefaultRoute: store.IMSModeHost, AutoConnectCellular: true, WiFiRadioPolicy: "coexist"}}
			card := store.CardPolicy{ICCID: hardware.currentICCID, Source: "user"}
			if err := s.store.SaveTelephonyPolicy(ctx, config, *config.Telephony, card); err != nil {
				t.Fatal(err)
			}
			storedConfig, err := s.store.Device(ctx, config.ID)
			if err != nil {
				t.Fatal(err)
			}
			config = storedConfig
			task, err := s.store.SaveAutomaticTask(ctx, store.AutomaticTask{Name: "Host IMS", DeviceID: config.ID, ProfileICCID: hardware.currentICCID, TaskType: "sms", Environment: store.IMSModeHost,
				IntervalDays: 1, StartDate: "2026-10-27", RunTime: "12:00", Timezone: "Asia/Taipei", Payload: []byte(`{"phone":"+15550000001","message":"synthetic"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "normal" {
				if _, err := s.executeAutomaticTask(ctx, task, nil); err != nil || host.boundMessages != 1 {
					t.Fatal("same-card Host IMS task used legacy access or failed", err)
				}
			} else {
				journal, err := s.createAutomaticTaskJournal(ctx, task, config)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "old-journal" {
					journal.payload.Environment = ""
					journal.record.Payload, err = json.Marshal(journal.payload)
					if err != nil {
						t.Fatal(err)
					}
					if err := s.store.UpdateAutomaticTaskRecovery(ctx, journal.record); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.store.UpsertDevice(ctx, temporaryAutomaticTaskDevice(config)); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed-card" {
					hardware.currentICCID = "8901000000000000001"
				}
				s.recoverAutomaticTaskEnvironment(ctx, journal.record)
				if host.boundMessages != 0 {
					t.Fatal("recovery replayed SMS")
				}
				if scenario == "changed-card" {
					if err := s.store.RequireAutomaticTaskRecovered(ctx, config.ID); err == nil {
						t.Fatal("changed SIM erased the original task fence")
					}
					return
				}
			}
			got, err := s.store.Device(ctx, config.ID)
			if err != nil || got.Telephony == nil || *got.Telephony != *config.Telephony {
				t.Fatal("saved Host IMS projection not restored", err)
			}
			if err := s.store.RequireAutomaticTaskRecovered(ctx, config.ID); err != nil {
				t.Fatal("successful original-card cleanup retained the fence", err)
			}
			for _, action := range hardware.actions {
				if action == "register" || action == "automatic:true" || action == "data:true" {
					t.Fatal("same-card Host IMS used legacy radio/data activation", hardware.actions)
				}
			}
		})
	}
}

func (h *hostTaskIMS) SendSMS(ctx context.Context, id string, r vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	state, _ := h.State(id)
	return h.SendSMSForSubscription(ctx, id, state.IMSI, state.ICCID, r)
}

func (h *hostTaskIMS) StopDevice(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stops++
	h.state = vowifi.State{}
	return h.stopErr
}
func (h *hostTaskIMS) RequestEnabled(_ string, enabled bool) (vowifi.State, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled++
	h.state = vowifi.State{Enabled: enabled, Active: enabled, SIMReady: true, IMSReady: enabled, SMSReady: enabled,
		ICCID: h.hardware.currentICCID, IMSI: "310260123456789"}
	return h.state, nil
}
func (h *hostTaskIMS) SendSMSForSubscription(_ context.Context, _ string, imsi, iccid string, r vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.state.IMSReady || iccid != h.hardware.currentICCID || imsi != h.state.IMSI {
		return vowifi.SMSSubmitResult{}, device.ErrSIMObservation
	}
	h.boundMessages++
	if h.sendFailure != nil {
		return vowifi.SMSSubmitResult{To: r.Recipient, PartsTotal: 1, PartsAttempted: 1}, h.sendFailure
	}
	return vowifi.SMSSubmitResult{To: r.Recipient, PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true, SubmissionStatus: "accepted_by_ims"}, nil
}

func TestAutomaticHostIMSTaskSMSRestoresOriginalAndSwitchedProfiles(t *testing.T) {
	for _, target := range []string{"8901000000000000000", "8901000000000000001"} {
		t.Run(target, func(t *testing.T) {
			s := blockedRegionServer(t, "310260123456789")
			var logs bytes.Buffer
			s.logger = slog.New(slog.NewTextHandler(&logs, nil))
			core := &dispatchTestController{}
			hardware := &hostTaskHardware{host: core, revertTestController: &revertTestController{
				currentICCID: "8901000000000000000", registration: 1,
				fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "dev1", Discovered: true, Snapshot: automaticTaskTestSnapshot("8901000000000000000")}},
			}}
			host := &hostTaskIMS{dispatchTestController: core, hardware: hardware}
			wifi := &dispatchTestController{}
			s.devices, s.hostIMS, s.wifiIMS = hardware, host, wifi
			s.vowifi = &imsServiceDispatch{server: s}
			s.readSubscription = func(context.Context, string) (vowifi.SIMIdentity, error) {
				return vowifi.SIMIdentity{ICCID: hardware.currentICCID, IMSI: "310260123456789"}, nil
			}
			ctx := context.Background()
			original := store.Device{ID: "dev1", Name: "Modem", SMSEnabled: true}
			if err := s.store.UpsertDevice(ctx, original); err != nil {
				t.Fatal(err)
			}
			for _, iccid := range []string{"8901000000000000000", "8901000000000000001"} {
				if err := s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: iccid, Source: "user"}); err != nil {
					t.Fatal(err)
				}
			}
			output, err := s.executeAutomaticTask(ctx, store.AutomaticTask{DeviceID: "dev1", ProfileICCID: target, TaskType: "sms", Environment: "host_ims", RevertProfile: true,
				Payload: []byte(`{"phone":"+15550000001","message":"synthetic task"}`)}, nil)
			if err != nil || output == "" || host.boundMessages != 1 || wifi.boundMessages != 0 || hardware.currentICCID != "8901000000000000000" {
				t.Fatalf("Host IMS task/restore: messages=%d output=%q error=%v logs=%s", host.boundMessages, output, err, logs.String())
			}
			if (target != "8901000000000000000") != (len(hardware.switches) == 2) {
				t.Fatal("profile switch/revert did not match the requested target", hardware.switches)
			}
			for _, action := range hardware.actions {
				if action == "register" || action == "automatic:true" || action == "data:true" {
					t.Fatal("Host IMS task used legacy cellular activation", hardware.actions)
				}
			}
			restored, err := s.store.Device(ctx, "dev1")
			if err != nil || restored.Telephony != nil || restored.NetworkEnabled || restored.VoWiFiEnabled || core.state.Active {
				t.Fatal("legacy projection/IMS lifetime was not restored", err)
			}
			if err := s.store.RequireAutomaticTaskRecovered(ctx, "dev1"); err != nil {
				t.Fatal("successful restore retained the task fence", err)
			}
		})
	}
}

func TestAutomaticHostIMSTaskWaitRejectsWrongOwnerAndSubscription(t *testing.T) {
	for _, scenario := range []string{"ready", "wrong_card", "wrong_imsi", "no_sms", "wrong_route", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			config := dispatchConfig("line", store.IMSModeHost)
			config.Telephony = &store.TelephonyPolicy{ICCID: "8901000000000000000", CellularMode: store.IMSModeHost, AutoConnectCellular: true, DefaultRoute: store.IMSModeHost, WiFiRadioPolicy: "coexist"}
			s, wifi, host, hardware := newDispatchTestServer(t, config)
			host.state.ICCID, host.state.IMSI, host.state.Active = config.Telephony.ICCID, "310260123456789", true
			s.readSubscription = func(context.Context, string) (vowifi.SIMIdentity, error) {
				imsi := host.state.IMSI
				if scenario == "wrong_imsi" {
					imsi = "310260000000000"
				}
				return vowifi.SIMIdentity{ICCID: config.Telephony.ICCID, IMSI: imsi}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			switch scenario {
			case "wrong_card":
				host.state.ICCID = "8901000000000000001"
			case "no_sms":
				host.state.SMSReady = false
			case "wrong_route":
				policy := *config.Telephony
				policy.DefaultRoute = store.IMSModeVoWiFi
				config.Telephony = &policy
				if err := s.store.UpsertDevice(ctx, config); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			err := s.waitAutomaticHostIMS(ctx, "line", "8901000000000000000", true)
			if (err == nil) != (scenario == "ready") || hardware.at.Load()+hardware.sms.Load()+hardware.refresh.Load()+hardware.flight.Load() != 0 || wifi.enabled != 0 {
				t.Fatal("task admitted a stale/other owner or used legacy I/O", err)
			}
		})
	}
}

func TestAutomaticHostIMSTaskAPIAndStorage(t *testing.T) {
	s := blockedRegionServer(t, "310260123456789")
	if err := s.store.UpsertDevice(context.Background(), store.Device{ID: "dev1", Name: "Modem"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"sms", "call", "public_ip", "cellular_attach"} {
		body := fmt.Sprintf(`{"name":"Host task","device_id":"dev1","profile_iccid":"8901000000000000000","environment":"host_ims","task_type":%q,"interval_days":1,"start_date":"2026-10-27","run_time":"12:00","timezone":"UTC","payload":{"phone":"+15550000001","message":"synthetic","duration_seconds":1}}`, kind)
		task, err := s.decodeAutomaticTask(httptest.NewRequest("POST", "/api/automatic-tasks", strings.NewReader(body)), 0)
		if (err == nil) != (kind == "sms" || kind == "call") {
			t.Fatalf("type=%s error=%v", kind, err)
		}
		if err == nil {
			task, err = s.store.SaveAutomaticTask(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.store.AutomaticTask(context.Background(), task.ID)
			if err != nil || got.Environment != "host_ims" {
				t.Fatal("task environment was not preserved", err)
			}
		}
	}
}
