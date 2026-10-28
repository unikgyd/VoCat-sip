//go:build linux

package hostims

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type wdsTopology interface {
	Verify(context.Context) error
	Binding() wdsBinding
	Key() string
	Close() error
}
type wdsNetdev struct {
	node            endpointNode
	name, kind, mac string
	index, parent   int
}
type wdsLinkReader interface {
	LinkByName(string) (netlink.Link, error)
	LinkByIndex(int) (netlink.Link, error)
	SetSocketTimeout(time.Duration) error
	Close()
}
type linuxWDSTopology struct {
	namespace    *os.File
	handle       wdsLinkReader
	sys          string
	namespaceKey string
	nodes        []endpointNode
	links        []wdsNetdev
	function     string
	binding      wdsBinding
	key          string
	closed       bool
}

func usbFunction(path, physical string) (string, uint32, error) {
	for depth := 0; depth < 16 && withinDirectory(physical, path); depth++ {
		raw, err := os.ReadFile(filepath.Join(path, "bInterfaceNumber"))
		if err == nil {
			value, parseErr := strconv.ParseUint(strings.TrimSpace(string(raw)), 16, 8)
			subsystem, linkErr := filepath.EvalSymlinks(filepath.Join(path, "subsystem"))
			if len(raw) > 8 || parseErr != nil || linkErr != nil || filepath.Base(subsystem) != "usb" {
				return "", 0, ErrQMIEndpoint
			}
			return path, uint32(value), nil
		}
		path = filepath.Dir(path)
	}
	return "", 0, ErrQMIEndpoint
}

func pinWDSTopology(lease *BearerLease, control *qmiEndpointPin) (*linuxWDSTopology, error) {
	return pinWDSTopologyWith(lease, control, "/sys", func(namespace *os.File) (wdsLinkReader, error) {
		handle, err := netlink.NewHandleAt(netnsHandle(namespace), unix.NETLINK_ROUTE)
		if err != nil {
			return nil, err
		}
		return handle, nil
	})
}
func pinWDSTopologyWith(lease *BearerLease, control *qmiEndpointPin, sys string, open func(*os.File) (wdsLinkReader, error)) (*linuxWDSTopology, error) {
	if lease == nil || control == nil || !filepath.IsAbs(sys) || open == nil {
		return nil, ErrSelection
	}
	n := &linuxWDSTopology{sys: sys}
	var err error
	n.namespace, err = os.Open("/proc/self/ns/net")
	if err != nil {
		return nil, ErrQMIEndpoint
	}
	success := false
	defer func() {
		if !success {
			_ = n.Close()
		}
	}()
	n.namespaceKey, err = namespaceIdentity("/proc/self/ns/net")
	if err != nil {
		return nil, err
	}
	n.handle, err = open(n.namespace)
	if err != nil {
		n.handle = nil
		return nil, ErrQMIEndpoint
	}
	if n.handle == nil || n.handle.SetSocketTimeout(2*time.Second) != nil {
		return nil, ErrQMIEndpoint
	}
	physical := control.physical
	n.function, n.binding.Interface, err = usbFunction(control.charTarget, physical)
	if err != nil {
		return nil, err
	}
	n.binding.Endpoint = 2 // HSUSB, proven by the USB function, never inferred for MHI.
	function, err := pinEndpointNode(n.function, true)
	if err != nil {
		return nil, err
	}
	n.nodes = append(n.nodes, function)
	addLink := func(link netlink.Link) error {
		a := link.Attrs()
		if a.Index < 1 || a.MasterIndex != 0 || !validInterface(a.Name) {
			return ErrQMIEndpoint
		}
		path, err := filepath.EvalSymlinks(filepath.Join(n.sys, "class/net", a.Name))
		if err != nil || !netdevSysfsMatches(path, a.Index, a.ParentIndex) {
			return ErrQMIEndpoint
		}
		node, err := pinEndpointNode(path, true)
		if err != nil {
			return err
		}
		n.links = append(n.links, wdsNetdev{node: node, name: a.Name, index: a.Index, parent: a.ParentIndex, kind: link.Type(), mac: a.HardwareAddr.String()})
		return nil
	}
	link, err := n.handle.LinkByName(lease.configuration.Interface)
	if err != nil {
		return nil, ErrQMIEndpoint
	}
	if err := addLink(link); err != nil {
		return nil, err
	}
	lease.claim.mu.Lock()
	single := lease.claim.record.Options.SinglePDN
	lease.claim.mu.Unlock()
	lower := link
	if !single {
		if link.Attrs().ParentIndex < 1 || link.Attrs().ParentIndex == link.Attrs().Index {
			return nil, ErrQMIEndpoint
		}
		lower, err = n.handle.LinkByIndex(link.Attrs().ParentIndex)
		if err != nil {
			return nil, ErrQMIEndpoint
		}
		if err := addLink(lower); err != nil {
			return nil, err
		}
		if link.Type() == "rmnet" {
			n.binding.Mux, err = readRmnetMux(n.namespace, link.Attrs().Index)
		} else {
			n.binding.Mux, err = readQmapMux(n.links[0].node.path)
		}
		if err != nil {
			return nil, err
		}
	}
	device, err := filepath.EvalSymlinks(filepath.Join(n.sys, "class/net", lower.Attrs().Name, "device"))
	if err != nil || device != n.function {
		return nil, ErrQMIEndpoint
	}
	if err := n.Verify(context.Background()); err != nil {
		return nil, err
	}
	keys := n.linkKeys()
	for _, node := range control.nodes {
		keys = append(keys, fmt.Sprintf("%s:%d:%d:%d:%d:%d", node.path, node.stat.Dev, node.stat.Ino, node.stat.Rdev, node.stat.Ctim.Sec, node.stat.Ctim.Nsec))
	}
	n.key = hashText(fmt.Sprintf("%s:%s:%v:%v", n.namespaceKey, n.function, n.binding, keys))
	success = true
	return n, nil
}
func (n *linuxWDSTopology) linkKeys() []string {
	var keys []string
	for _, link := range n.links {
		keys = append(keys, fmt.Sprintf("%s:%d:%d:%d:%d", link.node.path, link.node.stat.Dev, link.node.stat.Ino, link.index, link.parent))
	}
	return keys
}
func readQmapMux(path string) (byte, error) {
	raw, err := os.ReadFile(filepath.Join(path, "qmap/mux_id"))
	text := strings.TrimSpace(string(raw))
	value, parseErr := strconv.ParseUint(strings.TrimPrefix(text, "0x"), 16, 8)
	if err != nil || len(raw) > 16 || !strings.HasPrefix(text, "0x") || parseErr != nil || value < 1 || value > 254 {
		return 0, ErrQMIEndpoint
	}
	return byte(value), nil
}
func readRmnetMux(namespace *os.File, index int) (byte, error) {
	socket, err := nl.GetNetlinkSocketAt(netnsHandle(namespace), netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		return 0, ErrQMIEndpoint
	}
	defer socket.Close()
	timeout := unix.NsecToTimeval((2 * time.Second).Nanoseconds())
	if socket.SetSendTimeout(&timeout) != nil || socket.SetReceiveTimeout(&timeout) != nil {
		return 0, ErrQMIEndpoint
	}
	request := nl.NewNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_REQUEST)
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: {Socket: socket}}
	message := nl.NewIfInfomsg(unix.AF_UNSPEC)
	message.Index = int32(index)
	request.AddData(message)
	rows, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if err != nil || len(rows) != 1 {
		return 0, ErrQMIEndpoint
	}
	return parseRmnetMux(rows[0], index)
}
func parseRmnetMux(raw []byte, index int) (byte, error) {
	if len(raw) < 16 || len(raw) > 65536 || int32(binary.NativeEndian.Uint32(raw[4:8])) != int32(index) {
		return 0, ErrQMIEndpoint
	}
	attrs, err := strictKernelAttrs(raw[16:])
	if err != nil {
		return 0, ErrQMIEndpoint
	}
	var info []byte
	for _, attr := range attrs {
		if attr.Attr.Type&0x3fff == unix.IFLA_LINKINFO {
			if attr.Attr.Type&0x4000 != 0 {
				return 0, ErrQMIEndpoint
			}
			if info != nil {
				return 0, ErrQMIEndpoint
			}
			info = attr.Value
		}
	}
	nested, err := strictKernelAttrs(info)
	if err != nil {
		return 0, ErrQMIEndpoint
	}
	var kind, data []byte
	for _, attr := range nested {
		if attr.Attr.Type&0x4000 != 0 {
			return 0, ErrQMIEndpoint
		}
		switch attr.Attr.Type & 0x3fff {
		case unix.IFLA_INFO_KIND:
			if attr.Attr.Type&0x8000 != 0 {
				return 0, ErrQMIEndpoint
			}
			if kind != nil {
				return 0, ErrQMIEndpoint
			}
			kind = attr.Value
		case unix.IFLA_INFO_DATA:
			if data != nil {
				return 0, ErrQMIEndpoint
			}
			data = attr.Value
		default:
			return 0, ErrQMIEndpoint
		}
	}
	if string(kind) != "rmnet\x00" || data == nil {
		return 0, ErrQMIEndpoint
	}
	fields, err := strictKernelAttrs(data)
	if err != nil {
		return 0, ErrQMIEndpoint
	}
	mux := uint16(0)
	seen := map[uint16]bool{}
	for _, attr := range fields {
		if attr.Attr.Type&0xc000 != 0 {
			return 0, ErrQMIEndpoint
		}
		kind := attr.Attr.Type & 0x3fff
		if seen[kind] {
			return 0, ErrQMIEndpoint
		}
		seen[kind] = true
		switch kind {
		case 1:
			if len(attr.Value) != 2 {
				return 0, ErrQMIEndpoint
			}
			mux = binary.NativeEndian.Uint16(attr.Value)
		case 2:
			if len(attr.Value) != 8 {
				return 0, ErrQMIEndpoint
			}
		default:
			continue
		}
	}
	if mux < 1 || mux > 254 {
		return 0, ErrQMIEndpoint
	}
	return byte(mux), nil
}
func (n *linuxWDSTopology) Binding() wdsBinding { return n.binding }
func (n *linuxWDSTopology) Key() string         { return n.key }
func (n *linuxWDSTopology) Verify(ctx context.Context) error {
	if n == nil || n.closed || ctx.Err() != nil {
		return ErrChanged
	}
	namespace, err := namespaceIdentity("/proc/self/ns/net")
	if err != nil || namespace != n.namespaceKey {
		return ErrChanged
	}
	for _, node := range n.nodes {
		var held, current unix.Stat_t
		if unix.Fstat(int(node.file.Fd()), &held) != nil || unix.Lstat(node.path, &current) != nil || !sameEndpointNode(node.stat, held) || !sameEndpointNode(node.stat, current) {
			return ErrChanged
		}
	}
	raw, err := os.ReadFile(filepath.Join(n.function, "bInterfaceNumber"))
	value, parseErr := strconv.ParseUint(strings.TrimSpace(string(raw)), 16, 8)
	if err != nil || len(raw) > 8 || parseErr != nil || uint32(value) != n.binding.Interface {
		return ErrChanged
	}
	for _, link := range n.links {
		var held, current unix.Stat_t
		classPath, err := filepath.EvalSymlinks(filepath.Join(n.sys, "class/net", link.name))
		if err != nil || classPath != link.node.path {
			return ErrChanged
		}
		if unix.Fstat(int(link.node.file.Fd()), &held) != nil || unix.Lstat(link.node.path, &current) != nil || !sameEndpointNode(link.node.stat, held) || !sameEndpointNode(link.node.stat, current) || !netdevSysfsMatches(link.node.path, link.index, link.parent) {
			return ErrChanged
		}
		actual, err := n.handle.LinkByIndex(link.index)
		if err != nil {
			return ErrChanged
		}
		a := actual.Attrs()
		if a.Name != link.name || a.ParentIndex != link.parent || a.MasterIndex != 0 || actual.Type() != link.kind || a.HardwareAddr.String() != link.mac {
			return ErrChanged
		}
	}
	if len(n.links) == 0 {
		return ErrChanged
	}
	lower := n.links[len(n.links)-1]
	device, err := filepath.EvalSymlinks(filepath.Join(lower.node.path, "device"))
	if err != nil || device != n.function {
		return ErrChanged
	}
	if n.binding.Mux != 0 {
		var mux byte
		if n.links[0].kind == "rmnet" {
			mux, err = readRmnetMux(n.namespace, n.links[0].index)
		} else {
			mux, err = readQmapMux(n.links[0].node.path)
		}
		if err != nil || mux != n.binding.Mux {
			return ErrChanged
		}
	}
	return nil
}
func (n *linuxWDSTopology) Close() error {
	if n == nil || n.closed {
		return nil
	}
	n.closed = true
	failed := false
	for _, node := range n.nodes {
		if node.file.Close() != nil {
			failed = true
		}
	}
	for _, link := range n.links {
		if link.node.file.Close() != nil {
			failed = true
		}
	}
	if n.handle != nil {
		n.handle.Close()
	}
	if n.namespace != nil && n.namespace.Close() != nil {
		failed = true
	}
	if failed {
		return ErrTransport
	}
	return nil
}
