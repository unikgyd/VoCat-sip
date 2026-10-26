package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type profileIdentityEntry struct {
	IMSI      string    `json:"imsi"`
	Seen      time.Time `json:"seen"`
	Succeeded time.Time `json:"succeeded,omitempty"`
}
type profileIdentityDocument struct {
	Version int                               `json:"version"`
	Cards   map[string][]profileIdentityEntry `json:"cards"`
}

var errProfileIdentityHistory = errors.New("private SIM identity history is unavailable or invalid")

func historyIMSI(value string) bool {
	if len(value) < 6 || len(value) > 16 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func historyKey(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32
}
func (s EPDGStore) loadProfileIdentities(ctx context.Context) (profileIdentityDocument, json.RawMessage, error) {
	doc := profileIdentityDocument{Version: 1, Cards: map[string][]profileIdentityEntry{}}
	if s.Store == nil {
		return doc, nil, errProfileIdentityHistory
	}
	setting, err := s.Store.AppSetting(ctx, store.ProfileIdentityHistoryKey)
	if errors.Is(err, store.ErrNotFound) {
		return doc, nil, nil
	}
	if err != nil || !setting.Sensitive || len(setting.Value) > store.MaxProfileIdentityHistoryBytes {
		return doc, nil, errProfileIdentityHistory
	}
	var decoded profileIdentityDocument
	if json.Unmarshal(setting.Value, &decoded) != nil || decoded.Version != 1 || len(decoded.Cards) > 128 {
		return doc, nil, errProfileIdentityHistory
	}
	for key, entries := range decoded.Cards {
		if !historyKey(key) || len(entries) > vowifi.MaxProfileIdentities {
			continue
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			if !historyIMSI(entry.IMSI) || entry.Seen.IsZero() || seen[entry.IMSI] {
				continue
			}
			seen[entry.IMSI] = true
			doc.Cards[key] = append(doc.Cards[key], entry)
		}
	}
	return doc, append(json.RawMessage(nil), setting.Value...), nil
}
func sortedProfileEntries(entries []profileIdentityEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Succeeded.IsZero() != b.Succeeded.IsZero() {
			return !a.Succeeded.IsZero()
		}
		if !a.Succeeded.Equal(b.Succeeded) {
			return a.Succeeded.After(b.Succeeded)
		}
		if !a.Seen.Equal(b.Seen) {
			return a.Seen.After(b.Seen)
		}
		return a.IMSI < b.IMSI
	})
}
func (s EPDGStore) updateProfileIdentities(ctx context.Context, live vowifi.SIMIdentity, success string) ([]profileIdentityEntry, error) {
	if live.ICCID == "" || !historyIMSI(live.IMSI) {
		return nil, nil
	}
	key := epdgHash(live.ICCID)
	for attempt := 0; attempt < 8; attempt++ {
		doc, before, err := s.loadProfileIdentities(ctx)
		if err != nil {
			return nil, err
		}
		entries := doc.Cards[key]
		now := time.Now().UTC()
		observe := func(imsi string, succeeded bool) {
			if !historyIMSI(imsi) {
				return
			}
			for i := range entries {
				if entries[i].IMSI == imsi {
					entries[i].Seen = now
					if succeeded {
						entries[i].Succeeded = now
					}
					return
				}
			}
			entry := profileIdentityEntry{IMSI: imsi, Seen: now}
			if succeeded {
				entry.Succeeded = now
			}
			entries = append(entries, entry)
		}
		for _, id := range vowifi.CurrentProfileIdentities(live) {
			if id.ICCID != live.ICCID {
				return nil, errProfileIdentityHistory
			}
			observe(id.IMSI, false)
		}
		if success != "" {
			observe(success, true)
		}
		sortedProfileEntries(entries)
		if len(entries) > vowifi.MaxProfileIdentities {
			current := map[string]bool{}
			for _, id := range vowifi.CurrentProfileIdentities(live) {
				current[id.IMSI] = true
			}
			kept := make([]profileIdentityEntry, 0, vowifi.MaxProfileIdentities)
			for _, entry := range entries {
				if current[entry.IMSI] && len(kept) < vowifi.MaxProfileIdentities {
					kept = append(kept, entry)
				}
			}
			for _, entry := range entries {
				if !current[entry.IMSI] && len(kept) < vowifi.MaxProfileIdentities {
					kept = append(kept, entry)
				}
			}
			entries = kept
			sortedProfileEntries(entries)
		}
		doc.Cards[key] = entries
		if len(doc.Cards) > 128 {
			keys := make([]string, 0, len(doc.Cards))
			for k := range doc.Cards {
				keys = append(keys, k)
			}
			latest := func(k string) time.Time {
				var found time.Time
				for _, entry := range doc.Cards[k] {
					if entry.Seen.After(found) {
						found = entry.Seen
					}
				}
				return found
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := latest(keys[i]), latest(keys[j])
				if a.Equal(b) {
					return keys[i] < keys[j]
				}
				return a.After(b)
			})
			for _, k := range keys[128:] {
				delete(doc.Cards, k)
			}
		}
		after, err := json.Marshal(doc)
		if err != nil {
			return nil, errProfileIdentityHistory
		}
		ok, err := s.Store.CompareProfileIdentityHistory(ctx, before, after)
		if err != nil {
			return nil, fmt.Errorf("%w: durable compare failed", errProfileIdentityHistory)
		}
		if ok {
			return entries, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: compare contention limit", errProfileIdentityHistory)
}

func (s EPDGStore) IdentityCandidates(ctx context.Context, live vowifi.SIMIdentity) ([]vowifi.SIMIdentity, error) {
	entries, err := s.updateProfileIdentities(ctx, live, "")
	if err != nil {
		return nil, err
	}
	var result []vowifi.SIMIdentity
	seen := map[string]bool{}
	add := func(id vowifi.SIMIdentity) {
		if !seen[id.IMSI] && len(result) < vowifi.MaxProfileIdentities {
			seen[id.IMSI] = true
			result = append(result, id)
		}
	}
	addEntry := func(entry profileIdentityEntry) {
		if id, err := vowifi.ProfileIdentityFromHistory(live, entry.IMSI); err == nil {
			add(id)
		}
	}
	for _, entry := range entries {
		if !entry.Succeeded.IsZero() {
			addEntry(entry)
			break // most recent success first, then the current physical reads
		}
	}
	for _, id := range vowifi.CurrentProfileIdentities(live) {
		add(id)
	}
	for _, entry := range entries {
		addEntry(entry)
	}
	return result, nil
}
func (s EPDGStore) RememberIdentityRegistered(ctx context.Context, selected vowifi.SIMIdentity) error {
	physical, err := vowifi.AKAIdentity(selected)
	if err != nil {
		return err
	}
	_, err = s.updateProfileIdentities(ctx, physical, selected.IMSI)
	return err
}
