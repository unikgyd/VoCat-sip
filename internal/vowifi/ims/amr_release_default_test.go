//go:build linux && (amd64 || arm64 || 386 || arm) && !vocat_static

package ims

import (
	"math"
	"net"
	"os"
	"strings"
	"testing"
)

// This test uses the default build, not the old development tag or a fake
// codec. Release verification sets REQUIRE so a missing codec is a failure.
func TestAMRReleaseDefaultHasRealCodec(t *testing.T) {
	codec, err := openNativeAMR()
	if err != nil {
		if os.Getenv("VOCAT_REQUIRE_AMR_TEST") == "1" {
			t.Fatal("default release has no real AMR backend:", err)
		}
		t.Skip("native AMR dependency not installed")
	}
	defer codec.Close()
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(11000 * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	var energy int64
	for i := 0; i < 8; i++ {
		frame, err := codec.Encode(pcm, 7)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := codec.Decode(frame)
		if err != nil || len(decoded) != 160 {
			t.Fatalf("decode len=%d error=%v", len(decoded), err)
		}
		for _, sample := range decoded {
			energy += int64(sample) * int64(sample)
		}
	}
	if energy < 100000 {
		t.Fatal("default codec returned silence")
	}
	media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	if !strings.Contains(string(media.offerSDP(net.IPv4(127, 0, 0, 1))), "AMR/8000") {
		t.Fatal("working default codec not offered")
	}
}
