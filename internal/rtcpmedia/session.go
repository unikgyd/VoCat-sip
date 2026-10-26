package rtcpmedia

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

type Sender func(remote *net.UDPAddr, revision uint64, packet []byte) error

type Session struct {
	conn                       *net.UDPConn
	send                       Sender
	mu                         sync.Mutex
	remote                     *net.UDPAddr
	revision                   uint64
	clock                      uint32
	origin                     time.Time
	cname                      string
	localSSRC                  uint32
	rx                         *reception
	candidateSSRC              uint32
	candidateSequence          uint16
	candidateValid             bool
	sentPackets, sentOctets    uint64
	lastSentStamp              uint32
	lastSentAt                 time.Time
	lastSR                     uint32
	lastSRArrival              time.Time
	pendingSRSource, pendingSR uint32
	pendingSRArrival           time.Time
	pendingSRValid             bool
	sentSR                     map[uint32]time.Time
	stats                      Statistics
	wake                       chan struct{}
	stop                       chan struct{}
	reportDone, readDone       chan struct{}
	done                       chan struct{}
	close                      sync.Once
}

func New(conn *net.UDPConn, ssrc, clock uint32, send Sender) (*Session, error) {
	if conn == nil || clock == 0 {
		return nil, errors.New("RTCP requires an owned socket and RTP clock")
	}
	var random [12]byte
	if _, err := io.ReadFull(cryptorand.Reader, random[:]); err != nil {
		return nil, err
	}
	s := &Session{conn: conn, send: send, localSSRC: ssrc, clock: clock, origin: time.Now(),
		cname: hex.EncodeToString(random[:]) + "@vocat", sentSR: make(map[uint32]time.Time),
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), reportDone: make(chan struct{}), readDone: make(chan struct{})}
	if s.send == nil {
		s.send = func(remote *net.UDPAddr, _ uint64, p []byte) error {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, err := conn.WriteToUDP(p, remote)
			return err
		}
	}
	go s.read()
	go s.reports()
	return s, nil
}

// SetRemote performs no socket or route I/O and can join an atomic SDP commit.
// A sender callback must check revision under its transport's write fence.
func (s *Session) SetRemote(remote *net.UDPAddr, revision uint64) {
	s.mu.Lock()
	if remote == nil || s.Closing() {
		s.mu.Unlock()
		return
	}
	first := s.remote == nil
	changed := first || !sameEndpoint(s.remote, remote)
	copy := *remote
	copy.IP = append(net.IP(nil), remote.IP...)
	s.remote, s.revision = &copy, revision
	if changed {
		s.rx = nil
		s.candidateSSRC, s.candidateSequence = 0, 0
		s.candidateValid = false
		s.lastSR, s.lastSRArrival, s.pendingSRValid = 0, time.Time{}, false
		s.stats.PeerLeft = false
	}
	s.mu.Unlock()
	if first {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func sameEndpoint(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP) && a.Zone == b.Zone
}

func (s *Session) reports() {
	defer close(s.reportDone)
	select {
	case <-s.stop:
		return
	case <-s.wake:
	}
	initial := true
	for {
		base := 5 * time.Second
		if initial {
			base /= 2
		}
		// Randomized unicast minimum from RFC 3550's timer compensation.
		timer := time.NewTimer(time.Duration(float64(base) * (0.5 + rand.Float64()) / 1.21828))
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		s.sendReport(false)
		initial = false
	}
}

func (s *Session) sendReport(bye bool) {
	now := time.Now()
	s.mu.Lock()
	if s.remote == nil {
		s.mu.Unlock()
		return
	}
	remote := *s.remote
	value := s.reportLocked(now, bye)
	s.mu.Unlock()
	err := s.send(&remote, value.remoteRevision, value.packet)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.stats.SendErrors++
		return
	}
	if value.remoteRevision != s.revision {
		return
	}
	s.stats.ReportsSent++
	if s.rx == value.source && s.rx != nil && s.rx.epoch == value.sourceEpoch {
		s.rx.expectedPrior, s.rx.receivedPrior = value.expected, value.received
	}
	if value.senderStamp != 0 && value.localSSRC == s.localSSRC {
		for stamp, sent := range s.sentSR {
			if now.Sub(sent) > time.Minute || len(s.sentSR) >= 8 {
				delete(s.sentSR, stamp)
			}
		}
		s.sentSR[value.senderStamp] = now
	}
}

func (s *Session) read() {
	defer close(s.readDone)
	buffer := make([]byte, 8193)
	for {
		n, source, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		s.mu.Lock()
		if sameEndpoint(s.remote, source) {
			s.processLocked(buffer[:n], time.Now())
		}
		s.mu.Unlock()
	}
}

func (s *Session) Close() {
	s.close.Do(func() {
		close(s.stop)
		go func() {
			defer close(s.done)
			<-s.reportDone
			s.sendReport(true) // best effort; signaling remains the call authority
			_ = s.conn.Close()
			<-s.readDone
		}()
	})
}

func (s *Session) Done() <-chan struct{} { return s.done }

// Abort prevents any late callback from sending after a surrounding bearer or
// tunnel cleanup budget expires. It does not redirect or reopen the socket.
func (s *Session) Abort() { _ = s.conn.Close(); s.Close() }

func (s *Session) Closing() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

type controlPacket struct {
	kind, count byte
	body        []byte
}

func parseControl(raw []byte) ([]controlPacket, bool) {
	if len(raw) < 4 || len(raw) > 8192 || len(raw)%4 != 0 {
		return nil, false
	}
	var packets []controlPacket
	for offset := 0; offset < len(raw); {
		if len(packets) >= 32 || offset+4 > len(raw) || raw[offset]>>6 != 2 {
			return nil, false
		}
		p := raw[offset:]
		size := (int(binary.BigEndian.Uint16(p[2:4])) + 1) * 4
		if size < 4 || size > len(p) {
			return nil, false
		}
		body := p[:size]
		if p[0]&32 != 0 {
			padding := int(body[size-1])
			if offset+size != len(raw) || padding == 0 || padding%4 != 0 || padding > size-4 {
				return nil, false
			}
			body = body[:size-padding]
		}
		kind, count := p[1], p[0]&31
		minimum := 4
		switch kind {
		case 200:
			minimum = 28 + int(count)*24
		case 201:
			minimum = 8 + int(count)*24
		case 202:
			if !validSDES(body, int(count)) {
				return nil, false
			}
		case 203:
			minimum = 4 + int(count)*4
			if len(body) > minimum && (minimum >= len(body) || int(body[minimum])+minimum+1 > len(body)) {
				return nil, false
			}
		case 204, 205, 206:
			minimum = 12
		case 207:
			minimum = 8
		default:
			if kind < 192 || kind > 223 {
				return nil, false
			}
		}
		if len(body) < minimum {
			return nil, false
		}
		packets = append(packets, controlPacket{kind, count, body})
		offset += size
	}
	return packets, true
}

func validSDES(p []byte, count int) bool {
	offset := 4
	for chunk := 0; chunk < count; chunk++ {
		if offset+4 > len(p) {
			return false
		}
		offset += 4
		ended := false
		for offset < len(p) {
			kind := p[offset]
			offset++
			if kind == 0 {
				for offset%4 != 0 {
					if offset >= len(p) || p[offset] != 0 {
						return false
					}
					offset++
				}
				ended = true
				break
			}
			if offset >= len(p) {
				return false
			}
			size := int(p[offset])
			offset++
			if offset+size > len(p) {
				return false
			}
			offset += size
		}
		if !ended {
			return false
		}
	}
	return offset == len(p)
}

func (s *Session) processLocked(raw []byte, now time.Time) {
	packets, valid := parseControl(raw)
	if !valid {
		s.stats.InvalidReports++
		return
	}
	for _, packet := range packets {
		p, start := packet.body, 0
		switch packet.kind {
		case 200:
			source := binary.BigEndian.Uint32(p[4:8])
			stamp := uint32(binary.BigEndian.Uint64(p[8:16]) >> 16)
			if s.rx != nil && s.rx.ssrc == source {
				s.lastSR, s.lastSRArrival = stamp, now
			} else if s.rx == nil {
				s.pendingSRSource, s.pendingSR, s.pendingSRArrival, s.pendingSRValid = source, stamp, now, true
			}
			start = 28
		case 201:
			start = 8
		case 203:
			for i := 0; i < int(packet.count); i++ {
				if s.rx != nil && binary.BigEndian.Uint32(p[4+i*4:8+i*4]) == s.rx.ssrc {
					s.stats.PeerLeft = true
				}
			}
		}
		if start == 0 {
			continue
		}
		s.stats.ReportsReceived++
		for i := 0; i < int(packet.count); i++ {
			block := p[start+i*24 : start+(i+1)*24]
			if binary.BigEndian.Uint32(block[:4]) != s.localSSRC {
				continue
			}
			lsr := binary.BigEndian.Uint32(block[16:20])
			if sent, ok := s.sentSR[lsr]; ok && lsr != 0 {
				delay := time.Duration(float64(binary.BigEndian.Uint32(block[20:24])) / 65536 * float64(time.Second))
				rtt := now.Sub(sent) - delay
				if rtt >= 0 && rtt < time.Minute {
					s.stats.RoundTripMillis = float64(rtt) / float64(time.Millisecond)
				}
			}
		}
	}
}
