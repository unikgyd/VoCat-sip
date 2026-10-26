//go:build linux

package ims

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"vocat/internal/vowifi/imssec"
)

// This test never sends packets. Inspect only objects installed inside a new
// network namespace with no production interfaces, modem, SIM or registrar.
func TestLinuxXFRMSharedPlanIncludesRequiredUEClientUDPInbound(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("set VOCAT_NETNS_TEST=1 inside a fresh isolated Linux network namespace")
	}
	self, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	init, err := os.Stat("/proc/1/ns/net")
	if err != nil || os.SameFile(self, init) {
		t.Fatal("refusing to install XFRM in the init network namespace")
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("shared-plan test requires a fresh namespace containing only loopback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := testIPSecSAConfig()
	handle, err := (linuxIPSecInstaller{ipCommand: "ip"}).Install(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := handle.Close(cleanup); err != nil {
			t.Error("remove test-owned security set", err)
		}
	})
	states, err := netlink.XfrmStateList(netlink.FAMILY_ALL)
	if err != nil || len(states) != 4 {
		t.Fatal("canonical four states not installed", err)
	}
	policies, err := netlink.XfrmPolicyList(netlink.FAMILY_ALL)
	if err != nil || len(policies) != 8 {
		t.Fatal("canonical eight policies not installed", err)
	}
	found := map[netlink.Proto]bool{}
	for _, policy := range policies {
		if policy.Dir != netlink.XFRM_DIR_IN || policy.SrcPort != config.PCSCFServerPort || policy.DstPort != config.UEClientPort {
			continue
		}
		if policy.Proto != 6 && policy.Proto != 17 || found[policy.Proto] || policy.Src == nil || policy.Dst == nil ||
			!policy.Src.IP.Equal(config.RemoteIP) || !policy.Dst.IP.Equal(config.LocalIP) || len(policy.Tmpls) != 1 {
			t.Fatal("client inbound selector widened or duplicated")
		}
		if ones, bits := policy.Src.Mask.Size(); ones != bits {
			t.Fatal("client inbound peer prefix is not exact")
		}
		if ones, bits := policy.Dst.Mask.Size(); ones != bits {
			t.Fatal("client inbound UE prefix is not exact")
		}
		template := policy.Tmpls[0]
		if template.Optional != 0 || template.Spi != int(config.UEClientSPI) || template.Reqid != int(imssec.ClientPairReqID(config)) || template.Proto != netlink.XFRM_PROTO_ESP || template.Mode != netlink.XFRM_MODE_TRANSPORT {
			t.Fatal("client inbound traffic does not require the correct ESP SA")
		}
		found[policy.Proto] = true
	}
	if !found[6] || !found[17] || len(found) != 2 {
		t.Fatal("kernel client inbound policy is missing TCP or UDP")
	}
}
