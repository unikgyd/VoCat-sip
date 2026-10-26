package ims

import (
	"bytes"
	"testing"
)

func TestTranscodeG711UsesExistingCodecAndOwnsBuffer(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	original := append([]byte(nil), payload...)
	for _, from := range []string{"PCMA", "PCMU"} {
		for _, to := range []string{"PCMA", "PCMU"} {
			out, err := TranscodeG711(payload, from, to)
			if err != nil || len(out) != len(payload) {
				t.Fatal(err)
			}
			if from == to && !bytes.Equal(out, payload) {
				t.Fatal("same-codec relay changed samples")
			}
			if from != to {
				for i, sample := range payload {
					var want byte
					if from == "PCMA" {
						want = linearToMuLaw(aLawToLinear(sample))
					} else {
						want = linearToALaw(muLawToLinear(sample))
					}
					if out[i] != want {
						t.Fatal("transcode differs from existing codec")
					}
				}
			}
			out[0] ^= 255
			if !bytes.Equal(payload, original) {
				t.Fatal("output aliases input")
			}
		}
	}
	for _, pair := range [][2]string{{"AMR", "PCMU"}, {"PCMU", "AMR"}, {"", "PCMA"}} {
		if _, err := TranscodeG711(payload, pair[0], pair[1]); err == nil {
			t.Fatal("unsupported codec accepted")
		}
	}
}

func TestG711PCMBridgeExportsMatchExistingCodecAndOwnBuffers(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	for _, from := range []string{"PCMA", "PCMU"} {
		pcm, err := DecodeG711(payload, from)
		if err != nil {
			t.Fatal(err)
		}
		for i, value := range payload {
			expected := muLawToLinear(value)
			if from == "PCMA" {
				expected = aLawToLinear(value)
			}
			if pcm[i] != expected {
				t.Fatal("PCM decoder diverged")
			}
		}
		for _, to := range []string{"PCMA", "PCMU"} {
			out, err := EncodeG711(pcm, to)
			if err != nil {
				t.Fatal(err)
			}
			for i, sample := range pcm {
				expected := linearToMuLaw(sample)
				if to == "PCMA" {
					expected = linearToALaw(sample)
				}
				if out[i] != expected {
					t.Fatal("PCM encoder diverged")
				}
			}
			out[0] ^= 255
			if payload[0] != 0 {
				t.Fatal("codec output aliases input")
			}
		}
		pcm[0]++
		if payload[0] != 0 {
			t.Fatal("PCM aliases encoded input")
		}
	}
	if _, err := DecodeG711(payload, "AMR"); err == nil {
		t.Fatal("unsupported PCM codec accepted")
	}
	if _, err := EncodeG711(make([]int16, 1601), "PCMA"); err == nil {
		t.Fatal("oversized PCM frame accepted")
	}
}
