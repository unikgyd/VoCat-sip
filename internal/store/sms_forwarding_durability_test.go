package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestSMSForwardEmptyReceiptsAndArchiveRejectUnsafeFileJournals(t *testing.T) {
	for _, journal := range []string{"off", "memory"} {
		for _, mode := range []string{"no-planner", "empty-plan", "capacity", "archive"} {
			t.Run(journal+"/"+mode, func(t *testing.T) {
				database := smsForwardTestStore(t, filepath.Join(t.TempDir(), "empty-durability.db"))
				if mode == "empty-plan" {
					database.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) { return nil, nil })
				} else if mode == "capacity" {
					database.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) { return nil, ErrSMSForwardCapacity })
				}
				ctx := context.Background()
				var actual string
				if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode="+journal).Scan(&actual); err != nil || actual != journal {
					t.Fatal("could not install the actual unsafe test journal:", actual, err)
				}
				message := smsForwardTestMessage("must-not-claim-an-empty-receipt")
				var err error
				if mode == "archive" {
					_, err = database.SaveSMSMessageArchiveWithResult(ctx, message)
				} else {
					_, err = database.SaveSMSMessage(ctx, message)
				}
				if err == nil || smsForwardTestCount(t, database, "sms_messages") != 0 || smsForwardTestCount(t, database, "sms_forward_receipts") != 0 {
					t.Fatal("an unsafe file journal claimed a durable empty source plan")
				}
				if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&actual); err != nil || actual != "wal" {
					t.Fatal("cannot restore the safe positive control:", actual, err)
				}
				if _, err := database.SaveSMSMessage(ctx, message); err != nil || smsForwardTestCount(t, database, "sms_forward_receipts") != 1 {
					t.Fatal("rejection leaked a transaction or source receipt:", err)
				}
			})
		}
	}
}

func TestSMSForwardLegacyOverCapacityUpgradePersistsProtectionForUnrecordedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-over-capacity.db")
	smsForwardTestLegacyDatabase(t, path) // actual handwritten schema 28, no receipts
	seed, err := openStoreDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Exercise the actual 32768-record limit, not a smaller test-only cap. These
	// are synthetic historical rows, inserted in one statement without live hooks.
	_, err = seed.ExecContext(ctx, `WITH RECURSIVE sequence(n) AS (
		VALUES(1) UNION ALL SELECT n+1 FROM sequence WHERE n<32766)
		INSERT INTO sms_messages(message_id,device_id,modem_imei,iccid,imsi,peer,direction,body,message_time,created_at,updated_at)
		SELECT 'legacy-cap-' || printf('%08d',n),'forward-device','867394042309830','8986000000000000001',
		'001010000000001','+15550000001','inbound','synthetic old archive',1500000000,1500000000,1500000000 FROM sequence`)
	if err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	last, err := scanSMSMessage(seed.QueryRowContext(ctx, smsMessageSelect+` WHERE message_id='legacy-cap-00032766'`))
	if closeErr := seed.Close(); err != nil || closeErr != nil {
		t.Fatal("could not finish the old-schema fixture:", err, closeErr)
	}
	before := time.Now()
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = database.Close()
		}
	}()
	if count := smsForwardTestCount(t, database, "sms_forward_receipts"); count != 32768 {
		t.Fatal("upgrade did not retain exactly the real bounded receipt capacity:", count)
	}
	var guard int64
	if err := database.db.QueryRowContext(ctx, "SELECT guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&guard); err != nil || guard < before.Add(23*time.Hour).Unix() {
		t.Fatal("over-capacity historical sources lack a durable upgrade protection window:", err)
	}
	var remembered int
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sms_forward_receipts WHERE source_key=?", SMSForwardSourceKey(last)).Scan(&remembered); err != nil || remembered != 0 {
		t.Fatal("last source was not actually outside the bounded receipt registry:", err)
	}
	calls := 0
	database.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"must-not-forward-old":true}`), nil
	})
	if _, err := database.db.ExecContext(ctx, "DELETE FROM sms_forward_receipts WHERE source_key IN (SELECT source_key FROM sms_forward_receipts LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteSMSMessage(ctx, last.ID); err != nil {
		t.Fatal(err)
	}
	last.ID = 0
	if _, err := database.SaveSMSMessage(ctx, last); err != nil || calls != 0 {
		t.Fatal("freeing one slot replayed a deleted, unremembered historical source:", err)
	}
	smsForwardTestLedger(t, database, nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened := openTestStore(t, path)
	var after int64
	if err := reopened.db.QueryRowContext(ctx, "SELECT guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&after); err != nil || after != guard {
		t.Fatal("reopen rescanned/renewed the one-time historical capacity guard:", err)
	}
}

func TestSMSForwardUpgradeInitializationRejectsUnsafeFileJournal(t *testing.T) {
	database := smsForwardTestStore(t, filepath.Join(t.TempDir(), "upgrade-durability.db"))
	ctx := context.Background()
	if _, err := database.db.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET initialized=0 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	var journal string
	if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode=OFF").Scan(&journal); err != nil || journal != "off" {
		t.Fatal(err)
	}
	if err := database.initializeSMSForwardReceipts(ctx); err == nil {
		t.Fatal("upgrade claimed durable protection without a persistent journal")
	}
	var initialized int
	if err := database.db.QueryRowContext(ctx, "SELECT initialized FROM sms_forward_receipt_state WHERE id=1").Scan(&initialized); err != nil || initialized != 0 {
		t.Fatal("failed initialization consumed its durable one-time marker:", err)
	}
	if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := database.initializeSMSForwardReceipts(ctx); err != nil {
		t.Fatal("safe initialization could not recover:", err)
	}
}
