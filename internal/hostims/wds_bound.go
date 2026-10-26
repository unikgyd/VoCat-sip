package hostims

import (
	"context"
	"encoding/binary"
	"net/netip"
	"strings"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

var ErrWDSUnavailable = staticError("host IMS: owned WDS client cannot verify the selected connected IMS bearer")

// Endpoint and mux come from pinned kernel topology, not a profile/PDP index.
// This first Linux connector supports verified USB endpoints; native endpoint
// mappings must be implemented explicitly rather than guessing the USB type.
type wdsBinding struct {
	Endpoint, Interface uint32
	Mux                 byte
}

func (b wdsBinding) valid() bool { return b.Endpoint == 2 && b.Interface <= 255 && b.Mux != 255 }

func wdsResult(packet *qmi.Packet, cid byte, message uint16) (map[byte][]byte, error) {
	if packet == nil || packet.ServiceType != qmi.ServiceWDS || packet.ClientID != cid || packet.MessageID != message || packet.IsIndication || len(packet.TLVs) > 32 {
		return nil, ErrRuntime
	}
	values := make(map[byte][]byte)
	total := 0
	for _, tlv := range packet.TLVs {
		if _, exists := values[tlv.Type]; exists {
			return nil, ErrRuntime
		}
		total += len(tlv.Value)
		if total > 16384 {
			return nil, ErrRuntime
		}
		values[tlv.Type] = tlv.Value
	}
	result := values[2]
	if len(result) != 4 {
		return nil, ErrRuntime
	}
	status, code := binary.LittleEndian.Uint16(result), binary.LittleEndian.Uint16(result[2:])
	if status == 1 && code != 0 {
		return nil, ErrWDSUnavailable
	}
	if status != 0 || code != 0 {
		return nil, ErrRuntime
	}
	return values, nil
}

// Configure only this newly allocated CID, never MM's client. No Start/Stop,
// autoconnect, profile, attach, reset, or global IP-family mutation exists here.
func readBoundRuntime(ctx context.Context, sender RequestSender, cid byte, binding wdsBinding, family byte, verify func(context.Context) error) (Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sender == nil || cid == 0 || cid == 255 || !binding.valid() || family != 4 && family != 6 || verify == nil {
		return Runtime{}, ErrParameters
	}
	request := func(message uint16, input []qmi.TLV, mutation bool) (map[byte][]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := verify(ctx); err != nil {
			return nil, err
		}
		packet, err := sender.SendRequest(ctx, qmi.ServiceWDS, cid, message, input)
		if err != nil {
			if mutation {
				return nil, ErrUncertain
			}
			return nil, ErrRuntime
		}
		defer clearProxyPacket(packet)
		values, err := wdsResult(packet, cid, message)
		if err != nil {
			if mutation && err != ErrWDSUnavailable {
				return nil, ErrUncertain
			}
			return nil, err
		}
		if err := verify(ctx); err != nil {
			return nil, err
		}
		copy := make(map[byte][]byte, len(values))
		for kind, value := range values {
			copy[kind] = append([]byte(nil), value...)
		}
		return copy, nil
	}
	if binding.Mux != 0 {
		endpoint := make([]byte, 8)
		binary.LittleEndian.PutUint32(endpoint, binding.Endpoint)
		binary.LittleEndian.PutUint32(endpoint[4:], binding.Interface)
		// Omit Client Type: no claim to an internal/baseband client. MM also
		// identifies this data path by endpoint and mux alone.
		if _, err := request(0x00a2, []qmi.TLV{{Type: 0x10, Value: endpoint}, {Type: 0x11, Value: []byte{binding.Mux}}}, true); err != nil {
			return Runtime{}, err
		}
	}
	if _, err := request(0x004d, []qmi.TLV{{Type: 1, Value: []byte{family}}}, true); err != nil {
		return Runtime{}, err
	}
	status := func() error {
		values, err := request(0x0022, nil, false)
		if err != nil {
			return err
		}
		if len(values[1]) != 1 || values[1][0] != 2 {
			return ErrWDSUnavailable
		}
		return nil
	}
	if err := status(); err != nil {
		return Runtime{}, err
	}
	if err := verify(ctx); err != nil {
		return Runtime{}, err
	}
	mask := qmi.RuntimeMaskProfileID | qmi.RuntimeMaskAPNName | qmi.RuntimeMaskIPAddr | qmi.RuntimeMaskGateway | qmi.RuntimeMaskPCSCFPCO | qmi.RuntimeMaskPCSCFAddr | qmi.RuntimeMaskPCSCFDomain | qmi.RuntimeMaskMTU | qmi.RuntimeMaskIPFamily
	values, err := request(qmi.WDSGetRuntimeSettings, []qmi.TLV{qmi.NewTLVUint32(0x10, mask)}, false)
	if err != nil {
		return Runtime{}, err
	}
	packet := &qmi.Packet{ServiceType: qmi.ServiceWDS, ClientID: cid, MessageID: qmi.WDSGetRuntimeSettings}
	for kind, value := range values {
		packet.TLVs = append(packet.TLVs, qmi.TLV{Type: kind, Value: value})
	}
	runtime, err := parseRuntime(packet)
	clearProxyPacket(packet)
	if err != nil || runtime.Family != family {
		return Runtime{}, ErrRuntime
	}
	if err := status(); err != nil {
		return Runtime{}, err
	}
	return runtime, nil
}

func matchBoundRuntime(runtime Runtime, address BearerAddress, apn string) error {
	family := byte(6)
	local, gateway := runtime.IPv6, runtime.Gateway6
	if address.Address.Addr().Is4() {
		family = 4
		local, gateway = runtime.IPv4, runtime.Gateway4
	}
	if !strings.EqualFold(runtime.APN, apn) || runtime.Family != family || !usable(local.Addr()) || local != address.Address {
		return ErrRuntime
	}
	if gateway.IsValid() && (!usable(gateway) || gateway != address.Gateway) {
		return ErrRuntime
	}
	if runtime.MTU != 0 && address.MTU != 0 && runtime.MTU != address.MTU {
		return ErrRuntime
	}
	return nil
}

func mergeBoundRuntimes(rows []Runtime, configuration BearerConfiguration, apn string) (Runtime, error) {
	if len(rows) != len(configuration.Addresses) || len(rows) < 1 || len(rows) > 2 {
		return Runtime{}, ErrRuntime
	}
	merged := Runtime{APN: strings.ToLower(apn)}
	for index, row := range rows {
		address := configuration.Addresses[index]
		if err := matchBoundRuntime(row, address, apn); err != nil {
			return Runtime{}, err
		}
		if address.Address.Addr().Is4() {
			merged.IPv4, merged.Gateway4 = address.Address, row.Gateway4
		} else {
			merged.IPv6, merged.Gateway6 = address.Address, row.Gateway6
		}
		for _, remote := range row.PCSCF {
			if !containsAddress(merged.PCSCF, remote) {
				merged.PCSCF = append(merged.PCSCF, remote)
			}
		}
		for _, domain := range row.PCSCFDomains {
			exists := false
			for _, previous := range merged.PCSCFDomains {
				if domain == previous {
					exists = true
				}
			}
			if !exists {
				merged.PCSCFDomains = append(merged.PCSCFDomains, domain)
			}
		}
		if len(merged.PCSCF) > 8 || len(merged.PCSCFDomains) > 8 {
			return Runtime{}, ErrRuntime
		}
	}
	if _, err := merged.Endpoints(apn); err != nil {
		return Runtime{}, err
	}
	return merged, nil
}

func cloneRuntime(runtime Runtime) Runtime {
	runtime.PCSCF = append([]netip.Addr(nil), runtime.PCSCF...)
	runtime.PCSCFDomains = append([]string(nil), runtime.PCSCFDomains...)
	if runtime.Profile != nil {
		profile := *runtime.Profile
		runtime.Profile = &profile
	}
	return runtime
}
