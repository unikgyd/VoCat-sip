package sipgateway

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCallSDPNegotiationAndMediaOrder(t *testing.T) {
	base := "v=0\r\no=test 1 1 IN IP4 192.0.2.10\r\ns=test\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\n"
	for _, item := range []struct {
		name, tail, codec string
		pt                byte
		bad               bool
	}{
		{"static-pcma", "m=audio 4000 RTP/AVP 8 0\r\n", "PCMA", 8, false},
		{"static-pcmu", "m=audio 4000 RTP/AVP 0 8\r\n", "PCMU", 0, false},
		{"dynamic", "m=audio 4000 RTP/AVP 101\r\na=rtpmap:101 PCMA/8000/1\r\n", "PCMA", 101, false},
		{"later-audio-does-not-override", "m=audio 4000 RTP/AVP 8\r\nm=audio 4002 RTP/AVP 0\r\n", "PCMA", 8, false},
		{"bad-static-map", "m=audio 4000 RTP/AVP 8\r\na=rtpmap:8 PCMU/8000\r\n", "", 0, true},
		{"bad-clock", "m=audio 4000 RTP/AVP 8\r\na=rtpmap:8 PCMA/16000\r\n", "", 0, true},
		{"bad-channels", "m=audio 4000 RTP/AVP 101\r\na=rtpmap:101 PCMA/8000/2\r\n", "", 0, true},
		{"reserved-pt", "m=audio 4000 RTP/AVP 9\r\na=rtpmap:9 PCMA/8000\r\n", "", 0, true},
		{"duplicate-pt", "m=audio 4000 RTP/AVP 8 8\r\n", "", 0, true},
		{"duplicate-map", "m=audio 4000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:8 PCMA/8000\r\n", "", 0, true},
		{"hold", "m=audio 4000 RTP/AVP 8\r\na=sendonly\r\n", "", 0, true},
		{"disabled", "m=audio 0 RTP/AVP 8\r\n", "", 0, true},
		{"srtp", "m=audio 4000 RTP/SAVP 8\r\n", "", 0, true},
		{"no-g711", "m=audio 4000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\n", "", 0, true},
		{"invalid-first-audio", "m=audio 4000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\nm=audio 4002 RTP/AVP 8\r\n", "", 0, true},
		{"bad-ip", "m=audio 4000 RTP/AVP 8\r\nc=IN IP4 invalid.example\r\n", "", 0, true},
		{"bad-ip-family", "m=audio 4000 RTP/AVP 8\r\nc=IN IP6 192.0.2.1\r\n", "", 0, true},
		{"multicast", "m=audio 4000 RTP/AVP 8\r\nc=IN IP4 239.1.1.1\r\n", "", 0, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			got, err := parseCallSDP([]byte(base + item.tail))
			if item.bad {
				if err == nil {
					t.Fatal("invalid SDP accepted")
				}
				return
			}
			if err != nil || got.format.codec != item.codec || got.format.pt != item.pt {
				t.Fatalf("negotiation %+v %v", got, err)
			}
		})
	}
	offer := base + "m=video 9000 RTP/AVP 100\r\na=rtpmap:100 H264/90000\r\nm=audio 4000 RTP/AVP 101\r\na=rtpmap:101 PCMA/8000\r\nm=audio 4002 RTP/AVP 0\r\n"
	desc, err := parseCallSDP([]byte(offer))
	if err != nil {
		t.Fatal(err)
	}
	answer := string(makeCallAnswer(desc, &net.UDPAddr{IP: net.ParseIP("::1"), Port: 5000}))
	if !strings.Contains(answer, "m=video 0 RTP/AVP 100\r\nm=audio 5000 RTP/AVP 101") || !strings.Contains(answer, "m=audio 0 RTP/AVP 0") || !strings.Contains(answer, "c=IN IP6 ::1") {
		t.Fatalf("answer changed m-line order or family: %s", answer)
	}
	for _, bad := range []string{"s=test\r\n" + offer, offer + "v=0\r\n", strings.Repeat("x", 65537)} {
		if _, err := parseCallSDP([]byte(bad)); err == nil {
			t.Fatal("malformed/oversized SDP accepted")
		}
	}
}
func TestCallRTPParserBounds(t *testing.T) {
	good := append([]byte{0x80, 8, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}, bytes.Repeat([]byte{0xd5}, 160)...)
	for _, item := range []struct {
		name   string
		change func([]byte) []byte
		valid  bool
	}{
		{"normal", func(p []byte) []byte { return p }, true},
		{"short", func(p []byte) []byte { return p[:12] }, false},
		{"version", func(p []byte) []byte { p[0] = 0x40; return p }, false},
		{"wrong-pt", func(p []byte) []byte { p[1] = 0; return p }, false},
		{"missing-csrc", func(p []byte) []byte { p[0] = 0x8f; return p[:13] }, false},
		{"missing-extension", func(p []byte) []byte { p[0] = 0x90; return p[:14] }, false},
		{"oversized-extension", func(p []byte) []byte { p[0] = 0x90; p[14] = 255; p[15] = 255; return p }, false},
		{"zero-padding", func(p []byte) []byte { p[0] = 0xa0; p[len(p)-1] = 0; return p }, false},
		{"excess-padding", func(p []byte) []byte { p[0] = 0xa0; p[len(p)-1] = 255; return p }, false},
		{"valid-padding", func(p []byte) []byte { p[0] = 0xa0; return append(p, 0, 0, 0, 4) }, true},
		{"too-large", func(p []byte) []byte { return append(p, make([]byte, 2049)...) }, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			p := item.change(append([]byte(nil), good...))
			_, _, ok := rtpPayload(p, 8)
			if ok != item.valid {
				t.Fatalf("valid=%t", ok)
			}
		})
	}
}
func TestCallMediaInvalidRTPDoesNotLatchAndCloseReleasesPort(t *testing.T) {
	a := testMediaSocket(t)
	b := testMediaSocket(t)
	invalid := testMediaSocket(t)
	left, err := newCallMedia(a.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer left.close()
	right, err := newCallMedia(b.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer right.close()
	description := audioDescription{format: audioFormat{"PCMA", 8}, port: a.LocalAddr().(*net.UDPAddr).Port}
	left.configure(a.LocalAddr().String(), description)
	description.port = b.LocalAddr().(*net.UDPAddr).Port
	right.configure(b.LocalAddr().String(), description)
	left.destination.Store(right)
	right.destination.Store(left)
	invalid.WriteToUDP([]byte{0x80, 8, 1}, left.address())
	time.Sleep(10 * time.Millisecond)
	packet := append([]byte{0x80, 8, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}, bytes.Repeat([]byte{0xd5}, 160)...)
	a.WriteToUDP(packet, left.address())
	b.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := b.ReadFromUDP(buffer)
	if err != nil || !bytes.Equal(buffer[:n], packet) {
		t.Fatalf("invalid packet captured RTP latch: %v", err)
	}
	left.mu.Lock()
	port := left.remote.Port
	left.mu.Unlock()
	if port != a.LocalAddr().(*net.UDPAddr).Port {
		t.Fatal("RTP source latch mismatch")
	}
	address := left.address()
	left.close()
	bound, err := net.ListenUDP("udp", address)
	if err != nil {
		t.Fatal("port leaked", err)
	}
	bound.Close()
}
func TestCallMediaIPv6Loopback(t *testing.T) {
	peer, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1")})
	if err != nil {
		t.Skip("IPv6 loopback unavailable")
	}
	defer peer.Close()
	media, err := newCallMedia(peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer media.close()
	if media.address().IP.To4() != nil {
		t.Fatal("IPv6 path downgraded")
	}
	desc, err := parseCallSDP(makeCallOffer(media.address()))
	if err != nil || desc.port != media.address().Port {
		t.Fatalf("IPv6 offer %v", err)
	}
}
