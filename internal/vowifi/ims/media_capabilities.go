package ims

import (
	"math"
	"slices"

	"vocat/internal/buildinfo"
)

type CodecCapability struct {
	Built     bool   `json:"built"`
	Available bool   `json:"available"`
	Code      string `json:"code"`
}
type MediaCapabilities struct {
	RuntimeFlavor string          `json:"runtime_flavor"`
	PCMCodecs     []string        `json:"pcm_codecs"`
	AMRNB         CodecCapability `json:"amr_nb"`
	AMRWB         CodecCapability `json:"amr_wb"`
}

// ProbeMediaCapabilities opens only local codec states and feeds synthetic PCM.
// No modem, network socket, database, microphone or carrier action is involved.
// Availability means encode AND decode worked, not merely that dlopen succeeded.
func ProbeMediaCapabilities() MediaCapabilities {
	return probeMediaCapabilities(openNativeAMR, buildinfo.NativeMedia)
}
func probeMediaCapabilities(factory func() (amrCodec, error), built bool) MediaCapabilities {
	result := MediaCapabilities{RuntimeFlavor: buildinfo.RuntimeFlavor, PCMCodecs: []string{"PCMA", "PCMU"}, AMRNB: CodecCapability{Built: built, Code: "not_built"}, AMRWB: CodecCapability{Code: "not_built"}}
	if !built {
		return result
	}
	codec, err := factory()
	if codec != nil {
		defer codec.Close()
	}
	if err != nil || codec == nil {
		result.AMRNB.Code = "dependency_unavailable"
		return result
	}
	result.AMRNB.Code = "self_test_failed"
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(11000 * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	original := slices.Clone(pcm)
	var energy int64
	for i := 0; i < 8; i++ {
		frame, encodeErr := codec.Encode(pcm, 7)
		if encodeErr != nil || !slices.Equal(original, pcm) {
			return result
		}
		if _, frameErr := amrFrameBits(frame); frameErr != nil || frame[0]>>3&15 != 7 {
			return result
		}
		before := slices.Clone(frame)
		decoded, decodeErr := codec.Decode(frame)
		if decodeErr != nil || len(decoded) != 160 || !slices.Equal(before, frame) {
			return result
		}
		for _, sample := range decoded {
			energy += int64(sample) * int64(sample)
		}
	}
	if energy < 100000 {
		return result
	}
	result.AMRNB.Available = true
	result.AMRNB.Code = "ready"
	return result
}
