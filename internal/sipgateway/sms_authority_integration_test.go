package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"vocat/internal/store"
)

// Actual Manager admission, actual file-backed SQLite, and actual loopback SIP.
// All addresses, identities and message bodies are synthetic. No real device,
// external network, carrier submission or production settings are involved.
func smsAuthorityConfig(t *testing.T) Config {
	t.Helper()
	config := smsTestConfig(t).Config
	config.Listen = unusedAddress(t)
	for i := range config.Accounts {
		config.Accounts[i].Password = testPassword
	}
	for i := range config.CarrierLines {
		config.CarrierLines[i].SMSRecipients = []string{"1001"}
	}
	return config
}

func smsAuthoritySaved(t *testing.T, db *store.Store) stored {
	t.Helper()
	value, err := db.AppSetting(context.Background(), SettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var config stored
	if !value.Sensitive || json.Unmarshal(value.Value, &config) != nil || !validLocalCredentialEpoch(config.ConfigRevision) {
		t.Fatal("durable manager configuration was not valid private settings")
	}
	return config
}

func smsAuthorityStart(t *testing.T, ctx context.Context, db *store.Store, backend CarrierSMSBackend) *Manager {
	t.Helper()
	m := New(ctx, db, WithCarrierSMSBackend(backend))
	t.Cleanup(m.Close)
	view, err := m.Apply(ctx, smsAuthorityConfig(t))
	if err != nil || !view.Status.Running || m.runtime == nil || !m.runtime.config.requireDurableAuthority {
		t.Fatal("actual Manager could not admit its durable SMS runtime:", err)
	}
	return m
}

func smsAuthoritySecondManager(t *testing.T, ctx context.Context, db *store.Store, backend CarrierSMSBackend) *Manager {
	t.Helper()
	// The first manager owns the saved port. New must still durably confirm a
	// fresh revision before attempting the listener, which deliberately fails.
	m := New(ctx, db, WithCarrierSMSBackend(backend))
	t.Cleanup(m.Close)
	if m.runtime != nil || m.Snapshot().Status.Error == "" || !m.current.requireDurableAuthority {
		t.Fatal("second Manager did not expose the expected occupied-listener startup")
	}
	return m
}

func TestSMSAuthorityActualManagersRevisionChangeKeepsSameEpochWireMOAndMTWorking(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := &smsCASBackend{result: smsCASAccepted()}
	first := smsAuthorityStart(t, ctx, stores[0], backend)
	peer := registerMessagePeer(t, first.runtime, "1001")
	message := IncomingSMS{ID: "same-epoch-queued-before-revision", DeviceID: "module-a", Subscription: "synthetic-sim",
		Sender: "+15550000001", Text: "synthetic manager MT body"}
	if err := first.QueueSMS(ctx, 1, &message); err != nil {
		t.Fatal(err)
	}
	before := smsAuthoritySaved(t, stores[0])
	_ = smsAuthoritySecondManager(t, ctx, stores[1], backend)
	after := smsAuthoritySaved(t, stores[1])
	if before.ConfigRevision == after.ConfigRevision || recipientIdentity(before, "1001") != recipientIdentity(after, "1001") {
		t.Fatal("second Manager must change the durable revision without changing the existing account epoch")
	}
	if first.runtime.closed.Load() {
		t.Fatal("first runtime unexpectedly stopped instead of checking the current durable authority")
	}

	caller := newWireClient(t, first.runtime)
	target, body := "+15550000001", "synthetic same-epoch MO body"
	caller.messageChallenge(target, body, "text/plain")
	requireStatus(t, caller.exchange(caller.messagePacket(target, body, "text/plain", caller.messageAuthorization(target), 2, "")), 202)
	if backend.calls.Load() != 1 {
		t.Fatal("revision-only change incorrectly blocked or duplicated the actual SIP MO")
	}
	delivery := replyOneMessage(peer.conn, 200)
	if err := first.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if result := <-delivery; result.err != nil || result.request == nil || !bytes.Equal(result.request.Body(), []byte(message.Text)) {
		t.Fatal("actual same-epoch MT delivery failed")
	}
	ledger := smsCASReadLedger(t, stores[1])
	if ledger.Version != 3 || len(ledger.Attempts) != 1 || len(ledger.Inbox) != 1 {
		t.Fatal("actual Manager wire operations were not durably recorded")
	}
	for _, attempt := range ledger.Attempts {
		if attempt.Code != 202 || !validSMSAttemptToken(attempt.AttemptToken) {
			t.Fatal("actual MO did not retain a token-bound accepted result")
		}
	}
	for _, inbox := range ledger.Inbox {
		if target := inbox.Targets["1001"]; target.Code != 200 || !validSMSAttemptToken(target.AttemptToken) {
			t.Fatal("actual MT did not retain its own token-bound final result")
		}
	}
}

func TestSMSAuthorityActualManagerResetFencesOldRuntimePaidAndQueuedMT(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := &smsCASBackend{result: smsCASAccepted()}
	first := smsAuthorityStart(t, ctx, stores[0], backend)
	oldRuntime := first.runtime
	oldPeer := registerMessagePeer(t, oldRuntime, "1001")
	message := IncomingSMS{ID: "queued-before-same-password-reset", DeviceID: "module-a", Subscription: "synthetic-sim",
		Sender: "+15550000001", Text: "synthetic old-epoch private MT body"}
	if err := first.QueueSMS(ctx, 1, &message); err != nil {
		t.Fatal(err)
	}
	before := smsAuthoritySaved(t, stores[0])
	second := smsAuthoritySecondManager(t, ctx, stores[1], backend)
	reset := second.Snapshot().Config
	reset.Listen = unusedAddress(t)
	for i := range reset.Accounts {
		if reset.Accounts[i].Password != "" {
			t.Fatal("public configuration unexpectedly disclosed a password")
		}
		if reset.Accounts[i].Username == "1001" {
			reset.Accounts[i].ResetCredentialIdentity = true
		}
	}
	view, err := second.Apply(ctx, reset)
	if err != nil || !view.Status.Running || second.runtime == nil {
		t.Fatal("second Manager could not start its durably reset account:", err)
	}
	after := smsAuthoritySaved(t, stores[1])
	if recipientIdentity(before, "1001") == recipientIdentity(after, "1001") || before.Secrets["1001"] != after.Secrets["1001"] {
		t.Fatal("explicit reset must change the epoch without changing the preserved password digest")
	}
	if first.runtime != oldRuntime || oldRuntime.closed.Load() {
		t.Fatal("test requires an old admitted runtime to remain active against newer durable settings")
	}
	if err := first.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ledger := smsCASReadLedger(t, stores[1])
	key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
	if target := ledger.Inbox[key].Targets["1001"]; target.Code != 403 || target.AttemptToken != "" {
		t.Fatal("old queued MT was not rejected before a transport claim")
	}
	if err := oldPeer.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := oldPeer.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("old Contact received private MT after the durable credential reset")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("could not verify the still-online old Contact:", err)
	}

	// The old listener still authenticates the unchanged password. A 403 (not
	// 401) therefore demonstrates fresh durable authority rejection after auth.
	caller := newWireClient(t, oldRuntime)
	target, body := "+15550000001", "synthetic revoked MO body"
	caller.messageChallenge(target, body, "text/plain")
	requireStatus(t, caller.exchange(caller.messagePacket(target, body, "text/plain", caller.messageAuthorization(target), 2, "")), 403)
	if backend.calls.Load() != 0 {
		t.Fatal("old runtime reached the carrier after another Manager reset its authority")
	}
	ledger = smsCASReadLedger(t, stores[1])
	if len(ledger.Attempts) != 0 {
		t.Fatal("pre-claim authority rejection invented a paid SMS attempt")
	}
}

func TestSMSAuthorityLegacyVersionOneUnknownMigratesWithoutCarrierReplay(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	operation, fingerprint := hashCallOperation("legacy-unknown-operation"), hashCallOperation("legacy-private-fingerprint")
	until := time.Now().UTC().Add(time.Hour)
	legacy := smsLedger{Version: 1, Attempts: map[string]smsAttempt{
		operation: {Fingerprint: fingerprint, Code: 504, Until: until},
	}, Inbox: map[string]smsInbox{}}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores[0].UpsertAppSetting(ctx, store.AppSetting{Key: SMSLedgerSettingKey, Value: data, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	backend := &smsCASBackend{result: smsCASAccepted()}
	manager := New(ctx, stores[1], WithCarrierSMSBackend(backend))
	t.Cleanup(manager.Close)
	if _, err := manager.SMSCursor(ctx, 0); err != nil {
		t.Fatal(err)
	}
	migrated := smsCASReadLedger(t, stores[0])
	if migrated.Version != 3 || migrated.Attempts[operation].Code != 504 ||
		!migrated.Attempts[operation].Until.Equal(until) || !validSMSAttemptToken(migrated.Attempts[operation].AttemptToken) {
		t.Fatal("legacy Unknown was not conservatively migrated with its unchanged outcome and expiry")
	}
	for _, db := range stores {
		if code := newSMSService(db, backend).attempt(ctx, operation, fingerprint, "module-a", "+15550000001", "synthetic body", ""); code != 504 {
			t.Fatal("legacy Unknown became safe to replay:", code)
		}
	}
	if backend.calls.Load() != 0 {
		t.Fatal("legacy migration replayed a possibly submitted SMS")
	}
}

func TestSMSAuthorityVersionTwoMissingAttemptTokenFailsClosedWithoutChangingRawHistory(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	corrupt := smsLedger{Version: 2, Attempts: map[string]smsAttempt{
		hashCallOperation("corrupt-no-token"): {Fingerprint: hashCallOperation("corrupt-fingerprint"), Code: 504, Until: time.Now().Add(time.Hour)},
	}, Inbox: map[string]smsInbox{}}
	data, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores[0].UpsertAppSetting(ctx, store.AppSetting{Key: SMSLedgerSettingKey, Value: data, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	before, err := stores[0].AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	backend := &smsCASBackend{result: smsCASAccepted()}
	service := newSMSService(stores[1], backend)
	if code := service.attempt(ctx, hashCallOperation("must-not-be-admitted"), hashCallOperation("new-fingerprint"),
		"module-a", "+15550000001", "synthetic body", ""); code != 503 || backend.calls.Load() != 0 {
		t.Fatal("corrupt token history was ignored instead of failing closed")
	}
	after, err := stores[0].AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil || !bytes.Equal(before.Value, after.Value) || !after.Sensitive {
		t.Fatal("failed admission rewrote corrupt history as an empty/new ledger")
	}
}
