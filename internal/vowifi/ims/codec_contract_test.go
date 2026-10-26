package ims

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// Mathematical G.711 samples shared with VoWin and the Rust reference.
//
//go:embed testdata/g711-codec-v1.json
var sharedG711Contract []byte

//go:embed testdata/amr-nb-rtp-v1.json
var sharedAMRContract []byte

func TestSharedVoWinAMRPayloadContract(t *testing.T) {
	var contract struct {
		Cases []struct {
			Name       string
			Octet      bool
			PayloadHex string
			Accepted   bool
			FramesHex  []string
		}
	}
	if err := json.Unmarshal(sharedAMRContract, &contract); err != nil {
		t.Fatal(err)
	}
	for _, tc := range contract.Cases {
		payload, err := hex.DecodeString(tc.PayloadHex)
		if err != nil {
			t.Fatal(err)
		}
		frames, _, err := unpackAMR(payload, tc.Octet)
		if (err == nil) != tc.Accepted {
			t.Fatalf("%s acceptance: %v", tc.Name, err)
		}
		if tc.Accepted {
			if len(frames) != len(tc.FramesHex) {
				t.Fatal(tc.Name)
			}
			for i, want := range tc.FramesHex {
				expected, _ := hex.DecodeString(want)
				if !bytes.Equal(frames[i], expected) {
					t.Fatalf("%s frame %d changed", tc.Name, i)
				}
			}
		}
	}
}

func TestSharedVoWinRustG711Vectors(t *testing.T) {
	var contract struct {
		Version int      `json:"version"`
		Cases   [][5]int `json:"cases"`
	}
	if err := json.Unmarshal(sharedG711Contract, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || len(contract.Cases) == 0 {
		t.Fatal("missing G.711 interoperability cases")
	}
	for _, row := range contract.Cases {
		if got := linearToALaw(int16(row[0])); int(got) != row[1] {
			t.Errorf("PCM %d A-law: got %x, want %x", row[0], got, row[1])
		}
		if got := aLawToLinear(byte(row[1])); int(got) != row[2] {
			t.Errorf("A-law %x: got %d, want %d", row[1], got, row[2])
		}
		if got := linearToMuLaw(int16(row[0])); int(got) != row[3] {
			t.Errorf("PCM %d mu-law: got %x, want %x", row[0], got, row[3])
		}
		if got := muLawToLinear(byte(row[3])); int(got) != row[4] {
			t.Errorf("mu-law %x: got %d, want %d", row[3], got, row[4])
		}
	}
}

func TestPCMFullScaleKeepsG711SignAndQuantization(t *testing.T) {
	for sample := -32768; sample <= 32767; sample++ {
		a := int(aLawToLinear(linearToALaw(int16(sample))))
		u := int(muLawToLinear(linearToMuLaw(int16(sample))))
		deltaA, deltaU := sample-a, sample-u
		if deltaA < -512 || deltaA > 512 || deltaU < -644 || deltaU > 644 ||
			(sample >= 0) != (a > 0) || (sample <= -4 || sample >= 4) && (sample > 0) != (u > 0) {
			t.Fatalf("PCM %d decoded A=%d mu=%d", sample, a, u)
		}
	}
}
