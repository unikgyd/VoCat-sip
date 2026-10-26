package imssec

import (
	"errors"
	"net/netip"
)

// SocketRole selects one of the two UE security pairs. The owner must reserve
// distinct, generation-scoped reqids before exposing a protected listener;
// negotiated SPIs alone are not an ownership or multi-modem isolation key.
type SocketRole uint8

const (
	ClientSocket SocketRole = iota + 1
	ServerSocket
)

// SocketBinding is deliberately independent of negotiated keys and SPIs. This
// allows a required ESP socket policy to be installed BEFORE the server port is
// bound/advertised, including while the initial unprotected REGISTER is pending.
// A zero remote port is intentional: a P-CSCF may use a fresh source port.
type SocketBinding struct {
	Local  netip.AddrPort
	Remote netip.Addr
	Role   SocketRole
}

var ErrSocketBinding = errors.New("IMS protected socket binding is invalid")

func (binding SocketBinding) Validate(transport string) error {
	local, remote := binding.Local.Addr(), binding.Remote
	if !validSocketIP(local) || !validSocketIP(remote) || local.Is4() != remote.Is4() ||
		binding.Local.Port() <= 1024 || binding.Local.Port() == 5060 || binding.Local.Port() == 5061 ||
		(binding.Role != ClientSocket && binding.Role != ServerSocket) {
		return ErrSocketBinding
	}
	switch transport {
	case "tcp4", "udp4":
		if !local.Is4() {
			return ErrSocketBinding
		}
	case "tcp6", "udp6":
		if !local.Is6() {
			return ErrSocketBinding
		}
	default:
		return ErrSocketBinding
	}
	return nil
}

func validSocketIP(address netip.Addr) bool {
	return address.IsValid() && address.Zone() == "" && !address.Is4In6() &&
		!address.IsUnspecified() && !address.IsMulticast() && !address.IsLinkLocalUnicast()
}
