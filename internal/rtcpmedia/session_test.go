package rtcpmedia

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestEndpointSeparatePortsAndAddressFamilies(t *testing.T) {
	for _, tc := range []struct {
		attribute, ip, want string
		port                int
	}{
		{"", "192.0.2.1", "192.0.2.1:4001", 4000},
		{"6000", "192.0.2.1", "192.0.2.1:6000", 4000},
		{"6000 IN IP4 192.0.2.2", "192.0.2.1", "192.0.2.2:6000", 4000},
		{"6000 IN IP6 2001:db8::2", "2001:db8::1", "[2001:db8::2]:6000", 4000},
		{"", "192.0.2.1", "", 65535},
		{"4000", "192.0.2.1", "", 4000},
		{"6000 IN IP6 ::1", "192.0.2.1", "", 4000},
		{"6000 IN IP4 224.0.0.1", "192.0.2.1", "", 4000},
		{"6000 IN IP4 0.0.0.0", "192.0.2.1", "", 4000},
		{"6000 IN IP4 255.255.255.255", "192.0.2.1", "", 4000},
		{"6000 extra", "192.0.2.1", "", 4000},
	} {
		t.Run(tc.attribute+tc.ip+tc.want, func(t *testing.T) {
			got, err := Endpoint(tc.attribute, net.ParseIP(tc.ip), tc.port)
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid control endpoint accepted", got)
				}
				return
			}
			if err != nil || got.String() != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}

func testRTP(ssrc uint32, seq uint16) []byte {
	p := make([]byte, 12)
	p[0] = 0x80
	binary.BigEndian.PutUint16(p[2:], seq)
	binary.BigEndian.PutUint32(p[4:], uint32(seq)*160)
	binary.BigEndian.PutUint32(p[8:], ssrc)
	return p
}

func testSession() *Session {
	return &Session{clock: 8000, origin: time.Now(), cname: "synthetic@vocat", localSSRC: 7,
		remote: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5001}, revision: 1, sentSR: make(map[uint32]time.Time)}
}

func TestReceptionWrapLossDuplicatesAndRestart(t *testing.T) {
	s := testSession()
	for _, seq := range []uint16{65534, 65535, 1} {
		s.ReceivedRTP(1, testRTP(42, seq), 160)
	}
	if got := s.Statistics(); got.PacketsLost != 1 || got.PacketsReceived != 3 {
		t.Fatal(got)
	}
	s.ReceivedRTP(1, testRTP(42, 0), 160)
	if s.Statistics().PacketsLost != 0 {
		t.Fatal("late packet did not repair loss")
	}
	s.ReceivedRTP(1, testRTP(42, 0), 160)
	if s.Statistics().PacketsLost != -1 {
		t.Fatal("duplicate accounting")
	}
	s.ReceivedRTP(2, testRTP(42, 2), 160)
	s.ReceivedRTP(1, testRTP(99, 10), 160)
	if s.Statistics().RemoteSSRC != 42 || s.Statistics().PacketsReceived != 5 {
		t.Fatal("stale revision/stray SSRC replaced source")
	}
	s.ReceivedRTP(1, testRTP(99, 11), 160)
	if s.Statistics().RemoteSSRC != 99 {
		t.Fatal("confirmed new source rejected")
	}
	s.ReceivedRTP(1, testRTP(99, 10000), 160)
	if s.rx.highest != 11 {
		t.Fatal("single dropout restarted reception")
	}
	s.ReceivedRTP(1, testRTP(99, 10001), 160)
	if s.rx.highest != 10001 || s.rx.received != 1 || s.rx.epoch != 2 {
		t.Fatal("confirmed restart did not reset interval")
	}
}

func TestCompoundReportsAreAtomicAndCorrelateRTT(t *testing.T) {
	s := testSession()
	s.ReceivedRTP(1, testRTP(42, 1), 160)
	s.SentRTP(testRTP(7, 1), 160)
	now := time.Now()
	out := s.reportLocked(now, true)
	packets, ok := parseControl(out.packet)
	if !ok || len(packets) != 3 || packets[0].kind != 200 || packets[1].kind != 202 || packets[2].kind != 203 {
		t.Fatal("invalid SR/SDES/BYE compound")
	}
	if binary.BigEndian.Uint32(packets[0].body[20:]) != 1 || binary.BigEndian.Uint32(packets[0].body[24:]) != 160 {
		t.Fatal("incorrect sender counts")
	}
	rr := make([]byte, 32)
	rr[0], rr[1], rr[3] = 0x81, 201, 7
	binary.BigEndian.PutUint32(rr[4:], 42)
	binary.BigEndian.PutUint32(rr[8:], 7)
	binary.BigEndian.PutUint32(rr[24:], 123)
	binary.BigEndian.PutUint32(rr[28:], 32768)
	s.sentSR[123] = now.Add(-time.Second)
	s.processLocked(append(append([]byte(nil), rr...), 0x80, 200, 0, 30), now)
	if s.stats.InvalidReports != 1 || s.stats.ReportsReceived != 0 {
		t.Fatal("malformed trailing packet partially committed")
	}
	s.processLocked(rr, now)
	if s.stats.ReportsReceived != 1 || s.stats.RoundTripMillis != 500 {
		t.Fatal(s.stats)
	}
	binary.BigEndian.PutUint32(rr[8:], 8)
	s.processLocked(rr, now.Add(time.Second))
	if s.stats.RoundTripMillis != 500 {
		t.Fatal("foreign SSRC updated RTT")
	}
	if got := clockTicks(72*time.Hour+time.Second/2, 8000); got != 2073604000 {
		t.Fatal("long call RTP clock overflow", got)
	}
}

func TestSeparateSocketPinsPeerAndSendsFinalBYE(t *testing.T) {
	listen := func() *net.UDPConn {
		c, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	local, peer, stranger := listen(), listen(), listen()
	s, err := New(local, 7, 8000, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Abort(); <-s.Done() })
	s.SetRemote(peer.LocalAddr().(*net.UDPAddr), 1)
	s.ReceivedRTP(1, testRTP(42, 1), 160)
	rr := []byte{0x80, 201, 0, 1, 0, 0, 0, 42}
	_, _ = stranger.WriteToUDP(rr, local.LocalAddr().(*net.UDPAddr))
	_, _ = peer.WriteToUDP(rr, local.LocalAddr().(*net.UDPAddr))
	deadline := time.After(2 * time.Second)
	for s.Statistics().ReportsReceived == 0 {
		select {
		case <-deadline:
			t.Fatal("control socket did not receive peer report")
		case <-time.After(time.Millisecond):
		}
	}
	s.Close()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("close did not join socket loops")
	}
	if s.Statistics().ReportsReceived != 1 {
		t.Fatal("accepted report from another source port")
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := peer.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	packets, ok := parseControl(buffer[:n])
	if !ok || packets[len(packets)-1].kind != 203 {
		t.Fatal("missing compound final BYE")
	}
}
