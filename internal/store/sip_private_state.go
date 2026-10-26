package store

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// These are private, whole-record journals. They are not public HTTP limits or
// SMS segment sizes. Consumers must retain the exact raw bytes they read.
const (
	MaxSMSLedgerBytes   = 4 << 20
	MaxSIPSettingsBytes = 1 << 20
)

// CompareSMSLedger atomically changes only the private SIP SMS journal. nil
// before inserts if absent; nil after deletes only the matching private record.
// A true result is returned only after a FULL-synchronous disk commit. Any
// error, especially a commit error, is not authorization to send or retry SMS.
func (s *Store) CompareSMSLedger(ctx context.Context, before, after json.RawMessage) (bool, error) {
	return s.compareSIPPrivateState(ctx, "sip.gateway.sms-ledger.private.v1", before, after, MaxSMSLedgerBytes)
}

// CompareSIPSettings has the same durable CAS semantics, but is restricted to
// the separate private SIP configuration record. It never normalizes either
// snapshot: canonicalizing a legacy before image would invalidate the fence.
func (s *Store) CompareSIPSettings(ctx context.Context, before, after json.RawMessage) (bool, error) {
	return s.compareSIPPrivateState(ctx, "sip.gateway.private.v1", before, after, MaxSIPSettingsBytes)
}

func (s *Store) compareSIPPrivateState(ctx context.Context, key string, before, after json.RawMessage, maximum int) (bool, error) {
	if before == nil && after == nil {
		return false, errors.New("SIP private CAS requires a before or after image")
	}
	for _, value := range []json.RawMessage{before, after} {
		if value != nil && (len(value) == 0 || len(value) > maximum || !utf8.Valid(value) || !json.Valid(value)) {
			return false, errors.New("invalid SIP private CAS data")
		}
	}
	// Capture the caller's current images before waiting for a pooled connection.
	before, after = bytes.Clone(before), bytes.Clone(after)
	if s == nil || s.db == nil {
		return false, errors.New("SIP private CAS store is unavailable")
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire SIP private CAS connection: %w", err)
	}
	defer connection.Close()
	// All queries and the transaction use this connection. s.db permits only
	// one open connection, so calling back into s.db here would deadlock.
	if err := prepareSIPPrivateConnection(ctx, connection); err != nil {
		return false, err
	}
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin SIP private CAS: %w", err)
	}
	defer tx.Rollback()
	var result sql.Result
	switch {
	case before == nil:
		result, err = tx.ExecContext(ctx, `INSERT INTO app_settings(key,value_json,sensitive,updated_at)
			VALUES(?,?,1,?) ON CONFLICT(key) DO NOTHING`, key, string(after), time.Now().UTC().Unix())
	case after == nil:
		result, err = tx.ExecContext(ctx, `DELETE FROM app_settings
			WHERE key=? AND CAST(value_json AS BLOB)=? AND sensitive=1`, key, []byte(before))
	default:
		result, err = tx.ExecContext(ctx, `UPDATE app_settings SET value_json=?,updated_at=?
			WHERE key=? AND CAST(value_json AS BLOB)=? AND sensitive=1`, string(after), time.Now().UTC().Unix(), key, []byte(before))
	}
	if err != nil {
		return false, fmt.Errorf("write SIP private CAS: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected < 0 || affected > 1 {
		return false, errors.New("invalid SIP private CAS write result")
	}
	if err := tx.Commit(); err != nil {
		// database/sql marks a Tx done even if SQLite rejects COMMIT (for
		// example, a deferred constraint). The driver's failed COMMIT can
		// leave SQLite in a transaction; tx.Rollback alone is then ErrTxDone.
		// Do not return that transaction to the one-connection pool.
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, cleanupErr := connection.ExecContext(cleanup, "ROLLBACK"); cleanupErr != nil {
			_ = connection.Raw(func(any) error { return driver.ErrBadConn })
		}
		return false, fmt.Errorf("commit SIP private CAS; result unknown: %w", err)
	}
	return affected == 1, nil
}

func prepareSIPPrivateConnection(ctx context.Context, connection *sql.Conn) error {
	// SQLite reports an empty filename for memory/anonymous temporary databases;
	// inspecting the actual connection also catches URI mode=memory without
	// adding a second, potentially stale path flag to Store.
	rows, err := connection.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return fmt.Errorf("inspect SIP private database: %w", err)
	}
	fileBacked := false
	for rows.Next() {
		var sequence int
		var name string
		var filename sql.NullString
		if err := rows.Scan(&sequence, &name, &filename); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect SIP private database row: %w", err)
		}
		if name == "main" && filename.Valid && filename.String != "" {
			fileBacked = true
		}
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil || closeErr != nil {
		return errors.New("cannot inspect SIP private database backing")
	}
	if !fileBacked {
		return errors.New("durable SIP private CAS requires a file-backed main database")
	}
	var journal string
	if err := connection.QueryRowContext(ctx, "PRAGMA main.journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("inspect SIP private journal: %w", err)
	}
	switch strings.ToLower(journal) {
	case "wal", "delete", "truncate", "persist":
	default:
		return errors.New("durable SIP private CAS requires a persistent journal")
	}
	if _, err := connection.ExecContext(ctx, "PRAGMA main.synchronous=FULL"); err != nil {
		return fmt.Errorf("configure SIP private durability: %w", err)
	}
	var synchronous int
	if err := connection.QueryRowContext(ctx, "PRAGMA main.synchronous").Scan(&synchronous); err != nil {
		return fmt.Errorf("verify SIP private durability: %w", err)
	}
	if synchronous < 2 {
		return errors.New("SIP private CAS FULL durability was not applied")
	}
	return nil
}
