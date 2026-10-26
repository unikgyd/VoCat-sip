package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

const EPDGSettingKey = "vowifi.epdg.discovery.private.v1"

var epdgSettingGate sync.Mutex

type EPDGStore struct{ Store *store.Store }
type epdgDocument struct {
	Version  int                                    `json:"version"`
	Profiles map[string]vowifi.EPDGDiscoveryOptions `json:"profiles"`
	History  map[string]vowifi.EPDGSelection        `json:"history"`
}

func epdgHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (s EPDGStore) load(ctx context.Context) (epdgDocument, error) {
	doc := epdgDocument{Version: 1, Profiles: map[string]vowifi.EPDGDiscoveryOptions{}, History: map[string]vowifi.EPDGSelection{}}
	if s.Store == nil {
		return doc, errors.New("ePDG store unavailable")
	}
	setting, err := s.Store.AppSetting(ctx, EPDGSettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	var wire struct {
		Version  int                                    `json:"version"`
		Profiles map[string]vowifi.EPDGDiscoveryOptions `json:"profiles"`
		History  json.RawMessage                        `json:"history"`
	}
	if len(setting.Value) > 262144 || json.Unmarshal(setting.Value, &wire) != nil || wire.Version != 1 || len(wire.Profiles) > 128 {
		return doc, errors.New("invalid ePDG discovery settings; repair the private configuration")
	}
	if wire.Profiles != nil {
		doc.Profiles = wire.Profiles
	}
	// Optional history must not break a valid manual-only policy. Decode and
	// validate entries independently; malformed history simply loses priority.
	var history map[string]json.RawMessage
	if json.Unmarshal(wire.History, &history) == nil {
		for key, value := range history {
			if len(key) != 64 {
				continue
			}
			if _, err := hex.DecodeString(key); err != nil {
				continue
			}
			var entry vowifi.EPDGSelection
			if json.Unmarshal(value, &entry) != nil || entry.SucceededAt.IsZero() || len(entry.HomeMCC) != 3 || len(entry.HomeMNC) < 2 || len(entry.HomeMNC) > 3 || strings.Trim(entry.HomeMCC+entry.HomeMNC, "0123456789") != "" {
				continue
			}
			if host, err := vowifi.NormalizeEPDGHost(entry.Host); err == nil {
				entry.Host = host
				doc.History[key] = entry
			}
		}
	}
	return doc, nil
}
func (s EPDGStore) save(ctx context.Context, doc epdgDocument) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if len(data) > 262144 {
		return errors.New("ePDG settings capacity reached")
	}
	return s.Store.UpsertAppSetting(ctx, store.AppSetting{Key: EPDGSettingKey, Value: data, Sensitive: true})
}
func (s EPDGStore) Config(ctx context.Context, iccid string) (vowifi.EPDGDiscoveryOptions, error) {
	epdgSettingGate.Lock()
	defer epdgSettingGate.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return vowifi.EPDGDiscoveryOptions{}, err
	}
	return doc.Profiles[epdgHash(strings.TrimSpace(iccid))].Normalize()
}
func (s EPDGStore) SetConfig(ctx context.Context, iccid string, options vowifi.EPDGDiscoveryOptions) (vowifi.EPDGDiscoveryOptions, error) {
	if strings.TrimSpace(iccid) == "" || len(iccid) > 64 {
		return options, errors.New("verified SIM required")
	}
	options, err := options.Normalize()
	if err != nil {
		return options, err
	}
	options.LastSuccessful = nil
	epdgSettingGate.Lock()
	defer epdgSettingGate.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return options, err
	}
	key := epdgHash(strings.TrimSpace(iccid))
	if len(options.PreferredAddresses) == 0 && len(options.AdditionalPLMNs) == 0 && !options.ManualOnly && !options.GeographicDNSFallback {
		delete(doc.Profiles, key)
	} else {
		if _, exists := doc.Profiles[key]; !exists && len(doc.Profiles) >= 128 {
			return options, errors.New("ePDG profile limit reached")
		}
		doc.Profiles[key] = options
	}
	return options, s.save(ctx, doc)
}
func (s EPDGStore) LoadDiscovery(ctx context.Context, identity vowifi.SIMIdentity) (vowifi.EPDGDiscoveryOptions, error) {
	epdgSettingGate.Lock()
	defer epdgSettingGate.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return vowifi.EPDGDiscoveryOptions{}, err
	}
	options, err := doc.Profiles[epdgHash(strings.TrimSpace(identity.ICCID))].Normalize()
	if err != nil {
		return options, errors.New("invalid saved ePDG profile")
	}
	last, found := doc.History[epdgHash(strings.TrimSpace(identity.ICCID)+"\n"+strings.TrimSpace(identity.IMSI))]
	if found && vowifi.AuthenticationHomeAllowed(identity, last.HomeMCC, last.HomeMNC) && !last.SucceededAt.IsZero() {
		if host, err := vowifi.NormalizeEPDGHost(last.Host); err == nil {
			last.Host = host
			options.LastSuccessful = &last
		}
	}
	return options, nil
}
func (s EPDGStore) RememberRegistered(ctx context.Context, identity vowifi.SIMIdentity, candidate vowifi.EPDGCandidate) error {
	if strings.TrimSpace(identity.ICCID) == "" || strings.TrimSpace(identity.IMSI) == "" {
		return nil
	}
	host, err := vowifi.NormalizeEPDGHost(candidate.Host)
	if err != nil {
		return err
	}
	epdgSettingGate.Lock()
	defer epdgSettingGate.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return err
	}
	key := epdgHash(strings.TrimSpace(identity.ICCID) + "\n" + strings.TrimSpace(identity.IMSI))
	doc.History[key] = vowifi.EPDGSelection{Host: host, HomeMCC: identity.HomeMCC, HomeMNC: identity.HomeMNC, SucceededAt: time.Now().UTC()}
	keys := make([]string, 0, len(doc.History))
	for key := range doc.History {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := doc.History[keys[i]], doc.History[keys[j]]
		if a.SucceededAt.Equal(b.SucceededAt) {
			return keys[i] < keys[j]
		}
		return a.SucceededAt.After(b.SucceededAt)
	})
	for _, key := range keys[min(128, len(keys)):] {
		delete(doc.History, key)
	}
	return s.save(ctx, doc)
}
