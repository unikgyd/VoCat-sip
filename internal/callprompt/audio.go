// Package callprompt imports a recording into bounded, immutable telephone PCM.
// It never opens a microphone, speaker, URL or user-selected filesystem path.
package callprompt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"unicode"
)

const MaxUploadBytes = 64 << 20
const MaxSeconds = 300
const SampleRate = 8000
const MaxPCMBytes = SampleRate * 2 * MaxSeconds

var ErrFormat = errors.New("recording must be a supported, non-empty mono/stereo audio file, at most five minutes")

type Recording struct {
	Name           string `json:"name"`
	Format         string `json:"format"`
	SourceRate     int    `json:"source_rate,omitempty"`
	SourceChannels int    `json:"source_channels,omitempty"`
	DurationMS     int    `json:"duration_ms"`
	PCM            []byte `json:"-"` // signed 16-bit little-endian mono at 8000 Hz
}

func Decode(ctx context.Context, data []byte, name string) (Recording, error) {
	if len(data) == 0 || len(data) > MaxUploadBytes {
		return Recording{}, ErrFormat
	}
	if err := ctx.Err(); err != nil {
		return Recording{}, err
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, name)
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	if name == "" || name == "." {
		name = "recording"
	}
	var result Recording
	var err error
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		result, err = decodeWAV(ctx, data)
	} else {
		result, err = decodeCompressed(ctx, data)
	}
	if err != nil {
		return Recording{}, err
	}
	if len(result.PCM) == 0 || len(result.PCM)%2 != 0 || len(result.PCM) > MaxPCMBytes {
		return Recording{}, ErrFormat
	}
	result.Name, result.DurationMS = name, (len(result.PCM)/2*1000+SampleRate-1)/SampleRate
	return result, nil
}

func decodeWAV(ctx context.Context, data []byte) (Recording, error) {
	bad := func() (Recording, error) { return Recording{}, ErrFormat }
	if uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return bad()
	}
	var format, audio []byte
	for offset, chunks := 12, 0; offset < len(data); chunks++ {
		if chunks >= 1024 || len(data)-offset < 8 {
			return bad()
		}
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		end := uint64(offset) + 8 + size
		if end > uint64(len(data)) {
			return bad()
		}
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if format != nil || size < 16 || size > 256 {
				return bad()
			}
			format = data[offset+8 : int(end)]
		case "data":
			if audio != nil {
				return bad()
			}
			audio = data[offset+8 : int(end)]
		}
		offset = int(end + size%2)
		if offset > len(data) {
			return bad()
		}
	}
	if len(format) < 16 || len(audio) == 0 {
		return bad()
	}
	encoding := binary.LittleEndian.Uint16(format[:2])
	channels := int(binary.LittleEndian.Uint16(format[2:4]))
	rate := int(binary.LittleEndian.Uint32(format[4:8]))
	bits := int(binary.LittleEndian.Uint16(format[14:16]))
	if len(format) > 16 && (len(format) < 18 || int(binary.LittleEndian.Uint16(format[16:18])) > len(format)-18) {
		return bad()
	}
	if encoding == 0xfffe {
		if len(format) < 40 || binary.LittleEndian.Uint16(format[16:18]) < 22 || !bytes.Equal(format[26:40], []byte{0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}) {
			return bad()
		}
		if valid := binary.LittleEndian.Uint16(format[18:20]); valid != 0 && int(valid) != bits {
			return bad()
		}
		encoding = binary.LittleEndian.Uint16(format[24:26])
	}
	if channels < 1 || channels > 2 || rate < 4000 || rate > 192000 || encoding != 1 && encoding != 3 ||
		encoding == 1 && bits != 8 && bits != 16 && bits != 24 && bits != 32 || encoding == 3 && bits != 32 && bits != 64 {
		return bad()
	}
	width, align := bits/8, channels*(bits/8)
	if int(binary.LittleEndian.Uint16(format[12:14])) != align || uint64(binary.LittleEndian.Uint32(format[8:12])) != uint64(rate*align) || len(audio)%align != 0 {
		return bad()
	}
	frames := len(audio) / align
	if frames == 0 || int64(frames) > int64(rate)*MaxSeconds {
		return bad()
	}
	result := Recording{Format: "wav", SourceRate: rate, SourceChannels: channels}
	if encoding == 1 && channels == 1 && bits == 16 && rate == SampleRate {
		result.PCM = append([]byte(nil), audio...)
		return result, nil
	}
	count := int(int64(frames) * SampleRate / int64(rate))
	if count == 0 {
		return bad()
	}
	result.PCM = make([]byte, count*2)
	read := func(frame int) float64 {
		var value float64
		for channel := 0; channel < channels; channel++ {
			b := audio[frame*align+channel*width:]
			var sample float64
			if encoding == 3 {
				if bits == 32 {
					sample = float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
				} else {
					sample = math.Float64frombits(binary.LittleEndian.Uint64(b))
				}
				if math.IsNaN(sample) || math.IsInf(sample, 0) {
					sample = 0
				}
			} else {
				switch bits {
				case 8:
					sample = float64(int(b[0])-128) / 128
				case 16:
					sample = float64(int16(binary.LittleEndian.Uint16(b))) / 32768
				case 24:
					v := int32(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16)
					sample = float64(v<<8) / 2147483648
				case 32:
					sample = float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648
				}
			}
			value += math.Max(-1, math.Min(1, sample))
		}
		return value / float64(channels)
	}
	// Six-pole low-pass before downsampling, followed by linear interpolation.
	// All source samples are processed once; no duration-sized float allocation.
	filters := []biquad{}
	if rate > SampleRate {
		for _, q := range []float64{0.5176380902, 0.7071067812, 1.9318516526} {
			filters = append(filters, lowpass(float64(rate), 3400, q))
		}
	}
	var previous, current float64
	next, output := 0, 0
	for frame := 0; frame < frames && output < count; frame++ {
		if frame%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return Recording{}, err
			}
		}
		previous, current = current, read(frame)
		for i := range filters {
			current = filters[i].apply(current)
		}
		for output < count {
			position := float64(output) * float64(rate) / SampleRate
			next = int(math.Ceil(position))
			if next > frame {
				break
			}
			value := current
			if next > 0 {
				fraction := position - float64(next-1)
				value = previous + (current-previous)*fraction
			}
			sample := int(math.Round(math.Max(-1, math.Min(1, value)) * 32768))
			binary.LittleEndian.PutUint16(result.PCM[output*2:], uint16(int16(max(-32768, min(32767, sample)))))
			output++
		}
	}
	// For upsampling, hold only the final source sample over its own interval.
	for output < count {
		sample := int(math.Round(math.Max(-1, math.Min(1, current)) * 32768))
		binary.LittleEndian.PutUint16(result.PCM[output*2:], uint16(int16(max(-32768, min(32767, sample)))))
		output++
	}
	return result, nil
}

type biquad struct{ b0, b1, b2, a1, a2, z1, z2 float64 }

func lowpass(rate, frequency, q float64) biquad {
	w := 2 * math.Pi * frequency / rate
	c, s := math.Cos(w), math.Sin(w)
	alpha := s / (2 * q)
	a0 := 1 + alpha
	return biquad{b0: (1 - c) / 2 / a0, b1: (1 - c) / a0, b2: (1 - c) / 2 / a0, a1: -2 * c / a0, a2: (1 - alpha) / a0}
}
func (b *biquad) apply(x float64) float64 {
	y := b.b0*x + b.z1
	b.z1 = b.b1*x - b.a1*y + b.z2
	b.z2 = b.b2*x - b.a2*y
	return y
}

func WAV(pcm []byte) []byte {
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > MaxPCMBytes {
		return nil
	}
	out := make([]byte, 44+len(pcm))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], SampleRate)
	binary.LittleEndian.PutUint32(out[28:], SampleRate*2)
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}
