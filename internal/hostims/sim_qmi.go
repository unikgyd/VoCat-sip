package hostims

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// This adapter accepts only an already-owned UIM CID and an endpoint guard.
// Endpoint discovery/allocation belongs to the Linux opener, not the codec.
// Layouts: libqmi data/qmi-service-uim.json (Get Card/Slot Status, Logical
// Channel, Send APDU, Read Transparent). No SDK first-card selection helper.
type qmiUIMSession struct {
	mu                sync.Mutex
	sender            RequestSender
	cid               byte
	guard             func(context.Context) error
	release           func() error
	card              uimCard
	channels          map[byte]byte
	uncertain, closed bool
}

func newQMIUIMSession(sender RequestSender, cid byte, guard func(context.Context) error, release func() error) (*qmiUIMSession, error) {
	if sender == nil || cid == 0 || cid == 255 || guard == nil || release == nil {
		return nil, ErrSIM
	}
	return &qmiUIMSession{sender: sender, cid: cid, guard: guard, release: release, channels: map[byte]byte{}}, nil
}

// Caller holds mu. The result code is exposed only internally to distinguish
// an explicit unsupported Slot Status from transport/malformed failures.
func (s *qmiUIMSession) request(ctx context.Context, message uint16, input []qmi.TLV, mutation bool) (map[byte][]byte, uint16, error) {
	if s.closed || s.uncertain || ctx.Err() != nil {
		return nil, 0, ErrSIM
	}
	if s.guard(ctx) != nil {
		if len(s.channels) > 0 {
			s.uncertain = true
		}
		return nil, 0, ErrSIM
	}
	packet, err := s.sender.SendRequest(ctx, qmi.ServiceUIM, s.cid, message, input)
	invalid := err != nil || packet == nil || ctx.Err() != nil
	if !invalid {
		invalid = packet.ServiceType != qmi.ServiceUIM || packet.ClientID != s.cid || packet.MessageID != message || packet.IsIndication || len(packet.TLVs) > 32
	}
	values := map[byte][]byte{}
	if !invalid {
		total := 0
		for _, tlv := range packet.TLVs {
			if _, duplicate := values[tlv.Type]; duplicate {
				invalid = true
				break
			}
			total += len(tlv.Value)
			if total > 16384 {
				invalid = true
				break
			}
			values[tlv.Type] = tlv.Value
		}
		invalid = invalid || len(values[2]) != 4
	}
	if invalid {
		if packet != nil {
			for _, tlv := range packet.TLVs {
				clear(tlv.Value)
			}
		}
		if mutation {
			s.uncertain = true
			return nil, 0, ErrUncertain
		}
		return nil, 0, ErrSIM
	}
	status, code := binary.LittleEndian.Uint16(values[2]), binary.LittleEndian.Uint16(values[2][2:])
	if status == 1 && code != 0 {
		clearUIMValues(values)
		return nil, code, ErrSIMApplication
	}
	if status != 0 || code != 0 {
		clearUIMValues(values)
		if mutation {
			s.uncertain = true
			return nil, 0, ErrUncertain
		}
		return nil, 0, ErrSIM
	}
	return values, 0, nil
}

func clearUIMValues(values map[byte][]byte) {
	for _, value := range values {
		clear(value)
	}
}

func (s *qmiUIMSession) Card(ctx context.Context, binding cardBinding) (uimCard, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, _, err := s.request(ctx, 0x002f, nil, false)
	if err != nil {
		return uimCard{}, "", err
	}
	defer clearUIMValues(values)
	card, err := parseUIMCardStatus(values[0x10])
	if err != nil {
		return uimCard{}, "", err
	}
	slots, code, err := s.request(ctx, 0x0047, nil, false)
	defer clearUIMValues(slots)
	var iccid string
	if err == nil {
		var physical byte
		physical, iccid, err = selectedUIMSlot(slots[0x10], card.Slot, binding.Slot)
		card.Slot = physical
	} else if (code == 0x5e || code == 0x47) && card.Count == 1 && card.Slot == 1 && binding.Slot <= 1 {
		// Only a confirmed unsupported query on a single-card subscription may
		// use the explicit slot-1 EF.ICCID read. Never retry after transport loss.
		values, _, err = s.request(ctx, 0x0020, []qmi.TLV{{Type: 1, Value: []byte{6, 0}}, {Type: 2, Value: []byte{0xe2, 0x2f, 2, 0, 0x3f}}, {Type: 3, Value: []byte{0, 0, 10, 0}}}, false)
		defer clearUIMValues(values)
		if err == nil {
			data := values[0x11]
			if !bytes.Equal(values[0x10], []byte{0x90, 0}) || len(data) != 12 || binary.LittleEndian.Uint16(data) != 10 {
				err = ErrSIMIdentity
			} else {
				iccid, err = decodeCardICCID(data[2:])
			}
		}
	}
	if err != nil || iccid != binding.ICCID {
		return uimCard{}, "", ErrSIM
	}
	s.card = card
	return card, iccid, nil
}

// Slot Status maps the GW-primary logical subscription to a physical socket.
// ICCID and ModemManager's primary physical slot must agree. Nothing switches.
func selectedUIMSlot(raw []byte, logical byte, expectedPhysical uint32) (byte, string, error) {
	if len(raw) < 1 || raw[0] < 1 || raw[0] > 5 {
		return 0, "", ErrSIM
	}
	count, offset := int(raw[0]), 1
	seenLogical := map[byte]bool{}
	seenICCID := map[string]bool{}
	var selected byte
	var selectedICCID string
	for i := 1; i <= count; i++ {
		if len(raw)-offset < 10 {
			return 0, "", ErrSIM
		}
		present, active, mapped, n := binary.LittleEndian.Uint32(raw[offset:]), binary.LittleEndian.Uint32(raw[offset+4:]), raw[offset+8], int(raw[offset+9])
		offset += 10
		if present > 2 || active > 1 || n > 10 || len(raw)-offset < n {
			return 0, "", ErrSIM
		}
		var iccid string
		if present == 2 {
			var err error
			iccid, err = decodeCardICCID(raw[offset : offset+n])
			if err != nil || seenICCID[iccid] {
				return 0, "", ErrSIM
			}
			seenICCID[iccid] = true
		}
		offset += n
		if active == 1 {
			if mapped < 1 || mapped > 5 || seenLogical[mapped] {
				return 0, "", ErrSIM
			}
			seenLogical[mapped] = true
			if mapped == logical {
				if present != 2 || expectedPhysical != uint32(i) && !(expectedPhysical == 0 && count == 1) {
					return 0, "", ErrSIM
				}
				selected, selectedICCID = byte(i), iccid
			}
		}
	}
	if offset != len(raw) || selected == 0 {
		return 0, "", ErrSIM
	}
	return selected, selectedICCID, nil
}

func (s *qmiUIMSession) Open(ctx context.Context, slot byte, aid []byte) (byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot == 0 || slot != s.card.Slot || len(s.channels) != 0 || !bytes.Equal(aid, s.card.USIM) && !(s.card.HasISIM && bytes.Equal(aid, s.card.ISIM)) {
		return 0, ErrSIMApplication
	}
	values, _, err := s.request(ctx, 0x003f, []qmi.TLV{{Type: 1, Value: []byte{slot}}, {Type: 0x10, Value: append([]byte{byte(len(aid))}, aid...)}}, true)
	defer clearUIMValues(values)
	if err != nil {
		return 0, err
	}
	channel := values[0x10]
	if len(channel) != 1 {
		s.uncertain = true
		return 0, ErrUncertain
	}
	if _, err := channelCLA(channel[0]); err != nil {
		s.uncertain = true
		return 0, ErrUncertain
	}
	s.channels[channel[0]] = slot
	// A successful QMI open can still carry a failed SELECT. Track the owned
	// channel before rejecting it so cleanup never loses its identity.
	if sw, ok := values[0x11]; ok && !bytes.Equal(sw, []byte{0x90, 0}) {
		s.uncertain = true
		return 0, ErrUncertain
	}
	return channel[0], nil
}
func (s *qmiUIMSession) APDU(ctx context.Context, slot, channel byte, command []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cla, err := channelCLA(channel)
	if err != nil || s.channels[channel] != slot || slot == 0 || len(command) < 4 || len(command) > 261 || command[0] != cla {
		return nil, ErrSIMApplication
	}
	encoded := make([]byte, 2+len(command))
	binary.LittleEndian.PutUint16(encoded, uint16(len(command)))
	copy(encoded[2:], command)
	defer clear(encoded)
	values, _, err := s.request(ctx, 0x003b, []qmi.TLV{{Type: 1, Value: []byte{slot}}, {Type: 2, Value: encoded}, {Type: 0x10, Value: []byte{channel}}}, true)
	defer clearUIMValues(values)
	if err != nil {
		return nil, err
	}
	response := values[0x10]
	defer clear(response)
	if len(response) < 4 || len(response) > 8194 || int(binary.LittleEndian.Uint16(response)) != len(response)-2 {
		s.uncertain = true
		return nil, ErrUncertain
	}
	return append([]byte(nil), response[2:]...), nil
}
func (s *qmiUIMSession) CloseChannel(ctx context.Context, slot, channel byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot == 0 || s.channels[channel] != slot {
		return ErrSIMApplication
	}
	values, _, err := s.request(ctx, 0x003f, []qmi.TLV{{Type: 1, Value: []byte{slot}}, {Type: 0x11, Value: []byte{channel}}, {Type: 0x13, Value: []byte{1}}}, true)
	defer clearUIMValues(values)
	if err != nil {
		s.uncertain = true
		return ErrUncertain
	}
	if sw, ok := values[0x11]; ok && !bytes.Equal(sw, []byte{0x90, 0}) {
		s.uncertain = true
		return ErrUncertain
	}
	delete(s.channels, channel)
	return nil
}
func (s *qmiUIMSession) Uncertain() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.uncertain }
func (s *qmiUIMSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.uncertain || len(s.channels) > 0 {
		return ErrUncertain
	}
	if s.release() != nil {
		s.uncertain = true
		return ErrUncertain
	}
	s.closed = true
	return nil
}
