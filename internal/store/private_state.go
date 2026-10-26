package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// MaxHostIMSClaimBytes bounds only the private, compare-and-swap resource WAL.
// It is unrelated to QMI frame sizes and to public settings/API payloads.
const MaxHostIMSClaimBytes = 32768

// CompareHostIMSClaim is a durable cross-process fence, not an ordinary
// settings upsert. nil before means insert-if-absent; nil after means delete
// the exact previous record. There is deliberately no expiry/force replacement.
func (s *Store) CompareHostIMSClaim(ctx context.Context, key string, before, after json.RawMessage) (bool, error) {
	suffix, ok := strings.CutPrefix(key, "host.ims.claim.private.v1.")
	decoded, err := hex.DecodeString(suffix)
	if !ok || err != nil || len(decoded) != 32 || before == nil && after == nil {
		return false, errors.New("invalid Host IMS claim key")
	}
	for _, value := range []json.RawMessage{before, after} {
		if value != nil && (len(value) == 0 || len(value) > MaxHostIMSClaimBytes || !json.Valid(value)) {
			return false, errors.New("invalid Host IMS claim data")
		}
	}
	var affected int64
	switch {
	case before == nil:
		result, err := s.db.ExecContext(ctx, `INSERT INTO app_settings (key,value_json,sensitive,updated_at)
			VALUES (?,?,1,?) ON CONFLICT(key) DO NOTHING`, key, string(after), time.Now().UTC().Unix())
		if err != nil {
			return false, err
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return false, err
		}
	case after == nil:
		result, err := s.db.ExecContext(ctx, `DELETE FROM app_settings WHERE key=? AND value_json=? AND sensitive=1`, key, string(before))
		if err != nil {
			return false, err
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return false, err
		}
	default:
		result, err := s.db.ExecContext(ctx, `UPDATE app_settings SET value_json=?,updated_at=?
			WHERE key=? AND value_json=? AND sensitive=1`, string(after), time.Now().UTC().Unix(), key, string(before))
		if err != nil {
			return false, err
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return false, err
		}
	}
	return affected == 1, nil
}
