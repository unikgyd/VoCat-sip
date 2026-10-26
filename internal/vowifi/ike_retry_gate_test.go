package vowifi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type retryTestClock struct {
	mu  sync.Mutex
	now IKERetryTime
}

func newRetryTestClock() *retryTestClock {
	return &retryTestClock{now: IKERetryTime{Wall: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Boot: strings.Repeat("a", 32), Uptime: time.Hour}}
}
func (c *retryTestClock) Snapshot() (IKERetryTime, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now, nil
}
func (c *retryTestClock) advance(uptime, wall time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now.Uptime += uptime
	c.now.Wall = c.now.Wall.Add(wall)
}
func retryTestPrepare(t *testing.T, g *IKERetryGate, device, card string) IKERetryCard {
	t.Helper()
	binding, err := g.Prepare(context.Background(), device, card)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
func retryTestObserve(t *testing.T, g *IKERetryGate, card IKERetryCard, target IKERetryTarget, code uint16, data []byte) {
	t.Helper()
	ctx := context.Background()
	token, err := g.Begin(ctx, card, target)
	if err != nil {
		t.Fatal(err)
	}
	scope := EPDGRejectionScope(retryScope(code))
	timer := IKEBackoffTimer{Status: IKEBackoffMissing}
	if data != nil {
		timer = DecodeIKEBackoffTimer(data)
	}
	err = g.Observe(ctx, card, target, token, EPDGNetworkRejection{NotifyType: code, Scope: scope, Backoff: timer, EPDG: target.Host, APN: target.APN, Endpoint: "192.0.2.9"})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Complete(ctx, card, token); err != nil {
		t.Fatal(err)
	}
}
func retryTestBlocked(t *testing.T, err error) *IKERetryBlockedError {
	t.Helper()
	var blocked *IKERetryBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("not blocked: %v", err)
	}
	return blocked
}

func TestIKERetryGateMonotonicExpirySurvivesWallClockChangesAndNewInstances(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	clock := newRetryTestClock()
	g := NewIKERetryGate(store, clock)
	card := retryTestPrepare(t, g, "slot-a", "card-a")
	target := IKERetryTarget{"one.example", "23415", "ims"}
	retryTestObserve(t, g, card, target, 10500, []byte{1, 0x63}) // 3 * 2 seconds
	clock.advance(time.Second, 48*time.Hour)
	g = NewIKERetryGate(store, clock)
	card = retryTestPrepare(t, g, "slot-a", "card-a")
	if got := retryTestBlocked(t, g.Permit(ctx, card, target, "")).Status.AfterSeconds; got != 5 {
		t.Fatalf("remaining=%d", got)
	}
	clock.advance(5*time.Second, -96*time.Hour)
	if err := g.Permit(ctx, card, target, ""); err != nil {
		t.Fatal(err)
	}
	clock.advance(0, 96*time.Hour)
	if err := g.Permit(ctx, card, target, ""); err != nil {
		t.Fatal("expired entry revived", err)
	}
}

func TestIKERetryGateHostRebootRebasesOnceRatherThanClearingOrTrustingUTC(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	clock := newRetryTestClock()
	g := NewIKERetryGate(store, clock)
	card := retryTestPrepare(t, g, "a", "same-card")
	target := IKERetryTarget{"one.example", "23415", "ims"}
	retryTestObserve(t, g, card, target, 10500, []byte{1, 0x63})
	clock.mu.Lock()
	clock.now.Boot = strings.Repeat("b", 32)
	clock.now.Uptime = time.Second
	clock.now.Wall = clock.now.Wall.Add(365 * 24 * time.Hour)
	clock.mu.Unlock()
	if got := retryTestBlocked(t, g.Permit(ctx, card, target, "")).Status.AfterSeconds; got != 6 {
		t.Fatalf("rebase=%d", got)
	}
	clock.advance(time.Second, 0)
	g = NewIKERetryGate(store, clock)
	if got := retryTestBlocked(t, g.Permit(ctx, card, target, "")).Status.AfterSeconds; got != 5 {
		t.Fatalf("restart extended twice: %d", got)
	}
	clock.advance(5*time.Second, 0)
	if err := g.Permit(ctx, card, target, ""); err != nil {
		t.Fatal(err)
	}
}

func TestIKERetryGateCauseScopeAndIndependentCards(t *testing.T) {
	for _, code := range []uint16{10500, 9002, 9003} {
		t.Run(retryScope(code), func(t *testing.T) {
			ctx := context.Background()
			g := NewIKERetryGate(nil, newRetryTestClock())
			card := retryTestPrepare(t, g, "a", "card-a")
			other := retryTestPrepare(t, g, "b", "card-b")
			target := IKERetryTarget{"one.example", "23415", "ims"}
			retryTestObserve(t, g, card, target, code, []byte{1, 1})
			retryTestBlocked(t, g.Permit(ctx, card, target, ""))
			if err := g.Permit(ctx, other, target, ""); err != nil {
				t.Fatal("other modem/card blocked", err)
			}
			for _, c := range []struct {
				name    string
				target  IKERetryTarget
				blocked bool
			}{
				{"other-host", IKERetryTarget{"two.example", "23415", "ims"}, code != 10500},
				{"other-plmn", IKERetryTarget{"two.example", "51566", "ims"}, false},
				{"other-apn", IKERetryTarget{"two.example", "23415", "other"}, code == 9003},
				{"same-host-other-apn", IKERetryTarget{"one.example", "23415", "other"}, code != 9002},
				{"unknown-plmn", IKERetryTarget{"two.example", "", "ims"}, code != 10500},
			} {
				t.Run(c.name, func(t *testing.T) {
					err := g.Permit(ctx, card, c.target, "")
					if errors.Is(err, ErrIKERetryBlocked) != c.blocked {
						t.Fatalf("blocked=%v err=%v", c.blocked, err)
					}
				})
			}
		})
	}
}

func TestIKERetryGateZeroMissingInvalidAndDeactivatedRemainDistinct(t *testing.T) {
	for _, c := range []struct {
		name    string
		data    []byte
		seconds int64
		until   bool
	}{
		{"zero", []byte{1, 0}, 0, false}, {"timed", []byte{1, 1}, 600, false}, {"missing", nil, 60, false}, {"bare", []byte{1}, 60, false}, {"deactivated", []byte{1, 0xE0}, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := NewIKERetryGate(nil, newRetryTestClock())
			card := retryTestPrepare(t, g, "a", "card-a")
			target := IKERetryTarget{"one.example", "23415", "ims"}
			retryTestObserve(t, g, card, target, 10500, c.data)
			err := g.Permit(context.Background(), card, target, "")
			if c.name == "zero" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			s := retryTestBlocked(t, err).Status
			if s.AfterSeconds != c.seconds || s.UntilCardEvent != c.until {
				t.Fatalf("status=%+v", s)
			}
		})
	}
}

func TestIKERetryGateUnknownAttemptNeverExpiresAndStaleCompletionCannotDeleteReplacement(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	clock := newRetryTestClock()
	g := NewIKERetryGate(store, clock)
	target := IKERetryTarget{"one.example", "23415", "ims"}
	card := retryTestPrepare(t, g, "a", "card-a")
	old, err := g.Begin(ctx, card, target)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(30*24*time.Hour, 30*24*time.Hour)
	g = NewIKERetryGate(store, clock)
	if got := retryTestBlocked(t, g.Permit(ctx, card, target, "")).Status; got.Source != "uncertain-attempt" || !got.UntilCardEvent {
		t.Fatalf("status=%+v", got)
	}
	_ = retryTestPrepare(t, g, "a", "card-b")
	card = retryTestPrepare(t, g, "a", "card-a")
	current, err := g.Begin(ctx, card, target)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Complete(ctx, card, old); err != nil {
		t.Fatal(err)
	}
	retryTestBlocked(t, g.Permit(ctx, card, target, ""))
	if err = g.Permit(ctx, card, target, current); err != nil {
		t.Fatal(err)
	}
}

func TestIKERetryGateVerifiedCardChangeClearsOldLifecycleButRejectsLateRejection(t *testing.T) {
	ctx := context.Background()
	g := NewIKERetryGate(nil, newRetryTestClock())
	target := IKERetryTarget{"one.example", "23415", "ims"}
	old := retryTestPrepare(t, g, "a", "card-a")
	token, err := g.Begin(ctx, old, target)
	if err != nil {
		t.Fatal(err)
	}
	_ = retryTestPrepare(t, g, "a", "card-b")
	err = g.Observe(ctx, old, target, token, EPDGNetworkRejection{NotifyType: 10500, Scope: EPDGRejectionEndpoint, Backoff: DecodeIKEBackoffTimer([]byte{1, 0xE0}), EPDG: target.Host, APN: target.APN, Endpoint: "192.0.2.9"})
	if !errors.Is(err, ErrIKERetryBinding) {
		t.Fatalf("late rejection=%v", err)
	}
	current := retryTestPrepare(t, g, "a", "card-a")
	if err = g.Permit(ctx, current, target, ""); err != nil {
		t.Fatal(err)
	}
}

func TestIKERetryGateMalformedJournalFailsClosedRatherThanDroppingRestrictions(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", "[]", `{"version":1,"cards":{},"devices":{},"entries":{},"active":{},"Entries":{}}`, `{"version":1,"cards":{},"devices":{},"entries":{},"active":{},"extra":0}`} {
		t.Run(raw, func(t *testing.T) {
			s := &memoryIKERetryStore{raw: json.RawMessage(raw)}
			g := NewIKERetryGate(s, newRetryTestClock())
			if _, err := g.Prepare(context.Background(), "a", "card-a"); !errors.Is(err, ErrIKERetryStore) {
				t.Fatalf("malformed journal accepted: %v", err)
			}
		})
	}
}

func TestIKERetryGateParallelCASRetainsEveryIndependentRestriction(t *testing.T) {
	ctx := context.Background()
	store := &memoryIKERetryStore{}
	clock := newRetryTestClock()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g := NewIKERetryGate(store, clock)
			device := strings.Repeat(string(rune('a'+i)), 2)
			card, err := g.Prepare(ctx, device, device)
			if err != nil {
				t.Error(err)
				return
			}
			target := IKERetryTarget{"one.example", "23415", "ims"}
			token, err := g.Begin(ctx, card, target)
			if err != nil {
				t.Error(err)
				return
			}
			err = g.Observe(ctx, card, target, token, EPDGNetworkRejection{NotifyType: 10500, Scope: EPDGRejectionEndpoint, Backoff: DecodeIKEBackoffTimer([]byte{1, 1}), EPDG: target.Host, APN: target.APN, Endpoint: "192.0.2.9"})
			if err != nil {
				t.Error(err)
				return
			}
			if err = g.Complete(ctx, card, token); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	raw, _ := store.LoadIKERetry(ctx)
	doc, err := decodeIKERetryJournal(raw)
	if err != nil || len(doc.Entries) != 8 || len(doc.Active) != 0 {
		t.Fatalf("lost CAS updates: entries=%d active=%d err=%v", len(doc.Entries), len(doc.Active), err)
	}
}

func TestIKERetryPolicyErrorsCannotBecomeAccessProofInsideNetworkErrors(t *testing.T) {
	for _, policy := range []error{ErrIKERetryBlocked, ErrIKERetryStore, ErrIKERetryBinding} {
		err := &EPDGAccessError{Cause: errors.Join(errors.New("network failed"), policy)}
		if EPDGFallbackAllowed(err) {
			t.Fatalf("policy fence became access permission: %v", policy)
		}
	}
	if !EPDGFallbackAllowed(&EPDGAccessError{Cause: errors.New("ordinary initial network failure")}) {
		t.Fatal("ordinary access control was blanket-disabled")
	}
}
