package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// Persist only task-owned settings. SIM PINs, proxy credentials, notification
// settings and unrelated device edits never enter the journal or get replayed.
type automaticTaskProjection struct {
	IMSMode        string
	IMSEnabled     bool
	Telephony      *store.TelephonyPolicy
	VoWiFiEnabled  bool
	NetworkEnabled bool
	APN            string
}

func taskProjection(config store.Device) automaticTaskProjection {
	value := automaticTaskProjection{config.IMSMode, config.IMSEnabled, nil, config.VoWiFiEnabled, config.NetworkEnabled, config.APN}
	if config.Telephony != nil {
		policy := *config.Telephony
		value.Telephony = &policy
	}
	return value
}

func (value automaticTaskProjection) apply(config store.Device) store.Device {
	config.IMSMode, config.IMSEnabled = value.IMSMode, value.IMSEnabled
	config.VoWiFiEnabled, config.NetworkEnabled, config.APN = value.VoWiFiEnabled, value.NetworkEnabled, value.APN
	config.Telephony = nil
	if value.Telephony != nil {
		policy := *value.Telephony
		config.Telephony = &policy
	}
	return config
}

type automaticTaskSavedEnvironment struct {
	Projection automaticTaskProjection
	Card       store.CardPolicy
}

type automaticTaskRecoveryPayload struct {
	Version         int
	Environment     string
	Original        automaticTaskSavedEnvironment
	Target          automaticTaskSavedEnvironment
	RevertProfile   bool
	SwitchAttempted bool
	InitialAID      string
	EID             string
	// Stable hardware evidence, rather than an enumerated wwan/tty index.
	IMEI              string
	USBPath           string
	ReaderName        string
	HardwareKind      string
	SerialNumber      string
	DeviceType        string
	ConfiguredIMEI    string
	ConfiguredUSBPath string
}

type automaticTaskJournalKey struct{}
type automaticTaskJournal struct {
	server  *Server
	record  store.AutomaticTaskRecovery
	payload automaticTaskRecoveryPayload
}

func (journal *automaticTaskJournal) save(ctx context.Context, phase string) error {
	payload, err := json.Marshal(journal.payload)
	if err != nil {
		return err
	}
	journal.record.Phase, journal.record.Payload = phase, payload
	journal.record.LastError = ""
	return journal.server.store.UpdateAutomaticTaskRecovery(ctx, journal.record)
}

func (s *Server) createAutomaticTaskJournal(ctx context.Context, task store.AutomaticTask, config store.Device) (*automaticTaskJournal, error) {
	entry, _, present := s.physicalForConfig(config)
	if !present {
		return nil, errors.New("configured device is offline")
	}
	identity, err := s.observedTelephonySubscription(ctx, config)
	if err != nil || identity.ICCID == "" || identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) {
		return nil, errors.Join(err, device.ErrSIMObservation)
	}
	if device.RegionBlockReason(identity.IMSI) != "" {
		return nil, device.ErrRegionBlocked
	}
	if config.Telephony != nil && config.Telephony.ICCID != identity.ICCID {
		return nil, device.ErrSIMObservation
	}
	originalCard, err := s.store.CardPolicy(ctx, identity.ICCID)
	if errors.Is(err, store.ErrNotFound) {
		originalCard, err = defaultCardPolicy(identity.ICCID), nil
	}
	if err != nil {
		return nil, err
	}
	target, err := s.taskCardSnapshot(ctx, config, vowifi.SIMIdentity{ICCID: task.ProfileICCID})
	if err != nil {
		return nil, err
	}
	payload := automaticTaskRecoveryPayload{Version: 1, Environment: task.Environment,
		Original:      automaticTaskSavedEnvironment{taskProjection(config), originalCard},
		Target:        automaticTaskSavedEnvironment{taskProjection(target.config), target.policy},
		RevertProfile: task.RevertProfile, USBPath: entry.Candidate.USBPath,
		ReaderName: entry.Candidate.ReaderName, HardwareKind: entry.Candidate.HardwareKind,
		SerialNumber: entry.Candidate.SerialNumber, DeviceType: config.DeviceType,
		ConfiguredIMEI: config.ModemIMEI, ConfiguredUSBPath: config.USBPath}
	if entry.Snapshot != nil {
		payload.IMEI = strings.TrimSpace(entry.Snapshot.IMEI)
	}
	if payload.IMEI == "" && payload.USBPath == "" && payload.ReaderName == "" {
		return nil, errors.New("automatic tasks require a stable modem or SIM reader identity for restart recovery")
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	journal := &automaticTaskJournal{server: s, payload: payload, record: store.AutomaticTaskRecovery{DeviceID: config.ID, TaskID: task.ID, Token: hex.EncodeToString(token[:]), Phase: "prepared"}}
	journal.record.Payload, err = json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateAutomaticTaskRecovery(ctx, journal.record); err != nil {
		return nil, err
	}
	return journal, nil
}

func (journal *automaticTaskJournal) verifyHardware(config store.Device) (device.Device, string, error) {
	p := journal.payload
	entry, physical, present := journal.server.physicalForConfig(config)
	if !present || config.DeviceType != p.DeviceType || config.ModemIMEI != p.ConfiguredIMEI || config.USBPath != p.ConfiguredUSBPath ||
		p.USBPath != "" && entry.Candidate.USBPath != p.USBPath ||
		p.ReaderName != "" && entry.Candidate.ReaderName != p.ReaderName ||
		p.HardwareKind != entry.Candidate.HardwareKind ||
		p.SerialNumber != "" && entry.Candidate.SerialNumber != p.SerialNumber ||
		p.IMEI != "" && (entry.Snapshot == nil || strings.TrimSpace(entry.Snapshot.IMEI) != p.IMEI) {
		return entry, "", device.ErrSIMObservation
	}
	return entry, physical, nil
}

func temporaryAutomaticTaskDevice(config store.Device) store.Device {
	config.IMSMode, config.IMSEnabled = store.IMSModeModule, false
	config.VoWiFiEnabled, config.NetworkEnabled = false, false
	if config.Telephony != nil {
		policy := *config.Telephony
		policy.CellularMode, policy.DefaultRoute, policy.WiFiRadioPolicy = store.IMSModeModule, store.IMSModeModule, "coexist"
		policy.AutoConnectCellular, policy.AutoConnectWiFi = false, false
		config.Telephony = &policy
	}
	return config
}

// Stop the original owner before exposing AT/UIM access. In particular, a Host
// IMS selection cannot be replaced by writing a temporary module preference.
func (s *Server) enterAutomaticTaskMaintenance(ctx context.Context, config store.Device) error {
	if err := s.stopTelephonyAccess(ctx, config); err != nil {
		return err
	}
	if err := s.quiesceOwnedData(ctx, config); err != nil {
		return err
	}
	return s.store.UpsertDevice(ctx, temporaryAutomaticTaskDevice(config))
}

func (journal *automaticTaskJournal) finishAfterChildren(ctx context.Context) {
	lease, _ := ctx.Value(automaticTaskLeaseKey{}).(*automaticTaskLease)
	if lease == nil {
		return
	}
	lease.finishRecovery = func() error {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return journal.server.store.DeleteAutomaticTaskRecovery(cleanup, journal.record.DeviceID, journal.record.Token)
	}
}

// Restoration is idempotent and never executes the business action. Unknown
// calls, foreign bearers, replacement cards and incomplete cleanup retain the
// durable admission fence for a later recovery attempt.
func (journal *automaticTaskJournal) restore(ctx context.Context) error {
	s := journal.server
	if journal.record.Phase == "restored" {
		journal.finishAfterChildren(ctx)
		return nil
	}
	if err := journal.save(ctx, "restoring"); err != nil {
		return err
	}
	current, err := s.store.Device(ctx, journal.record.DeviceID)
	if err != nil {
		return err
	}
	_, physical, err := journal.verifyHardware(current)
	if err != nil {
		return err
	}
	metadataOnly := journal.usesHostTaskMetadata(ctx, current)
	access := current
	if metadataOnly {
		access = journal.payload.Original.Projection.apply(current)
	}
	if err := s.requireAutomaticTaskCallsIdle(ctx, access); err != nil {
		return err
	}
	var live device.Snapshot
	if metadataOnly {
		if err := s.stopTelephonyAccess(ctx, access); err != nil {
			return err
		}
		if err := s.quiesceOwnedData(ctx, current); err != nil {
			return err
		}
		identity, err := s.hostTaskSubscription(ctx, current.ID, journal.payload.Original.Card.ICCID)
		if err != nil {
			return err
		}
		live.ICCID, live.IMSI = identity.ICCID, identity.IMSI
	} else {
		if err := s.enterAutomaticTaskMaintenance(ctx, current); err != nil {
			return err
		}
		current = temporaryAutomaticTaskDevice(current)
		live, err = s.devices.Refresh(ctx, physical)
		if err != nil {
			return err
		}
	}
	if _, verified, err := journal.verifyHardware(current); err != nil || verified != physical {
		return errors.Join(err, device.ErrSIMObservation)
	}
	iccid := strings.TrimSpace(live.ICCID)
	original, target := journal.payload.Original, journal.payload.Target
	if iccid == "" || iccid != original.Card.ICCID && iccid != target.Card.ICCID {
		return device.ErrSIMObservation
	}
	if current.Telephony != nil && current.Telephony.ICCID != iccid {
		policy := *current.Telephony
		policy.ICCID = iccid
		current.Telephony = &policy
		if err := s.store.UpsertDevice(ctx, current); err != nil {
			return err
		}
	}
	// Host-to-module AT observation must confirm there is no external call
	// before any RF/profile write, including recovery after process restart.
	if !metadataOnly {
		if err := s.requireModuleCallsIdle(ctx, current); err != nil {
			return err
		}
	}
	if journal.payload.RevertProfile && iccid != original.Card.ICCID {
		if !journal.payload.SwitchAttempted || journal.payload.EID == "" {
			return device.ErrSIMObservation
		}
		chip, err := s.devices.ESIMChipInfo(ctx, physical)
		if err != nil || chip == nil || strings.TrimSpace(chip.EID) != journal.payload.EID {
			return errors.Join(err, device.ErrSIMObservation)
		}
		identity := vowifi.SIMIdentity{ICCID: iccid, IMSI: strings.TrimSpace(live.IMSI)}
		if _, err := s.setSubscriptionFlight(ctx, physical, identity, true); err != nil {
			return err
		}
		switchErr := s.devices.ESIMSwitchProfile(ctx, physical, original.Card.ICCID, journal.payload.InitialAID)
		live, err = s.devices.Refresh(ctx, physical)
		if err != nil || strings.TrimSpace(live.ICCID) != original.Card.ICCID {
			return errors.Join(switchErr, err, device.ErrSIMObservation)
		}
		iccid = original.Card.ICCID
		if current.Telephony != nil {
			policy := *current.Telephony
			policy.ICCID = iccid
			current.Telephony = &policy
			if err := s.store.UpsertDevice(ctx, current); err != nil {
				return err
			}
		}
	}
	saved := target
	if iccid == original.Card.ICCID {
		saved = original
	}
	// Restore the target's legacy policy even when the task reverted to the
	// original card. Independent tasks never overwrite durable SIM preferences.
	if journal.payload.Target.Projection.Telephony == nil && target.Card.ICCID != original.Card.ICCID {
		if err := s.store.UpsertCardPolicy(ctx, target.Card); err != nil {
			return err
		}
	}
	snapshot := automaticTaskEnvironmentSnapshot{config: saved.Projection.apply(current), policy: saved.Card}
	if err := s.restoreAutomaticTaskEnvironment(physical, snapshot, ctx); err != nil {
		return err
	}
	if err := journal.save(ctx, "restored"); err != nil {
		return err
	}
	journal.finishAfterChildren(ctx)
	return nil
}

func (s *Server) recoverAutomaticTaskEnvironments(ctx context.Context) {
	for ctx.Err() == nil {
		records, err := s.store.ListAutomaticTaskRecoveries(ctx)
		if err == nil {
			jobs := make(chan store.AutomaticTaskRecovery)
			var workers sync.WaitGroup
			for i := 0; i < min(4, len(records)); i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for record := range jobs {
						s.recoverAutomaticTaskEnvironment(ctx, record)
					}
				}()
			}
			for _, record := range records {
				select {
				case jobs <- record:
				case <-ctx.Done():
				}
			}
			close(jobs)
			workers.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func (s *Server) recoverAutomaticTaskEnvironment(ctx context.Context, record store.AutomaticTaskRecovery) {
	operation, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	operation, release, err := s.beginAutomaticTaskRecovery(operation, record.DeviceID, record.Token)
	if err != nil {
		return
	} // Includes the still-running owner in this process.
	defer func() { _ = release() }()
	// The fleet snapshot can precede a live task's last journal write. Load
	// its final state under the writer rather than restoring an earlier phase.
	current, err := s.store.AutomaticTaskRecovery(operation, record.DeviceID)
	if err != nil || current.Token != record.Token {
		return
	}
	record = current
	journal := &automaticTaskJournal{server: s, record: record}
	if err := json.Unmarshal(record.Payload, &journal.payload); err != nil || journal.payload.Version != 1 {
		return // Retain unknown/corrupt versions rather than guessing a policy.
	}
	if err := journal.restore(operation); err != nil {
		// Raw device errors can include AT/APN credentials. Keep public status
		// useful without persisting a command transcript in the recovery row.
		journal.record.LastError = "Waiting for the original device, known SIM and confirmed call/access cleanup before restoring task settings."
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = s.store.UpdateAutomaticTaskRecovery(cleanup, journal.record)
	}
}

func recordAutomaticTaskProfileSwitch(ctx context.Context, aid, eid string) error {
	journal, _ := ctx.Value(automaticTaskJournalKey{}).(*automaticTaskJournal)
	if journal == nil {
		return nil
	}
	if eid == "" {
		return fmt.Errorf("eUICC identity is required before an automatic profile switch")
	}
	journal.payload.InitialAID, journal.payload.EID, journal.payload.SwitchAttempted = aid, eid, true
	return journal.save(ctx, "switching")
}
