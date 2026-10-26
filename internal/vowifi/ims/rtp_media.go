package ims

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/rtpevent"
	"vocat/internal/vowifi"
)

const (
	rtpClockRate     = 8000
	rtpPacketSamples = 160
)

type rtpMedia struct {
	conn           *net.UDPConn
	network        vowifi.CellularIMSNetwork
	lifetime       context.Context
	cancel         context.CancelFunc
	configureGate  chan struct{}
	route          vowifi.CellularMediaRoute // protected by mu; only changes with the remote endpoint
	cleanupMu      sync.Mutex
	retiredRoutes  []*cellularRTPRoute
	cleanupDone    chan struct{}
	cleanupRunning bool

	mu                  sync.RWMutex
	remote              *net.UDPAddr
	codec               string
	payloadType         byte
	amr                 amrCodec
	amrFactory          func() (amrCodec, error)
	amrAttempted        bool
	amrParams           amrParameters
	amrMode, amrRequest int
	amrSent             uint64
	offered             bool
	offeredAMR          bool
	dtmfPayload         byte
	dtmfEvents          uint16
	remoteRevision      uint64
	dtmfMu              sync.Mutex
	dtmfActive          bool // writeMu
	dtmfDownlink        chan dtmfKey
	dtmfReceiver        rtpevent.Receiver // receive goroutine only

	writeMu   sync.Mutex
	pending   []int16
	sequence  uint16
	timestamp uint32
	ssrc      uint32

	downlink chan []int16
	closed   chan struct{}
	close    sync.Once
}

func newRTPMedia(local net.IP) (*rtpMedia, error) {
	return newRTPMediaWithAMR(local, openNativeAMR)
}

func newRTPMediaWithAMR(local net.IP, factory func() (amrCodec, error)) (*rtpMedia, error) {
	return newAccessRTPMedia(context.Background(), local, nil, factory)
}

func newAccessRTPMedia(ctx context.Context, local net.IP, network vowifi.CellularIMSNetwork, factory func() (amrCodec, error)) (*rtpMedia, error) {
	var connection *net.UDPConn
	var err error
	if network == nil {
		// Keep VoWiFi's wildcard/socket-family behavior. Cellular must instead
		// bind its exact assigned source through the verified bearer hook.
		connection, err = net.ListenUDP("udp", &net.UDPAddr{IP: local})
	} else {
		connection, err = listenAccessUDP(ctx, local, 0, network)
	}
	if err != nil {
		return nil, fmt.Errorf("ims: open RTP socket: %w", err)
	}
	seed := make([]byte, 10)
	if _, err := io.ReadFull(cryptorand.Reader, seed); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("ims: initialize RTP state: %w", err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	media := &rtpMedia{
		conn: connection, sequence: binary.BigEndian.Uint16(seed[:2]),
		timestamp: binary.BigEndian.Uint32(seed[2:6]), ssrc: binary.BigEndian.Uint32(seed[6:]),
		downlink: make(chan []int16, 64), closed: make(chan struct{}),
		dtmfDownlink: make(chan dtmfKey, 16),
		amrFactory:   factory, amrRequest: 15,
		network: network, lifetime: lifetime, cancel: cancel, configureGate: make(chan struct{}, 1),
	}
	media.configureGate <- struct{}{}
	go media.receive()
	return media, nil
}

func (media *rtpMedia) Codec() string {
	media.mu.RLock()
	defer media.mu.RUnlock()
	return media.codec
}

func (media *rtpMedia) ensureAMR() error {
	media.mu.Lock()
	defer media.mu.Unlock()
	select {
	case <-media.closed:
		return io.EOF
	default:
	}
	if !media.amrAttempted {
		media.amrAttempted = true
		if media.amrFactory != nil {
			media.amr, _ = media.amrFactory()
		}
	}
	if media.amr == nil {
		return errAMRUnavailable
	}
	return nil
}

func (media *rtpMedia) ready() bool {
	select {
	case <-media.closed:
		return false
	default:
	}
	media.mu.RLock()
	defer media.mu.RUnlock()
	return media.remote != nil && media.codec != ""
}

func (media *rtpMedia) offerSDP(local net.IP) []byte {
	available := media.ensureAMR() == nil
	media.mu.Lock()
	media.offered = true
	media.offeredAMR = available
	media.mu.Unlock()
	if available {
		return media.buildSDP(local, "102 8 0 101", []string{"a=rtpmap:102 AMR/8000/1", "a=fmtp:102 octet-align=0;mode-change-capability=2", "a=rtpmap:8 PCMA/8000", "a=rtpmap:0 PCMU/8000", "a=rtpmap:101 telephone-event/8000", "a=fmtp:101 0-15"})
	}
	return media.buildSDP(local, "8 0 101", []string{
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000", "a=fmtp:101 0-15",
	})
}

func (media *rtpMedia) answerSDP(local net.IP) []byte {
	media.mu.RLock()
	codec, payload, params := media.codec, media.payloadType, media.amrParams
	dtmfPayload, dtmfEvents := media.dtmfPayload, media.dtmfEvents
	media.mu.RUnlock()
	if !supportedPCMCodec(codec) && codec != "AMR" {
		return media.offerSDP(local)
	}
	attributes := []string{
		fmt.Sprintf("a=rtpmap:%d %s/%d", payload, codec, rtpClockRate),
	}
	if codec == "AMR" {
		attributes = append(attributes, params.fmtp(payload))
	}
	formats := strconv.Itoa(int(payload))
	if dtmfEvents != 0 {
		formats += " " + strconv.Itoa(int(dtmfPayload))
		attributes = append(attributes, fmt.Sprintf("a=rtpmap:%d telephone-event/8000", dtmfPayload), fmt.Sprintf("a=fmtp:%d %s", dtmfPayload, dtmfFormat(dtmfEvents)))
	}
	return media.buildSDP(local, formats, attributes)
}

func (media *rtpMedia) buildSDP(local net.IP, formats string, attributes []string) []byte {
	if local == nil || local.IsUnspecified() {
		if udp, ok := media.conn.LocalAddr().(*net.UDPAddr); ok {
			local = udp.IP
		}
	}
	if local == nil || local.IsUnspecified() {
		local = net.IPv4zero
	}
	family := "IP4"
	if local.To4() == nil {
		family = "IP6"
	}
	port := media.conn.LocalAddr().(*net.UDPAddr).Port
	sessionID := time.Now().UnixNano()
	lines := []string{
		"v=0",
		fmt.Sprintf("o=- %d %d IN %s %s", sessionID, sessionID, family, local.String()),
		"s=VoCat",
		fmt.Sprintf("c=IN %s %s", family, local.String()),
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP %s", port, formats),
	}
	if attributes == nil {
		lines = append(lines,
			"a=rtpmap:8 PCMA/8000",
			"a=rtpmap:0 PCMU/8000",
		)
	} else {
		lines = append(lines, attributes...)
	}
	lines = append(lines, "a=ptime:20", "a=sendrecv", "")
	return []byte(strings.Join(lines, "\r\n"))
}

func (media *rtpMedia) configureRemote(body []byte) error {
	return media.configureRemoteContext(context.Background(), body)
}

func (media *rtpMedia) configureRemoteContext(ctx context.Context, body []byte) error {
	return media.configureRemoteWithCommit(ctx, body, nil)
}

// The optional gate arbitrates a dialog re-INVITE's CANCEL against the atomic
// media commit. It runs after route acquisition, never around network I/O.
func (media *rtpMedia) configureRemoteWithCommit(ctx context.Context, body []byte, commit func(func() error) error) error {
	ctx, release, err := media.beginRemoteChange(ctx)
	if err != nil {
		return err
	}
	defer release()
	address, port, formats, mappings, err := parseAudioSDP(body)
	if err != nil {
		return err
	}
	var codec string
	var payload byte
	var params amrParameters
	media.mu.RLock()
	offered, offeredAMR := media.offered, media.offeredAMR
	media.mu.RUnlock()
	for _, value := range formats {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed < 0 || parsed > 127 {
			continue
		}
		if offered && parsed != 0 && parsed != 8 && !(offeredAMR && parsed == 102) {
			continue
		}
		if name, supported := negotiatePCMCodec(parsed, mappings[parsed]); supported {
			codec, payload = name, byte(parsed)
			break
		}
		if parsed >= 96 && strings.EqualFold(strings.TrimSuffix(mappings[parsed], "/1"), "AMR/8000") && media.ensureAMR() == nil {
			parsedParams, parseErr := parseAMRParameters(body, byte(parsed))
			if parseErr != nil || offered && parsedParams.octet {
				continue
			}
			codec, payload, params = "AMR", byte(parsed), parsedParams
			break
		}
	}
	if codec == "" {
		return errors.New("ims: remote SDP has no supported two-way audio codec or AMR packet format")
	}
	dtmfPayload, dtmfEvents := negotiateDTMF(body, formats, mappings, offered)
	route, err := media.acquireRemoteRoute(ctx, address)
	if err != nil {
		return err
	}
	adopted := false
	defer func() {
		if !adopted && route != nil {
			media.retireCellularRoute(route)
		}
	}()
	var previous vowifi.CellularMediaRoute
	// A writer may be stalled in UDP send. Do not hold the transaction gate
	// while waiting for it: CANCEL must still win and retire this staged route.
	for !media.writeMu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	apply := func() error {
		media.mu.Lock()
		defer media.mu.Unlock()
		select {
		case <-media.closed:
			return io.EOF
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if media.codec != codec || media.payloadType != payload || media.remote == nil ||
			!media.remote.IP.Equal(address) || media.remote.Port != port {
			media.pending = nil
		}
		previous = media.route
		media.route = route
		media.remote = &net.UDPAddr{IP: address, Port: port}
		media.codec, media.payloadType = codec, payload
		media.dtmfPayload, media.dtmfEvents = dtmfPayload, dtmfEvents
		media.remoteRevision++
		media.amrParams = params
		media.amrMode = params.highest()
		media.amrRequest = 15
		media.amrSent = 0
		adopted = true
		return nil
	}
	if commit != nil {
		err = commit(apply)
	} else {
		err = apply()
	}
	media.writeMu.Unlock()
	if err != nil {
		return err
	}
	if previous != nil {
		media.retireCellularRoute(previous)
	}
	return nil
}

func parseAudioSDP(body []byte) (net.IP, int, []string, map[int]string, error) {
	selected, err := selectAudioSDP(body)
	if err != nil {
		return nil, 0, nil, nil, err
	}
	body = selected
	var sessionIP, mediaIP net.IP
	var port int
	var formats []string
	mappings := make(map[int]string)
	inAudio := false
	for _, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "m="):
			fields := strings.Fields(strings.TrimPrefix(line, "m="))
			inAudio = len(fields) >= 4 && strings.EqualFold(fields[0], "audio") && strings.HasPrefix(strings.ToUpper(fields[2]), "RTP/AVP")
			if inAudio {
				port, _ = strconv.Atoi(strings.Split(fields[1], "/")[0])
				formats = append([]string(nil), fields[3:]...)
			}
		case strings.HasPrefix(line, "c="):
			fields := strings.Fields(strings.TrimPrefix(line, "c="))
			if len(fields) >= 3 {
				ip := net.ParseIP(strings.Split(fields[2], "/")[0])
				if inAudio {
					mediaIP = ip
				} else {
					sessionIP = ip
				}
			}
		case inAudio && strings.HasPrefix(strings.ToLower(line), "a=rtpmap:"):
			fields := strings.Fields(line[len("a=rtpmap:"):])
			if len(fields) != 2 {
				return nil, 0, nil, nil, errors.New("ims: malformed RTP codec mapping")
			}
			pt, parseErr := strconv.Atoi(fields[0])
			if parseErr != nil || pt < 0 || pt > 127 {
				return nil, 0, nil, nil, errors.New("ims: invalid RTP payload assignment")
			}
			if previous, exists := mappings[pt]; exists && !strings.EqualFold(previous, fields[1]) {
				return nil, 0, nil, nil, errors.New("ims: conflicting RTP codec mappings")
			}
			mappings[pt] = fields[1]
		}
	}
	if mediaIP == nil {
		mediaIP = sessionIP
	}
	if mediaIP == nil || port < 1 || port > 65535 || len(formats) == 0 {
		return nil, 0, nil, nil, errors.New("ims: remote SDP has no usable audio endpoint")
	}
	return mediaIP, port, formats, mappings, nil
}

func supportedPCMCodec(codec string) bool {
	return codec == "PCMA" || codec == "PCMU"
}

// RTP/AVP static assignments cannot be redefined. A dynamic mapping must
// name an implemented codec, its actual clock and a single audio channel.
func negotiatePCMCodec(payload int, mapping string) (string, bool) {
	if mapping == "" {
		switch payload {
		case 0:
			return "PCMU", true
		case 8:
			return "PCMA", true
		default:
			return "", false
		}
	}
	parts := strings.Split(mapping, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return "", false
	}
	rate, err := strconv.Atoi(parts[1])
	if err != nil || rate != rtpClockRate {
		return "", false
	}
	if len(parts) == 3 {
		channels, err := strconv.Atoi(parts[2])
		if err != nil || channels != 1 {
			return "", false
		}
	}
	codec := strings.ToUpper(parts[0])
	if !supportedPCMCodec(codec) || payload < 0 || payload > 127 ||
		payload < 96 && !(payload == 0 && codec == "PCMU" || payload == 8 && codec == "PCMA") {
		return "", false
	}
	return codec, true
}

func (media *rtpMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-media.closed:
		return nil, io.EOF
	case samples := <-media.downlink:
		return samples, nil
	}
}

func (media *rtpMedia) WritePCM(samples []int16) error {
	media.writeMu.Lock()
	defer media.writeMu.Unlock()
	select {
	case <-media.closed:
		return io.EOF
	default:
	}
	if media.dtmfActive {
		return nil
	}
	media.mu.RLock()
	var remote *net.UDPAddr
	if media.remote != nil {
		copy := *media.remote
		remote = &copy
	}
	codec, payload := media.codec, media.payloadType
	media.mu.RUnlock()
	if remote == nil || !supportedPCMCodec(codec) && codec != "AMR" {
		return errors.New("ims: RTP media is not negotiated")
	}
	media.pending = append(media.pending, samples...)
	for len(media.pending) >= rtpPacketSamples {
		var encoded []byte
		if codec == "AMR" {
			media.mu.Lock()
			native, params := media.amr, media.amrParams
			mode := params.next(media.amrMode, media.amrRequest, media.amrSent)
			media.amrMode = mode
			media.amrSent++
			media.mu.Unlock()
			if native == nil {
				return errAMRUnavailable
			}
			frame, err := native.Encode(media.pending[:rtpPacketSamples], mode)
			if err != nil {
				return err
			}
			encoded, err = packAMR([][]byte{frame}, params.octet)
			if err != nil {
				return err
			}
		} else {
			encoded = make([]byte, rtpPacketSamples)
			for index, sample := range media.pending[:rtpPacketSamples] {
				if codec == "PCMA" {
					encoded[index] = linearToALaw(sample)
				} else {
					encoded[index] = linearToMuLaw(sample)
				}
			}
		}
		packet := make([]byte, 12+len(encoded))
		packet[0], packet[1] = 0x80, payload
		binary.BigEndian.PutUint16(packet[2:4], media.sequence)
		binary.BigEndian.PutUint32(packet[4:8], media.timestamp)
		binary.BigEndian.PutUint32(packet[8:12], media.ssrc)
		copy(packet[12:], encoded)
		if _, err := media.conn.WriteToUDP(packet, remote); err != nil {
			return fmt.Errorf("ims: send RTP: %w", err)
		}
		media.pending = media.pending[rtpPacketSamples:]
		media.sequence++
		media.timestamp += rtpPacketSamples
	}
	return nil
}

func (media *rtpMedia) receive() {
	packet := make([]byte, 2048)
	for {
		count, source, err := media.conn.ReadFromUDP(packet)
		if err != nil {
			return
		}
		media.mu.RLock()
		remote, codec, payload, native, params := media.remote, media.codec, media.payloadType, media.amr, media.amrParams
		dtmfPayload, dtmfEvents := media.dtmfPayload, media.dtmfEvents
		media.mu.RUnlock()
		isDTMF := dtmfEvents != 0 && count >= 12 && packet[1]&0x7f == dtmfPayload
		if remote == nil || !supportedPCMCodec(codec) && codec != "AMR" || !remote.IP.Equal(source.IP) || count < 12 || packet[0]>>6 != 2 || packet[1]&0x7f != payload && !isDTMF {
			continue
		}
		header := 12 + int(packet[0]&0x0f)*4
		if packet[0]&0x10 != 0 {
			if count < header+4 {
				continue
			}
			header += 4 + int(binary.BigEndian.Uint16(packet[header+2:header+4]))*4
		}
		if header >= count {
			continue
		}
		if packet[0]&0x20 != 0 {
			padding := int(packet[count-1])
			if padding == 0 || padding > count-header {
				continue
			}
			count -= padding
		}
		if isDTMF {
			if source.Port == remote.Port {
				media.receiveDTMF(packet[header:count], binary.BigEndian.Uint32(packet[8:12]), binary.BigEndian.Uint32(packet[4:8]), dtmfEvents)
			}
			continue
		}
		var samples []int16
		if codec == "AMR" {
			if native == nil {
				continue
			}
			frames, cmr, err := unpackAMR(packet[header:count], params.octet)
			if err != nil {
				continue
			}
			for _, frame := range frames {
				ft := frame[0] >> 3
				if ft <= 7 && !params.modes[ft] {
					samples = nil
					break
				}
				pcm, decodeErr := native.Decode(frame)
				if decodeErr != nil {
					samples = nil
					break
				}
				samples = append(samples, pcm...)
			}
			if len(samples) == 0 {
				continue
			}
			media.mu.Lock()
			if cmr <= 7 && params.modes[cmr] {
				media.amrRequest = int(cmr)
			}
			media.mu.Unlock()
		} else {
			samples = make([]int16, count-header)
			for index, encoded := range packet[header:count] {
				if codec == "PCMA" {
					samples[index] = aLawToLinear(encoded)
				} else {
					samples[index] = muLawToLinear(encoded)
				}
			}
		}
		// Learn symmetric RTP only after validating the negotiated codec.
		media.mu.Lock()
		if media.remote != remote {
			media.mu.Unlock()
			continue
		}
		remote.Port = source.Port
		media.mu.Unlock()
		for len(samples) > 0 {
			chunk := samples[:min(160, len(samples))]
			samples = samples[len(chunk):]
			select {
			case media.downlink <- chunk:
			default:
				// Keep real-time behavior by dropping the oldest queued packet.
				select {
				case <-media.downlink:
				default:
				}
				select {
				case media.downlink <- chunk:
				default:
				}
			}
		}
	}
}

func (media *rtpMedia) Close() error {
	media.close.Do(func() {
		close(media.closed)
		media.cancel()
		_ = media.conn.Close()
		media.mu.Lock()
		codec := media.amr
		media.amr = nil
		route := media.route
		media.route, media.remote = nil, nil
		media.mu.Unlock()
		if route != nil {
			media.retireCellularRoute(route)
		}
		if codec != nil {
			codec.Close()
		}
	})
	media.startCellularRouteCleanup()
	return media.cellularRouteCleanupError()
}

func linearToMuLaw(sample int16) byte {
	value := int(sample)
	sign := byte(0)
	if value < 0 {
		sign, value = 0x80, -value
		if value > 32767 {
			value = 32767
		}
	}
	value += 132
	if value > 32635 {
		value = 32635
	}
	exponent := 7
	for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (value >> (exponent + 3)) & 0x0f
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

func muLawToLinear(value byte) int16 {
	value = ^value
	magnitude := ((int(value)&0x0f)<<3 + 132) << ((value & 0x70) >> 4)
	magnitude -= 132
	if value&0x80 != 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}

func linearToALaw(sample int16) byte {
	value := int(sample)
	mask := byte(0xd5)
	if value < 0 {
		mask, value = 0x55, -value-1
	}
	if value > 32767 {
		value = 32767
	}
	var encoded byte
	if value < 256 {
		encoded = byte(value >> 4)
	} else {
		exponent := 1
		for threshold := 512; exponent < 7 && value >= threshold; threshold <<= 1 {
			exponent++
		}
		encoded = byte(exponent<<4) | byte((value>>(exponent+3))&0x0f)
	}
	return encoded ^ mask
}

func aLawToLinear(value byte) int16 {
	value ^= 0x55
	magnitude := int(value&0x0f)<<4 + 8
	exponent := int((value & 0x70) >> 4)
	if exponent != 0 {
		magnitude = (magnitude + 0x100) << (exponent - 1)
	}
	if value&0x80 == 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}
