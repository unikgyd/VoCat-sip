package ims

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

func cellularSocketBinding(local, remote net.IP, port int, role imssec.SocketRole) (imssec.SocketBinding, error) {
	localIP, localOK := netip.AddrFromSlice(local)
	remoteIP, remoteOK := netip.AddrFromSlice(remote)
	if !localOK || !remoteOK || port < 0 || port > 65535 {
		return imssec.SocketBinding{}, ErrCellularAccess
	}
	return imssec.SocketBinding{Local: netip.AddrPortFrom(localIP.Unmap(), uint16(port)),
		Remote: remoteIP.Unmap(), Role: role}, nil
}

func bindAccessSocket(ctx context.Context, network vowifi.CellularIMSNetwork, protection *imssec.SocketBinding,
	transport, address string, raw syscall.RawConn) error {
	if ctx.Err() != nil || network == nil {
		return ErrCellularAccess
	}
	if protection != nil && protection.Validate(transport) != nil {
		return ErrCellularAccess
	}
	var security vowifi.CellularIMSSecurity
	if protection != nil {
		var ok bool
		security, ok = network.(vowifi.CellularIMSSecurity)
		if !ok {
			return ErrCellularAccess
		}
	}
	if err := network.BindSocket(ctx, transport, address, raw); err != nil || ctx.Err() != nil {
		return ErrCellularAccess
	}
	if protection != nil {
		if err := security.BindProtectedSocket(ctx, transport, *protection, raw); err != nil || ctx.Err() != nil {
			// This may contain SIM/driver/kernel details. Do not leak it through
			// a SIP error, logger, or API response, or retry without protection.
			return ErrCellularAccess
		}
	}
	return nil
}

func (session *Session) installAccessIPSec(ctx context.Context, config IPSecSAConfig) (IPSecSAHandle, error) {
	if session.request.Cellular == nil {
		return session.provider.installer.Install(ctx, config)
	}
	security, ok := session.request.Cellular.(vowifi.CellularIMSSecurity)
	if !ok || ctx.Err() != nil {
		return nil, ErrCellularAccess
	}
	handle, err := security.InstallIPSec(ctx, config)
	if err != nil || ctx.Err() != nil {
		// A returned handle still belongs to this session, even when the caller
		// was cancelled just after an acknowledged install. Retain it for abort
		// rather than forgetting it or falling back to the global installer.
		if handle != nil {
			session.ipsecHandle = handle
		}
		return nil, ErrCellularAccess
	}
	return handle, nil
}

// Called by the serialized session Close/abort/registration paths. Local
// security cleanup gets a fresh bounded context after a SIP deadline expires;
// a failed cleanup remains attached for a subsequent Close retry.
func (session *Session) closeAccessIPSec() error {
	if session.ipsecHandle == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := session.ipsecHandle.Close(ctx); err != nil {
		if session.request.Cellular != nil {
			return ErrCellularAccess
		}
		return err
	}
	session.ipsecHandle = nil
	return nil
}
