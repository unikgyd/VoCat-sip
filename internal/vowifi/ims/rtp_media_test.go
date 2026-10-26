package ims

import (
	"context"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRTPMediaCarriesPCMOverPCMA(t *testing.T) {
	left, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	if err := left.configureRemote(right.offerSDP(net.IPv4(127, 0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	if err := right.configureRemote(left.answerSDP(net.IPv4(127, 0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	want := make([]int16, rtpPacketSamples)
	for index := range want {
		want[index] = int16(9000 * math.Sin(float64(index)*2*math.Pi/40))
	}
	if err := left.WritePCM(want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := right.ReadPCM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("received %d samples, want %d", len(got), len(want))
	}
	for index := range got {
		if difference := math.Abs(float64(got[index]) - float64(want[index])); difference > 700 {
			t.Fatalf("sample %d difference %.0f exceeds G.711 tolerance", index, difference)
		}
	}
}

func TestParseAudioSDPRejectsMissingEndpoint(t *testing.T) {
	if _, _, _, _, err := parseAudioSDP([]byte("v=0\r\nm=audio 0 RTP/AVP 8\r\n")); err == nil {
		t.Fatal("expected unusable SDP error")
	}
}

func TestPCMOfferAdvertisesOnlyImplementedCodecs(t *testing.T) {
	media, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	offer := string(media.offerSDP(net.IPv4(127, 0, 0, 1)))
	if strings.Contains(offer, "AMR") || !strings.Contains(offer, "a=rtpmap:101 telephone-event/8000") || !strings.Contains(offer, " RTP/AVP 8 0 101\r\n") {
		t.Fatalf("offer claims unimplemented codec or handler: %s", offer)
	}
}

func TestPCMNegotiationRejectsUnsupportedClocksChannelsAndStaticRemapping(t *testing.T) {
	for _, tc := range []struct {
		payload int
		mapping string
		codec   string
	}{
		{0, "", "PCMU"}, {8, "", "PCMA"}, {8, "PCMA/8000", "PCMA"},
		{96, "pcmu/8000/1", "PCMU"}, {97, "PCMA/8000", "PCMA"},
		{96, "PCMU/08000/01", "PCMU"},
		{104, "AMR-WB/16000", ""}, {102, "AMR/8000", ""}, {101, "", ""},
		{8, "PCMU/8000", ""}, {0, "PCMA/8000", ""}, {3, "PCMA/8000", ""},
		{97, "PCMA/16000", ""}, {97, "PCMA/8000/2", ""}, {97, "PCMA/8000/1/1", ""},
		{100, "telephone-event/8000", ""}, {128, "PCMA/8000", ""},
	} {
		codec, ok := negotiatePCMCodec(tc.payload, tc.mapping)
		if codec != tc.codec || ok != (tc.codec != "") {
			t.Errorf("payload %d mapping %q: got %q/%v, want %q", tc.payload, tc.mapping, codec, ok, tc.codec)
		}
	}
}

func TestPCMRemoteRejectsMalformedOrConflictingMappings(t *testing.T) {
	media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	base := "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 45000 RTP/AVP 8\r\n"
	for _, mappings := range []string{
		"a=rtpmap:8\r\n",
		"a=rtpmap:bad PCMA/8000\r\n",
		"a=rtpmap:8 PCMA/8000\r\na=rtpmap:8 PCMU/8000\r\n",
	} {
		if err := media.configureRemote([]byte(base + mappings)); err == nil || media.ready() {
			t.Fatalf("malformed mapping reached media-ready: %q", mappings)
		}
	}
}

func TestPCMRemoteCodecSelectionSkipsAMRAndRejectsAMROnly(t *testing.T) {
	media, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	base := "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 45000 RTP/AVP "
	if err := media.configureRemote([]byte(base + "104\r\na=rtpmap:104 AMR-WB/16000\r\n")); err == nil || media.ready() {
		t.Fatal("AMR-only peer must not become media-ready without an AMR backend")
	}
	if err := media.configureRemote([]byte(base + "104 0\r\na=rtpmap:104 AMR-WB/16000\r\n")); err != nil {
		t.Fatal(err)
	}
	if media.Codec() != "PCMU" {
		t.Fatal("a later supported codec must take precedence over an unsupported first format")
	}
	media.mu.Lock()
	media.codec = "AMR"
	media.mu.Unlock()
	if err := media.WritePCM(make([]int16, rtpPacketSamples)); err == nil {
		t.Fatal("unsupported codec must never fall through to mu-law encoding")
	}
}
