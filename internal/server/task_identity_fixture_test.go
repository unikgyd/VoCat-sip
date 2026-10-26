package server

import (
	"context"
	"errors"
	"testing"
	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type automaticTaskTestWiFi struct{ *fakeVoWiFiController }

func (f *automaticTaskTestWiFi) StopDevice(context.Context, string) error {
	f.state.Enabled = false
	return nil
}

func TestAutomaticTaskUnknownIdleCannotMutateOrCreateRecoveryJournal(t *testing.T) {
	s := blockedRegionServer(t, "310260123456789")
	s.vowifi = nil
	c := &registrationTaskController{idleErr: errors.New("unconfirmed idle"), fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "dev1", Discovered: true, Snapshot: automaticTaskTestSnapshot("8901000000000000002")}}}
	s.devices = c
	if err := s.store.UpsertDevice(context.Background(), store.Device{ID: "dev1", Name: "Modem"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.executeAutomaticTask(context.Background(), store.AutomaticTask{DeviceID: "dev1", ProfileICCID: "8901000000000000002", TaskType: "cellular_attach", Environment: "cellular", Payload: []byte(`{}`)}, nil); err == nil {
		t.Fatal("unknown CLCC response admitted task")
	}
	if len(c.actions) != 0 {
		t.Fatal("unconfirmed call state changed modem", c.actions)
	}
	if err := s.store.RequireAutomaticTaskRecovered(context.Background(), "dev1"); err != nil {
		t.Fatal("unstarted task left a recovery reservation", err)
	}
}
func (f *automaticTaskTestWiFi) Calls(string) ([]vowifi.Call, error) { return nil, nil }

func automaticTaskTestSnapshot(iccid string) *device.Snapshot {
	return &device.Snapshot{ICCID: iccid, IMSI: "310260123456789", IMEI: "123456789012340", SIMReady: true}
}
