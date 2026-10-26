//go:build linux && (amd64 || arm64 || 386 || arm) && !vocat_static

package ims

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNativeAMRAllModesAndRTPDuplex(t *testing.T) {
	codec, err := openNativeAMR()
	if err != nil {
		if os.Getenv("VOCAT_REQUIRE_AMR_TEST") == "1" {
			t.Fatal("required native codec unavailable:", err)
		}
		t.Skip("install libopencore-amrnb.so.0 for native AMR test")
	}
	defer codec.Close()
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	for mode := 0; mode <= 7; mode++ {
		var energy int64
		for i := 0; i < 8; i++ {
			before := append([]int16(nil), pcm...)
			frame, err := codec.Encode(pcm, mode)
			if !slices.Equal(before, pcm) {
				t.Fatal("native encoder modified caller PCM")
			}
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := codec.Decode(frame)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range decoded {
				if v < 0 {
					energy -= int64(v)
				} else {
					energy += int64(v)
				}
			}
		}
		t.Logf("native mode=%d energy=%d", mode, energy)
		if energy < 10000 {
			t.Fatalf("native mode %d returned silence: %d", mode, energy)
		}
	}
	for _, octet := range []int{0, 1} {
		left, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		right, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer left.Close()
			defer right.Close()
			sdp := func(m *rtpMedia) []byte {
				return m.buildSDP(net.IPv4(127, 0, 0, 1), "110", []string{"a=rtpmap:110 AMR/8000/1", fmt.Sprintf("a=fmtp:110 octet-align=%d;mode-set=7", octet)})
			}
			if err := left.configureRemote(sdp(right)); err != nil {
				t.Fatal(err)
			}
			if err := right.configureRemote(sdp(left)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for _, pair := range [][2]*rtpMedia{{left, right}, {right, left}} {
				var energy int64
				for i := 0; i < 12; i++ {
					if err := pair[0].WritePCM(pcm); err != nil {
						t.Fatal(err)
					}
					decoded, err := pair[1].ReadPCM(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, v := range decoded {
						energy += int64(v) * int64(v)
					}
				}
				if energy < 100000 {
					t.Fatal("native RTP returned silent PCM")
				}
			}
		}()
	}
	codec.Close()
	if _, err := codec.Encode(pcm, 7); err == nil {
		t.Fatal("closed native state reused")
	}
}

func TestNativeAMRMissingLibraryFallsBackWithoutFakeCapability(t *testing.T) {
	if os.Getenv("VOCAT_TEST_MISSING_AMR") != "1" {
		t.Skip("separate process without codec library")
	}
	if codec, err := openNativeAMR(); err == nil {
		codec.Close()
		t.Fatal("missing-library test unexpectedly found a codec")
	}
	media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	if strings.Contains(string(media.offerSDP(net.IPv4(127, 0, 0, 1))), "AMR/") {
		t.Fatal("missing codec was advertised")
	}
}
