package sipgateway

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

//go:embed testdata/sip-sms-operation-v1.json
var smsOperationContract []byte

func TestSMSSharedOperationContract(t *testing.T) {
	var fixture struct {
		Version int
		Cases   []struct {
			Realm, Account, CallID, OperationID string
			CSeq                                uint32
		}
	}
	if json.Unmarshal(smsOperationContract, &fixture) != nil || fixture.Version != 1 || len(fixture.Cases) != 3 {
		t.Fatal("invalid shared contract")
	}
	for _, c := range fixture.Cases {
		if got := smsOperationID(c.Realm, c.Account, c.CallID, c.CSeq); got != c.OperationID {
			t.Fatalf("operation mismatch %s", got)
		}
	}
}

type syntheticSMS struct {
	mu           sync.Mutex
	sent         [][3]string
	subscription string
	result       vowifi.SMSSubmitResult
	err          error
	wait         <-chan struct{}
	entered      chan struct{}
}

func newSyntheticSMS() *syntheticSMS {
	return &syntheticSMS{subscription: hashCallOperation("8901000000000000001"), result: vowifi.SMSSubmitResult{PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true}}
}
func (b *syntheticSMS) SMSReady(context.Context, string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscription, nil
}
func (b *syntheticSMS) SendCarrierSMS(_ context.Context, id, number, text, op, subscription string) (vowifi.SMSSubmitResult, error) {
	b.mu.Lock()
	b.sent = append(b.sent, [3]string{id, number, text})
	wait, entered := b.wait, b.entered
	b.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if wait != nil && id == "module-a" {
		<-wait
	} // Intentionally ignores cancellation.
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, b.err
}
func (b *syntheticSMS) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.sent) }
func (b *syntheticSMS) sentSnapshot() [][3]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][3]string(nil), b.sent...)
}
func smsTestConfig(t *testing.T) stored {
	c := carrierTestConfig(t)
	c.Config.CallsEnabled = false
	c.Config.CarrierEnabled = false
	c.Config.MessagesEnabled = false
	c.Config.SMSEnabled = true
	for i := range c.Config.CarrierLines {
		c.Config.CarrierLines[i].AllowSMSSend = true
		c.Config.CarrierLines[i].AllowSMSReceive = true
		c.Config.CarrierLines[i].SMSRecipients = []string{"1001", "1002"}
	}
	for i := range c.Config.Accounts {
		c.Config.Accounts[i].AllowCalls = false
		c.Config.Accounts[i].AllowMessages = false
		c.Config.Accounts[i].OutgoingLines = nil
		c.Config.Accounts[i].SMSSendLines = []string{"module-a", "module-b"}
	}
	return c
}
func newSMSTest(t *testing.T) (*Manager, *syntheticSMS) {
	t.Helper()
	db := carrierTestStore(t)
	backend := newSyntheticSMS()
	service := newSMSService(db, backend)
	config := smsTestConfig(t)
	r, err := start(config, runtimeOptions{sms: service})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{db: db, current: config, runtime: r, sms: service}
	t.Cleanup(m.Close)
	return m, backend
}
func TestSMSWireIndependentPermissionsAndKnownExtensionPriority(t *testing.T) {
	m, b := newSMSTest(t)
	r := m.runtime
	c := newWireClient(t, r)
	c.messageChallenge("+15550000001", "hello 手机", "text/plain; charset=utf-8")
	packet := c.messagePacket("+15550000001", "hello 手机", "text/plain", c.messageAuthorization("+15550000001"), 2, "")
	response := c.exchange(packet)
	requireStatus(t, response, 202)
	if len(response.headers["allow"]) != 1 || !strings.Contains(response.headers["allow"][0], "MESSAGE") {
		t.Fatal("SMS-only listener did not advertise MESSAGE")
	}
	packet = c.messagePacket("+15550000001", "hello 手机", "text/plain", c.messageAuthorization("+15550000001"), 2, "")
	requireStatus(t, c.exchange(packet), 202)
	packet = c.messagePacket("+15550000001", "changed", "text/plain", c.messageAuthorization("+15550000001"), 2, "")
	requireStatus(t, c.exchange(packet), 400)
	packet = c.messagePacket("1002", "not a mobile number", "text/plain", c.messageAuthorization("1002"), 3, "")
	requireStatus(t, c.exchange(packet), 405)
	if b.count() != 1 {
		t.Fatal("internal message/duplicate caused a paid send", b.count())
	}
	if got := b.sentSnapshot()[0]; got != [3]string{"module-a", "+15550000001", "hello 手机"} {
		t.Fatal(got)
	}
	if !r.status().Capabilities.CarrierSMS || r.status().Capabilities.Messages || r.status().Capabilities.Calls {
		t.Fatal("SMS coupled to calls/internal messages")
	}
}
func TestSMSDurableRetryUnknownPartialAndCorruptStore(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			db := carrierTestStore(t)
			b := newSyntheticSMS()
			if partial {
				b.mu.Lock()
				b.result.AllPartsAccepted = false
				b.result.PartsTotal = 2
				b.err = errors.New("synthetic transport failure")
				b.mu.Unlock()
			}
			s := newSMSService(db, b)
			key, fp := hashCallOperation("op"), hashCallOperation("body")
			want := 202
			if partial {
				want = 504
			}
			if code := s.attempt(context.Background(), key, fp, "module-a", "+15550000001", "body", ""); code != want {
				t.Fatal(code)
			}
			reopened := newSMSService(db, b)
			if code := reopened.attempt(context.Background(), key, fp, "module-a", "+15550000001", "body", ""); code != want || b.count() != 1 {
				t.Fatal("replayed paid operation", code, b.count())
			}
			if code := reopened.attempt(context.Background(), key, hashCallOperation("changed"), "module-a", "+15550000001", "changed", ""); code != 400 {
				t.Fatal(code)
			}
			if err := db.UpsertAppSetting(context.Background(), store.AppSetting{Key: SMSLedgerSettingKey, Value: []byte(`{"version":99}`)}); err != nil {
				t.Fatal(err)
			}
			if code := reopened.attempt(context.Background(), hashCallOperation("new"), fp, "module-a", "+15550000001", "body", ""); code != 503 || b.count() != 1 {
				t.Fatal("corrupt journal did not fail closed")
			}
			if reopened.statistics().Error == "" {
				t.Fatal("corrupt journal was not visible in status")
			}
		})
	}
}
func TestSMSReplyPinsModuleAndSubscription(t *testing.T) {
	m, b := newSMSTest(t)
	alias := smsReplyUser("module-b", "8901000000000000001", "+15550000002")
	line, number, code := smsRoute(m.current.Config, m.current.Config.Accounts[0], alias)
	if code != 0 || line.DeviceID != "module-b" || number != "+15550000002" {
		t.Fatal(line, number, code)
	}
	c := newWireClient(t, m.runtime)
	c.messageChallenge(alias, "reply", "text/plain")
	requireStatus(t, c.exchange(c.messagePacket(alias, "reply", "text/plain", c.messageAuthorization(alias), 2, "")), 202)
	if b.sentSnapshot()[0][0] != "module-b" {
		t.Fatal("reply sent through default SIM")
	}
	b.mu.Lock()
	b.subscription = hashCallOperation("different-sim")
	b.mu.Unlock()
	requireStatus(t, c.exchange(c.messagePacket(alias, "new reply", "text/plain", c.messageAuthorization(alias), 3, "")), 403)
	if b.count() != 1 {
		t.Fatal("reply crossed SIM replacement")
	}
}
func TestSMSBlockedModemDoesNotBlockOtherLineOrShutdown(t *testing.T) {
	m, b := newSMSTest(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	b.mu.Lock()
	b.wait, b.entered = gate, entered
	b.mu.Unlock()
	c := newWireClient(t, m.runtime)
	c.messageChallenge("+15550000001", "blocked", "text/plain")
	if _, err := c.conn.Write([]byte(c.messagePacket("+15550000001", "blocked", "text/plain", c.messageAuthorization("+15550000001"), 2, ""))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("send not reached")
	}
	if code := m.sms.attempt(context.Background(), hashCallOperation("same-line"), hashCallOperation("x"), "module-a", "+15550000001", "x", ""); code != 503 {
		t.Fatal("line fence missing")
	}
	if code := m.sms.attempt(context.Background(), hashCallOperation("other-line"), hashCallOperation("x"), "module-b", "+15550000002", "x", ""); code != 202 {
		t.Fatal("modules coupled", code)
	}
	start := time.Now()
	m.Close()
	if time.Since(start) > time.Second {
		t.Fatal("shutdown waited for hardware")
	}
	close(gate)
	waitCarrier(t, func() bool { m.sms.mu.Lock(); defer m.sms.mu.Unlock(); return len(m.sms.busy) == 0 })
}
func TestSMSInboundOfflineQueueCanonicalIdentityAndNoDuplicate(t *testing.T) {
	m, _ := newSMSTest(t)
	receiver := registerMessagePeer(t, m.runtime, "1002")
	message := IncomingSMS{ID: "logical-message", DeviceID: "module-b", Subscription: "synthetic-sim", Sender: "+15550000002", Text: "验证码 123456"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	delivery := replyOneMessage(receiver.conn, 200)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual := <-delivery
	if actual.err != nil {
		t.Fatal(actual.err)
	}
	if string(actual.request.Body()) != message.Text || actual.request.From().Address.User != smsReplyUser(message.DeviceID, message.Subscription, message.Sender) || actual.request.From().Address.Host != "voice.example.test" || actual.request.From().DisplayName != message.Sender {
		t.Fatal("wrong MT identity/body", actual.request.String())
	}
	stats := m.sms.statistics()
	if stats.IncomingAccepted != 1 || stats.IncomingQueued != 1 {
		t.Fatal(stats)
	}
	// The manager and listener may restart; the offline recipient remains queued.
	if cursor, err := m.SMSCursor(context.Background(), 999); err != nil || cursor != 1 {
		t.Fatal(cursor, err)
	}
	if err := m.QueueSMS(context.Background(), 2, &message); err != nil {
		t.Fatal(err)
	}
	late := registerMessagePeer(t, m.runtime, "1001")
	delivery = replyOneMessage(late.conn, 202)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if actual = <-delivery; actual.err != nil {
		t.Fatal(actual.err)
	}
	stats = m.sms.statistics()
	if stats.IncomingQueued != 0 || stats.IncomingAccepted != 2 {
		t.Fatal(stats)
	}
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiver.conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 2048)); err == nil {
		t.Fatal("accepted SMS was duplicated")
	}
}
func TestSMSInboundUnknownNeverAutomaticallyReplayed(t *testing.T) {
	m, _ := newSMSTest(t)
	// Stored-message delivery takes r.mu before reading this fixture timeout.
	m.runtime.mu.Lock()
	m.runtime.messageTimeout = 30 * time.Millisecond
	m.runtime.mu.Unlock()
	message := IncomingSMS{ID: "unknown", DeviceID: "module-a", Subscription: "synthetic-sim", Sender: "+15550000001", Text: "not retried"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	receiver := registerMessagePeer(t, m.runtime, "1002")
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := readPeerMessage(receiver.conn); err != nil {
		t.Fatal(err)
	}
	if m.sms.statistics().IncomingUnknown != 1 {
		t.Fatal(m.sms.statistics())
	}
	m.sms = newSMSService(m.db, newSyntheticSMS())
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiver.conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 2048)); err == nil {
		t.Fatal("unknown SMS replayed")
	}
}
func TestSMSOfflineEntriesDoNotStarveOnlineRecipient(t *testing.T) {
	m, _ := newSMSTest(t)
	receiver := registerMessagePeer(t, m.runtime, "1002")
	state := smsLedger{Version: 1, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{}}
	for i := 0; i < 21; i++ {
		user := "1001"
		key := fmt.Sprintf("%064x", i+1)
		if i == 20 {
			user = "1002"
			key = strings.Repeat("f", 64)
		}
		state.Inbox[key] = smsInbox{Message: IncomingSMS{ID: key, DeviceID: "module-a", Subscription: "synthetic-sim", Sender: "+15550000001", Text: "queued"}, Until: time.Now().Add(time.Hour), Targets: map[string]smsRecipient{user: {Identity: recipientIdentity(m.current, user)}}, SourceIndependent: true}
	}
	if err := m.sms.save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	delivery := replyOneMessage(receiver.conn, 200)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if actual := <-delivery; actual.err != nil {
		t.Fatal(actual.err)
	}
	if stats := m.sms.statistics(); stats.IncomingAccepted != 1 || stats.IncomingQueued != 20 {
		t.Fatal(stats)
	}
}
func TestSMSPolicyDefaultOffAndBadGrants(t *testing.T) {
	c := smsTestConfig(t)
	for _, mutate := range []func(*Config){func(c *Config) { c.Accounts[0].SMSSendLines = []string{"missing"} }, func(c *Config) { c.CarrierLines[0].SMSRecipients = []string{"missing"} }, func(c *Config) { c.CarrierLines[0].SMSRecipients = []string{"1001", "1001"} }} {
		config := cloneConfig(c.Config)
		mutate(&config)
		if _, err := prepare(config, c, true); err == nil {
			t.Fatal("invalid grant accepted")
		}
	}
	copy := cloneConfig(c.Config)
	copy.Accounts[0].SMSSendLines[0] = "changed"
	copy.CarrierLines[0].SMSRecipients[0] = "changed"
	if c.Config.Accounts[0].SMSSendLines[0] == "changed" || c.Config.CarrierLines[0].SMSRecipients[0] == "changed" {
		t.Fatal("mutable snapshot grants")
	}
	if DefaultConfig().SMSEnabled {
		t.Fatal("SMS enabled by default")
	}
}

func TestSMSExplicitPrefixAndPermissionDeny(t *testing.T) {
	m, b := newSMSTest(t)
	c := newWireClient(t, m.runtime)
	target := "82+15550000002"
	c.messageChallenge(target, "prefix", "text/plain")
	requireStatus(t, c.exchange(c.messagePacket(target, "prefix", "text/plain", c.messageAuthorization(target), 2, "")), 202)
	if sent := b.sentSnapshot(); sent[0][0] != "module-b" || sent[0][1] != "+15550000002" {
		t.Fatal("prefix ignored", sent)
	}
	account := m.current.Config.Accounts[0]
	account.SMSSendLines = []string{"module-a"}
	if _, _, code := smsRoute(m.current.Config, account, target); code != 403 {
		t.Fatal("call permission granted SMS", code)
	}
	if _, _, code := smsRoute(m.current.Config, account, "1003"); code != 484 {
		t.Fatal("short extension treated as phone", code)
	}
}

func TestSMSQueuedRecipientRevocationAndOversizeAreTerminal(t *testing.T) {
	m, _ := newSMSTest(t)
	message := IncomingSMS{ID: "large-message", DeviceID: "module-a", Subscription: "synthetic-sim", Sender: "+15550000001", Text: strings.Repeat("汉", 500)}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	_ = registerMessagePeerWithOversizedRequestURI(t, m.runtime, "1002")
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := m.sms.statistics(); stats.IncomingRejected != 1 || stats.IncomingQueued != 1 {
		t.Fatal("oversize not explicit", stats)
	}
	m.sms.mu.Lock()
	state, err := m.sms.load(context.Background())
	m.sms.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range state.Inbox {
		if in.Message.Text != message.Text {
			t.Fatal("message truncated")
		}
	}
	// Simulate an atomically installed new listener/config: re-created account
	// credentials must never inherit an old recipient's queued private SMS.
	config := smsTestConfig(t)
	config.Secrets = map[string]secret{"1001": {SHA256: hashCallOperation("rotated")}, "1002": m.current.Secrets["1002"]}
	m.runtime.close()
	r, err := start(config, runtimeOptions{sms: m.sms})
	if err != nil {
		t.Fatal(err)
	}
	m.runtime = r
	m.current = config
	// Registration is synthetic; no network request can pass the identity guard.
	r.mu.Lock()
	r.registrations["1001"] = map[string]registration{"synthetic": {source: "127.0.0.1:9", until: time.Now().Add(time.Minute)}}
	r.mu.Unlock()
	if err = m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := m.sms.statistics(); stats.IncomingQueued != 0 || stats.IncomingRejected != 2 {
		t.Fatal("queued SMS inherited by new credentials", stats)
	}
}

func TestSMSUnsupportedInboundIsVisibleAndDoesNotStallCursor(t *testing.T) {
	m, _ := newSMSTest(t)
	message := IncomingSMS{ID: "over-limit", DeviceID: "module-a", Subscription: "synthetic-sim", Sender: "+15550000001", Text: strings.Repeat("x", 16385)}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	if cursor, err := m.SMSCursor(context.Background(), 999); err != nil || cursor != 1 {
		t.Fatal("oversize stalled cursor", cursor, err)
	}
	if stats := m.sms.statistics(); stats.IncomingRejected != 2 {
		t.Fatal("unsupported input silently lost", stats)
	}
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal("terminal metadata invalid", err)
	}
}
