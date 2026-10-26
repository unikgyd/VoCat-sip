package store

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// SMSForwardWireSnapshot contains exact private records from one read snapshot.
// Presence is independent of Value: an existing empty or malformed document is
// not absent history. InboxMatched is false when no Inbox guard was requested.
type SMSForwardWireSnapshot struct {
	Settings      AppSetting
	Ledger        AppSetting
	SettingsFound bool
	LedgerFound   bool
	InboxMatched  bool
}

// ReadSMSForwardWireSnapshot never admits or repairs work. The caller must
// validate the original recipient, attempt token, Unknown state and expiry in
// the returned ledger. A nil guard only skips the Inbox lookup; it does not
// prove that a message is explicitly source-independent.
func (s *Store) ReadSMSForwardWireSnapshot(ctx context.Context, guard *SMSForwardInboxGuard) (snapshot SMSForwardWireSnapshot, err error) {
	if err := ctx.Err(); err != nil {
		return SMSForwardWireSnapshot{}, err
	}
	if s == nil || s.db == nil {
		return SMSForwardWireSnapshot{}, errors.New("SMS forwarding wire Store unavailable")
	}
	var source SMSForwardInboxGuard
	if guard != nil {
		source = *guard
		if source.ID <= 0 || !validSMSForwardHash(source.Fingerprint) {
			return SMSForwardWireSnapshot{}, errors.New("invalid SMS forwarding wire Inbox guard")
		}
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return SMSForwardWireSnapshot{}, fmt.Errorf("acquire SMS forwarding wire read connection: %w", err)
	}
	begun := false
	defer func() {
		// A cancelled caller still needs to end its read transaction before this
		// one-connection pool can safely serve the next receive or guard.
		var cleanupErr error
		if begun {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, cleanupErr = connection.ExecContext(cleanup, "ROLLBACK")
			cancel()
			if cleanupErr != nil {
				_ = connection.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		if closeErr := connection.Close(); cleanupErr == nil && closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
			cleanupErr = closeErr
		}
		if err == nil && cleanupErr != nil {
			err = fmt.Errorf("finish SMS forwarding wire read: %w", cleanupErr)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			snapshot = SMSForwardWireSnapshot{}
		}
	}()
	// Do not use the write admission helper here: it changes PRAGMAs and takes
	// a writer reservation. The first SELECT below anchors one DEFERRED snapshot.
	if _, err = connection.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		// A failed BEGIN must not return an ambiguously active connection to the
		// pool. Discarding it also handles a previously poisoned connection.
		_ = connection.Raw(func(any) error { return driver.ErrBadConn })
		return snapshot, fmt.Errorf("begin SMS forwarding wire read: %w", err)
	}
	begun = true
	if snapshot.Settings, snapshot.SettingsFound, err = smsForwardWireAppSetting(ctx, connection, smsForwardSettingsKey); err != nil {
		return snapshot, err
	}
	if snapshot.Ledger, snapshot.LedgerFound, err = smsForwardWireAppSetting(ctx, connection, smsForwardLedgerKey); err != nil {
		return snapshot, err
	}
	if guard != nil {
		if snapshot.InboxMatched, err = smsForwardSourceMatches(ctx, connection, source); err != nil {
			return snapshot, err
		}
	}
	if err = ctx.Err(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func smsForwardWireAppSetting(ctx context.Context, connection *sql.Conn, key string) (AppSetting, bool, error) {
	value, err := appSetting(connection.QueryRowContext(ctx, appSettingSelect+` WHERE key=?`, key))
	if errors.Is(err, ErrNotFound) {
		return AppSetting{Key: key}, false, nil
	}
	if err != nil {
		return AppSetting{}, false, err
	}
	value.Value = bytes.Clone(value.Value)
	return value, true, nil
}
