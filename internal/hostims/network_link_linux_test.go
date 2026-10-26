//go:build linux

package hostims

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestKernelOwnedDownLinkLifecycle(t *testing.T) {
	bearer, original, runtime, journal := networkFixture(t, true)
	parent, err := netlink.LinkByName(original.name)
	if err != nil {
		t.Fatal(err)
	}
	name := "mux" + hashText(t.Name())[:8]
	child := &netlink.IPVlan{LinkAttrs: netlink.LinkAttrs{Name: name, ParentIndex: parent.Attrs().Index}, Mode: netlink.IPVLAN_MODE_L3}
	if err := netlink.LinkAdd(child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(child) })
	bearer.configuration.Interface = name
	backend := bearer.pool.backend.(*fakeBearerBackend)
	backend.mu.Lock()
	snapshot := backend.bearers[bearer.claim.record.Path]
	snapshot.Interface = name
	backend.bearers[bearer.claim.record.Path] = snapshot
	backend.mu.Unlock()
	driver, err := openLinuxNetworkKernel(bearer, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(driver.close)
	originals := map[string]string{}
	for _, kind := range []string{"accept_ra", "autoconf"} {
		raw, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv6/conf", name, kind))
		if err != nil {
			t.Fatal(err)
		}
		originals[kind] = string(raw)
	}
	network := networkPrepare(t, bearer, driver, runtime)
	link, err := netlink.LinkByName(name)
	if err != nil || link.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("owned link was not brought UP", err)
	}
	record := journal.record(bearer.selection.key).Network
	sawLink := false
	for _, op := range record.Operations {
		if op.Kind == "link" {
			sawLink = op.Owned && hashString(op.Hash)
		}
	}
	if !sawLink {
		t.Fatal("link UP lacks durable acknowledged ownership")
	}
	for kind := range originals {
		raw, _ := os.ReadFile(filepath.Join("/proc/sys/net/ipv6/conf", name, kind))
		if string(raw) != "0\n" {
			t.Fatal("IPv6 auto configuration not suppressed", kind)
		}
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	link, err = netlink.LinkByName(name)
	if err != nil || link.Attrs().Flags&net.FlagUp != 0 {
		t.Fatal("owned link was not restored DOWN", err)
	}
	for kind, wanted := range originals {
		raw, _ := os.ReadFile(filepath.Join("/proc/sys/net/ipv6/conf", name, kind))
		if string(raw) != wanted {
			t.Fatal("original IPv6 setting not restored", kind)
		}
	}
	if journal.record(bearer.selection.key).Network != nil {
		t.Fatal("network claim survived acknowledged cleanup")
	}
	if err := bearer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkLinkOperationValidation(t *testing.T) {
	for _, kind := range []string{"link", "accept_ra", "autoconf"} {
		r := claimNetworkFixture()
		op := networkOperation{Kind: kind, Local: r.Operations[0].Local, Owned: true, Hash: hashText(kind)}
		if kind != "link" {
			op.Previous = 1
		}
		r.Operations = []networkOperation{op}
		if !validNetworkClaim(r) {
			t.Fatal("valid link operation rejected", kind)
		}
		for _, mutate := range []func(*networkOperation){func(o *networkOperation) { o.Owned = false }, func(o *networkOperation) { o.Previous = 3 }, func(o *networkOperation) { o.Media = true }} {
			copy := cloneNetworkClaim(r)
			mutate(&copy.Operations[0])
			if validNetworkClaim(copy) {
				t.Fatal("invalid link operation accepted", kind)
			}
		}
	}
}
