package ims

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"vocat/internal/rtpevent"
)

var ErrDTMFUnsupported = errors.New("ims: this digit was not negotiated as telephone-event/8000")
var ErrDTMFBusy = errors.New("ims: a telephone event is already in progress")

const dtmfAlphabet = rtpevent.Alphabet
const offeredDTMFPayload = rtpevent.OfferedPayload

// A missing events parameter retains RFC 2833's 0-15 compatibility default.
// Optional event errors disable telephone events without breaking voice.
func negotiateDTMF(body []byte, formats []string, mappings map[int]string, offered bool) (byte, uint16) {
	selected, err := selectAudioSDP(body)
	if err != nil {
		return 0, 0
	}
	for _, format := range formats {
		pt, err := strconv.Atoi(format)
		if err != nil || pt < 96 || pt > 127 || offered && pt != offeredDTMFPayload ||
			!strings.EqualFold(strings.TrimSuffix(mappings[pt], "/1"), "telephone-event/8000") {
			continue
		}
		mask, seen, valid := uint16(0xffff), false, true
		for _, raw := range strings.Split(string(selected), "\n") {
			line := strings.TrimSpace(raw)
			if !strings.HasPrefix(strings.ToLower(line), "a=fmtp:") {
				continue
			}
			fields := strings.Fields(line[len("a=fmtp:"):])
			if len(fields) == 0 {
				continue
			}
			assigned, e := strconv.Atoi(fields[0])
			if e != nil || assigned != pt {
				continue
			}
			if seen || len(fields) != 2 {
				valid = false
				break
			}
			seen = true
			mask, valid = rtpevent.ParseMask(fields[1])
		}
		if valid && mask != 0 {
			return byte(pt), mask
		}
	}
	return 0, 0
}

func (media *rtpMedia) DTMFDigits() string {
	media.mu.RLock()
	mask := media.dtmfEvents
	media.mu.RUnlock()
	return rtpevent.Digits(mask)
}

func dtmfFormat(mask uint16) string {
	return rtpevent.Format(mask)
}

// SendDTMF sends a single 160 ms RFC 4733 event. No key queue, audio/INFO/AT
// fallback or automatic application retry: a failed send can be partially heard.
func (media *rtpMedia) SendDTMF(ctx context.Context, digit byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	event := strings.IndexByte(dtmfAlphabet, digit)
	if event < 0 {
		return ErrDTMFUnsupported
	}
	if !media.dtmfMu.TryLock() {
		return ErrDTMFBusy
	}
	defer media.dtmfMu.Unlock()
	media.writeMu.Lock()
	media.mu.RLock()
	remote, payload, mask, revision := media.remote, media.dtmfPayload, media.dtmfEvents, media.remoteRevision
	media.mu.RUnlock()
	select {
	case <-media.closed:
		media.writeMu.Unlock()
		return io.EOF
	default:
	}
	if remote == nil || mask&(1<<uint(event)) == 0 {
		media.writeMu.Unlock()
		return ErrDTMFUnsupported
	}
	timestamp := media.timestamp
	media.dtmfActive = true
	media.pending = nil
	media.writeMu.Unlock()
	started := time.Now()
	defer func() {
		media.writeMu.Lock()
		// Audio writes during the event are discarded, never replayed afterward.
		ticks := uint32(time.Since(started).Milliseconds() * 8)
		if ticks < 1280 {
			ticks = 1280
		}
		media.timestamp = timestamp + ticks
		media.pending = nil
		media.dtmfActive = false
		media.writeMu.Unlock()
	}()
	return rtpevent.Transmit(ctx, media.closed, byte(event), func(payloadBytes []byte, marker bool) error {
		media.writeMu.Lock()
		media.mu.RLock()
		current := media.remoteRevision == revision
		media.mu.RUnlock()
		err := ctx.Err()
		select {
		case <-media.closed:
			err = io.EOF
		default:
		}
		if err == nil && !current {
			err = errors.New("ims: media negotiation changed during telephone event")
		}
		if err == nil {
			packet := make([]byte, 16)
			packet[0] = 0x80
			packet[1] = payload
			if marker {
				packet[1] |= 0x80
			}
			binary.BigEndian.PutUint16(packet[2:4], media.sequence)
			binary.BigEndian.PutUint32(packet[4:8], timestamp)
			binary.BigEndian.PutUint32(packet[8:12], media.ssrc)
			copy(packet[12:], payloadBytes)
			_, err = media.conn.WriteToUDP(packet, remote)
			media.sequence++
		}
		media.writeMu.Unlock()
		return err
	})
}

// Called only by the RTP reader after endpoint, RTP header and payload checks.
type dtmfKey struct {
	digit    byte
	received time.Time
}

func (media *rtpMedia) receiveDTMF(payload []byte, ssrc, timestamp uint32, mask uint16) {
	digit, ok := media.dtmfReceiver.Accept(rtpevent.Spec{Mask: mask}, payload, ssrc, timestamp)
	if !ok {
		return
	}
	select {
	case media.dtmfDownlink <- dtmfKey{digit: digit, received: time.Now()}:
	default:
	}
}

// FlushDTMF discards bounded pre-ownership events when a trusted bridge takes
// this call's media. It does not consume an unbounded incoming flood.
func (media *rtpMedia) FlushDTMF() {
	for i := 0; i < cap(media.dtmfDownlink); i++ {
		select {
		case <-media.dtmfDownlink:
		default:
			return
		}
	}
}

func (media *rtpMedia) ReadDTMF(ctx context.Context) (byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-media.closed:
			return 0, io.EOF
		case key := <-media.dtmfDownlink:
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			select {
			case <-media.closed:
				return 0, io.EOF
			default:
			}
			if time.Since(key.received) > 320*time.Millisecond {
				continue
			}
			return key.digit, nil
		}
	}
}
