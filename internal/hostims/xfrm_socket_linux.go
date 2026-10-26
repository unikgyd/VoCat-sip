//go:build linux

package hostims

import (
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// SocketSecurityRole identifies the negotiated IMS SA pair. It is not an
// authorization token: only the verified bearer owner may select the ReqID.
type SocketSecurityRole uint8

const (
	SocketSecurityClient SocketSecurityRole = 1
	SocketSecurityServer SocketSecurityRole = 2
)

// SocketSecurityPolicy requires transport ESP on one protected SIP socket.
// The two directions use the same negotiated SA-pair ReqID. The socket policy
// selector covers ALL traffic on the socket: a narrower selector can fall back
// to a permissive global policy when a different peer sends plaintext. The ESP
// template pins the negotiated endpoint pair and ReqID; the private socket bind
// pins LocalPort. Remote ports remain unrestricted for P-CSCF ephemeral ports.
// This must not be used for the initial unprotected REGISTER or for RTP.
type SocketSecurityPolicy struct {
	Local, Remote netip.Addr
	LocalPort     uint16
	Protocol      uint8 // IPPROTO_TCP or IPPROTO_UDP, never ESP or zero
	Role          SocketSecurityRole
	ReqID         uint32
}

var ErrSocketSecurityPolicy = staticError("host IMS: required socket ESP policy failed")

// RequireSocketESP installs required inbound AND outbound policies in one
// RawConn.Control callback. The caller must first verify ownership, namespace,
// device, source address and mark on this same private socket, and must not
// start I/O until this succeeds. An unbound socket is allowed for Dialer or
// ListenConfig.Control; a bound socket must already match Local/LocalPort.
//
// On ANY failure the caller MUST close the socket. Installation is not atomic
// across directions; rolling a partial installation back to plaintext is unsafe.
// Linux clones these policies to accepted TCP sockets. This helper neither
// installs SAs/global policies nor proves the supplied ReqID belongs to a modem.
func RequireSocketESP(raw syscall.RawConn, policy SocketSecurityPolicy) error {
	in, out, family, err := socketSecurityPolicies(policy)
	if err != nil || raw == nil {
		return ErrSocketSecurityPolicy
	}
	result := ErrSocketSecurityPolicy
	if err := raw.Control(func(fd uintptr) {
		if !socketSecurityMatches(int(fd), policy, family) {
			return
		}
		level, option := unix.IPPROTO_IP, unix.IP_XFRM_POLICY
		if family == unix.AF_INET6 {
			level, option = unix.IPPROTO_IPV6, unix.IPV6_XFRM_POLICY
		}
		// IP_XFRM_POLICY takes userpolicy_info followed directly by user_tmpl,
		// not netlink attributes. No key material is present in either buffer.
		if unix.SetsockoptString(int(fd), level, option, string(in)) != nil {
			return
		}
		if unix.SetsockoptString(int(fd), level, option, string(out)) != nil {
			return
		}
		result = ""
	}); err != nil || result != "" {
		return ErrSocketSecurityPolicy
	}
	return nil
}

func socketSecurityPolicies(p SocketSecurityPolicy) ([]byte, []byte, int, error) {
	if !p.Local.IsValid() || !p.Remote.IsValid() || p.Local.Is4In6() || p.Remote.Is4In6() ||
		p.Local.Zone() != "" || p.Remote.Zone() != "" || p.Local.IsUnspecified() || p.Remote.IsUnspecified() ||
		p.Local.IsMulticast() || p.Remote.IsMulticast() || p.Local == p.Remote || p.Local.Is4() != p.Remote.Is4() ||
		p.Local.IsLinkLocalUnicast() || p.Remote.IsLinkLocalUnicast() ||
		p.LocalPort <= 1024 || p.LocalPort == 5060 || p.LocalPort == 5061 || p.ReqID == 0 ||
		(p.Protocol != unix.IPPROTO_TCP && p.Protocol != unix.IPPROTO_UDP) ||
		(p.Role != SocketSecurityClient && p.Role != SocketSecurityServer) {
		return nil, nil, 0, ErrSocketSecurityPolicy
	}
	family := unix.AF_INET6
	if p.Local.Is4() {
		family = unix.AF_INET
	}
	makePolicy := func(inbound bool) []byte {
		local, remote := nl.XfrmAddress{}, nl.XfrmAddress{}
		copy(local[:], p.Local.AsSlice())
		copy(remote[:], p.Remote.AsSlice())
		info := nl.XfrmUserpolicyInfo{}
		// Do not narrow this selector. Unmatched socket selectors are allowed
		// to fall back to the global policy database (normally default allow).
		info.Sel.Family = uint16(family)
		info.Lft.SoftByteLimit, info.Lft.HardByteLimit = ^uint64(0), ^uint64(0)
		info.Lft.SoftPacketLimit, info.Lft.HardPacketLimit = ^uint64(0), ^uint64(0)
		tmpl := nl.XfrmUserTmpl{Family: uint16(family), Reqid: p.ReqID,
			Aalgos: ^uint32(0), Ealgos: ^uint32(0), Calgos: ^uint32(0)}
		tmpl.XfrmId.Proto = unix.IPPROTO_ESP
		// Zero mode = transport, zero optional = required. No SPI wildcard can
		// bypass ReqID; a wrong-pair decrypted packet fails the socket policy.
		if inbound {
			info.Dir = 0 // XFRM_POLICY_IN
			tmpl.Saddr, tmpl.XfrmId.Daddr = remote, local
		} else {
			info.Dir = 1 // XFRM_POLICY_OUT
			tmpl.Saddr, tmpl.XfrmId.Daddr = local, remote
		}
		result := make([]byte, 0, nl.SizeofXfrmUserpolicyInfo+nl.SizeofXfrmUserTmpl)
		result = append(result, info.Serialize()...)
		return append(result, tmpl.Serialize()...)
	}
	return makePolicy(true), makePolicy(false), family, nil
}

func socketSecurityMatches(fd int, p SocketSecurityPolicy, family int) bool {
	actualFamily, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN)
	if err != nil || actualFamily != family {
		return false
	}
	typeWanted := unix.SOCK_DGRAM
	if p.Protocol == unix.IPPROTO_TCP {
		typeWanted = unix.SOCK_STREAM
	}
	typeGot, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || typeGot != typeWanted {
		return false
	}
	protocol, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PROTOCOL)
	if err != nil || protocol != int(p.Protocol) {
		return false
	}
	local, err := unix.Getsockname(fd)
	if err != nil {
		return false
	}
	var addr netip.Addr
	var port int
	switch value := local.(type) {
	case *unix.SockaddrInet4:
		addr, port = netip.AddrFrom4(value.Addr), value.Port
	case *unix.SockaddrInet6:
		if value.ZoneId != 0 {
			return false
		}
		addr, port = netip.AddrFrom16(value.Addr), value.Port
	default:
		return false
	}
	// A nonzero port means bind has occurred. Wildcard binds are not an
	// acceptable substitute for the proven bearer source address.
	if port != 0 && (addr != p.Local || port != int(p.LocalPort)) {
		return false
	}
	return port != 0 || addr.IsUnspecified()
}
