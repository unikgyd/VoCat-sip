package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"vocat/internal/device"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type automaticAnswer struct {
	s      *Server
	mu     sync.Mutex
	closed bool
	jobs   map[string]*automaticAnswerJob
	seen   map[string]time.Time
}
type automaticAnswerJob struct {
	device, call, iccid, revision string // subscription fields use automaticAnswer.mu
	ctx                           context.Context
	cancel                        context.CancelFunc
	done                          chan struct{}
	takeovers                     chan *automaticTakeover
}
type automaticTakeover struct {
	ctx    context.Context
	client string
	reply  chan automaticTakeoverResult
}
type automaticTakeoverResult struct {
	lease *phoneLease
	call  vowifi.Call
	err   error
}

func newAutomaticAnswer(s *Server) *automaticAnswer {
	return &automaticAnswer{s: s, jobs: make(map[string]*automaticAnswerJob), seen: make(map[string]time.Time)}
}
func (a *automaticAnswer) offer(n IncomingCallNotification) {
	if n.DeviceID == "" || n.CallID == "" || len(n.CallID) > 256 {
		return
	}
	key := n.DeviceID + "\x00" + n.CallID
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	now := time.Now()
	for k, at := range a.seen {
		if now.Sub(at) > 24*time.Hour {
			delete(a.seen, k)
		}
	}
	if _, exists := a.seen[key]; exists || len(a.seen) >= 8192 || len(a.jobs) >= 128 {
		a.mu.Unlock()
		return
	}
	a.seen[key] = now
	ctx, cancel := context.WithCancel(context.Background())
	j := &automaticAnswerJob{device: n.DeviceID, call: n.CallID, ctx: ctx, cancel: cancel, done: make(chan struct{}), takeovers: make(chan *automaticTakeover, 1)}
	a.jobs[key] = j
	a.mu.Unlock()
	go func() {
		defer func() { cancel(); a.mu.Lock(); delete(a.jobs, key); a.mu.Unlock(); close(j.done) }()
		if err := a.run(j); err != nil && !errors.Is(err, context.Canceled) && a.s.logger != nil {
			a.s.logger.Info("automatic answering stopped", "category", "call", "device_id", j.device, "call_id", j.call, "error", err)
		}
	}()
}
func (a *automaticAnswer) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	for _, j := range a.jobs {
		j.cancel()
	}
}
func (a *automaticAnswer) cancelSubscription(iccid, keepRevision string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs {
		if j.iccid == iccid && j.revision != keepRevision {
			j.cancel()
		}
	}
}

func (a *automaticAnswer) identity(ctx context.Context, id string) (store.Device, vowifi.SIMIdentity, string, error) {
	config, err := a.s.store.Device(ctx, id)
	if err != nil {
		return config, vowifi.SIMIdentity{}, "", err
	}
	identity, err := a.s.observedTelephonySubscription(ctx, config)
	entry, physical, present := a.s.physicalForConfig(config)
	if err != nil || !present || identity.ICCID == "" || identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) || device.RegionBlockReason(identity.IMSI) != "" || config.Telephony != nil && config.Telephony.ICCID != identity.ICCID {
		return config, identity, "", errors.New("automatic answer subscription is unavailable")
	}
	return config, identity, physical + "\x00" + entry.Candidate.USBGeneration, nil
}
func (a *automaticAnswer) exact(ctx context.Context, j *automaticAnswerJob) (vowifi.Call, error) {
	calls, err := (sipCarrierBackend{a.s}).Calls(ctx, j.device)
	if err != nil {
		return vowifi.Call{}, err
	}
	for _, call := range calls {
		if call.ID == j.call && call.Direction == "incoming" && call.EndedAt == nil && call.State != "ended" && call.State != "failed" {
			return call, nil
		}
	}
	return vowifi.Call{}, errors.New("the original incoming call ended")
}

func sameAnsweringSIM(a, b vowifi.SIMIdentity) bool {
	return a.ICCID == b.ICCID && a.IMSI == b.IMSI
}

func (a *automaticAnswer) run(j *automaticAnswerJob) error {
	probe, cancel := context.WithTimeout(j.ctx, 5*time.Second)
	_, identity, generation, err := a.identity(probe, j.device)
	if err != nil {
		cancel()
		return err
	}
	settings, err := a.s.store.CallAnswering(probe, identity.ICCID)
	if err != nil || !settings.Enabled {
		cancel()
		return err
	}
	call, err := a.exact(probe, j)
	cancel()
	if err != nil || call.State != "ringing" {
		return err
	}
	a.mu.Lock()
	j.iccid = identity.ICCID
	j.revision = settings.Revision
	a.mu.Unlock()
	timer := time.NewTimer(time.Duration(settings.DelaySeconds) * time.Second)
	defer timer.Stop()
	select {
	case <-j.ctx.Done():
		return j.ctx.Err()
	case <-timer.C:
	}
	// The delay belongs to this exact invitation. Loading a prompt and checking
	// the revision precede any 200 OK / ATA submission.
	operation, cancelOperation := context.WithTimeout(j.ctx, 35*time.Second)
	defer cancelOperation()
	_, fresh, newGeneration, err := a.identity(operation, j.device)
	if err != nil || !sameAnsweringSIM(fresh, identity) || newGeneration != generation {
		return errors.New("subscription changed during answer delay")
	}
	settings, pcm, err := a.s.store.CallAnsweringAudio(operation, identity.ICCID, settings.Revision)
	if err != nil {
		return err
	}
	if !settings.Enabled || settings.Prompt == nil || len(pcm) == 0 {
		return errors.New("automatic answer message is no longer enabled")
	}
	if _, err = (sipCarrierBackend{a.s}).Incoming(operation, j.device, j.call); err != nil {
		return err
	}
	p := a.s.webPhone
	l, err := p.reserve(j.device, "automatic:"+j.call)
	if err != nil {
		return err
	}
	owned, attempted, handedOff := false, false, false
	p.mu.Lock()
	if !p.closed && p.leases[j.device] == l {
		l.call = j.call
		l.kind = "auto_answer"
		l.stage = "answering"
		l.attached = true
		l.cancel = j.cancel
		owned = true
	}
	p.mu.Unlock()
	defer func() {
		if !handedOff {
			p.drop(l, attempted)
		}
	}()
	if !owned {
		return context.Canceled
	}
	if err = a.s.claimLocalSIPAnswer(j.device, j.call); err != nil {
		return err
	}
	_, err = (sipCarrierBackend{a.s}).Answer(operation, j.device, j.call)
	attempted = !errors.Is(err, sipgateway.ErrCarrierAnswerNotAttempted)
	if err != nil {
		return err
	}
	// IMS must receive the ACK; a module call must be observed as active by its
	// existing monitor. Early RTP does not count as answer confirmation.
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		call, err = a.exact(operation, j)
		if err != nil {
			return err
		}
		if call.State == "active" && call.AnswerConfirmed && call.MediaReady {
			break
		}
		select {
		case <-operation.Done():
			return operation.Err()
		case <-poll.C:
		}
	}
	media, err := (sipCarrierBackend{a.s}).Media(operation, j.device, j.call)
	if err != nil {
		return err
	}
	maxDuration := time.Duration(settings.MaxCallSeconds) * time.Second
	if call.AnsweredAt != nil {
		maxDuration -= max(time.Duration(0), time.Since(*call.AnsweredAt))
	}
	if maxDuration <= 0 {
		return errors.New("automatic call duration limit reached")
	}
	lifetime, cancelLifetime := context.WithTimeout(j.ctx, maxDuration)
	defer cancelLifetime()
	mediaContext, cancelMedia := context.WithCancel(lifetime)
	defer cancelMedia()
	drained := make(chan struct{})
	mediaErr := make(chan error, 1)
	go func() {
		defer close(drained)
		for {
			if _, err := media.ReadPCM(mediaContext); err != nil {
				select {
				case mediaErr <- err:
				default:
				}
				return
			}
		}
	}()
	validate := func() error {
		ctx, cancel := context.WithTimeout(lifetime, 3*time.Second)
		defer cancel()
		_, live, gen, err := a.identity(ctx, j.device)
		if err != nil || !sameAnsweringSIM(live, identity) || gen != generation {
			return errors.New("automatic call subscription changed")
		}
		now, err := a.s.store.CallAnswering(ctx, identity.ICCID)
		if err != nil {
			return err
		}
		if !now.Enabled || now.Revision != settings.Revision {
			return errors.New("automatic answering settings changed")
		}
		active, err := a.exact(ctx, j)
		if err != nil {
			return err
		}
		if active.State != "active" || !active.AnswerConfirmed || !active.MediaReady || active.Held || active.HoldPending {
			return errors.New("automatic call is no longer ready for message playback")
		}
		return nil
	}
	if err = validate(); err != nil {
		return err
	}
	setStage := func(stage string) {
		p.mu.Lock()
		if p.leases[j.device] == l {
			l.stage = stage
		}
		p.mu.Unlock()
	}
	setStage("playing")
	pace := time.NewTicker(20 * time.Millisecond)
	defer pace.Stop()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	offset := 0
	var playbackEnded time.Time
	for {
		select {
		case <-lifetime.Done():
			return lifetime.Err()
		case err := <-mediaErr:
			return fmt.Errorf("automatic call audio ended: %w", err)
		case request := <-j.takeovers:
			if request.ctx.Err() != nil {
				request.reply <- automaticTakeoverResult{err: request.ctx.Err()}
				continue
			}
			if err = validate(); err != nil {
				request.reply <- automaticTakeoverResult{err: err}
				return err
			}
			// Playback writes happen only in this goroutine. Stop the reader and
			// wait for it before making the shared media available to a browser.
			cancelMedia()
			wait := time.NewTimer(3 * time.Second)
			select {
			case <-drained:
				wait.Stop()
			case <-wait.C:
				err = errors.New("automatic media did not stop")
			case <-request.ctx.Done():
				wait.Stop()
				err = request.ctx.Err()
			}
			if err != nil {
				request.reply <- automaticTakeoverResult{err: err}
				return err
			}
			var seed [32]byte
			if _, err = rand.Read(seed[:]); err != nil {
				request.reply <- automaticTakeoverResult{err: err}
				return err
			}
			call, err = a.exact(request.ctx, j)
			if err != nil {
				request.reply <- automaticTakeoverResult{err: err}
				return err
			}
			next := &phoneLease{device: j.device, call: j.call, client: request.client, token: hex.EncodeToString(seed[:]), readySeen: true, deadline: time.Now().Add(15 * time.Second)}
			p.mu.Lock()
			if p.closed || p.leases[j.device] != l || request.ctx.Err() != nil || lifetime.Err() != nil {
				err = errors.New("automatic call ownership changed")
			} else {
				for _, other := range p.leases {
					if other != l && other.client == request.client {
						err = errors.New("this browser already has a call")
						break
					}
				}
				if err == nil {
					p.leases[j.device] = next
					handedOff = true
				}
			}
			p.mu.Unlock()
			request.reply <- automaticTakeoverResult{lease: next, call: call, err: err}
			return err
		case <-check.C:
			if err = validate(); err != nil {
				return err
			}
		case <-pace.C:
			// Allow a short silence tail for the USB playback queue to consume
			// the final padded frame before tearing down the owned call.
			if !playbackEnded.IsZero() && settings.HangupAfterPlayback && time.Since(playbackEnded) >= 200*time.Millisecond {
				return nil
			}
			frame := make([]int16, 160)
			for i := 0; i < len(frame) && offset < len(pcm); i++ {
				frame[i] = int16(binary.LittleEndian.Uint16(pcm[offset:]))
				offset += 2
			}
			if err = media.WritePCM(frame); err != nil {
				return err
			}
			if offset >= len(pcm) && playbackEnded.IsZero() {
				playbackEnded = time.Now()
				if !settings.HangupAfterPlayback {
					setStage("awaiting_takeover")
				}
			}
		}
	}
}

func (a *automaticAnswer) takeover(ctx context.Context, device, call, client string) automaticTakeoverResult {
	a.mu.Lock()
	j := a.jobs[device+"\x00"+call]
	closed := a.closed
	a.mu.Unlock()
	if closed || j == nil {
		return automaticTakeoverResult{err: errors.New("automatic call is no longer available")}
	}
	request := &automaticTakeover{ctx: ctx, client: client, reply: make(chan automaticTakeoverResult, 1)}
	select {
	case j.takeovers <- request:
	case <-ctx.Done():
		return automaticTakeoverResult{err: ctx.Err()}
	case <-j.done:
		return automaticTakeoverResult{err: errors.New("automatic call ended")}
	}
	select {
	case result := <-request.reply:
		return result
	case <-ctx.Done():
		return automaticTakeoverResult{err: ctx.Err()}
	case <-j.done:
		select {
		case result := <-request.reply:
			return result
		default:
			return automaticTakeoverResult{err: errors.New("automatic call ended")}
		}
	}
}
