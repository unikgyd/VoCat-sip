package hostimsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"vocat/internal/hostims"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
	"vocat/internal/vowifi/imssec"
)

type callTrace struct {
	mu    sync.Mutex
	items []string
}

func (t *callTrace) add(item string) { t.mu.Lock(); t.items = append(t.items, item); t.mu.Unlock() }
func (t *callTrace) value() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.items, ",")
}

type fakeAccess struct {
	verifies atomic.Int32
	closes   atomic.Int32
	verify   func(context.Context, vowifi.SIMIdentity) error
	close    func(context.Context, int32) error
	trace    *callTrace
}

func (*fakeAccess) CardIdentity() hostims.CardIdentity { return hostims.CardIdentity{} }
func (*fakeAccess) Evidence() vowifi.CellularIMSEvidence {
	return vowifi.CellularIMSEvidence{Established: true}
}
func (a *fakeAccess) Verify(ctx context.Context, id vowifi.SIMIdentity) error {
	a.verifies.Add(1)
	if a.verify != nil {
		return a.verify(ctx, id)
	}
	return ctx.Err()
}
func (*fakeAccess) BindSocket(context.Context, string, string, syscall.RawConn) error { return nil }
func (*fakeAccess) AcquireMediaRoute(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
	return nil, errors.New("unused synthetic media")
}
func (*fakeAccess) BindProtectedSocket(context.Context, string, imssec.SocketBinding, syscall.RawConn) error {
	return nil
}
func (*fakeAccess) InstallIPSec(context.Context, imssec.Config) (imssec.Handle, error) {
	return nil, errors.New("unused synthetic security")
}
func (*fakeAccess) RetainIPSec(imssec.Handle) {}
func (*fakeAccess) CheckReady(context.Context, vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	return vowifi.AKAEvidence{Ready: true, Application: "ISIM"}, nil
}
func (*fakeAccess) Authenticate(context.Context, vowifi.SIMIdentity, vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	return vowifi.AKAResult{}, errors.New("unused synthetic AKA")
}
func (*fakeAccess) AuthenticateWithPreference(context.Context, vowifi.SIMIdentity, vowifi.AKAChallenge, string) (vowifi.AKAResult, error) {
	return vowifi.AKAResult{}, errors.New("unused synthetic AKA")
}
func (a *fakeAccess) Close(ctx context.Context) error {
	count := a.closes.Add(1)
	if a.trace != nil {
		a.trace.add("access")
	}
	if a.close != nil {
		return a.close(ctx, count)
	}
	return nil
}

type fakeSession struct {
	registered atomic.Bool
	closes     atomic.Int32
	close      func(context.Context, int32) error
	trace      *callTrace
}

func registeredSession() *fakeSession {
	s := &fakeSession{}
	s.registered.Store(true)
	return s
}
func (s *fakeSession) Evidence() vowifi.IMSEvidence {
	return vowifi.IMSEvidence{Registered: s.registered.Load(), AssociatedMSISDN: "+15555550100", PAssociatedURI: []string{"sip:private@ims.example.net"}}
}
func (*fakeSession) EnableSMS(context.Context) (vowifi.SMSEvidence, error) {
	return vowifi.SMSEvidence{}, nil
}
func (s *fakeSession) Close(ctx context.Context) error {
	count := s.closes.Add(1)
	if s.trace != nil {
		s.trace.add("session")
	}
	if s.close != nil {
		if err := s.close(ctx, count); err != nil {
			return err
		}
	}
	s.registered.Store(false)
	return nil
}

type providerFunc func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error)

func (f providerFunc) Start(ctx context.Context, request vowifi.IMSRequest) (vowifi.IMSSession, error) {
	return f(ctx, request)
}

func testCard() cardProvisioning {
	return cardProvisioning{identity: vowifi.SIMIdentity{ICCID: "8901000000000000001", IMSI: "310260123456780", IMEI: "123456789012345", HomeMCC: "310", HomeMNC: "260"},
		domain: "ims.example.net", private: "subscriber@private.example.net", public: "sip:+15555550100@public.example.net", app: "ISIM"}
}

func testRuntime(t *testing.T, prepare func(context.Context) (Access, error), start providerFunc) *Runtime {
	t.Helper()
	r, err := newRuntimeWith(Config{DeviceID: "synthetic-line", Prepare: prepare},
		func(vowifi.AKAProvider, ims.Config) (vowifi.IMSProvider, error) { return start, nil },
		func(Access) cardProvisioning { return testCard() })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic worker did not reach barrier")
	}
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("caller blocked on synthetic worker")
		return nil
	}
}

func waitState(t *testing.T, r *Runtime, state State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		ready := r.state == state && r.op == nil
		r.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime did not finish independent cleanup", r.Snapshot())
}

func TestRuntimeRegistersWithCardProvisioningAndStopsInOrder(t *testing.T) {
	trace := &callTrace{}
	access := &fakeAccess{trace: trace}
	session := registeredSession()
	session.trace = trace
	var requests atomic.Int32
	r, err := newRuntimeWith(Config{DeviceID: "line-a", IMS: ims.Config{SecurityMode: ims.SecurityDisabled,
		HomeDomain: "forged.example.net", PrivateIdentity: "forged@forged.example.net", PublicIdentity: "sip:forged@forged.example.net"},
		Prepare: func(context.Context) (Access, error) { return access, nil }},
		func(aka vowifi.AKAProvider, config ims.Config) (vowifi.IMSProvider, error) {
			card := testCard()
			if aka != access || config.HomeDomain != card.domain || config.PrivateIdentity != card.private || config.PublicIdentity != card.public || config.SecurityMode != ims.SecurityRequired || config.IPSecInstaller != nil {
				t.Error("SIP identities or security did not come from prepared access")
			}
			return providerFunc(func(ctx context.Context, req vowifi.IMSRequest) (vowifi.IMSSession, error) {
				requests.Add(1)
				if ctx.Err() != nil || req.DeviceID != "line-a" || req.Cellular != access || req.Tunnel != nil || req.Identity.ICCID != card.identity.ICCID {
					t.Error("wrong prepared line or transport")
				}
				return session, nil
			}), nil
		}, func(Access) cardProvisioning { return testCard() })
	if err != nil || r.Start(context.Background()) != nil {
		t.Fatal("startup failed", err)
	}
	if snapshot := r.Snapshot(); snapshot.State != Ready || !snapshot.Ready || r.Session() != session {
		t.Fatal("registered session was not published", snapshot)
	}
	if err := r.Start(context.Background()); err != nil || requests.Load() != 1 {
		t.Fatal("ready Start duplicated the registration", err)
	}
	if err := r.Stop(context.Background()); err != nil || trace.value() != "session,access" || r.Session() != nil || r.Snapshot().State != Stopped {
		t.Fatal("stop order or state incorrect", err, trace.value())
	}
	if err := r.Stop(context.Background()); err != nil || access.closes.Load() != 1 || session.closes.Load() != 1 {
		t.Fatal("completed stop repeated cleanup", err)
	}
}

func TestRuntimeDoesNotPublishUnregisteredSession(t *testing.T) {
	trace := &callTrace{}
	a := &fakeAccess{trace: trace}
	s := &fakeSession{trace: trace}
	r := testRuntime(t, func(context.Context) (Access, error) { return a, nil }, func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return s, nil })
	if err := r.Start(context.Background()); !errors.Is(err, ErrNotRegistered) || r.Session() != nil || r.Snapshot().State != Failed || trace.value() != "session,access" {
		t.Fatal("unregistered session accepted or lost", err, r.Snapshot(), trace.value())
	}
}

func TestRuntimeCancellationRetainsLateAccessWithoutRegistering(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &fakeAccess{}
	var starts atomic.Int32
	r := testRuntime(t, func(context.Context) (Access, error) { close(entered); <-release; return a, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
			starts.Add(1)
			return registeredSession(), nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	waitSignal(t, entered)
	cancel()
	if err := waitResult(t, done); !errors.Is(err, context.Canceled) || r.Snapshot().State != Stopping || r.Session() != nil {
		t.Fatal("cancellation blocked or published unfinished access", err)
	}
	if err := r.Start(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("parallel setup bypassed retained worker", err)
	}
	close(release)
	waitState(t, r, Stopped)
	if starts.Load() != 0 || a.closes.Load() != 1 {
		t.Fatal("canceled access registered or was abandoned")
	}
}

func TestRuntimeCancellationRetainsLateSession(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	trace := &callTrace{}
	a := &fakeAccess{trace: trace}
	s := registeredSession()
	s.trace = trace
	r := testRuntime(t, func(context.Context) (Access, error) { return a, nil }, func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
		close(entered)
		<-release
		return s, errors.New("private IMS failure for 310260123456780")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	waitSignal(t, entered)
	cancel()
	if err := waitResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("caller waited for late session", err)
	}
	if r.Session() != nil || r.Snapshot().State != Stopping {
		t.Fatal("late session published")
	}
	close(release)
	waitState(t, r, Stopped)
	if trace.value() != "session,access" {
		t.Fatal("late result ownership/order lost", trace.value())
	}
}

func TestRuntimeStopCancelsPendingStartAndDoesNotWaitForCaller(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &fakeAccess{}
	r := testRuntime(t, func(context.Context) (Access, error) { close(entered); <-release; return a, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
			t.Error("canceled start reached registration")
			return registeredSession(), nil
		})
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	waitSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := r.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Stop did not respect its caller deadline", err)
	}
	close(release)
	if err := waitResult(t, started); !errors.Is(err, context.Canceled) {
		t.Fatal("stopped Start reported readiness", err)
	}
	waitState(t, r, Stopped)
	if a.closes.Load() != 1 {
		t.Fatal("late access not cleaned")
	}
}

func TestRuntimeTwoLinesNeverShareOperationGateOrCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	firstAccess, secondAccess := &fakeAccess{}, &fakeAccess{}
	first := testRuntime(t, func(context.Context) (Access, error) { close(entered); <-release; return firstAccess, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return registeredSession(), nil })
	secondSession := registeredSession()
	second := testRuntime(t, func(context.Context) (Access, error) { return secondAccess, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return secondSession, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- first.Start(ctx) }()
	waitSignal(t, entered)
	if err := second.Start(context.Background()); err != nil || !second.Snapshot().Ready {
		t.Fatal("one line's acquire blocked another", err)
	}
	cancel()
	_ = waitResult(t, done)
	close(release)
	waitState(t, first, Stopped)
	if second.Session() != secondSession || !second.Snapshot().Ready || secondAccess.closes.Load() != 0 {
		t.Fatal("first cleanup affected second line")
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCleanupFailureRetainsSameOwnersForRetry(t *testing.T) {
	for _, failure := range []string{"session", "access"} {
		t.Run(failure, func(t *testing.T) {
			trace := &callTrace{}
			a := &fakeAccess{trace: trace}
			s := registeredSession()
			s.trace = trace
			failFirst := func(context.Context, int32) error { return nil }
			failFirst = func(_ context.Context, count int32) error {
				if count == 1 {
					return errors.New("raw secret 310260123456780 +15555550100")
				}
				return nil
			}
			if failure == "session" {
				s.close = failFirst
			} else {
				a.close = failFirst
			}
			r := testRuntime(t, func(context.Context) (Access, error) { return a, nil },
				func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return s, nil })
			if err := r.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := r.Stop(context.Background()); !errors.Is(err, ErrCleanup) || strings.Contains(err.Error(), "secret") {
				t.Fatal("cleanup failure lost or leaked", err)
			}
			if snapshot := r.Snapshot(); snapshot.State != Failed || snapshot.Failure != "cleanup_failed" || snapshot.Ready {
				t.Fatal("cleanup not fenced", snapshot)
			}
			if failure == "session" && a.closes.Load() != 0 {
				t.Fatal("access removed under a session with failed close")
			}
			if err := r.Start(context.Background()); !errors.Is(err, ErrBusy) {
				t.Fatal("new Start lost retained owner", err)
			}
			if err := r.Stop(context.Background()); err != nil || r.Snapshot().State != Stopped {
				t.Fatal("retry failed", err)
			}
			want := "session,access,access"
			if failure == "session" {
				want = "session,session,access"
			}
			if trace.value() != want {
				t.Fatal("wrong retained owner retry", trace.value())
			}
		})
	}
}

func TestRuntimeStopCallerCancellationDoesNotAbandonCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &fakeAccess{}
	s := registeredSession()
	s.close = func(ctx context.Context, _ int32) error {
		if ctx.Err() != nil {
			t.Error("cleanup used an already canceled caller context")
		}
		close(entered)
		<-release
		return nil
	}
	r := testRuntime(t, func(context.Context) (Access, error) { return a, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return s, nil })
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Stop(ctx) }()
	waitSignal(t, entered)
	cancel()
	if err := waitResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r.Session() != nil || a.closes.Load() != 0 {
		t.Fatal("cleanup abandoned or access closed too early")
	}
	close(release)
	waitState(t, r, Stopped)
	if a.closes.Load() != 1 {
		t.Fatal("access cleanup lost")
	}
}

func TestRuntimeAccessAndErrorRemainOwnedAfterFailedPrepare(t *testing.T) {
	a := &fakeAccess{close: func(_ context.Context, count int32) error {
		if count == 1 {
			return errors.New("private cleanup detail")
		}
		return nil
	}}
	r := testRuntime(t, func(context.Context) (Access, error) { return a, errors.New("private acquire detail") },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
			t.Error("failed prepare registered")
			return nil, nil
		})
	err := r.Start(context.Background())
	if !errors.Is(err, ErrPrepare) || !errors.Is(err, ErrCleanup) || strings.Contains(err.Error(), "private") || r.Snapshot().Failure != "cleanup_failed" {
		t.Fatal("acquired error resource not retained safely", err)
	}
	if err := r.Stop(context.Background()); err != nil || a.closes.Load() != 2 {
		t.Fatal("failed acquire owner not retried", err)
	}
}

func TestRuntimePublicFactoryRejectsFabricatedEmptyCard(t *testing.T) {
	a := &fakeAccess{}
	r, err := NewRuntime(Config{DeviceID: "synthetic", Prepare: func(context.Context) (Access, error) { return a, nil },
		IMS: ims.Config{HomeDomain: "forged.example.net", PrivateIdentity: "forged@forged.example.net", PublicIdentity: "sip:forged@forged.example.net"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); !errors.Is(err, ErrPrepare) || a.verifies.Load() != 0 || a.closes.Load() != 1 {
		t.Fatal("configured identity promoted an empty card", err)
	}
}

func TestRuntimeRegistrationLossAndStatusNeverExposePrivateEvidence(t *testing.T) {
	a := &fakeAccess{}
	s := registeredSession()
	r := testRuntime(t, func(context.Context) (Access, error) { return a, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return s, nil })
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.registered.Store(false)
	if snapshot := r.Snapshot(); snapshot.State != Failed || snapshot.Failure != "registration_lost" || snapshot.Ready || r.Session() != nil {
		t.Fatal("expired registration stayed ready", snapshot)
	}
	encoded, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{string(encoded), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r)} {
		for _, secret := range []string{testCard().identity.ICCID, testCard().identity.IMSI, "+15555550100", "private@"} {
			if strings.Contains(value, secret) {
				t.Fatal("runtime diagnostic leaked private information")
			}
		}
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeConfigValidationAndCancellationBeforeStart(t *testing.T) {
	if _, err := NewRuntime(Config{}); !errors.Is(err, ErrConfig) {
		t.Fatal("empty runtime configured", err)
	}
	var prepares atomic.Int32
	r := testRuntime(t, func(context.Context) (Access, error) { prepares.Add(1); return &fakeAccess{}, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return registeredSession(), nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Start(ctx); !errors.Is(err, context.Canceled) || prepares.Load() != 0 || r.Snapshot().State != Stopped {
		t.Fatal("canceled-before-start acquired resources", err)
	}
}

func TestRuntimeConcurrentStopsCoalesceOnOneCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a := &fakeAccess{}
	s := registeredSession()
	s.close = func(_ context.Context, count int32) error {
		if count != 1 {
			return errors.New("concurrent cleanup was not coalesced")
		}
		close(entered)
		<-release
		return nil
	}
	r := testRuntime(t, func(context.Context) (Access, error) { return a, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return s, nil })
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 32)
	for i := 0; i < cap(results); i++ {
		go func() { results <- r.Stop(context.Background()) }()
	}
	waitSignal(t, entered)
	close(release)
	for i := 0; i < cap(results); i++ {
		if err := waitResult(t, results); err != nil {
			t.Fatal(err)
		}
	}
	if s.closes.Load() != 1 || a.closes.Load() != 1 || r.Snapshot().State != Stopped {
		t.Fatal("one owner was closed more than once")
	}
}

func TestRuntimeLateSessionCleanupFailureCanBeRetried(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &fakeAccess{}
	s := registeredSession()
	s.close = func(_ context.Context, count int32) error {
		if count == 1 {
			return errors.New("late cleanup error with private identity")
		}
		return nil
	}
	r := testRuntime(t, func(context.Context) (Access, error) { return a, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
			close(entered)
			<-release
			return s, nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	waitSignal(t, entered)
	cancel()
	if err := waitResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	waitState(t, r, Failed)
	if a.closes.Load() != 0 || r.Snapshot().Failure != "cleanup_failed" {
		t.Fatal("late owner was discarded")
	}
	if err := r.Stop(context.Background()); err != nil || s.closes.Load() != 2 || a.closes.Load() != 1 {
		t.Fatal("late failed cleanup not retried", err)
	}
}

func TestRuntimeMayRestartOnlyAfterAcknowledgedStop(t *testing.T) {
	var acquisitions, registrations atomic.Int32
	r := testRuntime(t, func(context.Context) (Access, error) { acquisitions.Add(1); return &fakeAccess{}, nil },
		func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
			registrations.Add(1)
			return registeredSession(), nil
		})
	for i := 0; i < 2; i++ {
		if err := r.Start(context.Background()); err != nil || !r.Snapshot().Ready {
			t.Fatal("restart failed", err)
		}
		if err := r.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if acquisitions.Load() != 2 || registrations.Load() != 2 {
		t.Fatal("restart reused closed owners")
	}
}
