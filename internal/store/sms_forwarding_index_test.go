package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func smsForwardReceiptQueryPlan(t *testing.T, db *Store, query string, args ...any) string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func smsForwardRequireReceiptIndexes(t *testing.T, db *Store) {
	t.Helper()
	invalidPlan := smsForwardReceiptQueryPlan(t, db, smsForwardReceiptInvalidSelect)
	if !strings.Contains(invalidPlan, "sms_forward_receipts_invalid_idx") {
		t.Fatalf("receipt validation does not use its partial index: %s", invalidPlan)
	}
	expiryPlan := smsForwardReceiptQueryPlan(t, db,
		"DELETE FROM sms_forward_receipts WHERE expires_at<=?", time.Now().UTC().Unix())
	if !strings.Contains(expiryPlan, "sms_forward_receipts_expiry_idx") {
		t.Fatalf("receipt expiry does not use its expiry index: %s", expiryPlan)
	}
}

func TestSMSForwardReceiptValidationAndExpiryUseIndexes(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "indexed-receipts.db"))
	ctx := context.Background()
	for _, source := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if _, err := db.db.ExecContext(ctx,
			"INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at) VALUES(?,?,?)",
			source, strings.Repeat("c", 64), time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	smsForwardRequireReceiptIndexes(t, db)
	var invalid int
	if err := db.db.QueryRowContext(ctx, smsForwardReceiptInvalidSelect).Scan(&invalid); err != nil || invalid != 0 {
		t.Fatalf("valid receipts were considered corrupt: invalid=%d error=%v", invalid, err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE sms_forward_receipts SET fingerprint=? WHERE source_key=?",
		strings.Repeat("D", 64), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, smsForwardReceiptInvalidSelect).Scan(&invalid); err != nil || invalid != 1 {
		t.Fatalf("partial index did not track a direct SQL update: invalid=%d error=%v", invalid, err)
	}
	smsForwardRequireReceiptIndexes(t, db)
}

func TestSMSForwardSchema29ReopenRepairsIndexesWithoutRenewingReceipts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "existing-schema29.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	mustSaveDevice(t, db, "forward-device", "Synthetic forward device")
	result, err := db.SaveSMSMessageWithResult(ctx, smsForwardTestMessage("before-index-repair"))
	if err != nil || !result.Inserted {
		t.Fatalf("seed existing Inbox and receipt: inserted=%t error=%v", result.Inserted, err)
	}
	key := SMSForwardSourceKey(result.Message)
	expires := time.Now().Add(10 * time.Hour).Unix()
	guardUntil := time.Now().Add(4 * time.Hour).Unix()
	if _, err := db.db.ExecContext(ctx, "UPDATE sms_forward_receipts SET expires_at=? WHERE source_key=?", expires, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET guard_until=? WHERE id=1", guardUntil); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"sms_forward_receipts_invalid_idx", "sms_forward_receipts_expiry_idx"} {
		if _, err := db.db.ExecContext(ctx, "DROP INDEX "+index); err != nil {
			t.Fatal(err)
		}
	}
	for reopen := 0; reopen < 2; reopen++ {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db = nil
		db, err = Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var version, initialized, indexes int
		var actualExpires, actualGuard int64
		if err := db.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 29 {
			t.Fatalf("reopen changed schema revision: version=%d error=%v", version, err)
		}
		if err := db.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('sms_forward_receipts_invalid_idx','sms_forward_receipts_expiry_idx')").Scan(&indexes); err != nil || indexes != 2 {
			t.Fatalf("schema29 indexes were not repaired idempotently: indexes=%d error=%v", indexes, err)
		}
		if err := db.db.QueryRowContext(ctx, "SELECT expires_at FROM sms_forward_receipts WHERE source_key=?", key).Scan(&actualExpires); err != nil || actualExpires != expires {
			t.Fatalf("index repair renewed the source receipt: expiry=%d want=%d error=%v", actualExpires, expires, err)
		}
		if err := db.db.QueryRowContext(ctx, "SELECT initialized,guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&initialized, &actualGuard); err != nil || initialized != 1 || actualGuard != guardUntil {
			t.Fatalf("index repair changed initialization or guard: initialized=%d guard=%d error=%v", initialized, actualGuard, err)
		}
		if smsForwardTestCount(t, db, "sms_forward_receipts") != 1 || smsForwardTestCount(t, db, "sms_messages") != 1 {
			t.Fatal("index repair duplicated or removed existing receipt/Inbox rows")
		}
		smsForwardRequireReceiptIndexes(t, db)
	}
}

type smsForwardReceiptSQLSnapshot struct {
	sourceType, sourceHex, fingerprintType, fingerprintHex, expiryType, expiryLiteral string
}

func smsForwardReceiptSnapshot(t *testing.T, db *Store, rowID int64) smsForwardReceiptSQLSnapshot {
	t.Helper()
	var value smsForwardReceiptSQLSnapshot
	if err := db.db.QueryRowContext(context.Background(),
		"SELECT typeof(source_key),hex(source_key),typeof(fingerprint),hex(fingerprint),typeof(expires_at),quote(expires_at) FROM sms_forward_receipts WHERE rowid=?", rowID).
		Scan(&value.sourceType, &value.sourceHex, &value.fingerprintType, &value.fingerprintHex, &value.expiryType, &value.expiryLiteral); err != nil {
		t.Fatal(err)
	}
	return value
}

func smsForwardReceiptCorruptionMatrix(t *testing.T, dropIndexes bool) {
	t.Helper()
	ctx := context.Background()
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "receipt-corruption.db"))
	if dropIndexes {
		for _, index := range []string{"sms_forward_receipts_invalid_idx", "sms_forward_receipts_expiry_idx"} {
			if _, err := db.db.ExecContext(ctx, "DROP INDEX "+index); err != nil {
				t.Fatal(err)
			}
		}
		plan := smsForwardReceiptQueryPlan(t, db, smsForwardReceiptInvalidSelect)
		if strings.Contains(plan, "sms_forward_receipts_invalid_idx") {
			t.Fatalf("validation depends on a dropped partial index: %s", plan)
		}
	} else {
		smsForwardRequireReceiptIndexes(t, db)
	}
	calls := 0
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return nil, nil
	})
	source := strings.Repeat("a", 64)
	fingerprint := strings.Repeat("b", 64)
	expired := time.Now().Add(-time.Hour).Unix()
	cases := []struct {
		name   string
		column string
		value  any
	}{
		{"empty-source", "source_key", ""},
		{"short-source", "source_key", strings.Repeat("a", 63)},
		{"long-source", "source_key", strings.Repeat("a", 65)},
		{"uppercase-source", "source_key", strings.Repeat("A", 64)},
		{"nonhex-source", "source_key", strings.Repeat("g", 64)},
		{"blob-source", "source_key", []byte(source)},
		{"integer-source", "source_key", int64(123)},
		{"empty-fingerprint", "fingerprint", ""},
		{"short-fingerprint", "fingerprint", strings.Repeat("b", 63)},
		{"long-fingerprint", "fingerprint", strings.Repeat("b", 65)},
		{"uppercase-fingerprint", "fingerprint", strings.Repeat("B", 64)},
		{"nonhex-fingerprint", "fingerprint", strings.Repeat("z", 64)},
		{"blob-fingerprint", "fingerprint", []byte(fingerprint)},
		{"integer-fingerprint", "fingerprint", int64(123)},
		{"zero-expiry", "expires_at", int64(0)},
		{"negative-expiry", "expires_at", int64(-1)},
		{"overflow-expiry", "expires_at", int64(253402300800)},
		{"text-expiry", "expires_at", "not-an-integer"},
		{"blob-expiry", "expires_at", []byte("1")},
		{"real-expiry", "expires_at", float64(1.5)},
	}
	for _, mutation := range []string{"insert", "update"} {
		t.Run(mutation, func(t *testing.T) {
			for _, item := range cases {
				t.Run(item.name, func(t *testing.T) {
					var sourceValue, fingerprintValue, expiryValue any = source, fingerprint, expired
					if mutation == "insert" {
						switch item.column {
						case "source_key":
							sourceValue = item.value
						case "fingerprint":
							fingerprintValue = item.value
						case "expires_at":
							expiryValue = item.value
						}
					}
					inserted, err := db.db.ExecContext(ctx,
						"INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at) VALUES(?,?,?)",
						sourceValue, fingerprintValue, expiryValue)
					if err != nil {
						t.Fatal(err)
					}
					rowID, err := inserted.LastInsertId()
					if err != nil {
						t.Fatal(err)
					}
					if mutation == "update" {
						var invalid int
						if err := db.db.QueryRowContext(ctx, smsForwardReceiptInvalidSelect).Scan(&invalid); err != nil || invalid != 0 {
							t.Fatalf("valid expired receipt rejected before direct SQL update: invalid=%d error=%v", invalid, err)
						}
						if _, err := db.db.ExecContext(ctx, "UPDATE sms_forward_receipts SET "+item.column+"=? WHERE rowid=?", item.value, rowID); err != nil {
							t.Fatal(err)
						}
					}
					before := smsForwardReceiptSnapshot(t, db, rowID)
					var invalid int
					if err := db.db.QueryRowContext(ctx, smsForwardReceiptInvalidSelect).Scan(&invalid); err != nil || invalid != 1 {
						t.Fatalf("direct SQL corruption escaped receipt validation: invalid=%d error=%v", invalid, err)
					}
					if _, err := db.SaveSMSMessageWithResult(ctx, smsForwardTestMessage("must-rollback")); err == nil {
						t.Fatal("receive accepted corrupt source history")
					}
					if calls != 0 || smsForwardTestCount(t, db, "sms_messages") != 0 || smsForwardTestCount(t, db, "sms_forward_receipts") != 1 {
						t.Fatalf("corruption planned, saved an Inbox row, or expiry-pruned evidence: planner calls=%d", calls)
					}
					if after := smsForwardReceiptSnapshot(t, db, rowID); after != before {
						t.Fatal("failed receive changed or normalized corrupt receipt evidence")
					}
					if _, err := db.db.ExecContext(ctx, "DELETE FROM sms_forward_receipts WHERE rowid=?", rowID); err != nil {
						t.Fatal(err)
					}
					if err := db.db.QueryRowContext(ctx, smsForwardReceiptInvalidSelect).Scan(&invalid); err != nil || invalid != 0 {
						t.Fatalf("receipt validation remained poisoned after evidence removal: invalid=%d error=%v", invalid, err)
					}
				})
			}
		})
	}
	result, err := db.SaveSMSMessageWithResult(ctx, smsForwardTestMessage("after-corruption-removal"))
	if err != nil || !result.Inserted || calls != 1 {
		t.Fatalf("rolled-back receive left the connection unusable: inserted=%t calls=%d error=%v", result.Inserted, calls, err)
	}
}

func TestSMSForwardIndexedReceiptsRejectDirectSQLCorruptionBeforeExpiry(t *testing.T) {
	smsForwardReceiptCorruptionMatrix(t, false)
}

func TestSMSForwardMissingIndexesStillRejectDirectSQLCorruptionBeforeExpiry(t *testing.T) {
	smsForwardReceiptCorruptionMatrix(t, true)
}
