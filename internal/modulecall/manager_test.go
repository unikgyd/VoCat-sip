package modulecall

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"vocat/internal/modem"
)

type testAudio struct {
	mu              sync.Mutex
	started, closed bool
	done            chan struct{}
	frames          chan []int16
}

func newTestAudio() *testAudio {
	return &testAudio{done: make(chan struct{}), frames: make(chan []int16, 4)}
}
func (a *testAudio) Codec() string { return "PCM" }
func (a *testAudio) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrChanged
	}
	a.started = true
	return nil
}
func (a *testAudio) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.done)
	}
	return nil
}
func (a *testAudio) Done() <-chan struct{} { return a.done }
func (a *testAudio) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.done:
		return nil, ErrClosed
	case frame := <-a.frames:
		return frame, nil
	}
}
func (a *testAudio) WritePCM(frame []int16) error {
	a.mu.Lock()
	ready := a.started && !a.closed
	a.mu.Unlock()
	if !ready {
		return ErrState
	}
	a.frames <- append([]int16(nil), frame...)
	return nil
}

type testLine struct {
	mu                     sync.Mutex
	owner, current         bool
	rows                   []Record
	order                  []string
	audio                  *testAudio
	openError, actionError error
	openWait               <-chan struct{}
	opened                 chan struct{}
	retainHangup           bool
	hangupAttempts         int
	executeEntered         chan struct{}
	executeWait            <-chan struct{}
}

func (l *testLine) Current() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.owner && l.current }
func (l *testLine) Close()        { l.mu.Lock(); l.owner = false; l.mu.Unlock() }
func (l *testLine) Read(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.owner || !l.current {
		return nil, ErrChanged
	}
	return append([]Record(nil), l.rows...), nil
}
func (l *testLine) OpenAudio(_ context.Context, current func() bool) (Audio, error) {
	l.mu.Lock()
	l.order = append(l.order, "audio:open")
	wait, opened, fail := l.openWait, l.opened, l.openError
	l.audio = newTestAudio()
	audio := l.audio
	l.mu.Unlock()
	if opened != nil {
		close(opened)
	}
	if wait != nil {
		<-wait
	}
	if fail != nil {
		return nil, fail
	}
	return audio, nil
}
func (l *testLine) Execute(ctx context.Context, a Action) (bool, error) {
	if l.executeEntered != nil {
		close(l.executeEntered)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-l.executeWait:
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.current {
		return false, ErrChanged
	}
	l.order = append(l.order, "action:"+a.Kind)
	switch a.Kind {
	case "dial":
		l.rows = []Record{{Index: 1, Direction: 0, Number: a.Number}}
	case "answer":
		l.rows[0].State = 0
	case "hangup":
		l.hangupAttempts++
		if !l.retainHangup {
			l.rows = nil
			l.current = false
		}
	}
	return true, l.actionError
}

type testBackend struct {
	mu             sync.Mutex
	lines          map[string]*testLine
	availableError error
}

type delayedAcquireBackend struct {
	Backend
	entered chan struct{}
	release <-chan struct{}
}

func (b delayedAcquireBackend) Acquire(ctx context.Context, id string) (Lease, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return b.Backend.Acquire(ctx, id)
	}
}

func TestUSBModuleShutdownWaitsForAlreadyAdmittedPreparation(t *testing.T) {
	line := &testLine{}
	entered, release := make(chan struct{}), make(chan struct{})
	b := delayedAcquireBackend{Backend: &testBackend{lines: map[string]*testLine{"a": line}}, entered: entered, release: release}
	m := New(context.Background(), b)
	dialed := make(chan error, 1)
	go func() { _, err := m.DialCall(context.Background(), "a", "+15550000001"); dialed <- err }()
	<-entered
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closed <- m.Close(ctx)
	}()
	wait(t, func() bool { return !m.accepting() })
	if _, err := m.DialCall(context.Background(), "b", "+15550000002"); !errors.Is(err, ErrClosed) {
		t.Fatal("shutdown admitted a new module call", err)
	}
	close(release)
	if err := <-dialed; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	line.mu.Lock()
	defer line.mu.Unlock()
	if !reflect.DeepEqual(line.order, []string{"audio:open", "action:dial", "action:hangup"}) || line.owner {
		t.Fatal("shutdown missed the call prepared during acquisition", line.order, line.owner)
	}
}

func (b *testBackend) Available(context.Context, string) error { return b.availableError }
func (b *testBackend) Acquire(_ context.Context, id string) (Lease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	line := b.lines[id]
	if line == nil {
		return nil, ErrUnavailable
	}
	line.mu.Lock()
	defer line.mu.Unlock()
	if line.owner {
		return nil, ErrBusy
	}
	line.owner = true
	line.current = true
	return line, nil
}
func fixture(t *testing.T) (*Manager, *testLine) {
	t.Helper()
	line := &testLine{}
	b := &testBackend{lines: map[string]*testLine{"a": line}}
	m := New(context.Background(), b)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m, line
}
func wait(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("synthetic module call did not reach the required state")
}
func TestUSBModuleCallPreparesAudioBeforeDialAndCarriesPCM(t *testing.T) {
	m, line := fixture(t)
	call, err := m.DialCall(context.Background(), "a", "+15550000001")
	if err != nil || !ID(call.ID) {
		t.Fatal(call, err)
	}
	line.mu.Lock()
	order := append([]string(nil), line.order...)
	line.mu.Unlock()
	if !reflect.DeepEqual(order, []string{"audio:open", "action:dial"}) {
		t.Fatal(order)
	}
	wait(t, func() bool {
		calls, _ := m.Calls("a")
		return len(calls) == 1 && calls[0].State == "active" && calls[0].MediaReady
	})
	media, err := m.CallMedia(context.Background(), "a", call.ID)
	if err != nil {
		t.Fatal(err)
	}
	frame := []int16{2, -4, 8, -16}
	if err = media.WritePCM(frame); err != nil {
		t.Fatal(err)
	}
	got, err := media.ReadPCM(context.Background())
	if err != nil || !reflect.DeepEqual(frame, got) {
		t.Fatal(got, err)
	}
	if err = m.HangupCall(context.Background(), "a", call.ID); err != nil {
		t.Fatal(err)
	}
	if m.Busy("a") {
		t.Fatal("confirmed cleanup retained the call")
	}
	if _, err = m.CallMedia(context.Background(), "a", call.ID); err == nil {
		t.Fatal("ended call still exposes PCM")
	}
}

func TestUSBModuleUnknownReleaseIsObservedWithoutRepeatingAT(t *testing.T) {
	m, line := fixture(t)
	call, err := m.DialCall(context.Background(), "a", "+15550000001")
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { calls, _ := m.Calls("a"); return len(calls) == 1 && calls[0].MediaReady })
	line.mu.Lock()
	line.retainHangup = true
	line.actionError = ErrUncertain
	line.mu.Unlock()
	if err = m.HangupCall(context.Background(), "a", call.ID); !errors.Is(err, ErrUncertain) {
		t.Fatal("unknown release was reported as confirmed", err)
	}
	// Wait through at least two watcher polls; explicit retries also only read.
	time.Sleep(450 * time.Millisecond)
	if err = m.HangupCall(context.Background(), "a", call.ID); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	line.mu.Lock()
	if line.hangupAttempts != 1 {
		t.Fatal("uncertain release was retransmitted", line.hangupAttempts)
	}
	line.mu.Unlock()
	if !m.Busy("a") {
		t.Fatal("unknown release allowed another call")
	}
	if _, err = m.CallMedia(context.Background(), "a", call.ID); err == nil {
		t.Fatal("unknown release retained media")
	}
	line.mu.Lock()
	line.rows = nil
	line.mu.Unlock()
	wait(t, func() bool { return !m.Busy("a") })
}

func TestUSBModuleShutdownDeadlineCancelsATWaitingBehindAnotherOperation(t *testing.T) {
	line := &testLine{executeEntered: make(chan struct{}), executeWait: make(chan struct{})}
	m := New(context.Background(), &testBackend{lines: map[string]*testLine{"a": line}})
	dialed := make(chan error, 1)
	go func() { _, err := m.DialCall(context.Background(), "a", "+15550000001"); dialed <- err }()
	<-line.executeEntered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shutdown incorrectly confirmed a blocked command", err)
	}
	select {
	case err := <-dialed:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left the AT waiter using the caller's live context")
	}
	line.mu.Lock()
	defer line.mu.Unlock()
	if !reflect.DeepEqual(line.order, []string{"audio:open"}) {
		t.Fatal("a dial was written after shutdown", line.order)
	}
}
func TestUSBModuleCallAudioFailureNeverDialsOrAnswers(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		t.Run(map[bool]string{false: "dial", true: "answer"}[incoming], func(t *testing.T) {
			m, line := fixture(t)
			line.openError = errors.New("synthetic audio format failure")
			if incoming {
				line.rows = []Record{{Index: 1, Direction: 1, State: 4, Number: "+15550000002"}}
				calls, err := m.Poll(context.Background(), "a")
				if err != nil || len(calls) != 1 {
					t.Fatal(calls, err)
				}
				_, err = m.AnswerCall(context.Background(), "a", calls[0].ID)
				if !errors.Is(err, ErrNotAttempted) {
					t.Fatal(err)
				}
			} else {
				_, err := m.DialCall(context.Background(), "a", "+15550000001")
				if err == nil {
					t.Fatal("missing audio accepted")
				}
			}
			line.mu.Lock()
			defer line.mu.Unlock()
			for _, op := range line.order {
				if op == "action:dial" || op == "action:answer" {
					t.Fatal("charged action ran before audio admission", line.order)
				}
			}
		})
	}
}
func TestUSBModuleCallIncomingFirstAnswerAndUnownedActiveCall(t *testing.T) {
	m, line := fixture(t)
	line.rows = []Record{{Index: 1, Direction: 1, State: 0, Number: "+15550000002"}}
	if calls, err := m.Poll(context.Background(), "a"); err != nil || len(calls) != 0 {
		t.Fatal("adopted an external active call", calls, err)
	}
	line.mu.Lock()
	line.rows[0].State = 4
	line.mu.Unlock()
	calls, err := m.Poll(context.Background(), "a")
	if err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
	id := calls[0].ID
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, e := m.AnswerCall(context.Background(), "a", id); results <- e }()
	}
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("answer owners %v / %v", first, second)
	}
	line.mu.Lock()
	count := 0
	for _, op := range line.order {
		if op == "action:answer" {
			count++
		}
	}
	line.mu.Unlock()
	if count != 1 {
		t.Fatalf("%d physical answers", count)
	}
}
func TestUSBModuleCallReplacementAndStaleHangupCannotTouchNextCall(t *testing.T) {
	m, line := fixture(t)
	old, err := m.DialCall(context.Background(), "a", "+15550000001")
	if err != nil {
		t.Fatal(err)
	}
	line.mu.Lock()
	line.current = false
	line.rows = []Record{{Index: 1, Direction: 1, State: 4, Number: "+15550000002"}}
	line.mu.Unlock()
	wait(t, func() bool { return !m.Busy("a") })
	calls, err := m.Poll(context.Background(), "a")
	if err != nil || len(calls) != 1 || calls[0].ID == old.ID {
		t.Fatal(calls, err)
	}
	if err = m.HangupCall(context.Background(), "a", old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	line.mu.Lock()
	defer line.mu.Unlock()
	for _, op := range line.order {
		if op == "action:hangup" {
			t.Fatal("stale cleanup touched replacement", line.order)
		}
	}
}
func TestUSBModuleCallLateCanceledAudioKeepsOtherModuleIndependent(t *testing.T) {
	m, line := fixture(t)
	ready, release := make(chan struct{}), make(chan struct{})
	line.openWait = release
	line.opened = ready
	other := &testLine{}
	m.backend.(*testBackend).lines["b"] = other
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.DialCall(ctx, "a", "+15550000001"); done <- err }()
	<-ready
	cancel()
	if _, err := m.DialCall(context.Background(), "b", "+15550000003"); err != nil {
		t.Fatal("another module was blocked", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("late canceled audio was accepted")
	}
	line.mu.Lock()
	defer line.mu.Unlock()
	line.audio.mu.Lock()
	defer line.audio.mu.Unlock()
	if !line.audio.closed || line.owner {
		t.Fatal("late audio or physical reservation leaked")
	}
	for _, op := range line.order {
		if op == "action:dial" {
			t.Fatal("canceled preparation dialed")
		}
	}
}
func TestUSBModuleCallStrictCLCCRejectsAmbiguityAndRetainsDataRecords(t *testing.T) {
	for _, body := range []string{`+CLCC: x,1,4,0,0`, `+CLCC: 1,1,4,0`, `+CLCC: 8,1,4,0,0`, "+CLCC: 1,1,4,0,0\n+CLCC: 1,1,4,0,0", `+CLCC: 1,1,4,8,0`} {
		if _, err := ParseRecords(modem.Response{Final: "OK", Lines: splitLines(body)}); err == nil {
			t.Fatalf("accepted ambiguous CLCC: %s", body)
		}
	}
	rows, err := ParseRecords(modem.Response{Final: "OK", Lines: []string{`+CLCC: 1,1,0,1,0,"",128`, `+CLCC: 2,1,4,0,0,"+15550000001",145`}})
	if err != nil || len(rows) != 2 || len(VoiceRecords(rows)) != 1 {
		t.Fatal(rows, err)
	}
}
func splitLines(s string) []string {
	var result []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	return append(result, s[start:])
}
