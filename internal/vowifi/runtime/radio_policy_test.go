package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type policyRadio struct {
	touches     atomic.Int32
	restores    atomic.Int32
	failRestore atomic.Bool
}

func (r *policyRadio) Snapshot(context.Context, string) (vowifi.RadioSnapshot, error) {
	r.touches.Add(1)
	return vowifi.RadioSnapshot{OperatingMode: 1, CellularDataEnabled: true}, nil
}
func (r *policyRadio) StopCellularData(context.Context, string) error { r.touches.Add(1); return nil }
func (r *policyRadio) EnterVoWiFiRFOff(context.Context, string) error { r.touches.Add(1); return nil }
func (r *policyRadio) Restore(context.Context, string, vowifi.RadioSnapshot) error {
	r.touches.Add(1)
	r.restores.Add(1)
	if r.failRestore.Load() {
		return errors.New("modem did not acknowledge RF restore")
	}
	return nil
}

func policyManager(t *testing.T, radio *policyRadio, policy *atomic.Value, tunnel vowifi.TunnelProvider) *Manager {
	return policyManagerWithRetry(t, radio, policy, tunnel, time.Hour)
}

func policyManagerWithRetry(t *testing.T, radio *policyRadio, policy *atomic.Value, tunnel vowifi.TunnelProvider, retry time.Duration) *Manager {
	t.Helper()
	m := New(Options{OperationTimeout: time.Second, RetryInitial: retry, RetryMaximum: retry})
	t.Cleanup(func() { radio.failRestore.Store(false); _ = m.Close(context.Background()) })
	o, err := vowifi.New(vowifi.Dependencies{
		SIM: fakeSIM{}, AKA: fakeAKA{}, Radio: radio, Proxy: fakeProxy{},
		Tunnel: tunnel, IMS: fakeIMSProvider{}, Phones: fakePhones{},
	}, vowifi.Options{DeviceID: "line", CleanupTimeout: time.Second,
		ResolveRadioPolicy: func(context.Context) (vowifi.RadioPolicy, error) { return policy.Load().(vowifi.RadioPolicy), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(o); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCoexistWiFiManagerAutomaticRecoveryKeepsCellularOwner(t *testing.T) {
	radio := &policyRadio{}
	var policy atomic.Value
	policy.Store(vowifi.RadioPolicyCoexist)
	tunnel := &flakyTunnelProvider{failures: 1}
	m := policyManagerWithRetry(t, radio, &policy, tunnel, 10*time.Millisecond)
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	state := waitPolicyManager(t, m, func(s vowifi.State) bool { return s.IMSReady })
	if state.Attempt != 2 || tunnel.Attempts() != 2 || radio.touches.Load() != 0 {
		t.Fatalf("Wi-Fi recovery changed cellular or did not retry: %+v, starts=%d, radio=%d", state, tunnel.Attempts(), radio.touches.Load())
	}
}

func waitPolicyManager(t *testing.T, m *Manager, match func(vowifi.State) bool) vowifi.State {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		item := m.entries["line"]
		busy := item.busy
		m.mu.Unlock()
		state, err := m.State("line")
		if err == nil && !busy && match(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	state, _ := m.State("line")
	t.Fatalf("manager did not settle: %+v", state)
	return vowifi.State{}
}

func TestCoexistWiFiManagerStopAcknowledgesOnlyWiFiCleanup(t *testing.T) {
	radio := &policyRadio{}
	var policy atomic.Value
	policy.Store(vowifi.RadioPolicyCoexist)
	m := policyManager(t, radio, &policy, fakeTunnelProvider{})
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	waitPolicyManager(t, m, func(s vowifi.State) bool { return s.IMSReady })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.StopDevice(ctx, "line"); err != nil {
		t.Fatal(err)
	}
	// The maintenance fence suppresses background and user enable requests
	// until the caller has finished applying the independent line policy.
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	waitPolicyManager(t, m, func(s vowifi.State) bool { return !s.Enabled && s.Phase == vowifi.PhaseIdle })
	if radio.touches.Load() != 0 {
		t.Fatal("stopping Wi-Fi changed the cellular owner")
	}
	m.EndMaintenance("line")
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	waitPolicyManager(t, m, func(s vowifi.State) bool { return s.IMSReady })
	if radio.touches.Load() != 0 {
		t.Fatal("restarting Wi-Fi changed the cellular owner")
	}
}

func TestWiFiManagerRetainedRadioCleanupDoesNotSpinOrTransferOwnership(t *testing.T) {
	radio := &policyRadio{}
	var policy atomic.Value
	policy.Store(vowifi.RadioPolicyExclusive)
	m := policyManager(t, radio, &policy, fakeTunnelProvider{})
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	waitPolicyManager(t, m, func(s vowifi.State) bool { return s.IMSReady })
	radio.failRestore.Store(true)
	if _, err := m.RequestEnabled("line", false); err != nil {
		t.Fatal(err)
	}
	waitPolicyManager(t, m, func(s vowifi.State) bool { return !s.Enabled && s.LastErrorClass == "radio_cleanup_pending" })
	policy.Store(vowifi.RadioPolicyCoexist)
	before := radio.touches.Load()
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	state := waitPolicyManager(t, m, func(s vowifi.State) bool { return !s.Enabled && s.Phase == vowifi.PhaseFailed })
	if state.RadioPolicy != vowifi.RadioPolicyExclusive || radio.touches.Load() != before {
		t.Fatalf("new intent lost the old radio lease: %+v", state)
	}
	// Failed StopDevice must keep the fence. Recovery retries the old restore;
	// it never replaces that transaction with a coexist enable.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.StopDevice(ctx, "line"); !errors.Is(err, vowifi.ErrCleanupIncomplete) {
		t.Fatalf("stop ACK: %v", err)
	}
	radio.failRestore.Store(false)
	if err := m.StopDevice(ctx, "line"); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	m.EndMaintenance("line")
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	state = waitPolicyManager(t, m, func(s vowifi.State) bool { return s.IMSReady })
	if state.RadioPolicy != vowifi.RadioPolicyCoexist {
		t.Fatalf("new policy was not applied after ACK: %+v", state)
	}
}

func TestWiFiManagerCloseRetriesRetainedRadioCleanup(t *testing.T) {
	radio := &policyRadio{}
	var policy atomic.Value
	policy.Store(vowifi.RadioPolicyExclusive)
	m := policyManager(t, radio, &policy, fakeTunnelProvider{})
	if _, err := m.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	waitPolicyManager(t, m, func(s vowifi.State) bool { return s.IMSReady })
	radio.failRestore.Store(true)
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.Close(context.Background()); !errors.Is(err, vowifi.ErrCleanupIncomplete) {
			t.Fatalf("close %d acknowledged an un-restored radio: %v", attempt+1, err)
		}
	}
	radio.failRestore.Store(false)
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("final cleanup: %v", err)
	}
	if radio.restores.Load() != 3 {
		t.Fatalf("retained restore attempts = %d, want 3", radio.restores.Load())
	}
	if _, err := m.RequestEnabled("line", true); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed manager was restarted: %v", err)
	}
}
