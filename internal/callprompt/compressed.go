package callprompt

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

func CompressedAvailable() bool { _, err := exec.LookPath("ffmpeg"); return err == nil }

func decodeCompressed(ctx context.Context, data []byte) (Recording, error) {
	format := ""
	switch {
	case len(data) >= 3 && string(data[:3]) == "ID3":
		format = "mp3"
	case len(data) >= 16 && bytes.Equal(data[:16], []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11, 0xa6, 0xd9, 0, 0xaa, 0, 0x62, 0xce, 0x6c}):
		format = "asf"
	case len(data) >= 8 && string(data[4:8]) == "ftyp":
		format = "mov"
	case len(data) >= 2 && data[0] == 0xff && data[1]&0xf6 == 0xf0:
		format = "aac"
	case len(data) >= 2 && data[0] == 0xff && data[1]&0xe0 == 0xe0:
		format = "mp3"
	default:
		return Recording{}, ErrFormat
	}
	program, err := exec.LookPath("ffmpeg")
	if err != nil {
		return Recording{}, errors.New("MP3/WMA/AAC import requires ffmpeg; PCM/float WAV works without it")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, "-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-max_alloc", "67108864", "-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "pipe", "-f", format, "-i", "pipe:0", "-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1", "-ar", "8000", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
	command.WaitDelay = time.Second
	command.Stdin = bytes.NewReader(data)
	output := &boundedBuffer{limit: MaxPCMBytes}
	command.Stdout = output
	command.Stderr = &boundedBuffer{limit: 2048}
	err = command.Run()
	if ctx.Err() != nil {
		return Recording{}, ctx.Err()
	}
	if err != nil || output.exceeded || output.Len() == 0 || output.Len()%2 != 0 {
		return Recording{}, ErrFormat
	}
	return Recording{Format: format, PCM: append([]byte(nil), output.Bytes()...)}, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.exceeded = true
		return 0, ErrFormat
	}
	return b.Buffer.Write(p)
}
