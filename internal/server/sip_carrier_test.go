package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type sipCarrierController struct {
	fakeVoWiFiController
	fakeCallController
	dialed, hungup, answered int
}

func (c *sipCarrierController) DialCall(context.Context, string, string) (vowifi.Call, error) {
	c.dialed++
	return vowifi.Call{ID: "synthetic-call"}, nil
}
func (c *sipCarrierController) HangupCall(context.Context, string, string) error {
	c.hungup++
	return nil
}
func (c *sipCarrierController) AnswerCall(_ context.Context, _ string, id string) (vowifi.Call, error) {
	c.answered++
	return vowifi.Call{ID: id, Direction: "incoming", State: "active", MediaReady: true}, nil
}

func TestSIPCarrierIncomingExactCallGuardAndCleanup(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertDevice(context.Background(), store.Device{ID: "module-a", Name: "Synthetic incoming modem"}); err != nil {
		t.Fatal(err)
	}
	controller := &sipCarrierController{fakeVoWiFiController: fakeVoWiFiController{state: vowifi.State{Enabled: true, SIMReady: true, IMSReady: true, IMSI: "001010000000001", ICCID: "synthetic-sim"}}}
	controller.calls = []vowifi.Call{{ID: "known", Direction: "incoming", State: "ringing"}}
	server := &Server{store: db, vowifi: controller, devices: fakeDeviceController{entry: device.Device{ID: "module-a", Discovered: true, Snapshot: &device.Snapshot{ICCID: "synthetic-sim"}}}}
	backend := sipCarrierBackend{server}
	if _, err := backend.Incoming(context.Background(), "module-a", "wrong"); !errors.Is(err, sipgateway.ErrCarrierBusy) {
		t.Fatal("wrong call accepted", err)
	}
	if call, err := backend.Answer(context.Background(), "module-a", "known"); err != nil || call.ID != "known" || controller.answered != 1 {
		t.Fatal(call, err)
	}
	for _, state := range []string{"active", "ended"} {
		controller.calls[0].State = state
		if _, err := backend.Answer(context.Background(), "module-a", "known"); !errors.Is(err, sipgateway.ErrCarrierAnswerNotAttempted) {
			t.Fatal("nonringing call attempted", err)
		}
	}
	controller.calls[0].State = "ringing"
	controller.state.SIMReady = false
	if _, err := backend.Answer(context.Background(), "module-a", "known"); !errors.Is(err, sipgateway.ErrCarrierAnswerNotAttempted) || !errors.Is(err, sipgateway.ErrCarrierUnavailable) {
		t.Fatal("invalid SIM attempted answer", err)
	}
	if controller.answered != 1 || controller.dialed != 0 {
		t.Fatal("guard bypass or redial")
	}
	if err := backend.Hangup(context.Background(), "module-a", "known"); err != nil || controller.hungup != 1 {
		t.Fatal("cleanup blocked", err)
	}
}
func (*sipCarrierController) CallMedia(context.Context, string, string) (vowifi.CallMedia, error) {
	return nil, nil
}

func TestSIPCarrierAdapterRequiresVerifiedRegisteredMatchingSIM(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*vowifi.State, *store.Device, *device.Device)
		want   error
	}{
		{"ready", func(*vowifi.State, *store.Device, *device.Device) {}, nil},
		{"not registered", func(s *vowifi.State, _ *store.Device, _ *device.Device) { s.IMSReady = false }, sipgateway.ErrCarrierUnavailable},
		{"not verified", func(s *vowifi.State, _ *store.Device, _ *device.Device) { s.SIMReady = false }, sipgateway.ErrCarrierUnavailable},
		{"missing IMSI", func(s *vowifi.State, _ *store.Device, _ *device.Device) { s.IMSI = "" }, sipgateway.ErrCarrierUnavailable},
		{"SIM changed", func(_ *vowifi.State, _ *store.Device, p *device.Device) { p.Snapshot.ICCID = "other-sim" }, sipgateway.ErrCarrierUnavailable},
		{"placeholder identity", func(s *vowifi.State, _ *store.Device, _ *device.Device) { s.IMSI = "460000000000000" }, sipgateway.ErrCarrierUnavailable},
		{"blocked runtime identity", func(s *vowifi.State, _ *store.Device, _ *device.Device) { s.IMSI = "460001234567890" }, sipgateway.ErrCarrierForbidden},
		{"blocked cached identity", func(_ *vowifi.State, _ *store.Device, p *device.Device) { p.Snapshot.IMSI = "460001234567890" }, sipgateway.ErrCarrierForbidden},
		{"missing hardware", func(_ *vowifi.State, _ *store.Device, p *device.Device) { p.Discovered = false }, sipgateway.ErrCarrierUnavailable},
		{"conflicting data policy", func(_ *vowifi.State, c *store.Device, _ *device.Device) { c.NetworkEnabled = true }, sipgateway.ErrCarrierForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(context.Background(), ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			state := vowifi.State{Enabled: true, SIMReady: true, IMSReady: true, IMSI: "001010000000001", ICCID: "synthetic-sim"}
			config := store.Device{ID: "module-a", Name: "Synthetic module A", VoWiFiEnabled: true}
			physical := device.Device{ID: "module-a", Discovered: true, Snapshot: &device.Snapshot{ICCID: "synthetic-sim", IMSI: "001010000000001"}}
			tc.mutate(&state, &config, &physical)
			if err := db.UpsertDevice(context.Background(), config); err != nil {
				t.Fatal(err)
			}
			controller := &sipCarrierController{fakeVoWiFiController: fakeVoWiFiController{state: state}}
			server := &Server{store: db, vowifi: controller, devices: fakeDeviceController{entry: physical}}
			backend := sipCarrierBackend{server}
			_, got := backend.Dial(context.Background(), "module-a", "+15550000001")
			if !errors.Is(got, tc.want) || tc.want != nil && controller.dialed != 0 || tc.want == nil && controller.dialed != 1 {
				t.Fatalf("got=%v calls=%d want=%v", got, controller.dialed, tc.want)
			}
			if controller.reconnects != 0 || len(controller.enabled) != 0 {
				t.Fatal("SIP dial changed registration/mode")
			}
		})
	}
}

func TestSIPCarrierAdapterDoesNotFallBackToATOrBlockCleanup(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertDevice(context.Background(), store.Device{ID: "module-a", Name: "Synthetic module A"}); err != nil {
		t.Fatal(err)
	}
	controller := &sipCarrierController{}
	server := &Server{store: db, vowifi: controller, devices: fakeDeviceController{entry: device.Device{ID: "module-a", Discovered: true}, atHandler: func(string) (modem.Response, error) {
		t.Fatal("SIP must not fall back to AT")
		return modem.Response{}, errors.New("AT must not be used")
	}}}
	backend := sipCarrierBackend{server}
	if _, err := backend.Dial(context.Background(), "module-a", "+15550000001"); !errors.Is(err, sipgateway.ErrCarrierUnavailable) {
		t.Fatal(err)
	}
	if err := backend.Hangup(context.Background(), "module-a", "known-call"); err != nil || controller.hungup != 1 {
		t.Fatal("cleanup required active registration", err)
	}
}

func TestSIPCarrierAdapterKeepsPendingCancellationBusy(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertDevice(context.Background(), store.Device{ID: "module-a", Name: "Synthetic module A"}); err != nil {
		t.Fatal(err)
	}
	controller := &sipCarrierController{fakeVoWiFiController: fakeVoWiFiController{state: vowifi.State{Enabled: true, SIMReady: true, IMSReady: true, IMSI: "001010000000001", ICCID: "synthetic-sim"}}}
	server := &Server{store: db, vowifi: controller, devices: fakeDeviceController{entry: device.Device{ID: "module-a", Discovered: true, Snapshot: &device.Snapshot{ICCID: "synthetic-sim"}}}}
	backend := sipCarrierBackend{server}
	now := time.Now()
	controller.calls = []vowifi.Call{{ID: "pending", State: "ended", EndedAt: &now, SignalingPending: true}}
	if err := backend.Ready(context.Background(), "module-a"); !errors.Is(err, sipgateway.ErrCarrierBusy) {
		t.Fatal("pending cancellation reused", err)
	}
	controller.calls[0].SignalingPending = false
	if err := backend.Ready(context.Background(), "module-a"); err != nil {
		t.Fatal("completed cancellation did not release line", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Dial(ctx, "module-a", "+15550000001"); !errors.Is(err, context.Canceled) || controller.dialed != 0 {
		t.Fatal("cancelled adapter dial was submitted", err)
	}
}
