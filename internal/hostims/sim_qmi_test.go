package hostims

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

type uimSender func(context.Context, byte, byte, uint16, []qmi.TLV) (*qmi.Packet, error)

func (f uimSender) SendRequest(ctx context.Context, service, cid byte, message uint16, tlvs []qmi.TLV) (*qmi.Packet, error) {
	return f(ctx, service, cid, message, tlvs)
}
func uimReply(message uint16, tlvs ...qmi.TLV) *qmi.Packet {
	return &qmi.Packet{ServiceType: qmi.ServiceUIM, ClientID: 7, MessageID: message, TLVs: append([]qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}, tlvs...)}
}
func slotStatusRaw(logical byte, iccid []byte) []byte {
	raw := make([]byte, 10)
	binary.LittleEndian.PutUint32(raw, 2)
	binary.LittleEndian.PutUint32(raw[4:], 1)
	raw[8] = logical
	raw[9] = byte(len(iccid))
	return append(raw, iccid...)
}
func TestCellularQMIMapsLogicalSubscriptionToExactPhysicalSIM(t *testing.T) {
	iccid := cardHex(t, "981000000000000000f1")
	raw := append([]byte{2}, slotStatusRaw(2, cardHex(t, "981000000000000000f2"))...)
	raw = append(raw, slotStatusRaw(1, iccid)...)
	physical, value, err := selectedUIMSlot(raw, 1, 2)
	if err != nil || physical != 2 || value != "8901000000000000001" {
		t.Fatal("wrong physical selection", err)
	}
	for n := 0; n < len(raw); n++ {
		if _, _, err := selectedUIMSlot(raw[:n], 1, 2); err == nil {
			t.Fatal("truncated slots accepted")
		}
	}
	for _, bad := range [][]byte{append(append([]byte(nil), raw...), 0), append(append([]byte{2}, slotStatusRaw(1, iccid)...), slotStatusRaw(1, cardHex(t, "981000000000000000f2"))...), append(append([]byte{2}, slotStatusRaw(1, iccid)...), slotStatusRaw(2, iccid)...)} {
		if _, _, err := selectedUIMSlot(bad, 1, 2); err == nil {
			t.Fatal("ambiguous mapping accepted")
		}
	}
	if _, _, err := selectedUIMSlot(raw, 1, 1); err == nil {
		t.Fatal("MM slot mismatch accepted")
	}
	if _, _, err := selectedUIMSlot(raw, 1, 0); err == nil {
		t.Fatal("unknown MM slot guessed for multi-slot hardware")
	}
}
func TestCellularQMIUsesOnlyOwnedChannelAndCID(t *testing.T) {
	steps := 0
	released := 0
	guarded := 0
	var authEncoded, rawResponse []byte
	sender := uimSender(func(_ context.Context, service, cid byte, message uint16, input []qmi.TLV) (*qmi.Packet, error) {
		if service != qmi.ServiceUIM || cid != 7 {
			t.Fatal("wrong CID")
		}
		steps++
		values := map[byte][]byte{}
		for _, tlv := range input {
			values[tlv.Type] = tlv.Value
		}
		switch steps {
		case 1:
			if message != 0x002f {
				t.Fatal("unexpected query")
			}
			return uimReply(message, qmi.TLV{Type: 0x10, Value: statusEnvelope(0, statusCard(uimApplication{Kind: 2, State: 7, AID: usimPrefix}, uimApplication{Kind: 5, State: 7, AID: isimPrefix}))}), nil
		case 2:
			if message != 0x0047 {
				t.Fatal("unexpected query")
			}
			return uimReply(message, qmi.TLV{Type: 0x10, Value: append([]byte{1}, slotStatusRaw(1, cardHex(t, "981000000000000000f1"))...)}), nil
		case 3:
			if message != 0x0042 || !bytes.Equal(values[1], []byte{1}) || !bytes.Equal(values[0x10], append([]byte{7}, isimPrefix...)) {
				t.Fatal("wrong channel open")
			}
			return uimReply(message, qmi.TLV{Type: 0x10, Value: []byte{4}}, qmi.TLV{Type: 0x11, Value: []byte{0x90, 0}}), nil
		case 4:
			if message != 0x003b || !bytes.Equal(values[0x10], []byte{4}) || binary.LittleEndian.Uint16(values[2]) != uint16(len(values[2])-2) || values[2][2] != 0x40 {
				t.Fatal("wrong APDU framing")
			}
			authEncoded = values[2]
			rawResponse = []byte{2, 0, 0x90, 0}
			return uimReply(message, qmi.TLV{Type: 0x10, Value: rawResponse}), nil
		case 5:
			if message != 0x003f || !bytes.Equal(values[0x11], []byte{4}) || len(values) != 3 || !bytes.Equal(values[0x13], []byte{0}) {
				t.Fatal("wrong owned channel close")
			}
			return uimReply(message), nil
		default:
			t.Fatal("unexpected I/O")
		}
		return nil, nil
	})
	session, err := newQMIUIMSession(sender, 7, func(context.Context) error { guarded++; return nil }, func() error { released++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	card, iccid, err := session.Card(context.Background(), cardBinding{ICCID: "8901000000000000001", Slot: 1})
	if err != nil || iccid == "" || !card.HasISIM {
		t.Fatal("card proof failed", err)
	}
	if _, err := session.Open(context.Background(), 1, isimPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := session.APDU(context.Background(), 1, 3, []byte{3, 0x88, 0, 0x81}); err == nil {
		t.Fatal("unowned channel accepted")
	}
	if err := session.Close(); !errors.Is(err, ErrUncertain) || released != 0 {
		t.Fatal("CID released with live channel")
	}
	response, err := session.APDU(context.Background(), 1, 4, []byte{0x40, 0x88, 0, 0x81})
	if err != nil || !bytes.Equal(response, []byte{0x90, 0}) {
		t.Fatal("APDU failed", err)
	}
	for _, raw := range [][]byte{authEncoded, rawResponse} {
		for _, b := range raw {
			if b != 0 {
				t.Fatal("borrowed APDU buffer retained")
			}
		}
	}
	if err := session.CloseChannel(context.Background(), 1, 4); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if steps != 5 || guarded != 5 || released != 1 {
		t.Fatal("unexpected lifecycle")
	}
}
func TestCellularQMIFallbackOnlyAfterExplicitUnsupportedSingleSlot(t *testing.T) {
	for _, mode := range []string{"unsupported", "transport", "malformed", "denied", "multi"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			sender := uimSender(func(_ context.Context, _, _ byte, message uint16, input []qmi.TLV) (*qmi.Packet, error) {
				switch message {
				case 0x002f:
					cards := [][]byte{statusCard(uimApplication{Kind: 2, State: 7, AID: usimPrefix})}
					if mode == "multi" {
						cards = append(cards, cards[0])
					}
					return uimReply(message, qmi.TLV{Type: 0x10, Value: statusEnvelope(0, cards...)}), nil
				case 0x0047:
					if mode == "transport" {
						return nil, errors.New("raw private failure")
					}
					if mode == "malformed" {
						return uimReply(message, qmi.TLV{Type: 0x10, Value: []byte{1}}), nil
					}
					packet := uimReply(message)
					packet.TLVs[0].Value = []byte{1, 0, 0x5e, 0}
					if mode == "denied" {
						packet.TLVs[0].Value[2] = 0x52
					}
					return packet, nil
				case 0x0020:
					reads++
					if len(input) != 3 || !bytes.Equal(input[0].Value, []byte{6, 0}) || !bytes.Equal(input[1].Value, []byte{0xe2, 0x2f, 2, 0, 0x3f}) {
						t.Fatal("wrong EF.ICCID session/path")
					}
					return uimReply(message, qmi.TLV{Type: 0x10, Value: []byte{0x90, 0}}, qmi.TLV{Type: 0x11, Value: append([]byte{10, 0}, cardHex(t, "981000000000000000f1")...)}), nil
				}
				t.Fatal("unexpected mutation")
				return nil, nil
			})
			session, _ := newQMIUIMSession(sender, 7, func(context.Context) error { return nil }, func() error { return nil })
			_, _, err := session.Card(context.Background(), cardBinding{ICCID: "8901000000000000001", Slot: 1})
			if mode == "unsupported" {
				if err != nil || reads != 1 {
					t.Fatal("explicit fallback failed", err)
				}
			} else if err == nil || reads != 0 {
				t.Fatal("unsafe fallback")
			}
		})
	}
}
func TestCellularQMIMalformedMutationRetainsCID(t *testing.T) {
	for _, mode := range []string{"cid", "service", "message", "indication", "duplicate", "missing-result", "invalid-channel", "transport"} {
		t.Run(mode, func(t *testing.T) {
			released := 0
			calls := 0
			sender := uimSender(func(_ context.Context, _, _ byte, message uint16, _ []qmi.TLV) (*qmi.Packet, error) {
				calls++
				packet := uimReply(message, qmi.TLV{Type: 0x10, Value: []byte{4}})
				switch mode {
				case "cid":
					packet.ClientID++
				case "service":
					packet.ServiceType++
				case "message":
					packet.MessageID++
				case "indication":
					packet.IsIndication = true
				case "duplicate":
					packet.TLVs = append(packet.TLVs, qmi.TLV{Type: 2, Value: []byte{0, 0, 0, 0}})
				case "missing-result":
					packet.TLVs = packet.TLVs[1:]
				case "invalid-channel":
					packet.TLVs[1].Value = []byte{0}
				case "transport":
					return nil, errors.New("raw secret")
				}
				return packet, nil
			})
			session, _ := newQMIUIMSession(sender, 7, func(context.Context) error { return nil }, func() error { released++; return nil })
			session.card = uimCard{Slot: 1, USIM: usimPrefix}
			if _, err := session.Open(context.Background(), 1, usimPrefix); !errors.Is(err, ErrUncertain) {
				t.Fatal("unknown open accepted", err)
			}
			if !session.Uncertain() || session.Close() == nil || released != 0 || calls != 1 {
				t.Fatal("uncertain CID released or replayed")
			}
		})
	}
}

func TestCellularQMICloseCardFailureRetainsChannelAndCID(t *testing.T) {
	released := 0
	sender := uimSender(func(_ context.Context, _, _ byte, message uint16, _ []qmi.TLV) (*qmi.Packet, error) {
		return uimReply(message, qmi.TLV{Type: 0x11, Value: []byte{0x69, 0x85}}), nil
	})
	session, _ := newQMIUIMSession(sender, 7, func(context.Context) error { return nil }, func() error { released++; return nil })
	session.channels[4] = 1
	if err := session.CloseChannel(context.Background(), 1, 4); !errors.Is(err, ErrUncertain) {
		t.Fatal("card failed close accepted", err)
	}
	if session.channels[4] != 1 || !session.Uncertain() || session.Close() == nil || released != 0 {
		t.Fatal("failed close discarded channel ownership")
	}
}
