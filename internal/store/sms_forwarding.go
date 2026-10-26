package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	smsForwardLedgerKey       = "sip.gateway.sms-ledger.private.v1"
	smsForwardSettingsKey     = "sip.gateway.private.v1"
	smsForwardReceiptLimit    = 32768
	smsForwardReceiptLifetime = 24 * time.Hour
	smsForwardMaximumUnix     = int64(253402300799)
	smsForwardOriginKey       = "_sip_forward_origin_v1"
	// Keep this deterministic structural predicate identical in the partial
	// index and the validation query. It deliberately contains no current time:
	// expiry is a later operation, never a way to hide malformed durable rows.
	smsForwardReceiptInvalidExpression = `CASE WHEN
		typeof(source_key)<>'text' OR length(source_key)<>64 OR source_key GLOB '*[^0-9a-f]*'
		OR typeof(fingerprint)<>'text' OR length(fingerprint)<>64 OR fingerprint GLOB '*[^0-9a-f]*'
		OR typeof(expires_at)<>'integer' OR expires_at<1 OR expires_at>253402300799
		THEN 1 ELSE 0 END`
	smsForwardReceiptInvalidSelect = `SELECT EXISTS(SELECT 1 FROM sms_forward_receipts WHERE (` + smsForwardReceiptInvalidExpression + `)=1 LIMIT 1)`
)

// ErrSMSForwardCapacity declines this message's forwarding plan without losing
// the local Inbox or its empty source receipt. Other planner errors roll back
// the entire Inbox/receipt/plan transaction.
var ErrSMSForwardCapacity = errors.New("SMS forwarding capacity reached")

// ErrSMSForwardSourceChanged is not a CAS conflict. The actual Inbox row was
// deleted, is no longer deliverable, or no longer matches the original payload.
var ErrSMSForwardSourceChanged = errors.New("SMS forwarding source changed")

// SMSForwardingInput is a single write-transaction snapshot. A planner is pure:
// it may inspect these copies, but must not call Store or perform network I/O.
// OriginalPeer preserves the received sender before Inbox whitespace cleanup.
type SMSForwardingInput struct {
	Message       SMSMessage
	Settings      AppSetting
	Ledger        AppSetting
	Now           time.Time
	SourceKey     string
	Fingerprint   string
	OriginalPeer  string
	OriginalICCID string
}

// SMSForwardPlanner returns the next complete private ledger, or nil to retain
// an empty source plan. It never commits or sends anything itself.
type SMSForwardPlanner func(SMSForwardingInput) (json.RawMessage, error)

func (s *Store) SetSMSForwardPlanner(planner SMSForwardPlanner) {
	s.smsForwardMu.Lock()
	s.smsForwardPlanner = planner
	s.smsForwardMu.Unlock()
}

func (s *Store) smsPlanner() SMSForwardPlanner {
	s.smsForwardMu.RLock()
	planner := s.smsForwardPlanner
	s.smsForwardMu.RUnlock()
	return planner
}

func smsForwardInbound(value SMSMessage) bool {
	return value.Direction == "inbound" || value.Direction == "received"
}

// SMSForwardMessageReady is the shared Inbox readiness boundary, not an SMS
// transport acceptance or a claim that the source has a known SIM identity.
func SMSForwardMessageReady(value SMSMessage) bool {
	if !smsForwardInbound(value) || strings.TrimSpace(value.Body) == "" || SMSDecodeError(value.Extra) != "" || !ConcatSMSReadyToNotify(value.MessageID, value.Extra) {
		return false
	}
	if len(value.Extra) != 0 {
		if _, err := decodeJSONObject(value.Extra); err != nil {
			return false
		}
	}
	return true
}

func smsForwardHash(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SMSForwardSourceKey does not infer identity from the current modem/card. A
// missing stable MessageID has only row-ID scope; it cannot prove a later
// deletion/reinsert is the same network delivery.
func SMSForwardSourceKey(value SMSMessage) string {
	kind, source := "stable", value.MessageID
	if source == "" {
		kind, source = "row", strconv.FormatInt(value.ID, 10)
	}
	return smsForwardHash("vocat-sms-forward-source-v1", smsHardwareKey(value.ModemIMEI, value.DeviceID), value.ICCID, kind, source)
}

// SMSForwardFingerprint binds the actual durable message, not mutable Read or
// UpdatedAt fields. A guard can therefore cancel old payload copies without
// treating a local read acknowledgement as a content change.
func SMSForwardFingerprint(value SMSMessage) string {
	return smsForwardHash("vocat-sms-forward-message-v1", value.DeviceID, value.ModemIMEI, value.IMSI, value.ICCID,
		value.MessageID, value.Peer, value.Body, value.Direction, strconv.FormatBool(SMSForwardMessageReady(value)), smsForwardOriginFingerprint(value.Extra))
}

type smsForwardOrigin struct {
	Version int    `json:"version"`
	Peer    string `json:"peer"`
	ICCID   string `json:"iccid"`
	Archive bool   `json:"archive"`
	// Transaction-local only: display/hardware enrichment of an existing row
	// must not turn the same received message into a new recipient source.
	previousSourceKey string
}

func readSMSForwardOrigin(extra json.RawMessage) (smsForwardOrigin, bool, error) {
	var empty smsForwardOrigin
	if len(extra) == 0 {
		return empty, false, nil
	}
	document, err := decodeJSONObject(extra)
	if err != nil {
		return empty, false, err
	}
	value, exists := document[smsForwardOriginKey]
	if !exists {
		return empty, false, nil
	}
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 4 {
		return empty, false, errors.New("invalid durable SMS original source")
	}
	version, ok := fields["version"].(json.Number)
	if !ok || version.String() != "1" {
		return empty, false, errors.New("invalid durable SMS original source version")
	}
	peer, peerOK := fields["peer"].(string)
	iccid, iccidOK := fields["iccid"].(string)
	archive, archiveOK := fields["archive"].(bool)
	if !peerOK || !iccidOK || !archiveOK {
		return empty, false, errors.New("invalid durable SMS original source fields")
	}
	return smsForwardOrigin{Version: 1, Peer: peer, ICCID: iccid, Archive: archive}, true, nil
}

func smsForwardOriginFingerprint(extra json.RawMessage) string {
	origin, exists, err := readSMSForwardOrigin(extra)
	if err != nil {
		return "invalid"
	}
	if !exists {
		return "legacy:missing"
	}
	data, _ := json.Marshal(origin)
	return string(data)
}

// Read an existing row before saveSMSMessage can replace a partial concat or
// overwrite its Extra. Caller-supplied Extra is never an origin authority.
func smsForwardOriginalSource(ctx context.Context, connection *sql.Conn, value SMSMessage, archive bool) (smsForwardOrigin, error) {
	fresh := smsForwardOrigin{Version: 1, Peer: value.Peer, ICCID: value.ICCID, Archive: archive}
	if value.Direction != "inbound" && value.Direction != "received" && strings.ToLower(strings.TrimSpace(value.Direction)) != "inbound" && strings.ToLower(strings.TrimSpace(value.Direction)) != "received" {
		return fresh, nil
	}
	var existing SMSMessage
	var err error
	switch {
	case value.ID > 0:
		existing, err = scanSMSMessage(connection.QueryRowContext(ctx, smsMessageSelect+` WHERE id=?`, value.ID))
	case value.MessageID != "":
		existing, err = scanSMSMessage(connection.QueryRowContext(ctx, smsMessageSelect+` WHERE COALESCE(NULLIF(modem_imei,''),'device:' || device_id)=? AND message_id=?`, smsHardwareKey(value.ModemIMEI, value.DeviceID), value.MessageID))
		if errors.Is(err, ErrNotFound) {
			existing, err = scanSMSMessage(connection.QueryRowContext(ctx, smsMessageSelect+` WHERE device_id=? AND message_id=?`, strings.TrimSpace(value.DeviceID), value.MessageID))
		}
	default:
		return fresh, nil
	}
	if errors.Is(err, ErrNotFound) {
		return fresh, nil
	}
	if err != nil {
		return smsForwardOrigin{}, err
	}
	origin, exists, err := readSMSForwardOrigin(existing.Extra)
	if err != nil {
		return smsForwardOrigin{}, err
	}
	if !exists {
		// An old row is not a new network event. Persist this empty origin so a
		// later identity fill-in, rescan, or concat completion cannot upgrade it.
		return smsForwardOrigin{Version: 1, Archive: true}, nil
	}
	origin.previousSourceKey = SMSForwardSourceKey(existing)
	return origin, nil
}

func persistSMSForwardOrigin(ctx context.Context, connection *sql.Conn, message *SMSMessage, origin smsForwardOrigin) error {
	if !smsForwardInbound(*message) {
		return nil
	}
	document, err := decodeJSONObject(message.Extra)
	if err != nil {
		return err
	}
	document[smsForwardOriginKey] = origin
	extra, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if _, err := connection.ExecContext(ctx, `UPDATE sms_messages SET extra_json=? WHERE id=?`, string(extra), message.ID); err != nil {
		return err
	}
	message.Extra = extra
	return nil
}

type SMSForwardInboxGuard struct {
	ID          int64
	Fingerprint string
}

func validSMSForwardHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func smsForwardSourceMatches(ctx context.Context, executor contextQueryExecer, guard SMSForwardInboxGuard) (bool, error) {
	if guard.ID <= 0 || !validSMSForwardHash(guard.Fingerprint) {
		return false, errors.New("invalid SMS forwarding Inbox guard")
	}
	value, err := scanSMSMessage(executor.QueryRowContext(ctx, smsMessageSelect+` WHERE id=?`, guard.ID))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return SMSForwardMessageReady(value) && SMSForwardFingerprint(value) == guard.Fingerprint, nil
}

func (s *Store) SMSForwardSourceMatches(ctx context.Context, guard SMSForwardInboxGuard) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("SMS forwarding Store unavailable")
	}
	return smsForwardSourceMatches(ctx, s.db, guard)
}

// A manual BEGIN IMMEDIATE obtains the writer reservation before any read.
// database/sql's ordinary deferred transaction would otherwise allow two
// processes to select different original plans before one hit BUSY_SNAPSHOT.
type smsForwardWrite struct {
	connection *sql.Conn
	done       bool
}

func (s *Store) beginSMSForwardWrite(ctx context.Context) (*smsForwardWrite, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("SMS forwarding Store unavailable")
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*smsForwardWrite, error) { _ = connection.Close(); return nil, err }
	if _, err := connection.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return closeOnError(err)
	}
	var synchronous int
	if err := connection.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		return closeOnError(err)
	}
	if synchronous < 2 {
		return closeOnError(errors.New("SMS forwarding FULL synchronous setting unavailable"))
	}
	// Empty plans, source receipts, capacity guards and upgrade protection also
	// require a persistent journal on a file Store; not just nonempty payloads.
	if err := verifySMSForwardBacking(ctx, connection, false); err != nil {
		return closeOnError(err)
	}
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		// A cancelled/error return does not prove BEGIN was never executed.
		// Do not return an unconfirmed manual transaction to the pool.
		_ = connection.Raw(func(any) error { return driver.ErrBadConn })
		return closeOnError(err)
	}
	return &smsForwardWrite{connection: connection}, nil
}

func (tx *smsForwardWrite) close() {
	if !tx.done {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := tx.connection.ExecContext(cleanup, "ROLLBACK")
		cancel()
		if err != nil {
			_ = tx.connection.Raw(func(any) error { return driver.ErrBadConn })
		}
	}
	_ = tx.connection.Close()
}

func (tx *smsForwardWrite) commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := tx.connection.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("SMS forwarding COMMIT result unknown: %w", err)
	}
	tx.done = true
	return nil
}

// This variant only verifies: synchronous cannot be changed after BEGIN.
func verifySMSForwardDisk(ctx context.Context, connection *sql.Conn) error {
	return verifySMSForwardBacking(ctx, connection, true)
}

func verifySMSForwardBacking(ctx context.Context, connection *sql.Conn, requireFile bool) error {
	rows, err := connection.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return err
	}
	fileBacked := false
	for rows.Next() {
		var sequence int
		var name, filename string
		if err := rows.Scan(&sequence, &name, &filename); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "main" && filename != "" {
			fileBacked = true
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if !fileBacked && requireFile {
		return errors.New("durable SMS forwarding requires a file-backed main database")
	}
	var journal string
	if err := connection.QueryRowContext(ctx, "PRAGMA main.journal_mode").Scan(&journal); err != nil {
		return err
	}
	switch strings.ToLower(journal) {
	case "wal", "delete", "truncate", "persist":
	case "memory":
		if fileBacked || requireFile {
			return errors.New("durable SMS forwarding requires a persistent journal")
		}
	default:
		return errors.New("durable SMS forwarding requires a persistent journal")
	}
	var synchronous int
	if err := connection.QueryRowContext(ctx, "PRAGMA main.synchronous").Scan(&synchronous); err != nil {
		return err
	}
	if synchronous < 2 {
		return errors.New("durable SMS forwarding requires FULL synchronous")
	}
	return nil
}

type smsForwardReceiptState struct {
	initialized int
	guardUntil  int64
	count       int
}

// Validate all durable source metadata before expiring any record. Corruption
// must not be interpreted as an empty/new source registry.
func readSMSForwardReceiptState(ctx context.Context, connection *sql.Conn, now time.Time, initializing bool) (smsForwardReceiptState, error) {
	var state smsForwardReceiptState
	if now.Unix() < 1 || now.Unix() > smsForwardMaximumUnix-int64(smsForwardReceiptLifetime/time.Second) {
		return state, errors.New("invalid SMS forwarding receipt clock")
	}
	var metadataCount int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM sms_forward_receipt_state").Scan(&metadataCount); err != nil {
		return state, err
	}
	if metadataCount != 1 {
		return state, errors.New("invalid SMS forwarding source metadata count")
	}
	if err := connection.QueryRowContext(ctx, "SELECT initialized,guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&state.initialized, &state.guardUntil); err != nil {
		return state, err
	}
	if state.initialized < 0 || state.initialized > 1 || !initializing && state.initialized != 1 || state.guardUntil < 0 || state.guardUntil > smsForwardMaximumUnix {
		return state, errors.New("invalid SMS forwarding source metadata")
	}
	var invalid int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM sms_forward_receipts").Scan(&state.count); err != nil {
		return state, err
	}
	// SQLite maintains the partial invalid-row index on every INSERT/UPDATE,
	// including direct SQL writes outside this API. Missing indexes may fall
	// back to a full scan, but must never turn invalid rows into empty history.
	if err := connection.QueryRowContext(ctx, smsForwardReceiptInvalidSelect).Scan(&invalid); err != nil {
		return state, err
	}
	if invalid != 0 || state.count > smsForwardReceiptLimit {
		return state, errors.New("invalid SMS forwarding source receipt")
	}
	deleted, err := connection.ExecContext(ctx, "DELETE FROM sms_forward_receipts WHERE expires_at<=?", now.Unix())
	if err != nil {
		return state, err
	}
	count, err := deleted.RowsAffected()
	if err != nil || count < 0 || count > int64(state.count) {
		return state, errors.New("invalid SMS forwarding source expiry result")
	}
	state.count -= int(count)
	if state.guardUntil != 0 && state.guardUntil <= now.Unix() {
		if _, err := connection.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET guard_until=0 WHERE id=1"); err != nil {
			return state, err
		}
		state.guardUntil = 0
	}
	return state, nil
}

func reserveSMSForwardReceipt(ctx context.Context, connection *sql.Conn, state *smsForwardReceiptState, key, fingerprint string, now time.Time) (bool, error) {
	var exists int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM sms_forward_receipts WHERE source_key=?", key).Scan(&exists); err != nil {
		return false, err
	}
	if exists != 0 {
		return false, nil
	}
	if state.count >= smsForwardReceiptLimit {
		until := now.Add(smsForwardReceiptLifetime).Unix()
		if until > state.guardUntil {
			if _, err := connection.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET guard_until=? WHERE id=1", until); err != nil {
				return false, err
			}
			state.guardUntil = until
		}
		return false, nil
	}
	if _, err := connection.ExecContext(ctx, "INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at) VALUES(?,?,?)", key, fingerprint, now.Add(smsForwardReceiptLifetime).Unix()); err != nil {
		return false, err
	}
	state.count++
	return true, nil
}

func (s *Store) initializeSMSForwardReceipts(ctx context.Context) error {
	tx, err := s.beginSMSForwardWrite(ctx)
	if err != nil {
		return fmt.Errorf("initialize SMS forwarding receipts: %w", err)
	}
	defer tx.close()
	// Existing schema-29 databases may predate these performance indexes. The
	// writer reservation makes idempotent creation and all subsequent source
	// validation one atomic startup operation without changing stored receipts.
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS sms_forward_receipts_invalid_idx ON sms_forward_receipts(source_key) WHERE (` + smsForwardReceiptInvalidExpression + `)=1`,
		`CREATE INDEX IF NOT EXISTS sms_forward_receipts_expiry_idx ON sms_forward_receipts(expires_at)`,
	} {
		if _, err := tx.connection.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize SMS forwarding receipt index: %w", err)
		}
	}
	now := time.Now().UTC()
	state, err := readSMSForwardReceiptState(ctx, tx.connection, now, true)
	if err != nil {
		return err
	}
	if state.initialized == 0 {
		var after int64
		for {
			// Do not read old message bodies or guess a currently inserted SIM.
			rows, err := tx.connection.QueryContext(ctx, `SELECT id,message_id,device_id,modem_imei,iccid
				FROM sms_messages WHERE id>? AND direction IN ('inbound','received') ORDER BY id LIMIT 256`, after)
			if err != nil {
				return err
			}
			var values []SMSMessage
			for rows.Next() {
				var value SMSMessage
				if err := rows.Scan(&value.ID, &value.MessageID, &value.DeviceID, &value.ModemIMEI, &value.ICCID); err != nil {
					_ = rows.Close()
					return err
				}
				values = append(values, value)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if len(values) == 0 {
				break
			}
			for _, value := range values {
				key := SMSForwardSourceKey(value)
				if _, err := reserveSMSForwardReceipt(ctx, tx.connection, &state, key, smsForwardHash("vocat-legacy-empty-source-v1", key), now); err != nil {
					return err
				}
				after = value.ID
			}
			if state.guardUntil > now.Unix() {
				break
			}
		}
		if _, err := tx.connection.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET initialized=1 WHERE id=1"); err != nil {
			return err
		}
	}
	if err := tx.commit(ctx); err != nil {
		return err
	}
	return nil
}

func smsForwardAppSetting(ctx context.Context, connection *sql.Conn, key string) (AppSetting, error) {
	value, err := appSetting(connection.QueryRowContext(ctx, appSettingSelect+` WHERE key=?`, key))
	if errors.Is(err, ErrNotFound) {
		return AppSetting{Key: key}, nil
	}
	return value, err
}

func (s *Store) captureSMSForwardPlan(ctx context.Context, connection *sql.Conn, message SMSMessage, origin smsForwardOrigin, archive bool) error {
	if !smsForwardInbound(message) {
		return nil
	}
	if origin.previousSourceKey != "" && origin.previousSourceKey != SMSForwardSourceKey(message) {
		// A stable Inbox row can gain a previously unknown hardware identity,
		// or be explicitly edited to a different source key. That is not proof
		// of a new network delivery and cannot acquire today's recipients.
		archive = true
	}
	// A partial live concat is not a logical received message yet. Its first
	// complete merge captures a plan; archives/upgrade always retain emptiness.
	if !archive && !origin.Archive && isConcatSMSMessageID(message.MessageID) && !ConcatSMSReadyToNotify(message.MessageID, message.Extra) {
		return nil
	}
	now := time.Now().UTC()
	state, err := readSMSForwardReceiptState(ctx, connection, now, false)
	if err != nil {
		return err
	}
	key, fingerprint := SMSForwardSourceKey(message), SMSForwardFingerprint(message)
	reserved, err := reserveSMSForwardReceipt(ctx, connection, &state, key, fingerprint, now)
	if err != nil {
		return err
	}
	if !reserved || archive || origin.Archive || state.guardUntil > now.Unix() || !SMSForwardMessageReady(message) {
		return nil
	}
	planner := s.smsPlanner()
	if planner == nil {
		return nil
	}
	settings, err := smsForwardAppSetting(ctx, connection, smsForwardSettingsKey)
	if err != nil {
		return err
	}
	ledger, err := smsForwardAppSetting(ctx, connection, smsForwardLedgerKey)
	if err != nil {
		return err
	}
	data, err := planner(SMSForwardingInput{Message: message, Settings: settings, Ledger: ledger, Now: now, SourceKey: key, Fingerprint: fingerprint, OriginalPeer: origin.Peer, OriginalICCID: origin.ICCID})
	if errors.Is(err, ErrSMSForwardCapacity) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("prepare atomic SMS forwarding plan: %w", err)
	}
	if data == nil {
		return nil
	}
	data = bytes.Clone(data)
	if len(data) == 0 || len(data) > MaxSMSLedgerBytes || !utf8.Valid(data) || !json.Valid(data) {
		return errors.New("invalid atomic SMS forwarding ledger")
	}
	if ledger.Value != nil && !ledger.Sensitive {
		return errors.New("SMS forwarding ledger is not private")
	}
	if err := verifySMSForwardDisk(ctx, connection); err != nil {
		return err
	}
	_, err = connection.ExecContext(ctx, `INSERT INTO app_settings(key,value_json,sensitive,updated_at) VALUES(?,?,1,?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,sensitive=1,updated_at=excluded.updated_at`, smsForwardLedgerKey, string(data), now.Unix())
	if err != nil {
		return fmt.Errorf("save atomic SMS forwarding ledger: %w", err)
	}
	return nil
}

// CompareSMSLedgerWithInboxGuard is the same exact-image, FULL-commit CAS as
// CompareSMSLedger, with original Inbox checks in the same writer transaction.
// No unknown claim can commit after its source row was deleted or changed.
func (s *Store) CompareSMSLedgerWithInboxGuard(ctx context.Context, before, after json.RawMessage, guards ...SMSForwardInboxGuard) (bool, error) {
	if before == nil && after == nil {
		return false, errors.New("SMS ledger CAS requires a before or after image")
	}
	for _, value := range []json.RawMessage{before, after} {
		if value != nil && (len(value) == 0 || len(value) > MaxSMSLedgerBytes || !utf8.Valid(value) || !json.Valid(value)) {
			return false, errors.New("invalid SMS ledger CAS data")
		}
	}
	before, after = bytes.Clone(before), bytes.Clone(after)
	guards = append([]SMSForwardInboxGuard(nil), guards...)
	tx, err := s.beginSMSForwardWrite(ctx)
	if err != nil {
		return false, err
	}
	defer tx.close()
	if err := verifySMSForwardDisk(ctx, tx.connection); err != nil {
		return false, err
	}
	for _, guard := range guards {
		matched, err := smsForwardSourceMatches(ctx, tx.connection, guard)
		if err != nil {
			return false, err
		}
		if !matched {
			return false, ErrSMSForwardSourceChanged
		}
	}
	var result sql.Result
	switch {
	case before == nil:
		result, err = tx.connection.ExecContext(ctx, `INSERT INTO app_settings(key,value_json,sensitive,updated_at) VALUES(?,?,1,?) ON CONFLICT(key) DO NOTHING`, smsForwardLedgerKey, string(after), time.Now().UTC().Unix())
	case after == nil:
		result, err = tx.connection.ExecContext(ctx, `DELETE FROM app_settings WHERE key=? AND CAST(value_json AS BLOB)=? AND sensitive=1`, smsForwardLedgerKey, []byte(before))
	default:
		result, err = tx.connection.ExecContext(ctx, `UPDATE app_settings SET value_json=?,updated_at=? WHERE key=? AND CAST(value_json AS BLOB)=? AND sensitive=1`, string(after), time.Now().UTC().Unix(), smsForwardLedgerKey, []byte(before))
	}
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected < 0 || affected > 1 {
		return false, errors.New("invalid guarded SMS ledger CAS result")
	}
	if err := tx.commit(ctx); err != nil {
		return false, err
	}
	return affected == 1, nil
}
