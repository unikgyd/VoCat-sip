// Package hostimsruntime owns one prepared Host IMS line and its SIP session.
// It does not select devices, change radio state, or connect an application UI.
package hostimsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"vocat/internal/hostims"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

// Access is the same SIM, data-plane and security owner for this one line.
// Prepare must return any acquired Access even together with an error, so this
// runtime can retain it until cleanup is acknowledged.
type Access interface {
	vowifi.CellularIMSBearer
	vowifi.CellularIMSSecurity
	vowifi.PreferredAKAProvider
	CardIdentity() hostims.CardIdentity
	Close(context.Context) error
}

type Config struct {
	DeviceID string
	IMS      ims.Config
	Prepare  func(context.Context) (Access, error)
}

type State string

const (
	Stopped     State = "stopped"
	Preparing   State = "preparing"
	Registering State = "registering"
	Ready       State = "ready"
	Stopping    State = "stopping"
	Failed      State = "failed"
)

var (
	ErrConfig        = errors.New("host IMS runtime: invalid configuration")
	ErrBusy          = errors.New("host IMS runtime: operation already in progress or cleanup required")
	ErrPrepare       = errors.New("host IMS runtime: access preparation failed")
	ErrRegistration  = errors.New("host IMS runtime: IMS registration failed")
	ErrNotRegistered = errors.New("host IMS runtime: session is not registered")
	ErrCleanup       = errors.New("host IMS runtime: cleanup is incomplete; stop again to retry")
)

// Snapshot deliberately excludes identities, phone numbers, protocol evidence
// and untrusted error strings. Failure is a fixed machine-readable code.
type Snapshot struct {
	State   State  `json:"state"`
	Ready   bool   `json:"ready"`
	Failure string `json:"failure,omitempty"`
}

type cardProvisioning struct {
	identity                     vowifi.SIMIdentity
	domain, private, public, app string
}

type providerFactory func(vowifi.AKAProvider, ims.Config) (vowifi.IMSProvider, error)
type cardReader func(Access) cardProvisioning

type operation struct {
	start         bool
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	abort         bool // Runtime.mu
	stopRequested bool // Runtime.mu
	err           error
	cleanupErr    error
}

// Runtime is independent of every other line. mu protects ownership and the
// operation gate; it is never held across external I/O. A canceled caller does
// not abandon the worker or any late Access/session that it returns.
type Runtime struct {
	mu         sync.Mutex
	config     Config
	provider   providerFactory
	readCard   cardReader
	state      State
	failure    string
	op         *operation
	access     Access
	session    vowifi.IMSSession
	generation uint64
}

const cleanupTimeout = 15 * time.Second

func (*Runtime) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private Host IMS runtime>")
}

func NewRuntime(config Config) (*Runtime, error) {
	return newRuntimeWith(config, func(aka vowifi.AKAProvider, config ims.Config) (vowifi.IMSProvider, error) {
		return ims.NewProvider(aka, config)
	}, func(access Access) cardProvisioning {
		card := access.CardIdentity()
		return cardProvisioning{identity: card.Subscriber(), domain: card.IMSDomain(),
			private: card.PrivateIdentity(), public: card.PublicIdentity(), app: card.Application()}
	})
}

// The injection seam is private: production callers cannot supply fabricated
// provisioning or bypass the real SIP provider with a successful mock.
func newRuntimeWith(config Config, provider providerFactory, reader cardReader) (*Runtime, error) {
	if config.DeviceID == "" || config.Prepare == nil || provider == nil || reader == nil {
		return nil, ErrConfig
	}
	config.IMS.TransportByPLMN = cloneMap(config.IMS.TransportByPLMN)
	config.IMS.SMSCenterByPLMN = cloneMap(config.IMS.SMSCenterByPLMN)
	return &Runtime{config: config, provider: provider, readCard: reader, state: Stopped}, nil
}

func cloneMap(original map[string]string) map[string]string {
	if original == nil {
		return nil
	}
	copy := make(map[string]string, len(original))
	for key, value := range original {
		copy[key] = value
	}
	return copy
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

func callerContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (r *Runtime) Start(ctx context.Context) error {
	ctx = callerContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	// Refresh a previously ready session's registration status without holding
	// our mutex across an implementation's Evidence method.
	_ = r.Snapshot()
	r.mu.Lock()
	if r.op != nil {
		r.mu.Unlock()
		return ErrBusy
	}
	if r.state == Ready && !nilValue(r.session) {
		r.mu.Unlock()
		return nil
	}
	if !nilValue(r.access) || !nilValue(r.session) {
		r.mu.Unlock()
		return ErrBusy
	}
	worker, cancel := context.WithCancel(ctx)
	op := &operation{start: true, ctx: worker, cancel: cancel, done: make(chan struct{})}
	r.generation++
	r.op, r.state, r.failure = op, Preparing, ""
	r.mu.Unlock()
	go r.runStart(op)
	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
		r.mu.Lock()
		if r.op != op {
			// Completion won the publication gate before cancellation.
			err := op.err
			r.mu.Unlock()
			return err
		}
		op.abort = true
		r.state = Stopping
		r.mu.Unlock()
		op.cancel()
		return ctx.Err()
	}
}

func (r *Runtime) aborted(op *operation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return op.abort || op.ctx.Err() != nil
}

func abortError(op *operation) error {
	if err := op.ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func (r *Runtime) runStart(op *operation) {
	defer op.cancel()
	access, err := r.config.Prepare(op.ctx)
	if !nilValue(access) {
		r.mu.Lock()
		r.access = access
		r.mu.Unlock()
	}
	if r.aborted(op) {
		r.failStart(op, abortError(op), "canceled")
		return
	}
	if err != nil || nilValue(access) {
		r.failStart(op, ErrPrepare, "prepare_failed")
		return
	}
	card := r.readCard(access)
	if !validProvisioning(card) || access.Verify(op.ctx, card.identity) != nil {
		r.failStart(op, ErrPrepare, "prepare_failed")
		return
	}
	config := r.config.IMS
	config.SecurityMode = ims.SecurityRequired
	// The owner capability, never an unrelated global installer, protects this
	// cellular request. Configured or guessed SIP identities cannot replace SIM.
	config.IPSecInstaller = nil
	config.HomeDomain, config.PrivateIdentity, config.PublicIdentity = card.domain, card.private, card.public
	provider, err := r.provider(access, config)
	if r.aborted(op) {
		r.failStart(op, abortError(op), "canceled")
		return
	}
	if err != nil || nilValue(provider) {
		r.failStart(op, ErrRegistration, "register_failed")
		return
	}
	r.mu.Lock()
	if op.abort || op.ctx.Err() != nil {
		r.mu.Unlock()
		r.failStart(op, abortError(op), "canceled")
		return
	}
	r.state = Registering
	r.mu.Unlock()
	// Start's context is already canceled if cancellation won after the gate;
	// production Provider checks that same context before access/network I/O.
	session, err := provider.Start(op.ctx, vowifi.IMSRequest{DeviceID: r.config.DeviceID, Identity: card.identity, Cellular: access})
	if !nilValue(session) {
		r.mu.Lock()
		r.session = session
		r.mu.Unlock()
	}
	if r.aborted(op) {
		r.failStart(op, abortError(op), "canceled")
		return
	}
	if err != nil || nilValue(session) {
		r.failStart(op, ErrRegistration, "register_failed")
		return
	}
	if !session.Evidence().Registered {
		r.failStart(op, ErrNotRegistered, "not_registered")
		return
	}
	r.mu.Lock()
	if op.abort || op.ctx.Err() != nil {
		r.mu.Unlock()
		r.failStart(op, abortError(op), "canceled")
		return
	}
	r.completeLocked(op, nil, Ready, "")
	r.mu.Unlock()
}

func validProvisioning(card cardProvisioning) bool {
	digits := func(value string, min, max int) bool {
		if len(value) < min || len(value) > max {
			return false
		}
		for _, c := range value {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return digits(card.identity.ICCID, 18, 20) && digits(card.identity.IMSI, 5, 16) &&
		card.domain != "" && card.private != "" && card.public != "" && (card.app == "ISIM" || card.app == "USIM")
}

func (r *Runtime) failStart(op *operation, primary error, failure string) {
	r.mu.Lock()
	r.state = Stopping
	r.mu.Unlock()
	cleanupErr := r.cleanup()
	r.mu.Lock()
	defer r.mu.Unlock()
	op.cleanupErr = cleanupErr
	if cleanupErr != nil {
		r.completeLocked(op, errors.Join(primary, ErrCleanup), Failed, "cleanup_failed")
	} else if op.abort || op.ctx.Err() != nil || op.stopRequested {
		r.completeLocked(op, primary, Stopped, "")
	} else {
		r.completeLocked(op, primary, Failed, failure)
	}
}

// cleanup is called only by the single operation worker. A failed session
// close keeps BOTH references: access cannot be torn down under live IMS I/O.
func (r *Runtime) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	r.mu.Lock()
	session, access := r.session, r.access
	r.mu.Unlock()
	if !nilValue(session) {
		if session.Close(ctx) != nil {
			return ErrCleanup
		}
		r.mu.Lock()
		r.session = nil
		r.mu.Unlock()
	}
	if !nilValue(access) {
		if access.Close(ctx) != nil {
			return ErrCleanup
		}
		r.mu.Lock()
		r.access = nil
		r.mu.Unlock()
	}
	return nil
}

func (r *Runtime) completeLocked(op *operation, err error, state State, failure string) {
	op.err = err
	r.state, r.failure, r.op = state, failure, nil
	close(op.done)
}

// Stop coalesces concurrent callers. Canceling its caller never cancels the
// independent cleanup attempt or drops its retained resources. Call Stop
// again after cleanup_failed to retry the same session/access, in that order.
func (r *Runtime) Stop(ctx context.Context) error {
	ctx = callerContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	op := r.op
	if op != nil {
		if op.start {
			op.abort, op.stopRequested = true, true
			r.state = Stopping
			op.cancel()
		}
		r.mu.Unlock()
		select {
		case <-op.done:
			if op.start {
				return op.cleanupErr
			}
			return op.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if nilValue(r.session) && nilValue(r.access) {
		r.state, r.failure = Stopped, ""
		r.mu.Unlock()
		return nil
	}
	op = &operation{done: make(chan struct{})}
	r.generation++
	r.op, r.state, r.failure = op, Stopping, ""
	r.mu.Unlock()
	go func() {
		err := r.cleanup()
		r.mu.Lock()
		defer r.mu.Unlock()
		if err != nil {
			r.completeLocked(op, ErrCleanup, Failed, "cleanup_failed")
		} else {
			r.completeLocked(op, nil, Stopped, "")
		}
	}()
	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	state, failure, session, generation := r.state, r.failure, r.session, r.generation
	r.mu.Unlock()
	if state == Ready && !nilValue(session) && !session.Evidence().Registered {
		r.mu.Lock()
		if r.state == Ready && r.generation == generation && r.op == nil {
			r.state, r.failure = Failed, "registration_lost"
		}
		state, failure = r.state, r.failure
		r.mu.Unlock()
	}
	return Snapshot{State: state, Ready: state == Ready, Failure: failure}
}

// Session is a borrowed application capability, not an ownership transfer.
// The caller must stop this Runtime rather than independently closing it.
func (r *Runtime) Session() vowifi.IMSSession {
	if !r.Snapshot().Ready {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != Ready || r.op != nil {
		return nil
	}
	return r.session
}
