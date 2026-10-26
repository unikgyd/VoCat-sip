package integration

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type retryIntegrationClock struct{ now vowifi.IKERetryTime }

func (c *retryIntegrationClock) Snapshot() (vowifi.IKERetryTime, error) { return c.now, nil }
func retryIntegrationTime() *retryIntegrationClock {
	return &retryIntegrationClock{vowifi.IKERetryTime{Wall: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Boot: strings.Repeat("c", 32), Uptime: time.Hour}}
}
func retryIntegrationProof(target vowifi.IKERetryTarget) vowifi.EPDGNetworkRejection {
	return vowifi.EPDGNetworkRejection{Cause: errors.New("synthetic authenticated rejection"), NotifyType: 10500, Scope: vowifi.EPDGRejectionEndpoint, Backoff: vowifi.DecodeIKEBackoffTimer([]byte{1, 1}), EPDG: target.Host, Endpoint: "192.0.2.9", APN: target.APN}
}

func TestIKERetrySQLiteReopenKeepsTimerAndPrivateNamespace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	clock := retryIntegrationTime()
	g := vowifi.NewIKERetryGate(IKERetryStore{db}, clock)
	target := vowifi.IKERetryTarget{Host: "one.example", PLMN: "23415", APN: "ims"}
	card, err := g.Prepare(ctx, "slot-a", "card-a")
	if err != nil {
		t.Fatal(err)
	}
	token, err := g.Begin(ctx, card, target)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Observe(ctx, card, target, token, retryIntegrationProof(target)); err != nil {
		t.Fatal(err)
	}
	if err = g.Complete(ctx, card, token); err != nil {
		t.Fatal(err)
	}
	setting, err := db.AppSetting(ctx, store.IKERetryJournalKey)
	if err != nil || !setting.Sensitive || strings.Contains(string(setting.Value), "card-a") || strings.Contains(string(setting.Value), "slot-a") {
		t.Fatalf("private namespace: sensitive=%v err=%v", setting.Sensitive, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock.now.Uptime += time.Minute
	clock.now.Wall = clock.now.Wall.Add(72 * time.Hour)
	g = vowifi.NewIKERetryGate(IKERetryStore{db}, clock)
	card, err = g.Prepare(ctx, "slot-a", "card-a")
	if err != nil {
		t.Fatal(err)
	}
	var blocked *vowifi.IKERetryBlockedError
	if err = g.Permit(ctx, card, target, ""); !errors.As(err, &blocked) || blocked.Status.AfterSeconds != 540 {
		t.Fatalf("reopen: %+v %v", blocked, err)
	}
}

type retryIntegrationDevice struct{ ready int }

func (d *retryIntegrationDevice) ReadIdentity(context.Context, string) (vowifi.SIMIdentity, error) {
	return vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"}, nil
}
func (d *retryIntegrationDevice) CheckReady(context.Context, vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	d.ready++
	return vowifi.AKAEvidence{Ready: true}, nil
}
func (d *retryIntegrationDevice) Authenticate(context.Context, vowifi.SIMIdentity, vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	panic("no hardware AKA in retry integration test")
}
func (d *retryIntegrationDevice) Snapshot(context.Context, string) (vowifi.RadioSnapshot, error) {
	return vowifi.RadioSnapshot{OperatingMode: 1}, nil
}
func (d *retryIntegrationDevice) EnterVoWiFiRFOff(context.Context, string) error { return nil }
func (d *retryIntegrationDevice) StopCellularData(context.Context, string) error { return nil }
func (d *retryIntegrationDevice) Restore(context.Context, string, vowifi.RadioSnapshot) error {
	return nil
}
func (d *retryIntegrationDevice) Resolve(context.Context, vowifi.ProxyRequest) (vowifi.ProxyRoute, error) {
	return vowifi.ProxyRoute{Mode: vowifi.ProxyModeDirect}, nil
}
func (d *retryIntegrationDevice) Start(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
	panic("IMS must never start after rejected IKE")
}
func (d *retryIntegrationDevice) SaveAssociatedNumber(context.Context, vowifi.PhoneRecord) error {
	return nil
}

type retryIntegrationTunnel struct {
	calls         int
	beforeObserve func()
	cancel        context.CancelFunc
}

func (p *retryIntegrationTunnel) Start(ctx context.Context, r vowifi.TunnelRequest) (vowifi.TunnelSession, error) {
	p.calls++
	if err := r.BeforeIKE(ctx); err != nil {
		return nil, err
	}
	proof := retryIntegrationProof(vowifi.IKERetryTarget{Host: r.EPDG, APN: "ims"})
	if p.beforeObserve != nil {
		p.beforeObserve()
	}
	if p.cancel != nil {
		p.cancel()
	}
	err := r.ObserveNetworkRejection(proof)
	return nil, errors.Join(&proof, err, ctx.Err())
}
func retryIntegrationOrchestrator(t *testing.T, db *store.Store, clock vowifi.IKERetryClock, p *retryIntegrationTunnel, d *retryIntegrationDevice) *vowifi.Orchestrator {
	t.Helper()
	o, err := vowifi.New(vowifi.Dependencies{IKERetry: IKERetryStore{db}, SIM: d, AKA: d, Radio: d, Proxy: d, Tunnel: p, IMS: d, Phones: d}, vowifi.Options{DeviceID: "slot-a", IKERetryClock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestIKERetryActualOrchestratorPersistsBeforeCanceledCallerAndNewInstance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	clock := retryIntegrationTime()
	caller, cancel := context.WithCancel(ctx)
	p := &retryIntegrationTunnel{cancel: cancel}
	d := &retryIntegrationDevice{}
	o := retryIntegrationOrchestrator(t, db, clock, p, d)
	state, err := o.Enable(caller)
	if err == nil || p.calls != 1 || state.IKERetry == nil {
		t.Fatalf("first result: %+v %v", state.IKERetry, err)
	}
	_, _ = o.Disable(ctx)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p = &retryIntegrationTunnel{}
	d = &retryIntegrationDevice{}
	o = retryIntegrationOrchestrator(t, db, clock, p, d)
	defer o.Close(ctx)
	state, err = o.Enable(ctx)
	if !errors.Is(err, vowifi.ErrIKERetryBlocked) || p.calls != 0 || d.ready != 0 || state.IKERetry == nil || state.IKERetry.AfterSeconds != 600 {
		t.Fatalf("restart bypassed cancellation commit: calls=%d ready=%d state=%+v err=%v", p.calls, d.ready, state.IKERetry, err)
	}
}

func TestIKERetryFailedObservationRetainsDurableUnknownAdmissionAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	clock := retryIntegrationTime()
	p := &retryIntegrationTunnel{beforeObserve: func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}}
	d := &retryIntegrationDevice{}
	o := retryIntegrationOrchestrator(t, db, clock, p, d)
	state, err := o.Enable(ctx)
	if err == nil || state.IKERetry == nil || !state.IKERetry.UntilCardEvent || p.calls != 1 {
		t.Fatalf("failed save was not paused: %+v %v", state.IKERetry, err)
	}
	_, _ = o.Disable(ctx)
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p = &retryIntegrationTunnel{}
	d = &retryIntegrationDevice{}
	o = retryIntegrationOrchestrator(t, db, clock, p, d)
	defer o.Close(ctx)
	state, err = o.Enable(ctx)
	if !errors.Is(err, vowifi.ErrIKERetryBlocked) || p.calls != 0 || d.ready != 0 || state.IKERetry == nil || state.IKERetry.Source != "uncertain-attempt" {
		t.Fatalf("unknown result lost on restart: calls=%d ready=%d state=%+v err=%v", p.calls, d.ready, state.IKERetry, err)
	}
}

func TestIKERetryStoreRefusesNonPrivateOrDamagedRecord(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "retry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sensitive := range []bool{false, true} {
		if err = db.UpsertAppSetting(ctx, store.AppSetting{Key: store.IKERetryJournalKey, Value: []byte(`{}`), Sensitive: sensitive}); err != nil {
			t.Fatal(err)
		}
		g := vowifi.NewIKERetryGate(IKERetryStore{db}, retryIntegrationTime())
		if _, err = g.Prepare(ctx, "slot", "card"); !errors.Is(err, vowifi.ErrIKERetryStore) {
			t.Fatalf("invalid/private=%v accepted: %v", sensitive, err)
		}
	}
}

func TestIKERetrySQLiteConcurrentWritersPreserveAllCardsAndRejectStaleCAS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry.sqlite")
	a, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	adapters := []IKERetryStore{{a}, {b}}
	clock := retryIntegrationTime()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g := vowifi.NewIKERetryGate(adapters[i%2], clock)
			identity := strings.Repeat(string(rune('a'+i)), 3)
			card, err := g.Prepare(ctx, identity, identity)
			if err != nil {
				t.Error(err)
				return
			}
			target := vowifi.IKERetryTarget{Host: "one.example", PLMN: "23415", APN: "ims"}
			token, err := g.Begin(ctx, card, target)
			if err != nil {
				t.Error(err)
				return
			}
			if err = g.Observe(ctx, card, target, token, retryIntegrationProof(target)); err != nil {
				t.Error(err)
				return
			}
			if err = g.Complete(ctx, card, token); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	before, err := adapters[0].LoadIKERetry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Entries map[string]json.RawMessage
		Active  map[string]json.RawMessage
	}
	if err = json.Unmarshal(before, &document); err != nil || len(document.Entries) != 8 || len(document.Active) != 0 {
		t.Fatalf("lost concurrent entries: %d active=%d err=%v", len(document.Entries), len(document.Active), err)
	}
	g := vowifi.NewIKERetryGate(adapters[0], clock)
	if _, err = g.Prepare(ctx, "another-slot", "another-card"); err != nil {
		t.Fatal(err)
	}
	if committed, err := adapters[1].CompareIKERetry(ctx, before, before); err != nil || committed {
		t.Fatalf("stale full-document CAS committed: %v %v", committed, err)
	}
}
