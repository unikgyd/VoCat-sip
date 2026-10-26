package store

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type smsForwardWireFixture struct {
	database *Store
	other    *Store
	path     string
	message  SMSMessage
	guard    SMSForwardInboxGuard
	settings json.RawMessage
	ledger   json.RawMessage
}

func smsForwardWireTestFixture(t *testing.T) smsForwardWireFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wire-snapshot.db")
	database := smsForwardTestStore(t, path)
	message, err := database.SaveSMSMessage(context.Background(), smsForwardTestMessage("wire-original-source"))
	if err != nil {
		t.Fatal(err)
	}
	settings := json.RawMessage(" \n{\"settings_epoch\":\"original\"}\t")
	ledger := json.RawMessage("\t{\"version\":1,\"cursor\":0,\"cursor_initialized\":true,\"attempts\":{},\"inbox\":{}} \n")
	for _, value := range []AppSetting{{Key: smsForwardSettingsKey, Value: settings, Sensitive: true},
		{Key: smsForwardLedgerKey, Value: ledger, Sensitive: true}} {
		if err := database.UpsertAppSetting(context.Background(), value); err != nil {
			t.Fatal(err)
		}
	}
	return smsForwardWireFixture{database: database, other: openTestStore(t, path), path: path, message: message,
		guard: SMSForwardInboxGuard{ID: message.ID, Fingerprint: SMSForwardFingerprint(message)}, settings: settings, ledger: ledger}
}

func smsForwardWireRequireSnapshot(t *testing.T, value SMSForwardWireSnapshot, settings, ledger []byte, matched bool) {
	t.Helper()
	if !value.SettingsFound || !value.LedgerFound || value.Settings.Key != smsForwardSettingsKey ||
		value.Ledger.Key != smsForwardLedgerKey || !value.Settings.Sensitive || !value.Ledger.Sensitive ||
		!bytes.Equal(value.Settings.Value, settings) || !bytes.Equal(value.Ledger.Value, ledger) || value.InboxMatched != matched {
		t.Fatal("wire snapshot lost exact private documents, presence or actual Inbox match")
	}
}

func smsForwardWireRequireZero(t *testing.T, value SMSForwardWireSnapshot, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(value, SMSForwardWireSnapshot{}) {
		t.Fatalf("failed read exposed a partial authorization snapshot: error=%v", err)
	}
}

func TestSMSForwardWireMissingAndPresentEmptyDocumentsRemainDistinct(t *testing.T) {
	ctx := context.Background()
	database := smsForwardTestStore(t, filepath.Join(t.TempDir(), "wire-presence.db"))
	missing, err := database.ReadSMSForwardWireSnapshot(ctx, nil)
	if err != nil || missing.SettingsFound || missing.LedgerFound || missing.InboxMatched ||
		missing.Settings.Key != smsForwardSettingsKey || missing.Ledger.Key != smsForwardLedgerKey {
		t.Fatalf("absent documents or nil guard acquired authority: error=%v", err)
	}
	for _, key := range []string{smsForwardSettingsKey, smsForwardLedgerKey} {
		if _, err := database.db.ExecContext(ctx, "INSERT INTO app_settings(key,value_json,sensitive,updated_at) VALUES(?,?,0,17)", key, ""); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := database.ReadSMSForwardWireSnapshot(ctx, nil)
	if err != nil || !empty.SettingsFound || !empty.LedgerFound || len(empty.Settings.Value) != 0 || len(empty.Ledger.Value) != 0 ||
		empty.Settings.Sensitive || empty.Ledger.Sensitive || empty.InboxMatched || empty.Settings.UpdatedAt.Unix() != 17 || empty.Ledger.UpdatedAt.Unix() != 17 {
		t.Fatalf("present empty/nonprivate documents were normalized into absence: error=%v", err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE app_settings SET value_json='null',sensitive=1 WHERE key=?", smsForwardSettingsKey); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE app_settings SET value_json='not-json' WHERE key=?", smsForwardLedgerKey); err != nil {
		t.Fatal(err)
	}
	opaque, err := database.ReadSMSForwardWireSnapshot(ctx, nil)
	if err != nil || !opaque.SettingsFound || !opaque.LedgerFound || string(opaque.Settings.Value) != "null" ||
		string(opaque.Ledger.Value) != "not-json" || !opaque.Settings.Sensitive || opaque.Ledger.Sensitive || opaque.InboxMatched {
		t.Fatalf("wire read parsed, repaired or dropped opaque stored documents: error=%v", err)
	}
}

func TestSMSForwardWireActualSourceMutationAndReadAcknowledgement(t *testing.T) {
	for _, mutation := range []string{"delete", "body", "sender", "direction", "origin", "read"} {
		t.Run(mutation, func(t *testing.T) {
			f := smsForwardWireTestFixture(t)
			before, err := f.database.ReadSMSForwardWireSnapshot(context.Background(), &f.guard)
			if err != nil {
				t.Fatal(err)
			}
			smsForwardWireRequireSnapshot(t, before, f.settings, f.ledger, true)
			var query string
			switch mutation {
			case "delete":
				query = "DELETE FROM sms_messages WHERE id=?"
			case "body":
				query = "UPDATE sms_messages SET body='changed actual body' WHERE id=?"
			case "sender":
				query = "UPDATE sms_messages SET peer='+15550009999' WHERE id=?"
			case "direction":
				query = "UPDATE sms_messages SET direction='outbound' WHERE id=?"
			case "origin":
				query = "UPDATE sms_messages SET extra_json=json_set(extra_json,'$._sip_forward_origin_v1.peer','FORGED') WHERE id=?"
			default:
				query = "UPDATE sms_messages SET is_read=1,updated_at=updated_at+1 WHERE id=?"
			}
			if _, err := f.other.db.ExecContext(context.Background(), query, f.message.ID); err != nil {
				t.Fatal(err)
			}
			after, err := f.database.ReadSMSForwardWireSnapshot(context.Background(), &f.guard)
			if err != nil {
				t.Fatal(err)
			}
			smsForwardWireRequireSnapshot(t, after, f.settings, f.ledger, mutation == "read")
			if smsForwardTestCount(t, f.database, "sms_forward_receipts") != 1 {
				t.Fatal("wire source read modified the independent receipt history")
			}
		})
	}
}

func TestSMSForwardWireReadDoesNotMigratePruneRenewOrWrite(t *testing.T) {
	f := smsForwardWireTestFixture(t)
	ctx := context.Background()
	if _, err := f.database.db.ExecContext(ctx, "UPDATE sms_forward_receipts SET expires_at=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.db.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET guard_until=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"app_settings", "sms_messages", "sms_forward_receipts", "sms_forward_receipt_state"} {
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			query := "CREATE TRIGGER wire_read_deny_" + table + "_" + operation + " BEFORE " + operation + " ON " + table +
				" BEGIN SELECT RAISE(ABORT,'wire snapshot attempted a write'); END"
			if _, err := f.database.db.ExecContext(ctx, query); err != nil {
				t.Fatal(err)
			}
		}
	}
	trace := &smsForwardWireTrace{}
	smsForwardWireTrackStore(t, f.database, f.path, trace)
	for attempt := 0; attempt < 2; attempt++ {
		value, err := f.database.ReadSMSForwardWireSnapshot(ctx, &f.guard)
		if err != nil {
			t.Fatal(err)
		}
		smsForwardWireRequireSnapshot(t, value, f.settings, f.ledger, true)
		// Mutating the caller's copy must not modify a later snapshot or disk.
		value.Settings.Value[0] = 'X'
		value.Ledger.Value[0] = 'Y'
	}
	var expires, initialized, guardUntil int64
	if err := f.other.db.QueryRowContext(ctx, "SELECT expires_at FROM sms_forward_receipts").Scan(&expires); err != nil || expires != 1 {
		t.Fatalf("wire read expired or renewed source history: expiry=%d error=%v", expires, err)
	}
	if err := f.other.db.QueryRowContext(ctx, "SELECT initialized,guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&initialized, &guardUntil); err != nil || initialized != 1 || guardUntil != 1 {
		t.Fatalf("wire read repaired or renewed source metadata: initialized=%d guard=%d error=%v", initialized, guardUntil, err)
	}
	for _, key := range []string{smsForwardSettingsKey, smsForwardLedgerKey} {
		value, err := f.other.AppSetting(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		expected := f.settings
		if key == smsForwardLedgerKey {
			expected = f.ledger
		}
		if !bytes.Equal(value.Value, expected) {
			t.Fatal("wire read migrated legacy JSON or mutated its exact raw record")
		}
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if len(trace.executed) != 4 {
		t.Fatalf("unexpected wire read transaction operations: %v", trace.executed)
	}
	for index, query := range trace.executed {
		expected := "BEGIN DEFERRED"
		if index%2 == 1 {
			expected = "ROLLBACK"
		}
		if query != expected {
			t.Fatalf("wire API performed a write, DDL or PRAGMA: %q", query)
		}
	}
}

func TestSMSForwardWireConcurrentWriterCannotSpliceSettingsLedgerAndInboxSnapshots(t *testing.T) {
	f := smsForwardWireTestFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	allow := func() { releaseOnce.Do(func() { close(release) }) }
	defer allow()
	trace := &smsForwardWireTrace{afterSettings: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	smsForwardWireTrackStore(t, f.database, f.path, trace)
	type result struct {
		value SMSForwardWireSnapshot
		err   error
	}
	read := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		value, err := f.database.ReadSMSForwardWireSnapshot(ctx, &f.guard)
		read <- result{value, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("wire read did not reach the actual first SQLite result:", ctx.Err())
	}
	nextSettings, nextLedger := `{"settings_epoch":"replacement"}`, `{"version":3,"replacement":"new attempt"}`
	write, err := f.other.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Rollback()
	for _, update := range []struct {
		query string
		args  []any
	}{
		{"UPDATE app_settings SET value_json=? WHERE key=?", []any{nextSettings, smsForwardSettingsKey}},
		{"UPDATE app_settings SET value_json=? WHERE key=?", []any{nextLedger, smsForwardLedgerKey}},
		{"UPDATE sms_messages SET body=? WHERE id=?", []any{"replacement actual Inbox body", f.message.ID}},
	} {
		if _, err := write.ExecContext(ctx, update.query, update.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := write.Commit(); err != nil {
		t.Fatal("independent WAL writer could not commit during the read snapshot:", err)
	}
	allow()
	select {
	case got := <-read:
		if got.err != nil {
			t.Fatal(got.err)
		}
		smsForwardWireRequireSnapshot(t, got.value, f.settings, f.ledger, true)
	case <-ctx.Done():
		t.Fatal("wire read did not finish after releasing the first result:", ctx.Err())
	}
	fresh, err := f.database.ReadSMSForwardWireSnapshot(context.Background(), &f.guard)
	if err != nil {
		t.Fatal(err)
	}
	smsForwardWireRequireSnapshot(t, fresh, []byte(nextSettings), []byte(nextLedger), false)
	replacement, err := f.other.SMSMessage(context.Background(), f.message.ID)
	if err != nil {
		t.Fatal(err)
	}
	guard := SMSForwardInboxGuard{ID: replacement.ID, Fingerprint: SMSForwardFingerprint(replacement)}
	fresh, err = f.database.ReadSMSForwardWireSnapshot(context.Background(), &guard)
	if err != nil {
		t.Fatal(err)
	}
	smsForwardWireRequireSnapshot(t, fresh, []byte(nextSettings), []byte(nextLedger), true)
}

func TestSMSForwardWireErrorsAndCancellationReturnZeroAndReleaseTheReadConnection(t *testing.T) {
	for _, failure := range []string{"cancel-before", "cancel-between", "ledger-query", "begin-uncertain", "rollback-uncertain", "missing-inbox-table"} {
		t.Run(failure, func(t *testing.T) {
			f := smsForwardWireTestFixture(t)
			trace := &smsForwardWireTrace{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "cancel-before":
				cancel()
			case "cancel-between":
				trace.afterSettings = func(context.Context) error { cancel(); return ctx.Err() }
			case "ledger-query":
				trace.failLedger.Store(true)
			case "begin-uncertain":
				trace.failBegin.Store(true)
			case "rollback-uncertain":
				trace.failRollback.Store(true)
			case "missing-inbox-table":
				if _, err := f.other.db.ExecContext(ctx, "ALTER TABLE sms_messages RENAME TO wire_temporarily_missing_inbox"); err != nil {
					t.Fatal(err)
				}
			}
			smsForwardWireTrackStore(t, f.database, f.path, trace)
			value, err := f.database.ReadSMSForwardWireSnapshot(ctx, &f.guard)
			smsForwardWireRequireZero(t, value, err)
			if strings.HasPrefix(failure, "cancel-") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled wire read lost cancellation identity: %v", err)
			}
			if trace.cancelledRollback.Load() {
				t.Fatal("read cleanup used the already-cancelled caller context")
			}
			if failure == "missing-inbox-table" {
				if _, err := f.other.db.ExecContext(context.Background(), "ALTER TABLE wire_temporarily_missing_inbox RENAME TO sms_messages"); err != nil {
					t.Fatal(err)
				}
			}
			fresh, err := f.database.ReadSMSForwardWireSnapshot(context.Background(), &f.guard)
			if err != nil {
				t.Fatal("failed wire read poisoned its next real SQLite read:", err)
			}
			smsForwardWireRequireSnapshot(t, fresh, f.settings, f.ledger, true)
			if strings.HasSuffix(failure, "-uncertain") && (trace.connections.Load() < 2 || trace.closed.Load() < 1) {
				t.Fatal("uncertain transaction connection was reused instead of discarded")
			}
		})
	}
}

func TestSMSForwardWireInvalidGuardsAndUnavailableStoresNeverExposePartialSnapshots(t *testing.T) {
	f := smsForwardWireTestFixture(t)
	for _, guard := range []SMSForwardInboxGuard{{ID: 0, Fingerprint: f.guard.Fingerprint},
		{ID: f.guard.ID, Fingerprint: "not-a-fingerprint"}, {ID: f.guard.ID, Fingerprint: strings.Repeat("A", 64)}} {
		value, err := f.database.ReadSMSForwardWireSnapshot(context.Background(), &guard)
		smsForwardWireRequireZero(t, value, err)
	}
	var unavailable *Store
	value, err := unavailable.ReadSMSForwardWireSnapshot(context.Background(), &f.guard)
	smsForwardWireRequireZero(t, value, err)
	value, err = (&Store{}).ReadSMSForwardWireSnapshot(context.Background(), nil)
	smsForwardWireRequireZero(t, value, err)
	value, err = f.database.ReadSMSForwardWireSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	smsForwardWireRequireSnapshot(t, value, f.settings, f.ledger, false)
}

// This observer wraps the actual registered SQLite driver. It does not invent
// rows or matching sources: it only pauses after a real Settings result has
// anchored the read snapshot, or injects an I/O boundary failure once.
type smsForwardWireTrace struct {
	mu                sync.Mutex
	executed          []string
	afterSettings     func(context.Context) error
	settingsPaused    atomic.Bool
	failLedger        atomic.Bool
	failBegin         atomic.Bool
	failRollback      atomic.Bool
	cancelledRollback atomic.Bool
	connections       atomic.Int32
	closed            atomic.Int32
}

func smsForwardWireTrackStore(t *testing.T, database *Store, path string, trace *smsForwardWireTrace) {
	t.Helper()
	previous := database.db
	base := storeConnector{base: storeDSNConnector{path: path, driver: previous.Driver()}}
	database.db = sql.OpenDB(smsForwardWireConnector{base: base, trace: trace})
	database.db.SetMaxOpenConns(1)
	database.db.SetMaxIdleConns(1)
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
}

type smsForwardWireConnector struct {
	base  driver.Connector
	trace *smsForwardWireTrace
}

func (c smsForwardWireConnector) Driver() driver.Driver { return c.base.Driver() }

func (c smsForwardWireConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c.trace.connections.Add(1)
	return &smsForwardWireConnection{Conn: connection, trace: c.trace}, nil
}

type smsForwardWireConnection struct {
	driver.Conn
	trace *smsForwardWireTrace
}

func (c *smsForwardWireConnection) Close() error {
	c.trace.closed.Add(1)
	return c.Conn.Close()
}

func (c *smsForwardWireConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.trace.mu.Lock()
	c.trace.executed = append(c.trace.executed, query)
	c.trace.mu.Unlock()
	if query == "ROLLBACK" {
		if ctx.Err() != nil {
			c.trace.cancelledRollback.Store(true)
		}
		if c.trace.failRollback.Swap(false) {
			return nil, errors.New("synthetic uncertain wire read rollback")
		}
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && query == "BEGIN DEFERRED" && c.trace.failBegin.Swap(false) {
		return nil, errors.New("synthetic error after SQLite began the read transaction")
	}
	return result, err
}

func (c *smsForwardWireConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	privateSetting := strings.Contains(query, "FROM app_settings") && len(args) == 1
	if privateSetting && args[0].Value == smsForwardLedgerKey && c.trace.failLedger.Swap(false) {
		return nil, errors.New("synthetic ledger read failure")
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if privateSetting && args[0].Value == smsForwardSettingsKey && c.trace.afterSettings != nil && !c.trace.settingsPaused.Swap(true) {
		return &smsForwardWireRows{Rows: rows, ctx: ctx, afterClose: c.trace.afterSettings}, nil
	}
	return rows, nil
}

type smsForwardWireRows struct {
	driver.Rows
	ctx        context.Context
	afterClose func(context.Context) error
	once       sync.Once
	err        error
}

func (r *smsForwardWireRows) Close() error {
	r.once.Do(func() {
		r.err = r.Rows.Close()
		if r.err == nil {
			r.err = r.afterClose(r.ctx)
		}
	})
	return r.err
}
