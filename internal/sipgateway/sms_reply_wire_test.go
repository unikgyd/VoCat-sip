package sipgateway

import (
	"context"
	"strings"
	"testing"
)

const smsReplyWireICCID = "8900000000000000001"

func TestSMSReplyWireLegacyAsteriskBoundaryCannotBecomeShorterPhone(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, func(config *stored) {
		config.Config.CarrierLines[1].DeviceID = "m"
		for i := range config.Config.Accounts {
			config.Config.Accounts[i].SMSSendLines = []string{"module-a", "m"}
		}
	})
	phone := newWireClient(t, m.runtime)
	full := legacySMSReplyAliasForCodecTest("m", smsReplyWireICCID, "+15550000002")
	if len(full) <= 79 {
		t.Fatal("legacy boundary fixture is not actually too long")
	}
	truncated := full[:79]
	phone.messageChallenge(truncated, "must not shorten recipient", "text/plain")
	requireSMSReplyWireRejected(t, phone.exchange(phone.messagePacket(truncated, "must not shorten recipient", "text/plain", phone.messageAuthorization(truncated), 2, "")))
	if backend.count() != 0 {
		t.Fatal("Asterisk boundary submitted to a syntactically valid but truncated phone number")
	}
	// Untruncated legacy input still works when the transport preserves it.
	requireStatus(t, phone.exchange(phone.messagePacket(full, "legacy positive control", "text/plain", phone.messageAuthorization(full), 3, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"m", "+15550000002", "legacy positive control"} {
		t.Fatal("legacy boundary guard broke its full-address positive control:", sent)
	}
}

func newSMSReplyWireTest(t *testing.T, change func(*stored)) (*Manager, *syntheticSMS) {
	t.Helper()
	database := carrierTestStore(t)
	backend := newSyntheticSMS()
	backend.subscription = hashCallOperation(smsReplyWireICCID)
	config := smsTestConfig(t)
	if change != nil {
		change(&config)
	}
	service := newSMSService(database, backend)
	r, err := start(config, runtimeOptions{sms: service})
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{db: database, current: config, runtime: r, sms: service}
	t.Cleanup(manager.Close)
	return manager, backend
}

func requireSMSReplyWireRejected(t *testing.T, response wireResponse) {
	t.Helper()
	// A challenge is not evidence that authenticated routing rejected the
	// alias. All these requests have valid Digest credentials already.
	if response.code < 400 || response.code >= 500 || response.code == 401 || response.code == 407 {
		t.Fatalf("authenticated reply route was not explicitly rejected: %s", response.raw)
	}
}

func TestSMSReplyWireReceivedCompactFromCanReplyOnlyThroughOriginalDeviceAndCard(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, nil)
	phone := registerMessagePeer(t, m.runtime, "1001")
	message := IncomingSMS{ID: "compact-reply-wire-mt", DeviceID: "module-b", Subscription: smsReplyWireICCID,
		Sender: "+15550000002", Text: "synthetic inbound reply source"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	delivery := replyOneMessage(phone.conn, 200)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual := <-delivery
	if actual.err != nil || actual.request == nil || string(actual.request.Body()) != message.Text {
		t.Fatal("actual MT source message was not delivered:", actual.err)
	}
	alias := actual.request.From().Address.User
	if !strings.HasPrefix(alias, "sms2-") || len(alias) > 78 || actual.request.From().DisplayName != message.Sender {
		t.Fatal("wire From did not use the compact original-card alias and caller display")
	}
	phone.messageChallenge(alias, "synthetic compact reply", "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(alias, "synthetic compact reply", "text/plain", phone.messageAuthorization(alias), 2, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"module-b", message.Sender, "synthetic compact reply"} {
		t.Fatal("reply did not reach its exact original line/number, or fell through the module-a default:", sent)
	}
	// An authenticated retry of the same logical operation must not submit a
	// second paid SMS merely because it uses a new Via branch/nonce count.
	requireStatus(t, phone.exchange(phone.messagePacket(alias, "synthetic compact reply", "text/plain", phone.messageAuthorization(alias), 2, "")), 202)
	if backend.count() != 1 {
		t.Fatal("compact reply retry duplicated the carrier operation")
	}
}

func TestSMSReplyWireMalformedReadOnlyUnknownAndTruncatedAliasesNeverUseDefaultLine(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, nil)
	phone := newWireClient(t, m.runtime)
	const defaultNumber = "+15550000001"
	phone.messageChallenge(defaultNumber, "default control before", "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(defaultNumber, "default control before", "text/plain", phone.messageAuthorization(defaultNumber), 2, "")), 202)
	valid := compactSMSReplyUser("module-b", smsReplyWireICCID, "+15550000002")
	longLegacy := legacySMSReplyAliasForCodecTest(strings.Repeat("a", 64), smsReplyWireICCID, "+15550000002")
	if len(longLegacy) <= 79 {
		t.Fatal("legacy truncation fixture is not longer than Asterisk's extension buffer")
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, valid[60])
	if last < 0 || last&3 != 0 {
		t.Fatal("invalid canonical compact fixture")
	}
	for index, target := range []string{
		"sms2-", "SMS2-invalid", "sms2", "sms-ro-" + strings.Repeat("a", 64), "SMS-old-unproven",
		"sms.incomplete", "SMS.bad", valid[:40], valid[:len(valid)-7],
		valid[:60] + string(alphabet[last+1]) + valid[61:],
		longLegacy[:79],
		compactSMSReplyUser("not-configured", smsReplyWireICCID, "+15550000002"),
		compactSMSReplyUser("module-b", "unproven-card", "+15550000002"),
	} {
		response := phone.exchange(phone.messagePacket(target, "must not use default", "text/plain", phone.messageAuthorization(target), 3+index, ""))
		requireSMSReplyWireRejected(t, response)
		if backend.count() != 1 {
			t.Fatalf("rejected target reached the still-enabled default carrier line: %q", target)
		}
	}
	// The valid default route remains usable; the negative result was not a
	// disabled/broken SMS backend that would trivially reject every request.
	requireStatus(t, phone.exchange(phone.messagePacket(defaultNumber, "default control after", "text/plain", phone.messageAuthorization(defaultNumber), 100, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 2 || sent[0] != [3]string{"module-a", defaultNumber, "default control before"} || sent[1] != [3]string{"module-a", defaultNumber, "default control after"} {
		t.Fatal("negative alias checks changed the enabled default route or secretly submitted another SMS:", sent)
	}
}

func TestSMSReplyWireChangedSIMRejectsOldCompactAndLegacySourcesBeforeBackend(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, nil)
	phone := newWireClient(t, m.runtime)
	compact := compactSMSReplyUser("module-b", smsReplyWireICCID, "+15550000002")
	legacy := legacySMSReplyAliasForCodecTest("module-b", smsReplyWireICCID, "+15550000002")
	phone.messageChallenge(compact, "old card reply", "text/plain")
	backend.mu.Lock()
	backend.subscription = hashCallOperation("8900000000000000002")
	backend.mu.Unlock()
	for index, target := range []string{compact, legacy} {
		requireStatus(t, phone.exchange(phone.messagePacket(target, "old card reply", "text/plain", phone.messageAuthorization(target), 2+index, "")), 403)
		if backend.count() != 0 {
			t.Fatal("an old source alias submitted through the replacement SIM")
		}
	}
	// The replacement SIM can handle a new conversation, but only using its
	// own binding and the exact original device selected by that new source.
	fresh := compactSMSReplyUser("module-b", "8900000000000000002", "+15550000002")
	requireStatus(t, phone.exchange(phone.messagePacket(fresh, "new card reply", "text/plain", phone.messageAuthorization(fresh), 4, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"module-b", "+15550000002", "new card reply"} {
		t.Fatal("replacement SIM control used a default/other device or replayed the old source:", sent)
	}
}

func TestSMSReplyWireCompleteLegacyAliasStillPreservesOriginalRouteAndCard(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, nil)
	phone := newWireClient(t, m.runtime)
	alias := legacySMSReplyAliasForCodecTest("module-b", smsReplyWireICCID, "+15550000002")
	phone.messageChallenge(alias, "synthetic legacy reply", "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(alias, "synthetic legacy reply", "text/plain", phone.messageAuthorization(alias), 2, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"module-b", "+15550000002", "synthetic legacy reply"} {
		t.Fatal("a complete legacy card-bound reply lost its exact original route:", sent)
	}
}

func TestSMSReplyWireOriginalLineACLDoesNotFallThroughToAuthorizedDefault(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, func(config *stored) {
		for index := range config.Config.Accounts {
			config.Config.Accounts[index].SMSSendLines = []string{"module-a"}
		}
	})
	phone := newWireClient(t, m.runtime)
	alias := compactSMSReplyUser("module-b", smsReplyWireICCID, "+15550000002")
	phone.messageChallenge(alias, "denied original line", "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(alias, "denied original line", "text/plain", phone.messageAuthorization(alias), 2, "")), 403)
	if backend.count() != 0 {
		t.Fatal("missing original-line SMS grant silently selected the authorized default")
	}
	const target = "+15550000001"
	requireStatus(t, phone.exchange(phone.messagePacket(target, "authorized default control", "text/plain", phone.messageAuthorization(target), 3, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"module-a", target, "authorized default control"} {
		t.Fatal("the explicitly authorized default control failed or another send escaped:", sent)
	}
}

func TestSMSReplyWireDeviceIDCaseDistinctionSelectsExactNonDefaultDevice(t *testing.T) {
	m, backend := newSMSReplyWireTest(t, func(config *stored) {
		config.Config.CarrierLines[1].DeviceID = "MODULE-A"
		for index := range config.Config.Accounts {
			config.Config.Accounts[index].SMSSendLines = []string{"module-a", "MODULE-A"}
		}
	})
	phone := newWireClient(t, m.runtime)
	upper := compactSMSReplyUser("MODULE-A", smsReplyWireICCID, "+15550000002")
	lower := compactSMSReplyUser("module-a", smsReplyWireICCID, "+15550000001")
	phone.messageChallenge(upper, "uppercase original device", "text/plain")
	requireStatus(t, phone.exchange(phone.messagePacket(upper, "uppercase original device", "text/plain", phone.messageAuthorization(upper), 2, "")), 202)
	requireStatus(t, phone.exchange(phone.messagePacket(lower, "lowercase original device", "text/plain", phone.messageAuthorization(lower), 3, "")), 202)
	if sent := backend.sentSnapshot(); len(sent) != 2 || sent[0] != [3]string{"MODULE-A", "+15550000002", "uppercase original device"} || sent[1] != [3]string{"module-a", "+15550000001", "lowercase original device"} {
		t.Fatal("case-sensitive physical device selection was folded into the default route:", sent)
	}
}

func TestSMSReplyRouteDuplicateHashCandidatesAreAmbiguousNotDefault(t *testing.T) {
	config := smsTestConfig(t).Config
	// Duplicate device IDs are rejected by normal configuration admission.
	// Exercise the route resolver's own multiple-candidate guard as a pure
	// corrupted-document fixture; do not brute-force a cryptographic collision.
	config.CarrierLines = append(config.CarrierLines, config.CarrierLines[1])
	alias := compactSMSReplyUser("module-b", smsReplyWireICCID, "+15550000002")
	if _, _, code := smsRoute(config, config.Accounts[0], alias); code < 400 || code >= 500 {
		t.Fatal("multiple original-device hash candidates did not fail closed:", code)
	}
}
