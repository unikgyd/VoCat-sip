package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/developer"
	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// Only the physical boundary is synthetic. All Inbox, raw archive, immutable
// origin/source receipts and planner transactions use an actual file Store.
type smsForwardScanController struct {
	fakeDeviceController
	mu            sync.Mutex
	entries       map[string]device.Device
	messages      map[string][]device.SMSMessage
	listingErrors map[string]error
	afterList     map[string]func(*device.Snapshot)
	arrivals      chan struct{}
}

func smsForwardScanCopyEntry(entry device.Device) device.Device {
	if entry.Snapshot != nil {
		snapshot := *entry.Snapshot
		entry.Snapshot = &snapshot
	}
	return entry
}

func (c *smsForwardScanController) List() []device.Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]device.Device, 0, len(c.entries))
	for _, entry := range c.entries {
		result = append(result, smsForwardScanCopyEntry(entry))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (c *smsForwardScanController) Get(id string) (device.Device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return device.Device{}, device.ErrNotFound
	}
	return smsForwardScanCopyEntry(entry), nil
}

func (c *smsForwardScanController) ListSMS(_ context.Context, id string) (device.SMSListing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.listingErrors[id]; err != nil {
		return device.SMSListing{}, err
	}
	result := device.SMSListing{Messages: append([]device.SMSMessage(nil), c.messages[id]...)}
	if change := c.afterList[id]; change != nil {
		entry := smsForwardScanCopyEntry(c.entries[id])
		if entry.Snapshot != nil {
			change(entry.Snapshot)
		}
		c.entries[id] = entry
		delete(c.afterList, id)
	}
	return result, nil
}

func (c *smsForwardScanController) SMSArrivals() <-chan struct{} { return c.arrivals }

func (c *smsForwardScanController) setMessages(id string, messages ...device.SMSMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages[id] = append([]device.SMSMessage(nil), messages...)
}

func (c *smsForwardScanController) changeSnapshot(id string, change func(*device.Snapshot)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := smsForwardScanCopyEntry(c.entries[id])
	change(entry.Snapshot)
	c.entries[id] = entry
}

type smsForwardScanFixture struct {
	t           *testing.T
	ctx         context.Context
	path        string
	db          *store.Store
	server      *Server
	controller  *smsForwardScanController
	plans       atomic.Int32
	planMu      sync.Mutex
	planDevices map[string]int
}

func newSMSForwardScanFixture(t *testing.T, ids ...string) *smsForwardScanFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scan.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := developer.SetAutoClearModemStorage(ctx, db, false); err != nil {
		t.Fatal(err)
	}
	controller := &smsForwardScanController{entries: map[string]device.Device{}, messages: map[string][]device.SMSMessage{}, listingErrors: map[string]error{}, afterList: map[string]func(*device.Snapshot){}, arrivals: make(chan struct{}, 2)}
	fixture := &smsForwardScanFixture{t: t, ctx: ctx, path: path, db: db, controller: controller, planDevices: map[string]int{}}
	fixture.server = &Server{store: db, logger: regionTestLogger(), devices: controller}
	for index, id := range ids {
		imei := "867394042309830"
		if index != 0 {
			imei = "867394042309831"
		}
		if err := db.UpsertDevice(ctx, store.Device{ID: id, Name: id, DeviceType: store.DeviceTypePCIeEC20EC25, ModemIMEI: imei, SMSEnabled: true, IMSMode: store.IMSModeModule}); err != nil {
			t.Fatal(err)
		}
		controller.entries[id] = device.Device{ID: id, Discovered: true, Snapshot: &device.Snapshot{DeviceID: id, IMEI: imei, ICCID: "8986000000000000001", IMSI: "001010000000001"}}
	}
	db.SetSMSForwardPlanner(func(input store.SMSForwardingInput) (json.RawMessage, error) {
		fixture.plans.Add(1)
		fixture.planMu.Lock()
		fixture.planDevices[input.Message.DeviceID]++
		fixture.planMu.Unlock()
		return json.Marshal(map[string]string{"synthetic_scan_plan": input.SourceKey, "fingerprint": input.Fingerprint})
	})
	return fixture
}

func smsForwardScanPlain(index int, body string) device.SMSMessage {
	digest := sha256.Sum256([]byte(body))
	return device.SMSMessage{Index: index, Storage: "SM", StorageStatus: device.SMSStatusReceivedUnread,
		Direction: device.SMSDirectionReceived, From: "+15550000001", Text: body, Encoding: device.SMSEncodingGSM7PDU,
		RawPDU: hex.EncodeToString(digest[:])}
}

func (f *smsForwardScanFixture) observed(id string) { f.server.syncObservedModemSMS(f.ctx, id) }
func (f *smsForwardScanFixture) manual(id string)   { f.server.syncModemSMS(f.ctx, id) }
func (f *smsForwardScanFixture) expectPlans(expected int32) {
	f.t.Helper()
	if actual := f.plans.Load(); actual != expected {
		f.t.Fatalf("live receive planner calls=%d, want %d", actual, expected)
	}
}

func (f *smsForwardScanFixture) config(id string, change func(*store.Device)) {
	f.t.Helper()
	config, err := f.db.Device(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	change(&config)
	if err := f.db.UpsertDevice(f.ctx, config); err != nil {
		f.t.Fatal(err)
	}
}

func (f *smsForwardScanFixture) sql(statement string) {
	f.t.Helper()
	connection, err := sql.Open("sqlite", f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(f.ctx, statement); err != nil {
		f.t.Fatal(err)
	}
}

func TestSMSForwardScanFirstSnapshotArchivesThenOnlyNewPhysicalRowsAreLive(t *testing.T) {
	f := newSMSForwardScanFixture(t, "scan-device")
	old, newer := smsForwardScanPlain(1, "old storage"), smsForwardScanPlain(2, "new observation")
	f.controller.setMessages("scan-device", old)
	f.observed("scan-device")
	f.expectPlans(0)
	f.controller.setMessages("scan-device", old, newer)
	f.observed("scan-device")
	f.expectPlans(1)
	f.observed("scan-device")
	f.expectPlans(1)
	// The same SM slot reused with a different PDU is a new physical row.
	reused := smsForwardScanPlain(2, "new PDU in reused slot")
	f.controller.setMessages("scan-device", old, reused)
	f.observed("scan-device")
	f.expectPlans(2)
	inbox, err := f.db.ListSMSMessages(f.ctx, store.SMSFilter{DeviceID: "scan-device"})
	if err != nil || len(inbox) != 3 {
		t.Fatalf("live/archive observations did not preserve local history: count=%d error=%v", len(inbox), err)
	}
}

func TestSMSForwardScanManualCatchupAlwaysArchivesAndMayEstablishBaseline(t *testing.T) {
	f := newSMSForwardScanFixture(t, "scan-device")
	old, newer, manual, last := smsForwardScanPlain(1, "manual original"), smsForwardScanPlain(2, "observed after manual"), smsForwardScanPlain(3, "manual while qualified"), smsForwardScanPlain(4, "live after manual")
	f.controller.setMessages("scan-device", old)
	f.manual("scan-device")
	f.expectPlans(0)
	f.controller.setMessages("scan-device", old, newer)
	f.observed("scan-device")
	f.expectPlans(1)
	f.controller.setMessages("scan-device", old, newer, manual)
	// HTTP's per-device catch-up also uses the manual locked entry point.
	f.server.smsSyncMu.Lock()
	f.server.syncOneModemSMSLocked(f.ctx, "scan-device")
	f.server.smsSyncMu.Unlock()
	f.expectPlans(1)
	f.observed("scan-device")
	f.expectPlans(1)
	f.controller.setMessages("scan-device", old, newer, manual, last)
	f.observed("scan-device")
	f.expectPlans(2)
}

func TestSMSForwardScanScopeChangesRequireNewArchiveBaseline(t *testing.T) {
	for _, field := range []string{"ICCID", "IMSI", "actual IMEI", "physical ID", "effective mode"} {
		t.Run(field, func(t *testing.T) {
			f := newSMSForwardScanFixture(t, "scan-device")
			f.observed("scan-device") // Successful empty storage is a real baseline.
			physicalID := "scan-device"
			switch field {
			case "ICCID":
				f.controller.changeSnapshot(physicalID, func(s *device.Snapshot) { s.ICCID = "8986000000000000002" })
			case "IMSI":
				f.controller.changeSnapshot(physicalID, func(s *device.Snapshot) { s.IMSI = "001010000000002" })
			case "actual IMEI":
				f.controller.changeSnapshot(physicalID, func(s *device.Snapshot) { s.IMEI = "867394042309832" })
			case "physical ID":
				f.controller.mu.Lock()
				entry := f.controller.entries[physicalID]
				delete(f.controller.entries, physicalID)
				physicalID = "reallocated-physical-device"
				entry.ID = physicalID
				f.controller.entries[physicalID] = entry
				f.controller.mu.Unlock()
			default:
				f.config("scan-device", func(c *store.Device) { c.IMSMode = store.IMSModeVoWiFi })
			}
			first, next := smsForwardScanPlain(1, "first in changed scope"), smsForwardScanPlain(2, "second in changed scope")
			f.controller.setMessages(physicalID, first)
			f.observed("scan-device")
			f.expectPlans(0)
			f.controller.setMessages(physicalID, first, next)
			f.observed("scan-device")
			f.expectPlans(1)
		})
	}
}

func TestSMSForwardScanSkippedOrFailedPassInvalidatesQualification(t *testing.T) {
	for _, failure := range []string{"ListSMS failure", "missing physical", "Host mode skip", "VoWiFi setup defer", "identity changed during read"} {
		t.Run(failure, func(t *testing.T) {
			f := newSMSForwardScanFixture(t, "scan-device")
			f.observed("scan-device")
			first, next := smsForwardScanPlain(1, "source observed across failed scan"), smsForwardScanPlain(2, "source after recovered baseline")
			f.controller.setMessages("scan-device", first)
			switch failure {
			case "ListSMS failure":
				f.controller.listingErrors["scan-device"] = errors.New("synthetic CMGL failure")
			case "missing physical":
				entry := f.controller.entries["scan-device"]
				entry.Discovered = false
				f.controller.entries["scan-device"] = entry
			case "Host mode skip":
				f.config("scan-device", func(c *store.Device) { c.IMSMode = store.IMSModeHost })
			case "VoWiFi setup defer":
				f.server.vowifi = &fakeVoWiFiController{state: vowifi.State{Enabled: true, Phase: vowifi.PhaseSIMReady}}
			default:
				f.controller.afterList["scan-device"] = func(snapshot *device.Snapshot) { snapshot.IMSI = "001010000000002" }
			}
			f.observed("scan-device")
			f.expectPlans(0)
			delete(f.controller.listingErrors, "scan-device")
			entry := f.controller.entries["scan-device"]
			entry.Discovered = true
			f.controller.entries["scan-device"] = entry
			f.config("scan-device", func(c *store.Device) { c.IMSMode = store.IMSModeModule })
			f.server.vowifi = nil
			f.observed("scan-device")
			f.expectPlans(0)
			f.controller.setMessages("scan-device", first, next)
			f.observed("scan-device")
			f.expectPlans(1)
		})
	}
}

func TestSMSForwardScanRawAndInboxErrorsInvalidateTheWholeObservation(t *testing.T) {
	for _, table := range []string{"sms_raw_records", "sms_messages"} {
		t.Run(table, func(t *testing.T) {
			f := newSMSForwardScanFixture(t, "scan-device")
			f.observed("scan-device")
			first, next := smsForwardScanPlain(1, "failed persistence observation"), smsForwardScanPlain(2, "after persistence recovery")
			f.controller.setMessages("scan-device", first)
			f.sql("CREATE TRIGGER fail_forward_scan BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(FAIL,'synthetic observation persistence failure'); END")
			f.observed("scan-device")
			f.expectPlans(0)
			f.sql("DROP TRIGGER fail_forward_scan")
			f.observed("scan-device")
			f.expectPlans(0)
			f.controller.setMessages("scan-device", first, next)
			f.observed("scan-device")
			f.expectPlans(1)
			inbox, err := f.db.ListSMSMessages(f.ctx, store.SMSFilter{DeviceID: "scan-device"})
			if err != nil || len(inbox) != 2 {
				t.Fatalf("persistence recovery lost the original local Inbox: count=%d error=%v", len(inbox), err)
			}
		})
	}
}

func TestSMSForwardScanBaselinePartialCannotBecomeLiveAfterFirstSegmentAppears(t *testing.T) {
	f := newSMSForwardScanFixture(t, "scan-device")
	second := multipartSegment(9, 2, "second", nil)
	first := multipartSegment(8, 1, "first ", nil)
	second.RawPDU, first.RawPDU = "AABB0902", "AABB0801"
	f.controller.setMessages("scan-device", second)
	f.observed("scan-device")
	f.expectPlans(0)
	// The newly observed #1 changes the concat storage-generation source id.
	// Its old #2 still proves this whole logical message belongs to baseline.
	f.controller.setMessages("scan-device", first, second)
	f.observed("scan-device")
	f.expectPlans(0)
	f.observed("scan-device")
	f.expectPlans(0)
	inbox, err := f.db.ListSMSMessages(f.ctx, store.SMSFilter{DeviceID: "scan-device"})
	if err != nil {
		t.Fatal(err)
	}
	complete := false
	for _, message := range inbox {
		if message.Body == "first second" && store.SMSForwardMessageReady(message) {
			complete = true
		}
	}
	if !complete {
		t.Fatal("baseline multipart was not preserved as a complete local Inbox message")
	}
	f.controller.setMessages("scan-device", first, second, smsForwardScanPlain(12, "independent later live message"))
	f.observed("scan-device")
	f.expectPlans(1)
}

func TestSMSForwardScanArchivePartialFamilySurvivesDisappearanceAndRecovery(t *testing.T) {
	for _, interruption := range []string{"successful empty snapshot", "ListSMS failure", "Host mode skip", "different card and back"} {
		t.Run(interruption, func(t *testing.T) {
			f := newSMSForwardScanFixture(t, "scan-device")
			oldSecond := multipartSegment(9, 2, "second", nil)
			oldSecond.RawPDU = "AABB0902"
			f.controller.setMessages("scan-device", oldSecond)
			f.observed("scan-device")
			f.expectPlans(0)
			// No current physical row can carry the old archive flag after this.
			f.controller.setMessages("scan-device")
			f.observed("scan-device")
			switch interruption {
			case "ListSMS failure":
				f.controller.listingErrors["scan-device"] = errors.New("synthetic interruption after old part disappeared")
				f.observed("scan-device")
				delete(f.controller.listingErrors, "scan-device")
			case "Host mode skip":
				f.config("scan-device", func(c *store.Device) { c.IMSMode = store.IMSModeHost })
				f.observed("scan-device")
				f.config("scan-device", func(c *store.Device) { c.IMSMode = store.IMSModeModule })
			case "different card and back":
				f.controller.changeSnapshot("scan-device", func(s *device.Snapshot) { s.ICCID = "8986000000000000002"; s.IMSI = "001010000000002" })
				f.observed("scan-device")
				f.controller.changeSnapshot("scan-device", func(s *device.Snapshot) { s.ICCID = "8986000000000000001"; s.IMSI = "001010000000001" })
			}
			// Establish a valid empty baseline again. A new-scope/failed-scan reset
			// alone therefore cannot explain the following conservative decision.
			f.observed("scan-device")
			first, second := multipartSegment(20, 1, "first ", nil), multipartSegment(21, 2, "second", nil)
			first.RawPDU, second.RawPDU = "AABB2001", "AABB2102"
			f.controller.setMessages("scan-device", first, second)
			f.observed("scan-device")
			f.expectPlans(0)
			f.observed("scan-device")
			f.expectPlans(0)
			// An unrelated, complete live family is not an archive-incomplete
			// family and must not poison later independent use of its reference.
			liveFirst, liveSecond := multipartSegment(30, 1, "live first ", nil), multipartSegment(31, 2, "live second", nil)
			liveFirst.Concat.Reference, liveSecond.Concat.Reference = 42, 42
			liveFirst.RawPDU, liveSecond.RawPDU = "AABB3001", "AABB3102"
			f.controller.setMessages("scan-device", first, second, liveFirst, liveSecond)
			f.observed("scan-device")
			f.expectPlans(1)
			f.observed("scan-device")
			f.expectPlans(1)
			f.controller.setMessages("scan-device")
			f.observed("scan-device")
			liveFirst.Index, liveSecond.Index = 40, 41
			liveFirst.RawPDU, liveSecond.RawPDU = "AABB4001", "AABB4102"
			f.controller.setMessages("scan-device", liveFirst, liveSecond)
			f.observed("scan-device")
			f.expectPlans(2)
		})
	}
}

func TestSMSForwardScanLivePartialRescanDoesNotCreateAnEmptyArchivePlan(t *testing.T) {
	f := newSMSForwardScanFixture(t, "scan-device")
	f.observed("scan-device")
	first, second := multipartSegment(5, 1, "first ", nil), multipartSegment(6, 2, "second", nil)
	first.RawPDU, second.RawPDU = "AABB0501", "AABB0602"
	f.controller.setMessages("scan-device", first)
	f.observed("scan-device")
	f.expectPlans(0)
	// Seeing only the same live first part again is not a manual/archive
	// receive and must not create an empty receipt before the last part.
	f.observed("scan-device")
	f.expectPlans(0)
	f.controller.setMessages("scan-device", first, second)
	f.observed("scan-device")
	f.expectPlans(1)
	f.observed("scan-device")
	f.expectPlans(1)
	inbox, err := f.db.ListSMSMessages(f.ctx, store.SMSFilter{DeviceID: "scan-device"})
	if err != nil || len(inbox) != 1 || inbox[0].Body != "first second" || !store.SMSForwardMessageReady(inbox[0]) {
		t.Fatal("repeated live partial did not become one complete local Inbox message:", err)
	}
}

func TestSMSForwardScanArchiveFamilyDeadlineIsFirstSeenAndExpiresWithoutRenewal(t *testing.T) {
	t0 := time.Date(2026, time.October, 24, 0, 0, 0, 0, time.UTC)
	scope := smsForwardScanScope{physicalID: "physical", imei: "867394042309830", iccid: "8986000000000000001", imsi: "001010000000001", mode: store.IMSModeModule}
	partial := multipartSegment(9, 2, "old second", nil)
	partial.RawPDU = "AABB0902"
	messages := []device.SMSMessage{partial}
	decision, first, trackable := smsForwardScanDecisionsAt(smsForwardScan{}, scope, messages, modemSMSConcatSources(messages), true, t0)
	key := smsForwardScanFamilyKey(scope, partial, firstNonEmpty(partial.From, partial.To, "未知发送方"))
	if !trackable || decision[modemSMSScanRowKey(partial)] || !first.families[key].Equal(t0.Add(24*time.Hour)) {
		t.Fatal("archive incomplete family did not receive its original bounded first-seen deadline")
	}
	_, repeated, trackable := smsForwardScanDecisionsAt(first, scope, messages, modemSMSConcatSources(messages), true, t0.Add(time.Hour))
	if !trackable || !repeated.families[key].Equal(first.families[key]) {
		t.Fatal("repeating an old partial renewed its original protection deadline")
	}
	_, missing, trackable := smsForwardScanDecisionsAt(repeated, scope, nil, modemSMSConcatSources(nil), true, t0.Add(20*time.Hour))
	if !trackable || !missing.families[key].Equal(first.families[key]) {
		t.Fatal("physical disappearance erased or renewed the still-active archive family")
	}
	_, expired, trackable := smsForwardScanDecisionsAt(missing, scope, nil, modemSMSConcatSources(nil), true, t0.Add(25*time.Hour))
	if !trackable || len(expired.families) != 0 {
		t.Fatal("expired archive family was retained or renewed after its bounded lifetime")
	}
}

func TestSMSForwardScanBudgetOverflowLatchesConservativePlansButKeepsInbox(t *testing.T) {
	now := time.Date(2026, time.October, 24, 0, 0, 0, 0, time.UTC)
	scope := smsForwardScanScope{physicalID: "scan-device", imei: "867394042309830", iccid: "8986000000000000001", imsi: "001010000000001", mode: store.IMSModeModule}
	previous := smsForwardScan{scope: scope, ready: true, rows: map[string]smsForwardScanRow{}, families: map[string]time.Time{}}
	for index := 0; index < 4096; index++ {
		digest := sha256.Sum256([]byte("held-family-" + strconv.Itoa(index)))
		previous.families[hex.EncodeToString(digest[:])] = now.Add(24 * time.Hour)
	}
	message := smsForwardScanPlain(1, "new source beyond remembered-family budget")
	decision, blocked, trackable := smsForwardScanDecisionsAt(previous, scope, []device.SMSMessage{message}, modemSMSConcatSources([]device.SMSMessage{message}), true, now)
	if trackable || !blocked.blocked || blocked.ready || len(decision) != 0 {
		t.Fatal("rows plus protected families over budget discarded protection and stayed live")
	}
	many := make([]device.SMSMessage, 4097)
	for index := range many {
		many[index] = smsForwardScanPlain(index, "synthetic oversized snapshot")
	}
	decision, tooMany, trackable := smsForwardScanDecisionsAt(smsForwardScan{}, scope, many, modemSMSConcatSources(many), true, now)
	if trackable || !tooMany.blocked || tooMany.ready || len(decision) != 0 {
		t.Fatal("oversized physical snapshot remained eligible for live plans")
	}
	f := newSMSForwardScanFixture(t, "scan-device")
	f.server.smsSyncMu.Lock()
	f.server.rememberSMSForwardScan("scan-device", blocked)
	f.server.smsSyncMu.Unlock()
	if !f.server.smsForwardScanBlocked {
		t.Fatal("forgetting over-budget source protections did not latch the conservative process guard")
	}
	f.controller.setMessages("scan-device", message)
	f.observed("scan-device")
	f.expectPlans(0)
	inbox, err := f.db.ListSMSMessages(f.ctx, store.SMSFilter{DeviceID: "scan-device"})
	if err != nil || len(inbox) != 1 || inbox[0].Body != message.Text {
		t.Fatal("conservative forwarding budget guard disabled normal local Inbox capture:", err)
	}
	f.controller.setMessages("scan-device", message, smsForwardScanPlain(2, "another local source while blocked"))
	f.observed("scan-device")
	f.expectPlans(0)
}

func TestSMSForwardScanIncompletePhysicalIdentityNeverBuildsLiveBaseline(t *testing.T) {
	for _, field := range []string{"actual IMEI", "ICCID", "IMSI"} {
		t.Run(field, func(t *testing.T) {
			f := newSMSForwardScanFixture(t, "scan-device")
			f.controller.changeSnapshot("scan-device", func(s *device.Snapshot) {
				switch field {
				case "actual IMEI":
					s.IMEI = ""
				case "ICCID":
					s.ICCID = ""
				default:
					s.IMSI = ""
				}
			})
			first, second, third := smsForwardScanPlain(1, "unknown physical origin"), smsForwardScanPlain(2, "still unknown physical origin"), smsForwardScanPlain(3, "known origin new observation")
			f.controller.setMessages("scan-device", first)
			f.observed("scan-device")
			f.controller.setMessages("scan-device", first, second)
			f.observed("scan-device")
			f.expectPlans(0)
			// The configured IMEI is already known, but is not actual-read proof.
			f.controller.changeSnapshot("scan-device", func(s *device.Snapshot) {
				s.IMEI = "867394042309830"
				s.ICCID = "8986000000000000001"
				s.IMSI = "001010000000001"
			})
			f.observed("scan-device")
			f.expectPlans(0)
			f.controller.setMessages("scan-device", first, second, third)
			f.observed("scan-device")
			f.expectPlans(1)
		})
	}
}

func TestSMSForwardScanIndependentDevicesDoNotShareQualification(t *testing.T) {
	f := newSMSForwardScanFixture(t, "scan-a", "scan-b")
	f.observed("")
	a, b := smsForwardScanPlain(1, "a during failure"), smsForwardScanPlain(1, "b valid live")
	f.controller.setMessages("scan-a", a)
	f.controller.setMessages("scan-b", b)
	f.controller.listingErrors["scan-a"] = errors.New("synthetic first-line failure")
	f.observed("")
	f.expectPlans(1)
	f.planMu.Lock()
	aCount, bCount := f.planDevices["scan-a"], f.planDevices["scan-b"]
	f.planMu.Unlock()
	if aCount != 0 || bCount != 1 {
		t.Fatal("first failed modem blocked or borrowed the second modem's baseline")
	}
	delete(f.controller.listingErrors, "scan-a")
	f.observed("")
	f.expectPlans(1)
	f.controller.setMessages("scan-a", a, smsForwardScanPlain(2, "a after recovery"))
	f.controller.setMessages("scan-b", b, smsForwardScanPlain(2, "b remains live"))
	f.observed("")
	f.expectPlans(3)
	f.planMu.Lock()
	aCount, bCount = f.planDevices["scan-a"], f.planDevices["scan-b"]
	f.planMu.Unlock()
	if aCount != 1 || bCount != 2 {
		t.Fatal("independent modem observation state was shared or reset globally")
	}
}

func TestSMSForwardScanCMTIOnlyWakesScanAndCannotBypassFirstBaseline(t *testing.T) {
	f := newSMSForwardScanFixture(t, "scan-device")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.server.StartSMSSyncLoop(ctx, time.Hour) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("SMS sync loop did not stop after cancellation")
		}
	}()
	old, newer := smsForwardScanPlain(1, "old storage on first CMTI"), smsForwardScanPlain(2, "new storage on second CMTI")
	f.controller.setMessages("scan-device", old)
	wake := f.db.SMSInserted()
	f.controller.arrivals <- struct{}{}
	select {
	case <-wake:
	case <-ctx.Done():
		t.Fatal("first CMTI did not persist local Inbox:", ctx.Err())
	}
	f.expectPlans(0)
	f.controller.setMessages("scan-device", old, newer)
	wake = f.db.SMSInserted()
	f.controller.arrivals <- struct{}{}
	select {
	case <-wake:
	case <-ctx.Done():
		t.Fatal("second CMTI did not persist newly observed Inbox:", ctx.Err())
	}
	f.expectPlans(1)
}

var _ DeviceController = (*smsForwardScanController)(nil)
