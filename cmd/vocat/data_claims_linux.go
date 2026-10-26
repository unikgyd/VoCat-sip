//go:build linux

package main

import (
	"vocat/internal/dataownership"
	"vocat/internal/device"
	"vocat/internal/store"
)

func newDataClaimJournal(database *store.Store) device.DataClaimJournal {
	return dataownership.Journal{Database: database}
}
