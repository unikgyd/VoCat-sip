package server

import (
	"context"
	"errors"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// EnableProfile is already committed and verified by the device manager. Its
// complete ICCID/IMSI snapshot can lag after the modem's REFRESH or MBN recovery.
// Wait for that original device's metadata; never submit EnableProfile again or
// combine the target ICCID with the previous profile's IMSI.
func (s *Server) awaitSwitchedSubscription(ctx context.Context, config store.Device, physicalID, iccid string) (vowifi.SIMIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	for {
		entry, physical, present := s.physicalForConfig(config)
		if !present || physical != physicalID {
			return vowifi.SIMIdentity{}, device.ErrSIMObservation
		}
		identity, err := s.observedTelephonySubscription(ctx, config)
		latest, latestPhysical, latestPresent := s.physicalForConfig(config)
		if !latestPresent || latestPhysical != physicalID || latest.Candidate.USBGeneration != entry.Candidate.USBGeneration {
			return vowifi.SIMIdentity{}, device.ErrSIMObservation
		}
		equipmentMatches := config.ModemIMEI == "" || latest.Snapshot != nil && latest.Snapshot.IMEI == config.ModemIMEI
		if err == nil && equipmentMatches && identity.ICCID == iccid && identity.IMSI != "" && !device.IsPlaceholderIMSI(identity.IMSI) {
			return identity, nil
		}
		// The caller owns the line writer, so background refresh/reconciliation
		// cannot publish this snapshot for us. Reuse the bounded device refresh
		// here; its guarded recovery keeps uncertain AT replies fenced.
		refreshCtx, cancelRefresh := context.WithTimeout(ctx, 8*time.Second)
		_, _ = s.devices.Refresh(refreshCtx, physicalID)
		cancelRefresh()
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return vowifi.SIMIdentity{}, errors.Join(device.ErrSIMObservation, ctx.Err())
		case <-timer.C:
		}
	}
}
