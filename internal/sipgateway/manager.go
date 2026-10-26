package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"vocat/internal/store"
)

type repository interface {
	AppSetting(context.Context, string) (store.AppSetting, error)
	UpsertAppSetting(context.Context, store.AppSetting) error
}

// Durable settings admission is optional on a repository, but may never fall
// back to Upsert: that would erase another process's newer credential epochs.
type sipSettingsComparer interface {
	CompareSIPSettings(context.Context, json.RawMessage, json.RawMessage) (bool, error)
}
type View struct {
	Config Config `json:"config"`
	Status Status `json:"status"`
}
type Manager struct {
	mu           sync.Mutex
	db           repository
	current      stored
	savedRaw     json.RawMessage
	runtime      *runtime
	lastError    string
	closed       bool
	carrier      CarrierBackend
	journal      *callJournal
	incomingSeen map[string]time.Time
	sms          *smsService
}

// Invalid saved settings or a busy port must not make the whole management UI
// unavailable. They are reported in the status card and may be repaired there.
func New(ctx context.Context, db repository, options ...Option) *Manager {
	if capture, ok := db.(interface{ SetSMSForwardPlanner(store.SMSForwardPlanner) }); ok {
		capture.SetSMSForwardPlanner(captureSMSForwarding)
	}
	m := &Manager{db: db, current: stored{Version: 1, Config: DefaultConfig(), Secrets: map[string]secret{}}}
	m.journal = &callJournal{db: db}
	for _, option := range options {
		option(m)
	}
	if m.sms == nil {
		m.sms = newSMSService(db, nil)
	}
	value, err := db.AppSetting(ctx, SettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return m
	}
	if err != nil {
		m.lastError = "SIP settings could not be read"
		return m
	}
	m.savedRaw = append(json.RawMessage(nil), value.Value...)
	var saved stored
	if json.Unmarshal(value.Value, &saved) != nil || saved.Version != 1 {
		m.lastError = "SIP settings are invalid; save a new configuration"
		return m
	}
	clean, err := prepare(saved.Config, saved, false)
	if err != nil {
		m.lastError = "SIP settings are invalid; save a new configuration"
		return m
	}
	clean.requireDurableAuthority = true
	clean.ConfigRevision, err = newConfigRevision()
	if err != nil {
		m.lastError = "SIP settings revision could not be prepared"
		return m
	}
	m.current = clean
	// Even an unchanged document must be checked atomically before admission.
	// This also makes legacy local/peer epoch generation durable before any SIP
	// authentication or queued MT payload can use the new recipient identity.
	data, err := json.Marshal(clean)
	if err != nil {
		m.lastError = "SIP settings could not be prepared"
		return m
	}
	committed, err := m.compareSettings(ctx, m.savedRaw, data)
	if err != nil || !committed {
		m.lastError = "SIP settings could not be durably confirmed; service remains stopped"
		return m
	}
	m.savedRaw = append(json.RawMessage(nil), data...)
	if clean.Config.Enabled {
		m.runtime, err = start(clean, runtimeOptions{m.carrier, m.journal, m.sms})
		if err != nil {
			m.lastError = "SIP listener could not start: " + err.Error()
		}
	}
	return m
}
func (m *Manager) Snapshot() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}
func (m *Manager) snapshotLocked() View {
	config := cloneConfig(m.current.Config)
	status := Status{Listen: config.Listen, Error: m.lastError, Bindings: []Binding{}, Capabilities: Capabilities{Registration: true}, Peer: PeerStatus{State: "disabled"}}
	if m.runtime != nil {
		status = m.runtime.status()
		if m.lastError != "" {
			status.Error = m.lastError
		}
	}
	status.SMS = m.sms.statistics()
	return View{Config: config, Status: status}
}

func (m *Manager) Apply(ctx context.Context, request Config) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return View{}, errors.New("SIP service is closed")
	}
	next, err := prepare(request, m.current, false)
	if err != nil {
		return View{}, err
	}
	next.requireDurableAuthority = true
	previous := m.current
	previousRaw := append(json.RawMessage(nil), m.savedRaw...)
	unchanged := reflect.DeepEqual(next, previous) && m.lastError == "" && (!next.Config.Enabled || m.runtime != nil && m.runtime.status().Running)
	if next.Config.Enabled && m.runtime != nil && len(m.runtime.status().Calls) != 0 {
		if unchanged {
			return m.snapshotLocked(), nil
		}
		return View{}, errors.New("End active calls before changing SIP settings, or disable the SIP service")
	}
	next.ConfigRevision, err = newConfigRevision()
	if err != nil {
		return View{}, err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return View{}, errors.New("SIP settings could not be prepared")
	}
	committed, err := m.compareSettings(ctx, previousRaw, data)
	if err != nil {
		// A failed write may be ambiguous. Keep an existing listener only when
		// the authoritative bytes are still exactly its previous configuration.
		if !m.settingsStillMatch(previousRaw) {
			m.stopRuntimeLocked()
			m.lastError = "SIP settings could not be confirmed; service remains stopped"
		}
		return View{}, errors.New("SIP settings could not be durably saved")
	}
	if !committed {
		m.stopRuntimeLocked()
		m.lastError = "SIP settings changed in another manager; restart this manager before saving"
		return View{}, errors.New(m.lastError)
	}
	m.savedRaw = append(json.RawMessage(nil), data...)
	if unchanged {
		m.current = next
		return m.snapshotLocked(), nil
	}
	m.stopRuntimeLocked()
	var candidate *runtime
	if next.Config.Enabled {
		candidate, err = start(next, runtimeOptions{m.carrier, m.journal, m.sms})
		if err != nil {
			// Do not restore old settings over a concurrent manager's newer save.
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			restored, rollbackErr := m.compareSettings(cleanup, data, previousRaw)
			cancel()
			if rollbackErr != nil || !restored {
				m.current = next
				m.lastError = "SIP listener failed and settings changed or rollback could not be confirmed; service remains stopped"
				return View{}, errors.New(m.lastError)
			}
			m.savedRaw = previousRaw
			m.lastError = ""
			if previous.Config.Enabled {
				var restoreErr error
				m.runtime, restoreErr = start(previous, runtimeOptions{m.carrier, m.journal, m.sms})
				if restoreErr != nil {
					m.lastError = "SIP listener could not be restored: " + restoreErr.Error()
				}
			}
			return View{}, fmt.Errorf("SIP listener could not start: %w", err)
		}
	}
	m.current = next
	m.runtime = candidate
	m.lastError = ""
	return m.snapshotLocked(), nil
}

func (m *Manager) compareSettings(ctx context.Context, before, after json.RawMessage) (bool, error) {
	db, ok := m.db.(sipSettingsComparer)
	if !ok {
		return false, errors.New("repository lacks durable SIP settings comparison")
	}
	return db.CompareSIPSettings(ctx, before, after)
}

func (m *Manager) settingsStillMatch(expected json.RawMessage) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	value, err := m.db.AppSetting(ctx, SettingKey)
	if expected == nil {
		return errors.Is(err, store.ErrNotFound)
	}
	return err == nil && value.Sensitive && bytes.Equal(value.Value, expected)
}

func (m *Manager) stopRuntimeLocked() {
	if m.runtime != nil {
		m.runtime.close()
		m.runtime = nil
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.stopRuntimeLocked()
}
