package sipgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// These regression tests use independent real SQLite stores on the same file.
// The adapter only pauses after a real read; no SQL result or write is mocked.
// Only the MT ABA case starts actual loopback SIP listeners; no modem, carrier,
// remote service, or production file is used.
type smsCASPausedRead struct {
	*store.Store
	readNumber int32
	reads      atomic.Int32
	arrived    chan<- struct{}
	release    <-chan struct{}
}

func (db *smsCASPausedRead) AppSetting(ctx context.Context, key string) (store.AppSetting, error) {
	value, err := db.Store.AppSetting(ctx, key)
	if key == SMSLedgerSettingKey && db.reads.Add(1) == db.readNumber {
		db.arrived <- struct{}{}
		select {
		case <-db.release:
		case <-ctx.Done():
			return store.AppSetting{}, ctx.Err()
		}
	}
	return value, err
}

// Dynamic forwarding keeps this test compilable against the historical source
// that has no CAS method. The old implementation never calls this method.
func (db *smsCASPausedRead) CompareSMSLedger(ctx context.Context, before, after json.RawMessage) (bool, error) {
	cas, ok := any(db.Store).(interface {
		CompareSMSLedger(context.Context, json.RawMessage, json.RawMessage) (bool, error)
	})
	if !ok {
		return false, errors.New("real SQLite store has no SMS ledger CAS")
	}
	return cas.CompareSMSLedger(ctx, before, after)
}

type smsCASBackend struct {
	calls   atomic.Int32
	entered chan struct{}
	release <-chan struct{}
	result  vowifi.SMSSubmitResult
}

func (b *smsCASBackend) SMSReady(context.Context, string) (string, error) {
	return hashCallOperation("cas-regression-synthetic-subscription"), nil
}

func (b *smsCASBackend) SendCarrierSMS(context.Context, string, string, string, string, string) (vowifi.SMSSubmitResult, error) {
	b.calls.Add(1)
	if b.entered != nil {
		b.entered <- struct{}{}
	}
	if b.release != nil {
		<-b.release // Deliberately survives listener cancellation, like a blocked device operation.
	}
	return b.result, nil
}

func smsCASAccepted() vowifi.SMSSubmitResult {
	return vowifi.SMSSubmitResult{PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true}
}

type smsCASRelease struct {
	once sync.Once
	ch   chan struct{}
}

func newSMSCASRelease() *smsCASRelease { return &smsCASRelease{ch: make(chan struct{})} }
func (g *smsCASRelease) close()        { g.once.Do(func() { close(g.ch) }) }

func smsCASStores(t *testing.T) [2]*store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "actual-shared-sms.db")
	var stores [2]*store.Store
	for i := range stores {
		db, err := store.Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = db
		t.Cleanup(func() { _ = db.Close() })
	}
	return stores
}

func smsCASWait(t *testing.T, ctx context.Context, signal <-chan struct{}, count int, description string) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case <-signal:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s (%d/%d): %v", description, i, count, ctx.Err())
		}
	}
}

func smsCASResult(t *testing.T, ctx context.Context, result <-chan int) int {
	t.Helper()
	select {
	case code := <-result:
		return code
	case <-ctx.Done():
		t.Fatal("SMS operation did not terminate:", ctx.Err())
		return 0
	}
}

func smsCASReadLedger(t *testing.T, db *store.Store) smsLedger {
	t.Helper()
	value, err := db.AppSetting(context.Background(), SMSLedgerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var state smsLedger
	if err := json.Unmarshal(value.Value, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSMSCASSameFileIndependentServicesAdmitOneCarrierSubmission(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reads := make(chan struct{}, 2)
	gate := newSMSCASRelease()
	defer gate.close()
	backend := &smsCASBackend{result: smsCASAccepted()}
	results := make(chan int, 2)
	operation, fingerprint := hashCallOperation("same-operation"), hashCallOperation("same-private-body")
	for _, db := range stores {
		paused := &smsCASPausedRead{Store: db, readNumber: 2, arrived: reads, release: gate.ch}
		service := newSMSService(paused, backend)
		go func() {
			results <- service.attempt(ctx, operation, fingerprint, "module-a", "+15550000001", "synthetic private body", "")
		}()
	}
	// Both independent services have read the same absent reservation after
	// SMSReady, before either can persist Unknown. Only the actual SQLite CAS
	// may decide which worker is allowed to reach the backend.
	smsCASWait(t, ctx, reads, 2, "independent pre-reservation SQLite snapshots")
	gate.close()
	codes := []int{smsCASResult(t, ctx, results), smsCASResult(t, ctx, results)}
	if got := backend.calls.Load(); got != 1 {
		t.Fatalf("same file/operation reached carrier %d times; expected exactly one; responses=%v", got, codes)
	}
	for _, code := range codes {
		if code != 200 && code != 202 && code != 504 {
			t.Fatal("deduplicated operation did not retain its durable outcome:", codes)
		}
	}
}

func TestSMSCASSameFileDisjointAttemptsCannotOverwriteTheOtherUnknown(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reads := make(chan struct{}, 2)
	readGate, backendGate := newSMSCASRelease(), newSMSCASRelease()
	defer readGate.close()
	defer backendGate.close()
	backend := &smsCASBackend{entered: make(chan struct{}, 2), release: backendGate.ch, result: smsCASAccepted()}
	results := make(chan int, 2)
	operations := []string{hashCallOperation("independent-operation-a"), hashCallOperation("independent-operation-b")}
	for i, db := range stores {
		service := newSMSService(&smsCASPausedRead{Store: db, readNumber: 2, arrived: reads, release: readGate.ch}, backend)
		operation := operations[i]
		go func() {
			results <- service.attempt(ctx, operation, hashCallOperation("private-body"), "module-a", "+15550000001", "synthetic private body", "")
		}()
	}
	smsCASWait(t, ctx, reads, 2, "disjoint pre-reservation SQLite snapshots")
	readGate.close()
	smsCASWait(t, ctx, backend.entered, 2, "independent carrier submissions")
	during := smsCASReadLedger(t, stores[0])
	backendGate.close()
	codes := []int{smsCASResult(t, ctx, results), smsCASResult(t, ctx, results)}
	if len(during.Attempts) != 2 {
		t.Fatalf("whole-blob lost update erased a different paid reservation: have=%d expected=2; responses=%v", len(during.Attempts), codes)
	}
	for _, operation := range operations {
		if during.Attempts[operation].Code != 504 {
			t.Fatal("each operation must be durable Unknown before its backend finishes")
		}
	}
	after := smsCASReadLedger(t, stores[1])
	for _, operation := range operations {
		if attempt, ok := after.Attempts[operation]; !ok || attempt.Code != 202 {
			t.Fatal("completion overwrote a different operation:", operation, codes)
		}
	}
}

func TestSMSCASSameOperationExpiryABARejectsOldWorkerCompletion(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldGate, newGate := newSMSCASRelease(), newSMSCASRelease()
	defer oldGate.close()
	defer newGate.close()
	oldBackend := &smsCASBackend{entered: make(chan struct{}, 1), release: oldGate.ch, result: smsCASAccepted()}
	newBackend := &smsCASBackend{entered: make(chan struct{}, 1), release: newGate.ch,
		result: vowifi.SMSSubmitResult{PartsTotal: 1, PartsAttempted: 1}}
	operation, fingerprint := hashCallOperation("ABA-identical-operation"), hashCallOperation("ABA-identical-body")
	oldResult, newResult := make(chan int, 1), make(chan int, 1)
	go func() {
		oldResult <- newSMSService(stores[0], oldBackend).attempt(ctx, operation, fingerprint, "module-a", "+15550000001", "synthetic body", "")
	}()
	smsCASWait(t, ctx, oldBackend.entered, 1, "old backend after durable Unknown")

	// Expire only the original reservation, preserving every unknown JSON field
	// (including a future attempt token). This is raw fixture setup, not a fake
	// repository mutation. The same operation/fingerprint is then admitted anew.
	value, err := stores[1].AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	var attempts map[string]json.RawMessage
	var oldAttempt map[string]json.RawMessage
	if json.Unmarshal(value.Value, &root) != nil || json.Unmarshal(root["attempts"], &attempts) != nil || json.Unmarshal(attempts[operation], &oldAttempt) != nil {
		t.Fatal("invalid raw reservation fixture")
	}
	oldAttempt["until"], _ = json.Marshal(time.Now().Add(-time.Hour))
	attempts[operation], _ = json.Marshal(oldAttempt)
	root["attempts"], _ = json.Marshal(attempts)
	value.Value, _ = json.Marshal(root)
	if err := stores[1].UpsertAppSetting(ctx, value); err != nil {
		t.Fatal(err)
	}
	go func() {
		newResult <- newSMSService(stores[1], newBackend).attempt(ctx, operation, fingerprint, "module-a", "+15550000001", "synthetic body", "")
	}()
	smsCASWait(t, ctx, newBackend.entered, 1, "replacement backend after its new durable Unknown")
	replacement := smsCASReadLedger(t, stores[1]).Attempts[operation]
	if replacement.Code != 504 {
		t.Fatal("replacement was not Unknown before old completion")
	}
	oldGate.close()
	oldCode := smsCASResult(t, ctx, oldResult)
	afterOld := smsCASReadLedger(t, stores[1]).Attempts[operation]
	newGate.close()
	_ = smsCASResult(t, ctx, newResult)
	if afterOld.Code != 504 || !afterOld.Until.Equal(replacement.Until) {
		t.Fatalf("old worker mutated a replacement reservation with the same operation/fingerprint: code=%d expected=504; old-worker-response=%d", afterOld.Code, oldCode)
	}
}

// Observe only a completed real SQLite write. The hook cannot grant a CAS,
// fabricate a read, or change the content being committed by production code.
type smsCASObservedWrite struct {
	*store.Store
	after func(json.RawMessage)
}

func (db *smsCASObservedWrite) UpsertAppSetting(ctx context.Context, value store.AppSetting) error {
	err := db.Store.UpsertAppSetting(ctx, value)
	if err == nil && value.Key == SMSLedgerSettingKey && db.after != nil {
		db.after(value.Value)
	}
	return err
}

func (db *smsCASObservedWrite) CompareSMSLedger(ctx context.Context, before, after json.RawMessage) (bool, error) {
	cas, ok := any(db.Store).(interface {
		CompareSMSLedger(context.Context, json.RawMessage, json.RawMessage) (bool, error)
	})
	if !ok {
		return false, errors.New("real SQLite store has no SMS ledger CAS")
	}
	changed, err := cas.CompareSMSLedger(ctx, before, after)
	if changed && err == nil && db.after != nil {
		db.after(after)
	}
	return changed, err
}

func smsCASSeedInbox(t *testing.T, db *store.Store, config stored, message IncomingSMS) string {
	t.Helper()
	key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
	state := smsLedger{
		Version: 1, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{
			key: {Message: message, Until: time.Now().Add(time.Hour), SourceIndependent: true, Targets: map[string]smsRecipient{
				"1001": {Identity: recipientIdentity(config, "1001"), Code: 0},
			}},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	// Intentional raw legacy fixture import, not the production mutation helper.
	if err := db.UpsertAppSetting(context.Background(), store.AppSetting{Key: SMSLedgerSettingKey, Value: data, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSMSCASDurableUnknownRemainsUnknownWhenRuntimeStopsBeforeWire(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := smsTestConfig(t)
	message := IncomingSMS{ID: "stop-after-unknown", DeviceID: "module-a", Subscription: "synthetic-sim",
		Sender: "+15550000001", Text: "synthetic private MT body"}
	key := smsCASSeedInbox(t, stores[0], config, message)
	// No transport is constructed: the runtime is stopped at the durable claim
	// boundary, before any MESSAGE can be handed to a transport implementation.
	r := &runtime{config: config, ctx: ctx, messageTargets: map[string]bool{},
		registrations: map[string]map[string]registration{
			"1001": {"synthetic-local": {source: "127.0.0.1:9", until: time.Now().Add(time.Hour)}},
		}}
	var observed atomic.Bool
	db := &smsCASObservedWrite{Store: stores[0], after: func(data json.RawMessage) {
		var state smsLedger
		if json.Unmarshal(data, &state) == nil && state.Inbox[key].Targets["1001"].Code == 504 {
			observed.Store(true)
			r.closed.Store(true)
		}
	}}
	if err := newSMSService(db, nil).deliverInbox(ctx, r, key, "1001"); err != nil {
		t.Fatal(err)
	}
	if !observed.Load() {
		t.Fatal("the real store never committed the pre-wire Unknown claim")
	}
	if code := smsCASReadLedger(t, stores[1]).Inbox[key].Targets["1001"].Code; code != 504 {
		t.Fatalf("stopping a claimed runtime changed durable Unknown to %d; must not requeue as 0", code)
	}
}

func smsCASExpireInbox(t *testing.T, db *store.Store, key string) {
	t.Helper()
	value, err := db.AppSetting(context.Background(), SMSLedgerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var root, inbox, entry map[string]json.RawMessage
	if json.Unmarshal(value.Value, &root) != nil || json.Unmarshal(root["inbox"], &inbox) != nil || json.Unmarshal(inbox[key], &entry) != nil {
		t.Fatal("invalid raw inbox fixture")
	}
	entry["until"], _ = json.Marshal(time.Now().Add(-time.Hour))
	inbox[key], _ = json.Marshal(entry)
	root["inbox"], _ = json.Marshal(inbox)
	value.Value, _ = json.Marshal(root)
	if err := db.UpsertAppSetting(context.Background(), value); err != nil {
		t.Fatal(err)
	}
}

func smsCASDeliveryResult(t *testing.T, ctx context.Context, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("MT worker did not terminate:", ctx.Err())
	}
}

func smsCASReply(t *testing.T, conn *net.UDPConn, request *sip.Request, code int) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response := sip.NewResponseFromRequest(request, code, "Synthetic held response", nil)
	if _, err := conn.Write([]byte(response.String())); err != nil {
		t.Fatal(err)
	}
}

func smsCASRecipientToken(target smsRecipient) string {
	// JSON inspection rather than a static field reference permits the same
	// regression file to demonstrate the bug against the pre-token source.
	data, _ := json.Marshal(target)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	var token string
	_ = json.Unmarshal(fields["attempt_token"], &token)
	return token
}

func TestSMSCASInboxExpiryABAHeldOldMessageCannotCompleteReplacementRecipient(t *testing.T) {
	stores := smsCASStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	config := smsTestConfig(t)
	for i := range config.Config.CarrierLines {
		config.Config.CarrierLines[i].SMSRecipients = []string{"1001"}
	}
	message := IncomingSMS{ID: "same-inbox-source-after-expiry", DeviceID: "module-a", Subscription: "synthetic-sim",
		Sender: "+15550000001", Text: "synthetic held MT body"}
	key := smsCASSeedInbox(t, stores[0], config, message)
	oldService, replacementService := newSMSService(stores[0], nil), newSMSService(stores[1], nil)
	oldRuntime, err := start(config, runtimeOptions{sms: oldService})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oldRuntime.close)
	oldRuntime.mu.Lock()
	oldRuntime.messageTimeout = 10 * time.Second
	oldRuntime.mu.Unlock()
	oldPeer := registerMessagePeer(t, oldRuntime, "1001")
	oldResult := make(chan error, 1)
	go func() { oldResult <- oldService.deliverInbox(ctx, oldRuntime, key, "1001") }()
	oldRequest, err := readPeerMessage(oldPeer.conn)
	if err != nil {
		t.Fatal(err)
	}
	oldClaim := smsCASReadLedger(t, stores[0]).Inbox[key].Targets["1001"]
	if oldClaim.Code != 504 {
		t.Fatal("old actual MESSAGE was not preceded by durable Unknown")
	}

	smsCASExpireInbox(t, stores[1], key)
	replacementConfig := config
	replacementConfig.Config = cloneConfig(config.Config)
	// Same account authority/epoch, different loopback listener. This is a token
	// ABA regression, not a credential replacement/revocation test.
	replacementConfig.Config.Listen = smsTestConfig(t).Config.Listen
	replacementRuntime, err := start(replacementConfig, runtimeOptions{sms: replacementService})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacementRuntime.close)
	replacementRuntime.mu.Lock()
	replacementRuntime.messageTimeout = 10 * time.Second
	replacementRuntime.mu.Unlock()
	replacementPeer := registerMessagePeer(t, replacementRuntime, "1001")
	manager := &Manager{db: stores[1], current: replacementConfig, runtime: replacementRuntime, sms: replacementService}
	if err := manager.QueueSMS(ctx, 2, &message); err != nil {
		t.Fatal(err)
	}
	replacementResult := make(chan error, 1)
	go func() { replacementResult <- replacementService.deliverInbox(ctx, replacementRuntime, key, "1001") }()
	replacementRequest, err := readPeerMessage(replacementPeer.conn)
	if err != nil {
		t.Fatal(err)
	}
	replacementLedger := smsCASReadLedger(t, stores[1])
	replacementInbox := replacementLedger.Inbox[key]
	replacementClaim := replacementInbox.Targets["1001"]
	if replacementClaim.Code != 504 {
		t.Fatal("replacement actual MESSAGE was not preceded by its durable Unknown")
	}
	if replacementLedger.Version >= 2 && (smsCASRecipientToken(replacementClaim) == "" || smsCASRecipientToken(replacementClaim) == smsCASRecipientToken(oldClaim)) {
		t.Fatal("replacement recipient did not receive a distinct attempt token")
	}
	smsCASReply(t, oldPeer.conn, oldRequest, 200)
	smsCASDeliveryResult(t, ctx, oldResult)
	afterOld := smsCASReadLedger(t, stores[1]).Inbox[key]
	smsCASReply(t, replacementPeer.conn, replacementRequest, 200)
	smsCASDeliveryResult(t, ctx, replacementResult)
	if afterOld.Targets["1001"].Code != 504 || !afterOld.Until.Equal(replacementInbox.Until) ||
		smsCASRecipientToken(afterOld.Targets["1001"]) != smsCASRecipientToken(replacementClaim) {
		t.Fatalf("old held MESSAGE completed a replacement recipient: code=%d expected=504", afterOld.Targets["1001"].Code)
	}
	if code := smsCASReadLedger(t, stores[1]).Inbox[key].Targets["1001"].Code; code != 200 {
		t.Fatal("replacement's own final response could not complete its claim:", code)
	}
}
