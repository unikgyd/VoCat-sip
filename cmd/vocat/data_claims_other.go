//go:build !linux

package main

import (
	"vocat/internal/device"
	"vocat/internal/store"
)

func newDataClaimJournal(*store.Store) device.DataClaimJournal { return nil }
