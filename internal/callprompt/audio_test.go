package callprompt

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func syntheticWAV() []byte {
	b := make([]byte, 44+320)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 8000)
	binary.LittleEndian.PutUint32(b[28:], 16000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 320)
	binary.LittleEndian.PutUint16(b[44:], 1234)
	return b
}

func TestPromptWAVIsBoundedPrivatePCMAndCopiesUpload(t *testing.T) {
	wav := syntheticWAV()
	recording, err := Decode(context.Background(), wav, "../../message.wav")
	if err != nil {
		t.Fatal(err)
	}
	if recording.Name != "message.wav" || recording.DurationMS != 20 || len(recording.PCM) != 320 || binary.LittleEndian.Uint16(recording.PCM) != 1234 {
		t.Fatal(recording)
	}
	wav[44] = 0
	if binary.LittleEndian.Uint16(recording.PCM) != 1234 {
		t.Fatal("playback retained mutable upload buffer")
	}
	for _, mode := range []string{"truncated", "declared-size", "frame-size", "duration", "encoding", "url"} {
		t.Run(mode, func(t *testing.T) {
			data := syntheticWAV()
			switch mode {
			case "truncated":
				data = data[:43]
			case "declared-size":
				data[4]++
			case "frame-size":
				data[32] = 4
			case "duration":
				binary.LittleEndian.PutUint32(data[24:], 1)
			case "encoding":
				data[20] = 9
			case "url":
				data = []byte("https://example.invalid/audio.wav")
			}
			if _, err := Decode(context.Background(), data, "upload.wav"); !errors.Is(err, ErrFormat) {
				t.Fatal("malformed upload accepted", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Decode(ctx, syntheticWAV(), "a.wav"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled import processed audio", err)
	}
}
