package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"vocat/internal/store"
)

const smsCaptureICCID = "8900000000000000001"

func smsCaptureFixture(t *testing.T) (context.Context, *store.Store, *Manager, *syntheticSMS) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	database := carrierTestStore(t)
	backend := newSyntheticSMS()
	backend.subscription = hashCallOperation(smsCaptureICCID)
	manager := smsAuthorityStart(t, ctx, database, backend)
	return ctx, database, manager, backend
}

func smsCaptureMessage(id, body string) store.SMSMessage {
	return store.SMSMessage{MessageID: id, DeviceID: "module-a", ModemIMEI: "860000000000001",
		ICCID: smsCaptureICCID, IMSI: "001010000000001", Peer: "+15550000001", Direction: "inbound",
		Body: body, Source: "ims", PartsTotal: 1}
}

func smsCaptureRawLedger(t *testing.T, database *store.Store) smsLedger {
	t.Helper()
	value, err := database.AppSetting(context.Background(), SMSLedgerSettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return smsLedger{Version: 3, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{}}
	}
	if err != nil {
		t.Fatal(err)
	}
	var state smsLedger
	if !value.Sensitive || json.Unmarshal(value.Value, &state) != nil || state.Version != 3 || state.Attempts == nil || state.Inbox == nil {
		t.Fatal("actual private SMS journal is invalid")
	}
	return state // Raw durable bytes: do not hide a missing migration with load().
}

func smsCaptureSavedInbox(t *testing.T, database *store.Store, saved store.SMSMessage) (string, smsInbox) {
	t.Helper()
	key := hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
	state := smsCaptureRawLedger(t, database)
	in, exists := state.Inbox[key]
	if !exists {
		t.Fatal("Store receive did not atomically create its actual Manager forwarding plan")
	}
	return key, in
}

func smsCaptureNoUDP(t *testing.T, phone *wireClient) {
	t.Helper()
	if err := phone.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("a cancelled original plan exposed a private UDP payload")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("could not verify the still-registered receiver:", err)
	}
}

func smsCaptureFreshWireControl(t *testing.T, ctx context.Context, database *store.Store, manager *Manager, phone *wireClient, id string) {
	t.Helper()
	message := smsCaptureMessage(id, "synthetic fresh positive-control body")
	saved, err := database.SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	delivery := replyOneMessage(phone.conn, 200)
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-delivery
	if result.err != nil || result.request == nil || string(result.request.Body()) != message.Body {
		t.Fatal("fresh positive control did not traverse the still-working receiver:", result.err)
	}
	_, in := smsCaptureSavedInbox(t, database, saved)
	if in.Targets["1001"].Code != 200 || !validSMSAttemptToken(in.Targets["1001"].AttemptToken) {
		t.Fatal("fresh positive control did not retain its durable accepted result")
	}
}

// The only synthetic part is the carrier backend. Manager admission, saved
// settings, the Inbox, atomic builder, private ledger and UDP are all real.
func TestSMSCaptureActualManagerStoreReceiveBindsActualInboxAndOriginalPayload(t *testing.T) {
	ctx, database, manager, backend := smsCaptureFixture(t)
	message := smsCaptureMessage("capture-bound-inbox", "synthetic first original body")
	saved, err := database.SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	_, in := smsCaptureSavedInbox(t, database, saved)
	if in.InboxID != saved.ID || in.SourceKey != store.SMSForwardSourceKey(saved) ||
		in.SourceFingerprint != store.SMSForwardFingerprint(saved) || in.SourceIndependent ||
		in.Message.ID != message.MessageID || in.Message.DeviceID != message.DeviceID || in.Message.Subscription != smsCaptureICCID ||
		in.Message.Sender != message.Peer || in.Message.Text != message.Body ||
		len(in.Targets) != 1 || in.Targets["1001"].Identity != recipientIdentity(manager.current, "1001") || in.Targets["1001"].Code != 0 {
		t.Fatal("actual receive lost its Inbox/source/card/payload/original-recipient binding")
	}
	if matched, err := database.SMSForwardSourceMatches(ctx, store.SMSForwardInboxGuard{ID: in.InboxID, Fingerprint: in.SourceFingerprint}); err != nil || !matched {
		t.Fatal("captured plan does not match its actual durable Inbox:", err)
	}
	if backend.count() != 0 {
		t.Fatal("receiving or planning an SMS submitted a carrier message")
	}
}

func TestSMSCaptureReadAcknowledgementKeepsActualMTAndCompactOriginalCardReplyWorking(t *testing.T) {
	ctx, database, manager, backend := smsCaptureFixture(t)
	phone := registerMessagePeer(t, manager.runtime, "1001")
	message := smsCaptureMessage("capture-read-then-deliver", "synthetic readable original body")
	saved, err := database.SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	_, before := smsCaptureSavedInbox(t, database, saved)
	if err := database.MarkSMSMessagesRead(ctx, []int64{saved.ID}); err != nil {
		t.Fatal(err)
	}
	read, err := database.SMSMessage(ctx, saved.ID)
	if err != nil || !read.Read {
		t.Fatal("actual Inbox read acknowledgement did not persist:", err)
	}
	if matches, err := database.SMSForwardSourceMatches(ctx, store.SMSForwardInboxGuard{ID: saved.ID, Fingerprint: before.SourceFingerprint}); err != nil || !matches {
		t.Fatal("a non-payload read flag invalidated the original source guard:", err)
	}
	delivery := replyOneMessage(phone.conn, 200)
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-delivery
	if result.err != nil || result.request == nil || string(result.request.Body()) != message.Body {
		t.Fatal("actual read-marked MT was not delivered:", result.err)
	}
	alias := result.request.From().Address.User
	_, binding, number, valid := parseCompactSMSReplyUser(alias)
	if !valid || len(alias) > 78 || binding != hashCallOperation(smsCaptureICCID) || number != message.Peer {
		t.Fatal("actual MT From did not carry its original compact card-bound reply")
	}
	_, accepted := smsCaptureSavedInbox(t, database, saved)
	if accepted.InboxID != before.InboxID || accepted.SourceKey != before.SourceKey || accepted.SourceFingerprint != before.SourceFingerprint ||
		accepted.Targets["1001"].Code != 200 || !validSMSAttemptToken(accepted.Targets["1001"].AttemptToken) {
		t.Fatal("actual MT result lost its original binding or durable attempt ownership")
	}
	phone.messageChallenge(alias, "synthetic actual original-card reply", "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(alias, "synthetic actual original-card reply", "text/plain", phone.messageAuthorization(alias), 2, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{message.DeviceID, message.Peer, "synthetic actual original-card reply"} {
		t.Fatal("the actual captured MT could not reply through its original card/device:", sent)
	}
}

func TestSMSCaptureOriginalRecipientGrantIsNotExpandedAndRevocationFencesBothRuntimeKinds(t *testing.T) {
	for _, independentManager := range []bool{false, true} {
		t.Run(fmt.Sprintf("independent-manager=%t", independentManager), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stores := smsCASStores(t)
			backend := newSyntheticSMS()
			backend.subscription = hashCallOperation(smsCaptureICCID)
			first := smsAuthorityStart(t, ctx, stores[0], backend)
			message := smsCaptureMessage("capture-original-recipient-before-change", "synthetic old original-recipient body")
			saved, err := stores[0].SaveSMSMessage(ctx, message)
			if err != nil {
				t.Fatal(err)
			}
			active := first
			if independentManager {
				active = smsAuthoritySecondManager(t, ctx, stores[1], backend)
			}
			changed := active.Snapshot().Config
			if independentManager {
				changed.Listen = unusedAddress(t)
			}
			for index := range changed.CarrierLines {
				changed.CarrierLines[index].SMSRecipients = []string{"1002"}
			}
			if _, err := active.Apply(ctx, changed); err != nil {
				t.Fatal(err)
			}
			// Same-manager save tests the new admitted configuration; a second
			// Manager tests a still-running old listener against newer durable ACLs.
			phone := registerMessagePeer(t, first.runtime, "1001")
			_, queued := smsCaptureSavedInbox(t, stores[0], saved)
			if len(queued.Targets) != 1 || queued.Targets["1001"].Code != 0 || queued.Targets["1002"].Identity != "" {
				t.Fatal("a later save appended a new recipient to the received source")
			}
			if err := first.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			_, rejected := smsCaptureSavedInbox(t, stores[0], saved)
			if rejected.Targets["1001"].Code != 403 || rejected.Targets["1001"].AttemptToken != "" || rejected.Targets["1002"].Identity != "" {
				t.Fatal("revoked original grant was claimed, expanded or not durably rejected")
			}
			smsCaptureNoUDP(t, phone)
			if backend.count() != 0 {
				t.Fatal("MT grant revocation invoked the carrier SMS sender")
			}
			// Regranting cannot revive that terminal old plan. A fresh source
			// proves the active transport and current original grant still work.
			restored := active.Snapshot().Config
			for index := range restored.CarrierLines {
				restored.CarrierLines[index].SMSRecipients = []string{"1001"}
			}
			if _, err := active.Apply(ctx, restored); err != nil {
				t.Fatal(err)
			}
			freshPhone := registerMessagePeer(t, active.runtime, "1001")
			smsCaptureFreshWireControl(t, ctx, stores[0], active, freshPhone, "capture-new-source-after-grant-restored")
		})
	}
}

func TestSMSCaptureActualInboxDeleteOrContentEditRejectsBeforeClaimAndWire(t *testing.T) {
	for _, mutation := range []string{"delete", "body", "direction"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, database, manager, backend := smsCaptureFixture(t)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			message := smsCaptureMessage("capture-source-mutated-before-claim", "synthetic original body must stay private")
			saved, err := database.SaveSMSMessage(ctx, message)
			if err != nil {
				t.Fatal(err)
			}
			_, original := smsCaptureSavedInbox(t, database, saved)
			if mutation == "delete" {
				err = database.DeleteSMSMessage(ctx, saved.ID)
			} else {
				changed := saved
				if mutation == "body" {
					changed.Body = "synthetic edited local history"
				} else {
					changed.Direction = "outbound"
				}
				_, err = database.SaveSMSMessage(ctx, changed)
			}
			if err != nil {
				t.Fatal(err)
			}
			if matches, err := database.SMSForwardSourceMatches(ctx, store.SMSForwardInboxGuard{ID: saved.ID, Fingerprint: original.SourceFingerprint}); err != nil || matches {
				t.Fatal("changed/deleted actual Inbox still matched the original guard:", err)
			}
			if err := manager.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			_, rejected := smsCaptureSavedInbox(t, database, saved)
			if rejected.Targets["1001"].Code != 403 || rejected.Targets["1001"].AttemptToken != "" {
				t.Fatal("source veto was not a pre-claim terminal rejection")
			}
			smsCaptureNoUDP(t, phone)
			if backend.count() != 0 {
				t.Fatal("a rejected MT source reached the paid SMS backend")
			}
			smsCaptureFreshWireControl(t, ctx, database, manager, phone, "capture-fresh-after-source-veto")
		})
	}
}

func TestSMSCaptureDisabledOrUngrantedSourceKeepsEmptyReceiptAfterInboxDeleteAndEnable(t *testing.T) {
	for _, condition := range []string{"disabled", "no-receive-grant"} {
		t.Run(condition, func(t *testing.T) {
			ctx, database, manager, _ := smsCaptureFixture(t)
			paused := manager.Snapshot().Config
			if condition == "disabled" {
				paused.SMSEnabled = false
			} else {
				for index := range paused.CarrierLines {
					paused.CarrierLines[index].AllowSMSReceive = false
					paused.CarrierLines[index].SMSRecipients = nil
				}
			}
			if _, err := manager.Apply(ctx, paused); err != nil {
				t.Fatal(err)
			}
			message := smsCaptureMessage("capture-empty-original-plan", "synthetic disabled/ungranted private source")
			saved, err := database.SaveSMSMessage(ctx, message)
			if err != nil {
				t.Fatal(err)
			}
			if state := smsCaptureRawLedger(t, database); len(state.Inbox) != 0 {
				t.Fatal("a disabled/ungranted receive created a payload plan")
			}
			if err := database.DeleteSMSMessage(ctx, saved.ID); err != nil {
				t.Fatal(err)
			}
			enabled := manager.Snapshot().Config
			enabled.SMSEnabled = true
			enabled.Accounts = []Account{{Username: "1003", Password: "synthetic-new-capture-password"}}
			for index := range enabled.CarrierLines {
				enabled.CarrierLines[index].AllowSMSReceive = true
				enabled.CarrierLines[index].SMSRecipients = []string{"1003"}
			}
			if _, err := manager.Apply(ctx, enabled); err != nil {
				t.Fatal(err)
			}
			if _, err := database.SaveSMSMessage(ctx, message); err != nil {
				t.Fatal(err)
			}
			if state := smsCaptureRawLedger(t, database); len(state.Inbox) != 0 {
				t.Fatal("deleted Inbox/source replay upgraded an empty plan to a new account")
			}
			fresh := smsCaptureMessage("capture-fresh-new-account-control", "synthetic fresh new-account source")
			current, err := database.SaveSMSMessage(ctx, fresh)
			if err != nil {
				t.Fatal(err)
			}
			_, accepted := smsCaptureSavedInbox(t, database, current)
			if len(accepted.Targets) != 1 || accepted.Targets["1003"].Identity == "" || accepted.Targets["1003"].Code != 0 {
				t.Fatal("fresh source did not acquire the new account's explicit current grant")
			}
		})
	}
}

func TestSMSCaptureCorruptRealLedgerRollsBackInboxAndReceiptThenSameSourceCanRecover(t *testing.T) {
	ctx, database, _, backend := smsCaptureFixture(t)
	corrupt := json.RawMessage(`{"version":99,"attempts":{},"inbox":{}}`)
	if err := database.UpsertAppSetting(ctx, store.AppSetting{Key: SMSLedgerSettingKey, Value: corrupt, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	message := smsCaptureMessage("capture-retry-after-atomic-rollback", "synthetic transaction rollback body")
	if _, err := database.SaveSMSMessage(ctx, message); err == nil {
		t.Fatal("corrupt actual builder state was treated as an empty queue")
	}
	if rows, err := database.ListSMSMessages(ctx, store.SMSFilter{}); err != nil || len(rows) != 0 {
		t.Fatal("builder failure committed the normal Inbox without its plan/receipt:", err)
	}
	value, err := database.AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil || !value.Sensitive || !bytes.Equal(value.Value, corrupt) {
		t.Fatal("failed receive rewrote the corrupt durable history:", err)
	}
	// Test-only repair of a deliberately empty malformed fixture; this is not
	// a production instruction to erase or retry unknown submission history.
	if changed, err := database.CompareSMSLedger(ctx, corrupt, nil); err != nil || !changed {
		t.Fatal("could not remove only the exact deliberately corrupt fixture:", err)
	}
	saved, err := database.SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	_, recovered := smsCaptureSavedInbox(t, database, saved)
	if recovered.InboxID != saved.ID || recovered.Targets["1001"].Code != 0 || len(recovered.Targets) != 1 {
		t.Fatal("rolled-back source receipt prevented a later genuine receive from acquiring its original plan")
	}
	if backend.count() != 0 {
		t.Fatal("atomic receive rollback/recovery submitted a carrier SMS")
	}
}

func TestSMSCaptureActualBuilderCapacityKeepsInboxAndEmptyReceiptWithoutLaterBackfill(t *testing.T) {
	ctx, database, manager, _ := smsCaptureFixture(t)
	// Seed a valid existing library-admitted queue in one durable write. The
	// capacity decision below must still run the REAL Manager planner through
	// Store.SaveSMSMessage, not a fake callback or direct pure-planner call.
	state := smsLedger{Version: 3, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{}}
	for index := 0; index < 256; index++ {
		id := fmt.Sprintf("existing-capacity-slot-%03d", index)
		reply := "+15550000001"
		state.Inbox[hashCallOperation(id)] = smsInbox{Message: IncomingSMS{ID: id, DeviceID: "module-a", Subscription: smsCaptureICCID,
			Sender: "+15550000001", Text: "synthetic existing queued payload"}, CanonicalReply: &reply, Until: time.Now().Add(time.Hour), SourceIndependent: true,
			Targets: map[string]smsRecipient{"1001": {Identity: recipientIdentity(manager.current, "1001")}}}
	}
	seed, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := database.CompareSMSLedger(ctx, nil, seed); err != nil || !changed {
		t.Fatal("could not durably seed the valid full queue:", err)
	}
	message := smsCaptureMessage("capture-while-real-builder-full", "synthetic locally retained capacity body")
	saved, err := database.SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal("forward queue capacity must not discard the normal Inbox:", err)
	}
	if actual, err := database.SMSMessage(ctx, saved.ID); err != nil || actual.Body != message.Body {
		t.Fatal("full queue lost its normal Inbox row:", err)
	}
	blockedKey := hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
	full := smsCaptureRawLedger(t, database)
	if _, exists := full.Inbox[blockedKey]; exists || len(full.Inbox) != 256 {
		t.Fatal("full actual builder invented a payload plan or lost an existing queue entry")
	}
	delete(full.Inbox, hashCallOperation("existing-capacity-slot-000"))
	released, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := database.CompareSMSLedger(ctx, seed, released); err != nil || !changed {
		t.Fatal("could not release one exact queue snapshot slot:", err)
	}
	if _, err := database.SaveSMSMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteSMSMessage(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SaveSMSMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if after := smsCaptureRawLedger(t, database); len(after.Inbox) != 255 || after.Inbox[blockedKey].Message.ID != "" {
		t.Fatal("freeing queue capacity or deleting Inbox replayed an original empty source receipt")
	}
	fresh := smsCaptureMessage("capture-fresh-after-real-capacity-release", "synthetic fresh capacity positive control")
	current, err := database.SaveSMSMessage(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	_, plan := smsCaptureSavedInbox(t, database, current)
	if len(smsCaptureRawLedger(t, database).Inbox) != 256 || plan.InboxID != current.ID || plan.SourceIndependent || plan.Targets["1001"].Code != 0 {
		t.Fatal("new source failed to use the released real planner capacity")
	}
}

func TestSMSCaptureLegacyUnboundVersionTwoQuarantinesQueuedAndKeepsUnknownWithoutReplay(t *testing.T) {
	ctx, database, manager, backend := smsCaptureFixture(t)
	queuedKey, unknownKey := hashCallOperation("old-unbound-queued"), hashCallOperation("old-unbound-unknown")
	until, token := time.Now().Add(time.Hour), strings.Repeat("e", 32)
	legacy := smsLedger{Version: 2, CursorInitialized: true, Cursor: 7, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{}}
	for key, code := range map[string]int{queuedKey: 0, unknownKey: 504} {
		entry := smsInbox{Message: IncomingSMS{ID: key, DeviceID: "module-a", Subscription: smsCaptureICCID, Sender: "+15550000001",
			Text: "synthetic old unbound private payload"}, Until: until,
			Targets: map[string]smsRecipient{"1001": {Identity: recipientIdentity(manager.current, "1001"), Code: code}}}
		if code == 504 {
			target := entry.Targets["1001"]
			target.AttemptToken = token
			entry.Targets["1001"] = target
		}
		legacy.Inbox[key] = entry
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"inbox_id"`)) || bytes.Contains(data, []byte(`"source_independent"`)) || bytes.Contains(data, []byte(`"source_fingerprint"`)) {
		t.Fatal("legacy fixture accidentally contains a new source authority")
	}
	if err := database.UpsertAppSetting(ctx, store.AppSetting{Key: SMSLedgerSettingKey, Value: data, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	if cursor, err := manager.SMSCursor(ctx, 0); err != nil || cursor != 7 {
		t.Fatal("legacy quarantine changed the cursor or could not commit:", err)
	}
	durable := smsCaptureRawLedger(t, database)
	if durable.Inbox[queuedKey].Targets["1001"].Code != 403 || durable.Inbox[queuedKey].Targets["1001"].AttemptToken != "" ||
		durable.Inbox[unknownKey].Targets["1001"].Code != 504 || durable.Inbox[unknownKey].Targets["1001"].AttemptToken != token ||
		!durable.Inbox[unknownKey].Until.Equal(until) {
		t.Fatal("old unbound queued/Unknown history was not conservatively persisted")
	}
	phone := registerMessagePeer(t, manager.runtime, "1001")
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	smsCaptureNoUDP(t, phone)
	if backend.count() != 0 {
		t.Fatal("legacy quarantine replayed an outgoing carrier submission")
	}
	smsCaptureFreshWireControl(t, ctx, database, manager, phone, "capture-fresh-after-legacy-quarantine")
}
