package server

import (
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type subscriptionObservation struct {
	identity             vowifi.SIMIdentity
	physical, generation string
	available            bool
	at                   time.Time
}

// Project only metadata that the Host IMS observer has verified for this USB
// generation. Legacy AT snapshots are deliberately not refreshed under MM.
// This copy is presentation only; all mutations re-read their own identity.
func (s *Server) projectHostSubscription(config store.Device, entry *device.Device) (*device.Device, bool) {
	if !store.HostIMSConfigured(config) {
		if !s.awaitingLegacySubscription(config, entry) {
			return entry, true
		}
		if entry == nil {
			return nil, false
		}
		copy := *entry
		snapshot := device.Snapshot{DeviceID: entry.ID, IMEI: config.ModemIMEI, OperatingMode: -1, SIMStatus: "unknown"}
		if entry.Snapshot != nil {
			snapshot = *entry.Snapshot
			snapshot.ICCID, snapshot.IMSI = "", ""
			snapshot.SIMReady = false
			snapshot.SIMStatus = "unknown"
			snapshot.Phone = device.PhoneNumber{}
			snapshot.SPN, snapshot.GID1, snapshot.GID2 = "", "", ""
			snapshot.MNCLength = 0
			snapshot.IdentityFilesRead = false
			snapshot.OperatorName, snapshot.OperatorCode = "", ""
			snapshot.RegistrationStatus = 0
			snapshot.PSAttached = false
		}
		copy.Snapshot = &snapshot
		return &copy, false
	}
	if entry == nil {
		return nil, false
	}
	copy := *entry
	snapshot := device.Snapshot{DeviceID: entry.ID, IMEI: config.ModemIMEI, OperatingMode: -1}
	if entry.Snapshot != nil {
		snapshot = *entry.Snapshot
	}
	snapshot.ICCID, snapshot.IMSI = "", ""
	snapshot.SIMReady = false
	snapshot.SIMStatus = "unknown"
	snapshot.Phone = device.PhoneNumber{}
	snapshot.SPN, snapshot.GID1, snapshot.GID2 = "", "", ""
	snapshot.MNCLength = 0
	snapshot.IdentityFilesRead = false
	snapshot.OperatorName, snapshot.OperatorCode = "", ""
	snapshot.RegistrationStatus = 0
	snapshot.PSAttached = false
	verified := false
	if value, exists := s.subscriptionObservations.Load(config.ID); exists {
		if observed, ok := value.(subscriptionObservation); ok && observed.available && time.Since(observed.at) <= 15*time.Second && observed.physical == entry.ID && observed.generation == entry.Candidate.USBGeneration {
			snapshot.ICCID, snapshot.IMSI = observed.identity.ICCID, observed.identity.IMSI
			snapshot.UpdatedAt = observed.at.UTC()
			verified = true
			if snapshot.ICCID == "" {
				snapshot.SIMStatus = "not_inserted"
			} else {
				snapshot.SIMStatus = "ready"
				snapshot.SIMReady = true
			}
		}
	}
	copy.Snapshot = &snapshot
	return &copy, verified && snapshot.ICCID != ""
}

func (s *Server) awaitingLegacySubscription(config store.Device, entry *device.Device) bool {
	if config.Telephony == nil || store.HostIMSConfigured(config) || entry == nil {
		return false
	}
	value, exists := s.subscriptionTransitions.Load(config.ID)
	if !exists {
		return false
	}
	barrier, ok := value.(subscriptionObservation)
	if !ok || barrier.physical != entry.ID || barrier.generation != entry.Candidate.USBGeneration {
		s.subscriptionTransitions.Delete(config.ID)
		return false
	}
	if entry.Snapshot != nil && entry.Snapshot.UpdatedAt.After(barrier.at) {
		s.subscriptionTransitions.Delete(config.ID)
		return false
	}
	return true
}
