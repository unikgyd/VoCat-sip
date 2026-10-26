package modulecall

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"vocat/internal/callrecord"
	"vocat/internal/vowifi"
)

type Manager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	backend    Backend
	mu         sync.Mutex
	closed     bool
	closing    bool
	entries    map[string]*entry
	recordCall callrecord.Factory
}
type entry struct {
	id   string
	gate chan struct{}
	call *call
}
type call struct {
	deviceID                 string
	public                   vowifi.Call
	line                     Lease
	audio                    Audio
	ctx                      context.Context
	cancel                   context.CancelFunc
	record                   *Record
	claimed, started, ending bool
	hangupAttempted          bool
	queryFailures            int
	observed                 bool
	recordingAttempted       bool
	signalingOnly            bool
}

func New(parent context.Context, backend Backend, recordings ...callrecord.Factory) *Manager {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{ctx: ctx, cancel: cancel, backend: backend, entries: map[string]*entry{}}
	if len(recordings) == 1 {
		m.recordCall = recordings[0]
	}
	return m
}
func (m *Manager) entry(id string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		e = &entry{id: id, gate: make(chan struct{}, 1)}
		e.gate <- struct{}{}
		m.entries[id] = e
	}
	return e
}
func (m *Manager) lock(ctx context.Context, e *entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return ErrClosed
	case <-e.gate:
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		e.gate <- struct{}{}
		return ErrClosed
	}
	return nil
}
func unlock(e *entry) { e.gate <- struct{}{} }
func (m *Manager) current(e *entry, c *call) bool {
	m.mu.Lock()
	owned := !m.closed && e.call == c && c.public.EndedAt == nil
	m.mu.Unlock()
	return owned && c.line.Current()
}
func (m *Manager) Calls(id string) ([]vowifi.Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; e != nil && e.call != nil {
		view := e.call.public
		view.AnswerConfirmed = view.State == "active" && view.EndedAt == nil
		return []vowifi.Call{view}, nil
	}
	return []vowifi.Call{}, nil
}
func (m *Manager) Owns(id, cid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	return e != nil && e.call != nil && e.call.public.ID == cid
}
func (m *Manager) Busy(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	return e != nil && e.call != nil && (e.call.public.EndedAt == nil || e.call.public.SignalingPending)
}
func (m *Manager) Available(ctx context.Context, id string) error {
	if !m.accepting() {
		return ErrClosed
	}
	return m.backend.Available(ctx, id)
}
func (m *Manager) accepting() bool {
	m.mu.Lock()
	accepting := !m.closed && !m.closing
	m.mu.Unlock()
	return accepting
}

func (m *Manager) begin(e *entry, line Lease, number, direction, state string) (*call, error) {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		line.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	c := &call{deviceID: e.id, line: line, ctx: ctx, cancel: cancel, public: vowifi.Call{ID: "usb-" + hex.EncodeToString(seed[:]), Number: number, Direction: direction, State: state, StartedAt: time.Now().UTC()}}
	m.mu.Lock()
	e.call = c
	m.mu.Unlock()
	return c, nil
}
func (m *Manager) snapshot(c *call) vowifi.Call { m.mu.Lock(); defer m.mu.Unlock(); return c.public }
func actionContext(ctx context.Context, c *call) (context.Context, func()) {
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	if c.ctx.Err() != nil {
		cancel()
	}
	return child, func() { stop(); cancel() }
}
func (m *Manager) prepare(ctx context.Context, e *entry, c *call) error {
	if !m.current(e, c) {
		return ErrChanged
	}
	// A canceled preparation cannot return a late sound device to another call.
	stop := context.AfterFunc(ctx, c.cancel)
	audio, err := c.line.OpenAudio(c.ctx, func() bool { return m.current(e, c) })
	stopped := stop()
	if err == nil && audio == nil {
		err = ErrUnavailable
	}
	if err == nil && (!stopped || ctx.Err() != nil || !m.current(e, c)) {
		err = ErrChanged
	}
	if err != nil {
		if audio != nil {
			_ = audio.Close()
		}
		return err
	}
	m.mu.Lock()
	c.audio = audio
	m.mu.Unlock()
	return nil
}

func (m *Manager) DialCall(ctx context.Context, id, number string) (vowifi.Call, error) {
	return m.dialCall(ctx, id, number, false)
}

func (m *Manager) DialSignalingCall(ctx context.Context, id, number string) (vowifi.Call, error) {
	return m.dialCall(ctx, id, number, true)
}

func (m *Manager) dialCall(ctx context.Context, id, number string, signalingOnly bool) (vowifi.Call, error) {
	if !ValidNumber(number) {
		return vowifi.Call{}, ErrState
	}
	e := m.entry(id)
	if err := m.lock(ctx, e); err != nil {
		return vowifi.Call{}, err
	}
	defer unlock(e)
	if !m.accepting() {
		return vowifi.Call{}, ErrClosed
	}
	if m.Busy(id) {
		return vowifi.Call{}, ErrBusy
	}
	var line Lease
	var err error
	if signalingOnly {
		backend, ok := m.backend.(SignalingBackend)
		if !ok {
			return vowifi.Call{}, ErrUnavailable
		}
		if err := backend.AvailableSignaling(ctx, id); err != nil {
			return vowifi.Call{}, err
		}
		line, err = backend.AcquireSignaling(ctx, id)
	} else {
		if err := m.backend.Available(ctx, id); err != nil {
			return vowifi.Call{}, err
		}
		line, err = m.backend.Acquire(ctx, id)
	}
	if err != nil {
		return vowifi.Call{}, err
	}
	rows, err := line.Read(ctx)
	if err != nil || len(VoiceRecords(rows)) != 0 {
		line.Close()
		if err == nil {
			err = ErrBusy
		}
		return vowifi.Call{}, err
	}
	c, err := m.begin(e, line, number, "outgoing", "dialing")
	if err != nil {
		return vowifi.Call{}, err
	}
	c.signalingOnly = signalingOnly
	if !signalingOnly {
		if err = m.prepare(ctx, e, c); err != nil {
			m.finish(c, "failed")
			return vowifi.Call{}, err
		}
	}
	m.mu.Lock()
	c.public.SignalingPending = true
	m.mu.Unlock()
	commandCtx, cancelCommand := actionContext(ctx, c)
	attempted, err := line.Execute(commandCtx, Action{Kind: "dial", Number: number})
	cancelCommand()
	if !attempted {
		m.finish(c, "failed")
		if err == nil {
			err = ErrUncertain
		}
		return vowifi.Call{}, err
	}
	c.claimed = true
	if err != nil {
		c.ending = true
		m.mu.Lock()
		c.public.Reason = "modem dial outcome unknown; cleanup pending"
		m.mu.Unlock()
	}
	go m.watch(e, c)
	return m.snapshot(c), err
}

// Poll creates an incoming identity only from a sole, nonmultiparty ringing
// voice record. An already active external call is never adopted after restart.
func (m *Manager) Poll(ctx context.Context, id string) ([]vowifi.Call, error) {
	e := m.entry(id)
	if err := m.lock(ctx, e); err != nil {
		return nil, err
	}
	defer unlock(e)
	if !m.accepting() {
		return nil, ErrClosed
	}
	if m.Busy(id) {
		return m.Calls(id)
	}
	if err := m.backend.Available(ctx, id); err != nil {
		return nil, err
	}
	line, err := m.backend.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := line.Read(ctx)
	voices := VoiceRecords(rows)
	if err != nil {
		line.Close()
		return nil, err
	}
	if len(voices) != 1 || voices[0].Direction != 1 || voices[0].State != 4 && voices[0].State != 5 || voices[0].Multiparty != 0 {
		line.Close()
		return m.Calls(id)
	}
	r := voices[0]
	c, err := m.begin(e, line, r.Number, "incoming", "ringing")
	if err != nil {
		return nil, err
	}
	c.record = &r
	c.observed = true
	go m.watch(e, c)
	return m.Calls(id)
}

func (m *Manager) AnswerCall(ctx context.Context, id, cid string) (vowifi.Call, error) {
	e := m.entry(id)
	if err := m.lock(ctx, e); err != nil {
		return vowifi.Call{}, errors.Join(ErrNotAttempted, err)
	}
	defer unlock(e)
	if !m.accepting() {
		return vowifi.Call{}, errors.Join(ErrNotAttempted, ErrClosed)
	}
	c := m.get(e, cid)
	if c == nil || c.claimed || c.public.Direction != "incoming" || c.public.State != "ringing" {
		return vowifi.Call{}, errors.Join(ErrNotAttempted, ErrState)
	}
	if err := m.prepare(ctx, e, c); err != nil {
		m.finish(c, "failed")
		return vowifi.Call{}, errors.Join(ErrNotAttempted, err)
	}
	commandCtx, cancelCommand := actionContext(ctx, c)
	attempted, err := c.line.Execute(commandCtx, Action{Kind: "answer", Expected: c.record})
	cancelCommand()
	if !attempted {
		m.finish(c, "failed")
		if err == nil {
			err = ErrUncertain
		}
		return vowifi.Call{}, errors.Join(ErrNotAttempted, err)
	}
	c.claimed = true
	if err != nil {
		c.ending = true
	}
	m.mu.Lock()
	c.public.SignalingPending = true
	m.mu.Unlock()
	return m.snapshot(c), err
}

func (m *Manager) get(e *entry, cid string) *call {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.call != nil && e.call.public.ID == cid && e.call.public.EndedAt == nil {
		return e.call
	}
	return nil
}
func (m *Manager) CallMedia(ctx context.Context, id, cid string) (vowifi.CallMedia, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	e := m.entries[id]
	if e == nil || e.call == nil || e.call.public.ID != cid {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	c := e.call
	if c.public.EndedAt != nil || !c.public.MediaReady || c.public.State != "active" || c.audio == nil {
		m.mu.Unlock()
		return nil, ErrState
	}
	audio := c.audio
	m.mu.Unlock()
	if !m.current(e, c) {
		return nil, ErrChanged
	}
	return audio, nil
}
func (m *Manager) SendDTMF(ctx context.Context, id, cid string, digit byte) error {
	if !stringsContainsDigit(digit) {
		return ErrState
	}
	e := m.entry(id)
	if err := m.lock(ctx, e); err != nil {
		return err
	}
	defer unlock(e)
	c := m.get(e, cid)
	if c == nil || !c.claimed || !c.started || c.ending {
		return ErrState
	}
	commandCtx, cancelCommand := actionContext(ctx, c)
	defer cancelCommand()
	_, err := c.line.Execute(commandCtx, Action{Kind: "dtmf", Expected: c.record, Digit: digit})
	return err
}
func stringsContainsDigit(digit byte) bool {
	return digit >= '0' && digit <= '9' || digit == '*' || digit == '#' || digit >= 'A' && digit <= 'D'
}

func (m *Manager) HangupCall(ctx context.Context, id, cid string) error {
	e := m.entry(id)
	if err := m.lock(ctx, e); err != nil {
		return err
	}
	defer unlock(e)
	c := m.get(e, cid)
	if c == nil {
		if m.Owns(id, cid) {
			return nil
		}
		return ErrNotFound
	}
	c.ending = true
	m.hideMedia(c)
	return m.end(ctx, c)
}
func (m *Manager) end(ctx context.Context, c *call) error {
	ctx, cancel := actionContext(ctx, c)
	defer cancel()
	if !c.line.Current() {
		m.finish(c, "ended")
		return nil
	}
	rows, err := c.line.Read(ctx)
	if err != nil {
		return ErrUncertain
	}
	voices := VoiceRecords(rows)
	if len(voices) == 0 {
		if c.observed || time.Since(c.public.StartedAt) > 3*time.Second {
			m.finish(c, "ended")
			return nil
		}
		return ErrUncertain
	}
	r, ok := match(c, voices)
	if !ok {
		return ErrUncertain
	}
	c.record = &r
	c.observed = true
	// Once a release may have reached the wire, observe its outcome rather
	// than repeating it against a potentially reused CLCC index.
	if !c.hangupAttempted {
		attempted, executeErr := c.line.Execute(ctx, Action{Kind: "hangup", Expected: &r})
		c.hangupAttempted = attempted
		err = executeErr
	}
	if !c.line.Current() {
		m.finish(c, "ended")
		return nil
	}
	if err != nil {
		return ErrUncertain
	}
	rows, err = c.line.Read(ctx)
	if err == nil && len(VoiceRecords(rows)) == 0 {
		m.finish(c, "ended")
		return nil
	}
	return ErrUncertain
}
func match(c *call, voices []Record) (Record, bool) {
	if len(voices) != 1 || voices[0].Multiparty != 0 {
		return Record{}, false
	}
	r := voices[0]
	if c.record != nil {
		return r, r.Index == c.record.Index && r.Direction == c.record.Direction && (c.record.Number == "" || r.Number == c.record.Number)
	}
	return r, c.public.Direction == "outgoing" && r.Direction == 0 && r.Number == c.public.Number
}
func (m *Manager) hideMedia(c *call) {
	m.mu.Lock()
	c.public.MediaReady = false
	c.public.DTMFDigits = ""
	c.public.SignalingPending = true
	audio := c.audio
	m.mu.Unlock()
	if audio != nil {
		_ = audio.Close()
	}
}
func (m *Manager) finish(c *call, state string) {
	m.hideMedia(c)
	c.cancel()
	c.line.Close()
	now := time.Now().UTC()
	m.mu.Lock()
	c.public.State = state
	c.public.EndedAt = &now
	c.public.SignalingPending = false
	m.mu.Unlock()
}

func (m *Manager) watch(e *entry, c *call) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
		if err := m.lock(ctx, e); err != nil {
			cancel()
			continue
		}
		if m.get(e, c.public.ID) != c {
			unlock(e)
			cancel()
			return
		}
		if !m.current(e, c) {
			m.finish(c, "ended")
			unlock(e)
			cancel()
			return
		}
		if c.ending {
			_ = m.end(ctx, c)
			unlock(e)
			cancel()
			continue
		}
		rows, err := c.line.Read(ctx)
		if err != nil {
			if !c.line.Current() {
				m.finish(c, "ended")
				unlock(e)
				cancel()
				return
			}
			c.queryFailures++
			if c.queryFailures >= 3 {
				c.ending = true
				m.hideMedia(c)
			}
			unlock(e)
			cancel()
			continue
		}
		c.queryFailures = 0
		voices := VoiceRecords(rows)
		if len(voices) == 0 {
			if c.observed || time.Since(c.public.StartedAt) > 3*time.Second {
				m.finish(c, "ended")
			}
			unlock(e)
			cancel()
			continue
		}
		r, ok := match(c, voices)
		if !ok {
			c.ending = true
			m.hideMedia(c)
			unlock(e)
			cancel()
			continue
		}
		c.record = &r
		c.observed = true
		if r.State == 1 {
			c.ending = true
			m.hideMedia(c)
			unlock(e)
			cancel()
			continue
		}
		if r.State == 0 {
			if !c.claimed {
				m.finish(c, "ended")
				unlock(e)
				cancel()
				return
			}
			if c.signalingOnly {
				m.mu.Lock()
				c.public.State = "active"
				c.public.SignalingPending = false
				if c.public.AnsweredAt == nil {
					now := time.Now().UTC()
					c.public.AnsweredAt = &now
				}
				m.mu.Unlock()
				unlock(e)
				cancel()
				continue
			}
			if !c.started {
				m.beginRecording(c)
				if c.audio == nil || c.audio.Start() != nil {
					c.ending = true
					m.hideMedia(c)
					unlock(e)
					cancel()
					continue
				}
				c.started = true
			}
			select {
			case <-c.audio.Done():
				c.ending = true
				m.hideMedia(c)
				unlock(e)
				cancel()
				continue
			default:
			}
			m.mu.Lock()
			c.public.State = "active"
			c.public.MediaReady = true
			c.public.SignalingPending = false
			c.public.Codec = "PCM"
			c.public.DTMFDigits = "0123456789*#ABCD"
			if c.public.AnsweredAt == nil {
				now := time.Now().UTC()
				c.public.AnsweredAt = &now
			}
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			c.public.State = "ringing"
			if r.State == 2 {
				c.public.State = "dialing"
			}
			c.public.SignalingPending = false
			if r.Number != "" {
				c.public.Number = r.Number
			}
			m.mu.Unlock()
		}
		unlock(e)
		cancel()
	}
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	var problems []error
	for _, e := range entries {
		// Wait for any preparation/dial already admitted through this gate.
		// Looking at cached Calls first could miss an in-flight preparation.
		if err := m.lock(ctx, e); err != nil {
			problems = append(problems, err)
			continue
		}
		m.mu.Lock()
		c := e.call
		live := c != nil && c.public.EndedAt == nil
		m.mu.Unlock()
		if live {
			c.ending = true
			m.hideMedia(c)
			if err := m.end(ctx, c); err != nil {
				problems = append(problems, err)
			}
		}
		unlock(e)
	}
	m.mu.Lock()
	m.closed = true
	var retained []*call
	for _, e := range m.entries {
		if e.call != nil && e.call.public.EndedAt == nil {
			retained = append(retained, e.call)
		}
	}
	m.mu.Unlock()
	for _, c := range retained {
		m.hideMedia(c)
		c.cancel()
	}
	m.cancel()
	return errors.Join(problems...)
}
