package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const smsForwardTestSettingsKey = "sip.gateway.private.v1"
const smsForwardTestLedgerKey = "sip.gateway.sms-ledger.private.v1"

func smsForwardTestMessage(id string) SMSMessage {
	return SMSMessage{MessageID: id, DeviceID: "forward-device", ModemIMEI: "867394042309830",
		ICCID: "8986000000000000001", IMSI: "001010000000001", Peer: "+15550000001",
		Direction: "inbound", Body: "synthetic private inbox body", Source: "ims"}
}

func smsForwardTestStore(t *testing.T, path string) *Store {
	t.Helper()
	db := openTestStore(t, path)
	mustSaveDevice(t, db, "forward-device", "Synthetic forward device")
	return db
}

func smsForwardTestCount(t *testing.T, db *Store, table string) int {
	t.Helper()
	if table != "sms_messages" && table != "sms_forward_receipts" {
		t.Fatal("unexpected test table")
	}
	var count int
	if err := db.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func smsForwardTestLedger(t *testing.T, db *Store, expected json.RawMessage) {
	t.Helper()
	value, err := db.AppSetting(context.Background(), smsForwardTestLedgerKey)
	if expected == nil {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("uncommitted ledger exists: %v", err)
		}
		return
	}
	if err != nil || !value.Sensitive || !bytes.Equal(value.Value, expected) {
		t.Fatalf("private ledger did not retain exact committed bytes: %v", err)
	}
}

func smsForwardTestDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func TestSMSForwardAtomicPlanAndInboxCommitUsesPersistedSnapshot(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "atomic.db"))
	ctx := context.Background()
	settings := json.RawMessage(" \n{\"revision\":\"original\"}\t")
	before := json.RawMessage(" {\"queued\":0}\n")
	after := json.RawMessage(" {\"queued\":1,\"recipient\":\"original epoch\"}\n")
	for _, item := range []AppSetting{{Key: smsForwardTestSettingsKey, Value: settings, Sensitive: true},
		{Key: smsForwardTestLedgerKey, Value: before, Sensitive: true}} {
		if err := db.UpsertAppSetting(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	var input SMSForwardingInput
	calls := 0
	db.SetSMSForwardPlanner(func(value SMSForwardingInput) (json.RawMessage, error) {
		calls++
		input = value
		if !value.Settings.Sensitive || !bytes.Equal(value.Settings.Value, settings) ||
			!value.Ledger.Sensitive || !bytes.Equal(value.Ledger.Value, before) {
			return nil, errors.New("planner did not receive the exact private transaction snapshots")
		}
		return after, nil
	})
	wake := db.SMSInserted()
	message := smsForwardTestMessage("atomic-one")
	message.Peer = " " + message.Peer + " "
	result, err := db.SaveSMSMessageWithResult(ctx, message)
	if err != nil || !result.Inserted || calls != 1 {
		t.Fatalf("atomic receive failed: inserted=%t calls=%d error=%v", result.Inserted, calls, err)
	}
	if input.Message.ID != result.Message.ID || input.Message.Body != result.Message.Body ||
		input.Message.ICCID != result.Message.ICCID || input.Fingerprint != SMSForwardFingerprint(result.Message) ||
		!smsForwardTestDigest(input.SourceKey) || !smsForwardTestDigest(input.Fingerprint) || input.Now.IsZero() ||
		input.OriginalPeer != message.Peer || result.Message.Peer != strings.TrimSpace(message.Peer) {
		t.Fatal("plan is not bound to the actual saved row and original source")
	}
	smsForwardTestLedger(t, db, after)
	var fingerprint string
	var expires int64
	if err := db.db.QueryRowContext(ctx, "SELECT fingerprint,expires_at FROM sms_forward_receipts WHERE source_key=?", input.SourceKey).Scan(&fingerprint, &expires); err != nil {
		t.Fatal(err)
	}
	if fingerprint != input.Fingerprint || expires < input.Now.Unix()+86399 || expires > input.Now.Unix()+86401 {
		t.Fatal("source receipt did not commit with its first fingerprint and bounded lifetime")
	}
	select {
	case <-wake:
	default:
		t.Fatal("committed receive did not wake consumers")
	}
}

func TestSMSForwardPlannerErrorsAndCancellationRollbackWholeReceive(t *testing.T) {
	for _, failure := range []string{"planner error", "invalid json", "empty nonnil json", "invalid utf8", "oversized json", "cancelled planner"} {
		t.Run(failure, func(t *testing.T) {
			db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "rollback.db"))
			before := json.RawMessage(`{"untouched":true}`)
			if err := db.UpsertAppSetting(context.Background(), AppSetting{Key: smsForwardTestLedgerKey, Value: before, Sensitive: true}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
				switch failure {
				case "planner error":
					return nil, errors.New("synthetic planning failure")
				case "invalid json":
					return json.RawMessage(`{`), nil
				case "empty nonnil json":
					return json.RawMessage{}, nil
				case "invalid utf8":
					return json.RawMessage{'"', 0xff, '"'}, nil
				case "oversized json":
					return json.RawMessage(`{"payload":"` + strings.Repeat("x", MaxSMSLedgerBytes) + `"}`), nil
				default:
					cancel()
					return json.RawMessage(`{"must_not_commit":true}`), nil
				}
			})
			wake := db.SMSInserted()
			if _, err := db.SaveSMSMessageWithResult(ctx, smsForwardTestMessage("failed-plan")); err == nil {
				t.Fatal("failed plan was reported as a durable receive")
			}
			if smsForwardTestCount(t, db, "sms_messages") != 0 || smsForwardTestCount(t, db, "sms_forward_receipts") != 0 {
				t.Fatal("failed receive left an Inbox row or source receipt")
			}
			smsForwardTestLedger(t, db, before)
			select {
			case <-wake:
				t.Fatal("failed receive woke insertion consumers")
			default:
			}
			db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) { return json.RawMessage(`{"recovered":true}`), nil })
			if _, err := db.SaveSMSMessage(context.Background(), smsForwardTestMessage("good-plan")); err != nil {
				t.Fatal("connection remained poisoned after rollback:", err)
			}
		})
	}
}

func TestSMSForwardCommitFailureRollsBackAndConnectionRecovers(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "commit-failure.db"))
	ctx := context.Background()
	_, err := db.db.ExecContext(ctx, `CREATE TABLE forward_commit_parent(id INTEGER PRIMARY KEY);
		CREATE TABLE forward_commit_child(parent INTEGER REFERENCES forward_commit_parent(id) DEFERRABLE INITIALLY DEFERRED);
		CREATE TRIGGER forward_commit_failure AFTER INSERT ON sms_forward_receipts
		BEGIN INSERT INTO forward_commit_child(parent) VALUES(77); END`)
	if err != nil {
		t.Fatal(err)
	}
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) { return json.RawMessage(`{"planned":true}`), nil })
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("failed-commit")); err == nil || !strings.Contains(strings.ToLower(err.Error()), "commit") {
		t.Fatal("real failed COMMIT was not reported as a receive failure:", err)
	}
	if smsForwardTestCount(t, db, "sms_messages") != 0 || smsForwardTestCount(t, db, "sms_forward_receipts") != 0 {
		t.Fatal("failed COMMIT left uncommitted Inbox/source data visible")
	}
	smsForwardTestLedger(t, db, nil)
	if _, err := db.db.ExecContext(ctx, "DROP TRIGGER forward_commit_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("successful-after-failed-commit")); err != nil {
		t.Fatal("failed COMMIT left an active transaction or unsafe connection:", err)
	}
	smsForwardTestLedger(t, db, json.RawMessage(`{"planned":true}`))
}

func TestSMSForwardDuplicateSourceSurvivesInboxDeletionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt-restart.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})
	mustSaveDevice(t, db, "forward-device", "Synthetic original device")
	var original SMSForwardingInput
	calls := 0
	db.SetSMSForwardPlanner(func(input SMSForwardingInput) (json.RawMessage, error) {
		calls++
		original = input
		return json.RawMessage(`{"first_recipient":"original"}`), nil
	})
	message := smsForwardTestMessage("stable-modem-source")
	saved, err := db.SaveSMSMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	var until int64
	if err := db.db.QueryRow("SELECT expires_at FROM sms_forward_receipts WHERE source_key=?", original.SourceKey).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSMSMessage(context.Background(), message); err != nil || calls != 1 {
		t.Fatalf("normal source retry expanded its original plan: calls=%d err=%v", calls, err)
	}
	if err := db.DeleteSMSMessage(context.Background(), saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened := openTestStore(t, path)
	newCalls := 0
	reopened.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		newCalls++
		return json.RawMessage(`{"must_not_assign":"new account"}`), nil
	})
	// Hardware is the stable original source even if the UI's device id changed.
	mustSaveDevice(t, reopened, "renamed-forward-device", "Synthetic renamed device")
	message.DeviceID = "renamed-forward-device"
	if _, err := reopened.SaveSMSMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if newCalls != 0 || smsForwardTestCount(t, reopened, "sms_forward_receipts") != 1 {
		t.Fatal("deleted Inbox/restarted/renamed source was assigned to a new recipient")
	}
	var afterUntil int64
	if err := reopened.db.QueryRow("SELECT expires_at FROM sms_forward_receipts WHERE source_key=?", original.SourceKey).Scan(&afterUntil); err != nil || afterUntil != until {
		t.Fatal("duplicate or restart renewed the original source receipt")
	}
	smsForwardTestLedger(t, reopened, json.RawMessage(`{"first_recipient":"original"}`))
}

func TestSMSForwardIndependentStoresAdmitOneOriginalPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "independent-stores.db")
	one := smsForwardTestStore(t, path)
	two := openTestStore(t, path)
	var calls atomic.Int32
	planner := func(input SMSForwardingInput) (json.RawMessage, error) {
		calls.Add(1)
		return json.Marshal(map[string]any{"first_source": input.SourceKey, "original_fingerprint": input.Fingerprint})
	}
	one.SetSMSForwardPlanner(planner)
	two.SetSMSForwardPlanner(planner)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errorsFound := make(chan error, 8)
	var workers sync.WaitGroup
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			db := one
			if index%2 != 0 {
				db = two
			}
			_, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("same-physical-source"))
			errorsFound <- err
		}(index)
	}
	close(start)
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 || smsForwardTestCount(t, one, "sms_messages") != 1 || smsForwardTestCount(t, one, "sms_forward_receipts") != 1 {
		t.Fatal("same-file independent receive stores did not commit exactly one original plan")
	}
}

func TestSMSForwardPlannerSerializesSettingsWithReceive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings-snapshot.db")
	one := smsForwardTestStore(t, path)
	two := openTestStore(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	old := json.RawMessage(`{"epoch":"old"}`)
	newer := json.RawMessage(`{"epoch":"new"}`)
	if err := one.UpsertAppSetting(ctx, AppSetting{Key: smsForwardTestSettingsKey, Value: old, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	one.SetSMSForwardPlanner(func(input SMSForwardingInput) (json.RawMessage, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !bytes.Equal(input.Settings.Value, old) {
			return nil, errors.New("receive used later account settings")
		}
		return json.RawMessage(`{"recipient_epoch":"old"}`), nil
	})
	received := make(chan error, 1)
	go func() {
		_, err := one.SaveSMSMessage(ctx, smsForwardTestMessage("paused-original-receive"))
		received <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	updated := make(chan error, 1)
	go func() {
		updated <- two.UpsertAppSetting(ctx, AppSetting{Key: smsForwardTestSettingsKey, Value: newer, Sensitive: true})
	}()
	releaseOnce.Do(func() { close(release) })
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	smsForwardTestLedger(t, two, json.RawMessage(`{"recipient_epoch":"old"}`))
	setting, err := one.AppSetting(ctx, smsForwardTestSettingsKey)
	if err != nil || !bytes.Equal(setting.Value, newer) {
		t.Fatal("independent writer did not finish after original receive committed")
	}
}

func TestSMSForwardFirstCompletedConcatCapturesOnce(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "concat.db"))
	calls := 0
	var captured SMSMessage
	var capturedRawPeer string
	db.SetSMSForwardPlanner(func(input SMSForwardingInput) (json.RawMessage, error) {
		calls++
		captured = input.Message
		capturedRawPeer = input.OriginalPeer
		return json.RawMessage(`{"complete":true}`), nil
	})
	message := smsForwardTestMessage(StableConcatMessageID("ims", "867394042309830", "forward-device", "+15550000001", 22, 2))
	message.Peer = " " + message.Peer + " "
	originalRawPeer := message.Peer
	message.PartsTotal, message.Body, message.Extra = 2, "first ", concatExtra(t, 22, 2, 1)
	partial, err := db.SaveSMSMessage(context.Background(), message)
	if err != nil || calls != 0 || smsForwardTestCount(t, db, "sms_forward_receipts") != 0 || SMSForwardMessageReady(partial) {
		t.Fatalf("partial concat prematurely acquired a final plan: calls=%d err=%v", calls, err)
	}
	message.Body, message.Extra = "second", concatExtra(t, 22, 2, 2)
	message.Peer = strings.TrimSpace(message.Peer)
	complete, err := db.SaveSMSMessage(context.Background(), message)
	if err != nil || calls != 1 || !SMSForwardMessageReady(complete) || captured.Body != "first second" || captured.ID != complete.ID {
		t.Fatalf("first complete concat did not capture merged actual row: calls=%d err=%v", calls, err)
	}
	if capturedRawPeer != originalRawPeer {
		t.Fatal("later concat segment replaced the original raw sender and could upgrade read-only reply identity")
	}
	message.Body, message.Extra = "first ", concatExtra(t, 22, 2, 1)
	if _, err := db.SaveSMSMessage(context.Background(), message); err != nil || calls != 1 || smsForwardTestCount(t, db, "sms_forward_receipts") != 1 {
		t.Fatal("concat rescan created another original plan")
	}
}

func TestSMSForwardArchiveAndEmptyPlanNeverGainRecipients(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "empty-plans.db"))
	ctx := context.Background()
	archived := smsForwardTestMessage("batch-history")
	calls := 0
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"must_not_forward_history":true}`), nil
	})
	if err := db.SaveSMSMessages(ctx, []SMSMessage{archived}); err != nil || calls != 0 {
		t.Fatal("batch history invoked live receive planner:", err)
	}
	db.SetSMSForwardPlanner(nil)
	empty := smsForwardTestMessage("received-while-disabled")
	if _, err := db.SaveSMSMessage(ctx, empty); err != nil {
		t.Fatal(err)
	}
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"must_not_add_new_recipient":true}`), nil
	})
	for _, message := range []SMSMessage{archived, empty} {
		if _, err := db.SaveSMSMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 || smsForwardTestCount(t, db, "sms_forward_receipts") != 2 {
		t.Fatal("history/empty original plan acquired future recipients")
	}
	smsForwardTestLedger(t, db, nil)
	capacity := smsForwardTestMessage("capacity-rejected-plan")
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) { calls++; return nil, ErrSMSForwardCapacity })
	if _, err := db.SaveSMSMessage(ctx, capacity); err != nil || calls != 1 {
		t.Fatal("capacity rejection discarded the local Inbox:", err)
	}
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"must_not_replay_capacity":true}`), nil
	})
	if _, err := db.SaveSMSMessage(ctx, capacity); err != nil || calls != 1 {
		t.Fatal("capacity-empty receipt became a new plan:", err)
	}
	if smsForwardTestCount(t, db, "sms_messages") != 3 || smsForwardTestCount(t, db, "sms_forward_receipts") != 3 {
		t.Fatal("empty/capacity receive did not keep Inbox and source protection")
	}
	// A later manual rescan cannot retire the original identity of a live plan.
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"original_live_plan":true}`), nil
	})
	live := smsForwardTestMessage("live-before-manual-rescan")
	saved, err := db.SaveSMSMessage(ctx, live)
	if err != nil || calls != 2 {
		t.Fatal("new live receive did not capture a plan:", err)
	}
	guard := SMSForwardInboxGuard{ID: saved.ID, Fingerprint: SMSForwardFingerprint(saved)}
	live.Extra = json.RawMessage(`{"_sip_forward_origin_v1":{"version":1,"peer":"FORGED","iccid":"FORGED","archive":true}}`)
	if _, err := db.SaveSMSMessageArchiveWithResult(ctx, live); err != nil || calls != 2 {
		t.Fatal("manual archive rescan replanned an original live source:", err)
	}
	if matches, err := db.SMSForwardSourceMatches(ctx, guard); err != nil || !matches {
		t.Fatal("manual rescan rewrote the first live origin and invalidated its queued plan:", err)
	}
}

func TestSMSForwardFirstSourceOriginCannotBeUpgradedOrForged(t *testing.T) {
	for _, scenario := range []string{"first unknown card", "first raw card whitespace", "first raw sender whitespace"} {
		t.Run(scenario, func(t *testing.T) {
			db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "origin.db"))
			ctx := context.Background()
			if err := db.UpsertDeviceRuntime(ctx, DeviceRuntime{DeviceID: "forward-device", ICCID: "8986000000000000099", IMSI: "001010000000099"}); err != nil {
				t.Fatal(err)
			}
			first := smsForwardTestMessage("concat:first-origin-" + scenario)
			switch scenario {
			case "first unknown card":
				first.ICCID, first.IMSI = "", ""
			case "first raw card whitespace":
				first.ICCID = " " + first.ICCID + " "
			default:
				first.Peer = " " + first.Peer + " "
			}
			originalPeer, originalICCID := first.Peer, first.ICCID
			first.Body, first.PartsTotal, first.Extra = "first ", 2, concatExtra(t, 33, 2, 1)
			var captured SMSForwardingInput
			calls := 0
			db.SetSMSForwardPlanner(func(input SMSForwardingInput) (json.RawMessage, error) {
				calls++
				captured = input
				return json.RawMessage(`{"original_source":true}`), nil
			})
			if _, err := db.SaveSMSMessage(ctx, first); err != nil || calls != 0 {
				t.Fatal("initial partial receive failed or planned early:", err)
			}
			second := first
			second.Peer = "+15550000001"
			second.ICCID, second.IMSI = "8986000000000000001", "001010000000001"
			second.Body = "second"
			var extra map[string]any
			if err := json.Unmarshal(concatExtra(t, 33, 2, 2), &extra); err != nil {
				t.Fatal(err)
			}
			extra["_sip_forward_origin_v1"] = map[string]any{"version": 1, "peer": second.Peer, "iccid": second.ICCID, "archive": false}
			var err error
			second.Extra, err = json.Marshal(extra)
			if err != nil {
				t.Fatal(err)
			}
			merged, err := db.SaveSMSMessage(ctx, second)
			if err != nil || calls != 1 || !SMSForwardMessageReady(merged) {
				t.Fatal("complete original-source message was not saved once:", err)
			}
			if captured.OriginalPeer != originalPeer || captured.OriginalICCID != originalICCID {
				t.Fatal("later segment/current card/caller Extra upgraded the first raw identity")
			}
			if scenario == "first unknown card" && (merged.ICCID != "" || merged.IMSI != "") {
				t.Fatal("unknown source acquired the currently active SIM")
			}
		})
	}
	// A pre-origin row is historical even if it had no receipt at its old write.
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "missing-origin.db"))
	ctx := context.Background()
	legacy := smsForwardTestMessage("legacy-missing-origin")
	legacy.ModemIMEI, legacy.ICCID, legacy.IMSI = "", "", ""
	if _, err := saveSMSMessage(ctx, db.db, legacy); err != nil {
		t.Fatal(err)
	}
	calls := 0
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"must_not_adopt_legacy":true}`), nil
	})
	legacy.ModemIMEI, legacy.ICCID, legacy.IMSI = "867394042309830", "8986000000000000001", "001010000000001"
	legacy.Extra = json.RawMessage(`{"_sip_forward_origin_v1":{"version":1,"peer":"+15550000001","iccid":"8986000000000000001","archive":false}}`)
	if _, err := db.SaveSMSMessage(ctx, legacy); err != nil || calls != 0 {
		t.Fatal("missing-origin history adopted caller/current card forwarding identity:", err)
	}
	if _, err := db.SaveSMSMessage(ctx, legacy); err != nil || calls != 0 {
		t.Fatal("later rescan removed legacy read-only/archive origin:", err)
	}
	smsForwardTestLedger(t, db, nil)
}

func TestSMSForwardMemoryInboxCannotAuthorizeDurablePlan(t *testing.T) {
	db := smsForwardTestStore(t, ":memory:")
	ctx := context.Background()
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("ordinary-memory-inbox")); err != nil {
		t.Fatal("ordinary local memory Inbox stopped working without a forwarding plan:", err)
	}
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		return json.RawMessage(`{"must_not_authorize_durability":true}`), nil
	})
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("memory-planned-inbox")); err == nil {
		t.Fatal("memory-only database authorized a durable forwarding plan")
	}
	if smsForwardTestCount(t, db, "sms_messages") != 1 || smsForwardTestCount(t, db, "sms_forward_receipts") != 1 {
		t.Fatal("rejected memory forwarding left a committed Inbox/source claim")
	}
	smsForwardTestLedger(t, db, nil)
	db.SetSMSForwardPlanner(nil)
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("memory-after-rejection")); err != nil {
		t.Fatal("rejected durable forwarding poisoned ordinary memory storage:", err)
	}
}

func TestSMSForwardCapacityGuardPersistsUntilExpiry(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "capacity.db"))
	ctx := context.Background()
	_, err := db.db.ExecContext(ctx, `WITH RECURSIVE sequence(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM sequence WHERE n<32768)
		INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at)
		SELECT printf('%064x',n),?,? FROM sequence`, strings.Repeat("a", 64), time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"after_guard":true}`), nil
	})
	first := smsForwardTestMessage("unrecorded-full-capacity-source")
	if _, err := db.SaveSMSMessage(ctx, first); err != nil || calls != 0 {
		t.Fatal("full source capacity did not preserve local Inbox without forwarding:", err)
	}
	var guard int64
	if err := db.db.QueryRowContext(ctx, "SELECT guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&guard); err != nil || guard < time.Now().Add(23*time.Hour).Unix() {
		t.Fatal("full-capacity source was not protected by a persistent window:", err)
	}
	if _, err := db.db.ExecContext(ctx, "DELETE FROM sms_forward_receipts WHERE source_key=printf('%064x',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("during-capacity-guard")); err != nil || calls != 0 {
		t.Fatal("freeing one receipt bypassed the active capacity guard:", err)
	}
	var after int64
	if err := db.db.QueryRowContext(ctx, "SELECT guard_until FROM sms_forward_receipt_state WHERE id=1").Scan(&after); err != nil || after < guard {
		t.Fatal("minor capacity release shortened the protection window")
	}
	// Synthetic wall-clock movement: do not sleep or wait a day in the test.
	if _, err := db.db.ExecContext(ctx, "DELETE FROM sms_forward_receipts WHERE source_key IN (printf('%064x',2),printf('%064x',3),printf('%064x',4))"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET guard_until=? WHERE id=1", time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("new-source-after-guard")); err != nil || calls != 1 {
		t.Fatal("expired guard did not permit a genuinely new bounded source:", err)
	}
}

func TestSMSForwardCorruptProtectionFailsBeforeExpiryPruning(t *testing.T) {
	for _, corruption := range []string{"expired bad source", "expired bad fingerprint", "bad expiry", "bad guard", "uninitialized state"} {
		t.Run(corruption, func(t *testing.T) {
			db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "corrupt.db"))
			ctx := context.Background()
			if _, err := db.db.ExecContext(ctx, "PRAGMA ignore_check_constraints=ON"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch corruption {
			case "expired bad source":
				_, err = db.db.ExecContext(ctx, "INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at) VALUES(?,?,?)", "not-a-hash", strings.Repeat("a", 64), time.Now().Add(-time.Hour).Unix())
			case "expired bad fingerprint":
				_, err = db.db.ExecContext(ctx, "INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at) VALUES(?,?,?)", strings.Repeat("b", 64), "not-a-hash", time.Now().Add(-time.Hour).Unix())
			case "bad expiry":
				_, err = db.db.ExecContext(ctx, "INSERT INTO sms_forward_receipts(source_key,fingerprint,expires_at) VALUES(?,?,0)", strings.Repeat("b", 64), strings.Repeat("a", 64))
			case "bad guard":
				_, err = db.db.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET guard_until=-1 WHERE id=1")
			default:
				_, err = db.db.ExecContext(ctx, "UPDATE sms_forward_receipt_state SET initialized=0 WHERE id=1")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.ExecContext(ctx, "PRAGMA ignore_check_constraints=OFF"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`{"must_not_send":true}`), nil
			})
			if _, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("must-not-erase-corruption")); err == nil || calls != 0 {
				t.Fatal("corrupt protection was pruned or treated as empty instead of failing closed")
			}
			if smsForwardTestCount(t, db, "sms_messages") != 0 {
				t.Fatal("corrupt protection still committed a newly planned Inbox")
			}
		})
	}
}

// Construct the old schema without new Store.Open or receipt tables. This is an
// actual old database upgrade, not deletion of a new table to imitate one.
func smsForwardTestLegacyDatabase(t *testing.T, path string) []SMSMessage {
	t.Helper()
	db, err := openStoreDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for version := 1; version <= 28; version++ {
		for _, statement := range migrationStatements(version) {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				t.Fatalf("old schema migration %d: %v", version, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version=28"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO devices(id,name,created_at,updated_at) VALUES('forward-device','Legacy synthetic device',1500000000,1500000000)"); err != nil {
		t.Fatal(err)
	}
	complete := smsForwardTestMessage("legacy-stable-source")
	complete.Timestamp = time.Unix(1_500_000_000, 0).UTC()
	partial := smsForwardTestMessage("concat:legacy-partial-source")
	partial.Body, partial.PartsTotal, partial.Extra = "old first ", 2, concatExtra(t, 44, 2, 1)
	unknown := smsForwardTestMessage("legacy-unknown-sim")
	unknown.ICCID, unknown.IMSI = "", ""
	result := make([]SMSMessage, 0, 3)
	for _, message := range []SMSMessage{complete, partial, unknown} {
		saved, err := saveSMSMessage(ctx, tx, message)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, saved)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSMSForwardLegacyUpgradeUsesEmptyOriginalReceiptsAndDoesNotRenew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-v28.db")
	legacy := smsForwardTestLegacyDatabase(t, path)
	before := time.Now().UTC()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})
	if smsForwardTestCount(t, db, "sms_forward_receipts") != len(legacy) {
		t.Fatal("upgrade did not protect every original Inbox including partial/unknown sources")
	}
	var earliest, latest int64
	if err := db.db.QueryRow("SELECT MIN(expires_at),MAX(expires_at) FROM sms_forward_receipts").Scan(&earliest, &latest); err != nil || earliest < before.Add(23*time.Hour).Unix() || latest > time.Now().Add(25*time.Hour).Unix() {
		t.Fatal("old message timestamps made the upgrade protection immediately expire:", err)
	}
	smsForwardTestLedger(t, db, nil)
	if _, err := db.db.Exec("UPDATE sms_forward_receipts SET expires_at=expires_at-60"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened := openTestStore(t, path)
	var reopenedEarliest int64
	if err := reopened.db.QueryRow("SELECT MIN(expires_at) FROM sms_forward_receipts").Scan(&reopenedEarliest); err != nil || reopenedEarliest != earliest-60 {
		t.Fatal("reopen rescanned legacy Inbox or renewed source protection")
	}
	calls := 0
	reopened.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"must_not_reassign_legacy":true}`), nil
	})
	for _, old := range legacy {
		if err := reopened.DeleteSMSMessage(context.Background(), old.ID); err != nil {
			t.Fatal(err)
		}
		if old.MessageID == "legacy-stable-source" {
			old.ICCID = " " + old.ICCID + " "
			old.Peer = " " + old.Peer + " "
		}
		if strings.HasPrefix(old.MessageID, "concat:") {
			old.Body, old.Extra = "new second", concatExtra(t, 44, 2, 2)
		}
		old.ID = 0 // Network resubmission carries the stable source, not a former SQL row id.
		if _, err := reopened.SaveSMSMessage(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(old.MessageID, "concat:") {
			old.Body, old.Extra = "old first ", concatExtra(t, 44, 2, 1)
			if _, err := reopened.SaveSMSMessage(context.Background(), old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls != 0 {
		t.Fatal("deleted old Inbox source was forwarded after upgrade to a new account")
	}
	smsForwardTestLedger(t, reopened, nil)
}

func TestSMSForwardConcurrentLegacyOpenDoesNotRenewOrReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-old-v28.db")
	legacy := smsForwardTestLegacyDatabase(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	type outcome struct {
		db  *Store
		err error
	}
	opened := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; db, err := Open(ctx, path); opened <- outcome{db, err} }()
	}
	close(start)
	stores := make([]*Store, 0, 2)
	for i := 0; i < 2; i++ {
		result := <-opened
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		stores = append(stores, result.db)
		t.Cleanup(func() { _ = result.db.Close() })
	}
	if len(stores) != 2 {
		t.Fatal("concurrent old database opens did not both finish")
	}
	if smsForwardTestCount(t, stores[0], "sms_forward_receipts") != len(legacy) {
		t.Fatal("concurrent upgrade lost original sources")
	}
	var initialized int
	if err := stores[1].db.QueryRow("SELECT initialized FROM sms_forward_receipt_state WHERE id=1").Scan(&initialized); err != nil || initialized != 1 {
		t.Fatal("initialization was not durably complete:", err)
	}
	var calls atomic.Int32
	for _, db := range stores {
		db.SetSMSForwardPlanner(func(SMSForwardingInput) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"must_not_replay":true}`), nil
		})
	}
	if err := stores[0].DeleteSMSMessage(ctx, legacy[0].ID); err != nil {
		t.Fatal(err)
	}
	replay := legacy[0]
	replay.ID = 0
	if _, err := stores[1].SaveSMSMessage(ctx, replay); err != nil || calls.Load() != 0 {
		t.Fatal("concurrent migrated managers replayed a legacy source:", err)
	}
}

func TestSMSForwardFingerprintAndReadinessGuardExactOriginalData(t *testing.T) {
	original := smsForwardTestMessage("fingerprint-source")
	fingerprint := SMSForwardFingerprint(original)
	if !smsForwardTestDigest(fingerprint) || !SMSForwardMessageReady(original) {
		t.Fatal("valid original did not produce an eligible bound fingerprint")
	}
	for name, change := range map[string]func(*SMSMessage){
		"device":       func(m *SMSMessage) { m.DeviceID = "another-device" },
		"hardware":     func(m *SMSMessage) { m.ModemIMEI = "867394042309831" },
		"subscription": func(m *SMSMessage) { m.ICCID = "8986000000000000002" },
		"imsi":         func(m *SMSMessage) { m.IMSI = "001010000000002" },
		"source id":    func(m *SMSMessage) { m.MessageID = "different-source" },
		"raw sender":   func(m *SMSMessage) { m.Peer = " " + m.Peer },
		"body":         func(m *SMSMessage) { m.Body += " changed" },
		"direction":    func(m *SMSMessage) { m.Direction = "outbound" },
		"readiness":    func(m *SMSMessage) { m.Extra = json.RawMessage(`{"decode_error":"synthetic failure"}`) },
		"original source provenance": func(m *SMSMessage) {
			m.Extra = json.RawMessage(`{"_sip_forward_origin_v1":{"version":1,"peer":" raw original ","iccid":"raw card","archive":false}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := original
			change(&changed)
			if SMSForwardFingerprint(changed) == fingerprint {
				t.Fatal("critical original source/content field was not bound")
			}
		})
	}
	readOnly := original
	readOnly.Read = true
	readOnly.UpdatedAt = time.Now()
	if SMSForwardFingerprint(readOnly) != fingerprint {
		t.Fatal("reading a message invalidated its original forwarding content")
	}
	for _, message := range []SMSMessage{{Direction: "inbound", Body: " "}, {Direction: "outbound", Body: "text"}, {Direction: "inbound", Body: "text", Extra: json.RawMessage(`{"decode_error":"bad"}`)}, {Direction: "inbound", Body: "text", MessageID: "concat:partial", Extra: json.RawMessage(`{"concat_complete":false}`)}} {
		if SMSForwardMessageReady(message) {
			t.Fatal("unsupported/partial message was eligible for payload forwarding")
		}
	}
}

func TestSMSForwardGuardedCASBindsCurrentInboxContent(t *testing.T) {
	db := smsForwardTestStore(t, filepath.Join(t.TempDir(), "guarded-cas.db"))
	ctx := context.Background()
	saved, err := db.SaveSMSMessage(ctx, smsForwardTestMessage("guarded-original"))
	if err != nil {
		t.Fatal(err)
	}
	guard := SMSForwardInboxGuard{ID: saved.ID, Fingerprint: SMSForwardFingerprint(saved)}
	if matches, err := db.SMSForwardSourceMatches(ctx, guard); err != nil || !matches {
		t.Fatal("actual source did not match its original guard:", err)
	}
	before, after := json.RawMessage(`{"step":1}`), json.RawMessage(`{"step":2}`)
	if ok, err := db.CompareSMSLedgerWithInboxGuard(ctx, nil, before, guard); err != nil || !ok {
		t.Fatal("guarded initial CAS failed:", err)
	}
	if err := db.MarkSMSMessagesRead(ctx, []int64{saved.ID}); err != nil {
		t.Fatal(err)
	}
	if matches, err := db.SMSForwardSourceMatches(ctx, guard); err != nil || !matches {
		t.Fatal("non-payload read flag invalidated source:", err)
	}
	if ok, err := db.CompareSMSLedgerWithInboxGuard(ctx, json.RawMessage(`{"wrong":true}`), after, guard); err != nil || ok {
		t.Fatal("ordinary CAS mismatch did not remain false/nil:", err)
	}
	if ok, err := db.CompareSMSLedgerWithInboxGuard(ctx, before, after, guard); err != nil || !ok {
		t.Fatal("valid guarded CAS did not commit:", err)
	}
	for _, update := range []string{"UPDATE sms_messages SET body='different body' WHERE id=?", "UPDATE sms_messages SET peer='+15550000002' WHERE id=?", "UPDATE sms_messages SET direction='outbound' WHERE id=?", "UPDATE sms_messages SET extra_json=json_set(extra_json,'$._sip_forward_origin_v1.peer','FORGED') WHERE id=?"} {
		if _, err := db.db.ExecContext(ctx, update, saved.ID); err != nil {
			t.Fatal(err)
		}
		if matches, err := db.SMSForwardSourceMatches(ctx, guard); err != nil || matches {
			t.Fatal("modified actual Inbox still matched original source:", err)
		}
		if ok, err := db.CompareSMSLedgerWithInboxGuard(ctx, after, json.RawMessage(`{"must_not_claim":true}`), guard); ok || !errors.Is(err, ErrSMSForwardSourceChanged) {
			t.Fatal("source change did not veto guarded CAS:", err)
		}
		smsForwardTestLedger(t, db, after)
		if _, err := db.db.ExecContext(ctx, "UPDATE sms_messages SET body=?,peer=?,direction=?,extra_json=? WHERE id=?", saved.Body, saved.Peer, saved.Direction, string(saved.Extra), saved.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteSMSMessage(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if matches, err := db.SMSForwardSourceMatches(ctx, guard); err != nil || matches {
		t.Fatal("deleted actual Inbox still matched source:", err)
	}
	if ok, err := db.CompareSMSLedgerWithInboxGuard(ctx, after, json.RawMessage(`{"must_not_claim_deleted":true}`), guard); ok || !errors.Is(err, ErrSMSForwardSourceChanged) {
		t.Fatal("deleted source authorized a claim:", err)
	}
	smsForwardTestLedger(t, db, after)
}
