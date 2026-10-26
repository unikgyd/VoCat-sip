//go:build linux

package hostims

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Operational classifier evidence, not a real bearer: two synthetic muxes have
// identical private addresses, ports and SPI, but different mark/reqid/keys.
// The production flower driver must separate them before kernel ESP lookup.
func TestSecurityTCNativeOverlappingModemESPDelivery(t *testing.T) {
	requireSecurityTCNamespace(t)
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		states, err := netlink.XfrmStateList(family)
		if err != nil || len(states) != 0 {
			t.Fatal("refusing an existing synthetic/XFRM namespace", err)
		}
	}
	if output, err := exec.Command("sysctl", "-w", "net.ipv4.conf.all.rp_filter=0").CombinedOutput(); err != nil {
		t.Fatalf("private namespace rp_filter: %v %s", err, output)
	}
	namespace, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	seed, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := unix.GetsockoptUint64(seed, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	unix.Close(seed)
	if err != nil {
		t.Fatal(err)
	}
	type syntheticMux struct {
		local, peer netlink.Link
		kernel      *linuxSecurityTC
		hashes      []string
		mark, reqid uint32
		key, ik     []byte
		sockets     []*os.File
		injector    int
	}
	muxes := make([]syntheticMux, 2)
	flows := []securityTCFlow{{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.20")},
		{netip.MustParseAddr("2001:db8:55::10"), netip.MustParseAddr("2001:db8:55::20")}}
	for index := range muxes {
		mux := &muxes[index]
		name, peerName := fmt.Sprintf("vctc%d", index), fmt.Sprintf("vctcp%d", index)
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name}, PeerName: peerName}
		if err := netlink.LinkAdd(veth); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.LinkDel(veth) })
		mux.local, err = netlink.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		mux.peer, err = netlink.LinkByName(peerName)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(mux.local); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(mux.peer); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("sysctl", "-w", "net.ipv4.conf."+name+".rp_filter=0").CombinedOutput(); err != nil {
			t.Fatalf("private mux rp_filter: %v %s", err, output)
		}
		mux.mark, mux.reqid = 0x5101+uint32(index), 0x6101+uint32(index)
		mux.key, mux.ik = bytes.Repeat([]byte{byte(0x11 + index)}, 16), bytes.Repeat([]byte{byte(0x21 + index)}, 20)
		mux.kernel, err = openLinuxSecurityTC(namespace, cookie, securityTCSpec{InterfaceIndex: mux.local.Attrs().Index,
			Mark: mux.mark, Token: strings.Repeat(fmt.Sprintf("%x", index+1), 64), Flows: flows})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mux.kernel.close)
		for _, operation := range mux.kernel.operations() {
			if err := mux.kernel.apply(operation); err != nil {
				t.Fatal("production classifier creation", err)
			}
			hash, present, err := mux.kernel.observe(operation)
			if err != nil || !present {
				t.Fatal("classifier fingerprint", err)
			}
			mux.hashes = append(mux.hashes, hash)
		}
		for version, flow := range flows {
			ipnet := &net.IPNet{IP: net.IP(flow.Local.AsSlice()), Mask: net.CIDRMask(flow.Local.BitLen(), flow.Local.BitLen())}
			if err := netlink.AddrAdd(mux.local, &netlink.Addr{IPNet: ipnet, Flags: unix.IFA_F_NODAD | unix.IFA_F_NOPREFIXROUTE}); err != nil {
				t.Fatal(err)
			}
			state := &netlink.XfrmState{Src: net.IP(flow.Remote.AsSlice()), Dst: net.IP(flow.Local.AsSlice()), Proto: netlink.XFRM_PROTO_ESP,
				Mode: netlink.XFRM_MODE_TRANSPORT, Spi: 0x1111, Reqid: int(mux.reqid), ReplayWindow: 32,
				Mark:  &netlink.XfrmMark{Value: mux.mark, Mask: 0xffffffff},
				Auth:  &netlink.XfrmStateAlgo{Name: "hmac(sha1)", Key: mux.ik, TruncateLen: 96},
				Crypt: &netlink.XfrmStateAlgo{Name: "cbc(aes)", Key: mux.key}}
			if err := netlink.XfrmStateAdd(state); err != nil {
				t.Fatal(err)
			}
			// Captured immutable fixture key, only in this new namespace.
			fixtureState := state
			t.Cleanup(func() { netlink.XfrmStateDel(fixtureState) })
			family := unix.AF_INET
			var address unix.Sockaddr
			if flow.Local.Is6() {
				family = unix.AF_INET6
				address = &unix.SockaddrInet6{Addr: flow.Local.As16(), Port: 62100 + version}
			} else {
				address = &unix.SockaddrInet4{Addr: flow.Local.As4(), Port: 62100 + version}
			}
			fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
			if err != nil {
				t.Fatal(err)
			}
			file := os.NewFile(uintptr(fd), "synthetic-mux-ESP-receiver")
			t.Cleanup(func() { file.Close() })
			mux.sockets = append(mux.sockets, file)
			if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1) != nil ||
				unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name) != nil ||
				unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, int(mux.mark)) != nil {
				t.Fatal("synthetic socket owner binding")
			}
			raw, _ := file.SyscallConn()
			if err := RequireSocketESP(raw, SocketSecurityPolicy{Local: flow.Local, Remote: flow.Remote, LocalPort: uint16(62100 + version),
				Protocol: unix.IPPROTO_UDP, Role: SocketSecurityServer, ReqID: mux.reqid}); err != nil {
				t.Fatal(err)
			}
			if err := unix.Bind(fd, address); err != nil {
				t.Fatal("duplicate-address socket must remain per-device", err)
			}
		}
		mux.injector, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(swapTCProtocol(unix.ETH_P_ALL)))
		if err != nil {
			t.Fatal(err)
		}
		injector := mux.injector
		t.Cleanup(func() { unix.Close(injector) })
		// This preexisting high mark MUST be overwritten, not ORed/masked.
		if unix.SetsockoptInt(mux.injector, unix.SOL_SOCKET, unix.SO_MARK, 0x6f000000) != nil {
			t.Fatal("pre-mark synthetic injector")
		}
	}
	read := func(file *os.File, expected []byte) {
		t.Helper()
		poll := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
		deadline := time.Now().Add(150 * time.Millisecond)
		var ready int
		var err error
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				ready, err = 0, nil
				break
			}
			ready, err = unix.Poll(poll, int(remaining.Milliseconds())+1)
			if err != unix.EINTR {
				break
			}
		}
		if err != nil || expected == nil && ready != 0 || expected != nil && ready != 1 {
			t.Fatalf("unexpected per-mux packet availability expected=%q ready=%d err=%v", expected, ready, err)
		}
		if expected != nil {
			buffer := make([]byte, 512)
			count, _, err := unix.Recvfrom(int(file.Fd()), buffer, unix.MSG_DONTWAIT)
			if err != nil {
				t.Fatal("synthetic read", err)
			}
			if !bytes.Equal(buffer[:count], expected) {
				t.Fatalf("wrong modem/family payload: %q err=%v", buffer[:count], err)
			}
		}
	}
	for version, flow := range flows {
		for index := range muxes {
			mux := &muxes[index]
			payload := []byte(fmt.Sprintf("modem=%d ipv6=%v", index, flow.Local.Is6()))
			packet := securityTCTestFrame(flow, uint16(62100+version), uint32(version+1), payload, mux.key, mux.ik, true,
				mux.local.Attrs().HardwareAddr, mux.peer.Attrs().HardwareAddr)
			if err := unix.Sendto(mux.injector, packet, 0, &unix.SockaddrLinklayer{Ifindex: mux.peer.Attrs().Index, Protocol: swapTCProtocol(unix.ETH_P_ALL)}); err != nil {
				t.Fatal(err)
			}
			read(mux.sockets[version], payload)
			read(muxes[1-index].sockets[version], nil)
			plain := securityTCTestFrame(flow, uint16(62100+version), 20, []byte("plaintext-must-drop"), mux.key, mux.ik, false,
				mux.local.Attrs().HardwareAddr, mux.peer.Attrs().HardwareAddr)
			if err := unix.Sendto(mux.injector, plain, 0, &unix.SockaddrLinklayer{Ifindex: mux.peer.Attrs().Index, Protocol: swapTCProtocol(unix.ETH_P_ALL)}); err != nil {
				t.Fatal(err)
			}
			read(mux.sockets[version], nil)
		}
	}
	for index := range muxes {
		mux := &muxes[index]
		for ordinal, operation := range mux.kernel.operations() {
			hash, present, err := mux.kernel.observe(operation)
			if err != nil || !present || hash != mux.hashes[ordinal] {
				t.Fatal("traffic statistics changed static ownership fingerprint", err)
			}
		}
		for ordinal := len(mux.hashes) - 1; ordinal >= 0; ordinal-- {
			if err := mux.kernel.remove(mux.kernel.operations()[ordinal], mux.hashes[ordinal]); err != nil {
				t.Fatal("owned classifier cleanup", err)
			}
		}
	}
}

func swapTCProtocol(protocol uint16) uint16 { return protocol<<8 | protocol>>8 }

func securityTCTestChecksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) != 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func securityTCTestFrame(flow securityTCFlow, port uint16, sequence uint32, text, key, ik []byte, protected bool, destinationMAC, sourceMAC []byte) []byte {
	segment := make([]byte, 8+len(text))
	binary.BigEndian.PutUint16(segment[:2], 61000)
	binary.BigEndian.PutUint16(segment[2:4], port)
	binary.BigEndian.PutUint16(segment[4:6], uint16(len(segment)))
	copy(segment[8:], text)
	if flow.Local.Is6() {
		pseudo := append(flow.Remote.AsSlice(), flow.Local.AsSlice()...)
		pseudo = binary.BigEndian.AppendUint32(pseudo, uint32(len(segment)))
		pseudo = append(pseudo, 0, 0, 0, unix.IPPROTO_UDP)
		checksum := securityTCTestChecksum(append(pseudo, segment...))
		if checksum == 0 {
			checksum = 0xffff
		}
		binary.BigEndian.PutUint16(segment[6:8], checksum)
	}
	body, protocol := segment, byte(unix.IPPROTO_UDP)
	if protected {
		padding := (aes.BlockSize - (len(segment)+2)%aes.BlockSize) % aes.BlockSize
		plain := append([]byte(nil), segment...)
		for value := 1; value <= padding; value++ {
			plain = append(plain, byte(value))
		}
		plain = append(plain, byte(padding), unix.IPPROTO_UDP)
		body = binary.BigEndian.AppendUint32(nil, 0x1111)
		body = binary.BigEndian.AppendUint32(body, sequence)
		iv := make([]byte, aes.BlockSize)
		binary.BigEndian.PutUint32(iv[12:], sequence)
		body = append(body, iv...)
		block, _ := aes.NewCipher(key)
		ciphertext := make([]byte, len(plain))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, plain)
		body = append(body, ciphertext...)
		mac := hmac.New(sha1.New, ik)
		mac.Write(body)
		body = append(body, mac.Sum(nil)[:12]...)
		protocol = unix.IPPROTO_ESP
	}
	frame := append(append([]byte(nil), destinationMAC...), sourceMAC...)
	if flow.Local.Is6() {
		frame = append(frame, 0x86, 0xdd)
		header := []byte{0x60, 0, 0, 0, 0, 0, protocol, 64}
		binary.BigEndian.PutUint16(header[4:6], uint16(len(body)))
		header = append(header, flow.Remote.AsSlice()...)
		header = append(header, flow.Local.AsSlice()...)
		return append(append(frame, header...), body...)
	}
	frame = append(frame, 8, 0)
	header := []byte{0x45, 0, 0, 0, 0, 1, 0, 0, 64, protocol, 0, 0}
	binary.BigEndian.PutUint16(header[2:4], uint16(len(body)+20))
	header = append(header, flow.Remote.AsSlice()...)
	header = append(header, flow.Local.AsSlice()...)
	binary.BigEndian.PutUint16(header[10:12], securityTCTestChecksum(header))
	return append(append(frame, header...), body...)
}
