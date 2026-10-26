package sipgateway

import (
	"bufio"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/rtcpmedia"
	"vocat/internal/rtpevent"
	"vocat/internal/vowifi/ims"
)

type audioFormat struct {
	codec string
	pt    byte
}
type sdpBlock struct {
	kind, proto           string
	port                  int
	formats               []string
	connection, direction string
	maps                  map[byte]string
	fmtps                 map[byte][]string
	controlAttribute      string
	controlSeen, muxOnly  bool
}
type audioDescription struct {
	blocks           []sdpBlock
	selected         int
	format           audioFormat
	port             int
	events           rtpevent.Spec
	controlAttribute string
}

func parseCallSDP(body []byte) (audioDescription, error) {
	bad := func() (audioDescription, error) {
		return audioDescription{}, errors.New("unsupported or malformed audio SDP")
	}
	if len(body) == 0 || len(body) > 64*1024 {
		return bad()
	}
	blocks := []sdpBlock{}
	connection, direction := "", "sendrecv"
	lines := 0
	version := false
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lines++
		if lines > 1024 {
			return bad()
		}
		if lines == 1 && line != "v=0" {
			return bad()
		}
		if line == "v=0" {
			if version {
				return bad()
			}
			version = true
		}
		if strings.HasPrefix(line, "m=") {
			fields := strings.Fields(line[2:])
			if len(fields) < 4 || len(fields) > 35 || len(blocks) >= 16 {
				return bad()
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil || port < 0 || port > 65535 {
				return bad()
			}
			blocks = append(blocks, sdpBlock{kind: fields[0], proto: fields[2], port: port, formats: fields[3:], connection: connection, direction: direction, maps: map[byte]string{}, fmtps: map[byte][]string{}})
			continue
		}
		if strings.HasPrefix(line, "c=") {
			if len(blocks) == 0 {
				connection = line[2:]
			} else {
				blocks[len(blocks)-1].connection = line[2:]
			}
			continue
		}
		if strings.HasPrefix(line, "a=rtcp:") {
			if len(blocks) == 0 {
				return bad()
			}
			block := &blocks[len(blocks)-1]
			if block.controlSeen {
				return bad()
			}
			block.controlSeen, block.controlAttribute = true, strings.TrimSpace(strings.TrimPrefix(line, "a=rtcp:"))
			if block.controlAttribute == "" {
				return bad()
			}
		}
		if line == "a=rtcp-mux-only" && len(blocks) > 0 {
			blocks[len(blocks)-1].muxOnly = true
		}
		if line == "a=sendrecv" || line == "a=sendonly" || line == "a=recvonly" || line == "a=inactive" {
			if len(blocks) == 0 {
				direction = line[2:]
			} else {
				blocks[len(blocks)-1].direction = line[2:]
			}
			continue
		}
		if strings.HasPrefix(line, "a=rtpmap:") && len(blocks) > 0 {
			fields := strings.Fields(line[len("a=rtpmap:"):])
			if len(fields) != 2 {
				return bad()
			}
			pt, err := strconv.Atoi(fields[0])
			if err != nil || pt < 0 || pt > 127 {
				return bad()
			}
			block := &blocks[len(blocks)-1]
			if _, exists := block.maps[byte(pt)]; exists {
				return bad()
			}
			block.maps[byte(pt)] = strings.ToUpper(fields[1])
		}
		if strings.HasPrefix(line, "a=fmtp:") && len(blocks) > 0 {
			value := strings.TrimSpace(line[len("a=fmtp:"):])
			fields := strings.Fields(value)
			if len(fields) > 0 {
				pt, err := strconv.Atoi(fields[0])
				if err == nil && pt >= 0 && pt <= 127 {
					block := &blocks[len(blocks)-1]
					block.fmtps[byte(pt)] = append(block.fmtps[byte(pt)], strings.TrimSpace(strings.TrimPrefix(value, fields[0])))
				}
			}
		}
	}
	if scanner.Err() != nil || !version {
		return bad()
	}
	for index, block := range blocks {
		if block.kind != "audio" || block.port == 0 || block.proto != "RTP/AVP" {
			continue
		}
		if block.direction != "sendrecv" {
			return bad()
		}
		fields := strings.Fields(block.connection)
		if len(fields) != 3 || fields[0] != "IN" {
			return bad()
		}
		ip, err := netip.ParseAddr(fields[2])
		if err != nil || ip.IsUnspecified() || ip.IsMulticast() || fields[1] != "IP4" && fields[1] != "IP6" || fields[1] == "IP4" && !ip.Is4() || fields[1] == "IP6" && !ip.Is6() {
			return bad()
		}
		if _, err := rtcpmedia.Endpoint(block.controlAttribute, net.IP(ip.AsSlice()), block.port); err != nil || block.muxOnly {
			return bad()
		}
		if mapping, exists := block.maps[0]; exists && mapping != "PCMU/8000" && mapping != "PCMU/8000/1" {
			return bad()
		}
		if mapping, exists := block.maps[8]; exists && mapping != "PCMA/8000" && mapping != "PCMA/8000/1" {
			return bad()
		}
		seen := map[byte]bool{}
		for _, token := range block.formats {
			pt, err := strconv.Atoi(token)
			if err != nil || pt < 0 || pt > 127 || seen[byte(pt)] {
				return bad()
			}
			seen[byte(pt)] = true
		}
		for _, token := range block.formats {
			pt, _ := strconv.Atoi(token)
			mapping := block.maps[byte(pt)]
			if mapping == "" {
				if pt == 0 {
					mapping = "PCMU/8000"
				}
				if pt == 8 {
					mapping = "PCMA/8000"
				}
			}
			codec := ""
			if mapping == "PCMU/8000" || mapping == "PCMU/8000/1" {
				codec = "PCMU"
			}
			if mapping == "PCMA/8000" || mapping == "PCMA/8000/1" {
				codec = "PCMA"
			}
			if codec != "" {
				if pt != 0 && pt != 8 && pt < 96 {
					return bad()
				}
				return audioDescription{blocks: blocks, selected: index, format: audioFormat{codec: codec, pt: byte(pt)}, port: block.port, events: rtpevent.Negotiate(block.formats, block.maps, block.fmtps), controlAttribute: block.controlAttribute}, nil
			}
		}
		return bad() // later audio blocks cannot override the selected one
	}
	return bad()
}
func sdpHeader(ip net.IP) string {
	family := "IP4"
	if ip.To4() == nil {
		family = "IP6"
	}
	return fmt.Sprintf("v=0\r\no=vocat 1 1 IN %s %s\r\ns=VoCat\r\nt=0 0\r\nc=IN %s %s\r\n", family, ip, family, ip)
}
func makeCallOffer(address *net.UDPAddr, control ...*net.UDPAddr) []byte {
	return []byte(sdpHeader(address.IP) + fmt.Sprintf("m=audio %d RTP/AVP 8 0 101\r\n", address.Port) + callControlAttribute(address, control) + "a=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-15\r\na=sendrecv\r\na=ptime:20\r\n")
}
func makeCallAnswer(offer audioDescription, address *net.UDPAddr, control ...*net.UDPAddr) []byte {
	var result strings.Builder
	result.WriteString(sdpHeader(address.IP))
	for index, block := range offer.blocks {
		if index == offer.selected {
			fmt.Fprintf(&result, "m=audio %d RTP/AVP %d", address.Port, offer.format.pt)
			if offer.events.Mask != 0 {
				fmt.Fprintf(&result, " %d", offer.events.Payload)
			}
			fmt.Fprintf(&result, "\r\na=rtpmap:%d %s/8000\r\n", offer.format.pt, offer.format.codec)
			result.WriteString(callControlAttribute(address, control))
			if offer.events.Mask != 0 {
				fmt.Fprintf(&result, "a=rtpmap:%d telephone-event/8000\r\na=fmtp:%d %s\r\n", offer.events.Payload, offer.events.Payload, rtpevent.Format(offer.events.Mask))
			}
			result.WriteString("a=sendrecv\r\na=ptime:20\r\n")
		} else {
			fmt.Fprintf(&result, "m=%s 0 %s %s\r\n", block.kind, block.proto, strings.Join(block.formats, " "))
		}
	}
	return []byte(result.String())
}

func callControlAttribute(address *net.UDPAddr, control []*net.UDPAddr) string {
	port, family := address.Port+1, "IP4"
	if len(control) > 0 && control[0] != nil {
		port = control[0].Port
	}
	if address.IP.To4() == nil {
		family = "IP6"
	}
	return fmt.Sprintf("a=rtcp:%d IN %s %s\r\n", port, family, address.IP)
}

type callMedia struct {
	conn         *net.UDPConn
	controlConn  *net.UDPConn
	control      *rtcpmedia.Session
	mu           sync.Mutex
	remote       *net.UDPAddr
	format       audioFormat
	events       rtpevent.Spec
	eventReceipt rtpevent.Receiver // RTP reader only, never owned by a signaling worker
	revision     uint64
	latched      bool
	destination  atomic.Pointer[callMedia]
	pcm          atomic.Pointer[carrierPCMBridge]
	pcmSequence  uint16
	pcmSSRC      uint32
	pcmSeen      bool
	packets      atomic.Uint64
	closed       sync.Once
	stopped      atomic.Bool
	done         chan struct{}
}

func sourceLocalIP(peer string) (net.IP, error) {
	remote, err := net.ResolveUDPAddr("udp", peer)
	if err != nil {
		return nil, err
	}
	probe, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil, err
	}
	defer probe.Close()
	return append(net.IP(nil), probe.LocalAddr().(*net.UDPAddr).IP...), nil
}
func newCallMedia(peer string) (*callMedia, error) {
	local, err := sourceLocalIP(peer)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: local})
	if err != nil {
		return nil, err
	}
	controlConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: local})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var seed [4]byte
	if _, err := io.ReadFull(cryptorand.Reader, seed[:]); err != nil {
		_ = conn.Close()
		_ = controlConn.Close()
		return nil, err
	}
	media := &callMedia{conn: conn, controlConn: controlConn, done: make(chan struct{})}
	media.control, err = rtcpmedia.New(controlConn, binary.BigEndian.Uint32(seed[:]), 8000, func(remote *net.UDPAddr, revision uint64, packet []byte) error {
		media.mu.Lock()
		defer media.mu.Unlock()
		if media.revision != revision {
			return io.EOF
		}
		_ = controlConn.SetWriteDeadline(time.Now().Add(time.Second))
		_, err := controlConn.WriteToUDP(packet, remote)
		return err
	})
	if err != nil {
		_ = conn.Close()
		_ = controlConn.Close()
		return nil, err
	}
	go media.read()
	return media, nil
}
func (m *callMedia) configure(peer string, description audioDescription) error {
	source, err := net.ResolveUDPAddr("udp", peer)
	if err != nil {
		return err
	}
	controlRemote, err := rtcpmedia.Endpoint(description.controlAttribute, source.IP, description.port)
	if err != nil {
		return err
	}
	if controlRemote.IP.Equal(source.IP) {
		controlRemote.Zone = source.Zone
	}
	m.mu.Lock()
	if m.stopped.Load() {
		m.mu.Unlock()
		return io.EOF
	}
	m.remote = &net.UDPAddr{IP: source.IP, Zone: source.Zone, Port: description.port}
	m.format = description.format
	m.events = description.events
	m.revision++
	m.control.SetRemote(controlRemote, m.revision)
	m.latched = false
	m.mu.Unlock()
	return nil
}
func (m *callMedia) address() *net.UDPAddr        { return m.conn.LocalAddr().(*net.UDPAddr) }
func (m *callMedia) controlAddress() *net.UDPAddr { return m.controlConn.LocalAddr().(*net.UDPAddr) }
func (m *callMedia) close() {
	m.stopped.Store(true)
	m.closed.Do(func() {
		m.destination.Store(nil)
		if bridge := m.pcm.Swap(nil); bridge != nil {
			bridge.cancel()
		}
		m.conn.Close()
		m.control.Close()
		<-m.done
		<-m.control.Done()
	})
}

func rtpPayload(packet []byte, pt byte) (int, int, bool) {
	if len(packet) < 13 || len(packet) > 2048 || packet[0]>>6 != 2 || packet[1]&127 != pt {
		return 0, 0, false
	}
	header := 12 + int(packet[0]&15)*4
	if header > len(packet) {
		return 0, 0, false
	}
	if packet[0]&16 != 0 {
		if header+4 > len(packet) {
			return 0, 0, false
		}
		header += 4 + 4*int(binary.BigEndian.Uint16(packet[header+2:header+4]))
		if header > len(packet) {
			return 0, 0, false
		}
	}
	end := len(packet)
	if packet[0]&32 != 0 {
		padding := int(packet[len(packet)-1])
		if padding == 0 || padding > end-header {
			return 0, 0, false
		}
		end -= padding
	}
	return header, end, end > header && end-header <= 1600
}
func (m *callMedia) read() {
	defer close(m.done)
	buffer := make([]byte, 2049)
	for {
		n, source, err := m.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		packet := buffer[:n]
		m.mu.Lock()
		if m.remote == nil || !m.remote.IP.Equal(source.IP) || m.latched && m.remote.Port != source.Port {
			m.mu.Unlock()
			continue
		}
		isEvent := len(packet) >= 12 && m.events.Mask != 0 && packet[1]&127 == m.events.Payload
		pt := m.format.pt
		if isEvent {
			pt = m.events.Payload
		}
		start, end, valid := rtpPayload(packet, pt)
		if !valid {
			m.mu.Unlock()
			continue
		}
		if isEvent {
			// Telephone events cannot capture the symmetric RTP source latch.
			if source.Port != m.remote.Port || !m.events.Valid(packet[start:end]) {
				m.mu.Unlock()
				continue
			}
			spec := m.events
			pcm, other := m.pcm.Load(), m.destination.Load()
			m.control.ReceivedRTP(m.revision, packet[:12], end-start)
			m.mu.Unlock()
			digit, ok := m.eventReceipt.Accept(spec, packet[start:end], binary.BigEndian.Uint32(packet[8:12]), binary.BigEndian.Uint32(packet[4:8]))
			if pcm != nil {
				if ok {
					pcm.enqueueDTMF(digit)
				}
			} else if other != nil {
				m.forwardEvent(other, packet, start, end)
			}
			continue
		}
		if !m.latched {
			m.remote.Port = source.Port
			m.latched = true
		}
		from := m.format
		m.control.ReceivedRTP(m.revision, packet[:12], end-start)
		pcm := m.pcm.Load()
		if pcm != nil {
			sequence, ssrc := binary.BigEndian.Uint16(packet[2:4]), binary.BigEndian.Uint32(packet[8:12])
			if m.pcmSeen && ssrc == m.pcmSSRC && int16(sequence-m.pcmSequence) <= 0 {
				m.mu.Unlock()
				continue
			}
			m.pcmSequence, m.pcmSSRC, m.pcmSeen = sequence, ssrc, true
		}
		m.mu.Unlock()
		if pcm != nil {
			samples, err := ims.DecodeG711(packet[start:end], from.codec)
			if err == nil {
				pcm.enqueue(samples)
			}
			continue
		}
		other := m.destination.Load()
		if other == nil {
			continue
		}
		other.mu.Lock()
		if other.remote == nil {
			other.mu.Unlock()
			continue
		}
		target := *other.remote
		to := other.format
		revision := other.revision
		other.mu.Unlock()
		payload, err := ims.TranscodeG711(packet[start:end], from.codec, to.codec)
		if err != nil {
			continue
		}
		// Keep sequence, timestamp, SSRC and marker; strip padding/extensions/
		// CSRC which are not generated or interpreted by the media relay.
		out := make([]byte, 12+len(payload))
		copy(out, packet[:12])
		out[0] = 0x80
		out[1] = packet[1]&128 | to.pt
		copy(out[12:], payload)
		if err := other.writeRTP(out, &target, revision); err == nil {
			m.packets.Add(1)
		}
	}
}

func (m *callMedia) writeRTP(packet []byte, target *net.UDPAddr, revision uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped.Load() || m.revision != revision {
		return io.EOF
	}
	_ = m.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err := m.conn.WriteToUDP(packet, target)
	if err == nil {
		m.control.SentRTP(packet, len(packet)-12)
	}
	return err
}
