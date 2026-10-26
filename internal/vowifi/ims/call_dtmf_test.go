package ims

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestDTMFCallRequiresExactActiveDialogAndStopsOnCallEnd(t *testing.T) {
	s, _ := newCallTestSession(t)
	m, peer := dtmfMedia(t)
	s.calls["this-call"] = &imsCall{callID: "this-call", media: m, public: vowifi.Call{ID: "this-call", State: "ringing", StartedAt: time.Now()}}
	if err := s.SendDTMF(context.Background(), "other-call", '1'); !errors.Is(err, ErrCallNotFound) {
		t.Fatal(err)
	}
	if err := s.SendDTMF(context.Background(), "this-call", '1'); !errors.Is(err, ErrCallState) {
		t.Fatal(err)
	}
	s.calls["this-call"].public.State = "active"
	if calls := s.Calls(); len(calls) != 1 || calls[0].DTMFDigits != "0123456789*#ABCD" {
		t.Fatalf("snapshot=%v", calls)
	}
	done := make(chan error, 1)
	go func() { done <- s.SendDTMF(context.Background(), "this-call", '1') }()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peer.ReadFromUDP(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	s.finishCall("this-call", "ended", 0, "")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ended call event succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("event did not stop")
	}
	if calls := s.Calls(); calls[0].DTMFDigits != "" {
		t.Fatal("terminal call still claims keypad capability")
	}
	// A later dialog on the same device must never inherit a stale call ID.
	m2, _ := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), nil)
	defer m2.Close()
	s.calls["next-call"] = &imsCall{callID: "next-call", media: m2, public: vowifi.Call{ID: "next-call", State: "active"}}
	if err := s.SendDTMF(context.Background(), "this-call", '2'); !errors.Is(err, ErrCallState) {
		t.Fatal(err)
	}
}
