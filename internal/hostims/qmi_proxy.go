package hostims

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// This deliberately small, serialized proxy transport has no CTL Sync,
// implicit CID allocation, retransmission, raw-device fallback or RF API.
// Connections use only owned UIM/WDS/NAS CIDs. Every response matches all headers.
type proxyReply struct {
	packet *qmi.Packet
	err    error
}
type proxyPending struct {
	service, cid         byte
	message, transaction uint16
	reply                chan proxyReply
}
type proxyQMI struct {
	connection   net.Conn
	gate         chan struct{}
	mu           sync.Mutex
	pending      *proxyPending
	ctl, service uint16
	broken       bool
	closed       atomic.Bool
	wdsEvents    atomic.Uint64
	nasEvents    atomic.Uint64
	readDone     chan struct{}
	closeOnce    sync.Once
	uimCloses    []ownedUIMClose
}

func newProxyQMI(connection net.Conn) *proxyQMI {
	p := &proxyQMI{connection: connection, gate: make(chan struct{}, 1), readDone: make(chan struct{})}
	p.gate <- struct{}{}
	go p.readLoop()
	return p
}
func (p *proxyQMI) healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.broken && !p.closed.Load()
}
func (p *proxyQMI) wdsRevision() uint64 { return p.wdsEvents.Load() }
func (p *proxyQMI) nasRevision() uint64 { return p.nasEvents.Load() }
func (p *proxyQMI) fail() {
	p.mu.Lock()
	p.broken = true
	if pending := p.pending; pending != nil {
		select {
		case pending.reply <- proxyReply{err: ErrUncertain}:
		default:
		}
	}
	p.mu.Unlock()
	_ = p.connection.Close()
}
func (p *proxyQMI) Close() error {
	p.closeOnce.Do(func() { p.closed.Store(true); p.fail() })
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-p.readDone:
		return nil
	case <-timer.C:
		return ErrUncertain
	}
}
func (p *proxyQMI) SendRequest(ctx context.Context, service, cid byte, message uint16, tlvs []qmi.TLV) (*qmi.Packet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.gate:
	}
	defer func() { p.gate <- struct{}{} }()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	p.mu.Lock()
	if p.broken || p.closed.Load() {
		p.mu.Unlock()
		return nil, ErrTransport
	}
	if service == qmi.ServiceControl {
		p.ctl++
		if p.ctl > 255 {
			p.mu.Unlock()
			p.fail()
			return nil, ErrTransport
		}
	} else {
		p.service++
		if p.service == 0 {
			p.mu.Unlock()
			p.fail()
			return nil, ErrTransport
		}
	}
	transaction := p.service
	if service == qmi.ServiceControl {
		transaction = p.ctl
	}
	pending := &proxyPending{service: service, cid: cid, message: message, transaction: transaction, reply: make(chan proxyReply, 1)}
	p.pending = pending
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.pending == pending {
			p.pending = nil
		}
		// Cancellation can win the select after the reader queued a response.
		// Erase that abandoned response rather than leaving AKA data buffered.
		select {
		case abandoned := <-pending.reply:
			clearProxyPacket(abandoned.packet)
		default:
		}
		p.mu.Unlock()
	}()
	frame, err := encodeProxyRequest(service, cid, message, transaction, tlvs)
	if err != nil {
		return nil, err
	}
	defer clear(frame)
	watchStop, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			p.fail()
		case <-watchStop:
		}
	}()
	defer func() { close(watchStop); <-watchDone }()
	deadline, _ := ctx.Deadline()
	if p.connection.SetWriteDeadline(deadline) != nil {
		p.fail()
		return nil, ErrTransport
	}
	for len(frame) > 0 {
		n, writeErr := p.connection.Write(frame)
		if writeErr != nil || n <= 0 {
			p.fail()
			return nil, ErrUncertain
		}
		frame = frame[n:]
	}
	select {
	case result := <-pending.reply:
		if ctx.Err() != nil {
			clearProxyPacket(result.packet)
			p.fail()
			return nil, ctx.Err()
		}
		return result.packet, result.err
	case <-ctx.Done():
		p.fail()
		return nil, ctx.Err()
	}
}
func encodeProxyRequest(service, cid byte, message, transaction uint16, tlvs []qmi.TLV) ([]byte, error) {
	if len(tlvs) > 32 {
		return nil, ErrParameters
	}
	size := 0
	for _, tlv := range tlvs {
		if len(tlv.Value) > 8192 {
			return nil, ErrParameters
		}
		size += 3 + len(tlv.Value)
	}
	if size > 16384 {
		return nil, ErrParameters
	}
	header := 7
	if service == qmi.ServiceControl {
		header = 6
	}
	frame := make([]byte, 6+header+size)
	frame[0] = 1
	binary.LittleEndian.PutUint16(frame[1:3], uint16(len(frame)-1))
	frame[4], frame[5] = service, cid
	if service == qmi.ServiceControl {
		frame[7] = byte(transaction)
		binary.LittleEndian.PutUint16(frame[8:10], message)
		binary.LittleEndian.PutUint16(frame[10:12], uint16(size))
	} else {
		binary.LittleEndian.PutUint16(frame[7:9], transaction)
		binary.LittleEndian.PutUint16(frame[9:11], message)
		binary.LittleEndian.PutUint16(frame[11:13], uint16(size))
	}
	offset := 6 + header
	for _, tlv := range tlvs {
		frame[offset] = tlv.Type
		binary.LittleEndian.PutUint16(frame[offset+1:offset+3], uint16(len(tlv.Value)))
		copy(frame[offset+3:], tlv.Value)
		offset += 3 + len(tlv.Value)
	}
	return frame, nil
}
func readProxyPacket(reader io.Reader) (*qmi.Packet, error) {
	header := make([]byte, 6)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, ErrTransport
	}
	total := int(binary.LittleEndian.Uint16(header[1:3])) + 1
	if header[0] != 1 || (header[3] != 0 && header[3] != 0x80) || total < 12 || total > 32768 {
		return nil, ErrTransport
	}
	frame := make([]byte, total)
	copy(frame, header)
	if _, err := io.ReadFull(reader, frame[6:]); err != nil {
		clear(frame)
		return nil, ErrTransport
	}
	packet, err := decodeProxyPacket(frame)
	if err != nil {
		clear(frame)
	}
	return packet, err
}
func decodeProxyPacket(frame []byte) (*qmi.Packet, error) {
	if len(frame) < 12 || len(frame) > 32768 || frame[0] != 1 || (frame[3] != 0 && frame[3] != 0x80) || int(binary.LittleEndian.Uint16(frame[1:3]))+1 != len(frame) {
		return nil, ErrTransport
	}
	p := &qmi.Packet{ServiceType: frame[4], ClientID: frame[5]}
	offset := 12
	flags := frame[6]
	var size uint16
	if p.ServiceType == qmi.ServiceControl {
		if flags != 1 && flags != 2 {
			return nil, ErrTransport
		}
		p.IsIndication = flags == 2
		p.TransactionID = uint16(frame[7])
		p.MessageID = binary.LittleEndian.Uint16(frame[8:10])
		size = binary.LittleEndian.Uint16(frame[10:12])
	} else {
		if len(frame) < 13 || flags != 2 && flags != 4 {
			return nil, ErrTransport
		}
		offset = 13
		p.IsIndication = flags == 4
		p.TransactionID = binary.LittleEndian.Uint16(frame[7:9])
		p.MessageID = binary.LittleEndian.Uint16(frame[9:11])
		size = binary.LittleEndian.Uint16(frame[11:13])
	}
	if int(size) != len(frame)-offset {
		return nil, ErrTransport
	}
	seen := map[byte]bool{}
	for offset < len(frame) {
		if len(frame)-offset < 3 || len(p.TLVs) >= 32 {
			return nil, ErrTransport
		}
		kind := frame[offset]
		n := int(binary.LittleEndian.Uint16(frame[offset+1 : offset+3]))
		offset += 3
		if n > len(frame)-offset || seen[kind] {
			return nil, ErrTransport
		}
		seen[kind] = true
		p.TLVs = append(p.TLVs, qmi.TLV{Type: kind, Value: frame[offset : offset+n]})
		offset += n
	}
	return p, nil
}
func clearProxyPacket(packet *qmi.Packet) {
	if packet != nil {
		for _, tlv := range packet.TLVs {
			clear(tlv.Value)
		}
	}
}
func (p *proxyQMI) readLoop() {
	defer close(p.readDone)
	unsolicited := 0
	for {
		packet, err := readProxyPacket(p.connection)
		if err != nil {
			p.fail()
			return
		}
		if packet.IsIndication {
			unsolicited++
			invalid := unsolicited > 64 || packet.ServiceType == qmi.ServiceControl && (packet.MessageID == 0x0024 || packet.MessageID == 0x0027) || packet.ServiceType == qmi.ServiceUIM && !p.consumeOwnedUIMClose(packet)
			if packet.ServiceType == qmi.ServiceWDS {
				p.wdsEvents.Add(1)
			}
			// Count every NAS indication, including an unknown vendor message.
			// Equality is only absence of OBSERVED changes, not a promise that
			// firmware reports every cell transition or never supplies caches.
			if packet.ServiceType == qmi.ServiceNAS {
				p.nasEvents.Add(1)
			}
			clearProxyPacket(packet)
			if invalid {
				p.fail()
				return
			}
			continue
		}
		unsolicited = 0
		p.mu.Lock()
		pending := p.pending
		matches := pending != nil && packet.ServiceType == pending.service && packet.ClientID == pending.cid && packet.MessageID == pending.message && packet.TransactionID == pending.transaction
		if matches {
			select {
			case pending.reply <- proxyReply{packet: packet}:
			default:
				matches = false
			}
		}
		p.mu.Unlock()
		if !matches {
			clearProxyPacket(packet)
			p.fail()
			return
		}
	}
}
