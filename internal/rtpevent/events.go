// Package rtpevent contains the shared RFC 4733 DTMF payload rules. It does
// not choose a socket, device, dialog or fallback transport.
package rtpevent

import (
	"context"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"time"
)

const Alphabet = "0123456789*#ABCD"
const OfferedPayload = 101

type Spec struct {
	Payload byte
	Mask    uint16
}

func (s Spec) Event(digit byte) (byte, bool) {
	n := strings.IndexByte(Alphabet, digit)
	return byte(n), n >= 0 && s.Mask&(1<<uint(n)) != 0
}
func (s Spec) Valid(payload []byte) bool {
	return len(payload) == 4 && payload[0] < 16 && s.Mask&(1<<uint(payload[0])) != 0 && binary.BigEndian.Uint16(payload[2:]) != 0
}
func Digits(mask uint16) string {
	var out strings.Builder
	for i, digit := range Alphabet {
		if mask&(1<<uint(i)) != 0 {
			out.WriteRune(digit)
		}
	}
	return out.String()
}
func Format(mask uint16) string {
	if mask == 0xffff {
		return "0-15"
	}
	var out []string
	for i := 0; i < 16; i++ {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, strconv.Itoa(i))
		}
	}
	return strings.Join(out, ",")
}

func ParseMask(value string) (uint16, bool) {
	if value == "" || len(value) > 1024 {
		return 0, false
	}
	var mask uint16
	for _, item := range strings.Split(value, ",") {
		if strings.IndexFunc(item, func(r rune) bool { return r != '-' && (r < '0' || r > '9') }) >= 0 {
			return 0, false
		}
		bounds := strings.Split(item, "-")
		if len(bounds) > 2 {
			return 0, false
		}
		first, err := strconv.Atoi(bounds[0])
		last := first
		if err != nil {
			return 0, false
		}
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
		}
		if err != nil || first < 0 || last < first || last > 255 {
			return 0, false
		}
		for n := first; n <= last && n < 16; n++ {
			mask |= 1 << uint(n)
		}
	}
	return mask, mask != 0
}

// Missing fmtp retains the 0-15 compatibility default. Malformed optional
// events disable this capability without making ordinary voice unavailable.
func Negotiate(formats []string, maps map[byte]string, fmtps map[byte][]string) Spec {
	for _, token := range formats {
		n, err := strconv.Atoi(token)
		if err != nil || n < 96 || n > 127 || !strings.EqualFold(strings.TrimSuffix(strings.ToLower(maps[byte(n)]), "/1"), "telephone-event/8000") {
			continue
		}
		mask := uint16(0xffff)
		if values := fmtps[byte(n)]; len(values) > 0 {
			if len(values) != 1 {
				continue
			}
			var valid bool
			mask, valid = ParseMask(values[0])
			if !valid {
				continue
			}
		}
		return Spec{Payload: byte(n), Mask: mask}
	}
	return Spec{}
}

type receipt struct {
	ssrc, timestamp uint32
	event           byte
}

// Receiver is owned by one RTP reader, after source/header/payload validation.
type Receiver struct {
	seen  map[receipt]bool
	order []receipt
}

func (r *Receiver) Accept(spec Spec, payload []byte, ssrc, timestamp uint32) (byte, bool) {
	if !spec.Valid(payload) || payload[1]&0x80 == 0 {
		return 0, false
	}
	key := receipt{ssrc, timestamp, payload[0]}
	if r.seen[key] {
		return 0, false
	}
	if r.seen == nil {
		r.seen = map[receipt]bool{}
	}
	if len(r.order) >= 64 {
		delete(r.seen, r.order[0])
		r.order = r.order[1:]
	}
	r.seen[key] = true
	r.order = append(r.order, key)
	return Alphabet[payload[0]], true
}

// Transmit emits one fixed 160 ms event, with 20 ms updates and three final
// reports. The owner supplies sequence/SSRC/timestamp and serializes audio.
// A send failure is returned once; it is never retried as another event.
func Transmit(ctx context.Context, closed <-chan struct{}, event byte, write func([]byte, bool) error) error {
	wait := func(delay time.Duration) error {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-closed:
			return io.EOF
		case <-timer.C:
			return nil
		}
	}
	for i := 0; i < 10; i++ {
		if i > 0 {
			if err := wait(20 * time.Millisecond); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-closed:
			return io.EOF
		default:
		}
		payload := []byte{event, 10, 0, 0}
		if i >= 7 {
			payload[1] |= 0x80
		}
		binary.BigEndian.PutUint16(payload[2:], uint16(min(i+1, 8)*160))
		if err := write(payload, i == 0); err != nil {
			return err
		}
	}
	return wait(50 * time.Millisecond)
}
