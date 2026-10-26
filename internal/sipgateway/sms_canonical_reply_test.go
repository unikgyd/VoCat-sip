package sipgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

const smsCanonicalICCID = "8900000000000000001"
const smsCanonicalNational = "15550000001"

// These tests deliberately use only the already accepted v2 APIs and fields.
// They can be copied alone onto the preceding source archive to obtain a real
// behavioral RED, rather than a missing-v3-symbol or compilation failure.
func smsCanonicalStart(t *testing.T, ctx context.Context, database *store.Store, backend *syntheticSMS) *Manager {
	t.Helper()
	manager := New(ctx, database, WithCarrierSMSBackend(backend))
	t.Cleanup(manager.Close)
	config := smsAuthorityConfig(t)
	config.CarrierLines[0].CountryCode = "86"
	config.CarrierLines[0].NationalNumberLength = 11
	view, err := manager.Apply(ctx, config)
	if err != nil || !view.Status.Running || manager.runtime == nil {
		t.Fatal("could not start the original real Manager and durable country policy:", err)
	}
	return manager
}

func smsCanonicalPlan(t *testing.T, database *store.Store, key string) smsInbox {
	t.Helper()
	state := smsCASReadLedger(t, database)
	in, exists := state.Inbox[key]
	if !exists || len(in.Targets) != 1 || in.Targets["1001"].Identity == "" {
		t.Fatal("real admission did not preserve its one original recipient plan")
	}
	return in
}

func smsCanonicalNoUDP(t *testing.T, phone *wireClient) {
	t.Helper()
	if err := phone.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("the CAS-losing runtime also exposed the queued private SMS")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("could not verify the still-registered losing receiver:", err)
	}
}

func smsCanonicalDeliver(t *testing.T, ctx context.Context, manager *Manager, phone *wireClient, body string) *sip.Request {
	t.Helper()
	delivery := replyOneMessage(phone.conn, 200)
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Fatal("real queued delivery failed:", err)
	}
	actual := <-delivery
	if actual.err != nil || actual.request == nil || actual.request.From() == nil || string(actual.request.Body()) != body {
		t.Fatal("real registered UDP receiver did not accept the original SMS body:", actual.err)
	}
	return actual.request
}

func smsCanonicalReply(t *testing.T, phone *wireClient, request *sip.Request, wantNumber, body string, cseq int) {
	t.Helper()
	alias := request.From().Address.User
	_, binding, number, valid := parseCompactSMSReplyUser(alias)
	if !valid || binding != hashCallOperation(smsCanonicalICCID) || len(alias) > 78 {
		t.Fatal("actual MT did not expose a valid compact original-card reply address")
	}
	if number != wantNumber {
		t.Errorf("delivery reinterpreted the original sender using the claimant's country policy: got %q, want frozen %q", number, wantNumber)
	}
	phone.messageChallenge(alias, body, "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(alias, body, "text/plain", phone.messageAuthorization(alias), cseq, "")), 202)
}

func smsCanonicalCountryChange(t *testing.T, admission string) {
	t.Helper()
	for _, newRuntimeWins := range []bool{false, true} {
		t.Run(fmt.Sprintf("new-runtime-wins=%t", newRuntimeWins), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stores := smsCASStores(t)
			backend := newSyntheticSMS()
			backend.subscription = hashCallOperation(smsCanonicalICCID)
			first := smsCanonicalStart(t, ctx, stores[0], backend)
			original := IncomingSMS{ID: "canonical-original-" + admission, DeviceID: "module-a", Subscription: smsCanonicalICCID,
				Sender: smsCanonicalNational, Text: "synthetic original national sender body"}
			actualMessage := store.SMSMessage{MessageID: original.ID, DeviceID: original.DeviceID, ModemIMEI: "860000000000001",
				ICCID: smsCanonicalICCID, IMSI: "001010000000001", Peer: original.Sender, Direction: "inbound",
				Body: original.Text, Source: "ims", PartsTotal: 1}
			key := hashCallOperation(original.DeviceID + "\x00" + original.Subscription + "\x00" + original.ID)
			var saved store.SMSMessage
			if admission == "atomic-store" {
				var err error
				saved, err = stores[0].SaveSMSMessage(ctx, actualMessage)
				if err != nil {
					t.Fatal(err)
				}
				key = hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
			} else if err := first.QueueSMS(ctx, 1, &original); err != nil {
				t.Fatal(err)
			}
			beforePlan := smsCanonicalPlan(t, stores[0], key)
			if beforePlan.Message.Sender != original.Sender || beforePlan.Targets["1001"].Code != 0 ||
				beforePlan.SourceIndependent != (admission == "explicit-queue") {
				t.Fatal("offline original admission did not retain its raw sender and source authority")
			}
			if admission == "atomic-store" && (beforePlan.InboxID != saved.ID || beforePlan.SourceKey != store.SMSForwardSourceKey(saved) ||
				beforePlan.SourceFingerprint != store.SMSForwardFingerprint(saved)) {
				t.Fatal("atomic original source was not bound to its real durable Inbox")
			}
			beforeConfig := smsAuthoritySaved(t, stores[0])
			second := smsAuthoritySecondManager(t, ctx, stores[1], backend)
			changed := second.Snapshot().Config
			changed.Listen = unusedAddress(t)
			changed.CarrierLines[0].CountryCode = "63"
			changed.CarrierLines[0].NationalNumberLength = 11
			view, err := second.Apply(ctx, changed)
			if err != nil || !view.Status.Running || second.runtime == nil {
				t.Fatal("could not start the new independent Manager with its changed country policy:", err)
			}
			afterConfig := smsAuthoritySaved(t, stores[1])
			if recipientIdentity(beforeConfig, "1001") != recipientIdentity(afterConfig, "1001") ||
				beforePlan.Targets["1001"].Identity != recipientIdentity(afterConfig, "1001") ||
				first.runtime.closed.Load() || first.runtime.config.Config.CarrierLines[0].CountryCode != "86" ||
				second.runtime.config.Config.CarrierLines[0].CountryCode != "63" ||
				!second.runtime.config.Config.CarrierLines[0].AllowSMSReceive {
				t.Fatal("country-policy update reset an epoch/grant or failed to leave both different admitted runtimes active")
			}
			// Re-observing the same physical source, or explicitly admitting the
			// same library event again, must not recompute its original reply.
			if admission == "atomic-store" {
				if _, err := stores[1].SaveSMSMessage(ctx, actualMessage); err != nil {
					t.Fatal(err)
				}
			} else if err := second.QueueSMS(ctx, 2, &original); err != nil {
				t.Fatal(err)
			}
			oldPhone := registerMessagePeer(t, first.runtime, "1001")
			newPhone := registerMessagePeer(t, second.runtime, "1001")
			winner, loser := first, second
			winnerPhone, loserPhone := oldPhone, newPhone
			loserStore := stores[1]
			if newRuntimeWins {
				winner, loser = second, first
				winnerPhone, loserPhone = newPhone, oldPhone
				loserStore = stores[0]
			}
			reads := make(chan struct{}, 1)
			gate := newSMSCASRelease()
			defer gate.close()
			// Pause only after a REAL read of the old queued snapshot immediately
			// before the losing claim CAS. Neither SQL results nor writes are faked.
			loser.sms.db = &smsCASPausedRead{Store: loserStore, readNumber: 3, arrived: reads, release: gate.ch}
			loserDone := make(chan error, 1)
			go func() { loserDone <- loser.DeliverSMSOnce(ctx) }()
			smsCASWait(t, ctx, reads, 1, "the selected losing Manager's actual pre-claim SQLite read")
			request := smsCanonicalDeliver(t, ctx, winner, winnerPhone, original.Text)
			gate.close()
			select {
			case err := <-loserDone:
				if err != nil {
					t.Fatal("losing real CAS worker could not observe the accepted winner:", err)
				}
			case <-ctx.Done():
				t.Fatal("losing real CAS worker did not terminate:", ctx.Err())
			}
			smsCanonicalNoUDP(t, loserPhone)
			afterPlan := smsCanonicalPlan(t, stores[1], key)
			if afterPlan.Message.Sender != original.Sender || afterPlan.Targets["1001"].Code != 200 ||
				!validSMSAttemptToken(afterPlan.Targets["1001"].AttemptToken) || !smsInboxSameSource(beforePlan, afterPlan) {
				t.Fatal("winner completion changed the original raw sender/source or lost its owned accepted result")
			}
			if request.From().DisplayName != original.Sender {
				t.Fatal("canonical reply policy changed the original sender display")
			}
			const originalReplyBody = "synthetic reply must retain the original country"
			smsCanonicalReply(t, winnerPhone, request, "+86"+smsCanonicalNational, originalReplyBody, 2)
			// A distinct source saved under the new policy proves this is a
			// per-source snapshot, not a hard-coded preference for the old +86.
			fresh := original
			fresh.ID, fresh.Text = "canonical-new-source-"+admission, "synthetic fresh national sender body"
			if admission == "atomic-store" {
				actualMessage.MessageID, actualMessage.Body = fresh.ID, fresh.Text
				if _, err := stores[1].SaveSMSMessage(ctx, actualMessage); err != nil {
					t.Fatal(err)
				}
			} else if err := second.QueueSMS(ctx, 3, &fresh); err != nil {
				t.Fatal(err)
			}
			freshRequest := smsCanonicalDeliver(t, ctx, second, newPhone, fresh.Text)
			const freshReplyBody = "synthetic fresh reply must use the new country"
			smsCanonicalReply(t, newPhone, freshRequest, "+63"+smsCanonicalNational, freshReplyBody, 3)
			sent := backend.sentSnapshot()
			if len(sent) != 2 || sent[0] != [3]string{original.DeviceID, "+86" + smsCanonicalNational, originalReplyBody} ||
				sent[1] != [3]string{original.DeviceID, "+63" + smsCanonicalNational, freshReplyBody} {
				t.Errorf("actual Digest replies did not retain each source's original card/device/number, or duplicated a paid operation: %v", sent)
			}
		})
	}
}

func TestSMSCanonicalReplyAtomicStoreFreezesCountryBeforeEitherIndependentManagerClaims(t *testing.T) {
	smsCanonicalCountryChange(t, "atomic-store")
}

func TestSMSCanonicalReplyExplicitQueueFreezesFirstAdmissionBeforeEitherIndependentManagerClaims(t *testing.T) {
	smsCanonicalCountryChange(t, "explicit-queue")
}

func TestSMSCanonicalReplyLegacyHistoryUsesOnlyStrictInternationalSenderAndNeverRevivesUnknown(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, source := range []struct{ name, sender, wantNumber string }{
			{"international", "+15550000001", "+15550000001"},
			{"national", smsCanonicalNational, ""},
			{"short", "12345", ""},
			{"alphanumeric", "SYNTHETIC-BANK", ""},
		} {
			t.Run(fmt.Sprintf("v%d/%s", version, source.name), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				database := carrierTestStore(t)
				backend := newSyntheticSMS()
				backend.subscription = hashCallOperation(smsCanonicalICCID)
				manager := smsCanonicalStart(t, ctx, database, backend)
				phone := registerMessagePeer(t, manager.runtime, "1001")
				key, unknownKey := hashCallOperation("legacy-original-source"), hashCallOperation("legacy-unknown-source")
				operation, fingerprint := hashCallOperation("legacy-unknown-MO"), hashCallOperation("legacy-unknown-MO-payload")
				until := time.Now().UTC().Add(time.Hour)
				token := ""
				if version == 2 {
					token = strings.Repeat("c", 32)
				}
				legacy := smsLedger{Version: version, CursorInitialized: true, Cursor: 17,
					Attempts: map[string]smsAttempt{operation: {Fingerprint: fingerprint, Code: 504, Until: until, AttemptToken: token}},
					Inbox:    map[string]smsInbox{}}
				legacy.Inbox[key] = smsInbox{Message: IncomingSMS{ID: "legacy-original-source", DeviceID: "module-a",
					Subscription: smsCanonicalICCID, Sender: source.sender, Text: "synthetic legacy source body"}, Until: until, SourceIndependent: true,
					Targets: map[string]smsRecipient{"1001": {Identity: recipientIdentity(manager.current, "1001")}}}
				unknown := legacy.Inbox[key]
				unknown.Message.ID = "legacy-unknown-source"
				unknown.Targets = map[string]smsRecipient{"1001": {Identity: recipientIdentity(manager.current, "1001"), Code: 504, AttemptToken: token}}
				legacy.Inbox[unknownKey] = unknown
				data, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpsertAppSetting(ctx, store.AppSetting{Key: SMSLedgerSettingKey, Value: data, Sensitive: true}); err != nil {
					t.Fatal(err)
				}
				request := smsCanonicalDeliver(t, ctx, manager, phone, legacy.Inbox[key].Message.Text)
				if request.From().DisplayName != source.sender {
					t.Fatal("legacy migration changed the original sender display")
				}
				alias := request.From().Address.User
				const replyBody = "synthetic legacy reply safety control"
				phone.messageChallenge(alias, replyBody, "text/plain")
				response := phone.exchange(phone.messagePacket(alias, replyBody, "text/plain", phone.messageAuthorization(alias), 2, ""))
				if source.wantNumber == "" {
					if response.code < 400 || response.code >= 500 || response.code == 401 || response.code == 407 || backend.count() != 0 {
						t.Errorf("legacy raw sender acquired a paid reply by guessing today's country instead of remaining read-only: sender=%q code=%d sends=%v", source.sender, response.code, backend.sentSnapshot())
					}
				} else {
					requireStatus(t, response, 202)
					if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"module-a", source.wantNumber, replyBody} {
						t.Fatal("strict international legacy positive control lost its exact original card/number:", sent)
					}
				}
				durable := smsCASReadLedger(t, database)
				mtUnknown, moUnknown := durable.Inbox[unknownKey], durable.Attempts[operation]
				if durable.Cursor != 17 || !durable.CursorInitialized || mtUnknown.Targets["1001"].Code != 504 ||
					!mtUnknown.Until.Equal(until) || !validSMSAttemptToken(mtUnknown.Targets["1001"].AttemptToken) ||
					moUnknown.Code != 504 || !moUnknown.Until.Equal(until) || !validSMSAttemptToken(moUnknown.AttemptToken) ||
					version == 2 && (mtUnknown.Targets["1001"].AttemptToken != token || moUnknown.AttemptToken != token) {
					t.Fatal("legacy canonical migration revived Unknown, changed its expiry/token, or moved the original cursor")
				}
				beforeCount := backend.count()
				if code := manager.sms.attempt(ctx, operation, fingerprint, "module-a", "+15550000001", "synthetic old unknown body", ""); code != 504 || backend.count() != beforeCount {
					t.Fatal("legacy MO Unknown became safe to submit again")
				}
				if err := manager.DeliverSMSOnce(ctx); err != nil {
					t.Fatal(err)
				}
				smsCanonicalNoUDP(t, phone)
			})
		}
	}
}
