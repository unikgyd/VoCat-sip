package sipgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"vocat/internal/vowifi"
)

// Incoming validates the exact ringing call without answering it. Answer must
// atomically acquire the original call owner, returning ErrCarrierBusy if another
// consumer already answered. Preflight failures must wrap
// ErrCarrierAnswerNotAttempted; an ordinary error means delivery is uncertain.
// Neither operation may fall back to a different call source or signaling-only AT.
type CarrierIncomingBackend interface {
	CarrierBackend
	Incoming(context.Context, string, string) (vowifi.Call, error)
	Answer(context.Context, string, string) (vowifi.Call, error)
}

type carrierIncoming struct {
	call       *sipCall
	line       CarrierLine
	id         string
	ctx        context.Context
	cancel     context.CancelFunc
	events     chan carrierEvent
	selected   chan *callBranch
	owner      string      // guarded by call.mu: empty, local, sip
	endCarrier atomic.Bool // true only after SIP claims an answer or explicit decline
	bridge     atomic.Pointer[carrierPCMBridge]
	lease      *carrierLeg
}

func incomingKey(deviceID, callID string) string {
	sum := sha256.Sum256([]byte(deviceID + "\n" + callID))
	return hex.EncodeToString(sum[:])
}

// ForwardIncoming is non-blocking with respect to IMS/network I/O. Register the
// group before notifying other consumers so a local answer can cancel it at once.
// Duplicate events never ring a second group during this manager's lifetime.
func (m *Manager) ForwardIncoming(deviceID, callID string) bool {
	if !deviceIDPattern.MatchString(deviceID) || callID == "" || len(callID) > 1024 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.runtime == nil {
		return false
	}
	now := time.Now()
	if m.incomingSeen == nil {
		m.incomingSeen = map[string]time.Time{}
	}
	for key, until := range m.incomingSeen {
		if !until.After(now) {
			delete(m.incomingSeen, key)
		}
	}
	key := incomingKey(deviceID, callID)
	if _, duplicate := m.incomingSeen[key]; duplicate || len(m.incomingSeen) >= 2048 {
		return false
	}
	if !m.runtime.offerIncoming(deviceID, callID) {
		return false
	}
	m.incomingSeen[key] = now.Add(24 * time.Hour)
	return true
}

// ClaimLocalAnswer fences the SIP answer path before the HTTP/web audio owner
// calls IMS AnswerCall. A failed local answer may be retried locally; it never
// causes the SIP group to answer behind the user's back.
func (m *Manager) ClaimLocalAnswer(deviceID, callID string) error {
	m.mu.Lock()
	r := m.runtime
	var c *sipCall
	if r != nil {
		r.mu.Lock()
		if leg := r.carrierBusy[deviceID]; leg != nil && leg.call.inbound != nil && leg.call.inbound.id == callID {
			c = leg.call
		}
		r.mu.Unlock()
	}
	if c != nil {
		c.mu.Lock()
		if c.inbound.owner == "sip" {
			c.mu.Unlock()
			m.mu.Unlock()
			return ErrCarrierBusy
		}
		c.inbound.owner = "local"
		c.inbound.cancel()
		c.mu.Unlock()
	}
	// The local receiver may answer before the asynchronous IMS notification
	// reaches ForwardIncoming. Fence that later admission as well.
	if deviceIDPattern.MatchString(deviceID) && callID != "" && len(callID) <= 1024 {
		if m.incomingSeen == nil {
			m.incomingSeen = map[string]time.Time{}
		}
		now := time.Now()
		for key, until := range m.incomingSeen {
			if !until.After(now) {
				delete(m.incomingSeen, key)
			}
		}
		if len(m.incomingSeen) < 2048 {
			m.incomingSeen[incomingKey(deviceID, callID)] = now.Add(24 * time.Hour)
		}
	}
	m.mu.Unlock()
	if c != nil {
		c.stopCall("local_answer")
	}
	return nil
}

// A SIP-owned PCM stream must not be consumed concurrently by a browser socket.
// A module only permits one live voice call; block an outgoing lease even while
// Dial has not yet returned its ID. Local incoming owners remain unaffected.
func (m *Manager) CarrierOwnsMedia(deviceID, callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtime == nil {
		return false
	}
	r := m.runtime
	r.mu.Lock()
	leg := r.carrierBusy[deviceID]
	r.mu.Unlock()
	if leg == nil {
		return false
	}
	c := leg.call
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.carrier != nil || c.inbound != nil && c.inbound.id == callID && c.inbound.owner == "sip"
}

func (r *runtime) offerIncoming(deviceID, callID string) bool {
	config := r.config.Config
	if !config.CallsEnabled || !config.CarrierEnabled {
		return false
	}
	if _, ok := r.carrier.(CarrierIncomingBackend); !ok {
		return false
	}
	var line CarrierLine
	for _, item := range config.CarrierLines {
		if item.DeviceID == deviceID {
			line = item
			break
		}
	}
	peerTarget := ""
	if peer := config.Peer; peer != nil && peer.Enabled && peer.AllowCallsToPeer {
		for _, route := range peer.IncomingRoutes {
			if route.DeviceID == deviceID {
				peerTarget = route.CallTarget
			}
		}
	}
	if !line.AllowIncoming || len(line.IncomingExtensions) == 0 && peerTarget == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	if r.closed.Load() || len(r.calls) >= 16 || r.carrierBusy[deviceID] != nil {
		return false
	}
	c := &sipCall{r: r, id: randomToken(), caller: Account{Username: "anonymous", DisplayName: "Anonymous"}, callee: Account{Username: strings.Join(line.IncomingExtensions, ", ")}, started: time.Now().UTC(), phase: "ringing", stop: make(chan struct{}), ack: make(chan struct{}), events: make(chan callEvent, 128)}
	// Stable order and observed source pinning; Contact hosts never route traffic.
	for _, user := range line.IncomingExtensions {
		account, exists := r.account(user)
		if !exists || !account.AllowCalls || r.callBusy[user] != nil {
			continue
		}
		bySource := map[string]registration{}
		for _, entry := range r.registrations[user] {
			old, ok := bySource[entry.source]
			if !ok || entry.order > old.order {
				bySource[entry.source] = entry
			}
		}
		sources := make([]string, 0, len(bySource))
		for source := range bySource {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		for _, source := range sources {
			c.branches = append(c.branches, &callBranch{call: c, entry: localDeliveryTarget(bySource[source]), target: account, stop: make(chan struct{})})
		}
	}
	if peerTarget != "" {
		if target, ok := r.peerDeliveryTarget(peerTarget); ok {
			c.branches = append(c.branches, &callBranch{call: c, entry: target, target: Account{Username: peerTarget}, stop: make(chan struct{})})
		}
	}
	if len(c.branches) == 0 || len(c.branches) > 64 || len(r.callWires)+len(c.branches) > 480 || r.branchSlots+len(c.branches) > 480 {
		return false
	}
	ctx, cancel := context.WithCancel(r.ctx)
	in := &carrierIncoming{call: c, line: line, id: callID, ctx: ctx, cancel: cancel, events: make(chan carrierEvent, 8), selected: make(chan *callBranch, 1)}
	in.lease = &carrierLeg{call: c, line: line}
	c.inbound = in
	r.branchSlots += len(c.branches)
	r.calls[c.id], r.carrierBusy[deviceID] = c, in.lease
	for _, branch := range c.branches {
		if !branch.entry.peer {
			r.callBusy[branch.target.Username] = c
		}
	}
	r.work.Add(1)
	go func() { defer r.work.Done(); c.run() }()
	return true
}

var incomingNumberPattern = regexp.MustCompile(`^\+?[0-9]{2,20}$`)

func (in *carrierIncoming) post(phase string, code int) {
	select {
	case in.events <- carrierEvent{phase: phase, code: code}:
	case <-in.ctx.Done():
	}
}

// Observing an already-active call before our own Answer is authoritative. Set
// ownership under the same lock as branch selection, not just a cleanup flag:
// a queued SIP 200 must not subsequently acquire or terminate the web call.
func (in *carrierIncoming) localOwner() {
	in.call.mu.Lock()
	in.owner = "local"
	in.endCarrier.Store(false)
	in.call.mu.Unlock()
}

// Backend operations deliberately do not join runtime.work: a stuck modem must
// not keep a disabled SIP listener or the application alive. Keep its line lease
// until the exact known call has been cleaned up, not until an arbitrary timer.
func (in *carrierIncoming) work() {
	r, ctx := in.call.r, in.ctx
	backend := r.carrier.(CarrierIncomingBackend)
	defer in.lease.release()
	defer func() {
		if bridge := in.bridge.Load(); bridge != nil {
			bridge.close()
		}
		if in.endCarrier.Load() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = backend.Hangup(cleanup, in.line.DeviceID, in.id)
		}
	}()
	setup, cancel := context.WithTimeout(ctx, 5*time.Second)
	call, err := backend.Incoming(setup, in.line.DeviceID, in.id)
	cancel()
	if err != nil || call.ID != in.id || call.Direction != "incoming" || call.State != "ringing" || call.EndedAt != nil {
		in.post("ended", 480)
		return
	}
	in.call.mu.Lock()
	if incomingNumberPattern.MatchString(call.Number) {
		in.call.caller = Account{Username: call.Number, DisplayName: call.Number}
	}
	in.call.mu.Unlock()
	in.post("ready", 0)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var selected *callBranch
	var mediaErrors <-chan error
	for {
		select {
		case <-ctx.Done():
			return
		case branch := <-in.selected:
			selected = branch
			answerCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			call, err = backend.Answer(answerCtx, in.line.DeviceID, in.id)
			stop()
			if errors.Is(err, ErrCarrierBusy) || errors.Is(err, ErrCarrierAnswerNotAttempted) {
				// A web answer raced admission. IMS owns the authoritative claim;
				// never hang up a call answered by another consumer.
				in.localOwner()
				in.post("local", 0)
				return
			}
			if err != nil || call.ID != in.id {
				in.post("ended", 503)
				return
			}
		case <-mediaErrors:
			in.post("ended", 488)
			return
		case <-ticker.C:
			poll, stop := context.WithTimeout(ctx, 3*time.Second)
			calls, pollErr := backend.Calls(poll, in.line.DeviceID)
			stop()
			if pollErr != nil {
				in.post("ended", 503)
				return
			}
			found := false
			for _, item := range calls {
				if item.ID == in.id {
					call, found = item, true
					break
				}
			}
			if !found || call.EndedAt != nil || call.State == "ended" || call.State == "failed" {
				in.post("ended", 480)
				return
			}
			if selected == nil && call.State == "active" {
				in.localOwner()
				in.post("local", 0)
				return
			}
		}
		if selected != nil && call.State == "active" && call.MediaReady && in.bridge.Load() == nil {
			mediaCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			media, mediaErr := backend.Media(mediaCtx, in.line.DeviceID, in.id)
			stop()
			if mediaErr != nil || media == nil {
				in.post("ended", 488)
				return
			}
			selected.mu.Lock()
			sipMedia := selected.media
			selected.mu.Unlock()
			bridge, mediaErr := newCarrierPCMBridge(ctx, sipMedia, media)
			if mediaErr != nil {
				in.post("ended", 488)
				return
			}
			in.bridge.Store(bridge)
			bridge.bindDTMF(backend, in.line.DeviceID, in.id, call.DTMFDigits)
			bridge.start()
			mediaErrors = bridge.errors
			in.post("active", 0)
		}
	}
}

func (c *sipCall) runIncoming() {
	in := c.inbound
	go in.work()
	ring := time.NewTimer(c.r.callRingTimeout)
	defer ring.Stop()
	var deadline <-chan time.Time = ring.C
	remaining := len(c.branches)
	for {
		select {
		case event := <-in.events:
			switch event.phase {
			case "ready":
				c.mu.Lock()
				if c.stopped || in.owner != "" {
					c.mu.Unlock()
					return
				}
				for _, branch := range c.branches {
					c.branchWork.Add(1)
					go branch.run()
				}
				c.mu.Unlock()
			case "active":
				c.mu.Lock()
				c.phase = "active"
				c.mu.Unlock()
				deadline = nil
			case "local":
				c.stopCall("local_answer")
				return
			default:
				c.stopCall("carrier")
				return
			}
		case event := <-c.events:
			if event.response == nil || event.branch.isStopped() || event.branch.remoteEnded.Load() {
				remaining--
				// A global SIP decline terminates the whole call. Ordinary busy,
				// offline or no-answer leaves the independent web receiver alone.
				c.mu.Lock()
				decline := !c.stopped && in.owner == "" && c.winner == nil && event.failure >= 600 && event.failure < 700
				if decline {
					in.endCarrier.Store(true)
				}
				c.mu.Unlock()
				if decline {
					c.stopCall("declined")
					return
				}
				if remaining == 0 && c.winner == nil {
					c.stopCall("no_extensions")
					return
				}
				continue
			}
			c.mu.Lock()
			if c.stopped || in.owner != "" {
				c.mu.Unlock()
				event.branch.stopBranch()
				continue
			}
			in.owner = "sip"
			in.endCarrier.Store(true)
			c.winner, c.phase, c.final = event.branch, "connecting", 200
			ring.Reset(10 * time.Second) // bound IMS answer/media setup, not prior ring duration
			event.branch.selected.Store(true)
			for _, branch := range c.branches {
				if branch != event.branch {
					branch.stopBranch()
				}
			}
			c.mu.Unlock()
			c.r.mu.Lock()
			for _, branch := range c.branches {
				if (event.branch.entry.peer || branch.target.Username != event.branch.target.Username) && c.r.callBusy[branch.target.Username] == c {
					delete(c.r.callBusy, branch.target.Username)
				}
			}
			c.r.mu.Unlock()
			in.selected <- event.branch
		case <-deadline:
			c.stopCall("timeout")
			return
		case <-c.stop:
			return
		case <-c.r.ctx.Done():
			c.stopCall("shutdown")
			return
		}
	}
}
