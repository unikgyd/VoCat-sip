package ims

import "errors"

var errAMRUnavailable = errors.New("ims: AMR-NB encoder and decoder are unavailable")

type amrCodec interface {
	Encode([]int16, int) ([]byte, error)
	Decode([]byte) ([]int16, error)
	Close()
}

// Sizes are speech bits, not storage-octet counts (RFC 4867).
var amrNBBits = [16]int{95, 103, 118, 134, 148, 159, 204, 244, 39, -1, -1, -1, -1, -1, -1, 0}

func amrFrameBits(frame []byte) (int, error) {
	if len(frame) == 0 {
		return 0, errors.New("ims: empty AMR storage frame")
	}
	bits := amrNBBits[(frame[0]>>3)&15]
	if bits < 0 || len(frame) != 1+(bits+7)/8 {
		return 0, errors.New("ims: invalid AMR frame type or size")
	}
	return bits, nil
}

// AMR payloads are bounded to 16 mono frames (320 ms). This implementation
// deliberately does not negotiate interleaving, CRC or robust sorting.
func packAMR(frames [][]byte, octet bool) ([]byte, error) {
	if len(frames) == 0 || len(frames) > 16 {
		return nil, errors.New("ims: excessive AMR frame count")
	}
	bits := 4 + 6*len(frames)
	if octet {
		bits = 8 + 8*len(frames)
	}
	for _, frame := range frames {
		n, err := amrFrameBits(frame)
		if err != nil {
			return nil, err
		}
		if octet {
			n = (n + 7) / 8 * 8
		}
		bits += n
	}
	out := make([]byte, (bits+7)/8)
	put := func(at, value int) { out[at/8] |= byte(value&1) << (7 - at%8) }
	for at := 0; at < 4; at++ {
		put(at, 1)
	} // CMR=15: no mode request.
	at := 4
	if octet {
		at = 8
	}
	for i, frame := range frames {
		if i+1 < len(frames) {
			put(at, 1)
		}
		at++
		for bit := 3; bit >= 0; bit-- {
			put(at, int(frame[0]>>(3+bit)))
			at++
		}
		put(at, int(frame[0]>>2))
		at++
		if octet {
			at += 2
		}
	}
	for _, frame := range frames {
		n, _ := amrFrameBits(frame)
		for bit := 0; bit < n; bit++ {
			put(at, int(frame[1+bit/8]>>(7-bit%8)))
			at++
		}
		if octet {
			at = (at + 7) / 8 * 8
		}
	}
	return out, nil
}

func unpackAMR(payload []byte, octet bool) ([][]byte, byte, error) {
	if len(payload) < 2 || len(payload) > 1024 {
		return nil, 0, errors.New("ims: invalid AMR RTP payload size")
	}
	get := func(at int) int { return int(payload[at/8]>>(7-at%8)) & 1 }
	cmr := payload[0] >> 4 // Future/reserved CMR values are not a request we apply.
	at := 4
	if octet {
		at = 8
	}
	var frames [][]byte
	for {
		width := 6
		if octet {
			width = 8
		}
		if at+width > len(payload)*8 || len(frames) >= 16 {
			return nil, cmr, errors.New("ims: truncated or excessive AMR TOC")
		}
		more := get(at)
		at++
		ft := 0
		for bit := 0; bit < 4; bit++ {
			ft = ft*2 + get(at)
			at++
		}
		q := get(at)
		at++
		if octet {
			at += 2
		}
		n := amrNBBits[ft]
		if n < 0 {
			return nil, cmr, errors.New("ims: reserved AMR-NB frame type")
		}
		frame := make([]byte, 1+(n+7)/8)
		frame[0] = byte(ft<<3 | q<<2)
		frames = append(frames, frame)
		if more == 0 {
			break
		}
	}
	for _, frame := range frames {
		n := amrNBBits[frame[0]>>3]
		if at+n > len(payload)*8 {
			return nil, cmr, errors.New("ims: truncated AMR speech")
		}
		for bit := 0; bit < n; bit++ {
			frame[1+bit/8] |= byte(get(at)) << (7 - bit%8)
			at++
		}
		if octet {
			at = (at + 7) / 8 * 8
		}
	}
	// Padding bits are ignored on receipt, but extra whole octets are not
	// padding (RFC 4867 4.3.4 and 4.4.3).
	if (at+7)/8 != len(payload) {
		return nil, cmr, errors.New("ims: trailing AMR payload octets")
	}
	return frames, cmr, nil
}
