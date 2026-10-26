package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type retryRuntimeProvider struct {
	calls   atomic.Int32
	encoded byte
}

func (p *retryRuntimeProvider) Start(_ context.Context, r vowifi.TunnelRequest) (vowifi.TunnelSession, error) {
	p.calls.Add(1)
	return nil, &vowifi.EPDGNetworkRejection{Cause: errors.New("synthetic network rejection"), NotifyType: 10500, Scope: vowifi.EPDGRejectionEndpoint, Backoff: vowifi.DecodeIKEBackoffTimer([]byte{1, p.encoded}), EPDG: r.EPDG, APN: "ims", Endpoint: "192.0.2.9"}
}
func retryRuntimeWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("runtime condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIKERetryRuntimeHonorsLongWaitAndStopReleasesPendingWorker(t *testing.T) {
	for _, encoded := range []byte{1, 0xE0} {
		t.Run(string(vowifi.DecodeIKEBackoffTimer([]byte{1, encoded}).Status), func(t *testing.T) {
			p := &retryRuntimeProvider{encoded: encoded}
			o, err := vowifi.New(vowifi.Dependencies{SIM: fakeSIM{}, AKA: fakeAKA{}, Radio: fakeRadio{}, Proxy: fakeProxy{}, Tunnel: p, IMS: fakeIMSProvider{}, Phones: fakePhones{}}, vowifi.Options{DeviceID: "retry"})
			if err != nil {
				t.Fatal(err)
			}
			m := New(Options{RetryInitial: 5 * time.Millisecond, RetryMaximum: 10 * time.Millisecond})
			if err = m.Register(o); err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			if _, err = m.RequestEnabled("retry", true); err != nil {
				t.Fatal(err)
			}
			retryRuntimeWait(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return p.calls.Load() == 1 && !m.entries["retry"].busy })
			time.Sleep(50 * time.Millisecond)
			if p.calls.Load() != 1 {
				t.Fatalf("short retry ladder bypassed carrier wait: %d", p.calls.Load())
			}
			state, err := m.State("retry")
			if err != nil || state.IKERetry == nil {
				t.Fatal("missing retry status", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err = m.StopDevice(ctx, "retry"); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			pending := m.entries["retry"].autoRetryPending
			m.mu.Unlock()
			if pending {
				t.Fatal("disabled line retained a long retry worker")
			}
		})
	}
}

func TestIKERetryRuntimeZeroStillUsesNormalAutomaticRetry(t *testing.T) {
	p := &retryRuntimeProvider{}
	o, err := vowifi.New(vowifi.Dependencies{SIM: fakeSIM{}, AKA: fakeAKA{}, Radio: fakeRadio{}, Proxy: fakeProxy{}, Tunnel: p, IMS: fakeIMSProvider{}, Phones: fakePhones{}}, vowifi.Options{DeviceID: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{RetryInitial: 5 * time.Millisecond, RetryMaximum: 10 * time.Millisecond})
	if err = m.Register(o); err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	if _, err = m.RequestEnabled("retry", true); err != nil {
		t.Fatal(err)
	}
	retryRuntimeWait(t, func() bool { return p.calls.Load() >= 2 })
}
