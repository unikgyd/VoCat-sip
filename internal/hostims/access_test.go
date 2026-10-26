package hostims

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

type accessFakeBackend struct {
	prepares, verifies, binds, installs, closes, retained atomic.Int32
	failPrepare, failClose                                atomic.Bool
	prepareEntered, prepareRelease                        chan struct{}
	closeObserved                                         chan struct{}
}

func (b *accessFakeBackend) Prepare(context.Context, accessSample) error {
	b.prepares.Add(1)
	if b.prepareEntered != nil {
		close(b.prepareEntered)
	}
	if b.prepareRelease != nil {
		<-b.prepareRelease
	}
	if b.failPrepare.Load() {
		return ErrRuntime
	}
	return nil
}
func (b *accessFakeBackend) Verify(context.Context, accessSample) error {
	b.verifies.Add(1)
	return nil
}
func (b *accessFakeBackend) Close(context.Context) error {
	b.closes.Add(1)
	defer func() {
		if b.closeObserved != nil {
			select {
			case b.closeObserved <- struct{}{}:
			default:
			}
		}
	}()
	if b.failClose.Load() {
		return ErrUncertain
	}
	return nil
}
func (b *accessFakeBackend) BindSocket(context.Context, string, string, syscall.RawConn) error {
	b.binds.Add(1)
	return nil
}
func (b *accessFakeBackend) BindProtectedSocket(context.Context, string, imssec.SocketBinding, syscall.RawConn) error {
	b.binds.Add(1)
	return nil
}
func (*accessFakeBackend) AcquireMediaRoute(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
	return nil, ErrRuntime
}
func (b *accessFakeBackend) InstallIPSec(context.Context, imssec.Config) (imssec.Handle, error) {
	b.installs.Add(1)
	return nil, nil
}
func (b *accessFakeBackend) RetainIPSec(imssec.Handle) { b.retained.Add(1) }

type accessFixtureState struct {
	sim          *SIMProvider
	uim          *fakeUIM
	backend      *accessFakeBackend
	clock        atomic.Int64
	observations atomic.Int32
	deps         accessDependencies
}

func accessFixtureForSIM(t *testing.T, sim *SIMProvider, uim *fakeUIM) *accessFixtureState {
	t.Helper()
	f := &accessFixtureState{sim: sim, uim: uim, backend: &accessFakeBackend{closeObserved: make(chan struct{}, 8)}}
	f.clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, f.clock.Load()) }
	runtime := boundRuntimeRow(sim.lease.configuration.Addresses[0])
	proof := &RuntimeProof{source: &RuntimeSource{lease: sim.lease}, runtime: runtime, fingerprint: hashText(sim.lease.token)}
	f.deps = accessDependencies{sim: sim, backend: f.backend, now: now, observe: func(context.Context) (accessSample, error) {
		f.observations.Add(1)
		return accessSample{runtime: proof, cell: vowifi.LTECell{MCC: "310", MNC: "260", Duplex: "FDD", TAC: 1, CellID: 2}, start: now().Add(-time.Second), end: now()}, nil
	}}
	return f
}

func accessFixture(t *testing.T) *accessFixtureState {
	t.Helper()
	sim, uim, _, _ := simFixture(t)
	return accessFixtureForSIM(t, sim, uim)
}

func (f *accessFixtureState) prepare(t *testing.T) *PreparedAccess {
	t.Helper()
	a, err := prepareAccessWith(context.Background(), f.sim.lease, f.deps)
	if err != nil || a == nil {
		t.Fatal("prepare", err)
	}
	t.Cleanup(func() {
		f.backend.failClose.Store(false)
		if err := a.Close(context.Background()); err != nil {
			t.Error("close", err)
		}
	})
	return a
}

func waitAccessSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("access fixture signal timed out")
	}
}

func TestPreparedAccessVerifyRefreshesWithoutUIMAndCopiesEvidence(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	identity := a.CardIdentity().Subscriber()
	f.uim.mu.Lock()
	released := f.uim.released
	f.uim.mu.Unlock()
	for range 3 {
		if err := a.Verify(context.Background(), identity); err != nil {
			t.Fatal(err)
		}
	}
	if ready, err := a.CheckReady(context.Background(), identity); err != nil || !ready.Ready || ready.Application != "ISIM" {
		t.Fatal("ready", ready, err)
	}
	f.uim.mu.Lock()
	after := f.uim.released
	f.uim.mu.Unlock()
	if after != released || f.observations.Load() != 5 || f.backend.verifies.Load() != 4 {
		t.Fatal("Verify re-opened UIM or skipped fresh sample")
	}
	e := a.Evidence()
	if !e.Established || len(e.PCSCF) != 1 || a.VerificationTimeout() != 20*time.Second {
		t.Fatal("missing access evidence")
	}
	e.PCSCF[0] = "192.0.2.250"
	if a.Evidence().PCSCF[0] == e.PCSCF[0] {
		t.Fatal("caller mutated owned evidence")
	}
	card := a.CardIdentity()
	card.aid[0] ^= 1
	if sameCardIdentity(card, a.CardIdentity()) {
		t.Fatal("caller mutated pinned card")
	}
}

func TestPreparedAccessIdentityFieldsCannotBeRewritten(t *testing.T) {
	for _, field := range []string{"iccid", "imsi", "imei", "mcc", "mnc"} {
		t.Run(field, func(t *testing.T) {
			f := accessFixture(t)
			a := f.prepare(t)
			identity := a.CardIdentity().Subscriber()
			switch field {
			case "iccid":
				identity.ICCID += "1"
			case "imsi":
				identity.IMSI += "1"
			case "imei":
				identity.IMEI = "999999999999999"
			case "mcc":
				identity.HomeMCC = "999"
			case "mnc":
				identity.HomeMNC = "99"
			}
			if err := a.Verify(context.Background(), identity); !errors.Is(err, ErrSIM) || a.Evidence().Established || f.backend.verifies.Load() != 0 {
				t.Fatal("rewritten identity accepted", err)
			}
		})
	}
}

func TestPreparedAccessFixedCardDoesNotFollowSIMProviderCache(t *testing.T) {
	f := accessFixture(t)
	a, err := prepareAccessWith(context.Background(), f.sim.lease, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	identity := a.CardIdentity().Subscriber()
	f.uim.mu.Lock()
	f.uim.impi = "changed@private.example.net"
	f.uim.mu.Unlock()
	cardB, err := f.sim.ReadCard(context.Background())
	if err != nil || cardB.PrivateIdentity() == a.CardIdentity().PrivateIdentity() {
		t.Fatal("cache fixture did not change", err)
	}
	result, err := a.Authenticate(context.Background(), identity, vowifi.AKAChallenge{})
	assertNoAKA(t, result)
	f.uim.mu.Lock()
	auths := f.uim.auths
	f.uim.mu.Unlock()
	if err == nil || auths != 0 || a.Evidence().Established {
		t.Fatal("AKA followed mutable identity cache", err)
	}
	if err := a.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("changed provisioning lost its UIM cleanup fence", err)
	}
	f.sim.lease.claim.mu.Lock()
	retained := f.sim.lease.claim.preparedAccess == a && f.sim.lease.claim.record.SIMUncertain
	f.sim.lease.claim.mu.Unlock()
	if !retained {
		t.Fatal("changed provisioning forgot owner")
	}
}

func TestPreparedAccessAKAUsesPinnedApplication(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	result, err := a.AuthenticateWithPreference(context.Background(), a.CardIdentity().Subscriber(), vowifi.AKAChallenge{}, "isim_strict")
	if err != nil || len(result.CK) != 16 || len(result.IK) != 16 || len(result.RES) != 4 {
		t.Fatal("pinned ISIM authentication failed", err)
	}
	clearCardAKA(&result)
	f.uim.mu.Lock()
	auths := f.uim.auths
	application := f.uim.appKinds[len(f.uim.appKinds)-1]
	f.uim.mu.Unlock()
	if auths != 1 || application != "ISIM" || !a.Evidence().Established {
		t.Fatal("wrong application/replayed authentication")
	}
}

func TestPreparedAccessAgeStartsAtQueryRequestAndCanRefresh(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	f.clock.Add(int64(10 * time.Second)) // original query start is now 11s old
	if a.Evidence().Established {
		t.Fatal("response end was used instead of query start")
	}
	if err := a.Verify(context.Background(), a.CardIdentity().Subscriber()); err != nil || !a.Evidence().Established {
		t.Fatal("fresh query could not renew expired snapshot", err)
	}
	f.clock.Add(int64(10 * time.Second))
	if err := a.BindSocket(context.Background(), "udp4", "198.51.100.20:5060", nil); err == nil || f.backend.binds.Load() != 0 {
		t.Fatal("expired snapshot reached socket backend")
	}
}

func TestPreparedAccessInvalidQueryWindowsNeverPrepareBackend(t *testing.T) {
	for _, mode := range []string{"old-start", "long-query", "future", "wrong-source", "invalid-cell"} {
		t.Run(mode, func(t *testing.T) {
			f := accessFixture(t)
			observe := f.deps.observe
			f.deps.observe = func(ctx context.Context) (accessSample, error) {
				s, err := observe(ctx)
				switch mode {
				case "old-start":
					s.start = f.deps.now().Add(-11 * time.Second)
					s.end = s.start.Add(time.Second)
				case "long-query":
					s.start = s.end.Add(-6 * time.Second)
				case "future":
					s.end = s.end.Add(time.Second)
				case "wrong-source":
					cp := *s.runtime
					cp.source = &RuntimeSource{}
					s.runtime = &cp
				case "invalid-cell":
					s.cell.Duplex = ""
				}
				return s, err
			}
			a, err := prepareAccessWith(context.Background(), f.sim.lease, f.deps)
			if a == nil || err == nil || a.Evidence().Established || f.backend.prepares.Load() != 0 {
				t.Fatal("invalid query accepted", err)
			}
			if err := a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPreparedAccessCannotBypassOwnerAndCleanupCanRetry(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	lease := f.sim.lease
	if err := lease.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("public PDN close bypassed owner", err)
	}
	if err := lease.pool.Recover(context.Background(), lease.selection); !errors.Is(err, ErrBusy) {
		t.Fatal("Recover bypassed owner", err)
	}
	if next, err := prepareAccessWith(context.Background(), lease, f.deps); next != nil || !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate owner accepted", err)
	}
	f.backend.failClose.Store(true)
	if err := a.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("cleanup error lost", err)
	}
	lease.claim.mu.Lock()
	retained := lease.claim.preparedAccess == a && lease.claim.record.Stage == "active"
	lease.claim.mu.Unlock()
	if !retained || a.Evidence().Established {
		t.Fatal("failed cleanup forgot owner or released PDN")
	}
	f.backend.failClose.Store(false)
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease.claim.mu.Lock()
	cleared := lease.claim.preparedAccess == nil && lease.claim.record.Token == ""
	lease.claim.mu.Unlock()
	if !cleared {
		t.Fatal("successful cleanup retained owner slot")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal("Close not idempotent", err)
	}
}

func TestPreparedAccessCancelReturnsOwnerAndObservesLatePrepare(t *testing.T) {
	f := accessFixture(t)
	f.backend.prepareEntered, f.backend.prepareRelease = make(chan struct{}), make(chan struct{})
	f.backend.failClose.Store(true)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.backend.prepareRelease) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		access *PreparedAccess
		err    error
	}
	done := make(chan outcome, 1)
	go func() { a, err := prepareAccessWith(ctx, f.sim.lease, f.deps); done <- outcome{a, err} }()
	waitAccessSignal(t, f.backend.prepareEntered)
	cancel()
	var out outcome
	select {
	case out = <-done:
	case <-time.After(time.Second):
		t.Fatal("caller waited for late Prepare")
	}
	if out.access == nil || !errors.Is(out.err, context.Canceled) || out.access.Evidence().Established {
		t.Fatal("cancellation lost/falsely readied owner", out.err)
	}
	if err := f.sim.lease.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("late owner not rooted", err)
	}
	if f.backend.closes.Load() != 0 {
		t.Fatal("cleanup raced an unfinished Prepare")
	}
	release()
	waitAccessSignal(t, f.backend.closeObserved)
	f.sim.lease.claim.mu.Lock()
	retained := f.sim.lease.claim.preparedAccess == out.access
	f.sim.lease.claim.mu.Unlock()
	if !retained {
		t.Fatal("late cleanup failure lost owner")
	}
	f.backend.failClose.Store(false)
	if err := out.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedAccessFailedPrepareReturnsRetainedOwner(t *testing.T) {
	f := accessFixture(t)
	f.backend.failPrepare.Store(true)
	f.backend.failClose.Store(true)
	a, err := prepareAccessWith(context.Background(), f.sim.lease, f.deps)
	if a == nil || !errors.Is(err, ErrRuntime) {
		t.Fatal("failure discarded owner", err)
	}
	waitAccessSignal(t, f.backend.closeObserved)
	if err := f.sim.lease.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("cleanup fence bypassed", err)
	}
	f.backend.failClose.Store(false)
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedAccessCancelCardReadStopsBeforeObservation(t *testing.T) {
	f := accessFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	open := f.sim.open
	f.sim.open = func(ctx context.Context, selection Selection) (uimSession, error) {
		close(entered)
		<-release
		return open(ctx, selection)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		access *PreparedAccess
		err    error
	}
	done := make(chan outcome, 1)
	go func() { a, err := prepareAccessWith(ctx, f.sim.lease, f.deps); done <- outcome{a, err} }()
	waitAccessSignal(t, entered)
	cancel()
	var out outcome
	select {
	case out = <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled card-read caller blocked")
	}
	if out.access == nil || !errors.Is(out.err, context.Canceled) {
		t.Fatal("canceled owner lost", out.err)
	}
	unblock()
	if err := out.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.observations.Load() != 0 || f.backend.prepares.Load() != 0 {
		t.Fatal("canceled card read began another setup operation")
	}
}

func TestPreparedAccessCanceledAKAReturnsNoKeysAndDoesNotReplay(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.uim.mu.Lock()
	f.uim.authEntered, f.uim.authRelease = entered, release
	f.uim.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result vowifi.AKAResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := a.Authenticate(ctx, a.CardIdentity().Subscriber(), vowifi.AKAChallenge{})
		done <- outcome{result, err}
	}()
	waitAccessSignal(t, entered)
	cancel()
	select {
	case out := <-done:
		assertNoAKA(t, out.result)
		if !errors.Is(out.err, context.Canceled) {
			t.Fatal(out.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled AKA caller blocked")
	}
	unblock()
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.uim.mu.Lock()
	auths := f.uim.auths
	f.uim.mu.Unlock()
	if auths != 1 {
		t.Fatal("canceled AUTH replayed")
	}
}

func TestPreparedAccessCancelFreshVerifyCannotPublishOrReachBackend(t *testing.T) {
	f := accessFixture(t)
	original := f.deps.observe
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f.deps.observe = func(ctx context.Context) (accessSample, error) {
		if f.observations.Load() == 1 {
			close(entered)
			<-release
		}
		return original(ctx)
	}
	a := f.prepare(t)
	t.Cleanup(unblock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Verify(ctx, a.CardIdentity().Subscriber()) }()
	waitAccessSignal(t, entered)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Verify caller blocked")
	}
	if a.Evidence().Established {
		t.Fatal("canceled query still ready")
	}
	unblock()
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.backend.verifies.Load() != 0 {
		t.Fatal("canceled query reached next backend step")
	}
}

func TestPreparedAccessRetainIPSecSynchronouslyFencesEveryDelegate(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	a.RetainIPSec(nil)
	if a.Evidence().Established || f.backend.retained.Load() != 1 {
		t.Fatal("retained cleanup was not fenced/transferred")
	}
	if err := a.BindProtectedSocket(context.Background(), "udp4", imssec.SocketBinding{}, nil); err == nil {
		t.Fatal("protected socket escaped stop fence")
	}
	if _, err := a.InstallIPSec(context.Background(), imssec.Config{}); err == nil || f.backend.installs.Load() != 0 {
		t.Fatal("install escaped stop fence")
	}
	if f.backend.binds.Load() != 0 {
		t.Fatal("stopped socket reached backend")
	}
}

func TestPreparedAccessTwoModemsHaveIndependentOwners(t *testing.T) {
	f := accessFixture(t)
	first := f.prepare(t)
	pool := f.sim.lease.pool
	selection, err := pool.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	uim := newFakeUIM(selection)
	sim, err := newSIMProvider(lease, uim.open)
	if err != nil {
		t.Fatal(err)
	}
	secondFixture := accessFixtureForSIM(t, sim, uim)
	second := secondFixture.prepare(t)
	f.backend.failClose.Store(true)
	if err := first.Close(context.Background()); err == nil {
		t.Fatal("expected first cleanup failure")
	}
	if err := second.Verify(context.Background(), second.CardIdentity().Subscriber()); err != nil || !second.Evidence().Established {
		t.Fatal("first line blocked second", err)
	}
	f.backend.failClose.Store(false)
}
