package ims

import (
	"context"
	"net"
	"strconv"
	"syscall"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

// ControlContext has precedence over Control in net.Dialer. Explicitly compose
// the bearer hook with the existing protected-TCP MSS hook, never replace it.
func bindCellularDialer(dialer *net.Dialer, network vowifi.CellularIMSNetwork) {
	bindProtectedCellularDialer(dialer, network, nil)
}

func bindProtectedCellularDialer(dialer *net.Dialer, network vowifi.CellularIMSNetwork, protection *imssec.SocketBinding) {
	if network == nil && protection == nil {
		return
	}
	// A single owned IMS bearer and its ESP selectors do not authorize MPTCP
	// subflows. Also prevent net's MPTCP->TCP retry after a denied Control hook.
	dialer.SetMultipathTCP(false)
	previous, previousContext := dialer.Control, dialer.ControlContext
	dialer.ControlContext = func(ctx context.Context, transport, address string, raw syscall.RawConn) error {
		if err := bindAccessSocket(ctx, network, protection, transport, address, raw); err != nil {
			return ErrCellularAccess
		}
		if previousContext != nil {
			return previousContext(ctx, transport, address, raw)
		}
		if previous != nil {
			return previous(transport, address, raw)
		}
		return nil
	}
}

func cellularListenConfig(ctx context.Context, network vowifi.CellularIMSNetwork) net.ListenConfig {
	return protectedCellularListenConfig(ctx, network, nil)
}

func protectedCellularListenConfig(ctx context.Context, network vowifi.CellularIMSNetwork, protection *imssec.SocketBinding) net.ListenConfig {
	config := net.ListenConfig{}
	if network != nil || protection != nil {
		config.SetMultipathTCP(false)
		config.Control = func(transport, address string, raw syscall.RawConn) error {
			if err := bindAccessSocket(ctx, network, protection, transport, address, raw); err != nil {
				return ErrCellularAccess
			}
			return nil
		}
	}
	return config
}

func listenAccessTCP(ctx context.Context, local net.IP, port int, network vowifi.CellularIMSNetwork) (*net.TCPListener, error) {
	return listenProtectedAccessTCP(ctx, local, port, network, nil)
}

func listenProtectedAccessTCP(ctx context.Context, local net.IP, port int, network vowifi.CellularIMSNetwork, protection *imssec.SocketBinding) (*net.TCPListener, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	config := protectedCellularListenConfig(ctx, network, protection)
	listener, err := config.Listen(ctx, "tcp", net.JoinHostPort(local.String(), strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return listener.(*net.TCPListener), nil
}

func listenAccessUDP(ctx context.Context, local net.IP, port int, network vowifi.CellularIMSNetwork) (*net.UDPConn, error) {
	return listenProtectedAccessUDP(ctx, local, port, network, nil)
}

func listenProtectedAccessUDP(ctx context.Context, local net.IP, port int, network vowifi.CellularIMSNetwork, protection *imssec.SocketBinding) (*net.UDPConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	config := protectedCellularListenConfig(ctx, network, protection)
	connection, err := config.ListenPacket(ctx, "udp", net.JoinHostPort(local.String(), strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return connection.(*net.UDPConn), nil
}

func (session *Session) bindAcceptedCellularTCP(connection *net.TCPConn) error {
	if session.request.Cellular == nil {
		return nil
	}
	local, ok := connection.LocalAddr().(*net.TCPAddr)
	if !ok || !local.IP.Equal(addressIP(session.conn.LocalAddr())) || local.Port != session.securityProposal.portServer {
		return ErrCellularAccess
	}
	remote := addressIP(connection.RemoteAddr())
	if remote == nil || !remote.Equal(addressIP(session.conn.RemoteAddr())) {
		return ErrCellularAccess
	}
	protection, err := cellularSocketBinding(local.IP, remote, local.Port, imssec.ServerSocket)
	if err != nil {
		return err
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return ErrCellularAccess
	}
	ctx, cancel := context.WithTimeout(session.callContext(), 15*time.Second)
	defer cancel()
	network := "tcp4"
	if local.IP.To4() == nil {
		network = "tcp6"
	}
	if err := bindAccessSocket(ctx, session.request.Cellular, &protection, network, connection.RemoteAddr().String(), raw); err != nil {
		return ErrCellularAccess
	}
	return nil
}
