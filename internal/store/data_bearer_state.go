package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const MaxDataBearerClaimBytes = 32768
const DataBearerClaimPrefix = "cellular.data.owner.private.v1."

// CompareDataBearerClaim commits ordinary-data ownership with the same
// FULL-synchronous, exact-byte CAS used by the private send/settings journals.
// It cannot modify an IMS claim, expire a record or replace another owner.
func (s *Store) CompareDataBearerClaim(ctx context.Context, key string, before, after json.RawMessage) (bool, error) {
	suffix, ok := strings.CutPrefix(key, DataBearerClaimPrefix)
	decoded, err := hex.DecodeString(suffix)
	if !ok || err != nil || len(decoded) != 32 {
		return false, errors.New("invalid ordinary data claim key")
	}
	return s.compareSIPPrivateState(ctx, key, before, after, MaxDataBearerClaimBytes)
}

// Equipment and physical checks are presence fences, never recovery tokens.
// Unknown records cannot prove another modem is safe to mutate.
func (s *Store) HasDataBearerClaim(ctx context.Context, equipmentKey, physicalKey string) (bool, error) {
	if equipmentKey == "" && physicalKey == "" {
		return false, errors.New("missing data ownership identity")
	}
	for _, key := range []string{equipmentKey, physicalKey} {
		if key != "" {
			decoded, err := hex.DecodeString(key)
			if err != nil || len(decoded) != 32 {
				return false, errors.New("invalid data ownership identity")
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value_json,sensitive FROM app_settings WHERE key GLOB 'cellular.data.owner.private.v1.*' LIMIT 129`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 128 {
			return true, nil
		}
		var key, raw string
		var sensitive bool
		if err := rows.Scan(&key, &raw, &sensitive); err != nil {
			return false, err
		}
		var header struct {
			Version                   int
			EquipmentKey, PhysicalKey string
		}
		if !sensitive || len(raw) > MaxDataBearerClaimBytes || json.Unmarshal([]byte(raw), &header) != nil || (header.Version != 1 && header.Version != 2) || key != DataBearerClaimPrefix+header.EquipmentKey {
			return true, nil
		}
		for _, value := range []string{header.EquipmentKey, header.PhysicalKey} {
			decoded, err := hex.DecodeString(value)
			if err != nil || len(decoded) != 32 {
				return true, nil
			}
		}
		if equipmentKey != "" && header.EquipmentKey == equipmentKey || physicalKey != "" && header.PhysicalKey == physicalKey {
			return true, nil
		}
	}
	return false, rows.Err()
}

// DataBearerClaims exposes only the bounded private data journal to its
// recovery coordinator. It must never enumerate unrelated application secrets.
func (s *Store) DataBearerClaims(ctx context.Context) ([]AppSetting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value_json,sensitive FROM app_settings WHERE key GLOB 'cellular.data.owner.private.v1.*' LIMIT 129`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AppSetting
	for rows.Next() {
		var setting AppSetting
		var raw string
		if err := rows.Scan(&setting.Key, &raw, &setting.Sensitive); err != nil {
			return nil, err
		}
		if len(result) >= 128 || !setting.Sensitive || len(raw) > MaxDataBearerClaimBytes {
			return nil, errors.New("unrecognised data ownership journal")
		}
		setting.Value = json.RawMessage(raw)
		result = append(result, setting)
	}
	return result, rows.Err()
}
