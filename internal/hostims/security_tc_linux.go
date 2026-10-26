//go:build linux

package hostims

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

var (
	ErrSecurityTCConflict = errors.New("host IMS: conflicting or unsupported ingress security classifier")
	ErrSecurityTCUnknown  = errors.New("host IMS: ingress security mutation acknowledgement is unknown")
	ErrSecurityTCMissing  = errors.New("host IMS: ingress security classifier is absent")
	ErrSecurityTCClosed   = errors.New("host IMS: ingress security transport is closed or fenced")
)

type securityTCFlow struct{ Local, Remote netip.Addr }
type securityTCSpec struct {
	InterfaceIndex int
	Mark           uint32
	Token          string
	Flows          []securityTCFlow
}

// Keyless, deterministic operation identity for the owner's write-ahead ledger.
// The owner must persist intent before apply/remove and own ONLY acknowledged
// additions. An identical object found after a lost ACK is NOT ownership proof.
type securityTCOperation struct {
	Kind    string `json:"k"`
	Ordinal uint8  `json:"o"`
}

const (
	securityTCClsact  = uint32(0xffff0000)
	securityTCParent  = uint32(0xfffffff1)
	securityTCIngress = uint32(0xfffffff2)
	securityTCEgress  = uint32(0xfffffff3)
	securityTCPref    = uint32(49152)
)

type linuxSecurityTC struct {
	mu        sync.Mutex
	socket    securityTCTransport
	namespace *os.File
	cookie    uint64
	spec      securityTCSpec
	closed    bool
	poisoned  bool
}

// The only production implementation is an explicit namespace-pinned raw fd.
// The interface permits deterministic lost-ACK protocol tests without touching
// a driver, route, modem or kernel classifier.
type securityTCTransport interface {
	GetFd() int
	GetPid() (uint32, error)
	BeginRequest(time.Time)
	Send(*nl.NetlinkRequest) (bool, error)
	Receive() ([]syscall.NetlinkMessage, *unix.SockaddrNetlink, error)
	Close()
}

func canonicalSecurityTCSpec(spec securityTCSpec) (securityTCSpec, error) {
	token, err := hex.DecodeString(spec.Token)
	if spec.InterfaceIndex <= 0 || int64(spec.InterfaceIndex) > 0x7fffffff || spec.Mark == 0 ||
		err != nil || len(token) != 32 || spec.Token != hex.EncodeToString(token) || len(spec.Flows) < 1 || len(spec.Flows) > 8 {
		return securityTCSpec{}, ErrSecurityTCConflict
	}
	copySpec := spec
	copySpec.Flows = append([]securityTCFlow(nil), spec.Flows...)
	for _, flow := range copySpec.Flows {
		if !securityTCAddress(flow.Local) || !securityTCAddress(flow.Remote) || flow.Local == flow.Remote || flow.Local.Is4() != flow.Remote.Is4() {
			return securityTCSpec{}, ErrSecurityTCConflict
		}
	}
	sort.Slice(copySpec.Flows, func(i, j int) bool {
		if c := copySpec.Flows[i].Local.Compare(copySpec.Flows[j].Local); c != 0 {
			return c < 0
		}
		return copySpec.Flows[i].Remote.Compare(copySpec.Flows[j].Remote) < 0
	})
	unique := copySpec.Flows[:0]
	for _, flow := range copySpec.Flows {
		if len(unique) == 0 || flow != unique[len(unique)-1] {
			unique = append(unique, flow)
		}
	}
	copySpec.Flows = unique
	return copySpec, nil
}

func securityTCAddress(address netip.Addr) bool {
	return address.IsValid() && address.Zone() == "" && !address.Is4In6() && !address.IsUnspecified() &&
		!address.IsMulticast() && !address.IsLinkLocalUnicast() && !address.IsLoopback()
}

func openLinuxSecurityTC(namespace *os.File, expectedCookie uint64, spec securityTCSpec) (*linuxSecurityTC, error) {
	validated, err := canonicalSecurityTCSpec(spec)
	if err != nil || namespace == nil || expectedCookie == 0 {
		return nil, ErrSecurityTCConflict
	}
	fd, err := unix.FcntlInt(namespace.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrSecurityTCClosed
	}
	owned := os.NewFile(uintptr(fd), "host-ims-tc-netns")
	socket, err := openSecurityTCNetlink(owned)
	if err != nil {
		owned.Close()
		return nil, ErrSecurityTCClosed
	}
	k := &linuxSecurityTC{socket: socket, namespace: owned, cookie: expectedCookie, spec: validated}
	if k.verifyLocked() != nil {
		k.close()
		return nil, ErrSecurityTCClosed
	}
	return k, nil
}

func (k *linuxSecurityTC) operations() []securityTCOperation {
	result := []securityTCOperation{{Kind: "clsact"}}
	for i := range k.spec.Flows {
		result = append(result, securityTCOperation{Kind: "flower", Ordinal: uint8(i + 1)})
	}
	return result
}

func (k *linuxSecurityTC) validOperation(op securityTCOperation) bool {
	return op.Kind == "clsact" && op.Ordinal == 0 || op.Kind == "flower" && op.Ordinal > 0 && int(op.Ordinal) <= len(k.spec.Flows)
}

func (k *linuxSecurityTC) verifyLocked() error {
	if k == nil || k.closed || k.poisoned || k.socket == nil || k.namespace == nil {
		return ErrSecurityTCClosed
	}
	cookie, err := unix.GetsockoptUint64(k.socket.GetFd(), unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil || cookie == 0 || cookie != k.cookie {
		return ErrSecurityTCClosed
	}
	return nil
}

func (k *linuxSecurityTC) close() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
	if k.socket != nil {
		k.socket.Close()
		k.socket = nil
	}
	if k.namespace != nil {
		k.namespace.Close()
		k.namespace = nil
	}
}

func (k *linuxSecurityTC) observe(op securityTCOperation) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.verifyLocked() != nil {
		return "", false, ErrSecurityTCClosed
	}
	if !k.validOperation(op) {
		return "", false, ErrSecurityTCConflict
	}
	objects, err := k.inventoryLocked()
	if err != nil {
		return "", false, err
	}
	hash, present := objects[op]
	return hash, present, nil
}

func (k *linuxSecurityTC) apply(op securityTCOperation) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.verifyLocked() != nil {
		return ErrSecurityTCClosed
	}
	if !k.validOperation(op) {
		return ErrSecurityTCConflict
	}
	objects, err := k.inventoryLocked()
	if err != nil {
		return err
	}
	if _, present := objects[op]; present {
		return ErrSecurityTCConflict
	}
	if _, present := objects[securityTCOperation{Kind: "clsact"}]; !present && op.Kind != "clsact" {
		return ErrSecurityTCMissing
	}
	request := k.request(op, false)
	_, err = k.exchangeLocked(request, 0, true)
	return err
}

func (k *linuxSecurityTC) remove(op securityTCOperation, expectedHash string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.verifyLocked() != nil {
		return ErrSecurityTCClosed
	}
	if !k.validOperation(op) || len(expectedHash) != 64 {
		return ErrSecurityTCConflict
	}
	objects, err := k.inventoryLocked()
	if err != nil {
		return err
	}
	actual, present := objects[op]
	if !present {
		return ErrSecurityTCMissing
	}
	if actual != expectedHash || op.Kind == "clsact" && len(objects) != 1 {
		return ErrSecurityTCConflict
	}
	_, err = k.exchangeLocked(k.request(op, true), 0, true)
	return err
}

func (k *linuxSecurityTC) request(op securityTCOperation, remove bool) *nl.NetlinkRequest {
	command, flags := unix.RTM_NEWQDISC, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK
	message := &nl.TcMsg{Family: unix.AF_UNSPEC, Ifindex: int32(k.spec.InterfaceIndex), Handle: securityTCClsact, Parent: securityTCParent}
	if remove {
		command, flags = unix.RTM_DELQDISC, unix.NLM_F_ACK
	}
	if op.Kind == "flower" {
		command = unix.RTM_NEWTFILTER
		if remove {
			command = unix.RTM_DELTFILTER
		}
		flow := k.spec.Flows[int(op.Ordinal)-1]
		protocol := uint16(unix.ETH_P_IP)
		if flow.Local.Is6() {
			protocol = unix.ETH_P_IPV6
		}
		message.Handle, message.Parent = uint32(op.Ordinal), securityTCIngress
		message.Info = (securityTCPref+uint32(op.Ordinal))<<16 | uint32(nl.Swap16(protocol))
	}
	request := nl.NewNetlinkRequest(command, flags)
	request.AddData(message)
	if remove {
		return request
	}
	if op.Kind == "clsact" {
		request.AddData(nl.NewRtAttr(nl.TCA_KIND, []byte("clsact\x00")))
		return request
	}
	request.AddData(nl.NewRtAttr(nl.TCA_KIND, []byte("flower\x00")))
	options := nl.NewRtAttr(nl.TCA_OPTIONS, nil)
	flow := k.spec.Flows[int(op.Ordinal)-1]
	protocol := uint16(unix.ETH_P_IP)
	srcType, srcMaskType, dstType, dstMaskType := nl.TCA_FLOWER_KEY_IPV4_SRC, nl.TCA_FLOWER_KEY_IPV4_SRC_MASK, nl.TCA_FLOWER_KEY_IPV4_DST, nl.TCA_FLOWER_KEY_IPV4_DST_MASK
	if flow.Local.Is6() {
		protocol = unix.ETH_P_IPV6
		srcType, srcMaskType, dstType, dstMaskType = nl.TCA_FLOWER_KEY_IPV6_SRC, nl.TCA_FLOWER_KEY_IPV6_SRC_MASK, nl.TCA_FLOWER_KEY_IPV6_DST, nl.TCA_FLOWER_KEY_IPV6_DST_MASK
	}
	eth := make([]byte, 2)
	binary.BigEndian.PutUint16(eth, protocol)
	options.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, eth)
	options.AddRtAttr(nl.TCA_FLOWER_KEY_IP_PROTO, []byte{unix.IPPROTO_ESP})
	mask := bytes.Repeat([]byte{255}, flow.Local.BitLen()/8)
	options.AddRtAttr(srcType, flow.Remote.AsSlice())
	options.AddRtAttr(srcMaskType, mask)
	options.AddRtAttr(dstType, flow.Local.AsSlice())
	options.AddRtAttr(dstMaskType, mask)
	options.AddRtAttr(nl.TCA_FLOWER_FLAGS, nl.Uint32Attr(nl.TCA_CLS_FLAGS_SKIP_HW))
	actions := options.AddRtAttr(nl.TCA_FLOWER_ACT, nil)
	action := actions.AddRtAttr(1, nil)
	action.AddRtAttr(nl.TCA_ACT_KIND, []byte("skbedit\x00"))
	action.AddRtAttr(nl.TCA_ACT_COOKIE, k.actionCookie(op))
	actionOptions := action.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
	parameters := make([]byte, 20)
	binary.NativeEndian.PutUint32(parameters[8:], 3) // TC_ACT_PIPE, never a jump/goto.
	actionOptions.AddRtAttr(nl.TCA_SKBEDIT_PARMS, parameters)
	actionOptions.AddRtAttr(nl.TCA_SKBEDIT_MARK, nl.Uint32Attr(k.spec.Mark))
	actionOptions.AddRtAttr(nl.TCA_SKBEDIT_MASK, nl.Uint32Attr(0xffffffff))
	request.AddData(options)
	return request
}

func (k *linuxSecurityTC) actionCookie(op securityTCOperation) []byte {
	flow := k.spec.Flows[int(op.Ordinal)-1]
	value := sha256.Sum256([]byte(fmt.Sprintf("vocat/owned-esp-ingress/v1/%s/%d/%08x/%d/%s/%s",
		k.spec.Token, k.spec.InterfaceIndex, k.spec.Mark, op.Ordinal, flow.Local, flow.Remote)))
	return value[:16] // Linux UAPI TC_COOKIE_MAX_SIZE is exactly 16 bytes.
}

// One explicit namespace-pinned socket only. A post-send transport/protocol
// failure poisons this driver and is NEVER inferred as an absent mutation.
// A complete matching NLMSG_ERROR is the sole mutation ACK/rejection source.
func (k *linuxSecurityTC) exchangeLocked(request *nl.NetlinkRequest, response uint16, mutation bool) ([][]byte, error) {
	k.socket.BeginRequest(time.Now().Add(2 * time.Second))
	pid, err := k.socket.GetPid()
	if err != nil {
		return nil, ErrSecurityTCClosed
	}
	unknown := func() ([][]byte, error) {
		if mutation {
			k.poisoned = true
			return nil, ErrSecurityTCUnknown
		}
		return nil, ErrSecurityTCConflict
	}
	if sent, err := k.socket.Send(request); err != nil {
		if sent {
			return unknown()
		}
		return nil, ErrSecurityTCConflict // Atomic netlink datagram was not sent.
	}
	var result [][]byte
	for batch := 0; batch < 64; batch++ {
		messages, from, err := k.socket.Receive()
		if err != nil || from == nil || from.Pid != 0 {
			return unknown()
		}
		for _, message := range messages {
			if message.Header.Seq != request.Seq || message.Header.Pid != pid || message.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
				return unknown()
			}
			switch message.Header.Type {
			case unix.NLMSG_ERROR:
				// CAP_ACK is mandatory and extended ACK strings are disabled.
				// No unexplained trailing data or mismatched echoed request.
				if len(message.Data) != 20 || binary.NativeEndian.Uint32(message.Data[4:8]) != request.Len ||
					binary.NativeEndian.Uint16(message.Data[8:10]) != request.Type ||
					binary.NativeEndian.Uint16(message.Data[10:12]) != request.Flags ||
					binary.NativeEndian.Uint32(message.Data[12:16]) != request.Seq ||
					binary.NativeEndian.Uint32(message.Data[16:20]) != request.Pid {
					return unknown()
				}
				errno := int32(binary.NativeEndian.Uint32(message.Data[:4]))
				if errno < 0 {
					return nil, fmt.Errorf("%w: kernel errno %d", ErrSecurityTCConflict, -errno)
				}
				if errno != 0 || !mutation {
					return unknown()
				}
				return nil, nil
			case unix.NLMSG_DONE:
				if mutation || len(message.Data) != 0 && (len(message.Data) != 4 || binary.NativeEndian.Uint32(message.Data) != 0) {
					return unknown()
				}
				return result, nil
			default:
				if mutation || message.Header.Type != response || len(message.Data) < nl.SizeofTcMsg || len(result) >= 128 {
					return unknown()
				}
				result = append(result, append([]byte(nil), message.Data...))
			}
		}
	}
	return unknown()
}

func (k *linuxSecurityTC) dumpLocked(command, response int, parent uint32) ([][]byte, error) {
	request := nl.NewNetlinkRequest(command, unix.NLM_F_DUMP)
	request.AddData(&nl.TcMsg{Family: unix.AF_UNSPEC, Ifindex: int32(k.spec.InterfaceIndex), Parent: parent})
	return k.exchangeLocked(request, uint16(response), false)
}

func (k *linuxSecurityTC) inventoryLocked() (map[securityTCOperation]string, error) {
	qdiscs, err := k.dumpLocked(unix.RTM_GETQDISC, unix.RTM_NEWQDISC, 0)
	if err != nil {
		return nil, err
	}
	result := make(map[securityTCOperation]string)
	heads := make(map[securityTCOperation]bool)
	parents := map[uint32]bool{0: true, securityTCIngress: true, securityTCEgress: true}
	for _, raw := range qdiscs {
		// RTM_GETQDISC ignores tcm_ifindex on several kernels and returns
		// other devices too. They are not children of this owned mux.
		if len(raw) < nl.SizeofTcMsg {
			return nil, ErrSecurityTCConflict
		}
		if int32(binary.NativeEndian.Uint32(raw[4:8])) != int32(k.spec.InterfaceIndex) {
			continue
		}
		message, attrs, err := securityTCMessage(raw, k.spec.InterfaceIndex)
		if err != nil {
			return nil, err
		}
		kind, ok := attrs[nl.TCA_KIND]
		if !ok || len(kind) < 2 || kind[len(kind)-1] != 0 || bytes.IndexByte(kind[:len(kind)-1], 0) >= 0 {
			return nil, ErrSecurityTCConflict
		}
		if string(kind) == "clsact\x00" {
			// qdisc tcm_info is the kernel reference count, not filter priority.
			// Our exclusive clsact must have exactly its one device attachment.
			if message.Handle != securityTCClsact || message.Parent != securityTCParent || message.Info != 1 {
				return nil, ErrSecurityTCConflict
			}
			hash, err := securityTCQdiscHash(raw, attrs)
			if err != nil || result[securityTCOperation{Kind: "clsact"}] != "" {
				return nil, ErrSecurityTCConflict
			}
			result[securityTCOperation{Kind: "clsact"}] = hash
		} else {
			if message.Parent == securityTCParent || message.Handle == securityTCClsact || string(kind) == "ingress\x00" {
				return nil, ErrSecurityTCConflict
			}
			switch string(kind) {
			case "noqueue\x00", "mq\x00", "fq\x00", "fq_codel\x00", "pfifo\x00", "bfifo\x00", "pfifo_fast\x00":
			default:
				return nil, ErrSecurityTCConflict
			}
			parents[message.Handle] = true
		}
	}
	for parent := range parents {
		filters, err := k.dumpLocked(unix.RTM_GETTFILTER, unix.RTM_NEWTFILTER, parent)
		if err != nil {
			return nil, err
		}
		for _, raw := range filters {
			if len(raw) < nl.SizeofTcMsg {
				return nil, ErrSecurityTCConflict
			}
			if int32(binary.NativeEndian.Uint32(raw[4:8])) != int32(k.spec.InterfaceIndex) {
				continue
			}
			message, attrs, err := securityTCMessage(raw, k.spec.InterfaceIndex)
			if err != nil || parent != securityTCIngress || message.Parent != securityTCIngress {
				return nil, ErrSecurityTCConflict
			}
			if message.Handle == 0 {
				// RTM_GETTFILTER includes the tcf_proto head, not just filters.
				// It has no actions/options and cannot itself establish ownership.
				ordinal := int(message.Info>>16) - int(securityTCPref)
				if ordinal < 1 || ordinal > len(k.spec.Flows) || len(attrs) != 2 ||
					!bytes.Equal(attrs[nl.TCA_KIND], []byte("flower\x00")) || len(attrs[nl.TCA_CHAIN]) != 4 || binary.NativeEndian.Uint32(attrs[nl.TCA_CHAIN]) != 0 {
					return nil, ErrSecurityTCConflict
				}
				op := securityTCOperation{Kind: "flower", Ordinal: uint8(ordinal)}
				flow := k.spec.Flows[ordinal-1]
				eth := uint16(unix.ETH_P_IP)
				if flow.Local.Is6() {
					eth = unix.ETH_P_IPV6
				}
				if heads[op] || message.Info != (securityTCPref+uint32(ordinal))<<16|uint32(nl.Swap16(eth)) {
					return nil, ErrSecurityTCConflict
				}
				heads[op] = true
				continue
			}
			if message.Handle > uint32(len(k.spec.Flows)) {
				return nil, ErrSecurityTCConflict
			}
			op := securityTCOperation{Kind: "flower", Ordinal: uint8(message.Handle)}
			if result[op] != "" {
				return nil, ErrSecurityTCConflict
			}
			hash, err := k.filterHash(raw, message, attrs, op)
			if err != nil {
				return nil, err
			}
			result[op] = hash
		}
	}
	for op := range heads {
		if result[op] == "" {
			return nil, ErrSecurityTCConflict
		}
	}
	for op := range result {
		if op.Kind == "flower" && !heads[op] {
			return nil, ErrSecurityTCConflict
		}
	}
	return result, nil
}

func securityTCMessage(raw []byte, index int) (nl.TcMsg, map[uint16][]byte, error) {
	if len(raw) < nl.SizeofTcMsg || raw[0] != unix.AF_UNSPEC || !bytes.Equal(raw[1:4], []byte{0, 0, 0}) ||
		int32(binary.NativeEndian.Uint32(raw[4:8])) != int32(index) {
		return nl.TcMsg{}, nil, ErrSecurityTCConflict
	}
	message := nl.TcMsg{Ifindex: int32(index), Handle: binary.NativeEndian.Uint32(raw[8:12]),
		Parent: binary.NativeEndian.Uint32(raw[12:16]), Info: binary.NativeEndian.Uint32(raw[16:20])}
	attrs, err := securityTCAttrs(raw[nl.SizeofTcMsg:])
	return message, attrs, err
}

func securityTCAttrs(raw []byte) (map[uint16][]byte, error) {
	if len(raw) > 65536 {
		return nil, ErrSecurityTCConflict
	}
	result := make(map[uint16][]byte)
	for offset := 0; offset < len(raw); {
		if len(raw)-offset < 4 || len(result) >= 256 {
			return nil, ErrSecurityTCConflict
		}
		size := int(binary.NativeEndian.Uint16(raw[offset : offset+2]))
		kind := binary.NativeEndian.Uint16(raw[offset+2 : offset+4])
		aligned := (size + 3) &^ 3
		if size < 4 || aligned > len(raw)-offset || kind&0xc000 != 0 || !securityTCZeros(raw[offset+size:offset+aligned]) {
			return nil, ErrSecurityTCConflict
		}
		if _, duplicate := result[kind]; duplicate {
			return nil, ErrSecurityTCConflict
		}
		result[kind] = raw[offset+4 : offset+size]
		offset += aligned
	}
	return result, nil
}

func securityTCQdiscHash(raw []byte, attrs map[uint16][]byte) (string, error) {
	static := append([]byte(nil), raw[:nl.SizeofTcMsg]...)
	for _, key := range securityTCKeys(attrs) {
		value := attrs[key]
		switch key {
		case nl.TCA_KIND:
		case nl.TCA_OPTIONS:
			if len(value) != 0 {
				return "", ErrSecurityTCConflict
			}
		case nl.TCA_HW_OFFLOAD:
			if len(value) != 1 || value[0] != 0 {
				return "", ErrSecurityTCConflict
			}
		case nl.TCA_STATS, nl.TCA_STATS2:
			if !securityTCStatistics(key, value) {
				return "", ErrSecurityTCConflict
			}
			continue
		case nl.TCA_PAD:
			if !securityTCZeros(value) {
				return "", ErrSecurityTCConflict
			}
			continue
		default:
			return "", ErrSecurityTCConflict
		}
		static = append(static, nl.NewRtAttr(int(key), value).Serialize()...)
	}
	sum := sha256.Sum256(static)
	return hex.EncodeToString(sum[:]), nil
}

func securityTCKeys(attrs map[uint16][]byte) []uint16 {
	keys := make([]uint16, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func securityTCZeros(value []byte) bool { return bytes.Equal(value, make([]byte, len(value))) }

func securityTCStatistics(kind uint16, raw []byte) bool {
	if kind == nl.TCA_STATS {
		return len(raw) == 36 || len(raw) == 40
	}
	attrs, err := securityTCAttrs(raw)
	if err != nil {
		return false
	}
	for key, value := range attrs {
		switch key {
		case nl.TCA_STATS_BASIC, nl.TCA_STATS_BASIC_HW:
			if len(value) != 12 && len(value) != 16 {
				return false
			}
		case nl.TCA_STATS_RATE_EST:
			if len(value) != 8 {
				return false
			}
		case nl.TCA_STATS_QUEUE:
			if len(value) != 20 {
				return false
			}
		case nl.TCA_STATS_RATE_EST64:
			if len(value) != 16 {
				return false
			}
		case nl.TCA_STATS_PKT64:
			if len(value) != 8 {
				return false
			}
		case nl.TCA_STATS_PAD:
			if !securityTCZeros(value) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (k *linuxSecurityTC) filterHash(raw []byte, message nl.TcMsg, attrs map[uint16][]byte, op securityTCOperation) (string, error) {
	flow := k.spec.Flows[int(op.Ordinal)-1]
	eth := uint16(unix.ETH_P_IP)
	if flow.Local.Is6() {
		eth = unix.ETH_P_IPV6
	}
	if message.Info != (securityTCPref+uint32(op.Ordinal))<<16|uint32(nl.Swap16(eth)) || !bytes.Equal(attrs[nl.TCA_KIND], []byte("flower\x00")) {
		return "", ErrSecurityTCConflict
	}
	static := append([]byte(nil), raw[:nl.SizeofTcMsg]...)
	for _, key := range securityTCKeys(attrs) {
		value := attrs[key]
		switch key {
		case nl.TCA_KIND:
		case nl.TCA_CHAIN:
			if len(value) != 4 || binary.NativeEndian.Uint32(value) != 0 {
				return "", ErrSecurityTCConflict
			}
		case nl.TCA_OPTIONS:
			var err error
			value, err = k.flowerStatic(value, op)
			if err != nil {
				return "", err
			}
		case nl.TCA_STATS, nl.TCA_STATS2:
			if !securityTCStatistics(key, value) {
				return "", ErrSecurityTCConflict
			}
			continue
		case nl.TCA_PAD:
			if !securityTCZeros(value) {
				return "", ErrSecurityTCConflict
			}
			continue
		default:
			return "", ErrSecurityTCConflict
		}
		static = append(static, nl.NewRtAttr(int(key), value).Serialize()...)
	}
	if attrs[nl.TCA_OPTIONS] == nil {
		return "", ErrSecurityTCConflict
	}
	sum := sha256.Sum256(static)
	return hex.EncodeToString(sum[:]), nil
}

func (k *linuxSecurityTC) flowerStatic(raw []byte, op securityTCOperation) ([]byte, error) {
	attrs, err := securityTCAttrs(raw)
	if err != nil {
		return nil, err
	}
	flow := k.spec.Flows[int(op.Ordinal)-1]
	eth := uint16(unix.ETH_P_IP)
	src, srcMask, dst, dstMask := uint16(nl.TCA_FLOWER_KEY_IPV4_SRC), uint16(nl.TCA_FLOWER_KEY_IPV4_SRC_MASK), uint16(nl.TCA_FLOWER_KEY_IPV4_DST), uint16(nl.TCA_FLOWER_KEY_IPV4_DST_MASK)
	if flow.Local.Is6() {
		eth = unix.ETH_P_IPV6
		src, srcMask, dst, dstMask = nl.TCA_FLOWER_KEY_IPV6_SRC, nl.TCA_FLOWER_KEY_IPV6_SRC_MASK, nl.TCA_FLOWER_KEY_IPV6_DST, nl.TCA_FLOWER_KEY_IPV6_DST_MASK
	}
	mask := bytes.Repeat([]byte{255}, flow.Local.BitLen()/8)
	if !bytes.Equal(attrs[src], flow.Remote.AsSlice()) || !bytes.Equal(attrs[dst], flow.Local.AsSlice()) ||
		!bytes.Equal(attrs[srcMask], mask) || !bytes.Equal(attrs[dstMask], mask) ||
		!bytes.Equal(attrs[nl.TCA_FLOWER_KEY_IP_PROTO], []byte{unix.IPPROTO_ESP}) ||
		len(attrs[nl.TCA_FLOWER_KEY_ETH_TYPE]) != 2 || binary.BigEndian.Uint16(attrs[nl.TCA_FLOWER_KEY_ETH_TYPE]) != eth ||
		len(attrs[nl.TCA_FLOWER_FLAGS]) != 4 || binary.NativeEndian.Uint32(attrs[nl.TCA_FLOWER_FLAGS]) != 1|8 {
		return nil, ErrSecurityTCConflict
	}
	var static []byte
	for _, key := range securityTCKeys(attrs) {
		value := attrs[key]
		switch key {
		case src, srcMask, dst, dstMask, nl.TCA_FLOWER_KEY_IP_PROTO, nl.TCA_FLOWER_KEY_ETH_TYPE, nl.TCA_FLOWER_FLAGS:
		case nl.TCA_FLOWER_ACT:
			value, err = k.actionStatic(value, op)
			if err != nil {
				return nil, err
			}
		case nl.TCA_FLOWER_IN_HW_COUNT:
			if len(value) != 4 || binary.NativeEndian.Uint32(value) != 0 {
				return nil, ErrSecurityTCConflict
			}
		default:
			return nil, ErrSecurityTCConflict
		}
		static = append(static, nl.NewRtAttr(int(key), value).Serialize()...)
	}
	if attrs[nl.TCA_FLOWER_ACT] == nil {
		return nil, ErrSecurityTCConflict
	}
	return static, nil
}

func (k *linuxSecurityTC) actionStatic(raw []byte, op securityTCOperation) ([]byte, error) {
	table, err := securityTCAttrs(raw)
	if err != nil || len(table) != 1 || table[1] == nil {
		return nil, ErrSecurityTCConflict
	}
	attrs, err := securityTCAttrs(table[1])
	if err != nil || !bytes.Equal(attrs[nl.TCA_ACT_KIND], []byte("skbedit\x00")) ||
		!bytes.Equal(attrs[nl.TCA_ACT_COOKIE], k.actionCookie(op)) || attrs[nl.TCA_ACT_OPTIONS] == nil {
		return nil, ErrSecurityTCConflict
	}
	var action []byte
	for _, key := range securityTCKeys(attrs) {
		value := attrs[key]
		switch key {
		case nl.TCA_ACT_KIND, nl.TCA_ACT_COOKIE:
		case nl.TCA_ACT_OPTIONS:
			value, err = k.skbeditStatic(value)
			if err != nil {
				return nil, err
			}
		case nl.TCA_ACT_STATS:
			if !securityTCStatistics(nl.TCA_STATS2, value) {
				return nil, ErrSecurityTCConflict
			}
			continue
		case nl.TCA_ACT_HW_STATS, nl.TCA_ACT_USED_HW_STATS:
			if len(value) != 1 || value[0] > 3 {
				return nil, ErrSecurityTCConflict
			}
		case nl.TCA_ACT_FLAGS:
			// nla_bitfield32: the flower's skip_hw forces this inline action
			// to software too. No skip_sw, bind-to-another-owner or unknown flag.
			if len(value) != 8 || binary.NativeEndian.Uint32(value[:4]) != 2 || binary.NativeEndian.Uint32(value[4:]) != 2 {
				return nil, ErrSecurityTCConflict
			}
		case nl.TCA_ACT_IN_HW_COUNT:
			if len(value) != 4 || binary.NativeEndian.Uint32(value) != 0 {
				return nil, ErrSecurityTCConflict
			}
		case nl.TCA_ACT_PAD:
			if !securityTCZeros(value) {
				return nil, ErrSecurityTCConflict
			}
			continue
		default:
			return nil, ErrSecurityTCConflict
		}
		action = append(action, nl.NewRtAttr(int(key), value).Serialize()...)
	}
	return nl.NewRtAttr(1, action).Serialize(), nil
}

func (k *linuxSecurityTC) skbeditStatic(raw []byte) ([]byte, error) {
	attrs, err := securityTCAttrs(raw)
	if err != nil || len(attrs[nl.TCA_SKBEDIT_PARMS]) != 20 || len(attrs[nl.TCA_SKBEDIT_MARK]) != 4 || len(attrs[nl.TCA_SKBEDIT_MASK]) != 4 ||
		binary.NativeEndian.Uint32(attrs[nl.TCA_SKBEDIT_MARK]) != k.spec.Mark || binary.NativeEndian.Uint32(attrs[nl.TCA_SKBEDIT_MASK]) != 0xffffffff {
		return nil, ErrSecurityTCConflict
	}
	var static []byte
	for _, key := range securityTCKeys(attrs) {
		value := attrs[key]
		switch key {
		case nl.TCA_SKBEDIT_PARMS:
			if binary.NativeEndian.Uint32(value[:4]) == 0 || binary.NativeEndian.Uint32(value[4:8]) != 0 || binary.NativeEndian.Uint32(value[8:12]) != 3 {
				return nil, ErrSecurityTCConflict
			}
			value = append([]byte(nil), value...)
			// refcnt/bindcnt are volatile, but require the inline action still
			// has exactly one binding and no external reference.
			if int32(binary.NativeEndian.Uint32(value[12:16])) != 1 || int32(binary.NativeEndian.Uint32(value[16:20])) != 1 {
				return nil, ErrSecurityTCConflict
			}
			clear(value[12:20])
		case nl.TCA_SKBEDIT_MARK, nl.TCA_SKBEDIT_MASK:
		case nl.TCA_SKBEDIT_TM:
			if len(value) != 32 || binary.NativeEndian.Uint64(value[16:24]) != 0 {
				return nil, ErrSecurityTCConflict
			}
			continue
		case nl.TCA_SKBEDIT_PAD:
			if !securityTCZeros(value) {
				return nil, ErrSecurityTCConflict
			}
			continue
		default:
			return nil, ErrSecurityTCConflict
		}
		static = append(static, nl.NewRtAttr(int(key), value).Serialize()...)
	}
	return static, nil
}
