package hostims

import (
	"context"
	"encoding/binary"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// The SDK allocator retries transport failures. Ownership transitions must
// instead be single attempt and require an exact service/CID echo.
func ownedUIMCID(ctx context.Context, sender RequestSender, release byte) (byte, error) {
	return ownedServiceCID(ctx, sender, qmi.ServiceUIM, release)
}

func ownedServiceCID(ctx context.Context, sender RequestSender, service, release byte) (byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sender == nil || service != qmi.ServiceUIM && service != qmi.ServiceWDS && service != qmi.ServiceNAS || release == 255 {
		return 0, ErrParameters
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	message := uint16(0x0022)
	value := []byte{service}
	if release != 0 {
		message = 0x0023
		value = append(value, release)
	}
	packet, err := sender.SendRequest(ctx, qmi.ServiceControl, 0, message, []qmi.TLV{{Type: 1, Value: value}})
	if err != nil {
		return 0, ErrUncertain
	}
	defer clearProxyPacket(packet)
	values, err := controlResult(packet, message, true)
	if err != nil {
		return 0, err
	}
	info := values[1]
	if len(info) != 2 || info[0] != service || info[1] == 0 || info[1] == 255 || release != 0 && info[1] != release {
		return 0, ErrUncertain
	}
	return info[1], nil
}

func controlResult(packet *qmi.Packet, message uint16, mutation bool) (map[byte][]byte, error) {
	invalid := func() (map[byte][]byte, error) {
		if mutation {
			return nil, ErrUncertain
		}
		return nil, ErrTransport
	}
	if packet == nil || packet.ServiceType != qmi.ServiceControl || packet.ClientID != 0 || packet.MessageID != message || packet.IsIndication || len(packet.TLVs) > 32 {
		return invalid()
	}
	values := map[byte][]byte{}
	total := 0
	for _, tlv := range packet.TLVs {
		if _, exists := values[tlv.Type]; exists {
			return invalid()
		}
		total += len(tlv.Value)
		if total > 16384 {
			return invalid()
		}
		values[tlv.Type] = tlv.Value
	}
	if len(values[2]) != 4 {
		return invalid()
	}
	status, code := binary.LittleEndian.Uint16(values[2]), binary.LittleEndian.Uint16(values[2][2:])
	if status == 1 && code != 0 {
		return nil, ErrSIMApplication
	}
	if status != 0 || code != 0 {
		return invalid()
	}
	return values, nil
}
