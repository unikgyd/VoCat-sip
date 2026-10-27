package sipgateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/vowifi"
)

var (
	ErrCarrierUnavailable        = errors.New("carrier line is unavailable")
	ErrCarrierForbidden          = errors.New("carrier line is not permitted")
	ErrCarrierBusy               = errors.New("carrier line is busy")
	ErrCarrierMedia              = errors.New("carrier media is unavailable")
	ErrCarrierAnswerNotAttempted = errors.New("carrier answer ownership was not attempted")
)

// Exposes a verified voice session: registered IMS or an explicitly selected
// USB module with its own prepared PCM owner. Implementations MUST NOT silently
// fall back to signaling-only AT, change SIM/mode, register another line, or
// choose another module after a failed request. Call IDs belong to that source.
type CarrierBackend interface {
	Ready(context.Context, string) error
	Calls(context.Context, string) ([]vowifi.Call, error)
	Dial(context.Context, string, string) (vowifi.Call, error)
	Hangup(context.Context, string, string) error
	Media(context.Context, string, string) (vowifi.CallMedia, error)
}

type Option func(*Manager)

func WithCarrierBackend(backend CarrierBackend) Option {
	return func(m *Manager) { m.carrier = backend }
}

type runtimeOptions struct {
	carrier CarrierBackend
	journal *callJournal
	sms     *smsService
}
type carrierEvent struct {
	phase string
	code  int
}
type carrierLeg struct {
	call       *sipCall
	line       CarrierLine
	number     string
	events     chan carrierEvent
	activate   chan struct{}
	activation sync.Once
	started    atomic.Bool
	bridge     atomic.Pointer[carrierPCMBridge]
	cancel     context.CancelFunc // assigned before worker starts; finish owns cancellation
}

func carrierErrorCode(err error) int {
	switch {
	case errors.Is(err, ErrCarrierForbidden):
		return 403
	case errors.Is(err, ErrCarrierBusy):
		return 486
	case errors.Is(err, ErrCarrierMedia):
		return 488
	default:
		return 503
	}
}
func (leg *carrierLeg) post(ctx context.Context, event carrierEvent) {
	select {
	case leg.events <- event:
	case <-ctx.Done():
	}
}
func (leg *carrierLeg) release() {
	r := leg.call.r
	r.mu.Lock()
	if r.carrierBusy[leg.line.DeviceID] == leg {
		delete(r.carrierBusy, leg.line.DeviceID)
	}
	r.mu.Unlock()
}

func (leg *carrierLeg) work(ctx context.Context) {
	r := leg.call.r
	defer leg.release()
	var callID string
	defer func() {
		if bridge := leg.bridge.Load(); bridge != nil {
			bridge.close()
		}
		if callID != "" {
			// Local SIP shutdown never waits here. Only terminate this call;
			// do not deregister IMS, stop a bearer or restore modem settings.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = r.carrier.Hangup(cleanup, leg.line.DeviceID, callID)
		}
	}()
	setup, cancelSetup := context.WithTimeout(ctx, 10*time.Second)
	defer cancelSetup()
	if err := r.carrier.Ready(setup, leg.line.DeviceID); err != nil {
		leg.post(ctx, carrierEvent{code: carrierErrorCode(err)})
		return
	}
	code, err := r.journal.reserve(setup, r.config.Config.Realm, leg.call.caller.Username, leg.call.callerLeg.id, leg.line.DeviceID, leg.number)
	if err != nil {
		leg.post(ctx, carrierEvent{code: code})
		return
	}
	if setup.Err() != nil {
		leg.post(ctx, carrierEvent{code: 503})
		return
	}
	call, err := r.carrier.Dial(setup, leg.line.DeviceID, leg.number)
	callID = call.ID // retain a known ID even when submission result is uncertain
	cancelSetup()
	if err != nil || callID == "" {
		leg.post(ctx, carrierEvent{code: carrierErrorCode(err)})
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastPhase := ""
	active := false
	var mediaErrors <-chan error
	for {
		if ctx.Err() != nil {
			return
		}
		if call.EndedAt != nil || call.State == "ended" || call.State == "failed" {
			code := call.SIPCode
			if code < 300 || code > 699 {
				code = 480
			}
			leg.post(ctx, carrierEvent{phase: "ended", code: code})
			return
		}
		if call.State != lastPhase && (call.State == "ringing" || call.State == "early_media") {
			leg.post(ctx, carrierEvent{phase: call.State})
			lastPhase = call.State
		}
		if !active && call.State == "active" && call.MediaReady {
			mediaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			media, mediaErr := r.carrier.Media(mediaCtx, leg.line.DeviceID, callID)
			cancel()
			if mediaErr != nil || media == nil {
				leg.post(ctx, carrierEvent{code: 488})
				return
			}
			bridge, mediaErr := newCarrierPCMBridge(ctx, leg.call.media, media)
			if mediaErr != nil {
				leg.post(ctx, carrierEvent{code: 488})
				return
			}
			leg.bridge.Store(bridge)
			bridge.bindDTMF(r.carrier, leg.line.DeviceID, callID, call.DTMFDigits)
			c := leg.call
			c.mu.Lock()
			c.description.events.Mask &= bridge.carrierEvents.Mask
			c.media.mu.Lock()
			c.media.events.Mask = c.description.events.Mask
			c.media.mu.Unlock()
			c.mu.Unlock()
			leg.post(ctx, carrierEvent{phase: "active"})
			select {
			case <-leg.activate:
			case <-ctx.Done():
				return
			}
			bridge.start()
			mediaErrors, active = bridge.errors, true
		}
		select {
		case <-ctx.Done():
			return
		case <-mediaErrors:
			leg.post(ctx, carrierEvent{code: 488})
			return
		case <-ticker.C:
			calls, err := r.carrier.Calls(ctx, leg.line.DeviceID)
			if err != nil {
				leg.post(ctx, carrierEvent{code: 503})
				return
			}
			found := false
			for _, item := range calls {
				if item.ID == callID {
					call, found = item, true
					break
				}
			}
			if !found {
				leg.post(ctx, carrierEvent{phase: "ended", code: 480})
				return
			}
		}
	}
}

func (c *sipCall) runCarrier() {
	leg := c.carrier
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.phase = "dialing"
	_ = c.transaction.Respond(c.callerResponse(100, "Trying", nil))
	ctx, cancel := context.WithCancel(c.r.ctx)
	leg.cancel = cancel
	leg.started.Store(true)
	c.mu.Unlock()
	defer cancel()
	go leg.work(ctx)
	ring := time.NewTimer(c.r.callRingTimeout)
	var ringDeadline <-chan time.Time = ring.C
	defer ring.Stop()
	var ackTimeout, retry *time.Timer
	var ackDeadline, retransmit <-chan time.Time
	defer func() {
		if ackTimeout != nil {
			ackTimeout.Stop()
		}
		if retry != nil {
			retry.Stop()
		}
	}()
	ack := c.ack
	delay := 500 * time.Millisecond
	for {
		select {
		case event := <-leg.events:
			c.mu.Lock()
			if c.stopped {
				c.mu.Unlock()
				return
			}
			if event.code != 0 || event.phase == "ended" {
				if c.final == 0 {
					c.final = event.code
					_ = c.transaction.Respond(c.callerResponse(event.code, "Carrier Call Unavailable", nil))
				}
				c.mu.Unlock()
				c.stopCall("carrier")
				return
			}
			if c.final != 0 {
				c.mu.Unlock()
				continue
			}
			switch event.phase {
			case "ringing", "early_media":
				c.phase = event.phase
				code, reason := 180, "Ringing"
				if event.phase == "early_media" {
					code, reason = 183, "Session Progress"
				}
				_ = c.transaction.Respond(c.callerResponse(code, reason, nil))
			case "active":
				c.final, c.phase = 200, "awaiting_ack"
				c.acceptedAt = time.Now()
				c.accepted = c.callerResponse(200, "OK", makeCallAnswer(c.description, c.media.address(), c.media.controlAddress()))
				if err := c.transaction.Respond(c.accepted); err != nil {
					c.mu.Unlock()
					c.stopCall("transport")
					return
				}
				ring.Stop()
				ringDeadline = nil
				leg.activation.Do(func() { close(leg.activate) })
				ackTimeout = time.NewTimer(c.r.callAckTimeout)
				ackDeadline = ackTimeout.C
				retry = time.NewTimer(delay)
				retransmit = retry.C
			}
			c.mu.Unlock()
		case <-ack:
			ack = nil
			if ackTimeout != nil {
				ackTimeout.Stop()
			}
			if retry != nil {
				retry.Stop()
			}
			ackDeadline, retransmit = nil, nil
			c.mu.Lock()
			if c.final == 200 {
				c.phase = "active"
			}
			c.mu.Unlock()
			c.startDialogProbes()
		case <-retransmit:
			_ = c.transaction.Respond(c.accepted)
			delay = min(2*delay, 4*time.Second)
			retry.Reset(delay)
		case <-ackDeadline:
			c.stopCall("ack_timeout")
			return
		case <-ringDeadline:
			c.mu.Lock()
			if c.final == 0 {
				c.final = 480
				_ = c.transaction.Respond(c.callerResponse(480, "Carrier Call Timed Out", nil))
			}
			c.mu.Unlock()
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
