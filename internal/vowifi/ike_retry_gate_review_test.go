package vowifi

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The preceding wire tests prove which provider outcomes are authenticated.
// Here the trusted provider seam drives the real Orchestrator lifecycle. No
// hypothetical storage/gate API is injected and no real device is involved.
type operatorRetryReviewProvider struct {
	mode  string
	calls int
}

func (p *operatorRetryReviewProvider) Start(_ context.Context, request TunnelRequest) (TunnelSession, error) {
	p.calls++
	if p.mode == "untrusted-enable" {
		return nil, errors.New("unverified NETWORK_FAILURE BACKOFF_TIMER=does not expire")
	}
	encoded := byte(0x01) // ten minutes, far beyond the test's total deadline.
	if p.mode == "zero-enable" {
		encoded = 0
	}
	if p.mode == "deactivated-enable" || p.mode == "toggle-deactivated" {
		encoded = 0xE0
	}
	return nil, &EPDGNetworkRejection{
		Cause:      errors.New("synthetic authenticated NETWORK_FAILURE"),
		NotifyType: 10500, Scope: EPDGRejectionEndpoint,
		Backoff: DecodeIKEBackoffTimer([]byte{1, encoded}),
		EPDG:    request.EPDG, Endpoint: "192.0.2.9:500", APN: "ims",
	}
}

func TestOperatorRetryGateActualLifecycleCannotBypassAuthenticatedBackoff(t *testing.T) {
	for _, mode := range []string{"timed-enable", "timed-retry", "timed-reconnect", "deactivated-enable", "toggle-deactivated", "zero-enable", "untrusted-enable"} {
		t.Run(mode, func(t *testing.T) {
			env := newFakeEnvironment()
			deps := discoveryDependencies(env)
			provider := &operatorRetryReviewProvider{mode: mode}
			deps.Tunnel = provider
			o, err := New(deps, Options{DeviceID: "isolated-retry-review"})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Disable(context.Background())
			state, first := o.Enable(context.Background())
			if first == nil || state.Phase != PhaseFailed || provider.calls != 1 {
				t.Fatalf("test never reached the original IKE failure: phase=%s calls=%d err=%v", state.Phase, provider.calls, first)
			}
			var rejection *EPDGNetworkRejection
			if errors.As(first, &rejection) != (mode != "untrusted-enable") {
				t.Fatalf("first rejection evidence lost: %v", first)
			}
			readyBefore := env.callCount("aka.ready")
			if mode == "toggle-deactivated" {
				if _, err := o.Disable(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			switch mode {
			case "timed-retry":
				_, err = o.Retry(ctx)
			case "timed-reconnect":
				_, err = o.Reconnect(ctx)
			default:
				_, err = o.Enable(ctx)
			}
			if err == nil {
				t.Fatal("failed provider unexpectedly became registered")
			}
			if mode == "zero-enable" || mode == "untrusted-enable" {
				if provider.calls != 2 {
					t.Fatalf("positive control was blanket-paused: calls=%d err=%v", provider.calls, err)
				}
			} else {
				if provider.calls != 1 {
					t.Fatalf("authenticated backoff bypassed by %s: IKE attempts=%d, want 1", mode, provider.calls)
				}
				if env.callCount("aka.ready") != readyBefore {
					t.Fatal("backoff re-probed AKA readiness")
				}
			}
		})
	}
}
