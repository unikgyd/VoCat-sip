//go:build linux

package ims

import (
	"context"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCellularNativeRTPBindsBearerAndSource(t *testing.T) {
	requireSocketNamespace(t)
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			seed, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host)})
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
			if err != nil {
				t.Fatal(err)
			}
			media, err := newAccessRTPMedia(context.Background(), net.ParseIP(host), network, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer media.Close()
			if !addressIP(media.conn.LocalAddr()).Equal(net.ParseIP(host)) {
				t.Fatal("RTP lost assigned source")
			}
			var socket syscall.Conn = media.conn
			boundRaw, err := socket.SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var device string
			var cookie uint64
			if err := boundRaw.Control(func(fd uintptr) {
				device, _ = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
				cookie, _ = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
			}); err != nil {
				t.Fatal(err)
			}
			if device != "lo" || cookie != network.cookie {
				t.Fatal("RTP bypassed bound bearer/namespace", device, cookie)
			}
		})
	}
}
