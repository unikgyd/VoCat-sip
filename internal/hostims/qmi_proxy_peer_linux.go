//go:build linux

package hostims

import (
	"context"
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func trustedProxyPeer(connection *net.UnixConn) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return ErrDenied
	}
	var credentials *unix.Ucred
	var credentialErr error
	if raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || credentialErr != nil || credentials == nil || credentials.Pid <= 0 || credentials.Uid != 0 && credentials.Uid != uint32(os.Geteuid()) {
		return ErrDenied
	}
	return nil
}

func dialQMIProxy(ctx context.Context) (*proxyQMI, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", "\x00qmi-proxy")
	if err != nil {
		return nil, ErrQMIProxy
	}
	local, ok := connection.(*net.UnixConn)
	if !ok || trustedProxyPeer(local) != nil {
		_ = connection.Close()
		return nil, ErrQMIProxy
	}
	return newProxyQMI(connection), nil
}
