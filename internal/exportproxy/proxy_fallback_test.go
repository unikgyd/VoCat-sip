//go:build linux

package exportproxy

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

func fallbackTestResolver(t *testing.T) *net.Resolver {
	t.Helper()
	dns, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, peer, err := dns.ReadFrom(buffer)
			if err != nil {
				return
			}
			query := append([]byte(nil), buffer[:n]...)
			if len(query) < 17 {
				continue
			}
			end := 12
			for end < len(query) && query[end] != 0 {
				end += int(query[end]) + 1
			}
			end++
			if end+4 > len(query) {
				continue
			}
			kind := binary.BigEndian.Uint16(query[end : end+2])
			answer := net.IPv4(127, 0, 0, 1).To4()
			if kind == 28 {
				answer = net.ParseIP("::1").To16()
			} else if kind != 1 {
				continue
			}
			reply := make([]byte, 12)
			copy(reply[:2], query[:2])
			binary.BigEndian.PutUint16(reply[2:], 0x8180)
			binary.BigEndian.PutUint16(reply[4:], 1)
			binary.BigEndian.PutUint16(reply[6:], 1)
			reply = append(reply, query[12:end+4]...)
			reply = append(reply, 0xc0, 0x0c, byte(kind>>8), byte(kind), 0, 1, 0, 0, 0, 60, 0, byte(len(answer)))
			reply = append(reply, answer...)
			_, _ = dns.WriteTo(reply, peer)
		}
	}()
	t.Cleanup(func() { _ = dns.Close(); <-done })
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", dns.LocalAddr().String())
	}}
}

func TestProxyAddressFamilyFallbackPreservesBoundControlAndResolver(t *testing.T) {
	v4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v4.Close() })
	_, port, _ := net.SplitHostPort(v4.Addr().String())
	v6, err := net.Listen("tcp6", net.JoinHostPort("::1", port))
	if err != nil {
		t.Fatal("IPv6 loopback is required for family-fallback acceptance", err)
	}
	t.Cleanup(func() { _ = v6.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	unblock := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(unblock) })
	var first sync.Once
	firstDone := make(chan struct{})
	var mu sync.Mutex
	families := map[string]bool{}
	dialer := &net.Dialer{FallbackDelay: 20 * time.Millisecond, Control: func(network, _ string, _ syscall.RawConn) error {
		mu.Lock()
		families[network] = true
		mu.Unlock()
		primary := false
		first.Do(func() { primary = true })
		if primary {
			defer close(firstDone)
			<-unblock // deterministic stalled first-family socket setup
			return context.Canceled
		}
		return nil
	}}
	type result struct {
		connection net.Conn
		err        error
	}
	finished := make(chan result, 1)
	resolver := fallbackTestResolver(t)
	go func() {
		connection, err := dialTarget(ctx, net.JoinHostPort("fallback.vocat.invalid", port), dialer, resolver)
		finished <- result{connection, err}
	}()
	select {
	case out := <-finished:
		if out.err != nil {
			t.Fatal("other address family did not establish a connection", out.err)
		}
		_ = out.connection.Close()
	case <-time.After(2 * time.Second):
		release.Do(func() { close(unblock) })
		cancel()
		out := <-finished
		if out.connection != nil {
			_ = out.connection.Close()
		}
		t.Fatal("serial first-family dialing consumed the available connection budget")
	}
	release.Do(func() { close(unblock) })
	<-firstDone
	mu.Lock()
	defer mu.Unlock()
	if !families["tcp4"] || !families["tcp6"] {
		t.Fatal("fallback lost the original socket controls", families)
	}
	if dialer.Resolver != nil || dialer.FallbackDelay != 20*time.Millisecond {
		t.Fatal("dialTarget mutated the caller's dialer")
	}
}
