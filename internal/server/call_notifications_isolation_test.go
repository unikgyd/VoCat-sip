package server

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/store"
)

func TestIncomingCallNotificationsUseDialogAndServerIdentity(t *testing.T) {
	s, _ := newPhoneTest(t)
	if err := s.store.UpsertNotificationSetting(context.Background(), store.NotificationSetting{Channel: "webhook", Enabled: true, Config: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	send := func(context.Context, string, map[string]any, IncomingCallNotification) error {
		count.Add(1)
		return nil
	}
	n := IncomingCallNotification{DeviceID: "module-a", Caller: "+15550000001", Environment: "host_ims", CallID: "dialog-1"}
	s.notifyIncomingCall(context.Background(), n, send)
	s.notifyIncomingCall(context.Background(), n, send)
	if count.Load() != 1 {
		t.Fatalf("retransmission sent %d notices", count.Load())
	}
	n.CallID = "dialog-2"
	s.notifyIncomingCall(context.Background(), n, send)
	n.DeviceID = "module-b"
	s.notifyIncomingCall(context.Background(), n, send)
	n.Environment = "vowifi"
	s.notifyIncomingCall(context.Background(), n, send)
	if count.Load() != 4 {
		t.Fatalf("distinct calls were suppressed: %d", count.Load())
	}
	other := &Server{store: s.store, logger: s.logger}
	other.notifyIncomingCall(context.Background(), n, send)
	if count.Load() != 5 {
		t.Fatal("different server instances share notification suppression")
	}
	if got := n.Text(); got == "" {
		t.Fatal("empty notice")
	}
	n.Environment = "host_ims"
	if got := n.Text(); !strings.Contains(got, "蜂窝 Host IMS") {
		t.Fatalf("Host IMS mislabeled: %s", got)
	}
}

func TestIncomingCallNotificationSlowChannelDoesNotDelayOtherChannels(t *testing.T) {
	s, _ := newPhoneTest(t)
	for _, channel := range []string{"telegram", "webhook"} {
		if err := s.store.UpsertNotificationSetting(context.Background(), store.NotificationSetting{Channel: channel, Enabled: true, Config: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	started, fast, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		s.notifyIncomingCall(ctx, IncomingCallNotification{DeviceID: "module-a", CallID: "parallel", Environment: "host_ims"}, func(ctx context.Context, channel string, _ map[string]any, _ IncomingCallNotification) error {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
				t.Error("channel has no bounded deadline")
			}
			if channel == "telegram" {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			}
			close(fast)
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow channel did not start")
	}
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("slow channel blocked another channel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notification did not stop on cancellation")
	}
}

func TestIncomingCallNotificationHistoryHasBoundedMemory(t *testing.T) {
	var h callNotificationHistory
	now := time.Now()
	for i := 0; i < 4100; i++ {
		h.shouldSuppressDuplicateCall(callNotificationKey{device: "a", dialog: fmt.Sprint(i)}, now.Add(time.Duration(i)*time.Nanosecond), time.Minute)
	}
	if len(h.seen) > 4096 {
		t.Fatalf("unbounded history: %d", len(h.seen))
	}
	if !h.shouldSuppressDuplicateCall(callNotificationKey{device: "a", dialog: "4099"}, now.Add(time.Second), time.Minute) {
		t.Fatal("recent dialog was forgotten")
	}
}
