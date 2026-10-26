package hostimsruntime

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

type featureSession struct {
	*fakeSession
	mu        sync.Mutex
	calls     []vowifi.Call
	smsReady  atomic.Bool
	sends     atomic.Int32
	ussis     atomic.Int32
	cancels   atomic.Int32
	dial      func(context.Context, string) (vowifi.Call, error)
	enable    func(context.Context) (vowifi.SMSEvidence, error)
	submitErr error
}

func featureIMS() *featureSession {
	s := &featureSession{fakeSession: registeredSession()}
	s.smsReady.Store(true)
	return s
}
func (s *featureSession) EnableSMS(ctx context.Context) (vowifi.SMSEvidence, error) {
	if s.enable != nil {
		return s.enable(ctx)
	}
	return vowifi.SMSEvidence{Ready: s.smsReady.Load()}, ctx.Err()
}
func (s *featureSession) Calls() []vowifi.Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]vowifi.Call(nil), s.calls...)
}
func (s *featureSession) DialCall(ctx context.Context, number string) (vowifi.Call, error) {
	if s.dial != nil {
		return s.dial(ctx, number)
	}
	call := vowifi.Call{ID: "synthetic-call", Number: number, State: "dialing"}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	return call, nil
}
func (s *featureSession) AnswerCall(ctx context.Context, id string) (vowifi.Call, error) {
	return vowifi.Call{ID: id, State: "active"}, ctx.Err()
}
func (s *featureSession) HangupCall(ctx context.Context, id string) error { return ctx.Err() }
func (s *featureSession) CallMedia(ctx context.Context, id string) (vowifi.CallMedia, error) {
	return featureMedia{}, ctx.Err()
}
func (s *featureSession) SendSMS(ctx context.Context, request vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	s.sends.Add(1)
	if s.submitErr != nil {
		return vowifi.SMSSubmitResult{}, s.submitErr
	}
	return vowifi.SMSSubmitResult{To: request.Recipient, AllPartsAccepted: true}, ctx.Err()
}
func (s *featureSession) SendUSSI(ctx context.Context, request vowifi.USSISubmitRequest) (vowifi.USSISubmitResult, error) {
	s.ussis.Add(1)
	if s.submitErr != nil {
		return vowifi.USSISubmitResult{}, s.submitErr
	}
	return vowifi.USSISubmitResult{SessionID: "synthetic-ussi"}, ctx.Err()
}
func (s *featureSession) CancelUSSI(ctx context.Context, id string) error {
	s.cancels.Add(1)
	return ctx.Err()
}

type featureMedia struct{}

func (featureMedia) Codec() string                                { return "synthetic-pcm" }
func (featureMedia) ReadPCM(ctx context.Context) ([]int16, error) { return nil, ctx.Err() }
func (featureMedia) WritePCM([]int16) error                       { return nil }

func managerRuntime(t *testing.T, id string, prepare func(context.Context) (Access, error), session vowifi.IMSSession, card cardProvisioning) *Runtime {
	t.Helper()
	r, err := newRuntimeWith(Config{DeviceID: id, Prepare: prepare}, func(vowifi.AKAProvider, ims.Config) (vowifi.IMSProvider, error) {
		return providerFunc(func(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) { return session, nil }), nil
	}, func(Access) cardProvisioning { return card })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func managerOptions(factory RuntimeFactory) Options {
	return Options{Factory: factory, PollInterval: 3 * time.Millisecond, OperationTimeout: 500 * time.Millisecond, RetryInitial: time.Hour, RetryMaximum: time.Hour}
}
func cleanupManager(t *testing.T, manager *Manager) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})
}
func awaitManager(t *testing.T, manager *Manager, id string, predicate func(vowifi.State) bool) vowifi.State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, err := manager.State(id)
		if err == nil && predicate(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	state, _ := manager.State(id)
	t.Fatal("manager state did not converge", state.Phase, state.LastErrorClass, state.IMSReady)
	return vowifi.State{}
}
func releaseOnCleanup(t *testing.T, release chan struct{}) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
}

func TestManagerFactoryAndAcquisitionNeverBlockOtherLines(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnCleanup(t, release)
	var factories atomic.Int32
	second := featureIMS()
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		factories.Add(1)
		if id == "first" {
			close(entered)
			<-release
		}
		session := second
		if id == "first" {
			session = featureIMS()
		}
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, session, testCard()), nil
	}))
	cleanupManager(t, manager)
	if err := manager.Ensure(context.Background(), "first"); err != nil || factories.Load() != 0 {
		t.Fatal("Ensure performed acquisition/factory I/O", err)
	}
	if _, err := manager.RequestEnabled("first", true); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	if _, err := manager.RequestEnabled("second", true); err != nil {
		t.Fatal(err)
	}
	state := awaitManager(t, manager, "second", func(state vowifi.State) bool { return state.IMSReady })
	if state.TunnelReady || state.DataplaneMode != "cellular-host-ims" || !state.AccessReady || !state.SIMReady {
		t.Fatal("Host IMS claimed a VoWiFi tunnel or incomplete state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := manager.StopDevice(ctx, "first"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("factory cancellation dropped the late worker", err)
	}
	close(release)
	awaitManager(t, manager, "first", func(state vowifi.State) bool {
		return !state.Enabled && !state.Active && state.Phase == vowifi.PhaseIdle
	})
	if !second.registered.Load() {
		t.Fatal("one line's cancellation closed another")
	}
}

func TestManagerDisableJoinsLatePrepareBeforeAcknowledgingStop(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnCleanup(t, release)
	a := &fakeAccess{}
	s := featureIMS()
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		return managerRuntime(t, id, func(context.Context) (Access, error) { close(entered); <-release; return a, nil }, s, testCard()), nil
	}))
	cleanupManager(t, manager)
	if _, err := manager.RequestEnabled("line", true); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := manager.StopDevice(ctx, "line"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("late acquisition stop was acknowledged early", err)
	}
	if !manager.ModemSMSSyncBlocked("line") || a.closes.Load() != 0 {
		t.Fatal("pending ownership was forgotten")
	}
	close(release)
	if err := manager.StopDevice(context.Background(), "line"); err != nil {
		t.Fatal(err)
	}
	if a.closes.Load() != 1 || s.closes.Load() != 0 || manager.ModemSMSSyncBlocked("line") {
		t.Fatal("canceled acquire registered or was abandoned")
	}
}

func TestManagerLateFailedCleanupCannotBackoffNewStopGeneration(t *testing.T) {
	for _, action := range []string{"stop", "maintenance", "close"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			releaseOnCleanup(t, release)
			access := &fakeAccess{close: func(_ context.Context, attempt int32) error {
				if attempt == 1 {
					close(entered)
					<-release
				}
				if attempt == 1 {
					return errors.New("synthetic pending-owner cleanup")
				}
				return nil
			}}
			var factories atomic.Int32
			session := featureIMS()
			manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
				factories.Add(1)
				return managerRuntime(t, id, func(context.Context) (Access, error) {
					return access, errors.New("synthetic acquisition with retained owner")
				}, session, testCard()), nil
			}))
			cleanupManager(t, manager)
			_, _ = manager.RequestEnabled("line", true)
			waitSignal(t, entered)
			entry, _ := manager.entry("line")
			entry.mu.Lock()
			oldGeneration := entry.generation
			ownedRuntime := entry.runtime
			entry.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch action {
				case "stop":
					done <- manager.StopDevice(ctx, "line")
				case "maintenance":
					if err := manager.BeginMaintenance("line"); err != nil {
						done <- err
						return
					}
					entry.mu.Lock()
					target := entry.generation
					entry.mu.Unlock()
					done <- waitStoppedLine(ctx, entry, target)
				case "close":
					done <- manager.Close(ctx)
				}
			}()
			// Do not release the old cleanup until the public request has
			// advanced intent. This reproduces the exact late-finish ordering,
			// without timing sleeps or changing the one-hour retry setting.
			for {
				entry.mu.Lock()
				advanced, change := entry.generation > oldGeneration, entry.changed
				entry.mu.Unlock()
				if advanced {
					break
				}
				select {
				case <-change:
				case <-ctx.Done():
					t.Fatal("stop intent was not admitted", ctx.Err())
				}
			}
			// Runtime.Start returns promptly on cancellation. Keep the first
			// failed cleanup blocked until the old manager worker has joined
			// that exact operation through Runtime.Stop, not a second cleanup.
			for {
				ownedRuntime.mu.Lock()
				joined := ownedRuntime.op != nil && ownedRuntime.op.stopRequested
				ownedRuntime.mu.Unlock()
				if joined {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("old worker did not join its canceled runtime", ctx.Err())
				default:
					runtime.Gosched()
				}
			}
			close(release)
			if err := waitResult(t, done); err != nil {
				t.Fatal("new stop generation inherited old cleanup backoff", err)
			}
			entry.mu.Lock()
			held, completed, generation := entry.runtime != nil, entry.completed, entry.generation
			entry.mu.Unlock()
			if held || completed < generation || access.closes.Load() != 2 || factories.Load() != 1 || session.closes.Load() != 0 {
				t.Fatal("cleanup lost its owner, retried registration, or acknowledged the wrong generation")
			}
		})
	}
}

func TestManagerReconnectRequiresAcknowledgedCleanupAndRetainsFailures(t *testing.T) {
	var allowCleanup atomic.Bool
	var factories atomic.Int32
	old := &fakeAccess{close: func(context.Context, int32) error {
		if !allowCleanup.Load() {
			return errors.New("private cleanup detail")
		}
		return nil
	}}
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		a := &fakeAccess{}
		if factories.Add(1) == 1 {
			a = old
		}
		return managerRuntime(t, id, func(context.Context) (Access, error) { return a, nil }, featureIMS(), testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady })
	_, _ = manager.RequestReconnect("line")
	failed := awaitManager(t, manager, "line", func(s vowifi.State) bool {
		return s.Phase == vowifi.PhaseFailed && s.LastErrorClass == "host_ims_cleanup_failed"
	})
	if factories.Load() != 1 || !manager.ModemSMSSyncBlocked("line") || failed.LastError != "Host IMS operation failed" {
		t.Fatal("failed old owner was replaced or leaked raw error")
	}
	allowCleanup.Store(true)
	_, _ = manager.RequestReconnect("line")
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady })
	if factories.Load() != 2 || old.closes.Load() < 2 {
		t.Fatal("reconnect did not retry the same failed owner first")
	}
}

func TestManagerCallsCanProveFailedRetainedAccessHasNoSessionBeforeStopRetry(t *testing.T) {
	var allowCleanup atomic.Bool
	access := &fakeAccess{close: func(context.Context, int32) error {
		if !allowCleanup.Load() {
			return errors.New("synthetic retained access")
		}
		return nil
	}}
	session := featureIMS()
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		return managerRuntime(t, id, func(context.Context) (Access, error) { return access, nil }, session, testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady })
	if err := manager.StopDevice(context.Background(), "line"); !errors.Is(err, ErrCleanup) {
		t.Fatal("first stop did not retain its failed access", err)
	}
	state := awaitManager(t, manager, "line", func(s vowifi.State) bool {
		return s.Phase == vowifi.PhaseFailed && s.Active
	})
	if state.IMSReady || session.closes.Load() != 1 {
		t.Fatal("session was not closed before access cleanup")
	}
	calls, err := manager.Calls("line")
	if err != nil || len(calls) != 0 {
		t.Fatal("retained access without a session could not be enumerated", len(calls), err)
	}
	allowCleanup.Store(true)
	if err := manager.StopDevice(context.Background(), "line"); err != nil {
		t.Fatal("failed owner could not retry stop after read-only admission", err)
	}
	if access.closes.Load() < 2 || manager.ModemSMSSyncBlocked("line") {
		t.Fatal("stop retry did not release the same retained owner")
	}
}

func TestManagerCallsExposesOwnedPendingCallsAfterRegistrationLossWithoutEnablingBusinessIO(t *testing.T) {
	session := featureIMS()
	var dials atomic.Int32
	session.dial = func(context.Context, string) (vowifi.Call, error) {
		dials.Add(1)
		return vowifi.Call{}, nil
	}
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, session, testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady })
	// Keep this retained owner stable while testing the cleanup admission
	// boundary, instead of allowing the normal reconnect worker to stop it.
	entry, _ := manager.entry("line")
	entry.mu.Lock()
	entry.nextRetry = time.Now().Add(time.Hour)
	entry.mu.Unlock()
	session.mu.Lock()
	session.calls = []vowifi.Call{{ID: "pending-owned-call", State: "ended", SignalingPending: true}}
	session.mu.Unlock()
	session.registered.Store(false)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.Phase == vowifi.PhaseFailed && !s.IMSReady })
	calls, err := manager.Calls("line")
	if err != nil || len(calls) != 1 || calls[0].ID != "pending-owned-call" || !calls[0].SignalingPending {
		t.Fatal("registration loss hid retained signaling", len(calls), err)
	}
	if _, err := manager.DialCall(context.Background(), "line", "synthetic-target"); err == nil || dials.Load() != 0 {
		t.Fatal("read-only cleanup capability enabled business I/O", err, dials.Load())
	}
	if _, err := manager.AnswerCall(context.Background(), "line", "pending-owned-call"); err == nil {
		t.Fatal("read-only cleanup capability enabled answering")
	}
	if _, err := manager.CallMedia(context.Background(), "line", "pending-owned-call"); err == nil {
		t.Fatal("read-only cleanup capability enabled media")
	}
	if session.closes.Load() != 0 || session.sends.Load() != 0 {
		t.Fatal("call enumeration performed session cleanup or submitted a message")
	}
}

func TestManagerPublishesLiveCellularEvidenceAndCallsWithoutSubmittingMessages(t *testing.T) {
	session := featureIMS()
	updates := make(chan vowifi.State, 100)
	options := managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, session, testCard()), nil
	})
	options.OnState = func(_ context.Context, state vowifi.State) error { updates <- state; return nil }
	manager := New(options)
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	initial := awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.SMSReady })
	if initial.TunnelReady || !initial.IMSReady || initial.ICCID != testCard().identity.ICCID || initial.IMSI != testCard().identity.IMSI || initial.DataplaneMode != "cellular-host-ims" {
		t.Fatal("state did not retain the owned cellular subscription")
	}
	session.mu.Lock()
	session.calls = []vowifi.Call{{ID: "incoming-call", State: "ringing", Direction: "incoming"}}
	session.mu.Unlock()
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.Sequence > initial.Sequence })
	readyUpdate := false
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !readyUpdate {
		select {
		case state := <-updates:
			readyUpdate = state.IMSReady && !state.TunnelReady
		case <-deadline.C:
			t.Fatal("no asynchronous registered cellular state")
		}
	}
	if session.sends.Load() != 0 || session.ussis.Load() != 0 {
		t.Fatal("polling state submitted a message")
	}
	calls, err := manager.Calls("line")
	if err != nil || len(calls) != 1 {
		t.Fatal("live incoming call unavailable", err)
	}
	if _, err := manager.AnswerCall(context.Background(), "line", "incoming-call"); err != nil {
		t.Fatal(err)
	}
	if err := manager.HangupCall(context.Background(), "line", "incoming-call"); err != nil {
		t.Fatal(err)
	}
	if media, err := manager.CallMedia(context.Background(), "line", "incoming-call"); err != nil || media.Codec() != "synthetic-pcm" {
		t.Fatal("live media controller unavailable", err)
	}
}

func TestManagerBoundSMSUsesFixedOwnedCardNotStaleProjectedState(t *testing.T) {
	session := featureIMS()
	var factories atomic.Int32
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		factories.Add(1)
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, session, testCard()), nil
	}))
	cleanupManager(t, manager)
	if _, err := manager.SendSMSForSubscription(context.Background(), "line", "stale", "stale", vowifi.SMSSubmitRequest{}); err == nil || factories.Load() != 0 {
		t.Fatal("bound submission implicitly started a device")
	}
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.SMSReady })
	e, _ := manager.entry("line")
	e.mu.Lock()
	e.state.IMSI, e.state.ICCID = "stale-imsi", "stale-iccid"
	e.mu.Unlock()
	if _, err := manager.SendSMSForSubscription(context.Background(), "line", "stale-imsi", "stale-iccid", vowifi.SMSSubmitRequest{}); !errors.Is(err, vowifi.ErrSMSNotReady) || session.sends.Load() != 0 {
		t.Fatal("stale projected subscription redirected SMS", err)
	}
	id := testCard().identity
	if result, err := manager.SendSMSForSubscription(context.Background(), "line", id.IMSI, id.ICCID, vowifi.SMSSubmitRequest{Recipient: "synthetic", Text: "local fake"}); err != nil || !result.AllPartsAccepted || session.sends.Load() != 1 {
		t.Fatal("fixed subscriber SMS capability unavailable", err)
	}
	if result, err := manager.SendUSSI(context.Background(), "line", vowifi.USSISubmitRequest{Code: "synthetic"}); err != nil || result.SessionID == "" {
		t.Fatal("USSI capability missing", err)
	}
	if err := manager.CancelUSSI(context.Background(), "line", "synthetic-ussi"); err != nil || session.cancels.Load() != 1 {
		t.Fatal(err)
	}
}

func TestManagerGenerationCancelsApplicationIOBeforeCleanup(t *testing.T) {
	entered := make(chan struct{})
	var factories atomic.Int32
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		s := featureIMS()
		if factories.Add(1) == 1 {
			s.dial = func(ctx context.Context, _ string) (vowifi.Call, error) {
				close(entered)
				<-ctx.Done()
				return vowifi.Call{}, ctx.Err()
			}
		}
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, s, testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady })
	done := make(chan error, 1)
	go func() { _, err := manager.DialCall(context.Background(), "line", "synthetic"); done <- err }()
	waitSignal(t, entered)
	_, _ = manager.RequestReconnect("line")
	if err := waitResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("old generation I/O was not canceled", err)
	}
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady && s.Attempt == 2 })
}

func TestManagerCloseStopsLinesConcurrentlyAndRetainsTimedOutOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnCleanup(t, release)
	secondClosed := make(chan struct{})
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		a := &fakeAccess{}
		s := featureIMS()
		if id == "first" {
			s.close = func(context.Context, int32) error { close(entered); <-release; return nil }
		} else {
			a.close = func(context.Context, int32) error { close(secondClosed); return nil }
		}
		return managerRuntime(t, id, func(context.Context) (Access, error) { return a, nil }, s, testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("first", true)
	_, _ = manager.RequestEnabled("second", true)
	awaitManager(t, manager, "first", func(s vowifi.State) bool { return s.IMSReady })
	awaitManager(t, manager, "second", func(s vowifi.State) bool { return s.IMSReady })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Close(ctx) }()
	waitSignal(t, entered)
	waitSignal(t, secondClosed)
	if err := waitResult(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Close did not remain bounded", err)
	}
	e, _ := manager.entry("first")
	e.mu.Lock()
	retained := e.runtime != nil
	e.mu.Unlock()
	if !retained {
		t.Fatal("timed out owner was discarded")
	}
	close(release)
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal("retrying Close failed", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal("completed Close was not idempotent", err)
	}
	if _, err := manager.RequestEnabled("second", true); !errors.Is(err, ErrClosed) {
		t.Fatal("closed manager restarted", err)
	}
}

func TestManagerMaintenanceSuppressesFactoryAndIndependentCleanup(t *testing.T) {
	var factories atomic.Int32
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		factories.Add(1)
		return managerRuntime(t, id,
			func(context.Context) (Access, error) { return &fakeAccess{}, nil }, featureIMS(), testCard()), nil
	}))
	cleanupManager(t, manager)
	if err := manager.BeginMaintenance("line"); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.RequestEnabled("line", true)
	if factories.Load() != 0 {
		t.Fatal("maintenance allowed startup")
	}
	manager.EndMaintenance("line")
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.IMSReady })
	if err := manager.BeginMaintenance("line"); err != nil {
		t.Fatal(err)
	}
	if err := manager.StopDevice(context.Background(), "line"); err != nil {
		t.Fatal(err)
	}
	if manager.ModemSMSSyncBlocked("line") {
		t.Fatal("acknowledged maintenance stop retained command ownership")
	}
	manager.EndMaintenance("line")
}

func TestManagerLateEvidencePollCannotRepublishPreviousGeneration(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnCleanup(t, release)
	var block atomic.Bool
	var once atomic.Bool
	session := featureIMS()
	session.enable = func(context.Context) (vowifi.SMSEvidence, error) {
		if block.Load() && once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return vowifi.SMSEvidence{Ready: true}, nil
	}
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, session, testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.SMSReady })
	block.Store(true)
	waitSignal(t, entered)
	if err := manager.StopDevice(context.Background(), "line"); err != nil {
		t.Fatal(err)
	}
	close(release)
	state := awaitManager(t, manager, "line", func(s vowifi.State) bool { return !s.Active && !s.Enabled && s.Phase == vowifi.PhaseIdle })
	time.Sleep(10 * time.Millisecond)
	after, err := manager.State("line")
	if err != nil || after.IMSReady || after.SMSReady || after.Enabled || after.Sequence < state.Sequence {
		t.Fatal("late poll restored a previous ready subscription", err)
	}
}

func TestManagerCanceledVerifiedSMSCannotMoveToNewSubscription(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnCleanup(t, release)
	var block atomic.Bool
	var factories atomic.Int32
	firstSession, secondSession := featureIMS(), featureIMS()
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		generation := factories.Add(1)
		card := testCard()
		session := firstSession
		a := &fakeAccess{}
		if generation == 1 {
			a.verify = func(context.Context, vowifi.SIMIdentity) error {
				if block.Load() {
					close(entered)
					<-release
				}
				return nil
			}
		} else {
			card.identity.ICCID, card.identity.IMSI = "8901000000000000002", "310260123456781"
			session = secondSession
		}
		return managerRuntime(t, id, func(context.Context) (Access, error) { return a, nil }, session, card), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.SMSReady })
	block.Store(true)
	done := make(chan error, 1)
	id := testCard().identity
	go func() {
		_, err := manager.SendSMSForSubscription(context.Background(), "line", id.IMSI, id.ICCID, vowifi.SMSSubmitRequest{})
		done <- err
	}()
	waitSignal(t, entered)
	_, _ = manager.RequestReconnect("line")
	close(release)
	if err := waitResult(t, done); err == nil {
		t.Fatal("canceled old subscription SMS was dispatched")
	}
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.SMSReady && s.Attempt == 2 })
	if _, err := manager.SendSMSForSubscription(context.Background(), "line", id.IMSI, id.ICCID, vowifi.SMSSubmitRequest{}); !errors.Is(err, vowifi.ErrSMSNotReady) {
		t.Fatal("old subscription was redirected after reconnect", err)
	}
	if firstSession.sends.Load() != 0 || secondSession.sends.Load() != 0 {
		t.Fatal("SMS reached a stale or replacement session")
	}
}

func TestManagerRejectsForeignRuntimePointerWithoutStoppingItsOwner(t *testing.T) {
	access := &fakeAccess{}
	session := featureIMS()
	runtime := managerRuntime(t, "first", func(context.Context) (Access, error) { return access, nil }, session, testCard())
	manager := New(managerOptions(func(context.Context, string) (*Runtime, error) { return runtime, nil }))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("first", true)
	awaitManager(t, manager, "first", func(s vowifi.State) bool { return s.IMSReady })
	_, _ = manager.RequestEnabled("second", true)
	awaitManager(t, manager, "second", func(s vowifi.State) bool {
		return s.Phase == vowifi.PhaseFailed && s.LastErrorClass == "host_ims_prepare_failed"
	})
	if access.closes.Load() != 0 || session.closes.Load() != 0 || !session.registered.Load() {
		t.Fatal("bad factory redirected another line's cleanup")
	}
	if err := manager.StopDevice(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if access.closes.Load() != 0 {
		t.Fatal("failed line retained foreign cleanup authority")
	}
	if err := manager.StopDevice(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	owners := len(manager.owners)
	manager.mu.Unlock()
	if owners != 0 || access.closes.Load() != 1 || session.closes.Load() != 1 {
		t.Fatal("acknowledged owner reservation was not released")
	}
}

func TestManagerAttemptedMessageFailureIsNotAPreflightFallbackSignal(t *testing.T) {
	session := featureIMS()
	session.submitErr = errors.New("private network transaction detail")
	manager := New(managerOptions(func(_ context.Context, id string) (*Runtime, error) {
		return managerRuntime(t, id, func(context.Context) (Access, error) { return &fakeAccess{}, nil }, session, testCard()), nil
	}))
	cleanupManager(t, manager)
	_, _ = manager.RequestEnabled("line", true)
	awaitManager(t, manager, "line", func(s vowifi.State) bool { return s.SMSReady })
	if _, err := manager.SendSMS(context.Background(), "line", vowifi.SMSSubmitRequest{}); !errors.Is(err, ErrSMSSubmit) || errors.Is(err, vowifi.ErrSMSNotReady) {
		t.Fatal("attempted SMS was relabeled as safe preflight fallback", err)
	}
	if _, err := manager.SendUSSI(context.Background(), "line", vowifi.USSISubmitRequest{}); !errors.Is(err, ErrUSSISubmit) || errors.Is(err, vowifi.ErrUSSINotReady) {
		t.Fatal("attempted USSI was relabeled as safe preflight fallback", err)
	}
	if session.sends.Load() != 1 || session.ussis.Load() != 1 {
		t.Fatal("message attempt was repeated or bypassed")
	}
}
