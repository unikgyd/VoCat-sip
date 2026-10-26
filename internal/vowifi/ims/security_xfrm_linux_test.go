//go:build linux

package ims

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// No packets from this test can enter a production interface. It requires a
// fresh unshare -n namespace, then sends to an unassigned TEST-NET address on a
// namespace-local veth. The far veth only sinks packets; no registrar is used.
func TestLinuxXFRMServerRepliesEncryptEphemeralTCPAndUDP(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("set VOCAT_NETNS_TEST=1 inside a fresh isolated Linux network namespace")
	}
	self, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	init, err := os.Stat("/proc/1/ns/net")
	if err != nil || os.SameFile(self, init) {
		t.Fatal("refusing to send test packets in the init network namespace")
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("XFRM packet test requires a fresh namespace containing only loopback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		if output, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("isolated ip setup failed: %v: %s", err, output)
		}
	}
	run("link", "add", "vtxfrmt0", "type", "veth", "peer", "name", "vtxfrmt1")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if output, err := exec.CommandContext(cleanup, "ip", "link", "del", "vtxfrmt0").CombinedOutput(); err != nil {
			t.Errorf("remove test-owned veth: %v: %s", err, output)
		}
	})
	run("addr", "add", "192.0.2.10/30", "dev", "vtxfrmt0")
	run("link", "set", "vtxfrmt0", "up")
	run("link", "set", "vtxfrmt1", "up")
	peer, err := net.InterfaceByName("vtxfrmt1")
	if err != nil {
		t.Fatal(err)
	}
	run("neigh", "add", "192.0.2.9", "lladdr", peer.HardwareAddr.String(), "nud", "permanent", "dev", "vtxfrmt0")
	config := testIPSecSAConfig()
	config.LocalIP, config.RemoteIP = net.ParseIP("192.0.2.10"), net.ParseIP("192.0.2.9")
	handle, err := (linuxIPSecInstaller{ipCommand: "ip"}).Install(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := handle.Close(cleanup); err != nil {
			t.Errorf("remove test-owned XFRM: %v", err)
		}
	})
	packets := func() uint64 {
		t.Helper()
		// Some iproute2 versions silently emit text for -j xfrm. Read the
		// kernel statistics directly instead of parsing output containing keys.
		state, err := netlink.XfrmStateGet(&netlink.XfrmState{Src: config.LocalIP, Dst: config.RemoteIP,
			Proto: netlink.XFRM_PROTO_ESP, Spi: int(config.PCSCFClientSPI)})
		if err != nil {
			t.Fatalf("read server-pair ESP counter: %v", err)
		}
		return state.Statistics.Packets
	}
	for _, protocol := range []string{"udp", "tcp"} {
		for _, remotePort := range []int{config.PCSCFClientPort, 41001} {
			before := packets()
			remote := net.JoinHostPort(config.RemoteIP.String(), fmt.Sprint(remotePort))
			if protocol == "udp" {
				conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: config.LocalIP, Port: config.UEServerPort})
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := conn.WriteToUDP([]byte("synthetic protected reply"), &net.UDPAddr{IP: config.RemoteIP, Port: remotePort})
				_ = conn.Close()
				if writeErr != nil {
					t.Fatal(writeErr)
				}
			} else {
				// No server accepts: the outbound SYN is enough to prove ESP
				// policy selection for replies on an accepted TCP tuple.
				dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: config.LocalIP, Port: config.UEServerPort}, Timeout: 100 * time.Millisecond}
				if conn, _ := dialer.DialContext(ctx, "tcp4", remote); conn != nil {
					_ = conn.Close()
				}
			}
			after := packets()
			if after <= before {
				t.Fatalf("%s server reply to port %d bypassed the server-pair ESP SA: before=%d after=%d", protocol, remotePort, before, after)
			}
			t.Logf("%s destination port %d: server-pair ESP packets %d -> %d", protocol, remotePort, before, after)
		}
	}
}
