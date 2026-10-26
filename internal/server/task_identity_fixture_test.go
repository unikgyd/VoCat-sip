package server

import "vocat/internal/device"

func automaticTaskTestSnapshot(iccid string) *device.Snapshot {
	return &device.Snapshot{ICCID: iccid, IMSI: "310260123456789", IMEI: "123456789012340", SIMReady: true}
}
