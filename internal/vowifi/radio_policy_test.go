package vowifi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func radioPolicyOrchestrator(t *testing.T, env *fakeEnvironment, resolve func(context.Context) (RadioPolicy, error)) *Orchestrator {
	t.Helper()
	o := newTestOrchestratorWithOptions(t, env, Options{
		DeviceID: "independent-line", ResolveRadioPolicy: resolve, CleanupTimeout: time.Second,
	})
	t.Cleanup(func() { _, _ = o.Disable(context.Background()) })
	return o
}

func assertNoRadioOwnership(t *testing.T, env *fakeEnvironment) {
	t.Helper()
	for _, call := range env.callsSnapshot() {
		if strings.HasPrefix(call, "radio.") {
			t.Fatalf("Wi-Fi access touched the cellular owner's radio: %s (%v)", call, env.callsSnapshot())
		}
	}
}

func TestCoexistWiFiLifecycleNeverTakesCellularRadio(t *testing.T) {
	env := newFakeEnvironment() // Cellular data and CFUN=1 are already active.
	o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return RadioPolicyCoexist, nil })
	for round := 0; round < 2; round++ {
		state, err := o.Enable(context.Background())
		if err != nil || !state.IMSReady || !state.SMSReady || state.RadioPolicy != RadioPolicyCoexist {
			t.Fatalf("enable %d: %+v, %v", round, state, err)
		}
		state, err = o.Reconnect(context.Background())
		if err != nil || !state.IMSReady || state.RadioPolicy != RadioPolicyCoexist {
			t.Fatalf("reconnect %d: %+v, %v", round, state, err)
		}
		state, err = o.Disable(context.Background())
		if err != nil || state.Phase != PhaseIdle || state.RadioPolicy != "" || state.IMSReady {
			t.Fatalf("disable %d: %+v, %v", round, state, err)
		}
	}
	assertNoRadioOwnership(t, env)
	if env.callCount("ims.close") != 4 || env.callCount("tunnel.close") != 4 {
		t.Fatalf("Wi-Fi resources were not independently released: %v", env.callsSnapshot())
	}
}

func TestCoexistWiFiSetupFailureRetriesWithoutCellularMutation(t *testing.T) {
	for _, stage := range []string{"sim.identity", "aka.ready", "proxy.resolve", "tunnel.start", "ims.start", "ims.sms"} {
		t.Run(stage, func(t *testing.T) {
			env := newFakeEnvironment()
			env.setFailure(stage, 1)
			o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return RadioPolicyCoexist, nil })
			if state, err := o.Enable(context.Background()); err == nil || state.Phase != PhaseFailed {
				t.Fatalf("expected failed attempt: %+v, %v", state, err)
			}
			if state, err := o.Retry(context.Background()); err != nil || !state.IMSReady {
				t.Fatalf("retry: %+v, %v", state, err)
			}
			if _, err := o.Disable(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertNoRadioOwnership(t, env)
		})
	}
}

func TestCoexistWiFiRuntimeFailureReleasesOnlyWiFiSessions(t *testing.T) {
	env := newFakeEnvironment()
	env.tunnelFailures = make(chan error, 1)
	o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return RadioPolicyCoexist, nil })
	if _, err := o.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.tunnelFailures <- errors.New("Wi-Fi uplink removed")
	deadline := time.After(2 * time.Second)
	for {
		state := o.State()
		if state.Phase == PhaseFailed {
			if state.IMSReady || state.TunnelReady || !state.Enabled || state.RadioPolicy != RadioPolicyCoexist {
				t.Fatalf("stale readiness or policy after failure: %+v", state)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("Wi-Fi runtime did not observe the uplink failure")
		case <-time.After(time.Millisecond):
		}
	}
	if state, err := o.Retry(context.Background()); err != nil || !state.IMSReady {
		t.Fatalf("runtime retry: %+v, %v", state, err)
	}
	if _, err := o.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoRadioOwnership(t, env)
}

func TestCoexistWiFiDisableCancelsSetupWithoutRestoringCellular(t *testing.T) {
	env := newFakeEnvironment()
	env.blockAt = "tunnel.start"
	o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return RadioPolicyCoexist, nil })
	done := make(chan error, 1)
	go func() { _, err := o.Enable(context.Background()); done <- err }()
	select {
	case <-env.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("setup did not reach the tunnel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if state, err := o.Disable(ctx); err != nil || state.Enabled || state.Phase != PhaseIdle {
		t.Fatalf("disable: %+v, %v", state, err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("setup was not canceled: %v", err)
	}
	assertNoRadioOwnership(t, env)
}

func TestWiFiRadioPolicyResolutionFailsBeforeModemOrSIMIO(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy RadioPolicy
		err    error
	}{
		{"missing", "", nil}, {"unknown", "automatic", nil},
		{"storage-error", RadioPolicyCoexist, errors.New("policy storage unavailable")},
		{"canceled", RadioPolicyCoexist, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeEnvironment()
			o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return tc.policy, tc.err })
			state, err := o.Enable(context.Background())
			if err == nil || state.Phase != PhaseFailed || state.IMSReady || len(env.callsSnapshot()) != 0 {
				t.Fatalf("unresolved policy performed I/O: %+v, %v, %v", state, err, env.callsSnapshot())
			}
		})
	}
}

func TestWiFiRadioPolicyCannotChangeDuringReconnectOrFailedRetry(t *testing.T) {
	for _, initial := range []RadioPolicy{RadioPolicyExclusive, RadioPolicyCoexist} {
		for _, failFirst := range []bool{false, true} {
			name := string(initial)
			if failFirst {
				name += "-failed"
			}
			t.Run(name, func(t *testing.T) {
				env := newFakeEnvironment()
				policy := initial
				o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return policy, nil })
				if failFirst {
					env.setFailure("ims.start", 1)
				}
				_, err := o.Enable(context.Background())
				if (err != nil) != failFirst {
					t.Fatalf("initial enable: %v", err)
				}
				policy = RadioPolicyCoexist
				if initial == RadioPolicyCoexist {
					policy = RadioPolicyExclusive
				}
				before := env.callCount("sim.identity")
				var state State
				if failFirst {
					state, err = o.Retry(context.Background())
				} else {
					state, err = o.Reconnect(context.Background())
				}
				if !errors.Is(err, ErrRadioPolicyTransition) || state.RadioPolicy != initial || state.IMSReady {
					t.Fatalf("changed radio ownership without cleanup: %+v, %v", state, err)
				}
				if env.callCount("sim.identity") != before {
					t.Fatal("policy change started new SIM setup")
				}
				if _, err := o.Disable(context.Background()); err != nil {
					t.Fatal(err)
				}
				if state, err := o.Enable(context.Background()); err != nil || !state.IMSReady || state.RadioPolicy != policy {
					t.Fatalf("policy after acknowledged disable: %+v, %v", state, err)
				}
			})
		}
	}
}

func TestFailedRadioRestoreRetainsCheckpointAndBlocksPolicyChange(t *testing.T) {
	env := newFakeEnvironment()
	policy := RadioPolicyExclusive
	o := radioPolicyOrchestrator(t, env, func(context.Context) (RadioPolicy, error) { return policy, nil })
	if _, err := o.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.setFailure("radio.restore", 1)
	state, err := o.Disable(context.Background())
	if !errors.Is(err, ErrCleanupIncomplete) || state.Enabled || state.Phase != PhaseFailed || state.LastErrorClass != "radio_cleanup_pending" {
		t.Fatalf("failed restore was acknowledged: %+v, %v", state, err)
	}
	policy = RadioPolicyCoexist
	before := len(env.callsSnapshot())
	state, err = o.Enable(context.Background())
	if !errors.Is(err, ErrCleanupIncomplete) || len(env.callsSnapshot()) != before || state.RadioPolicy != RadioPolicyExclusive {
		t.Fatalf("new owner abandoned the radio checkpoint: %+v, %v", state, err)
	}
	if state, err = o.Disable(context.Background()); err != nil || state.Phase != PhaseIdle {
		t.Fatalf("retry restore: %+v, %v", state, err)
	}
	if env.callCount("radio.restore") != 2 || env.callCount("ims.close") != 1 || env.callCount("tunnel.close") != 1 {
		t.Fatalf("cleanup did not retry only the retained radio: %v", env.callsSnapshot())
	}
	if state, err = o.Enable(context.Background()); err != nil || !state.IMSReady || state.RadioPolicy != RadioPolicyCoexist {
		t.Fatalf("coexist after restore: %+v, %v", state, err)
	}
}

type checkpointRadio struct {
	fakeRadio
	restored []RadioSnapshot
}

func (r *checkpointRadio) Restore(ctx context.Context, id string, snapshot RadioSnapshot) error {
	r.restored = append(r.restored, snapshot)
	return r.fakeRadio.Restore(ctx, id, snapshot)
}

func TestFailedRadioRestoreRetriesOriginalCheckpoint(t *testing.T) {
	env := newFakeEnvironment()
	original := env.radioSnapshot
	o := radioPolicyOrchestrator(t, env, nil)
	radio := &checkpointRadio{fakeRadio: fakeRadio{env}}
	o.deps.Radio = radio
	if _, err := o.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.setFailure("radio.restore", 1)
	if _, err := o.Disable(context.Background()); !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatal(err)
	}
	// The modem's current RF state is not the pre-transaction checkpoint.
	env.radioSnapshot = RadioSnapshot{OperatingMode: 4}
	if _, err := o.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(radio.restored) != 2 || radio.restored[0] != original || radio.restored[1] != original || env.callCount("radio.snapshot") != 1 {
		t.Fatalf("cleanup replaced the original checkpoint: %+v", radio.restored)
	}
}

func TestFailedRadioRestoreAlsoBlocksReconnect(t *testing.T) {
	env := newFakeEnvironment()
	o := radioPolicyOrchestrator(t, env, nil)
	if _, err := o.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.setFailure("radio.restore", 1)
	if _, err := o.Disable(context.Background()); !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatal(err)
	}
	before := len(env.callsSnapshot())
	state, err := o.Reconnect(context.Background())
	if !errors.Is(err, ErrCleanupIncomplete) || state.Enabled || state.IMSReady || len(env.callsSnapshot()) != before {
		t.Fatalf("reconnect bypassed unacknowledged radio cleanup: %+v, %v, calls=%v", state, err, env.callsSnapshot()[before:])
	}
}
