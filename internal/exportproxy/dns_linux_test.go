//go:build linux

package exportproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func dnsTestAnswer(query []byte, flags uint16) []byte {
	end := 12
	for end < len(query) && query[end] != 0 {
		end += int(query[end]) + 1
	}
	end++
	if end+4 > len(query) {
		return nil
	}
	kind := binary.BigEndian.Uint16(query[end : end+2])
	address := net.IPv4(127, 0, 0, 2).To4()
	if kind == 28 {
		address = net.ParseIP("::1").To16()
	}
	reply := make([]byte, 12)
	copy(reply[:2], query[:2])
	binary.BigEndian.PutUint16(reply[2:], flags)
	binary.BigEndian.PutUint16(reply[4:], 1)
	reply = append(reply, query[12:end+4]...)
	if flags&0x020f == 0 {
		binary.BigEndian.PutUint16(reply[6:], 1)
		reply = append(reply, 0xc0, 0x0c, byte(kind>>8), byte(kind), 0, 1, 0, 0, 0, 60, 0, byte(len(address)))
		reply = append(reply, address...)
	}
	return reply
}

func dnsTestUDP(t *testing.T, network string, respond func([]byte) []byte) string {
	t.Helper()
	address := "127.0.0.1:0"
	if network == "udp6" {
		address = "[::1]:0"
	}
	server, err := net.ListenPacket(network, address)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, peer, err := server.ReadFrom(buffer)
			if err != nil {
				return
			}
			if response := respond(append([]byte(nil), buffer[:n]...)); response != nil {
				_, _ = server.WriteTo(response, peer)
			}
		}
	}()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return server.LocalAddr().String()
}

func dnsTestResolver(servers []string, dial dnsSocketDial) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return newDNSRaceConn(ctx, network, servers, dial)
	}}
}

func TestDNSRaceUnresponsivePrimaryUsesOtherAddressFamily(t *testing.T) {
	for _, working := range []string{"udp4", "udp6"} {
		t.Run(working, func(t *testing.T) {
			blackholed := "udp6"
			if working == "udp6" {
				blackholed = "udp4"
			}
			seen := make(chan struct{})
			var once sync.Once
			primary := dnsTestUDP(t, blackholed, func([]byte) []byte { once.Do(func() { close(seen) }); return nil })
			secondary := dnsTestUDP(t, working, func(query []byte) []byte {
				select {
				case <-seen:
				case <-time.After(time.Second):
					return nil
				}
				return dnsTestAnswer(query, 0x8180)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var calls sync.Map
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				calls.Store(address, true)
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			addresses, err := dnsTestResolver([]string{primary, secondary}, dial).LookupIP(ctx, "ip4", "native-dns.vocat.invalid")
			if err != nil || len(addresses) != 1 || !addresses[0].Equal(net.IPv4(127, 0, 0, 2)) {
				t.Fatalf("other family DNS did not resolve: %v, %v", addresses, err)
			}
			for _, address := range []string{primary, secondary} {
				if _, ok := calls.Load(address); !ok {
					t.Fatal("original DNS dialer did not receive configured server")
				}
			}
		})
	}
}

func TestDNSRaceServerFailureDoesNotBeatUsableAnswer(t *testing.T) {
	failed := dnsTestUDP(t, "udp4", func(query []byte) []byte { return dnsTestAnswer(query, 0x8182) })
	working := dnsTestUDP(t, "udp4", func(query []byte) []byte { time.Sleep(15 * time.Millisecond); return dnsTestAnswer(query, 0x8180) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addresses, err := dnsTestResolver([]string{failed, working}, (&net.Dialer{}).DialContext).LookupIP(ctx, "ip4", "native-dns.vocat.invalid")
	if err != nil || len(addresses) != 1 {
		t.Fatalf("SERVFAIL hid the other server: %v %v", addresses, err)
	}
}

func TestDNSRaceTruncatedUDPReachesTCPAndReadsCompleteFrame(t *testing.T) {
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := server.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				var prefix [2]byte
				if _, err := io.ReadFull(conn, prefix[:]); err != nil {
					return
				}
				query := make([]byte, int(binary.BigEndian.Uint16(prefix[:])))
				if _, err := io.ReadFull(conn, query); err != nil {
					return
				}
				reply := dnsTestAnswer(query, 0x8180)
				binary.BigEndian.PutUint16(prefix[:], uint16(len(reply)))
				_, _ = conn.Write(prefix[:1])
				_, _ = conn.Write(prefix[1:])
				_, _ = conn.Write(reply[:7])
				_, _ = conn.Write(reply[7:])
			}()
		}
	}()
	t.Cleanup(func() { _ = server.Close(); <-done })
	udp, err := net.ListenPacket("udp4", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	udpDone := make(chan struct{})
	go func() {
		defer close(udpDone)
		buffer := make([]byte, 4096)
		for {
			n, peer, err := udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(dnsTestAnswer(buffer[:n], 0x8380), peer)
		}
	}()
	t.Cleanup(func() { _ = udp.Close(); <-udpDone })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var tcp atomic.Bool
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if network == "tcp" {
			tcp.Store(true)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	addresses, err := dnsTestResolver([]string{server.Addr().String()}, dial).LookupIP(ctx, "ip4", "native-dns.vocat.invalid")
	if err != nil || len(addresses) != 1 || !tcp.Load() {
		t.Fatalf("truncated UDP did not recover over TCP: %v %v", addresses, err)
	}
}

type dnsLateRead struct {
	net.Conn
	started, canceled, release chan struct{}
	once                       sync.Once
}

func (c *dnsLateRead) Read(buffer []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	n, err := c.Conn.Read(buffer)
	close(c.canceled)
	<-c.release
	return n, err
}

func TestDNSRaceCloseJoinsLosingQueryBeforeResolutionReturns(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	working := dnsTestUDP(t, "udp4", func(query []byte) []byte {
		select {
		case <-started:
		case <-time.After(time.Second):
			return nil
		}
		return dnsTestAnswer(query, 0x8180)
	})
	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	go func() { _, _ = io.Copy(io.Discard, right) }()
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "127.0.0.1:1" {
			return &dnsLateRead{Conn: left, started: started, canceled: canceled, release: release}, nil
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := dnsTestResolver([]string{"127.0.0.1:1", working}, dial).LookupIP(ctx, "ip4", "native-dns.vocat.invalid")
		done <- err
	}()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("losing DNS read was not canceled")
	}
	select {
	case <-done:
		t.Fatal("DNS resolution returned before canceled worker exited")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("DNS worker did not join")
	}
}

func TestDNSRaceCancellationDeadlineAndWrongQuestion(t *testing.T) {
	server := dnsTestUDP(t, "udp4", func(query []byte) []byte { reply := dnsTestAnswer(query, 0x8180); reply[13] ^= 1; return reply })
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	conn, err := newDNSRaceConn(ctx, "udp", []string{server}, (&net.Dialer{}).DialContext)
	if err != nil {
		t.Fatal(err)
	}
	query := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1}
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 512))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrong question escaped deadline: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(query); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed DNS exchange admitted a new query", err)
	}
	conn, err = newDNSRaceConn(context.Background(), "udp", []string{server}, (&net.Dialer{}).DialContext)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 512))
	if err == nil {
		t.Fatal("read deadline ignored")
	}
}
