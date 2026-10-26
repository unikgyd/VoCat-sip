package ims

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func syntheticAMR(mode int) []byte {
	n := amrNBBits[mode]
	frame := make([]byte, 1+(n+7)/8)
	frame[0] = byte(mode<<3 | 4)
	for i := 1; i < len(frame); i++ {
		frame[i] = byte(i * 7)
	}
	if n%8 != 0 {
		frame[len(frame)-1] &= byte(255 << (8 - n%8))
	}
	return frame
}

func TestAMRPayloadBitExactAndCompoundFrames(t *testing.T) {
	for _, octet := range []bool{false, true} {
		for _, mode := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 15} {
			frame := syntheticAMR(mode)
			payload, err := packAMR([][]byte{frame}, octet)
			if err != nil {
				t.Fatal(err)
			}
			// Independent string-bit oracle, not the production bit writer.
			bits := "1111"
			toc := fmt.Sprintf("0%04b1", mode)
			speech := ""
			for _, b := range frame[1:] {
				speech += fmt.Sprintf("%08b", b)
			}
			speech = speech[:amrNBBits[mode]]
			if octet {
				bits += "0000"
				toc += "00"
				speech += strings.Repeat("0", (8-len(speech)%8)%8)
			}
			bits += toc + speech
			bits += strings.Repeat("0", (8-len(bits)%8)%8)
			actual := ""
			for _, b := range payload {
				actual += fmt.Sprintf("%08b", b)
			}
			if actual != bits {
				t.Fatalf("mode %d octet %v: %s != %s", mode, octet, actual, bits)
			}
			decoded, cmr, err := unpackAMR(payload, octet)
			if err != nil || cmr != 15 || len(decoded) != 1 || !bytes.Equal(decoded[0], frame) {
				t.Fatalf("roundtrip mode %d: %x %v", mode, decoded, err)
			}
		}
	}
	for _, octet := range []bool{false, true} {
		frames := [][]byte{syntheticAMR(0), syntheticAMR(7), syntheticAMR(8), syntheticAMR(15)}
		payload, _ := packAMR(frames, octet)
		decoded, _, err := unpackAMR(payload, octet)
		if err != nil || len(decoded) != len(frames) {
			t.Fatal(err)
		}
		for i := range frames {
			if !bytes.Equal(frames[i], decoded[i]) {
				t.Fatal("compound frame changed")
			}
		}
		if _, _, err = unpackAMR(append(payload, 0), octet); err == nil {
			t.Fatal("trailing whole octet accepted")
		}
		if _, _, err = unpackAMR(payload[:len(payload)-1], octet); err == nil {
			t.Fatal("truncation padded with synthetic speech")
		}
	}
	if _, _, err := unpackAMR([]byte{0xf0, 0x4c}, true); err == nil {
		t.Fatal("reserved FT accepted")
	}
	if _, err := packAMR(make([][]byte, 17), false); err == nil {
		t.Fatal("excess frames accepted")
	}
}

func TestAMRParametersModeSetAndAdaptation(t *testing.T) {
	base := "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 45000 RTP/AVP 102\r\na=rtpmap:102 AMR/8000\r\na=fmtp:102 "
	p, err := parseAMRParameters([]byte(base+"octet-align=1;mode-set=0,2,7;mode-change-period=2;mode-change-neighbor=1"), 102)
	if err != nil || !p.octet || p.highest() != 7 || p.next(7, 0, 0) != 2 || p.next(2, 0, 1) != 2 || p.next(2, 0, 2) != 0 || p.next(0, 6, 4) != 0 {
		t.Fatalf("mode constraints: %+v %v", p, err)
	}
	for _, bad := range []string{"mode-set=", "mode-set=8", "mode-set=1,1", "octet-align=2", "octet-align=1;octet-align=0", "crc=1", "robust-sorting=1", "interleaving=0", "mode-change-period=3", "mode-change-neighbor=2"} {
		if _, err := parseAMRParameters([]byte(base+bad), 102); err == nil {
			t.Fatalf("bad fmtp accepted: %s", bad)
		}
	}
}

type mockAMR struct{}

func (*mockAMR) Encode(_ []int16, mode int) ([]byte, error) { return syntheticAMR(mode), nil }
func (*mockAMR) Decode(_ []byte) ([]int16, error)           { return make([]int16, 160), nil }
func (*mockAMR) Close()                                     {}

func TestAMRMediaDuplexUsesNegotiatedPayloadAndAlignment(t *testing.T) {
	for _, octet := range []int{0, 1} {
		factory := func() (amrCodec, error) { return &mockAMR{}, nil }
		left, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), factory)
		if err != nil {
			t.Fatal(err)
		}
		right, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), factory)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer left.Close()
			defer right.Close()
			sdp := func(m *rtpMedia) []byte {
				return m.buildSDP(net.IPv4(127, 0, 0, 1), "110", []string{"a=rtpmap:110 AMR/8000/1", fmt.Sprintf("a=fmtp:110 octet-align=%d;mode-set=0", octet)})
			}
			if err := left.configureRemote(sdp(right)); err != nil {
				t.Fatal(err)
			}
			if err := right.configureRemote(sdp(left)); err != nil {
				t.Fatal(err)
			}
			if left.Codec() != "AMR" || !strings.Contains(string(left.answerSDP(nil)), "mode-set=0") {
				t.Fatal("AMR not negotiated")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for _, pair := range [][2]*rtpMedia{{left, right}, {right, left}} {
				if err := pair[0].WritePCM(make([]int16, 320)); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					samples, err := pair[1].ReadPCM(ctx)
					if err != nil || len(samples) != 160 {
						t.Fatalf("no decoded PCM: %v", err)
					}
				}
			}
		}()
	}
}

func TestAMROfferAnswerCannotChangePayloadOrAlignment(t *testing.T) {
	media, err := newRTPMediaWithAMR(net.IPv4(127, 0, 0, 1), func() (amrCodec, error) { return &mockAMR{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	media.offerSDP(net.IPv4(127, 0, 0, 1))
	base := "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 45000 RTP/AVP "
	for _, answer := range []string{"110\r\na=rtpmap:110 AMR/8000\r\n", "102\r\na=rtpmap:102 AMR/8000\r\na=fmtp:102 octet-align=1\r\n"} {
		if err := media.configureRemote([]byte(base + answer)); err == nil {
			t.Fatal("answer changed unoffered payload/alignment")
		}
	}
	if err := media.configureRemote([]byte(base + "102\r\na=rtpmap:102 AMR/8000\r\na=fmtp:102 mode-set=2,7\r\n")); err != nil {
		t.Fatal(err)
	}
}
