package hostims

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeFake struct {
	mu                     sync.Mutex
	rows                   map[byte]Runtime
	opens, queries, closes int
	unknown, closeFail     bool
	queryErr               error
	entered, release       chan struct{}
	binding                string
	check                  func(context.Context) error
}
type runtimeFakeSession struct {
	state  *runtimeFake
	verify func(context.Context) error
	closed bool
}

func (f *runtimeFake) open(_ context.Context, verify func(context.Context) error) (runtimeSession, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return &runtimeFakeSession{state: f, verify: verify}, nil
}
func (s *runtimeFakeSession) Query(ctx context.Context, family byte) (Runtime, error) {
	f := s.state
	if err := s.verify(ctx); err != nil {
		return Runtime{}, err
	}
	f.mu.Lock()
	f.queries++
	entered, release, err, row, check := f.entered, f.release, f.queryErr, cloneRuntime(f.rows[family]), f.check
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if check != nil {
		if err := check(ctx); err != nil {
			return Runtime{}, err
		}
	}
	return row, err
}
func (s *runtimeFakeSession) BindingKey() string {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.binding
}
func (s *runtimeFakeSession) Uncertain() bool {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.unknown
}
func (s *runtimeFakeSession) Close(context.Context) error {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.closeFail {
		return errors.New("private APN/identity diagnostic")
	}
	if !s.closed {
		s.state.closes++
		s.closed = true
	}
	return nil
}

func runtimeSourceFixture(t *testing.T) (*RuntimeSource, *runtimeFake, *memoryClaims) {
	t.Helper()
	pool, _, journal, selection := bearerFixture(t)
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := &runtimeFake{rows: map[byte]Runtime{}, binding: hashText("private topology")}
	for _, address := range lease.configuration.Addresses {
		row := boundRuntimeRow(address)
		f.rows[row.Family] = row
	}
	source, err := newRuntimeSource(lease, f.open)
	if err != nil {
		t.Fatal(err)
	}
	return source, f, journal
}
func TestRuntimeSourceProofIsDetachedAndRevalidated(t *testing.T) {
	source, state, journal := runtimeSourceFixture(t)
	proof, err := source.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(proof.Endpoints()) != 2 {
		t.Fatal("dual stack proof lost")
	}
	copy := proof.Runtime()
	copy.PCSCF[0] = netip.MustParseAddr("203.0.113.99")
	if proof.Runtime().PCSCF[0] == copy.PCSCF[0] {
		t.Fatal("mutable proof alias")
	}
	if err := proof.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	row := state.rows[4]
	row.PCSCF = []netip.Addr{netip.MustParseAddr("198.51.100.99")}
	state.rows[4] = row
	state.mu.Unlock()
	if err := proof.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatal("changed P-CSCF accepted", err)
	}
	if journal.record(source.lease.selection.key).RuntimePending || journal.record(source.lease.selection.key).RuntimeUncertain {
		t.Fatal("clean read left a fence")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.opens != 3 || state.closes != 3 || state.queries != 6 {
		t.Fatal("resource count mismatch")
	}
}
func TestRuntimeSourceCleanupFailureWithholdsProofAndKeepsParent(t *testing.T) {
	for _, mode := range []string{"unknown", "close"} {
		t.Run(mode, func(t *testing.T) {
			source, state, journal := runtimeSourceFixture(t)
			state.unknown = mode == "unknown"
			state.closeFail = mode == "close"
			proof, err := source.Read(context.Background())
			if proof != nil || !errors.Is(err, ErrUncertain) {
				t.Fatal("unclean query exposed proof", err)
			}
			if !journal.record(source.lease.selection.key).RuntimeUncertain {
				t.Fatal("missing durable runtime fence")
			}
			if len(source.lease.claim.retained) == 0 {
				t.Fatal("unknown session has no application root")
			}
			if err := source.lease.Close(context.Background()); !errors.Is(err, ErrUncertain) {
				t.Fatal("parent closed")
			}
			if err := source.lease.pool.Recover(context.Background(), source.lease.selection); !errors.Is(err, ErrUncertain) {
				t.Fatal("runtime uncertainty silently recovered", err)
			}
		})
	}
}
func TestRuntimeSourceCancellationObservesAndStopsBeforeSecondFamily(t *testing.T) {
	source, state, journal := runtimeSourceFixture(t)
	state.entered = make(chan struct{}, 1)
	state.release = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := source.Read(ctx); done <- err }()
	select {
	case <-state.entered:
	case <-time.After(time.Second):
		t.Fatal("query not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller blocked")
	}
	if !journal.record(source.lease.selection.key).RuntimePending {
		t.Fatal("pending fence disappeared")
	}
	if err := source.lease.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("PDN closed during query")
	}
	close(state.release)
	awaitClaim(t, journal, source.lease.selection.key, func(r claimRecord) bool { return !r.RuntimePending })
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.queries != 1 || state.closes != 1 {
		t.Fatal("cancel continued query or missed cleanup")
	}
}
func TestRuntimeSourceBlocksConcurrentSIMAndNetwork(t *testing.T) {
	source, state, _ := runtimeSourceFixture(t)
	state.check = func(context.Context) error {
		c := source.lease.claim
		c.mu.Lock()
		valid := (&SIMProvider{lease: source.lease}).currentLocked(c)
		c.mu.Unlock()
		if valid {
			t.Error("SIM was allowed during WDS query")
		}
		if err := (&SIMProvider{lease: source.lease}).begin(context.Background()); !errors.Is(err, ErrBusy) {
			t.Error("runtime busy misreported as SIM failure", err)
		}
		if _, err := source.Read(context.Background()); !errors.Is(err, ErrBusy) {
			t.Error("parallel query was not rejected", err)
		}
		return nil
	}
	if _, err := source.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeSourceRejectsWrongAPNBeforeNextCID(t *testing.T) {
	source, state, journal := runtimeSourceFixture(t)
	row := state.rows[4]
	row.APN = "internet"
	state.rows[4] = row
	if proof, err := source.Read(context.Background()); proof != nil || !errors.Is(err, ErrRuntime) {
		t.Fatal("wrong APN accepted", err)
	}
	if state.queries != 1 || state.closes != 1 || journal.record(source.lease.selection.key).RuntimeUncertain {
		t.Fatal("known rejection did not clean")
	}
}
func TestRuntimeSourceJournalFailureBeforeOpen(t *testing.T) {
	source, state, journal := runtimeSourceFixture(t)
	journal.mu.Lock()
	journal.fail = true
	journal.mu.Unlock()
	if proof, err := source.Read(context.Background()); proof != nil || !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	if state.opens != 0 {
		t.Fatal("endpoint opened without durable intent")
	}
}
func TestRuntimeSourceBothModemsProgressIndependently(t *testing.T) {
	first, blocked, journal := runtimeSourceFixture(t)
	blocked.entered = make(chan struct{}, 1)
	blocked.release = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := first.Read(ctx); done <- err }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("blocked first modem not reached")
	}
	selection, err := first.lease.pool.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := first.lease.pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := &runtimeFake{binding: hashText("second modem"), rows: map[byte]Runtime{}}
	for _, address := range lease.configuration.Addresses {
		row := boundRuntimeRow(address)
		state.rows[row.Family] = row
	}
	second, err := newRuntimeSource(lease, state.open)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Read(context.Background()); err != nil {
		t.Fatal("second modem blocked", err)
	}
	cancel()
	close(blocked.release)
	<-done
	awaitClaim(t, journal, first.lease.selection.key, func(r claimRecord) bool { return !r.RuntimePending })
}
func TestRuntimeProofFormattingNeverPrintsPrivateGraph(t *testing.T) {
	source, _, _ := runtimeSourceFixture(t)
	proof, err := source.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("%+v %#v %+v", proof, *proof, source)
	for _, secret := range []string{source.lease.selection.SubscriberIMSI(), source.lease.selection.snapshot.Binding.ICCID, "198.51.100.20"} {
		if strings.Contains(text, secret) {
			t.Fatal("private graph escaped formatter")
		}
	}
}

func TestRuntimeSourceRevocationAfterReadStillAllowsCleanup(t *testing.T) {
	source, state, journal := runtimeSourceFixture(t)
	var denied atomic.Bool
	source.lease.pool.authorize = func(context.Context, Selection) error {
		if denied.Load() {
			return ErrDenied
		}
		return nil
	}
	state.check = func(context.Context) error {
		if state.queries == 2 {
			denied.Store(true)
		}
		return nil
	}
	proof, err := source.Read(context.Background())
	if proof != nil || !errors.Is(err, ErrDenied) {
		t.Fatal("revoked source produced a proof", err)
	}
	if state.closes != 1 || journal.record(source.lease.selection.key).RuntimeUncertain {
		t.Fatal("revocation stopped safe cleanup")
	}
}

func TestRuntimeSourceUnknownOpenWithoutHandleStillFencesParent(t *testing.T) {
	source, _, journal := runtimeSourceFixture(t)
	source.open = func(context.Context, func(context.Context) error) (runtimeSession, error) { return nil, ErrUncertain }
	if proof, err := source.Read(context.Background()); proof != nil || !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	if !journal.record(source.lease.selection.key).RuntimeUncertain {
		t.Fatal("missing handle erased unknown outcome")
	}
	if _, err := source.Read(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("unknown source was retried", err)
	}
}
