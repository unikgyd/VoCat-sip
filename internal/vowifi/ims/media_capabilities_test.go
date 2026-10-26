package ims

import (
	"errors"
	"reflect"
	"testing"
)

type capabilityCodec struct {
	closed int
	mode   string
}

func (c *capabilityCodec) Encode(pcm []int16, mode int) ([]byte, error) {
	if c.mode == "encode_error" {
		return nil, errors.New("private backend detail")
	}
	if c.mode == "mutate_pcm" {
		pcm[0]++
	}
	frame := make([]byte, 32)
	frame[0] = 7<<3 | 4
	if c.mode == "wrong_frame" {
		frame = frame[:3]
	}
	return frame, nil
}
func (c *capabilityCodec) Decode(frame []byte) ([]int16, error) {
	if c.mode == "decode_error" {
		return nil, errors.New("private backend detail")
	}
	if c.mode == "mutate_frame" {
		frame[0]++
	}
	pcm := make([]int16, 160)
	if c.mode != "silence" {
		for i := range pcm {
			pcm[i] = 1000
		}
	}
	if c.mode == "short_pcm" {
		return pcm[:159], nil
	}
	return pcm, nil
}
func (c *capabilityCodec) Close() { c.closed++ }
func TestMediaCapabilityRequiresRealBidirectionalSelfTest(t *testing.T) {
	for _, mode := range []string{"ok", "encode_error", "decode_error", "wrong_frame", "mutate_pcm", "mutate_frame", "silence", "short_pcm"} {
		t.Run(mode, func(t *testing.T) {
			codec := &capabilityCodec{mode: mode}
			status := probeMediaCapabilities(func() (amrCodec, error) { return codec, nil }, true)
			if status.AMRNB.Available != (mode == "ok") || status.AMRWB.Available || !reflect.DeepEqual(status.PCMCodecs, []string{"PCMA", "PCMU"}) || codec.closed != 1 {
				t.Fatalf("status=%+v closed=%d", status, codec.closed)
			}
			if mode == "ok" && status.AMRNB.Code != "ready" || mode != "ok" && status.AMRNB.Code != "self_test_failed" {
				t.Fatal(status)
			}
		})
	}
	missing := probeMediaCapabilities(func() (amrCodec, error) { return nil, errors.New("private path") }, true)
	if missing.AMRNB.Code != "dependency_unavailable" || missing.AMRNB.Available {
		t.Fatal(missing)
	}
	called := false
	static := probeMediaCapabilities(func() (amrCodec, error) { called = true; return nil, nil }, false)
	if called || static.AMRNB.Built || static.AMRNB.Available || static.AMRNB.Code != "not_built" {
		t.Fatal(static)
	}
}
