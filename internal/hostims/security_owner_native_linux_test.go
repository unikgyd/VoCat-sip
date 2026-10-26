//go:build linux

package hostims

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"vocat/internal/vowifi/imssec"

	"golang.org/x/sys/unix"
)

// Only the MM/physical modem is fake. Addresses, routes/rules, namespace-bound
// netlink, TC, XFRM, reservation locks and protected sockets are real Linux.
// No registrar, default route, SIM, AKA, REGISTER, phone call or SMS is used.
func TestKernelSecurityOwnerNativeDualStackOverlappingModems(t *testing.T) {
	requireKernelNamespace(t)
	for _, ns := range []string{"net", "mnt"} {
		self, err := os.Stat("/proc/self/ns/" + ns)
		init, other := os.Stat("/proc/1/ns/" + ns)
		if err != nil || other != nil || os.SameFile(self, init) {
			t.Fatal("requires fresh NET+MNT namespaces")
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("requires an empty network namespace")
	}
	sysfs, err := os.ReadDir("/sys/class/net")
	if err != nil || len(sysfs) != 1 || sysfs[0].Name() != "lo" {
		t.Fatal("mount a private sysfs AFTER creating NET+MNT namespaces")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	first, kernel1, runtime1, journal := networkFixture(t, true)
	backend := first.pool.backend.(*fakeBearerBackend)
	selection, err := first.pool.Select(ctx, "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	second, kernel2, runtime2, _ := networkModemFixture(t, true, first.pool, backend, journal, selection)
	networks := []*kernelNetworkLease{networkPrepare(t, first, kernel1, runtime1), networkPrepare(t, second, kernel2, runtime2)}
	kernels := []*linuxNetworkKernel{kernel1, kernel2}
	directory := filepath.Join(t.TempDir(), "security-reservations")
	owners := make([]*kernelSecurityLease, 2)
	for i := range owners {
		k := kernels[i]
		factory := func(r *securityClaim) (scopedXFRMAccess, scopedTCAccess, error) {
			x, err := openScopedXFRM(k.namespace, k.namespaceCookie)
			if err != nil {
				return nil, nil, err
			}
			spec := securityTCSpec{InterfaceIndex: k.index, Mark: r.Mark, Token: r.Token}
			for _, flow := range r.Flows {
				spec.Flows = append(spec.Flows, securityTCFlow{Local: flow.Local, Remote: flow.Remote})
			}
			tc, err := openLinuxSecurityTC(k.namespace, k.namespaceCookie, spec)
			if err != nil {
				x.Close()
				return nil, nil, err
			}
			return x, tc, nil
		}
		owner, err := prepareKernelSecurityWith(ctx, networks[i], directory, factory)
		if err != nil {
			t.Fatalf("prepare real security owner %d: %v", i, err)
		}
		owners[i] = owner
		t.Cleanup(func() {
			if !owner.closed {
				r := owner.record()
				if r != nil {
					t.Logf("retained synthetic scope: phase=%s owned=%d pending=%v", r.Phase, len(r.Operations), r.Pending != nil)
				}
				// Never bypass an ownership failure by deleting kernel objects.
				// Destruction of the wholly private namespace is test authority.
				owner.reservations.closeFDs()
				if owner.xfrm != nil {
					_ = owner.xfrm.Close()
				}
				if owner.tc != nil {
					owner.tc.close()
				}
			}
		})
	}
	if owners[0].record().Mark == owners[1].record().Mark || owners[0].record().ClientReqID == owners[1].record().ClientReqID ||
		owners[0].record().ServerReqID == owners[1].record().ServerReqID {
		t.Fatal("independent owners shared security IDs")
	}
	var handles [2]imssec.Handle
	var sockets [2]net.PacketConn
	for _, v6 := range []bool{false, true} {
		config := securityOwnerConfig()
		if v6 {
			config.LocalIP = runtime1.IPv6.Addr().AsSlice()
			config.RemoteIP = runtime1.PCSCF[1].AsSlice()
		}
		for i, owner := range owners {
			handle, err := owner.InstallIPSec(ctx, config)
			if err != nil {
				t.Fatalf("install owner=%d ipv6=%v: %v", i, v6, err)
			}
			handles[i] = handle
			r := owner.record()
			if r.Phase != "active" || r.Pending != nil || len(r.Operations) != 15 {
				t.Fatal("real owner did not acknowledge 3 TC + 12 XFRM operations")
			}
			if err := networks[i].Verify(ctx); err != nil {
				t.Fatalf("verify real owner %d: %v", i, err)
			}
			local, _ := netip.AddrFromSlice(config.LocalIP)
			remote, _ := netip.AddrFromSlice(config.RemoteIP)
			sockets[i] = nativeOwnerProtectedUDP(t, ctx, owner, kernels[i], local.Unmap(), remote.Unmap(), uint16(config.UEClientPort))
		}
		// Both real sockets bind the same IP and port, separated by device and
		// owner mark; all four SPIs are intentionally identical between owners.
		if sockets[0].LocalAddr().String() != sockets[1].LocalAddr().String() {
			t.Fatal("fixture no longer exercises duplicate local tuple")
		}
		if !v6 {
			if err := handles[0].Close(ctx); err != nil {
				t.Fatal(err)
			}
			r := owners[0].record()
			if err := owners[0].xfrm.ScopeVacant(ctx, r.Mark, r.ClientReqID, r.ServerReqID); !errors.Is(err, ErrXFRMConflict) {
				t.Fatalf("required socket policy vanished when SA handle closed: %v", err)
			}
			if err := sockets[0].Close(); err != nil {
				t.Fatal(err)
			}
			sockets[0] = nil
			if err := owners[0].xfrm.ScopeVacant(ctx, r.Mark, r.ClientReqID, r.ServerReqID); err != nil {
				t.Fatalf("closed socket still reserves reqids: %v", err)
			}
			if err := networks[1].Verify(ctx); err != nil {
				t.Fatalf("first SA close damaged second scope: %v", err)
			}
			if err := sockets[1].Close(); err != nil {
				t.Fatal(err)
			}
			sockets[1] = nil
			if err := handles[1].Close(ctx); err != nil {
				t.Fatal(err)
			}
			continue
		}
	}
	// Closing one complete security/network/bearer must preserve the other
	// modem's IPv6 agreement, TC filters, source rules and protected socket.
	if err := sockets[0].Close(); err != nil {
		t.Fatal(err)
	}
	sockets[0] = nil
	if err := owners[0].Close(ctx); err != nil {
		t.Fatal("close first security owner", err)
	}
	if err := networks[0].Close(ctx); err != nil {
		t.Fatal("close first network owner", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal("close first fake PDN", err)
	}
	if err := networks[1].Verify(ctx); err != nil {
		t.Fatal("first complete owner cleanup damaged second", err)
	}
	if owners[1].record() == nil || owners[1].record().Phase != "active" {
		t.Fatal("second security owner not active")
	}
	if err := sockets[1].Close(); err != nil {
		t.Fatal(err)
	}
	sockets[1] = nil
	// Lose only the fake MM connection, retaining the original physical
	// namespace/netdev anchor. New I/O must fail, while ACK-owned local SA/TC
	// cleanup is allowed via cleanupIdentity without reviving the fake PDN.
	backend.mu.Lock()
	snapshot := backend.bearers[second.claim.record.Path]
	snapshot.Connected = false
	backend.bearers[second.claim.record.Path] = snapshot
	backend.mu.Unlock()
	if err := networks[1].Verify(ctx); err == nil {
		t.Fatal("disconnected fake PDN still authorized new I/O")
	}
	if err := handles[1].Close(ctx); err != nil {
		t.Fatal("known-ACK SA cleanup after fake PDN disconnect", err)
	}
	if err := owners[1].Close(ctx); err != nil {
		t.Fatal("known-ACK TC/scope cleanup after fake PDN disconnect", err)
	}
	if networks[1].record.Security != nil || networks[1].security != nil {
		t.Fatal("security scope retained despite complete ACK cleanup")
	}
	// The pre-existing network lease still requires a connected PDN to remove
	// its own routes; restore only the fake snapshot for that separate cleanup.
	backend.mu.Lock()
	snapshot = backend.bearers[second.claim.record.Path]
	snapshot.Connected = true
	backend.bearers[second.claim.record.Path] = snapshot
	backend.mu.Unlock()
	if err := networks[1].Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("confirmed cleanup left security reservation names")
	}
	t.Log("real owner joint regression passed: two fake modems, overlapping dual-stack IP/P-CSCF/SPI/ports, real independent TC/XFRM/reservation scopes; IPv4 and IPv6 agreements, marked device-bound required-ESP sockets, socket-only reqid fence, independent teardown and known-ACK local cleanup after fake PDN loss")
}

func nativeOwnerProtectedUDP(t *testing.T, ctx context.Context, owner *kernelSecurityLease, kernel *linuxNetworkKernel, local, remote netip.Addr, port uint16) net.PacketConn {
	t.Helper()
	protocol := "udp4"
	if local.Is6() {
		protocol = "udp6"
	}
	binding := imssec.SocketBinding{Local: netip.AddrPortFrom(local, port), Remote: remote, Role: imssec.ClientSocket}
	listen := net.ListenConfig{Control: func(network, address string, raw syscall.RawConn) error {
		if err := owner.BindSocket(ctx, network, address, raw); err != nil {
			return err
		}
		return owner.BindProtectedSocket(ctx, network, binding, raw)
	}}
	conn, err := listen.ListenPacket(ctx, protocol, binding.Local.String())
	if err != nil {
		t.Fatal("protected real UDP socket", err)
	}
	t.Cleanup(func() { conn.Close() })
	raw, err := conn.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var device string
	var mark int
	var cookie uint64
	var inspectErr error
	if err := raw.Control(func(fd uintptr) {
		device, inspectErr = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		if inspectErr != nil {
			return
		}
		mark, inspectErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
		if inspectErr != nil {
			return
		}
		cookie, inspectErr = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	}); err != nil || inspectErr != nil || device != kernel.name || uint32(mark) != owner.record().Mark || cookie != kernel.namespaceCookie {
		t.Fatal("raw socket escaped its real namespace/device/mark owner")
	}
	return conn
}
