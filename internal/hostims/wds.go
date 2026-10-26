// Package hostims contains cellular IMS building blocks. The explicit bearer
// owner may create an IMS PDN; construction/discovery never starts one. This
// package never enables/resets RF or registers SIP. The explicit kernel lease
// manages only its acknowledged, privately journaled host network resources.
package hostims

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

var (
	ErrRuntime = errors.New("host IMS: invalid or unavailable WDS runtime settings")
	ErrPCSCF   = errors.New("host IMS: no numeric P-CSCF matches the selected IMS APN/address family")
)

// WDSProfile is a stored profile identifier, NEVER an AT PDP CID or QMI CID.
type WDSProfile struct{ Type, Index byte }

type Runtime struct {
	APN                string
	Profile            *WDSProfile
	Family             byte
	IPv4, IPv6         netip.Prefix
	Gateway4, Gateway6 netip.Addr
	PCSCF              []netip.Addr
	PCSCFDomains       []string
	MTU                uint32
}

type Endpoint struct{ Local, Remote netip.Addr }

type RequestSender interface {
	SendRequest(context.Context, uint8, uint8, uint16, []qmi.TLV) (*qmi.Packet, error)
}

// ReadRuntime uses an existing process-owned WDS CID. It never allocates,
// borrows a ModemManager CID, changes IP-family preference or starts a bearer.
// The caller serializes this operation with its bearer lifecycle. The generic
// SDK helper omits P-CSCF/APN/profile bits, so do not use it for Host IMS proof.
func ReadRuntime(ctx context.Context, sender RequestSender, clientID uint8) (Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sender == nil || clientID == 0 || clientID == 255 {
		return Runtime{}, ErrRuntime
	}
	if ctx.Err() != nil {
		return Runtime{}, ErrRuntime
	}
	mask := qmi.RuntimeMaskProfileID | qmi.RuntimeMaskAPNName | qmi.RuntimeMaskIPAddr |
		qmi.RuntimeMaskGateway | qmi.RuntimeMaskPCSCFPCO | qmi.RuntimeMaskPCSCFAddr |
		qmi.RuntimeMaskPCSCFDomain | qmi.RuntimeMaskMTU | qmi.RuntimeMaskIPFamily
	packet, err := sender.SendRequest(ctx, qmi.ServiceWDS, clientID, qmi.WDSGetRuntimeSettings,
		[]qmi.TLV{qmi.NewTLVUint32(0x10, mask)})
	if err != nil || ctx.Err() != nil || packet == nil || packet.ClientID != clientID || packet.IsIndication ||
		packet.ServiceType != qmi.ServiceWDS || packet.MessageID != qmi.WDSGetRuntimeSettings {
		return Runtime{}, ErrRuntime
	}
	return parseRuntime(packet)
}

// Layouts follow libqmi Get Current Settings; Gobi adds the IPv6 P-CSCF list
// at 0x2E. Numeric IPv4 fields are little endian; IPv6 octets are network order.
// Unknown extension TLVs are ignored, but duplicate singleton TLVs, short or
// trailing bytes and non-success results fail closed without echoing payloads.
func parseRuntime(packet *qmi.Packet) (Runtime, error) {
	var result Runtime
	if packet == nil || len(packet.TLVs) > 64 {
		return result, ErrRuntime
	}
	values := make(map[byte][]byte, len(packet.TLVs))
	total := 0
	for _, tlv := range packet.TLVs {
		if _, exists := values[tlv.Type]; exists {
			return Runtime{}, ErrRuntime
		}
		total += len(tlv.Value)
		if total > 32768 {
			return Runtime{}, ErrRuntime
		}
		values[tlv.Type] = tlv.Value
	}
	if len(values[2]) != 4 || packet.CheckResult() != nil {
		return result, ErrRuntime
	}
	for tag, length := range map[byte]int{0x1E: 4, 0x20: 4, 0x21: 4, 0x1F: 2, 0x22: 1, 0x25: 17, 0x26: 17, 0x29: 4, 0x2B: 1} {
		if value, ok := values[tag]; ok && len(value) != length {
			return Runtime{}, ErrRuntime
		}
	}
	if value, ok := values[0x14]; ok {
		result.APN = string(value)
		if !validAPN(result.APN) {
			return Runtime{}, ErrRuntime
		}
	}
	if value, ok := values[0x1F]; ok {
		result.Profile = &WDSProfile{Type: value[0], Index: value[1]}
	}
	if value, ok := values[0x2B]; ok {
		result.Family = value[0]
		if result.Family != 4 && result.Family != 6 {
			return Runtime{}, ErrRuntime
		}
	}
	if value, ok := values[0x1E]; ok {
		mask, exists := values[0x21]
		if !exists {
			return Runtime{}, ErrRuntime
		}
		maskIP := ipv4(mask).As4()
		ones, bits := net.IPMask(maskIP[:]).Size()
		if bits != 32 {
			return Runtime{}, ErrRuntime
		}
		result.IPv4 = netip.PrefixFrom(ipv4(value), ones)
	}
	if value, ok := values[0x20]; ok {
		result.Gateway4 = ipv4(value)
	}
	if value, ok := values[0x25]; ok {
		if value[16] > 128 {
			return Runtime{}, ErrRuntime
		}
		address, _ := netip.AddrFromSlice(value[:16])
		if address.Is4In6() {
			return Runtime{}, ErrRuntime
		}
		result.IPv6 = netip.PrefixFrom(address, int(value[16]))
	}
	if value, ok := values[0x26]; ok {
		if value[16] > 128 {
			return Runtime{}, ErrRuntime
		}
		result.Gateway6, _ = netip.AddrFromSlice(value[:16])
	}
	if value, ok := values[0x29]; ok {
		result.MTU = binary.LittleEndian.Uint32(value)
		if result.MTU < 68 || result.MTU > 65535 {
			return Runtime{}, ErrRuntime
		}
	}
	for _, spec := range []struct {
		tag   byte
		width int
	}{{0x23, 4}, {0x2E, 16}} {
		value, exists := values[spec.tag]
		if !exists {
			continue
		}
		if len(value) < 1 || int(value[0]) > 8 || len(value) != 1+int(value[0])*spec.width {
			return Runtime{}, ErrRuntime
		}
		for offset := 1; offset < len(value); offset += spec.width {
			var address netip.Addr
			if spec.width == 4 {
				address = ipv4(value[offset : offset+4])
			} else {
				address, _ = netip.AddrFromSlice(value[offset : offset+16])
			}
			if !usable(address) {
				return Runtime{}, ErrRuntime
			}
			if !containsAddress(result.PCSCF, address) {
				result.PCSCF = append(result.PCSCF, address)
			}
		}
	}
	if len(result.PCSCF) > 8 {
		return Runtime{}, ErrRuntime
	}
	if value, ok := values[0x24]; ok {
		if len(value) < 1 || value[0] > 8 {
			return Runtime{}, ErrRuntime
		}
		offset := 1
		for count := 0; count < int(value[0]); count++ {
			if len(value)-offset < 2 {
				return Runtime{}, ErrRuntime
			}
			length := int(binary.LittleEndian.Uint16(value[offset : offset+2]))
			offset += 2
			if length > len(value)-offset || length == 0 {
				return Runtime{}, ErrRuntime
			}
			domain := string(value[offset : offset+length])
			offset += length
			if !validDomain(domain) {
				return Runtime{}, ErrRuntime
			}
			result.PCSCFDomains = append(result.PCSCFDomains, strings.ToLower(domain))
		}
		if offset != len(value) {
			return Runtime{}, ErrRuntime
		}
	}
	return result, nil
}

// Endpoints binds runtime P-CSCF to the exact configured APN and matching
// local family. DNS servers, profile numbers and another PDN are never fallback
// P-CSCF evidence. DNS-only P-CSCF needs a future bearer-bound resolver.
func (runtime Runtime) Endpoints(apn string) ([]Endpoint, error) {
	if !validAPN(apn) || !strings.EqualFold(apn, runtime.APN) {
		return nil, ErrPCSCF
	}
	var endpoints []Endpoint
	for _, remote := range runtime.PCSCF {
		local := runtime.IPv6.Addr()
		if remote.Is4() {
			local = runtime.IPv4.Addr()
		}
		if usable(local) && usable(remote) && local.Is4() == remote.Is4() && local != remote {
			endpoints = append(endpoints, Endpoint{Local: local, Remote: remote})
		}
	}
	if len(endpoints) == 0 {
		return nil, ErrPCSCF
	}
	return endpoints, nil
}

func ipv4(value []byte) netip.Addr {
	return netip.AddrFrom4([4]byte{value[3], value[2], value[1], value[0]})
}
func usable(ip netip.Addr) bool {
	return ip.IsValid() && !ip.Is4In6() && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}
func containsAddress(values []netip.Addr, ip netip.Addr) bool {
	for _, value := range values {
		if value == ip {
			return true
		}
	}
	return false
}
func validAPN(value string) bool { return len(value) <= 100 && validDomain(value) }
func validDomain(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range []byte(label) {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
