package modemaudio

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/modem"
)

const frameSamples = 160
const queueFrames = 16
const maxFrameAge = 320 * time.Millisecond

type pcmFrame struct {
	samples []int16
	at      time.Time
}

type pcm interface {
	read([]int16) (int, error)
	write([]int16) (int, error)
	wait(context.Context, bool) error
	close() error
}

type Service struct {
	resolver  Resolver
	mu        sync.Mutex
	closed    bool
	active    map[string]*Session
	open      func(context.Context, *Binding) (pcm, error)
	available func() bool
}

func NewService(resolver Resolver) *Service {
	return &Service{resolver: resolver, active: make(map[string]*Session), open: openNative, available: nativeAvailable}
}

func (s *Service) Inspect(ctx context.Context, c modem.Candidate) (Info, error) {
	info := Info{SampleRate: 8000, Channels: 1, Pairing: "unavailable"}
	if err := ctx.Err(); err != nil {
		return info, err
	}
	bound, err := s.resolver.Resolve(ctx, c)
	if err == nil {
		v := bound.Info()
		info.Pair = &v
		info.Pairing = "bound"
	} else if errors.Is(err, ErrAmbiguous) {
		info.Pairing = "ambiguous"
	} else if errors.Is(err, ErrChanged) {
		info.Pairing = "changed"
	} else if !errors.Is(err, ErrUnavailable) {
		return info, err
	}
	info.BackendAvailable = s.available()
	// Pairing/backend presence does not claim format, call routing or media.
	return info, ctx.Err()
}

type Statistics struct {
	Captured        uint64
	Played          uint64
	DroppedCapture  uint64
	DroppedPlayback uint64
}

type Session struct {
	service                                           *Service
	binding                                           *Binding
	current                                           func() bool
	ctx                                               context.Context
	cancel                                            context.CancelFunc
	device                                            pcm
	start                                             chan struct{}
	done                                              chan struct{}
	mu                                                sync.Mutex
	started                                           bool
	err                                               error
	writeMu                                           sync.Mutex
	uplink, downlink                                  chan pcmFrame
	captured, played, droppedCapture, droppedPlayback atomic.Uint64
}

// Open reserves both selected endpoints without starting capture/playback.
// The controller must bind current to its device/SIM/call ownership revision.
// No ATD/ATA may be attempted until this admission has succeeded.
func (s *Service) Open(ctx context.Context, c modem.Candidate, current func() bool) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if current == nil || !current() {
		return nil, ErrChanged
	}
	b, err := s.resolver.Resolve(ctx, c)
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	session := &Session{service: s, binding: b, current: current, ctx: child, cancel: cancel, start: make(chan struct{}), done: make(chan struct{}), uplink: make(chan pcmFrame, queueFrames), downlink: make(chan pcmFrame, queueFrames)}
	s.mu.Lock()
	if s.closed {
		err = ErrClosed
	} else if s.active[b.usb] != nil {
		err = ErrBusy
	} else {
		s.active[b.usb] = session
	}
	s.mu.Unlock()
	if err != nil {
		cancel()
		return nil, err
	}
	session.device, err = s.open(child, b)
	if err == nil {
		err = child.Err()
	}
	if err == nil && !current() {
		err = ErrChanged
	}
	if err == nil {
		err = b.Validate(child)
	}
	if err != nil {
		cancel()
		if session.device != nil {
			_ = session.device.close()
		}
		session.release()
		close(session.done)
		return nil, err
	}
	go session.lifetime()
	return session, nil
}

func (s *Session) release() {
	s.service.mu.Lock()
	if s.service.active[s.binding.usb] == s {
		delete(s.service.active, s.binding.usb)
	}
	s.service.mu.Unlock()
}
func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}
func (s *Session) valid() bool { return s.ctx.Err() == nil && s.current() }

func (s *Session) Start() error {
	if err := s.binding.Validate(s.ctx); err != nil {
		return err
	}
	if !s.valid() {
		return ErrChanged
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	if !s.started {
		s.started = true
		close(s.start)
	}
	return nil
}
func (s *Session) Codec() string         { return "PCM" }
func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Stats() Statistics {
	return Statistics{s.captured.Load(), s.played.Load(), s.droppedCapture.Load(), s.droppedPlayback.Load()}
}
func (s *Session) Err() error   { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Session) Close() error { s.cancel(); <-s.done; return s.Err() }

func (s *Service) Close() error {
	s.mu.Lock()
	s.closed = true
	all := make([]*Session, 0, len(s.active))
	for _, session := range s.active {
		all = append(all, session)
		session.cancel()
	}
	s.mu.Unlock()
	var failures []error
	for _, session := range all {
		if err := session.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Session) lifetime() {
	ticker := time.NewTicker(100 * time.Millisecond)
waiting:
	for {
		select {
		case <-s.ctx.Done():
			break waiting
		case <-s.start:
			break waiting
		case <-ticker.C:
			if !s.valid() {
				s.fail(ErrChanged)
				break waiting
			}
			if err := s.binding.Validate(s.ctx); err != nil {
				s.fail(err)
				break waiting
			}
		}
	}
	ticker.Stop()
	var workers sync.WaitGroup
	if s.valid() {
		workers.Add(3)
		go func() { defer workers.Done(); s.capture() }()
		go func() { defer workers.Done(); s.playback() }()
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					if !s.valid() {
						s.fail(ErrChanged)
						return
					}
					if err := s.binding.Validate(s.ctx); err != nil {
						s.fail(err)
						return
					}
				}
			}
		}()
	} else if s.ctx.Err() == nil {
		s.fail(ErrChanged)
	}
	<-s.ctx.Done()
	workers.Wait()
	if err := s.device.close(); err != nil {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
	}
	s.release()
	close(s.done)
}

func (s *Session) ready() error {
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	if !s.current() {
		return ErrChanged
	}
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return ErrNotStarted
	}
	return nil
}
func (s *Session) ReadPCM(ctx context.Context) ([]int16, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, io.EOF
		case frame := <-s.downlink:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := s.ready(); err != nil {
				return nil, err
			}
			if time.Since(frame.at) > maxFrameAge {
				s.droppedCapture.Add(1)
				continue
			}
			return frame.samples, nil
		}
	}
}
func (s *Session) WritePCM(samples []int16) error {
	if len(samples) == 0 || len(samples) > 3200 {
		return ErrFormat
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	for len(samples) > 0 {
		n := min(len(samples), frameSamples)
		frame := append([]int16(nil), samples[:n]...)
		samples = samples[n:]
		enqueue(s.uplink, pcmFrame{frame, time.Now()}, &s.droppedPlayback)
	}
	return nil
}
func enqueue(queue chan pcmFrame, frame pcmFrame, dropped *atomic.Uint64) {
	select {
	case queue <- frame:
		return
	default:
	}
	select {
	case <-queue:
		dropped.Add(1)
	default:
	}
	select {
	case queue <- frame:
	default:
		dropped.Add(1)
	}
}
func (s *Session) capture() {
	buffer := make([]int16, frameSamples)
	pending := make([]int16, 0, frameSamples*2)
	var pendingAt time.Time
	for s.valid() {
		n, err := s.device.read(buffer)
		if n < 0 || n > len(buffer) {
			s.fail(ErrFormat)
			return
		}
		if n > 0 {
			if len(pending) > 0 && time.Since(pendingAt) > maxFrameAge {
				pending = pending[:0]
				s.droppedCapture.Add(1)
			}
			if len(pending) == 0 {
				pendingAt = time.Now()
			}
			pending = append(pending, buffer[:n]...)
			for len(pending) >= frameSamples {
				frame := append([]int16(nil), pending[:frameSamples]...)
				pending = pending[frameSamples:]
				enqueue(s.downlink, pcmFrame{frame, pendingAt}, &s.droppedCapture)
				pendingAt = time.Now()
				s.captured.Add(1)
			}
		}
		if err != nil && !errors.Is(err, errAgain) {
			s.fail(err)
			return
		}
		if n == 0 || errors.Is(err, errAgain) {
			if err := s.device.wait(s.ctx, true); err != nil {
				if s.ctx.Err() == nil {
					s.fail(err)
				}
				return
			}
		}
	}
	if s.ctx.Err() == nil {
		s.fail(ErrChanged)
	}
}
func (s *Session) playback() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.uplink:
			expired := false
			for len(frame.samples) > 0 {
				if !s.valid() {
					if s.ctx.Err() == nil {
						s.fail(ErrChanged)
					}
					return
				}
				if time.Since(frame.at) > maxFrameAge {
					s.droppedPlayback.Add(1)
					expired = true
					break
				}
				n, err := s.device.write(frame.samples)
				if n < 0 || n > len(frame.samples) {
					s.fail(ErrFormat)
					return
				}
				frame.samples = frame.samples[n:]
				if err != nil && !errors.Is(err, errAgain) {
					s.fail(err)
					return
				}
				if n == 0 || errors.Is(err, errAgain) {
					if err := s.device.wait(s.ctx, false); err != nil {
						if s.ctx.Err() == nil {
							s.fail(err)
						}
						return
					}
				}
			}
			if !expired {
				s.played.Add(1)
			}
		}
	}
}
