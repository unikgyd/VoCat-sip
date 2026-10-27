//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"time"

	"vocat/internal/dataownership"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

type orphanDataRecovery struct {
	claim  *dataownership.Orphan
	serial io.Closer
}

// The singleton listener is already held before this coordinator starts. It
// never asks the main process to refresh a SIM through a fenced primary port.
func recoverOrphanData(ctx context.Context, database *store.Store, ports map[string]string, logger *slog.Logger) {
	journal := dataownership.Journal{Database: database}
	discoverer := modem.NewSystemDiscoverer()
	active := map[string]*orphanDataRecovery{}
	defer func() {
		for _, entry := range active {
			if entry.serial != nil {
				_ = entry.serial.Close()
			}
			_ = entry.claim.Close()
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for id, port := range ports {
			if ctx.Err() != nil {
				return
			}
			entry := active[id]
			if entry == nil {
				probe, cancel := context.WithTimeout(ctx, 5*time.Second)
				original, err := journal.OpenOrphan(probe, id)
				cancel()
				if err != nil || original == nil {
					continue
				}
				entry = &orphanDataRecovery{claim: original}
				active[id] = entry
				logger.Warn("dead data owner recovered; original claim remains fenced", "device_id", id, "category", "data_owner")
			}
			gone, err := entry.claim.OriginalGone()
			if err != nil {
				continue
			}
			if gone {
				if entry.serial != nil {
					if err := entry.serial.Close(); err != nil {
						continue
					}
					entry.serial = nil
				}
				cleanup, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := entry.claim.Complete(cleanup)
				cancel()
				if err == nil {
					delete(active, id)
					logger.Info("orphan data claim released after original USB and host cleanup", "device_id", id, "category", "data_owner")
				}
				continue
			}
			if entry.serial != nil || entry.claim.RecoveryAttempted() {
				continue
			}
			probe, cancel := context.WithTimeout(ctx, 30*time.Second)
			candidates, err := discoverer.Discover(probe)
			if err == nil {
				for _, candidate := range candidates {
					if candidate.ID != id || !entry.claim.Matches(candidate) {
						continue
					}
					serial, submitted, _ := device.RestartOrphanData(probe, candidate, port, entry.claim, func(ctx context.Context, c modem.Candidate, iccid string) bool {
						return allowOwnedDataRecovery(ctx, database, c, iccid)
					})
					entry.serial = serial
					if submitted {
						logger.Warn("dead owner's original-modem recovery submitted once", "device_id", id, "category", "data_owner")
					}
					break
				}
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
