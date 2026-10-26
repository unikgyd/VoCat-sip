package store

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
)

// HasHostIMSClaim reads only key existence, never the private WAL payload.
// Malformed values/sensitivity flags still count as an unresolved claim.
func (s *Store) HasHostIMSClaim(ctx context.Context, key string) (bool, error) {
	suffix, ok := strings.CutPrefix(key, "host.ims.claim.private.v1.")
	decoded, err := hex.DecodeString(suffix)
	if !ok || err != nil || len(decoded) != 32 {
		return false, errors.New("invalid private claim identity")
	}
	var exists bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app_settings WHERE key=? LIMIT 1)`, key).Scan(&exists)
	return exists, err
}

// HasAnyHostIMSClaim is an exceptional unbound-equipment fence. Normal lines
// use HasHostIMSClaim so one modem's pending claim does not block another.
func (s *Store) HasAnyHostIMSClaim(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app_settings WHERE key GLOB 'host.ims.claim.private.v1.*' LIMIT 1)`).Scan(&exists)
	return exists, err
}
