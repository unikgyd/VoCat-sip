package hostims

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

var ErrHostAccessUnavailable = staticError("host IMS: prepared cellular access is unavailable")

const accessVerificationTimeout = 20 * time.Second

type accessSample struct {
	runtime    *RuntimeProof
	cell       vowifi.LTECell
	start, end time.Time
}

type preparedAccessBackend interface {
	vowifi.CellularIMSNetwork
	vowifi.CellularIMSSecurity
	Prepare(context.Context, accessSample) error
	Verify(context.Context, accessSample) error
	Close(context.Context) error
}

type accessDependencies struct {
	sim     *SIMProvider
	observe func(context.Context) (accessSample, error)
	backend preparedAccessBackend
	now     func() time.Time
}

// PreparedAccess owns one fixed card and one prepared network/security scope.
// QueryWindow freshness is a host request interval, not a modem timestamp or
// continuous cell coverage. No constructor enables RF or registers SIP.
type PreparedAccess struct {
	lease          *BearerLease
	apn            string
	deps           accessDependencies
	gate           chan struct{}
	stopped        atomic.Bool
	mu             sync.Mutex
	card           CardIdentity
	sample         accessSample
	ready, closed  bool
	retainedRoutes []vowifi.CellularMediaRoute
}

var _ vowifi.CellularIMSBearer = (*PreparedAccess)(nil)
var _ vowifi.CellularIMSSecurity = (*PreparedAccess)(nil)
var _ vowifi.PreferredAKAProvider = (*PreparedAccess)(nil)

func (*PreparedAccess) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private prepared cellular access>")
}
func (*PreparedAccess) VerificationTimeout() time.Duration { return accessVerificationTimeout }

func cloneAccessCard(card CardIdentity) CardIdentity {
	card.subscriber = card.Subscriber()
	card.aid = append([]byte(nil), card.aid...)
	return card
}

func prepareAccessWith(ctx context.Context, lease *BearerLease, deps accessDependencies) (*PreparedAccess, error) {
	if lease == nil || lease.pool == nil || lease.claim == nil {
		return nil, ErrSelection
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	a := &PreparedAccess{lease: lease, deps: deps, gate: make(chan struct{}, 1)}
	a.gate <- struct{}{}
	lease.claim.mu.Lock()
	if lease.claim.preparedAccess != nil || lease.claim.pendingCleanup != nil || lease.claim.busy || lease.claim.record.Token != lease.token || lease.claim.record.Stage != "active" {
		lease.claim.mu.Unlock()
		return nil, ErrBusy
	}
	lease.claim.preparedAccess = a
	a.apn = lease.claim.record.Options.APN
	lease.claim.mu.Unlock()
	_, err := runPreparedAccess(a, ctx, func(worker context.Context) (struct{}, error) {
		if deps.sim == nil || deps.sim.lease != lease || deps.observe == nil || deps.backend == nil {
			return struct{}{}, ErrSelection
		}
		card, err := deps.sim.ReadCard(worker)
		if err != nil {
			return struct{}{}, err
		}
		if err := a.continueOperation(worker); err != nil {
			return struct{}{}, err
		}
		a.mu.Lock()
		a.card = cloneAccessCard(card)
		a.mu.Unlock()
		sample, err := deps.observe(worker)
		if err != nil {
			return struct{}{}, err
		}
		if err := a.validateSample(sample); err != nil {
			return struct{}{}, err
		}
		if err := a.continueOperation(worker); err != nil {
			return struct{}{}, err
		}
		if err := deps.backend.Prepare(worker, sample); err != nil {
			return struct{}{}, err
		}
		if err := a.continueOperation(worker); err != nil {
			return struct{}{}, err
		}
		if err := a.publish(sample); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}, nil)
	if err != nil {
		a.stopped.Store(true)
		// A late Prepare remains rooted until this serialized, bounded cleanup
		// observes it. Failure retains the owner for an explicit Close retry.
		go func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*accessVerificationTimeout)
			defer cancel()
			_ = a.Close(cleanup)
		}()
	}
	return a, err
}

type accessResult[T any] struct {
	value T
	err   error
}

type accessCallerContextKey struct{}

func (a *PreparedAccess) continueOperation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		a.stopped.Store(true)
		return err
	}
	if caller, ok := ctx.Value(accessCallerContextKey{}).(context.Context); ok {
		if err := caller.Err(); err != nil {
			a.stopped.Store(true)
			return err
		}
	}
	if a.stopped.Load() {
		return ErrHostAccessUnavailable
	}
	return nil
}

// The caller may leave promptly; only the worker owns its result until delivery.
// In particular canceled AKA never erases CK/IK while a live APDU still uses it.
func runPreparedAccess[T any](a *PreparedAccess, ctx context.Context, action func(context.Context) (T, error), discard func(*T)) (T, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}
	caller, cancel := context.WithTimeout(ctx, accessVerificationTimeout)
	defer cancel()
	select {
	case <-caller.Done():
		a.stopped.Store(true)
		return zero, caller.Err()
	case <-a.gate:
	}
	if err := caller.Err(); err != nil {
		a.stopped.Store(true)
		a.gate <- struct{}{}
		return zero, err
	}
	done := make(chan accessResult[T])
	accepted := make(chan bool, 1)
	go func() {
		defer func() { a.gate <- struct{}{} }()
		worker, stop := context.WithTimeout(context.Background(), accessVerificationTimeout)
		worker = context.WithValue(worker, accessCallerContextKey{}, caller)
		value, err := action(worker)
		stop()
		if err != nil && discard != nil {
			discard(&value)
		}
		out := accessResult[T]{value: value, err: err}
		select {
		case done <- out:
			// Keep cleanup in the owning worker/gate even if cancellation wins
			// immediately after delivery; a route Close must not delay its caller.
			if !<-accepted && discard != nil {
				discard(&value)
			}
		case <-caller.Done():
			if discard != nil {
				discard(&value)
			}
		}
	}()
	select {
	case out := <-done:
		if err := caller.Err(); err != nil {
			a.stopped.Store(true)
			accepted <- false
			return zero, err
		}
		accepted <- true
		return out.value, out.err
	case <-caller.Done():
		a.stopped.Store(true)
		return zero, caller.Err()
	}
}

func (a *PreparedAccess) validateSample(sample accessSample) error {
	if sample.runtime == nil || sample.runtime.source == nil || sample.runtime.source.lease != a.lease || !hashString(sample.runtime.fingerprint) {
		return ErrRuntime
	}
	if _, err := sample.cell.PAccessNetworkInfo(); err != nil {
		return ErrServingCellUnavailable
	}
	now := a.deps.now()
	if sample.start.IsZero() || sample.end.IsZero() || sample.end.Before(sample.start) || sample.end.Sub(sample.start) > 5*time.Second || sample.end.After(now) || sample.start.After(now) || now.Sub(sample.start) > 10*time.Second {
		return ErrServingCellUnavailable
	}
	if _, err := sample.runtime.runtime.Endpoints(a.apn); err != nil {
		return err
	}
	return nil
}

func (a *PreparedAccess) publish(sample accessSample) error {
	if err := a.validateSample(sample); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped.Load() || a.closed {
		return ErrHostAccessUnavailable
	}
	if a.sample.runtime != nil && a.sample.runtime.fingerprint != sample.runtime.fingerprint {
		return ErrChanged
	}
	a.sample, a.ready = sample, true
	return nil
}

func (a *PreparedAccess) readyError() error {
	if a == nil || a.stopped.Load() {
		return ErrHostAccessUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready || a.closed {
		return ErrHostAccessUnavailable
	}
	return a.validateSample(a.sample)
}

func (a *PreparedAccess) CardIdentity() CardIdentity {
	if a == nil {
		return CardIdentity{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneAccessCard(a.card)
}

func (a *PreparedAccess) Evidence() vowifi.CellularIMSEvidence {
	if a == nil || a.readyError() != nil {
		return vowifi.CellularIMSEvidence{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped.Load() || !a.ready || a.closed || a.validateSample(a.sample) != nil {
		return vowifi.CellularIMSEvidence{}
	}
	r := a.sample.runtime.runtime
	e := vowifi.CellularIMSEvidence{Established: true, Cell: a.sample.cell}
	if r.IPv4.IsValid() {
		e.LocalIPv4 = r.IPv4.Addr().String()
	}
	if r.IPv6.IsValid() {
		e.LocalIPv6 = r.IPv6.Addr().String()
	}
	for _, address := range r.PCSCF {
		e.PCSCF = append(e.PCSCF, address.String())
	}
	return e
}

func (a *PreparedAccess) matchesIdentity(identity vowifi.SIMIdentity) bool {
	card := a.CardIdentity()
	fixed := card.subscriber
	return card.application != "" && identity.ICCID == fixed.ICCID && identity.IMSI == fixed.IMSI &&
		identity.IMEI == fixed.IMEI && identity.HomeMCC == fixed.HomeMCC && identity.HomeMNC == fixed.HomeMNC
}

func (a *PreparedAccess) Verify(ctx context.Context, identity vowifi.SIMIdentity) error {
	if a == nil {
		return ErrHostAccessUnavailable
	}
	_, err := runPreparedAccess(a, ctx, func(worker context.Context) (struct{}, error) {
		if a.stopped.Load() || !a.matchesIdentity(identity) {
			return struct{}{}, ErrSIM
		}
		sample, err := a.deps.observe(worker)
		if err != nil {
			return struct{}{}, err
		}
		if err = a.validateSample(sample); err != nil {
			return struct{}{}, err
		}
		if err := a.continueOperation(worker); err != nil {
			return struct{}{}, err
		}
		if err = a.deps.backend.Verify(worker, sample); err != nil {
			return struct{}{}, err
		}
		if err := a.continueOperation(worker); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, a.publish(sample)
	}, nil)
	if err != nil {
		a.stopped.Store(true)
	}
	return err
}

func (a *PreparedAccess) CheckReady(ctx context.Context, identity vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	if err := a.Verify(ctx, identity); err != nil {
		return vowifi.AKAEvidence{}, err
	}
	return vowifi.AKAEvidence{Ready: true, Application: a.CardIdentity().Application()}, nil
}

func (a *PreparedAccess) Authenticate(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	return a.AuthenticateWithPreference(ctx, identity, challenge, "")
}

func (a *PreparedAccess) AuthenticateWithPreference(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge, preference string) (vowifi.AKAResult, error) {
	if a == nil {
		return vowifi.AKAResult{}, ErrHostAccessUnavailable
	}
	result, err := runPreparedAccess(a, ctx, func(worker context.Context) (vowifi.AKAResult, error) {
		card := a.CardIdentity()
		if !a.matchesIdentity(identity) || preference != "" && preference != "isim_strict" || preference == "isim_strict" && card.application != "ISIM" {
			return vowifi.AKAResult{}, ErrSIMApplication
		}
		if err := a.readyError(); err != nil {
			return vowifi.AKAResult{}, err
		}
		out, err := a.deps.sim.run(worker, &card, &challenge)
		if err != nil {
			return vowifi.AKAResult{}, err
		}
		if err := a.continueOperation(worker); err != nil {
			clearCardAKA(&out.aka)
			return vowifi.AKAResult{}, err
		}
		if err := a.readyError(); err != nil {
			clearCardAKA(&out.aka)
			return vowifi.AKAResult{}, err
		}
		return out.aka, nil
	}, clearCardAKA)
	if err != nil {
		a.stopped.Store(true)
	}
	return result, err
}

func (a *PreparedAccess) BindSocket(ctx context.Context, kind, address string, raw syscall.RawConn) error {
	return a.bind(ctx, func(worker context.Context) error { return a.deps.backend.BindSocket(worker, kind, address, raw) })
}
func (a *PreparedAccess) BindProtectedSocket(ctx context.Context, kind string, binding imssec.SocketBinding, raw syscall.RawConn) error {
	return a.bind(ctx, func(worker context.Context) error {
		return a.deps.backend.BindProtectedSocket(worker, kind, binding, raw)
	})
}
func (a *PreparedAccess) bind(ctx context.Context, action func(context.Context) error) error {
	if a == nil {
		return ErrHostAccessUnavailable
	}
	_, err := runPreparedAccess(a, ctx, func(worker context.Context) (struct{}, error) {
		if err := a.readyError(); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, action(worker)
	}, nil)
	if err != nil {
		a.stopped.Store(true)
	}
	return err
}

func (a *PreparedAccess) AcquireMediaRoute(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
	if a == nil {
		return nil, ErrHostAccessUnavailable
	}
	ref, err := runPreparedAccess(a, ctx, func(worker context.Context) (vowifi.CellularMediaRoute, error) {
		if err := a.readyError(); err != nil {
			return nil, err
		}
		return a.deps.backend.AcquireMediaRoute(worker, local, remote)
	}, func(ref *vowifi.CellularMediaRoute) {
		if *ref == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if (*ref).Close(cleanup) != nil {
			a.mu.Lock()
			a.retainedRoutes = append(a.retainedRoutes, *ref)
			a.mu.Unlock()
		}
		*ref = nil
	})
	if err != nil {
		a.stopped.Store(true)
	}
	return ref, err
}

func (a *PreparedAccess) InstallIPSec(ctx context.Context, config imssec.Config) (imssec.Handle, error) {
	if err := a.readyError(); err != nil {
		return nil, err
	}
	// The security owner serializes its own worker and clones live key buffers.
	// Do not wrap this in another worker that can outlive caller-owned CK/IK.
	handle, err := a.deps.backend.InstallIPSec(ctx, config)
	if err != nil {
		a.stopped.Store(true)
	}
	return handle, err
}
func (a *PreparedAccess) RetainIPSec(handle imssec.Handle) {
	if a == nil {
		return
	}
	a.stopped.Store(true)
	if a.deps.backend != nil {
		a.deps.backend.RetainIPSec(handle)
	}
}

func (a *PreparedAccess) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.stopped.Store(true)
	_, err := runPreparedAccess(a, ctx, func(worker context.Context) (struct{}, error) {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return struct{}{}, nil
		}
		routes := a.retainedRoutes
		a.retainedRoutes = nil
		a.mu.Unlock()
		var failed []vowifi.CellularMediaRoute
		for _, route := range routes {
			if route.Close(worker) != nil {
				failed = append(failed, route)
			}
		}
		if len(failed) != 0 {
			a.mu.Lock()
			a.retainedRoutes = append(a.retainedRoutes, failed...)
			a.mu.Unlock()
			return struct{}{}, ErrUncertain
		}
		if a.deps.backend != nil {
			if err := a.deps.backend.Close(worker); err != nil {
				return struct{}{}, err
			}
		}
		if err := a.lease.close(worker, a); err != nil {
			return struct{}{}, err
		}
		a.mu.Lock()
		a.closed, a.ready = true, false
		a.mu.Unlock()
		a.lease.claim.mu.Lock()
		if a.lease.claim.preparedAccess == a {
			a.lease.claim.preparedAccess = nil
		}
		a.lease.claim.mu.Unlock()
		return struct{}{}, nil
	}, nil)
	return err
}
