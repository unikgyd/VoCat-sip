package vowifi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

type gatewayTunnelFunc func(context.Context, TunnelRequest) (TunnelSession, error)

func (f gatewayTunnelFunc) Start(c context.Context, r TunnelRequest) (TunnelSession, error) {
	return f(c, r)
}

type gatewayIMSFunc func(context.Context, IMSRequest) (IMSSession, error)

func (f gatewayIMSFunc) Start(c context.Context, r IMSRequest) (IMSSession, error) { return f(c, r) }

type gatewayTestTunnel struct {
	*fakeTunnelSession
	ip       string
	failures chan error
}

func (s *gatewayTestTunnel) Evidence() TunnelEvidence {
	e := s.fakeTunnelSession.Evidence()
	e.EPDGAddress = s.ip
	return e
}
func (s *gatewayTestTunnel) Failures() <-chan error { return s.failures }
func gatewayTimeout() error                         { return &IMSTransportError{Cause: context.DeadlineExceeded} }

func TestIMSGatewayErrorClassificationRequiresAllBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"tagged timeout", gatewayTimeout(), true}, {"EOF", &IMSTransportError{Cause: io.EOF}, true}, {"refused", &IMSTransportError{Cause: syscall.ECONNREFUSED}, true},
		{"aborted with provider proof", &IMSTransportError{Cause: syscall.ECONNABORTED}, true},
		{"raw abort", syscall.ECONNABORTED, false},
		{"abort plus unresolved cleanup", errors.Join(&IMSTransportError{Cause: syscall.ECONNABORTED}, ErrCleanupIncomplete), false},
		{"wrapped", fmt.Errorf("setup: %w", gatewayTimeout()), true}, {"two proven", errors.Join(gatewayTimeout(), gatewayTimeout()), true},
		{"raw timeout", context.DeadlineExceeded, false}, {"cancel", &IMSTransportError{Cause: context.Canceled}, false}, {"configuration", &IMSTransportError{Cause: ErrInvalidIdentity}, false},
		{"mixed outer", errors.Join(gatewayTimeout(), ErrCleanupIncomplete), false}, {"mixed cause", &IMSTransportError{Cause: errors.Join(context.DeadlineExceeded, ErrResponderAUTHRequired)}, false},
		{"nil", nil, false}, {"empty proof", &IMSTransportError{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IMSFallbackAllowed(tc.err); got != tc.want {
				t.Fatalf("allowed=%v want=%v", got, tc.want)
			}
		})
	}
	if EPDGFallbackAllowed(errors.Join(&EPDGAccessError{Cause: io.EOF}, ErrCleanupIncomplete)) {
		t.Fatal("pre-auth mixed failure was hidden")
	}
}

func TestIMSGatewayExcludesActualIPThenUsesNextHost(t *testing.T) {
	e := newFakeEnvironment()
	d := discoveryDependencies(e)
	memory := &discoveryMemory{options: EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"first.example", "second.example"}}}
	d.Discovery = memory
	var requests []TunnelRequest
	d.Tunnel = gatewayTunnelFunc(func(ctx context.Context, r TunnelRequest) (TunnelSession, error) {
		requests = append(requests, r)
		if r.EPDG == "first.example" && len(r.ExcludedEPDGAddresses) > 0 {
			if !reflect.DeepEqual(r.ExcludedEPDGAddresses, []string{"203.0.113.1"}) {
				t.Errorf("exclusions=%v", r.ExcludedEPDGAddresses)
			}
			return nil, ErrEPDGAddressesExhausted
		}
		ip := "203.0.113.1"
		if r.EPDG == "second.example" {
			ip = "203.0.113.2"
		}
		return &gatewayTestTunnel{fakeTunnelSession: &fakeTunnelSession{e}, ip: ip}, nil
	})
	imsStarts := 0
	d.IMS = gatewayIMSFunc(func(ctx context.Context, r IMSRequest) (IMSSession, error) {
		imsStarts++
		if imsStarts == 1 {
			return nil, gatewayTimeout()
		}
		return fakeIMSProvider{e}.Start(ctx, r)
	})
	o, err := New(d, Options{DeviceID: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	state, err := o.Enable(context.Background())
	if err != nil || !state.IMSReady || state.EPDG != "second.example" || state.EPDGAddress != "203.0.113.2" || len(requests) != 3 || imsStarts != 2 || len(memory.remembered) != 1 {
		t.Fatalf("state=%+v err=%v requests=%d IMS=%d", state, err, len(requests), imsStarts)
	}
	if len(requests[0].ExcludedEPDGAddresses) != 0 || len(requests[2].ExcludedEPDGAddresses) != 0 {
		t.Fatal("exclusions leaked across request/host")
	}
	for _, call := range []string{"sim.identity", "aka.ready", "radio.snapshot", "radio.rf_off", "proxy.resolve"} {
		if e.callCount(call) != 1 {
			t.Fatalf("%s repeated: %v", call, e.callsSnapshot())
		}
	}
	if e.callCount("radio.restore") != 0 {
		t.Fatal("fallback restored RF")
	}
}

func TestIMSGatewayRetryBudgetAndAuthenticationHomeStayBounded(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			e := newFakeEnvironment()
			if unknown {
				e.identity = uncertainMNCFixture(t)
			}
			d := discoveryDependencies(e)
			d.Discovery = &discoveryMemory{options: EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"one.example"}}}
			plan, err := BuildEPDGAttempts(e.identity, EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"one.example"}})
			if err != nil {
				t.Fatal(err)
			}
			expectedMNC := plan[0].Identity.HomeMNC
			starts, imsStarts := 0, 0
			d.Tunnel = gatewayTunnelFunc(func(_ context.Context, r TunnelRequest) (TunnelSession, error) {
				starts++
				if unknown && r.Identity.HomeMNC != expectedMNC {
					t.Fatal("IMS silence switched authentication home")
				}
				if unknown && len(r.ExcludedEPDGAddresses) > 0 {
					return nil, ErrEPDGAddressesExhausted
				}
				if len(r.ExcludedEPDGAddresses) != starts-1 {
					t.Fatal("exclusion list lost")
				}
				if len(r.ExcludedEPDGAddresses) > 0 && r.ExcludedEPDGAddresses[0] != "203.0.113.1" {
					t.Fatal("provider mutated owner exclusion state")
				}
				// Mutating a request cannot corrupt the owner's exclusion collection.
				if len(r.ExcludedEPDGAddresses) > 0 {
					r.ExcludedEPDGAddresses[0] = "203.0.113.250"
				}
				return &gatewayTestTunnel{fakeTunnelSession: &fakeTunnelSession{e}, ip: fmt.Sprintf("203.0.113.%d", starts)}, nil
			})
			d.IMS = gatewayIMSFunc(func(context.Context, IMSRequest) (IMSSession, error) { imsStarts++; return nil, gatewayTimeout() })
			o, err := New(d, Options{DeviceID: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Disable(context.Background())
			state, err := o.Enable(context.Background())
			if err == nil || state.IMSReady {
				t.Fatal("silence accepted")
			}
			wantStarts, wantIMS := 16, 16
			if unknown {
				wantStarts, wantIMS = 2, 1
			}
			if starts != wantStarts || imsStarts != wantIMS || e.callCount("tunnel.close") != wantIMS {
				t.Fatalf("starts=%d IMS=%d closes=%d", starts, imsStarts, e.callCount("tunnel.close"))
			}
		})
	}
}

func TestIMSGatewayFatalCleanupCancellationOrUnprovenPeerStops(t *testing.T) {
	for _, mode := range []string{"cleanup", "cancel", "no_ip", "reused_ip", "SIP_response", "plain_timeout", "mixed_exhaustion"} {
		t.Run(mode, func(t *testing.T) {
			e := newFakeEnvironment()
			d := discoveryDependencies(e)
			d.Discovery = &discoveryMemory{options: EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"one.example", "two.example"}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			starts, imsStarts := 0, 0
			d.Tunnel = gatewayTunnelFunc(func(context.Context, TunnelRequest) (TunnelSession, error) {
				starts++
				if starts == 2 && mode == "mixed_exhaustion" {
					return nil, errors.Join(ErrEPDGAddressesExhausted, ErrCleanupIncomplete)
				}
				ip := "203.0.113.1"
				if mode == "no_ip" {
					ip = ""
				}
				return &gatewayTestTunnel{fakeTunnelSession: &fakeTunnelSession{e}, ip: ip}, nil
			})
			if mode == "cleanup" {
				e.setFailure("tunnel.close", 1)
			}
			d.IMS = gatewayIMSFunc(func(context.Context, IMSRequest) (IMSSession, error) {
				imsStarts++
				switch mode {
				case "cancel":
					cancel()
				case "SIP_response":
					return &fakeIMSSession{e}, gatewayTimeout()
				case "plain_timeout":
					return nil, context.DeadlineExceeded
				}
				return nil, gatewayTimeout()
			})
			o, err := New(d, Options{DeviceID: "synthetic", CleanupTimeout: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Disable(context.Background())
			state, err := o.Enable(ctx)
			if err == nil || state.IMSReady {
				t.Fatal("fatal case registered")
			}
			want := 1
			if mode == "reused_ip" || mode == "mixed_exhaustion" {
				want = 2
			}
			if starts != want || imsStarts != 1 {
				t.Fatalf("fatal case retried: starts=%d IMS=%d", starts, imsStarts)
			}
			if mode == "cleanup" && len(state.CleanupErrors) != 1 {
				t.Fatalf("cleanup warning lost: %+v", state)
			}
		})
	}
}

func TestIMSGatewayRetiredNotifierCannotCancelReplacement(t *testing.T) {
	e := newFakeEnvironment()
	d := discoveryDependencies(e)
	oldFailure := make(chan error, 1)
	starts, imsStarts := 0, 0
	d.Discovery = &discoveryMemory{options: EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"one.example"}}}
	d.Tunnel = gatewayTunnelFunc(func(context.Context, TunnelRequest) (TunnelSession, error) {
		starts++
		s := &gatewayTestTunnel{fakeTunnelSession: &fakeTunnelSession{e}, ip: fmt.Sprintf("203.0.113.%d", starts)}
		if starts == 1 {
			s.failures = oldFailure
		}
		return s, nil
	})
	d.IMS = gatewayIMSFunc(func(ctx context.Context, r IMSRequest) (IMSSession, error) {
		imsStarts++
		if imsStarts == 1 {
			return nil, gatewayTimeout()
		}
		oldFailure <- errors.New("late error from retired tunnel")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
			return fakeIMSProvider{e}.Start(ctx, r)
		}
	})
	o, err := New(d, Options{DeviceID: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	state, err := o.Enable(context.Background())
	if err != nil || !state.IMSReady || starts != 2 {
		t.Fatalf("retired notifier cancelled replacement: %+v %v", state, err)
	}
}

func TestIMSGatewayConcurrentModulesKeepSeparateAttempts(t *testing.T) {
	var mu sync.Mutex
	tunnels, registrations := map[string]int{}, map[string]int{}
	contexts := make([]*Orchestrator, 0, 2)
	for i := 0; i < 2; i++ {
		e := newFakeEnvironment()
		e.identity.ICCID = fmt.Sprintf("894410000000000000%d", i)
		d := discoveryDependencies(e)
		d.Discovery = &discoveryMemory{options: EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"one.example"}}}
		d.Tunnel = gatewayTunnelFunc(func(_ context.Context, r TunnelRequest) (TunnelSession, error) {
			mu.Lock()
			tunnels[r.DeviceID]++
			n := tunnels[r.DeviceID]
			mu.Unlock()
			if len(r.ExcludedEPDGAddresses) != n-1 {
				return nil, errors.New("cross-module exclusions")
			}
			return &gatewayTestTunnel{fakeTunnelSession: &fakeTunnelSession{e}, ip: fmt.Sprintf("203.0.113.%d", n)}, nil
		})
		d.IMS = gatewayIMSFunc(func(ctx context.Context, r IMSRequest) (IMSSession, error) {
			mu.Lock()
			registrations[r.DeviceID]++
			n := registrations[r.DeviceID]
			mu.Unlock()
			if n == 1 {
				return nil, gatewayTimeout()
			}
			return fakeIMSProvider{e}.Start(ctx, r)
		})
		o, err := New(d, Options{DeviceID: fmt.Sprintf("modem-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		contexts = append(contexts, o)
		defer o.Disable(context.Background())
	}
	results := make(chan error, 2)
	for _, o := range contexts {
		go func() {
			state, err := o.Enable(context.Background())
			if err == nil && !state.IMSReady {
				err = errors.New("registration missing")
			}
			results <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("parallel modules blocked each other")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if tunnels["modem-0"] != 2 || tunnels["modem-1"] != 2 || registrations["modem-0"] != 2 || registrations["modem-1"] != 2 {
		t.Fatal("module attempts interfered")
	}
}
