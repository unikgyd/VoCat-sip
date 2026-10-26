package sipgateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/rtpevent"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

type carrierPCMBridge struct {
	ctx             context.Context
	cancel          context.CancelFunc
	media           *callMedia
	carrier         vowifi.CallMedia
	uplink          chan []int16 // at most 320 ms, old frames dropped rather than delayed
	errors          chan error
	sent            atomic.Uint64
	dropped         atomic.Uint64
	sequence        uint16
	timestamp, ssrc uint32
	writeMu, dtmfMu sync.Mutex
	eventActive     bool // writeMu: audio is discarded while sending an event
	carrierEvents   rtpevent.Spec
	sendCarrierDTMF func(context.Context, byte) error // bound before start, immutable
	digits          chan byte
	dtmfDropped     atomic.Uint64
	dtmfBusy        atomic.Bool
}

func newCarrierPCMBridge(ctx context.Context, media *callMedia, carrier vowifi.CallMedia) (*carrierPCMBridge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if media == nil || media.stopped.Load() {
		return nil, errors.New("SIP media is closed")
	}
	switch strings.ToUpper(carrier.Codec()) {
	case "PCMA", "PCMU", "AMR", "PCM":
	default:
		return nil, errors.New("unsupported carrier PCM media codec")
	}
	seed := make([]byte, 10)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &carrierPCMBridge{ctx: ctx, cancel: cancel, media: media, carrier: carrier, uplink: make(chan []int16, 16), errors: make(chan error, 1), digits: make(chan byte, 1),
		sequence: binary.BigEndian.Uint16(seed[:2]), timestamp: binary.BigEndian.Uint32(seed[2:6]), ssrc: binary.BigEndian.Uint32(seed[6:])}, nil
}
func (b *carrierPCMBridge) fail(err error) {
	select {
	case b.errors <- err:
	default:
	}
	b.cancel()
}
func (b *carrierPCMBridge) close() { b.cancel(); b.media.pcm.CompareAndSwap(b, nil) }
func (b *carrierPCMBridge) enqueue(samples []int16) {
	if b.ctx.Err() != nil {
		return
	}
	for len(samples) > 0 {
		size := min(len(samples), 160)
		frame := append([]int16(nil), samples[:size]...)
		samples = samples[size:]
		select {
		case b.uplink <- frame:
		default:
			select {
			case <-b.uplink:
				b.dropped.Add(1)
			default:
			}
			select {
			case b.uplink <- frame:
			default:
				b.dropped.Add(1)
			}
		}
	}
}
func (b *carrierPCMBridge) start() {
	if b.ctx.Err() != nil || b.media.stopped.Load() {
		b.close()
		return
	}
	b.media.pcm.Store(b)
	// close can race the backend's late Media result. Never attach a fresh PCM
	// consumer after that SIP RTP socket has already been released.
	if b.ctx.Err() != nil || b.media.stopped.Load() {
		b.close()
		return
	}
	b.startDTMF()
	go func() {
		next := time.Now()
		for {
			select {
			case <-b.ctx.Done():
				return
			case samples := <-b.uplink:
				if wait := time.Until(next); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-b.ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
				if b.ctx.Err() != nil {
					return
				}
				if err := b.carrier.WritePCM(samples); err != nil {
					b.fail(err)
					return
				}
				b.media.packets.Add(1)
				next = next.Add(time.Duration(len(samples)) * time.Second / 8000)
				if time.Since(next) > 100*time.Millisecond {
					next = time.Now()
				}
			}
		}
	}()
	go b.downlink()
}
func (b *carrierPCMBridge) downlink() {
	var pending []int16
	next := time.Now()
	first := true
	for {
		samples, err := b.carrier.ReadPCM(b.ctx)
		if err != nil {
			if b.ctx.Err() == nil {
				b.fail(err)
			}
			return
		}
		if len(samples) == 0 { // tolerate a short empty read without spinning
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
			continue
		}
		if len(samples) > 3200 {
			b.fail(errors.New("carrier PCM frame exceeds limit"))
			return
		}
		pending = append(pending, samples...)
		for len(pending) >= 160 {
			if wait := time.Until(next); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-b.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			if b.ctx.Err() != nil {
				return
			}
			b.writeMu.Lock()
			if b.eventActive {
				pending = nil
				next = time.Now()
				b.writeMu.Unlock()
				break
			}
			b.media.mu.Lock()
			var remote *net.UDPAddr
			if b.media.remote != nil {
				address := *b.media.remote
				remote = &address
			}
			format := b.media.format
			revision := b.media.revision
			b.media.mu.Unlock()
			if remote == nil {
				b.writeMu.Unlock()
				b.fail(errors.New("SIP media endpoint unavailable"))
				return
			}
			payload, err := ims.EncodeG711(pending[:160], format.codec)
			if err != nil {
				b.writeMu.Unlock()
				b.fail(err)
				return
			}
			packet := make([]byte, 12+len(payload))
			packet[0], packet[1] = 0x80, format.pt
			if first {
				packet[1] |= 0x80
				first = false
			}
			binary.BigEndian.PutUint16(packet[2:4], b.sequence)
			binary.BigEndian.PutUint32(packet[4:8], b.timestamp)
			binary.BigEndian.PutUint32(packet[8:12], b.ssrc)
			copy(packet[12:], payload)
			if err := b.media.writeRTP(packet, remote, revision); err != nil {
				b.writeMu.Unlock()
				b.fail(err)
				return
			}
			b.sequence++
			b.timestamp += 160
			b.writeMu.Unlock()
			b.sent.Add(1)
			pending = pending[160:]
			next = next.Add(20 * time.Millisecond)
			if time.Since(next) > 100*time.Millisecond {
				next = time.Now()
			}
		}
		pending = append([]int16(nil), pending...) // do not retain a large backend slice
	}
}
