package sipgateway

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"vocat/internal/rtpevent"
)

// Optional, call-scoped carrier capability. It must preserve the supplied
// device/call ID, reject unsupported digits and never retry uncertain sends.
type CarrierDTMFBackend interface {
	SendDTMF(context.Context, string, string, byte) error
}

func (m *callMedia) forwardEvent(other *callMedia, packet []byte, start, end int) {
	other.mu.Lock()
	if other.remote == nil || other.stopped.Load() || !other.events.Valid(packet[start:end]) {
		other.mu.Unlock()
		return
	}
	target, spec := *other.remote, other.events
	other.mu.Unlock()
	out := make([]byte, 16)
	copy(out, packet[:12])
	out[0] = 0x80
	out[1] = packet[1]&128 | spec.Payload
	copy(out[12:], packet[start:end])
	if _, err := other.conn.WriteToUDP(out, &target); err == nil {
		m.packets.Add(1)
	}
}

func (b *carrierPCMBridge) bindDTMF(backend CarrierBackend, device, callID, digits string) {
	controller, ok := backend.(CarrierDTMFBackend)
	if !ok {
		return
	}
	for i, digit := range rtpevent.Alphabet {
		for _, allowed := range digits {
			if digit == allowed {
				b.carrierEvents.Mask |= 1 << uint(i)
			}
		}
	}
	if b.carrierEvents.Mask == 0 {
		return
	}
	b.sendCarrierDTMF = func(ctx context.Context, digit byte) error { return controller.SendDTMF(ctx, device, callID, digit) }
}
func (b *carrierPCMBridge) enqueueDTMF(digit byte) {
	if b.ctx.Err() != nil || b.sendCarrierDTMF == nil {
		return
	}
	if _, ok := b.carrierEvents.Event(digit); !ok {
		return
	}
	if !b.dtmfBusy.CompareAndSwap(false, true) {
		b.dtmfDropped.Add(1)
		return
	}
	// No key backlog: a slow modem or a pressed-key flood cannot postpone PCM
	// or replay keys against a later call. Redundant RTP finals are deduped.
	select {
	case b.digits <- digit:
	default:
		b.dtmfBusy.Store(false)
		b.dtmfDropped.Add(1)
	}
}
func (b *carrierPCMBridge) startDTMF() {
	if b.sendCarrierDTMF != nil {
		go func() {
			for {
				select {
				case <-b.ctx.Done():
					return
				case digit := <-b.digits:
					ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
					if ctx.Err() == nil {
						if err := b.sendCarrierDTMF(ctx, digit); err != nil {
							b.dtmfDropped.Add(1)
						}
					}
					cancel()
					b.dtmfBusy.Store(false)
				}
			}
		}()
	}
	if receiver, ok := b.carrier.(interface {
		ReadDTMF(context.Context) (byte, error)
	}); ok {
		if flusher, ok := b.carrier.(interface{ FlushDTMF() }); ok {
			flusher.FlushDTMF()
		}
		go func() {
			for {
				digit, err := receiver.ReadDTMF(b.ctx)
				if err != nil || b.ctx.Err() != nil {
					return
				}
				if err = b.sendDTMF(b.ctx, digit); err != nil {
					b.dtmfDropped.Add(1)
				}
			}
		}()
	}
}

func (b *carrierPCMBridge) sendDTMF(ctx context.Context, digit byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.dtmfMu.TryLock() {
		return errors.New("telephone event busy")
	}
	defer b.dtmfMu.Unlock()
	b.writeMu.Lock()
	b.media.mu.Lock()
	remote, spec, revision := b.media.remote, b.media.events, b.media.revision
	var target *net.UDPAddr
	if remote != nil {
		value := *remote
		target = &value
	}
	b.media.mu.Unlock()
	event, allowed := spec.Event(digit)
	if !allowed || target == nil || b.ctx.Err() != nil || b.media.stopped.Load() {
		b.writeMu.Unlock()
		return errors.New("telephone event unavailable")
	}
	timestamp := b.timestamp
	b.eventActive = true
	b.writeMu.Unlock()
	started := time.Now()
	defer func() {
		b.writeMu.Lock()
		b.timestamp = timestamp + max(uint32(time.Since(started).Milliseconds()*8), 1280)
		b.eventActive = false
		b.writeMu.Unlock()
	}()
	return rtpevent.Transmit(ctx, b.ctx.Done(), event, func(payload []byte, marker bool) error {
		b.writeMu.Lock()
		defer b.writeMu.Unlock()
		b.media.mu.Lock()
		current := revision == b.media.revision
		b.media.mu.Unlock()
		if !current || b.media.stopped.Load() {
			return errors.New("telephone event source changed")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.ctx.Err(); err != nil {
			return err
		}
		packet := make([]byte, 16)
		packet[0], packet[1] = 0x80, spec.Payload
		if marker {
			packet[1] |= 0x80
		}
		binary.BigEndian.PutUint16(packet[2:4], b.sequence)
		binary.BigEndian.PutUint32(packet[4:8], timestamp)
		binary.BigEndian.PutUint32(packet[8:12], b.ssrc)
		copy(packet[12:], payload)
		_, err := b.media.conn.WriteToUDP(packet, target)
		b.sequence++
		return err
	})
}
