//go:build linux

package hostims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func requireKernelNamespace(t *testing.T) {
	t.Helper()
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("explicit isolated network namespace required")
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.Readlink("/proc/1/ns/net")
	if err != nil || current == initial {
		t.Fatal("refusing kernel mutation tests in the host network namespace")
	}
}
func networkFixture(t *testing.T, dual bool) (*BearerLease, *linuxNetworkKernel, Runtime, *memoryClaims) {
	t.Helper()
	requireKernelNamespace(t)
	pool, backend, journal, selection := bearerFixture(t)
	return networkModemFixture(t, dual, pool, backend, journal, selection)
}

var networkFixtureSequence atomic.Uint64

func networkModemFixture(t *testing.T, dual bool, pool *BearerPool, backend *fakeBearerBackend, journal *memoryClaims, selection Selection) (*BearerLease, *linuxNetworkKernel, Runtime, *memoryClaims) {
	t.Helper()
	requireKernelNamespace(t)
	name := "ims" + hashText(fmt.Sprint(t.Name(), networkFixtureSequence.Add(1)))[:8]
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal("isolated dummy interface", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	configuration := BearerConfiguration{Interface: name, Addresses: []BearerAddress{{Address: netip.MustParsePrefix("192.0.2.10/32"), MTU: 1500}}}
	runtime := Runtime{APN: "ims", IPv4: configuration.Addresses[0].Address, PCSCF: []netip.Addr{netip.MustParseAddr("198.51.100.20")}}
	if dual {
		configuration.Addresses = append(configuration.Addresses, BearerAddress{Address: netip.MustParsePrefix("2001:db8:1::10/128"), MTU: 1500})
		runtime.IPv6 = configuration.Addresses[1].Address
		runtime.PCSCF = append(runtime.PCSCF, netip.MustParseAddr("2001:db8:2::20"))
	}
	lease.configuration = configuration
	backend.mu.Lock()
	snapshot := backend.bearers[lease.claim.record.Path]
	snapshot.Interface = name
	snapshot.Addresses = append([]BearerAddress(nil), configuration.Addresses...)
	backend.bearers[lease.claim.record.Path] = snapshot
	backend.mu.Unlock()
	driver, err := openLinuxNetworkKernel(lease, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lease.claim.mu.Lock()
		for _, resource := range lease.claim.networkHolds {
			if n, ok := resource.(*kernelNetworkLease); ok {
				n.driver.close()
			}
		}
		lease.claim.mu.Unlock()
		driver.close()
		_ = netlink.LinkDel(link)
	})
	return lease, driver, runtime, journal
}
func networkPrepare(t *testing.T, bearer *BearerLease, driver kernelNetworkDriver, runtime Runtime) *kernelNetworkLease {
	t.Helper()
	network, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil })
	if err != nil {
		if kernel, ok := driver.(*linuxNetworkKernel); ok && kernel.handle != nil {
			routes, _ := kernel.handle.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: 0}, netlink.RT_FILTER_TABLE)
			rules, _ := kernel.handle.RuleList(netlink.FAMILY_ALL)
			t.Logf("isolated kernel routes=%v rules=%v", routes, rules)
			for _, rule := range rules {
				if rule.Table == kernel.table {
					raw, _ := json.Marshal(rule)
					wanted, _ := json.Marshal(kernel.wantedRule(networkOperation{Local: bearer.configuration.Addresses[0].Address}))
					t.Logf("rule actual=%s wanted=%s", raw, wanted)
				}
			}
			for _, route := range routes {
				if route.Table == kernel.table {
					raw, _ := json.Marshal(route)
					t.Logf("route actual=%s", raw)
				}
			}
		}
		t.Fatal("prepare network", err)
	}
	return network
}
func TestKernelNetworkDualStackSetupBoundSocketsAndCleanup(t *testing.T) {
	bearer, driver, runtime, journal := networkFixture(t, true)
	network := networkPrepare(t, bearer, driver, runtime)
	if err := network.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bearer.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("PDN closed before kernel lease")
	}
	if journal.record(bearer.selection.key).Network.Phase != "active" {
		t.Fatal("network state not durable")
	}
	for _, address := range bearer.configuration.Addresses {
		local := address.Address.Addr()
		protocol := "udp4"
		if local.Is6() {
			protocol = "udp6"
		}
		listen := net.ListenConfig{Control: func(kind, address string, raw syscall.RawConn) error {
			return network.BindSocket(context.Background(), kind, address, raw)
		}}
		connection, err := listen.ListenPacket(context.Background(), protocol, net.JoinHostPort(local.String(), "0"))
		if err != nil {
			t.Fatal("bound media listener", err)
		}
		raw, err := connection.(*net.UDPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		bound := ""
		if err := raw.Control(func(fd uintptr) { bound, _ = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE) }); err != nil {
			t.Fatal(err)
		}
		if bound != bearer.configuration.Interface {
			t.Fatal("socket was not bound to the owned device")
		}
		connection.Close()
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if journal.record(bearer.selection.key).Network != nil {
		t.Fatal("released network fence retained")
	}
	if err := bearer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestKernelNetworkReusesButNeverDeletesExistingAddress(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, false)
	link, err := driver.handle.LinkByIndex(driver.index)
	if err != nil {
		t.Fatal(err)
	}
	address := netlink.Addr{IPNet: ipPrefix(bearer.configuration.Addresses[0].Address)}
	if err := driver.handle.AddrAdd(link, &address); err != nil {
		t.Fatal(err)
	}
	network := networkPrepare(t, bearer, driver, runtime)
	if network.record.Operations[0].Owned {
		t.Fatal("borrowed address marked owned")
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil || len(rows) != 1 || !rows[0].IP.Equal(address.IP) {
		t.Fatal("foreign address removed")
	}
}
func TestKernelNetworkMediaReferenceCountsAndSignalingPin(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, runtime)
	endpoint := Endpoint{Local: runtime.IPv4.Addr(), Remote: runtime.PCSCF[0]}
	signaling, err := network.AcquireMediaRoute(context.Background(), endpoint.Local, endpoint.Remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := signaling.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := network.Verify(context.Background()); err != nil {
		t.Fatal("media release removed signaling route", err)
	}
	endpoint.Remote = netip.MustParseAddr("203.0.113.30")
	first, err := network.AcquireMediaRoute(context.Background(), endpoint.Local, endpoint.Remote)
	if err != nil {
		t.Fatal(err)
	}
	second, err := network.AcquireMediaRoute(context.Background(), endpoint.Local, endpoint.Remote)
	if err != nil {
		t.Fatal(err)
	}
	op, err := network.routeOperation(endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.observe(op); err != nil {
		t.Fatal("shared media route removed early", err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.observe(op); !errors.Is(err, ErrNetworkMissing) {
		t.Fatal("last media route not reclaimed", err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal("duplicate release", err)
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestKernelNetworkForeignRouteReplacementIsPreserved(t *testing.T) {
	bearer, driver, runtime, journal := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, runtime)
	op := network.record.Operations[len(network.record.Operations)-1]
	route := driver.wantedRoute(op)
	route.MTU = 1400
	if err := driver.handle.RouteReplace(route); err != nil {
		t.Fatal(err)
	}
	if err := network.Close(context.Background()); err == nil {
		t.Fatal("changed foreign route deleted")
	}
	rows, err := driver.handle.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: network.record.Table}, netlink.RT_FILTER_TABLE)
	if err != nil || len(rows) != 1 || rows[0].MTU != 1400 {
		t.Fatal("foreign replacement was removed")
	}
	if journal.record(bearer.selection.key).Network.Phase != "uncertain" {
		t.Fatal("conflict not retained")
	}
	if err := bearer.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("conflicting network released PDN")
	}
}

type interceptedNetworkDriver struct {
	kernelNetworkDriver
	once             sync.Once
	closeOnce        sync.Once
	mode             string
	entered, release chan struct{}
	closed           chan struct{}
	removes          atomic.Int32
	applies          atomic.Int32
	journal          *memoryClaims
}

func (d *interceptedNetworkDriver) apply(op networkOperation) error {
	d.applies.Add(1)
	first := false
	d.once.Do(func() { first = true })
	if first && d.mode == "known-reject" {
		return unix.EPERM
	}
	if err := d.kernelNetworkDriver.apply(op); err != nil {
		return err
	}
	if first {
		if d.mode == "storage-after-ack" {
			d.journal.mu.Lock()
			d.journal.fail = true
			d.journal.mu.Unlock()
		}
		if d.entered != nil {
			close(d.entered)
		}
		if d.release != nil {
			<-d.release
		}
		if d.mode == "lost-ack" {
			return ErrUncertain
		}
	}
	return nil
}
func (d *interceptedNetworkDriver) remove(op networkOperation) error {
	d.removes.Add(1)
	return d.kernelNetworkDriver.remove(op)
}
func (d *interceptedNetworkDriver) close() {
	d.closeOnce.Do(func() {
		d.kernelNetworkDriver.close()
		if d.closed != nil {
			close(d.closed)
		}
	})
}
func TestKernelNetworkLostAckNeverGuessesOwnership(t *testing.T) {
	bearer, kernel, runtime, journal := networkFixture(t, false)
	driver := &interceptedNetworkDriver{kernelNetworkDriver: kernel, mode: "lost-ack"}
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrUncertain) {
		t.Fatal("lost ACK accepted", err)
	}
	record := journal.record(bearer.selection.key).Network
	if record == nil || record.Pending == nil || record.Phase != "uncertain" || driver.removes.Load() != 0 {
		t.Fatal("unknown mutation inferred or removed")
	}
	if err := bearer.pool.Recover(context.Background(), bearer.selection); !errors.Is(err, ErrUncertain) {
		t.Fatal("network fence silently recovered")
	}
}
func TestKernelNetworkKnownRejectionHasNoUnknownFence(t *testing.T) {
	bearer, kernel, runtime, journal := networkFixture(t, false)
	driver := &interceptedNetworkDriver{kernelNetworkDriver: kernel, mode: "known-reject"}
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrNetworkConflict) {
		t.Fatal("known rejection misreported", err)
	}
	if journal.record(bearer.selection.key).Network != nil {
		t.Fatal("known unmodified failure retained network fence")
	}
}
func TestKernelNetworkCancellationKeepsPendingUntilObservedCleanup(t *testing.T) {
	bearer, kernel, runtime, journal := networkFixture(t, false)
	driver := &interceptedNetworkDriver{kernelNetworkDriver: kernel, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	var releaseOnce sync.Once
	releaseMutation := func() { releaseOnce.Do(func() { close(driver.release) }) }
	t.Cleanup(func() {
		cancel()
		releaseMutation()
		select {
		case <-driver.closed:
		case <-time.After(20 * time.Second):
			t.Error("canceled worker did not finish closing its driver before fixture cleanup")
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := prepareKernelNetworkWith(ctx, bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil })
		done <- err
	}()
	select {
	case <-driver.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("mutation not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller blocked on mutation")
	}
	if journal.record(bearer.selection.key).Network.Pending == nil {
		t.Fatal("pending fence lost")
	}
	if err := bearer.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("PDN released during mutation")
	}
	releaseMutation()
	awaitClaim(t, journal, bearer.selection.key, func(r claimRecord) bool { return r.Network == nil })
	select {
	case <-driver.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("journal cleared before canceled worker finished closing its driver")
	}
	if driver.removes.Load() != 1 || driver.applies.Load() != 1 {
		t.Fatal("late known resource was not cleaned or new mutations continued after cancellation")
	}
}
func TestKernelNetworkRejectsUnmatchedRuntimeBeforeMutation(t *testing.T) {
	bearer, driver, runtime, journal := networkFixture(t, false)
	runtime.APN = "internet"
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrPCSCF) {
		t.Fatal("wrong APN accepted", err)
	}
	if journal.record(bearer.selection.key).Network != nil {
		t.Fatal("invalid runtime started network ownership")
	}
}

func TestKernelNetworkTwoModemsWithOverlappingDualStackAddresses(t *testing.T) {
	first, driver1, runtime1, journal := networkFixture(t, true)
	backend := first.pool.backend.(*fakeBearerBackend)
	selection, err := first.pool.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	second, driver2, runtime2, _ := networkModemFixture(t, true, first.pool, backend, journal, selection)
	var networks [2]*kernelNetworkLease
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	for index, fixture := range []struct {
		bearer  *BearerLease
		driver  *linuxNetworkKernel
		runtime Runtime
	}{{first, driver1, runtime1}, {second, driver2, runtime2}} {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			network, err := prepareKernelNetworkWith(context.Background(), fixture.bearer, fixture.runtime, func() (kernelNetworkDriver, error) { return fixture.driver, nil })
			networks[index] = network
			errorsOut <- err
		}(index)
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal("parallel kernel lease", err)
		}
	}
	if networks[0].record.Table == networks[1].record.Table {
		t.Fatal("shared policy table")
	}
	for _, network := range networks {
		if err := network.Verify(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := networks[0].Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := networks[1].Verify(context.Background()); err != nil {
		t.Fatal("first modem cleanup affected second", err)
	}
	if err := networks[1].Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKernelNetworkRejectsExistingDefaultWithoutMutation(t *testing.T) {
	bearer, driver, runtime, journal := networkFixture(t, false)
	route := &netlink.Route{LinkIndex: driver.index, Dst: ipPrefix(netip.MustParsePrefix("0.0.0.0/0")), Scope: netlink.SCOPE_LINK}
	if err := driver.handle.RouteAdd(route); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrNetworkConflict) {
		t.Fatal("default route accepted", err)
	}
	if journal.record(bearer.selection.key).Network != nil {
		t.Fatal("default rejection mutated ownership")
	}
	rows, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{LinkIndex: driver.index}, netlink.RT_FILTER_OIF)
	if err != nil || len(rows) != 1 || prefixBits(rows[0].Dst) != 0 {
		t.Fatal("foreign default changed")
	}
}

func TestKernelNetworkPolicyOverrideFailsClosedAndRollsBack(t *testing.T) {
	bearer, driver, runtime, journal := networkFixture(t, false)
	rule := netlink.NewRule()
	rule.Priority, rule.Table, rule.Family, rule.Type = 100, 901, netlink.FAMILY_V4, unix.FR_ACT_BLACKHOLE
	rule.Src = ipPrefix(hostPrefix(runtime.IPv4.Addr()))
	if err := driver.handle.RuleAdd(rule); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.RuleDel(rule) })
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrNetwork) {
		t.Fatal("higher priority policy accepted", err)
	}
	if journal.record(bearer.selection.key).Network != nil {
		t.Fatal("known policy failure was not rolled back")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, current := range rules {
		if current.Priority == 100 && current.Table == 901 {
			found = true
		}
	}
	if !found {
		t.Fatal("foreign blocking policy was removed")
	}
}

func TestKernelNetworkDetectsRuleActionReplacement(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, runtime)
	op := network.record.Operations[1]
	rule := driver.wantedRule(op)
	if err := driver.handle.RuleDel(rule); err != nil {
		t.Fatal(err)
	}
	rule.Type = unix.FR_ACT_BLACKHOLE
	if err := driver.handle.RuleAdd(rule); err != nil {
		t.Fatal(err)
	}
	if err := network.Verify(context.Background()); err == nil {
		t.Fatal("changed rule action accepted")
	}
	if err := network.Close(context.Background()); err == nil {
		t.Fatal("foreign rule action deleted")
	}
	if _, err := driver.ruleHash(netlink.FAMILY_V4); !errors.Is(err, ErrNetworkConflict) {
		t.Fatal("rule action was not preserved", err)
	}
}

func TestKernelNetworkRenameAndMTUChangesInvalidate(t *testing.T) {
	requireKernelNamespace(t)
	for _, action := range []string{"rename", "mtu"} {
		t.Run(action, func(t *testing.T) {
			bearer, driver, runtime, _ := networkFixture(t, false)
			network := networkPrepare(t, bearer, driver, runtime)
			link, err := driver.handle.LinkByIndex(driver.index)
			if err != nil {
				t.Fatal(err)
			}
			if action == "rename" {
				err = driver.handle.LinkSetName(link, "ren"+driver.name[3:])
			} else {
				err = driver.handle.LinkSetMTU(link, 1400)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := network.Verify(context.Background()); err == nil {
				t.Fatal("changed link accepted")
			}
			if err := network.Close(context.Background()); err == nil {
				t.Fatal("changed link cleaned as owned")
			}
		})
	}
}

func TestKernelNetworkSocketBindingRejectsUnknownDestinationAndWrongFamily(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, runtime)
	defer network.Close(context.Background())
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	raw, err := connection.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ kind, address string }{{"udp4", "203.0.113.99:5060"}, {"udp4", "[2001:db8::1]:5060"}, {"udp6", "192.0.2.10:0"}, {"tcp4", "198.51.100.20:5060"}, {"udp-invalid", "192.0.2.10:0"}} {
		if err := network.BindSocket(context.Background(), item.kind, item.address, raw); err == nil {
			t.Fatal("invalid socket binding accepted", item.kind)
		}
	}
}

func TestKernelNetworkRejectsMultipathSocketOutsideTCPESPContract(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, runtime)
	defer network.Close(context.Background())
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_MPTCP)
	if errors.Is(err, unix.EPROTONOSUPPORT) || errors.Is(err, unix.ENOPROTOOPT) {
		t.Skip("kernel does not support MPTCP")
	}
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "private MPTCP socket")
	defer file.Close()
	raw, err := file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := network.BindSocket(context.Background(), "tcp4", net.JoinHostPort(runtime.PCSCF[0].String(), "5060"), raw); err == nil {
		t.Fatal("SOCK_STREAM alone accepted a non-TCP socket")
	}
	if err := network.Verify(context.Background()); err != nil {
		t.Fatal("rejection damaged the owned network", err)
	}
}

func TestKernelNetworkMutationExcludesSIM(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, runtime)
	provider := &SIMProvider{lease: bearer}
	if err := network.locked(context.Background(), func(ctx context.Context) error {
		return network.mutate(ctx, func() error {
			bearer.claim.mu.Lock()
			valid := provider.currentLocked(bearer.claim)
			bearer.claim.mu.Unlock()
			if valid {
				t.Error("SIM allowed during network mutation")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	bearer.claim.mu.Lock()
	valid := provider.currentLocked(bearer.claim)
	bearer.claim.mu.Unlock()
	if !valid {
		t.Fatal("SIM remained blocked after active state restored")
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKernelNetworkDualStackGateways(t *testing.T) {
	bearer, driver, runtime, _ := networkFixture(t, true)
	bearer.configuration.Addresses[0].Gateway = netip.MustParseAddr("192.0.2.254")
	bearer.configuration.Addresses[1].Gateway = netip.MustParseAddr("2001:db8:1::ffff")
	backend := bearer.pool.backend.(*fakeBearerBackend)
	backend.mu.Lock()
	snapshot := backend.bearers[bearer.claim.record.Path]
	snapshot.Addresses = append([]BearerAddress(nil), bearer.configuration.Addresses...)
	backend.bearers[bearer.claim.record.Path] = snapshot
	backend.mu.Unlock()
	network := networkPrepare(t, bearer, driver, runtime)
	if err := network.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKernelNetworkSocketFromAnotherNamespaceIsRejected(t *testing.T) {
	bearer, driver, rt, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, rt)
	defer network.Close(context.Background())
	// Create a socket in a second disposable namespace, restore the thread,
	// then pass the socket to the owner. No packet or host network mutation.
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	other, err := netns.New()
	if err != nil {
		original.Close()
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	fd, socketErr := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err := netns.Set(original); err != nil {
		t.Fatal("cannot restore test thread namespace", err)
	}
	other.Close()
	original.Close()
	runtime.UnlockOSThread()
	if socketErr != nil {
		t.Fatal(socketErr)
	}
	file := os.NewFile(uintptr(fd), "isolated-foreign-socket")
	connection, err := net.FilePacketConn(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	raw, err := connection.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := network.BindSocket(context.Background(), "udp4", "192.0.2.10:0", raw); err == nil {
		t.Fatal("foreign namespace socket accepted")
	}
}

func TestKernelNetworkJournalFailureBeforeIntentDoesNotMutate(t *testing.T) {
	bearer, kernel, rt, journal := networkFixture(t, false)
	driver := &interceptedNetworkDriver{kernelNetworkDriver: kernel}
	journal.mu.Lock()
	journal.fail = true
	journal.mu.Unlock()
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, rt, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	if driver.applies.Load() != 0 {
		t.Fatal("kernel changed without durable intention")
	}
}

func TestKernelNetworkJournalFailureAfterAckRetainsPendingFence(t *testing.T) {
	bearer, kernel, rt, journal := networkFixture(t, false)
	driver := &interceptedNetworkDriver{kernelNetworkDriver: kernel, mode: "storage-after-ack", journal: journal}
	if _, err := prepareKernelNetworkWith(context.Background(), bearer, rt, func() (kernelNetworkDriver, error) { return driver, nil }); !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	journal.mu.Lock()
	journal.fail = false
	journal.mu.Unlock()
	record := journal.record(bearer.selection.key).Network
	if record == nil || record.Pending == nil || driver.applies.Load() != 1 || driver.removes.Load() != 0 {
		t.Fatal("unknown journal outcome was inferred")
	}
	if err := bearer.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("PDN released despite pending write", err)
	}
}

func TestKernelNetworkMediaLimitAndLastRelease(t *testing.T) {
	bearer, driver, rt, _ := networkFixture(t, false)
	network := networkPrepare(t, bearer, driver, rt)
	var refs []*mediaRouteReference
	for index := 1; index <= 8; index++ {
		ref, err := network.AddMedia(context.Background(), Endpoint{Local: rt.IPv4.Addr(), Remote: netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", index))})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if _, err := network.AddMedia(context.Background(), Endpoint{Local: rt.IPv4.Addr(), Remote: netip.MustParseAddr("203.0.113.99")}); !errors.Is(err, ErrBusy) {
		t.Fatal("media limit ignored", err)
	}
	for _, ref := range refs {
		if err := ref.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := network.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKernelNetworkUnknownIPv6RoutePreferenceIsPreserved(t *testing.T) {
	bearer, driver, rt, _ := networkFixture(t, true)
	network := networkPrepare(t, bearer, driver, rt)
	// Independent iproute2 writer exercises a field absent from netlink.Route.
	args := []string{"-6", "route", "change", rt.PCSCF[1].String() + "/128", "table", strconv.Itoa(network.record.Table), "dev", driver.name, "metric", strconv.Itoa(network.record.Table), "proto", "242", "src", rt.IPv6.Addr().String(), "mtu", "1500", "pref", "high"}
	if output, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatal("isolated iproute2 fixture", err, string(output))
	}
	if err := network.Verify(context.Background()); err == nil {
		t.Fatal("unrepresented route preference accepted")
	}
	if err := network.Close(context.Background()); err == nil {
		t.Fatal("foreign route preference deleted")
	}
	rows, err := driver.handle.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: network.record.Table}, netlink.RT_FILTER_TABLE)
	if err != nil || len(rows) != 1 {
		t.Fatal("foreign IPv6 route was removed")
	}
}

func TestKernelNetworkWDSProofIsRevalidatedBeforeReuse(t *testing.T) {
	bearer, driver, _, _ := networkFixture(t, true)
	state := &runtimeFake{rows: map[byte]Runtime{}, binding: hashText("pinned usb topology")}
	for _, address := range bearer.configuration.Addresses {
		row := boundRuntimeRow(address)
		state.rows[row.Family] = row
	}
	source, err := newRuntimeSource(bearer, state.open)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := source.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	network, err := prepareProvenKernelNetworkWith(context.Background(), proof, func() (kernelNetworkDriver, error) { return driver, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := network.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	row := state.rows[4]
	row.PCSCF = []netip.Addr{netip.MustParseAddr("198.51.100.99")}
	state.rows[4] = row
	state.mu.Unlock()
	if err := network.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatal("stale P-CSCF origin accepted", err)
	}
	if _, err := network.AddMedia(context.Background(), Endpoint{Local: proof.Runtime().IPv4.Addr(), Remote: netip.MustParseAddr("203.0.113.20")}); !errors.Is(err, ErrChanged) {
		t.Fatal("stale proof opened a media path", err)
	}
	if err := network.Close(context.Background()); err != nil {
		t.Fatal("origin change blocked owned cleanup", err)
	}
}

func TestKernelNetworkRuntimeBusyDoesNotPoisonClose(t *testing.T) {
	bearer, driver, _, journal := networkFixture(t, true)
	network := networkPrepare(t, bearer, driver, Runtime{APN: "ims", IPv4: bearer.configuration.Addresses[0].Address, IPv6: bearer.configuration.Addresses[1].Address, PCSCF: []netip.Addr{netip.MustParseAddr("198.51.100.20")}})
	state := &runtimeFake{rows: map[byte]Runtime{}, binding: hashText("topology"), entered: make(chan struct{}, 1), release: make(chan struct{})}
	for _, address := range bearer.configuration.Addresses {
		row := boundRuntimeRow(address)
		state.rows[row.Family] = row
	}
	source, err := newRuntimeSource(bearer, state.open)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := source.Read(ctx); done <- err }()
	select {
	case <-state.entered:
	case <-time.After(time.Second):
		t.Fatal("query not reached")
	}
	if err := network.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("temporary runtime query was not treated as busy", err)
	}
	if journal.record(bearer.selection.key).Network.Phase != "active" {
		t.Fatal("temporary busy poisoned the network lease")
	}
	cancel()
	close(state.release)
	<-done
	awaitClaim(t, journal, bearer.selection.key, func(r claimRecord) bool { return !r.RuntimePending })
	if err := network.Close(context.Background()); err != nil {
		t.Fatal("close could not be retried after known busy", err)
	}
}
