//go:build linux

package ims

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// A synthetic socket capability, not a real modem/Established bearer. This
// verifies consumers really invoke Control before connect/listen on Linux.
type nativeSocketNetwork struct {
	fakeCellularBearer
	cookie uint64
}

func (network *nativeSocketNetwork) BindSocket(ctx context.Context, transport, address string, raw syscall.RawConn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var result error
	if err := raw.Control(func(fd uintptr) {
		cookie, err := unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
		if err != nil || cookie != network.cookie {
			result = errors.New("wrong socket namespace")
			return
		}
		family, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DOMAIN)
		if err != nil || strings.HasSuffix(transport, "4") && family != unix.AF_INET || strings.HasSuffix(transport, "6") && family != unix.AF_INET6 {
			result = errors.New("wrong family")
			return
		}
		result = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "lo")
	}); err != nil {
		return err
	}
	return result
}

func requireSocketNamespace(t *testing.T) {
	t.Helper()
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("explicit private network namespace required")
	}
	current, err := os.Readlink("/proc/self/ns/net")
	initial, initialErr := os.Readlink("/proc/1/ns/net")
	if err != nil || initialErr != nil || current == initial {
		t.Fatal("refusing socket test in host network namespace")
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
}

func TestCellularNativeBoundSignalingSockets(t *testing.T) {
	requireSocketNamespace(t)
	seed, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := seed.SyscallConn()
	network := &nativeSocketNetwork{}
	if err := raw.Control(func(fd uintptr) {
		network.cookie, err = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	}); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	if err != nil || network.cookie == 0 {
		t.Fatalf("namespace cookie: %v", err)
	}
	check := func(connection syscall.Conn, tcp bool) {
		t.Helper()
		raw, err := connection.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var bound string
		var cookie uint64
		var mss int
		if err := raw.Control(func(fd uintptr) {
			bound, _ = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
			cookie, _ = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
			if tcp {
				mss, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if bound != "lo" || cookie != network.cookie || tcp && (mss <= 0 || mss > protectedTCPMaxSegment) {
			t.Fatalf("lost binding/MSS: dev=%q cookie=%d mss=%d", bound, cookie, mss)
		}
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			local := net.ParseIP(host)
			listener, err := listenAccessTCP(ctx, local, 0, network)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			connection, err := dialSIPOnAccess(ctx, "tcp", host, 0, listener.Addr().String(), true, network)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			accepted, err := listener.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer accepted.Close()
			check(connection.(*net.TCPConn), true)
			check(listener, false)
			check(accepted, false)
			accepted.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := connection.Write([]byte("SIP-bound")); err != nil {
				t.Fatal(err)
			}
			packet := make([]byte, 9)
			if _, err := io.ReadFull(accepted, packet); err != nil || string(packet) != "SIP-bound" {
				t.Fatal("bound TCP transfer", err)
			}
			udp, err := listenAccessUDP(ctx, local, 0, network)
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			client, err := dialSIPOnAccess(ctx, "udp", host, 0, udp.LocalAddr().String(), false, network)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			check(udp, false)
			check(client.(*net.UDPConn), false)
			if !addressIP(client.LocalAddr()).Equal(local) || !addressIP(connection.LocalAddr()).Equal(local) {
				t.Fatal("wrong local source")
			}
			udp.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := client.Write([]byte("SIP-bound")); err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, 32)
			n, _, err := udp.ReadFromUDP(buffer)
			if err != nil || string(buffer[:n]) != "SIP-bound" {
				t.Fatal("bound UDP transfer", err)
			}
		})
	}
}
