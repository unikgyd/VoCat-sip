package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
)

func TestAutomaticTaskAdmissionWaitDoesNotReplayBusiness(t *testing.T) {
	for _, scenario := range []string{"release", "cancel", "business-busy"} {
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
			config := store.Device{ID: "dev1", Name: "Modem", SMSEnabled: true, IMSMode: store.IMSModeHost,
				Telephony: &store.TelephonyPolicy{ICCID: hardware.currentICCID, CellularMode: store.IMSModeHost, DefaultRoute: store.IMSModeHost, AutoConnectCellular: true, WiFiRadioPolicy: "coexist"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := s.store.SaveTelephonyPolicy(ctx, config, *config.Telephony, store.CardPolicy{ICCID: hardware.currentICCID, Source: "user"}); err != nil {
				t.Fatal(err)
			}
			task := store.AutomaticTask{DeviceID: config.ID, ProfileICCID: hardware.currentICCID, Environment: store.IMSModeHost, TaskType: "sms", RetryCount: 0,
				Payload: []byte(`{"phone":"+15550000001","message":"synthetic"}`)}
			if scenario == "business-busy" {
				host.sendFailure = errLineBusy
				if _, err := s.executeAutomaticTaskWhenAvailable(ctx, task, nil); err == nil || host.boundMessages != 1 {
					t.Fatal("business-stage busy error was replayed or ignored", err, host.boundMessages)
				}
				return
			}
			gate := s.imsModeLock(config.ID)
			gate.Lock()
			locked := true
			defer func() {
				if locked {
					gate.Unlock()
				}
			}()
			waiting, done := make(chan struct{}, 1), make(chan error, 1)
			go func() {
				_, err := s.executeAutomaticTaskWhenAvailable(ctx, task, func(message string) {
					if strings.Contains(message, "尚未提交业务") {
						select {
						case waiting <- struct{}{}:
						default:
						}
					}
				})
				done <- err
			}()
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("queued task did not wait for the line")
			}
			if host.boundMessages != 0 || s.store.RequireAutomaticTaskRecovered(ctx, config.ID) != nil {
				t.Fatal("unadmitted task submitted business or created a journal")
			}
			if scenario == "cancel" {
				cancel()
			} else {
				gate.Unlock()
				locked = false
			}
			select {
			case err := <-done:
				if scenario == "cancel" {
					if !errors.Is(err, context.Canceled) || host.boundMessages != 0 {
						t.Fatal("canceled admission submitted SMS", err)
					}
				} else if err != nil || host.boundMessages != 1 {
					t.Fatal("released admission did not execute exactly once", err, host.boundMessages)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("task did not finish after admission/cancellation")
			}
		})
	}
}
