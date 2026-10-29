package hostimsruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"vocat/internal/vowifi"
)

type StateHandler func(context.Context, vowifi.State) error
type RuntimeFactory func(context.Context, string) (*Runtime, error)

type Options struct {
	Logger           *slog.Logger // Kept API-compatible; no raw provider errors are logged.
	OperationTimeout time.Duration
	PollInterval     time.Duration
	RetryInitial     time.Duration
	RetryMaximum     time.Duration
	OnState          StateHandler
	Factory          RuntimeFactory
}

var (
	ErrClosed              = errors.New("host IMS manager: manager is closed")
	ErrOperationInProgress = ErrBusy
	ErrCall                = errors.New("host IMS manager: call operation failed")
	ErrSMSSubmit           = errors.New("host IMS manager: SMS submission failed; delivery outcome may be unknown")
	ErrUSSISubmit          = errors.New("host IMS manager: USSI operation failed; outcome may be unknown")
)

// Manager owns independent per-device workers. Its global mutex only protects
// the entry map and closed flag; no factory, provider, callback or modem I/O is
// ever performed under it. It does not select a radio mode or change RF state.
type Manager struct {
	mu        sync.Mutex
	closed    bool
	entries   map[string]*managedLine
	owners    map[*Runtime]*managedLine
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	options   Options
	closeGate chan struct{}
}

type managedLine struct {
	id          string
	mu          sync.Mutex
	runtime     *Runtime
	desired     bool
	maintenance bool
	shutdown    bool
	reconnect   bool
	busy        bool
	generation  uint64
	completed   uint64
	genContext  context.Context
	genCancel   context.CancelFunc
	opCancel    context.CancelFunc
	nextRetry   time.Time
	failures    uint
	failure     string
	attempt     uint64
	startedAt   *time.Time
	state       vowifi.State
	callsHash   [32]byte
	wake        chan struct{}
	changed     chan struct{}
	gate        chan struct{}
	updates     chan vowifi.State
	probe       uint64
}

func New(options Options) *Manager {
	if options.OperationTimeout <= 0 {
		options.OperationTimeout = 2 * time.Minute
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 250 * time.Millisecond
	}
	if options.RetryInitial <= 0 {
		options.RetryInitial = 2 * time.Second
	}
	if options.RetryMaximum < options.RetryInitial {
		options.RetryMaximum = 30 * time.Second
		if options.RetryMaximum < options.RetryInitial {
			options.RetryMaximum = options.RetryInitial
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Manager{ctx: ctx, cancel: cancel, options: options, entries: map[string]*managedLine{}, owners: map[*Runtime]*managedLine{}, closeGate: gate}
}

// Ensure creates only an entry. The lightweight Runtime factory runs later on
// that entry's enable worker, outside all global locks; it must not start RF.
func (m *Manager) Ensure(ctx context.Context, deviceID string) error {
	ctx = callerContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if deviceID == "" || strings.ContainsAny(deviceID, "\r\n\x00") {
		return ErrConfig
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.entries[deviceID] != nil {
		return nil
	}
	if m.options.Factory == nil || len(m.entries) >= 128 {
		return ErrConfig
	}
	gen, cancel := context.WithCancel(context.Background())
	e := &managedLine{id: deviceID, genContext: gen, genCancel: cancel, wake: make(chan struct{}, 1),
		changed: make(chan struct{}), gate: make(chan struct{}, 1), updates: make(chan vowifi.State, 8),
		state: vowifi.State{DeviceID: deviceID, Phase: vowifi.PhaseIdle, DataplaneMode: "cellular-host-ims", Sequence: 1, UpdatedAt: time.Now().UTC()}}
	e.gate <- struct{}{}
	e.updates <- cloneManagerState(e.state)
	m.entries[deviceID] = e
	m.wg.Add(3)
	go m.worker(e)
	go m.monitor(e)
	go m.publisher(e)
	signalLine(e)
	return nil
}

func signalLine(e *managedLine) {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func changedLine(e *managedLine) { close(e.changed); e.changed = make(chan struct{}) }

func (m *Manager) entry(id string) (*managedLine, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[id], m.closed
}

func (m *Manager) State(id string) (vowifi.State, error) {
	e, closed := m.entry(id)
	if e == nil {
		if closed {
			return vowifi.State{}, ErrClosed
		}
		if err := m.Ensure(m.ctx, id); err != nil {
			return vowifi.State{}, err
		}
		e, _ = m.entry(id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneManagerState(e.state), nil
}

func advanceLine(e *managedLine) (context.CancelFunc, context.CancelFunc) {
	old, operationCancel := e.genCancel, e.opCancel
	e.genContext, e.genCancel = context.WithCancel(context.Background())
	e.generation++
	e.nextRetry = time.Time{}
	changedLine(e)
	return old, operationCancel
}

func (m *Manager) request(id string, enabled, reconnect bool) (vowifi.State, error) {
	if _, closed := m.entry(id); closed {
		return vowifi.State{}, ErrClosed
	}
	if err := m.Ensure(m.ctx, id); err != nil {
		return vowifi.State{}, err
	}
	e, closed := m.entry(id)
	if closed {
		return vowifi.State{}, ErrClosed
	}
	e.mu.Lock()
	if e.shutdown {
		e.mu.Unlock()
		return vowifi.State{}, ErrClosed
	}
	if e.maintenance && enabled {
		state := cloneManagerState(e.state)
		e.mu.Unlock()
		return state, nil
	}
	changed := e.desired != enabled || reconnect || !enabled
	e.desired = enabled
	if reconnect {
		e.reconnect = true
	}
	var cancel, opCancel context.CancelFunc
	if changed {
		cancel, opCancel = advanceLine(e)
	} else {
		e.nextRetry = time.Time{}
	}
	state := cloneManagerState(e.state)
	state.Enabled = enabled
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if opCancel != nil {
		opCancel()
	}
	signalLine(e)
	return state, nil
}

func (m *Manager) RequestEnabled(id string, enabled bool) (vowifi.State, error) {
	return m.request(id, enabled, false)
}
func (m *Manager) RequestReconnect(id string) (vowifi.State, error) { return m.request(id, true, true) }

func (m *Manager) BeginMaintenance(id string) error {
	if err := m.Ensure(m.ctx, id); err != nil {
		return err
	}
	e, closed := m.entry(id)
	if closed {
		return ErrClosed
	}
	e.mu.Lock()
	e.maintenance = true
	cancel, opCancel := advanceLine(e)
	e.mu.Unlock()
	cancel()
	if opCancel != nil {
		opCancel()
	}
	signalLine(e)
	return nil
}

func (m *Manager) EndMaintenance(id string) {
	e, _ := m.entry(id)
	if e == nil {
		return
	}
	e.mu.Lock()
	e.maintenance = false
	e.nextRetry = time.Time{}
	e.mu.Unlock()
	signalLine(e)
}

func (m *Manager) ModemSMSSyncBlocked(id string) bool {
	e, _ := m.entry(id)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.busy || e.runtime != nil || e.desired
}

func (m *Manager) worker(e *managedLine) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-e.wake:
		case <-ticker.C:
		}
		m.drive(e)
	}
}

func (m *Manager) drive(e *managedLine) {
	e.mu.Lock()
	desired, rt, force := e.desired && !e.maintenance && !e.shutdown, e.runtime, e.reconnect
	if !e.nextRetry.IsZero() && time.Now().Before(e.nextRetry) {
		e.mu.Unlock()
		return
	}
	if rt == nil && !desired {
		gen := e.generation
		e.failure = ""
		e.mu.Unlock()
		m.refresh(e)
		e.mu.Lock()
		e.completed = gen
		changedLine(e)
		e.mu.Unlock()
		return
	}
	gen, parent := e.generation, e.genContext
	if rt != nil && desired && !force {
		e.mu.Unlock()
		ready := rt.Snapshot().Ready
		e.mu.Lock()
		if e.generation != gen || e.runtime != rt {
			e.mu.Unlock()
			signalLine(e)
			return
		}
		if ready {
			e.completed = gen
			changedLine(e)
			e.mu.Unlock()
			return
		}
	}
	e.busy = true
	ctx, cancel := context.WithTimeout(parent, m.options.OperationTimeout)
	e.opCancel = cancel
	e.mu.Unlock()
	defer cancel()
	select {
	case <-e.gate:
		defer func() { e.gate <- struct{}{} }()
	case <-ctx.Done():
		m.finishLine(e, gen, "", false)
		return
	}
	if rt != nil && desired && !force && rt.Snapshot().Ready {
		m.finishLine(e, gen, "", true)
		return
	}
	if rt != nil {
		if !m.stopRuntime(rt) {
			m.finishLine(e, gen, "cleanup_failed", false)
			return
		}
		m.releaseRuntime(e, rt)
	}
	if !m.currentDesired(e, gen) {
		m.finishLine(e, gen, "", false)
		return
	}
	rt, err := m.options.Factory(ctx, e.id)
	if rt != nil {
		if !m.claimRuntime(e, rt) {
			// A faulty factory must not let this line stop another line's
			// already-owned Runtime, even when the returned device ID is wrong.
			m.finishLine(e, gen, "prepare_failed", false)
			return
		}
		e.mu.Lock()
		e.runtime = rt
		e.mu.Unlock()
	}
	if err != nil || rt == nil || rt.config.DeviceID != e.id || !m.currentDesired(e, gen) || ctx.Err() != nil {
		failure := "prepare_failed"
		if !m.currentDesired(e, gen) || ctx.Err() != nil {
			failure = ""
		}
		if rt != nil {
			if !m.stopRuntime(rt) {
				failure = "cleanup_failed"
			} else {
				m.releaseRuntime(e, rt)
			}
		}
		m.finishLine(e, gen, failure, false)
		return
	}
	e.mu.Lock()
	e.attempt++
	now := time.Now().UTC()
	e.startedAt = &now
	e.reconnect = false
	e.mu.Unlock()
	err = rt.Start(ctx)
	if err != nil || !m.currentDesired(e, gen) || ctx.Err() != nil {
		failure := "registration_failed"
		if !m.currentDesired(e, gen) || ctx.Err() != nil {
			failure = ""
		}
		if !m.stopRuntime(rt) {
			failure = "cleanup_failed"
		} else {
			m.releaseRuntime(e, rt)
		}
		m.finishLine(e, gen, failure, false)
		return
	}
	m.finishLine(e, gen, "", true)
}

func (m *Manager) currentDesired(e *managedLine, gen uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation == gen && e.desired && !e.maintenance && !e.shutdown
}

func (m *Manager) claimRuntime(e *managedLine, rt *Runtime) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner := m.owners[rt]; owner != nil && owner != e {
		return false
	}
	m.owners[rt] = e
	return true
}

func (m *Manager) releaseRuntime(e *managedLine, rt *Runtime) {
	e.mu.Lock()
	if e.runtime == rt {
		e.runtime = nil
	}
	e.mu.Unlock()
	m.mu.Lock()
	if m.owners[rt] == e {
		delete(m.owners, rt)
	}
	m.mu.Unlock()
}

func (m *Manager) stopRuntime(rt *Runtime) bool {
	ctx, cancel := context.WithTimeout(context.Background(), m.options.OperationTimeout)
	defer cancel()
	return rt.Stop(ctx) == nil
}

func (m *Manager) finishLine(e *managedLine, gen uint64, failure string, ready bool) {
	e.mu.Lock()
	e.busy, e.opCancel = false, nil
	if e.generation != gen {
		// An explicit stop/reconnect/maintenance request has already cleared
		// the old backoff. Its generation must not inherit a late worker's
		// failure, retry deadline or completion acknowledgement.
		e.mu.Unlock()
		m.refresh(e)
		signalLine(e)
		return
	}
	e.failure = failure
	if failure != "" {
		e.failures++
		delay := m.options.RetryInitial
		for i := uint(1); i < e.failures && delay < m.options.RetryMaximum; i++ {
			if delay > m.options.RetryMaximum/2 {
				delay = m.options.RetryMaximum
				break
			}
			delay *= 2
		}
		if delay > m.options.RetryMaximum {
			delay = m.options.RetryMaximum
		}
		e.nextRetry = time.Now().Add(delay)
	} else if ready || !e.desired {
		e.failures = 0
		e.nextRetry = time.Time{}
	}
	e.mu.Unlock()
	m.refresh(e)
	e.mu.Lock()
	changed := e.generation != gen
	if !changed {
		e.completed = gen
	}
	changedLine(e)
	e.mu.Unlock()
	if changed {
		signalLine(e)
	}
}

func runtimeCard(rt *Runtime) (cardProvisioning, Access) {
	rt.mu.Lock()
	access := rt.access
	rt.mu.Unlock()
	if nilValue(access) {
		return cardProvisioning{}, nil
	}
	return rt.readCard(access), access
}

func (m *Manager) monitor(e *managedLine) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.options.PollInterval)
	defer ticker.Stop()
	m.refresh(e)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.refresh(e)
		}
	}
}

func (m *Manager) refresh(e *managedLine) {
	e.mu.Lock()
	e.probe++
	probe := e.probe
	rt, gen, desired, busy, failure, attempt := e.runtime, e.generation, e.desired, e.busy, e.failure, e.attempt
	started := e.startedAt
	e.mu.Unlock()
	state := vowifi.State{DeviceID: e.id, Enabled: desired, Phase: vowifi.PhaseIdle, DataplaneMode: "cellular-host-ims", Attempt: attempt, StartedAt: started}
	var callsHash [32]byte
	if rt != nil {
		snapshot := rt.Snapshot()
		card, access := runtimeCard(rt)
		if validProvisioning(card) {
			state.ICCID, state.IMSI, state.HomeMCC, state.HomeMNC = card.identity.ICCID, card.identity.IMSI, card.identity.HomeMCC, card.identity.HomeMNC
			state.SIMReady = true
		}
		state.Active = !nilValue(access)
		if !nilValue(access) {
			state.AccessReady = access.Evidence().Established
		}
		switch snapshot.State {
		case Preparing:
			state.IMSRegistration = "preparing"
			if state.SIMReady {
				state.Phase = vowifi.PhaseSIMReady
			}
		case Registering:
			state.IMSRegistration = "registering"
			if state.AccessReady {
				state.Phase = vowifi.PhaseAccessReady
			} else if state.SIMReady {
				state.Phase = vowifi.PhaseSIMReady
			}
		case Stopping:
			state.IMSRegistration, state.Phase = "stopping", vowifi.PhaseStopping
		case Failed:
			state.IMSRegistration, state.Phase = "failed", vowifi.PhaseFailed
			failure = snapshot.Failure
		}
		if session := rt.Session(); session != nil && snapshot.Ready && desired && !busy {
			evidence := session.Evidence()
			if evidence.Registered {
				if !nilValue(access) {
					state.AccessReady = true
				}
				state.IMSReady, state.IMSRegistration, state.Phase = true, "registered", vowifi.PhaseIMSReady
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				sms, err := session.EnableSMS(ctx)
				cancel()
				state.SMSReady = err == nil && sms.Ready
				if state.SMSReady {
					state.Phase = vowifi.PhaseSMSReady
				}
				if number, source, ok := vowifi.ExtractAssociatedMSISDN(evidence); ok {
					state.PhoneNumber, state.PhoneNumberSource = number, source
				}
				if calls, ok := session.(vowifi.CallController); ok {
					var text strings.Builder
					for _, call := range calls.Calls() {
						fmt.Fprintf(&text, "%s:%s:%t:%t;", call.ID, call.State, call.MediaReady, call.SignalingPending)
					}
					callsHash = sha256.Sum256([]byte(text.String()))
				}
			}
		}
	} else if busy {
		state.IMSRegistration = "preparing"
	}
	if failure != "" {
		state.LastErrorClass, state.LastError = "host_ims_"+failure, "Host IMS operation failed"
		state.Phase = vowifi.PhaseFailed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.generation != gen || e.runtime != rt || e.probe != probe {
		return
	}
	old := cloneManagerState(e.state)
	state.Sequence, state.UpdatedAt = old.Sequence, old.UpdatedAt
	if reflect.DeepEqual(old, state) && e.callsHash == callsHash {
		return
	}
	state.Sequence, state.UpdatedAt = old.Sequence+1, time.Now().UTC()
	e.state, e.callsHash = cloneManagerState(state), callsHash
	select {
	case e.updates <- cloneManagerState(state):
	default:
		select {
		case <-e.updates:
		default:
		}
		select {
		case e.updates <- cloneManagerState(state):
		default:
		}
	}
}

func cloneManagerState(state vowifi.State) vowifi.State {
	state.Warnings = append([]string(nil), state.Warnings...)
	state.CleanupErrors = append([]string(nil), state.CleanupErrors...)
	if state.StartedAt != nil {
		at := *state.StartedAt
		state.StartedAt = &at
	}
	return state
}

func (m *Manager) publisher(e *managedLine) {
	defer m.wg.Done()
	apply := func(state vowifi.State) {
		if m.options.OnState == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.options.OnState(ctx, cloneManagerState(state)) // Never log raw callback/provider errors or identities.
	}
	for {
		select {
		case state := <-e.updates:
			apply(state)
		case <-m.ctx.Done():
			for {
				select {
				case state := <-e.updates:
					apply(state)
				default:
					return
				}
			}
		}
	}
}

// DesiredEnabled includes an admitted retry/backoff, without queuing new work.
func (m *Manager) DesiredEnabled(id string) (bool, error) {
	e, closed := m.entry(id)
	if closed {
		return false, ErrClosed
	}
	if e == nil {
		return false, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.desired && !e.maintenance && !e.shutdown, nil
}

// StopDevice joins an acknowledged stop generation, not an optimistic !Active
// snapshot. It is suitable for mode switches, eSIM maintenance and deletion.
func (m *Manager) StopDevice(ctx context.Context, id string) error {
	ctx = callerContext(ctx)
	e, closed := m.entry(id)
	if e == nil {
		if closed {
			return ErrClosed
		}
		return nil
	}
	if closed && m.ctx.Err() != nil {
		e.mu.Lock()
		held := e.runtime != nil
		e.mu.Unlock()
		if held {
			return ErrCleanup
		}
		return nil
	}
	e.mu.Lock()
	e.desired, e.reconnect = false, false
	cancel, opCancel := advanceLine(e)
	target := e.generation
	e.mu.Unlock()
	cancel()
	if opCancel != nil {
		opCancel()
	}
	signalLine(e)
	return waitStoppedLine(ctx, e, target)
}

func waitStoppedLine(ctx context.Context, e *managedLine, target uint64) error {
	for {
		e.mu.Lock()
		done, held, change := e.completed >= target && !e.busy, e.runtime != nil, e.changed
		e.mu.Unlock()
		if done {
			if held {
				return ErrCleanup
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-change:
		}
	}
}

// Close starts every device's stop concurrently. An incomplete entry is kept
// for retry by another Close; no failed owner is silently discarded.
func (m *Manager) Close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(callerContext(ctx), m.options.OperationTimeout)
	defer cancel()
	select {
	case <-m.closeGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { m.closeGate <- struct{}{} }()
	m.mu.Lock()
	m.closed = true
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		done := make(chan struct{})
		go func() { m.wg.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	entries := make([]*managedLine, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	results := make(chan error, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		e.shutdown, e.desired = true, false
		stop, opStop := advanceLine(e)
		target := e.generation
		e.mu.Unlock()
		stop()
		if opStop != nil {
			opStop()
		}
		signalLine(e)
		go func(e *managedLine, target uint64) { results <- waitStoppedLine(ctx, e, target) }(e, target)
	}
	var failure error
	for range entries {
		select {
		case err := <-results:
			if err != nil {
				failure = errors.Join(failure, err)
			}
		case <-ctx.Done():
			return errors.Join(ErrCleanup, ctx.Err())
		}
	}
	if failure != nil {
		return failure
	}
	m.cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) withSession(ctx context.Context, id string, action func(context.Context, vowifi.IMSSession, cardProvisioning) error) error {
	ctx = callerContext(ctx)
	e, closed := m.entry(id)
	if closed {
		return ErrClosed
	}
	if e == nil {
		return ErrNotRegistered
	}
	e.mu.Lock()
	gen, generationContext := e.generation, e.genContext
	admitted := e.desired && !e.maintenance && !e.shutdown && !e.busy && e.runtime != nil
	e.mu.Unlock()
	if !admitted {
		return ErrNotRegistered
	}
	callContext, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(generationContext, cancel)
	defer stop()
	select {
	case <-callContext.Done():
		return callContext.Err()
	case <-e.gate:
	}
	defer func() { e.gate <- struct{}{} }()
	e.mu.Lock()
	rt, allowed := e.runtime, e.generation == gen && e.desired && !e.maintenance && !e.shutdown && !e.busy
	e.mu.Unlock()
	if !allowed || rt == nil {
		return ErrNotRegistered
	}
	session := rt.Session()
	if session == nil {
		return ErrNotRegistered
	}
	card, access := runtimeCard(rt)
	if !validProvisioning(card) || nilValue(access) || access.Verify(callContext, card.identity) != nil {
		return ErrNotRegistered
	}
	if err := callContext.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	current := e.generation == gen && e.desired && !e.maintenance && !e.shutdown
	e.mu.Unlock()
	if !current {
		return ErrNotRegistered
	}
	return action(callContext, session, card)
}

func safeActionError(ctx context.Context, err, fallback error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fallback
}

func (m *Manager) Calls(id string) (result []vowifi.Call, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, closed := m.entry(id)
	if closed {
		return nil, ErrClosed
	}
	if e == nil {
		return nil, ErrNotRegistered
	}
	e.mu.Lock()
	gen, generationContext, rt := e.generation, e.genContext, e.runtime
	busy := e.busy
	e.mu.Unlock()
	if busy {
		return nil, ErrBusy
	}
	readContext, stopRead := context.WithCancel(ctx)
	defer stopRead()
	stopGeneration := context.AfterFunc(generationContext, stopRead)
	defer stopGeneration()
	// Calls is a thread-safe, in-memory snapshot. It must remain readable
	// during a pending hold/resume transaction; a transient empty call list
	// would otherwise make the browser release that transaction's audio owner.
	if readContext.Err() != nil {
		return nil, readContext.Err()
	}
	e.mu.Lock()
	current := e.generation == gen && e.runtime == rt && !e.busy && !e.shutdown
	empty := rt == nil && (e.state.Phase == vowifi.PhaseIdle || e.state.Phase == vowifi.PhaseFailed)
	e.mu.Unlock()
	if !current || readContext.Err() != nil {
		return nil, ErrBusy
	}
	if rt == nil {
		if empty {
			return []vowifi.Call{}, nil
		}
		return nil, ErrNotRegistered
	}
	m.mu.Lock()
	owned := m.owners[rt] == e
	m.mu.Unlock()
	if !owned || rt.config.DeviceID != id {
		return nil, ErrNotRegistered
	}
	// Unlike business operations, this is an in-memory cleanup admission
	// check. A lost registration or failed cleanup must not hide retained
	// calls, nor require new SIM/network I/O before StopDevice can run.
	rt.mu.Lock()
	session, state, operation := rt.session, rt.state, rt.op
	rt.mu.Unlock()
	if operation != nil || state != Ready && state != Failed && state != Stopped {
		return nil, ErrBusy
	}
	if nilValue(session) {
		if state == Failed || state == Stopped {
			return []vowifi.Call{}, nil
		}
		return nil, ErrNotRegistered
	}
	calls, ok := session.(vowifi.CallController)
	if !ok {
		return nil, vowifi.ErrNotRunning
	}
	result = calls.Calls()
	e.mu.Lock()
	current = e.generation == gen && e.runtime == rt && !e.busy && !e.shutdown
	e.mu.Unlock()
	if !current || readContext.Err() != nil {
		return nil, ErrBusy
	}
	return result, nil
}
func (m *Manager) DialCall(ctx context.Context, id, number string) (result vowifi.Call, err error) {
	ctx = callerContext(ctx)
	err = m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		c, ok := s.(vowifi.CallController)
		if !ok {
			return vowifi.ErrNotRunning
		}
		var e error
		result, e = c.DialCall(ctx, number)
		return safeActionError(ctx, e, ErrCall)
	})
	return
}
func (m *Manager) AnswerCall(ctx context.Context, id, callID string) (result vowifi.Call, err error) {
	ctx = callerContext(ctx)
	err = m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		c, ok := s.(vowifi.CallController)
		if !ok {
			return vowifi.ErrNotRunning
		}
		var e error
		result, e = c.AnswerCall(ctx, callID)
		return safeActionError(ctx, e, ErrCall)
	})
	return
}
func (m *Manager) HangupCall(ctx context.Context, id, callID string) error {
	ctx = callerContext(ctx)
	return m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		c, ok := s.(vowifi.CallController)
		if !ok {
			return vowifi.ErrNotRunning
		}
		return safeActionError(ctx, c.HangupCall(ctx, callID), ErrCall)
	})
}
func (m *Manager) CallMedia(ctx context.Context, id, callID string) (result vowifi.CallMedia, err error) {
	ctx = callerContext(ctx)
	err = m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		c, ok := s.(vowifi.CallMediaController)
		if !ok {
			return vowifi.ErrNotRunning
		}
		var e error
		result, e = c.CallMedia(ctx, callID)
		return safeActionError(ctx, e, ErrCall)
	})
	return
}

func (m *Manager) SendDTMF(ctx context.Context, id, callID string, digit byte) error {
	ctx = callerContext(ctx)
	return m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		controller, ok := s.(vowifi.CallDTMFController)
		if !ok {
			return vowifi.ErrNotRunning
		}
		return safeActionError(ctx, controller.SendDTMF(ctx, callID, digit), ErrCall)
	})
}

func (m *Manager) SetCallHold(ctx context.Context, id, callID string, held bool) (result vowifi.Call, err error) {
	ctx = callerContext(ctx)
	err = m.withSession(ctx, id, func(ctx context.Context, session vowifi.IMSSession, _ cardProvisioning) error {
		controller, ok := session.(vowifi.CallHoldController)
		if !ok {
			return vowifi.ErrNotRunning
		}
		var actionErr error
		result, actionErr = controller.SetCallHold(ctx, callID, held)
		return safeActionError(ctx, actionErr, ErrCall)
	})
	return
}
func (m *Manager) SendSMS(ctx context.Context, id string, request vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	return m.sendSMS(ctx, id, "", "", request)
}
func (m *Manager) SendSMSForSubscription(ctx context.Context, id, imsi, iccid string, request vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	if imsi == "" || iccid == "" {
		return vowifi.SMSSubmitResult{}, vowifi.ErrSMSNotReady
	}
	return m.sendSMS(ctx, id, imsi, iccid, request)
}
func (m *Manager) sendSMS(ctx context.Context, id, imsi, iccid string, request vowifi.SMSSubmitRequest) (result vowifi.SMSSubmitResult, err error) {
	ctx = callerContext(ctx)
	err = m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, card cardProvisioning) error {
		if imsi != "" && (card.identity.IMSI != imsi || card.identity.ICCID != iccid) {
			return vowifi.ErrSMSNotReady
		}
		sms, e := s.EnableSMS(ctx)
		if e != nil || !sms.Ready {
			return vowifi.ErrSMSNotReady
		}
		sender, ok := s.(vowifi.SMSSender)
		if !ok {
			return vowifi.ErrSMSNotReady
		}
		result, e = sender.SendSMS(ctx, request)
		return safeActionError(ctx, e, ErrSMSSubmit)
	})
	return
}
func (m *Manager) SendUSSI(ctx context.Context, id string, request vowifi.USSISubmitRequest) (result vowifi.USSISubmitResult, err error) {
	ctx = callerContext(ctx)
	err = m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		sender, ok := s.(vowifi.USSISender)
		if !ok {
			return vowifi.ErrUSSINotReady
		}
		var e error
		result, e = sender.SendUSSI(ctx, request)
		return safeActionError(ctx, e, ErrUSSISubmit)
	})
	return
}
func (m *Manager) CancelUSSI(ctx context.Context, id, sessionID string) error {
	ctx = callerContext(ctx)
	return m.withSession(ctx, id, func(ctx context.Context, s vowifi.IMSSession, _ cardProvisioning) error {
		sender, ok := s.(vowifi.USSICanceler)
		if !ok {
			return vowifi.ErrUSSINotReady
		}
		return safeActionError(ctx, sender.CancelUSSI(ctx, sessionID), ErrUSSISubmit)
	})
}
