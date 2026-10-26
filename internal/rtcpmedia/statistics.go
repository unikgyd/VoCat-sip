package rtcpmedia

import (
	"encoding/binary"
	"math"
	"time"
)

type Statistics struct {
	LocalSSRC       uint32  `json:"local_ssrc"`
	RemoteSSRC      uint32  `json:"remote_ssrc"`
	PacketsSent     uint64  `json:"packets_sent"`
	PacketsReceived uint64  `json:"packets_received"`
	OctetsSent      uint64  `json:"octets_sent"`
	PacketsLost     int64   `json:"packets_lost"`
	Jitter          uint32  `json:"jitter"` // negotiated RTP clock units
	ReportsSent     uint64  `json:"reports_sent"`
	ReportsReceived uint64  `json:"reports_received"`
	InvalidReports  uint64  `json:"invalid_reports"`
	SendErrors      uint64  `json:"send_errors"`
	RoundTripMillis float64 `json:"round_trip_ms"`
	PeerLeft        bool    `json:"peer_left"`
}

type reception struct {
	epoch                                          uint64
	ssrc                                           uint32
	base, highest                                  uint16
	cycles, received, expectedPrior, receivedPrior uint64
	bad                                            uint32
	arrival                                        time.Time
	transit                                        int32
	jitter                                         float64
}

func (r *reception) expected() uint64 { return r.cycles + uint64(r.highest) - uint64(r.base) + 1 }

func (r *reception) sequence(seq uint16) bool {
	delta := uint16(seq - r.highest)
	if delta < 3000 {
		if seq < r.highest {
			r.cycles += 65536
		}
		r.highest = seq
	} else if delta <= 65536-100 {
		if uint32(seq) != r.bad {
			r.bad = uint32(seq + 1)
			return false
		}
		ssrc, epoch := r.ssrc, r.epoch+1
		*r = reception{ssrc: ssrc, epoch: epoch, base: seq, highest: seq, bad: 65537}
	}
	r.received++ // RFC 3550 includes accepted late and duplicate packets.
	return true
}

func ntp(t time.Time) uint64 {
	return uint64(t.Unix()+2208988800)<<32 | uint64(t.Nanosecond())*(1<<32)/1e9
}

func clockTicks(elapsed time.Duration, clock uint32) uint32 {
	elapsed = max(elapsed, 0)
	return uint32(uint64(elapsed/time.Second)*uint64(clock) + uint64(elapsed%time.Second)*uint64(clock)/uint64(time.Second))
}

func (s *Session) ReceivedRTP(revision uint64, header []byte, payloadOctets int) {
	if len(header) < 12 || payloadOctets < 1 {
		return
	}
	now := time.Now()
	ssrc, seq := binary.BigEndian.Uint32(header[8:12]), binary.BigEndian.Uint16(header[2:4])
	stamp := binary.BigEndian.Uint32(header[4:8])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != revision || s.remote == nil {
		return
	}
	if s.rx == nil {
		s.rx = &reception{ssrc: ssrc, epoch: 1, base: seq, highest: seq, bad: 65537}
	} else if s.rx.ssrc != ssrc {
		// A single stray SSRC cannot replace the active receive source.
		if !s.candidateValid || s.candidateSSRC != ssrc || seq != s.candidateSequence+1 {
			s.candidateSSRC, s.candidateSequence = ssrc, seq
			s.candidateValid = true
			return
		}
		s.rx = &reception{ssrc: ssrc, epoch: 1, base: seq, highest: seq, bad: 65537}
		s.lastSR, s.lastSRArrival = 0, time.Time{}
	}
	s.candidateValid = false
	r := s.rx
	if !r.sequence(seq) {
		return
	}
	transit := int32(clockTicks(now.Sub(s.origin), s.clock) - stamp)
	if !r.arrival.IsZero() {
		d := int64(int32(transit - r.transit))
		if d < 0 {
			d = -d
		}
		r.jitter += (float64(d) - r.jitter) / 16
	}
	r.transit, r.arrival = transit, now
	s.stats.PacketsReceived++
	s.stats.PeerLeft = false
	if s.pendingSRValid && s.pendingSRSource == ssrc {
		s.lastSR, s.lastSRArrival = s.pendingSR, s.pendingSRArrival
		s.pendingSRValid = false
	}
}

func (s *Session) SentRTP(header []byte, payloadOctets int) {
	if len(header) < 12 || payloadOctets < 1 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ssrc := binary.BigEndian.Uint32(header[8:12])
	if s.localSSRC != ssrc {
		s.localSSRC, s.sentPackets, s.sentOctets = ssrc, 0, 0
		s.sentSR = make(map[uint32]time.Time)
	}
	s.sentPackets++
	s.sentOctets += uint64(payloadOctets)
	s.lastSentStamp, s.lastSentAt = binary.BigEndian.Uint32(header[4:8]), time.Now()
	s.stats.PacketsSent++
	s.stats.OctetsSent += uint64(payloadOctets)
}

func (s *Session) Statistics() Statistics {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.stats
	result.LocalSSRC = s.localSSRC
	if r := s.rx; r != nil {
		result.RemoteSSRC = r.ssrc
		result.PacketsLost = int64(r.expected()) - int64(r.received)
		result.Jitter = uint32(math.Min(math.Max(r.jitter, 0), math.MaxUint32))
	}
	return result
}

type report struct {
	packet             []byte
	remoteRevision     uint64
	senderStamp        uint32
	expected, received uint64
	source             *reception
	sourceEpoch        uint64
	localSSRC          uint32
}

func (s *Session) reportLocked(now time.Time, bye bool) report {
	sender := !s.lastSentAt.IsZero() && now.Sub(s.lastSentAt) < 10*time.Second
	size, kind := 8, byte(201)
	if sender {
		size, kind = 28, 200
	}
	if s.rx != nil {
		size += 24
	}
	p := make([]byte, size)
	p[0], p[1] = 0x80, kind
	if s.rx != nil {
		p[0]++
	}
	binary.BigEndian.PutUint16(p[2:4], uint16(size/4-1))
	binary.BigEndian.PutUint32(p[4:8], s.localSSRC)
	value := report{packet: p, remoteRevision: s.revision, source: s.rx, localSSRC: s.localSSRC}
	if sender {
		timeStamp := ntp(now)
		binary.BigEndian.PutUint64(p[8:16], timeStamp)
		binary.BigEndian.PutUint32(p[16:20], s.lastSentStamp+clockTicks(now.Sub(s.lastSentAt), s.clock))
		binary.BigEndian.PutUint32(p[20:24], uint32(s.sentPackets))
		binary.BigEndian.PutUint32(p[24:28], uint32(s.sentOctets))
		value.senderStamp = uint32(timeStamp >> 16)
	}
	if r := s.rx; r != nil {
		value.sourceEpoch = r.epoch
		b := p[size-24:]
		binary.BigEndian.PutUint32(b[:4], r.ssrc)
		expected := r.expected()
		lost := int64(expected) - int64(r.received)
		interval := expected - r.expectedPrior
		missing := int64(interval) - int64(r.received-r.receivedPrior)
		if interval > 0 && missing > 0 {
			b[4] = byte(min(uint64(missing)*256/interval, 255))
		}
		lost = max(int64(-8388608), min(lost, int64(8388607)))
		b[5], b[6], b[7] = byte(lost>>16), byte(lost>>8), byte(lost)
		binary.BigEndian.PutUint32(b[8:12], uint32(r.cycles)+uint32(r.highest))
		binary.BigEndian.PutUint32(b[12:16], uint32(math.Min(math.Max(r.jitter, 0), math.MaxUint32)))
		binary.BigEndian.PutUint32(b[16:20], s.lastSR)
		if s.lastSR != 0 && !s.lastSRArrival.IsZero() {
			binary.BigEndian.PutUint32(b[20:24], uint32(max(now.Sub(s.lastSRArrival), 0).Seconds()*65536))
		}
		value.expected, value.received = expected, r.received
	}
	// Every outbound report is compound with one private, per-call CNAME.
	sdes := make([]byte, (4+4+2+len(s.cname)+1+3)&^3)
	sdes[0], sdes[1] = 0x81, 202
	binary.BigEndian.PutUint16(sdes[2:4], uint16(len(sdes)/4-1))
	binary.BigEndian.PutUint32(sdes[4:8], s.localSSRC)
	sdes[8], sdes[9] = 1, byte(len(s.cname))
	copy(sdes[10:], s.cname)
	value.packet = append(value.packet, sdes...)
	if bye {
		last := []byte{0x81, 203, 0, 1, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(last[4:], s.localSSRC)
		value.packet = append(value.packet, last...)
	}
	return value
}
