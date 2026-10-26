package ims

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func dtmfMedia(t *testing.T) (*rtpMedia, *net.UDPConn) {
	t.Helper()
	m, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	body := fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 8 110\r\na=rtpmap:110 telephone-event/8000\r\na=fmtp:110 0-15\r\n", peer.LocalAddr().(*net.UDPAddr).Port)
	if err = m.configureRemote([]byte(body)); err != nil {
		t.Fatal(err)
	}
	return m, peer
}

func TestDTMFQueuedKeysExpireAndOwnershipFlushDoesNotReplay(t *testing.T) {
	m := &rtpMedia{dtmfDownlink: make(chan dtmfKey, 16), closed: make(chan struct{})}
	m.dtmfDownlink <- dtmfKey{digit: '1', received: time.Now().Add(-time.Second)}
	m.dtmfDownlink <- dtmfKey{digit: '2', received: time.Now()}
	if got, err := m.ReadDTMF(context.Background()); err != nil || got != '2' {
		t.Fatal("stale key replayed", got, err)
	}
	m.dtmfDownlink <- dtmfKey{digit: '3', received: time.Now()}
	m.FlushDTMF()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.ReadDTMF(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pre-ownership key retained", err)
	}
}

func TestDTMFNegotiatesOnlySelectedAudioAndSupportedEvents(t *testing.T) {
	m, _ := dtmfMedia(t)
	if got := m.DTMFDigits(); got != "0123456789*#ABCD" {
		t.Fatalf("digits = %q", got)
	}
	if got := string(m.answerSDP(net.IPv4(127, 0, 0, 1))); !strings.Contains(got, " 8 110\r\n") || !strings.Contains(got, "a=fmtp:110 0-15") {
		t.Fatal(got)
	}
	for _, tc := range []struct{ mapping, fmtp, digits string }{
		{"telephone-event/8000", "0-9,11", "0123456789#"},
		{"telephone-event/8000", "", "0123456789*#ABCD"},
		{"telephone-event/16000", "0-15", ""},
		{"telephone-event/8000/2", "0-15", ""},
		{"telephone-event/8000", "0-99999", ""},
		{"telephone-event/8000", "15-0", ""},
		{"telephone-event/8000", "0-15;injected=1", ""},
	} {
		t.Run(tc.mapping+"/"+tc.fmtp, func(t *testing.T) {
			body := "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 19000 RTP/AVP 8 110\r\na=rtpmap:110 " + tc.mapping + "\r\n"
			if tc.fmtp != "" {
				body += "a=fmtp:110 " + tc.fmtp + "\r\n"
			}
			body += "m=video 19002 RTP/AVP 101\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-15\r\n"
			if err := m.configureRemote([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if got := m.DTMFDigits(); got != tc.digits {
				t.Fatalf("got %q want %q", got, tc.digits)
			}
		})
	}
}

func TestDTMFWireUsesOneTimestampSharedSequenceAndThreeFinalReports(t *testing.T) {
	m, peer := dtmfMedia(t)
	before := m.sequence
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.SendDTMF(ctx, '#'); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var timestamp uint32
	var lastDuration uint16
	ends := 0
	for i := 0; i < 10; i++ {
		b := make([]byte, 64)
		n, _, err := peer.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		if n != 16 || b[1]&127 != 110 || b[12] != 11 || b[13]&0x40 != 0 || b[13]&63 != 10 {
			t.Fatalf("invalid event %x", b[:n])
		}
		if binary.BigEndian.Uint16(b[2:4]) != before+uint16(i) {
			t.Fatalf("sequence %x", b[:n])
		}
		if i == 0 {
			timestamp = binary.BigEndian.Uint32(b[4:8])
		}
		if binary.BigEndian.Uint32(b[4:8]) != timestamp || (b[1]&128 != 0) != (i == 0) {
			t.Fatalf("timestamp/marker %x", b[:n])
		}
		d := binary.BigEndian.Uint16(b[14:16])
		if d < lastDuration {
			t.Fatal("duration decreased")
		}
		lastDuration = d
		if b[13]&128 != 0 {
			ends++
		}
	}
	if ends != 3 || lastDuration != 1280 {
		t.Fatalf("ends=%d duration=%d", ends, lastDuration)
	}
	if err := m.WritePCM(make([]int16, 160)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 256)
	n, _, err := peer.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 172 || binary.BigEndian.Uint16(b[2:4]) != before+10 || binary.BigEndian.Uint32(b[4:8])-timestamp < 1280 {
		t.Fatalf("audio timeline %x", b[:n])
	}
}

func TestDTMFRejectsInvalidUnnegotiatedCanceledAndConcurrentKeys(t *testing.T) {
	m, peer := dtmfMedia(t)
	for _, digit := range []byte{'x', '\n', 0, 255} {
		if err := m.SendDTMF(context.Background(), digit); err == nil {
			t.Fatalf("accepted %q", digit)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.SendDTMF(ctx, '1'); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.SendDTMF(ctx, '2') }()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 64)
	if _, _, err := peer.ReadFromUDP(b); err != nil {
		t.Fatal(err)
	}
	if err := m.SendDTMF(context.Background(), '3'); !errors.Is(err, ErrDTMFBusy) {
		t.Fatalf("concurrent result %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.configureRemote([]byte(callTestSDP)); err != nil {
		t.Fatal(err)
	}
	if err := m.SendDTMF(context.Background(), '1'); !errors.Is(err, ErrDTMFUnsupported) {
		t.Fatal(err)
	}
	_ = m.Close()
	if err := m.SendDTMF(context.Background(), '1'); err == nil {
		t.Fatal("sent after close")
	}
}

func TestDTMFReceiveRequiresNegotiatedEndpointAndDeduplicatesEndReports(t *testing.T) {
	m, peer := dtmfMedia(t)
	packet := make([]byte, 16)
	packet[0] = 0x80
	packet[1] = 110
	packet[12] = 5
	packet[13] = 0x80 | 10
	binary.BigEndian.PutUint32(packet[4:8], 123)
	binary.BigEndian.PutUint32(packet[8:12], 99)
	binary.BigEndian.PutUint16(packet[14:16], 1280)
	for i := 0; i < 3; i++ {
		binary.BigEndian.PutUint16(packet[2:4], uint16(i))
		if _, err := peer.WriteToUDP(packet, m.conn.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	digit, err := m.ReadDTMF(ctx)
	if err != nil || digit != '5' {
		t.Fatalf("digit=%q err=%v", digit, err)
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, err = m.ReadDTMF(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplicate event: %v", err)
	}
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	binary.BigEndian.PutUint32(packet[4:8], 456)
	_, _ = other.WriteToUDP(packet, m.conn.LocalAddr().(*net.UDPAddr))
	short, stop = context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, err = m.ReadDTMF(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrong endpoint event: %v", err)
	}
}
