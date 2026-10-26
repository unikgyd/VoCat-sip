//go:build linux

package modemaudio

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePCM struct {
	in                    chan []int16
	mu                    sync.Mutex
	pending, output       []int16
	blocked               atomic.Bool
	reads, writes, closes atomic.Int32
	writeError            error
}

func newFakePCM() *fakePCM { return &fakePCM{in: make(chan []int16, 64)} }
func (f *fakePCM) read(dst []int16) (int, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		select {
		case f.pending = <-f.in:
		default:
			return 0, errAgain
		}
	}
	n := copy(dst, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}
func (f *fakePCM) write(src []int16) (int, error) {
	f.writes.Add(1)
	if f.blocked.Load() {
		return 0, errAgain
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeError != nil {
		return 0, f.writeError
	}
	n := min(len(src), 73)
	f.output = append(f.output, src[:n]...)
	return n, nil
}
func (f *fakePCM) wait(ctx context.Context, _ bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
		return nil
	}
}
func (f *fakePCM) close() error { f.closes.Add(1); return nil }
func (f *fakePCM) played() []int16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int16(nil), f.output...)
}
func eventuallyAudio(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("audio condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func testAudioService(t *testing.T) (*audioFixture, *Service, *fakePCM, *Session) {
	t.Helper()
	fixture := newAudioFixture(t)
	c, _ := fixture.one(t)
	fake := newFakePCM()
	service := NewService(fixture.r)
	service.open = func(context.Context, *Binding) (pcm, error) { return fake, nil }
	service.available = func() bool { return true }
	session, e := service.Open(context.Background(), c, func() bool { return true })
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = service.Close() })
	return fixture, service, fake, session
}
func TestUSBAudioSessionDuplexCopiesAndPartialWrites(t *testing.T) {
	_, _, fake, s := testAudioService(t)
	if e := s.WritePCM([]int16{1}); !errors.Is(e, ErrNotStarted) {
		t.Fatal(e)
	}
	if fake.reads.Load() != 0 || fake.writes.Load() != 0 {
		t.Fatal("I/O before Start")
	}
	if e := s.Start(); e != nil {
		t.Fatal(e)
	}
	frame := make([]int16, 160)
	for i := range frame {
		frame[i] = int16(i*23 - 1800)
	}
	wanted := append([]int16(nil), frame...)
	fake.in <- append([]int16(nil), frame...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, e := s.ReadPCM(ctx)
	if e != nil || !reflect.DeepEqual(got, wanted) {
		t.Fatal(got, e)
	}
	if e := s.WritePCM(frame); e != nil {
		t.Fatal(e)
	}
	for i := range frame {
		frame[i] = 0
	}
	eventuallyAudio(t, func() bool { return len(fake.played()) == 160 })
	if !reflect.DeepEqual(fake.played(), wanted) {
		t.Fatal("mutated or reordered playback")
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if fake.closes.Load() != 1 {
		t.Fatal(fake.closes.Load())
	}
	if _, e := s.ReadPCM(ctx); e == nil {
		t.Fatal("read after close")
	}
	if e := s.WritePCM(wanted); e == nil {
		t.Fatal("write after close")
	}
	_ = s.Close()
	if fake.closes.Load() != 1 {
		t.Fatal("duplicate native close")
	}
}
func TestUSBAudioSessionRejectsInvalidPCMAndCancelsReads(t *testing.T) {
	_, _, _, s := testAudioService(t)
	if e := s.Start(); e != nil {
		t.Fatal(e)
	}
	for _, samples := range [][]int16{nil, make([]int16, 3201)} {
		if e := s.WritePCM(samples); !errors.Is(e, ErrFormat) {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.ReadPCM(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestUSBAudioQueueBoundAndExpiredSpeechNotReplayed(t *testing.T) {
	_, _, fake, s := testAudioService(t)
	fake.blocked.Store(true)
	if e := s.Start(); e != nil {
		t.Fatal(e)
	}
	old := make([]int16, 160)
	for i := range old {
		old[i] = 7
	}
	for i := 0; i < 40; i++ {
		if e := s.WritePCM(old); e != nil {
			t.Fatal(e)
		}
	}
	if len(s.uplink) > queueFrames || s.Stats().DroppedPlayback == 0 {
		t.Fatal("unbounded playback queue")
	}
	time.Sleep(maxFrameAge + 30*time.Millisecond)
	fake.blocked.Store(false)
	fresh := make([]int16, 160)
	for i := range fresh {
		fresh[i] = 9
	}
	if e := s.WritePCM(fresh); e != nil {
		t.Fatal(e)
	}
	eventuallyAudio(t, func() bool { return len(fake.played()) == 160 })
	for _, sample := range fake.played() {
		if sample != 9 {
			t.Fatal("expired speech replayed")
		}
	}
	for i := 0; i < 40; i++ {
		fake.in <- old
	}
	eventuallyAudio(t, func() bool { return s.Stats().Captured == 40 })
	if len(s.downlink) > queueFrames || s.Stats().DroppedCapture == 0 {
		t.Fatal("unbounded capture queue")
	}
}
func TestUSBAudioReservationsArePerPhysicalDeviceAndStaleCloseCannotReleaseNextOwner(t *testing.T) {
	f := newAudioFixture(t)
	c, _ := f.one(t)
	other := f.usb(t, "1-20", "6")
	f.card(t, other, 3, []int{0}, []int{0})
	service := NewService(f.r)
	service.open = func(context.Context, *Binding) (pcm, error) { return newFakePCM(), nil }
	t.Cleanup(func() { _ = service.Close() })
	first, e := service.Open(context.Background(), c, func() bool { return true })
	if e != nil {
		t.Fatal(e)
	}
	alias := c
	alias.ID = "alias"
	if _, e = service.Open(context.Background(), alias, func() bool { return true }); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	second, e := service.Open(context.Background(), other, func() bool { return true })
	if e != nil {
		t.Fatal(e)
	}
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	replacement, e := service.Open(context.Background(), c, func() bool { return true })
	if e != nil {
		t.Fatal(e)
	}
	_ = first.Close()
	if e = second.Start(); e != nil {
		t.Fatal("sibling disturbed", e)
	}
	if _, e = service.Open(context.Background(), c, func() bool { return true }); !errors.Is(e, ErrBusy) {
		t.Fatal("stale close released replacement", e)
	}
	_ = replacement.Close()
}
func TestUSBAudioLostOwnershipClosesPreparedOrStartedSessions(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "started"}[start], func(t *testing.T) {
			f := newAudioFixture(t)
			c, _ := f.one(t)
			fake := newFakePCM()
			service := NewService(f.r)
			service.open = func(context.Context, *Binding) (pcm, error) { return fake, nil }
			t.Cleanup(func() { _ = service.Close() })
			var current atomic.Bool
			current.Store(true)
			s, e := service.Open(context.Background(), c, current.Load)
			if e != nil {
				t.Fatal(e)
			}
			if start {
				if e = s.Start(); e != nil {
					t.Fatal(e)
				}
			}
			current.Store(false)
			eventuallyAudio(t, func() bool { return fake.closes.Load() == 1 })
			if !errors.Is(s.Err(), ErrChanged) {
				t.Fatal(s.Err())
			}
		})
	}
}
func TestUSBAudioStaleCompletionClosesBothEndpointsBeforeReleasingReservation(t *testing.T) {
	f := newAudioFixture(t)
	c, _ := f.one(t)
	fake := newFakePCM()
	service := NewService(f.r)
	entered, release := make(chan struct{}), make(chan struct{})
	service.open = func(context.Context, *Binding) (pcm, error) { close(entered); <-release; return fake, nil }
	var current atomic.Bool
	current.Store(true)
	result := make(chan error, 1)
	go func() { _, e := service.Open(context.Background(), c, current.Load); result <- e }()
	<-entered
	current.Store(false)
	close(release)
	if e := <-result; !errors.Is(e, ErrChanged) {
		t.Fatal(e)
	}
	if fake.closes.Load() != 1 {
		t.Fatal("lease released without native close")
	}
	service.mu.Lock()
	left := len(service.active)
	service.mu.Unlock()
	if left != 0 {
		t.Fatal(left)
	}
	_ = service.Close()
}
func TestUSBAudioInspectionDoesNotOpenAudioOrClaimBridge(t *testing.T) {
	f := newAudioFixture(t)
	c, _ := f.one(t)
	service := NewService(f.r)
	service.available = func() bool { return true }
	service.open = func(context.Context, *Binding) (pcm, error) { t.Fatal("inspection opened audio"); return nil, nil }
	info, e := service.Inspect(context.Background(), c)
	if e != nil || info.Pairing != "bound" || !info.BackendAvailable || info.VoiceBridgeReady {
		t.Fatal(info, e)
	}
}
