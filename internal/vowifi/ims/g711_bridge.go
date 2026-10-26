package ims

import "fmt"

// DecodeG711/EncodeG711 expose the same verified codec used by IMS RTP, not a
// second implementation. Returned buffers are owned by the caller.
func DecodeG711(payload []byte, codec string) ([]int16, error) {
	if !supportedPCMCodec(codec) || len(payload) > 1600 {
		return nil, fmt.Errorf("unsupported G.711 codec or frame size")
	}
	out := make([]int16, len(payload))
	for i, b := range payload {
		if codec == "PCMA" {
			out[i] = aLawToLinear(b)
		} else {
			out[i] = muLawToLinear(b)
		}
	}
	return out, nil
}
func EncodeG711(samples []int16, codec string) ([]byte, error) {
	if !supportedPCMCodec(codec) || len(samples) > 1600 {
		return nil, fmt.Errorf("unsupported G.711 codec or frame size")
	}
	out := make([]byte, len(samples))
	for i, sample := range samples {
		if codec == "PCMA" {
			out[i] = linearToALaw(sample)
		} else {
			out[i] = linearToMuLaw(sample)
		}
	}
	return out, nil
}

// TranscodeG711 reuses the media core's verified G.711 implementation for the
// private SIP gateway. It never aliases or mutates the caller's RTP payload.
func TranscodeG711(payload []byte, from, to string) ([]byte, error) {
	if !supportedPCMCodec(from) || !supportedPCMCodec(to) {
		return nil, fmt.Errorf("unsupported G.711 codec")
	}
	out := make([]byte, len(payload))
	for i, value := range payload {
		if from == to {
			out[i] = value
			continue
		}
		var pcm int16
		if from == "PCMA" {
			pcm = aLawToLinear(value)
		} else {
			pcm = muLawToLinear(value)
		}
		if to == "PCMA" {
			out[i] = linearToALaw(pcm)
		} else {
			out[i] = linearToMuLaw(pcm)
		}
	}
	return out, nil
}
