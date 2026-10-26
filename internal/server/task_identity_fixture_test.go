package server

import (
	"context"
	"vocat/internal/device"
	"vocat/internal/vowifi"
)

type automaticTaskTestWiFi struct{ *fakeVoWiFiController }

func (f *automaticTaskTestWiFi) StopDevice(context.Context, string) error {
	f.state.Enabled = false
	return nil
}
func (f *automaticTaskTestWiFi) Calls(string) ([]vowifi.Call, error) { return nil, nil }

func automaticTaskTestSnapshot(iccid string) *device.Snapshot {
	return &device.Snapshot{ICCID: iccid, IMSI: "310260123456789", IMEI: "123456789012340", SIMReady: true}
}
