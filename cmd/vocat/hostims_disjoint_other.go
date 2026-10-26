//go:build !linux

package main

import (
	"vocat/internal/device"
	"vocat/internal/store"
)

func hostIMSPhysicalDisjoint(store.Device, []device.Device, string) bool { return false }
