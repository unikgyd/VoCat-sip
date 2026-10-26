package exportproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const proxyTimeout = 30 * time.Second

func dialTarget(ctx context.Context, address string, dialer *net.Dialer, resolver *net.Resolver) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: no addresses for %s", errors.ErrUnsupported, host)
	}
	return nil, lastErr
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
