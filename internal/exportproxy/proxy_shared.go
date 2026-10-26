package exportproxy

import (
	"context"
	"io"
	"net"
	"time"
)

const proxyTimeout = 30 * time.Second

func dialTarget(ctx context.Context, address string, dialer *net.Dialer, resolver *net.Resolver) (net.Conn, error) {
	_, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	// Address preference is computed from the host's unmarked routes. That
	// ordering can prefer unreachable IPv4 on an IPv6-only modem. Preserve the
	// bound socket controls and DNS resolver while letting net.Dialer race the
	// other family instead of exhausting the whole lease on the first address.
	bound := *dialer
	bound.Resolver = resolver
	return bound.DialContext(ctx, "tcp", address)
}

func pipe(left, right net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = copyConnection(right, left); done <- struct{}{} }()
	go func() { _, _ = copyConnection(left, right); done <- struct{}{} }()
	<-done
	// Closing both sockets joins the other copy before releasing the cellular
	// lease. Half of a tunnel is not proof that all user-plane I/O has exited.
	_ = left.Close()
	_ = right.Close()
	<-done
}

func copyConnection(destination net.Conn, source net.Conn) (int64, error) {
	written, err := io.CopyBuffer(destination, source, make([]byte, 32*1024))
	if err == nil && written > 0 {
		if connection, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = connection.CloseWrite()
		}
	}
	return written, err
}
