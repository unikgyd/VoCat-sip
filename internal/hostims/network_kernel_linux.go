//go:build linux

package hostims

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const networkProtocol = 242
const networkPriority = 20000

var (
	ErrNetwork         = staticError("host IMS: owned kernel network verification failed")
	ErrNetworkConflict = staticError("host IMS: conflicting interface, address or route")
	ErrNetworkMissing  = staticError("host IMS: kernel network object is absent")
	ErrNetworkNotReady = staticError("host IMS: address is not ready")
)

type kernelNetworkDriver interface {
	identity() (string, int, string, string)
	verify(context.Context) error
	tableFree(int) error
	observe(networkOperation) (string, bool, error)
	apply(networkOperation) error
	remove(networkOperation) error
	lookup(Endpoint, int) error
	bindSocket(syscall.RawConn, bool, bool) error
	close()
}
type linuxNetworkKernel struct {
	handle                             *netlink.Handle
	ruleSocket                         *nl.SocketHandle
	namespace                          *os.File
	anchor                             endpointNode
	name                               string
	index, parent, mtu                 int
	kind, mac, namespaceKey, anchorKey string
	table                              int
	namespaceCookie                    uint64
	up                                 bool
}

func namespaceIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if unix.Stat(path, &stat) != nil {
		return "", ErrNetwork
	}
	return hashText(fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)), nil
}
func openLinuxNetworkKernel(lease *BearerLease, fixture bool) (*linuxNetworkKernel, error) {
	if lease == nil || !validInterface(lease.configuration.Interface) {
		return nil, ErrSelection
	}
	namespace, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return nil, ErrNetwork
	}
	handle, err := netlink.NewHandleAt(netns.NsHandle(namespace.Fd()), unix.NETLINK_ROUTE)
	if err != nil {
		namespace.Close()
		return nil, ErrNetwork
	}
	k := &linuxNetworkKernel{handle: handle, namespace: namespace, name: lease.configuration.Interface}
	success := false
	defer func() {
		if !success {
			k.close()
		}
	}()
	if handle.SetSocketTimeout(2*time.Second) != nil {
		return nil, ErrNetwork
	}
	ruleSocket, err := nl.GetNetlinkSocketAt(netns.NsHandle(namespace.Fd()), netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		return nil, ErrNetwork
	}
	k.ruleSocket = &nl.SocketHandle{Socket: ruleSocket}
	timeout := unix.NsecToTimeval((2 * time.Second).Nanoseconds())
	if ruleSocket.SetSendTimeout(&timeout) != nil || ruleSocket.SetReceiveTimeout(&timeout) != nil {
		return nil, ErrNetwork
	}
	k.namespaceCookie, err = unix.GetsockoptUint64(ruleSocket.GetFd(), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil || k.namespaceCookie == 0 {
		return nil, ErrNetwork
	}
	link, err := handle.LinkByName(k.name)
	if err != nil {
		return nil, ErrNetwork
	}
	attrs := link.Attrs()
	if attrs.Index < 1 || attrs.MasterIndex != 0 || attrs.MTU < 68 {
		return nil, ErrNetworkConflict
	}
	k.index, k.parent, k.mtu, k.mac, k.kind = attrs.Index, attrs.ParentIndex, attrs.MTU, attrs.HardwareAddr.String(), link.Type()
	k.up = attrs.Flags&net.FlagUp != 0
	for _, address := range lease.configuration.Addresses {
		if address.Address.Addr().Is6() && k.mtu < 1280 {
			return nil, ErrNetworkConflict
		}
	}
	k.namespaceKey, err = namespaceIdentity("/proc/self/ns/net")
	if err != nil {
		return nil, err
	}
	path, err := filepath.EvalSymlinks(filepath.Join("/sys/class/net", k.name))
	if err != nil {
		return nil, ErrNetwork
	}
	k.anchor, err = pinEndpointNode(path, true)
	if err != nil {
		return nil, ErrNetwork
	}
	k.anchorKey = hashText(fmt.Sprintf("%s:%d:%d", path, k.anchor.stat.Dev, k.anchor.stat.Ino))
	if !netdevSysfsMatches(path, k.index, k.parent) {
		return nil, ErrNetworkConflict
	}
	if !fixture {
		physical := lease.selection.snapshot.Binding.Physical
		device, _ := filepath.EvalSymlinks(filepath.Join(path, "device"))
		lease.claim.mu.Lock()
		single := lease.claim.record.Options.SinglePDN
		lease.claim.mu.Unlock()
		if single {
			if !k.up || device == "" || !withinDirectory(physical, device) {
				return nil, ErrNetworkConflict
			}
		} else {
			// Multiplexed data must have a proven lower link on the selected
			// physical modem. Never treat a virtual interface name as proof.
			if k.parent <= 0 || k.parent == k.index {
				return nil, ErrNetworkConflict
			}
			lower, err := handle.LinkByIndex(k.parent)
			if err != nil || lower.Attrs().Flags&net.FlagUp == 0 {
				return nil, ErrNetworkConflict
			}
			lowerPath, err := filepath.EvalSymlinks(filepath.Join("/sys/class/net", lower.Attrs().Name, "device"))
			if err != nil || !withinDirectory(physical, lowerPath) {
				return nil, ErrNetworkConflict
			}
			if link.Type() != "rmnet" {
				mux, err := os.ReadFile(filepath.Join(path, "qmap/mux_id"))
				id, parseErr := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(mux)), "0x"), 16, 8)
				if err != nil || len(mux) > 16 || !strings.HasPrefix(strings.TrimSpace(string(mux)), "0x") || parseErr != nil || id < 1 || id > 254 {
					return nil, ErrNetworkConflict
				}
			}
		}
	}
	if !k.up {
		// A DOWN interface with existing host configuration belongs to another
		// manager. Only an empty, proven multiplexed bearer may be activated.
		if k.parent <= 0 || k.emptyLink(link) != nil {
			return nil, ErrNetworkConflict
		}
	}
	if err := k.verify(context.Background()); err != nil {
		return nil, err
	}
	success = true
	return k, nil
}

func netdevSysfsMatches(path string, index, parent int) bool {
	for field, wanted := range map[string]int{"ifindex": index, "iflink": parent} {
		if field == "iflink" && wanted == 0 {
			wanted = index
		}
		raw, err := os.ReadFile(filepath.Join(path, field))
		if err != nil || len(raw) > 32 {
			return false
		}
		value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || value != wanted {
			return false
		}
	}
	return true
}
func (k *linuxNetworkKernel) identity() (string, int, string, string) {
	return k.name, k.index, k.namespaceKey, k.anchorKey
}
func (k *linuxNetworkKernel) close() {
	if k.ruleSocket != nil {
		k.ruleSocket.Close()
		k.ruleSocket = nil
	}
	if k.handle != nil {
		k.handle.Close()
		k.handle = nil
	}
	if k.namespace != nil {
		_ = k.namespace.Close()
		k.namespace = nil
	}
	if k.anchor.file != nil {
		_ = k.anchor.file.Close()
		k.anchor.file = nil
	}
}
func (k *linuxNetworkKernel) verify(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, err := namespaceIdentity("/proc/self/ns/net")
	if err != nil || current != k.namespaceKey {
		return ErrNetwork
	}
	var held, path unix.Stat_t
	if k.anchor.file == nil || unix.Fstat(int(k.anchor.file.Fd()), &held) != nil || unix.Stat(k.anchor.path, &path) != nil || !sameEndpointNode(k.anchor.stat, held) || !sameEndpointNode(k.anchor.stat, path) {
		return ErrNetwork
	}
	if !netdevSysfsMatches(k.anchor.path, k.index, k.parent) {
		return ErrNetwork
	}
	link, err := k.handle.LinkByIndex(k.index)
	if err != nil {
		return ErrNetwork
	}
	a := link.Attrs()
	if a.Name != k.name || a.ParentIndex != k.parent || a.MasterIndex != 0 || a.HardwareAddr.String() != k.mac || a.MTU != k.mtu || link.Type() != k.kind || (a.Flags&net.FlagUp != 0) != k.up {
		return ErrNetwork
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := k.handle.RouteListFiltered(family, &netlink.Route{LinkIndex: k.index, Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) > 4096 {
			return ErrNetwork
		}
		for _, route := range routes {
			if route.Dst == nil || route.Dst.Mask != nil && prefixBits(route.Dst) == 0 {
				return ErrNetworkConflict
			}
		}
	}
	return nil
}
func (k *linuxNetworkKernel) tableFree(table int) error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := k.handle.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) != 0 {
			return ErrNetworkConflict
		}
		rules, err := k.handle.RuleList(family)
		if err != nil || len(rules) > 4096 {
			return ErrNetwork
		}
		for _, rule := range rules {
			if rule.Table == table {
				return ErrNetworkConflict
			}
		}
	}
	k.table = table
	return nil
}
func prefixBits(value *net.IPNet) int {
	if value == nil {
		return 0
	}
	bits, _ := value.Mask.Size()
	return bits
}
func ipPrefix(prefix netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: net.IP(prefix.Addr().AsSlice()), Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen())}
}
func hostPrefix(address netip.Addr) netip.Prefix { return netip.PrefixFrom(address, address.BitLen()) }
func networkFamily(address netip.Addr) int {
	if address.Is4() {
		return netlink.FAMILY_V4
	}
	return netlink.FAMILY_V6
}
func networkObjectHash(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return hashText(string(raw))
}
func (k *linuxNetworkKernel) wantedRule(op networkOperation) *netlink.Rule {
	rule := netlink.NewRule()
	rule.Family = networkFamily(op.Local.Addr())
	rule.Table = k.table
	rule.Priority = networkPriority
	rule.OifName = k.name
	rule.Src = ipPrefix(hostPrefix(op.Local.Addr()))
	rule.Protocol = networkProtocol
	rule.Type = unix.FR_ACT_TO_TBL
	return rule
}
func (k *linuxNetworkKernel) wantedRoute(op networkOperation) *netlink.Route {
	route := &netlink.Route{LinkIndex: k.index, Dst: ipPrefix(hostPrefix(op.Remote)), Src: net.IP(op.Local.Addr().AsSlice()), Protocol: networkProtocol, Priority: k.table, Family: networkFamily(op.Remote), Table: k.table, Type: unix.RTN_UNICAST, Scope: netlink.SCOPE_LINK}
	// IPv6 routes are returned with universe scope even for a direct device
	// host route; request that value rather than relaxing snapshot equality.
	if op.Remote.Is6() {
		route.Scope = netlink.SCOPE_UNIVERSE
	}
	if op.Gateway.IsValid() {
		route.Gw = net.IP(op.Gateway.AsSlice())
		route.Flags = unix.RTNH_F_ONLINK
		route.Scope = netlink.SCOPE_UNIVERSE
	}
	if op.MTU > 0 {
		route.MTU = int(op.MTU)
	}
	return route
}

type kernelRuleSnapshot struct {
	Rule    netlink.Rule
	RawHash string
}

type kernelRouteSnapshot struct {
	Route   netlink.Route
	RawHash string
}

// netlink v1.3.1 RuleList omits the fib_rule_hdr action. Independently check
// that header and preserve the complete raw snapshot (including attributes
// omitted by the typed decoder); never equate blackhole/unreachable with a
// table lookup. Unknown selectors are rejected rather than silently ignored.
func (k *linuxNetworkKernel) ruleHash(family int) (string, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: k.ruleSocket}
	request.AddData(nl.NewIfInfomsg(family))
	var result string
	var invalid bool
	count := 0
	err := request.ExecuteIter(unix.NETLINK_ROUTE, unix.RTM_NEWRULE, func(raw []byte) bool {
		count++
		if len(raw) < 12 || count > 4096 {
			invalid = true
			return false
		}
		attributes, err := strictKernelAttrs(raw[12:])
		if err != nil {
			invalid = true
			return false
		}
		table := uint32(raw[4])
		for _, attr := range attributes {
			if attr.Attr.Type == unix.FRA_TABLE {
				if len(attr.Value) != 4 {
					invalid = true
					return false
				}
				table = binary.NativeEndian.Uint32(attr.Value)
			}
		}
		if table != uint32(k.table) {
			return true
		}
		if result != "" || int(raw[0]) != family || raw[7] != unix.FR_ACT_TO_TBL || raw[3] != 0 || raw[8] != 0 || raw[9] != 0 || raw[10] != 0 || raw[11] != 0 {
			invalid = true
			return false
		}
		seen := make(map[uint16]bool)
		for _, attr := range attributes {
			if seen[attr.Attr.Type] {
				invalid = true
				return false
			}
			seen[attr.Attr.Type] = true
			switch attr.Attr.Type {
			case unix.FRA_SRC, unix.FRA_OIFNAME, unix.FRA_TABLE, unix.FRA_PRIORITY, unix.FRA_PROTOCOL:
			case unix.FRA_SUPPRESS_PREFIXLEN, unix.FRA_SUPPRESS_IFGROUP:
				if len(attr.Value) != 4 || binary.NativeEndian.Uint32(attr.Value) != 0xffffffff {
					invalid = true
					return false
				}
			default:
				invalid = true
				return false
			}
		}
		result = hashText(string(raw))
		return true
	})
	if err != nil || invalid {
		return "", ErrNetworkConflict
	}
	if result == "" {
		return "", ErrNetworkMissing
	}
	return result, nil
}

// RouteList also omits some selectors/metrics. Keep the raw configured route
// bytes, reject unsupported attributes, and omit only the kernel's volatile
// usage cache after proving it has no expiry. This is not an ABA guarantee
// against a privileged process recreating an identical object.
func (k *linuxNetworkKernel) routeHash(op networkOperation) (string, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETROUTE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: k.ruleSocket}
	message := nl.NewRtMsg()
	message.Family = uint8(networkFamily(op.Remote))
	request.AddData(message)
	var result string
	invalid, count := false, 0
	err := request.ExecuteIter(unix.NETLINK_ROUTE, unix.RTM_NEWROUTE, func(raw []byte) bool {
		count++
		if len(raw) < 12 || count > 4096 {
			invalid = true
			return false
		}
		attrs, err := strictKernelAttrs(raw[12:])
		if err != nil {
			invalid = true
			return false
		}
		table := uint32(raw[4])
		var destination netip.Addr
		for _, attr := range attrs {
			switch attr.Attr.Type {
			case unix.RTA_TABLE:
				if len(attr.Value) != 4 {
					invalid = true
					return false
				}
				table = binary.NativeEndian.Uint32(attr.Value)
			case unix.RTA_DST:
				destination, _ = netip.AddrFromSlice(attr.Value)
			}
		}
		if table != uint32(k.table) || destination != op.Remote {
			return true
		}
		if result != "" || int(raw[0]) != networkFamily(op.Remote) || int(raw[1]) != op.Remote.BitLen() {
			invalid = true
			return false
		}
		canonical := append([]byte(nil), raw[:12]...)
		seen := make(map[uint16]bool)
		for _, attr := range attrs {
			if seen[attr.Attr.Type] {
				invalid = true
				return false
			}
			seen[attr.Attr.Type] = true
			switch attr.Attr.Type {
			case unix.RTA_DST, unix.RTA_OIF, unix.RTA_PRIORITY, unix.RTA_PREFSRC, unix.RTA_TABLE, unix.RTA_GATEWAY:
			case unix.RTA_METRICS:
				metrics, err := strictKernelAttrs(attr.Value)
				if err != nil || len(metrics) != 1 || metrics[0].Attr.Type != unix.RTAX_MTU || len(metrics[0].Value) != 4 {
					invalid = true
					return false
				}
			case unix.RTA_PREF:
				if op.Remote.Is4() || len(attr.Value) != 1 || attr.Value[0] != 0 {
					invalid = true
					return false
				}
			case unix.RTA_CACHEINFO:
				if len(attr.Value) != 32 || binary.NativeEndian.Uint32(attr.Value[8:12]) != 0 {
					invalid = true
					return false
				}
				continue
			default:
				invalid = true
				return false
			}
			canonical = binary.NativeEndian.AppendUint16(canonical, attr.Attr.Type)
			canonical = binary.NativeEndian.AppendUint16(canonical, uint16(len(attr.Value)))
			canonical = append(canonical, attr.Value...)
		}
		result = hashText(string(canonical))
		return true
	})
	if err != nil || invalid {
		return "", ErrNetworkConflict
	}
	if result == "" {
		return "", ErrNetworkMissing
	}
	return result, nil
}
func (k *linuxNetworkKernel) find(op networkOperation) (any, bool, error) {
	switch strings.TrimPrefix(op.Kind, "remove-") {
	case "accept_ra", "autoconf":
		value, err := k.ipv6Setting(strings.TrimPrefix(op.Kind, "remove-"), nil)
		if err != nil {
			return nil, false, err
		}
		if value != 0 {
			if value == op.Previous {
				return nil, false, ErrNetworkMissing
			}
			return nil, false, ErrNetworkConflict
		}
		return &kernelIPv6Snapshot{Kind: strings.TrimPrefix(op.Kind, "remove-"), Index: k.index, Anchor: k.anchorKey}, true, nil
	case "link":
		if err := k.verify(context.Background()); err != nil {
			return nil, false, err
		}
		if !k.up {
			return nil, false, ErrNetworkMissing
		}
		return &kernelLinkSnapshot{k.index, k.anchorKey, true}, true, nil
	case "address":
		link, err := k.handle.LinkByIndex(k.index)
		if err != nil {
			return nil, false, ErrNetwork
		}
		rows, err := k.handle.AddrList(link, networkFamily(op.Local.Addr()))
		if err != nil || len(rows) > 128 {
			return nil, false, ErrNetwork
		}
		var found *netlink.Addr
		for _, row := range rows {
			if row.IPNet == nil || !row.IP.Equal(net.IP(op.Local.Addr().AsSlice())) {
				continue
			}
			if found != nil || prefixBits(row.IPNet) != op.Local.Bits() {
				return nil, false, ErrNetworkConflict
			}
			copy := row
			found = &copy
		}
		if found == nil {
			return nil, false, ErrNetworkMissing
		}
		ready := found.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED) == 0
		if found.Flags&unix.IFA_F_PERMANENT == 0 || found.Scope != unix.RT_SCOPE_UNIVERSE {
			return nil, false, ErrNetworkConflict
		}
		found.Flags &^= unix.IFA_F_TENTATIVE | unix.IFA_F_DADFAILED
		return found, ready, nil
	case "rule":
		rows, err := k.handle.RuleList(networkFamily(op.Local.Addr()))
		if err != nil || len(rows) > 4096 {
			return nil, false, ErrNetwork
		}
		var found *netlink.Rule
		for _, row := range rows {
			if row.Table != k.table {
				continue
			}
			if found != nil {
				return nil, false, ErrNetworkConflict
			}
			copy := row
			found = &copy
		}
		if found == nil {
			return nil, false, ErrNetworkMissing
		}
		wanted := k.wantedRule(op)
		wanted.Type = 0 // Read decoder omits action; ruleHash checks the actual header.
		if networkObjectHash(found) != networkObjectHash(wanted) {
			return nil, false, ErrNetworkConflict
		}
		rawHash, err := k.ruleHash(networkFamily(op.Local.Addr()))
		if err != nil {
			return nil, false, err
		}
		return &kernelRuleSnapshot{Rule: *found, RawHash: rawHash}, true, nil
	case "route":
		rows, err := k.handle.RouteListFiltered(networkFamily(op.Remote), &netlink.Route{Table: k.table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(rows) > 32 {
			return nil, false, ErrNetwork
		}
		var found *netlink.Route
		for _, row := range rows {
			if row.Dst == nil || !row.Dst.IP.Equal(net.IP(op.Remote.AsSlice())) || prefixBits(row.Dst) != op.Remote.BitLen() {
				continue
			}
			if found != nil {
				return nil, false, ErrNetworkConflict
			}
			copy := row
			found = &copy
		}
		if found == nil {
			return nil, false, ErrNetworkMissing
		}
		if networkObjectHash(found) != networkObjectHash(k.wantedRoute(op)) {
			return nil, false, ErrNetworkConflict
		}
		rawHash, err := k.routeHash(op)
		if err != nil {
			return nil, false, err
		}
		return &kernelRouteSnapshot{Route: *found, RawHash: rawHash}, true, nil
	}
	return nil, false, ErrParameters
}
func (k *linuxNetworkKernel) observe(op networkOperation) (string, bool, error) {
	value, ready, err := k.find(op)
	if err != nil {
		return "", false, err
	}
	return networkObjectHash(value), ready, nil
}
func (k *linuxNetworkKernel) apply(op networkOperation) error {
	switch op.Kind {
	case "accept_ra", "autoconf":
		value, err := k.ipv6Setting(op.Kind, nil)
		if err != nil {
			return err
		}
		if k.up || value != op.Previous || value == 0 {
			return ErrNetworkConflict
		}
		zero := uint32(0)
		_, err = k.ipv6Setting(op.Kind, &zero)
		return err
	case "link":
		if k.up || k.parent <= 0 || k.verify(context.Background()) != nil {
			return ErrNetworkConflict
		}
		link, err := k.handle.LinkByIndex(k.index)
		if err != nil || k.emptyLink(link) != nil {
			return ErrNetworkConflict
		}
		if err := k.handle.LinkSetUp(link); err != nil {
			return err
		}
		k.up = true
		return nil
	case "address":
		link, err := k.handle.LinkByIndex(k.index)
		if err != nil {
			return ErrNetwork
		}
		return k.handle.AddrAdd(link, &netlink.Addr{IPNet: ipPrefix(op.Local), Flags: unix.IFA_F_NOPREFIXROUTE})
	case "rule":
		return k.handle.RuleAdd(k.wantedRule(op))
	case "route":
		return k.handle.RouteAdd(k.wantedRoute(op))
	}
	return ErrParameters
}
func (k *linuxNetworkKernel) remove(op networkOperation) error {
	value, _, err := k.find(op)
	if err != nil {
		return err
	}
	if op.Hash == "" || networkObjectHash(value) != op.Hash {
		return ErrNetworkConflict
	}
	switch item := value.(type) {
	case *kernelIPv6Snapshot:
		if k.up {
			return ErrNetworkConflict
		}
		_, err := k.ipv6Setting(item.Kind, &op.Previous)
		return err
	case *kernelLinkSnapshot:
		link, err := k.handle.LinkByIndex(k.index)
		if err != nil || k.emptyLink(link) != nil {
			return ErrNetworkConflict
		}
		if err := k.handle.LinkSetDown(link); err != nil {
			return err
		}
		k.up = false
		return nil
	case *netlink.Addr:
		link, err := k.handle.LinkByIndex(k.index)
		if err != nil {
			return ErrNetwork
		}
		return k.handle.AddrDel(link, item)
	case *kernelRuleSnapshot:
		item.Rule.Type = unix.FR_ACT_TO_TBL
		return k.handle.RuleDel(&item.Rule)
	case *kernelRouteSnapshot:
		return k.handle.RouteDel(&item.Route)
	}
	return ErrParameters
}

type kernelLinkSnapshot struct {
	Index  int
	Anchor string
	Up     bool
}

type kernelIPv6Snapshot struct {
	Kind   string
	Index  int
	Anchor string
}

// Only a previously proven, pinned bearer interface may change these two
// settings. Each mutation has its own durable intent and original value.
func (k *linuxNetworkKernel) ipv6Setting(kind string, wanted *uint32) (uint32, error) {
	if kind != "accept_ra" && kind != "autoconf" || k.verify(context.Background()) != nil {
		return 0, ErrNetwork
	}
	path := filepath.Join("/proc/sys/net/ipv6/conf", k.name, kind)
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if wanted != nil {
		flags = unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return 0, ErrNetwork
	}
	defer unix.Close(fd)
	if wanted != nil {
		text := strconv.FormatUint(uint64(*wanted), 10) + "\n"
		if count, err := unix.Write(fd, []byte(text)); err != nil || count != len(text) {
			return 0, ErrNetwork
		}
		if _, err := unix.Seek(fd, 0, 0); err != nil {
			return 0, ErrNetwork
		}
	}
	var raw [16]byte
	count, err := unix.Read(fd, raw[:])
	if err != nil || count == len(raw) {
		return 0, ErrNetwork
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw[:count])), 10, 32)
	if err != nil || value > 2 || kind == "autoconf" && value > 1 || wanted != nil && value != uint64(*wanted) {
		return 0, ErrNetwork
	}
	return uint32(value), nil
}

func (k *linuxNetworkKernel) emptyLink(link netlink.Link) error {
	if link == nil || link.Attrs().Index != k.index {
		return ErrNetworkConflict
	}
	addresses, err := k.handle.AddrList(link, netlink.FAMILY_ALL)
	if err != nil || len(addresses) > 128 {
		return ErrNetwork
	}
	for _, address := range addresses {
		// Link-local IPv6 is generated by the kernel when our link comes UP.
		if address.IP == nil || !address.IP.IsLinkLocalUnicast() || address.Scope != unix.RT_SCOPE_LINK {
			return ErrNetworkConflict
		}
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := k.handle.RouteListFiltered(family, &netlink.Route{LinkIndex: k.index, Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) > 4096 {
			return ErrNetwork
		}
		for _, route := range routes {
			if route.Protocol != unix.RTPROT_KERNEL || route.Dst == nil || !route.Dst.IP.IsLinkLocalUnicast() && !route.Dst.IP.IsMulticast() {
				return ErrNetworkConflict
			}
		}
	}
	return nil
}
func (k *linuxNetworkKernel) lookup(endpoint Endpoint, table int) error {
	return k.lookupMarked(endpoint, table, 0)
}

func (k *linuxNetworkKernel) lookupMarked(endpoint Endpoint, table int, mark uint32) error {
	if k == nil || k.handle == nil || k.namespace == nil {
		return ErrNetwork
	}
	rows, err := k.handle.RouteGetWithOptions(net.IP(endpoint.Remote.AsSlice()), &netlink.RouteGetOptions{SrcAddr: net.IP(endpoint.Local.AsSlice()), OifIndex: k.index, Mark: mark, FIBMatch: true})
	if err != nil || len(rows) != 1 || rows[0].LinkIndex != k.index || rows[0].Table != table || rows[0].Type != unix.RTN_UNICAST {
		return ErrNetwork
	}
	return nil
}

func (k *linuxNetworkKernel) bindSocket(raw syscall.RawConn, ipv4, stream bool) error {
	result := ErrNetwork
	err := raw.Control(func(fd uintptr) {
		cookie, err := unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
		if err != nil || cookie != k.namespaceCookie {
			return
		}
		family := unix.AF_INET6
		if ipv4 {
			family = unix.AF_INET
		}
		kind := unix.SOCK_DGRAM
		if stream {
			kind = unix.SOCK_STREAM
		}
		actualFamily, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DOMAIN)
		if err != nil || actualFamily != family {
			return
		}
		actualKind, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		if err != nil || actualKind != kind {
			return
		}
		protocol := unix.IPPROTO_UDP
		if stream {
			protocol = unix.IPPROTO_TCP
		}
		actualProtocol, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
		if err != nil || actualProtocol != protocol {
			return
		}
		bound, err := unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		if err != nil || bound != "" && bound != k.name {
			return
		}
		if unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, k.name) != nil {
			return
		}
		bound, err = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		if err == nil && bound == k.name {
			result = ""
		}
	})
	if err != nil || result != "" {
		return ErrNetwork
	}
	return nil
}
