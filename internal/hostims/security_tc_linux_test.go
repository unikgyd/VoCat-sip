//go:build linux

package hostims

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func securityTCTestSpec(index int) securityTCSpec {
	return securityTCSpec{InterfaceIndex: index, Mark: 0xa001, Token: strings.Repeat("a", 64),
		Flows: []securityTCFlow{{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")},
			{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2")}}}
}

func TestSecurityTCPlanCanonicalAndBounded(t *testing.T) {
	spec := securityTCTestSpec(3)
	spec.Flows = append([]securityTCFlow{spec.Flows[1]}, spec.Flows...)
	canonical, err := canonicalSecurityTCSpec(spec)
	if err != nil || len(canonical.Flows) != 2 || canonical.Flows[0].Local.Is6() {
		t.Fatal("flow canonicalization failed", err)
	}
	spec.Flows[1].Local = netip.MustParseAddr("198.51.100.1")
	if canonical.Flows[0].Local != netip.MustParseAddr("192.0.2.1") {
		t.Fatal("caller mutated immutable TC scope")
	}
	for _, mutate := range []func(*securityTCSpec){
		func(s *securityTCSpec) { s.InterfaceIndex = 0 },
		func(s *securityTCSpec) { s.Mark = 0 },
		func(s *securityTCSpec) { s.Token = strings.Repeat("A", 64) },
		func(s *securityTCSpec) { s.Flows = nil },
		func(s *securityTCSpec) { s.Flows = append(s.Flows, make([]securityTCFlow, 7)...) },
		func(s *securityTCSpec) { s.Flows[0].Remote = s.Flows[0].Local },
		func(s *securityTCSpec) { s.Flows[0].Remote = s.Flows[1].Remote },
		func(s *securityTCSpec) { s.Flows[0].Local = netip.MustParseAddr("127.0.0.1") },
	} {
		candidate := securityTCTestSpec(3)
		mutate(&candidate)
		if _, err := canonicalSecurityTCSpec(candidate); !errors.Is(err, ErrSecurityTCConflict) {
			t.Fatal("invalid TC scope accepted", err)
		}
	}
}

func TestSecurityTCAttributesRejectTruncationPaddingDuplicateAndFlags(t *testing.T) {
	valid := nl.NewRtAttr(3, []byte{50}).Serialize()
	if _, err := securityTCAttrs(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{append(append([]byte(nil), valid...), valid...), valid[:len(valid)-1], append(valid, 0), {3, 0, 1, 0}} {
		if _, err := securityTCAttrs(raw); err == nil {
			t.Fatal("malformed TC attribute stream accepted")
		}
	}
	for _, offset := range []int{3, 7} {
		mutated := append([]byte(nil), valid...)
		mutated[offset] = 0x80
		if _, err := securityTCAttrs(mutated); err == nil {
			t.Fatal("unsupported attribute flags/padding accepted")
		}
	}
}

func TestSecurityTCNativeOwnedCreateFingerprintAndForeignFence(t *testing.T) {
	requireSecurityTCNamespace(t)
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "vocat-tc-a"}, PeerName: "vocat-tc-b"}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(link)
	created, err := netlink.LinkByName(link.Attrs().Name)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	unix.Close(fd)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := openLinuxSecurityTC(namespace, cookie, securityTCTestSpec(created.Attrs().Index))
	if err != nil {
		t.Fatal(err)
	}
	defer kernel.close()
	debug := func() {
		rows, err := kernel.dumpLocked(unix.RTM_GETQDISC, unix.RTM_NEWQDISC, 0)
		t.Logf("qdisc dump err=%v", err)
		for _, row := range rows {
			t.Logf("qdisc raw=%x", row)
		}
		for _, parent := range []uint32{0, securityTCIngress, securityTCEgress} {
			rows, err := kernel.dumpLocked(unix.RTM_GETTFILTER, unix.RTM_NEWTFILTER, parent)
			t.Logf("filter parent=%x err=%v", parent, err)
			for _, row := range rows {
				t.Logf("filter raw=%x", row)
			}
		}
	}
	var hashes []string
	for _, operation := range kernel.operations() {
		if _, present, err := kernel.observe(operation); err != nil || present {
			debug()
			t.Fatal("new operation was not absent", operation, err)
		}
		if err := kernel.apply(operation); err != nil {
			debug()
			t.Fatal("operation add failed", operation, err)
		}
		hash, present, err := kernel.observe(operation)
		if err != nil || !present || len(hash) != 64 {
			debug()
			t.Fatal("operation fingerprint failed", operation, err)
		}
		if err := kernel.apply(operation); !errors.Is(err, ErrSecurityTCConflict) {
			t.Fatal("existing TC object was replaced", err)
		}
		hashes = append(hashes, hash)
	}
	if err := kernel.remove(kernel.operations()[0], hashes[0]); !errors.Is(err, ErrSecurityTCConflict) {
		t.Fatal("qdisc removed with live children", err)
	}
	// A deliberately foreign egress action is never replaced or deleted.
	foreignMark := uint32(0xbeef)
	foreign := &netlink.Flower{FilterAttrs: netlink.FilterAttrs{LinkIndex: created.Attrs().Index, Parent: securityTCEgress,
		Handle: 100, Priority: 100, Protocol: unix.ETH_P_IP}, EthType: unix.ETH_P_IP,
		Actions: []netlink.Action{&netlink.SkbEditAction{ActionAttrs: netlink.ActionAttrs{Action: netlink.TC_ACT_PIPE}, Mark: &foreignMark}}}
	if err := netlink.FilterAdd(foreign); err != nil {
		t.Fatal(err)
	}
	if _, _, err := kernel.observe(kernel.operations()[0]); !errors.Is(err, ErrSecurityTCConflict) {
		t.Fatal("foreign egress mark writer did not fence owner", err)
	}
	if err := kernel.remove(kernel.operations()[1], hashes[1]); !errors.Is(err, ErrSecurityTCConflict) {
		t.Fatal("mutation proceeded with a foreign classifier", err)
	}
	if err := netlink.FilterDel(foreign); err != nil {
		t.Fatal(err)
	}
	for index := len(hashes) - 1; index >= 0; index-- {
		op := kernel.operations()[index]
		if err := kernel.remove(op, strings.Repeat("0", 64)); !errors.Is(err, ErrSecurityTCConflict) {
			t.Fatal("fingerprint mismatch was ignored", err)
		}
		if err := kernel.remove(op, hashes[index]); err != nil {
			debug()
			t.Fatal("owned delete failed", op, err)
		}
		if _, present, err := kernel.observe(op); err != nil || present {
			t.Fatal("delete acknowledgement not observable", op, err)
		}
	}
	kernel.close()
	if _, _, err := kernel.observe(securityTCOperation{Kind: "clsact"}); !errors.Is(err, ErrSecurityTCClosed) {
		t.Fatal("closed driver fell back to a current-namespace socket", err)
	}
}

func requireSecurityTCNamespace(t *testing.T) {
	t.Helper()
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("explicit private NET+MNT namespace required")
	}
	for _, namespace := range []string{"net", "mnt"} {
		current, currentErr := os.Readlink("/proc/thread-self/ns/" + namespace)
		initial, initialErr := os.Readlink("/proc/1/ns/" + namespace)
		if currentErr != nil || initialErr != nil || current == initial {
			t.Fatalf("refusing TC mutation in host %s namespace", namespace)
		}
	}
}

func TestSecurityTCRequestIsExactESPFlowerCreateExclusive(t *testing.T) {
	kernel := &linuxSecurityTC{spec: securityTCTestSpec(7)}
	for _, operation := range kernel.operations() {
		request := kernel.request(operation, false)
		if request.Flags&unix.NLM_F_EXCL == 0 || request.Flags&unix.NLM_F_CREATE == 0 || request.Flags&unix.NLM_F_ACK == 0 || request.Flags&unix.NLM_F_REPLACE != 0 {
			t.Fatal("mutation is not exclusive acknowledged creation")
		}
		data := request.Serialize()[unix.SizeofNlMsghdr:]
		_, attrs, err := securityTCMessage(data, 7)
		if err != nil {
			t.Fatal(err)
		}
		if operation.Kind == "flower" {
			flower, err := securityTCAttrs(attrs[nl.TCA_OPTIONS])
			if err != nil || !bytes.Equal(flower[nl.TCA_FLOWER_KEY_IP_PROTO], []byte{50}) || binary.NativeEndian.Uint32(flower[nl.TCA_FLOWER_FLAGS]) != 1 {
				t.Fatal("not exact software ESP classifier", err)
			}
		}
	}
}

type securityTCFakeTransport struct {
	mode     string
	request  *nl.NetlinkRequest
	deadline time.Time
}

func (*securityTCFakeTransport) GetFd() int                        { return -1 }
func (*securityTCFakeTransport) GetPid() (uint32, error)           { return 1234, nil }
func (f *securityTCFakeTransport) BeginRequest(deadline time.Time) { f.deadline = deadline }
func (*securityTCFakeTransport) Close()                            {}
func (f *securityTCFakeTransport) Send(request *nl.NetlinkRequest) (bool, error) {
	f.request = request
	request.Serialize()
	if f.mode == "unsent" {
		return false, errors.New("synthetic write failure")
	}
	if f.mode == "sent_then_error" {
		return true, errors.New("synthetic ambiguous write")
	}
	return true, nil
}
func (f *securityTCFakeTransport) Receive() ([]syscall.NetlinkMessage, *unix.SockaddrNetlink, error) {
	if f.mode == "lost_ack" {
		return nil, nil, errors.New("synthetic receive deadline")
	}
	payload := make([]byte, 20)
	copy(payload[4:], f.request.Serialize()[:16])
	message := syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: unix.NLMSG_ERROR, Seq: f.request.Seq, Pid: 1234}, Data: payload}
	from := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	switch f.mode {
	case "reject":
		binary.NativeEndian.PutUint32(payload[:4], ^uint32(unix.EEXIST-1))
	case "foreign_sender":
		from.Pid = 123
	case "wrong_sequence":
		message.Header.Seq++
	case "wrong_echo":
		payload[12]++
	case "short_ack":
		message.Data = payload[:19]
	case "extra_ack_data":
		message.Data = append(payload, 0)
	case "positive_errno":
		binary.NativeEndian.PutUint32(payload[:4], 1)
	case "done_not_ack":
		message.Header.Type = unix.NLMSG_DONE
	}
	return []syscall.NetlinkMessage{message}, from, nil
}

func TestSecurityTCMutationAckClassificationAndFencing(t *testing.T) {
	for _, mode := range []string{"success", "reject", "unsent", "sent_then_error", "lost_ack", "foreign_sender", "wrong_sequence", "wrong_echo", "short_ack", "extra_ack_data", "positive_errno", "done_not_ack"} {
		t.Run(mode, func(t *testing.T) {
			transport := &securityTCFakeTransport{mode: mode}
			kernel := &linuxSecurityTC{socket: transport, spec: securityTCTestSpec(3)}
			request := kernel.request(kernel.operations()[0], false)
			before := time.Now()
			_, err := kernel.exchangeLocked(request, 0, true)
			if transport.deadline.Before(before.Add(1900*time.Millisecond)) || transport.deadline.After(time.Now().Add(2100*time.Millisecond)) {
				t.Fatal("request does not share one bounded two-second I/O deadline")
			}
			switch mode {
			case "success":
				if err != nil || kernel.poisoned {
					t.Fatal("valid ACK rejected", err)
				}
			case "reject", "unsent":
				if !errors.Is(err, ErrSecurityTCConflict) || kernel.poisoned {
					t.Fatal("known no-mutation rejection became uncertain", err)
				}
			default:
				if !errors.Is(err, ErrSecurityTCUnknown) || !kernel.poisoned {
					t.Fatal("unknown mutation was not fenced", err)
				}
				if kernel.verifyLocked() != ErrSecurityTCClosed {
					t.Fatal("uncertain driver remained reusable")
				}
			}
		})
	}
}

func FuzzSecurityTCStrictSchema(f *testing.F) {
	kernel := &linuxSecurityTC{spec: securityTCTestSpec(7)}
	for _, seed := range [][]byte{{}, {1, 2, 3}, nl.NewRtAttr(nl.TCA_KIND, []byte("flower\x00")).Serialize()} {
		f.Add(seed)
	}
	request := kernel.request(kernel.operations()[1], false).Serialize()
	f.Add(request[unix.SizeofNlMsghdr+nl.SizeofTcMsg:])
	f.Fuzz(func(t *testing.T, data []byte) {
		before := append([]byte(nil), data...)
		attrs, err := securityTCAttrs(data)
		if err == nil {
			wire := make([]byte, nl.SizeofTcMsg)
			wire = append(wire, data...)
			securityTCQdiscHash(wire, attrs)
		}
		kernel.flowerStatic(data, kernel.operations()[1])
		kernel.actionStatic(data, kernel.operations()[1])
		kernel.skbeditStatic(data)
		if !bytes.Equal(data, before) {
			t.Fatal("schema fingerprinting mutated the kernel snapshot")
		}
	})
}
