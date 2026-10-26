//go:build linux

package hostims

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

var (
	ErrXFRMClosed   = staticError("host IMS: scoped XFRM driver is closed")
	ErrXFRMSchema   = staticError("host IMS: unsupported or invalid raw XFRM schema")
	ErrXFRMConflict = staticError("host IMS: scoped XFRM object conflicts with the ownership record")
	ErrXFRMExists   = staticError("host IMS: XFRM ADD rejected because an object already exists")
	ErrXFRMUnknown  = staticError("host IMS: XFRM result is uncertain; the scope remains fenced")
	ErrXFRMRejected = staticError("host IMS: XFRM request explicitly rejected by the kernel")
)

// An explicit NETLINK_XFRM fd, never a netlink.Handle: there is no missing-family
// or closed-handle fallback which could silently open in a different netns.
type scopedXFRMDriver struct {
	gate              chan struct{}
	fd                int
	portID, sequence  uint32
	cookie            uint64
	namespace         *os.File
	closed, uncertain bool
	closing           atomic.Bool
}

// openScopedNetlinkFD borrows the held namespace until return. The caller owns
// fd binding, cookie verification, lifetime and ACK protocol. No library setns
// restore helper or implicit socket fallback is used, for either TC or XFRM.
func openScopedNetlinkFD(namespace *os.File, protocol int) (int, error) {
	if namespace == nil || (protocol != unix.NETLINK_ROUTE && protocol != unix.NETLINK_XFRM) {
		return -1, ErrXFRMSchema
	}
	pinned := namespace
	type opened struct {
		fd  int
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		runtime.LockOSThread()
		original, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			ch <- opened{-1, ErrXFRMRejected}
			return
		}
		defer original.Close()
		var fromStat, toStat unix.Stat_t
		if unix.Fstat(int(original.Fd()), &fromStat) != nil || unix.Fstat(int(pinned.Fd()), &toStat) != nil {
			runtime.UnlockOSThread()
			ch <- opened{-1, ErrXFRMRejected}
			return
		}
		changed := fromStat.Dev != toStat.Dev || fromStat.Ino != toStat.Ino
		if changed && unix.Setns(int(pinned.Fd()), unix.CLONE_NEWNET) != nil {
			runtime.UnlockOSThread()
			ch <- opened{-1, ErrXFRMRejected}
			return
		}
		fd, openErr := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, protocol)
		if changed && unix.Setns(int(original.Fd()), unix.CLONE_NEWNET) != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			// Never return an incorrectly restored thread to the Go pool. A
			// goroutine exiting while locked causes that OS thread to terminate.
			ch <- opened{-1, ErrXFRMRejected}
			return
		}
		runtime.UnlockOSThread()
		ch <- opened{fd, openErr}
	}()
	value := <-ch
	if value.err != nil {
		return -1, ErrXFRMRejected
	}
	return value.fd, nil
}

func openScopedXFRM(namespace *os.File, expectedCookie uint64) (*scopedXFRMDriver, error) {
	if namespace == nil || expectedCookie == 0 {
		return nil, ErrXFRMSchema
	}
	dup, err := unix.FcntlInt(namespace.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrXFRMRejected
	}
	pinned := os.NewFile(uintptr(dup), "private-XFRM-netns")
	fd, err := openScopedNetlinkFD(pinned, unix.NETLINK_XFRM)
	if err != nil {
		pinned.Close()
		return nil, err
	}
	d := &scopedXFRMDriver{fd: fd, namespace: pinned, cookie: expectedCookie, gate: make(chan struct{}, 1)}
	fail := func() (*scopedXFRMDriver, error) { _ = unix.Close(d.fd); pinned.Close(); return nil, ErrXFRMRejected }
	if unix.Bind(d.fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}) != nil {
		return fail()
	}
	if unix.SetsockoptInt(d.fd, unix.SOL_NETLINK, unix.NETLINK_CAP_ACK, 1) != nil {
		return fail()
	}
	// No multicast groups, no extended-ACK strings, no raw request/error logs.
	if unix.SetsockoptInt(d.fd, unix.SOL_NETLINK, unix.NETLINK_EXT_ACK, 0) != nil {
		return fail()
	}
	address, err := unix.Getsockname(d.fd)
	local, ok := address.(*unix.SockaddrNetlink)
	if err != nil || !ok || local.Pid == 0 || local.Groups != 0 {
		return fail()
	}
	d.portID = local.Pid
	actualCookie, err := unix.GetsockoptUint64(d.fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil || actualCookie != expectedCookie {
		return fail()
	}
	d.gate <- struct{}{}
	return d, nil
}

func (d *scopedXFRMDriver) lock(ctx context.Context) error {
	if d == nil || ctx == nil {
		return ErrXFRMClosed
	}
	if d.closing.Load() {
		return ErrXFRMClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.gate:
	}
	if d.closed || d.closing.Load() {
		d.gate <- struct{}{}
		return ErrXFRMClosed
	}
	if d.uncertain {
		d.gate <- struct{}{}
		return ErrXFRMUnknown
	}
	cookie, err := unix.GetsockoptUint64(d.fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil || cookie != d.cookie {
		d.uncertain = true
		d.gate <- struct{}{}
		return ErrXFRMUnknown
	}
	return nil
}
func (d *scopedXFRMDriver) unlock() { d.gate <- struct{}{} }

func (d *scopedXFRMDriver) Close() error {
	if d == nil {
		return nil
	}
	d.closing.Store(true)
	<-d.gate
	defer d.unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	first := unix.Close(d.fd)
	d.fd = -1
	second := d.namespace.Close()
	d.namespace = nil
	if first != nil || second != nil {
		return ErrXFRMRejected
	}
	return nil
}

func (d *scopedXFRMDriver) Add(ctx context.Context, operation scopedXFRMOperation) error {
	if validateScopedXFRMKey(operation.Key) != nil {
		return ErrXFRMSchema
	}
	snapshot, err := scopedXFRMSnapshotOf(operation.Key.Kind, operation.wireAdd)
	if err != nil || !snapshot.matchesKey(operation.Key) || snapshot.MatchFingerprint != operation.Fingerprint ||
		operation.Key.Kind == "policy" && snapshot.PolicyIndex != 0 {
		return ErrXFRMSchema
	}
	if err := d.lock(ctx); err != nil {
		return err
	}
	defer d.unlock()
	message := uint16(nl.XFRM_MSG_NEWSA)
	if operation.Key.Kind == "policy" {
		message = nl.XFRM_MSG_NEWPOLICY
	}
	_, err = d.exchange(ctx, message, unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_EXCL, operation.wireAdd, 0, false)
	return err
}

func (d *scopedXFRMDriver) Observe(ctx context.Context, key scopedXFRMKey) (scopedXFRMSnapshot, bool, error) {
	if validateScopedXFRMKey(key) != nil {
		return scopedXFRMSnapshot{}, false, ErrXFRMSchema
	}
	if err := d.lock(ctx); err != nil {
		return scopedXFRMSnapshot{}, false, err
	}
	defer d.unlock()
	return d.observe(ctx, key)
}

func scopedXFRMRequestKey(key scopedXFRMKey, policyIndex uint32) []byte {
	var wire []byte
	if key.Kind == "state" {
		id := nl.XfrmUsersaId{Daddr: scopedXFRMAddress(key.Dst), Spi: nl.Swap32(key.SPI), Family: scopedXFRMFamily(key.Dst), Proto: unix.IPPROTO_ESP}
		wire = append(wire, id.Serialize()...)
		address := scopedXFRMAddress(key.Src)
		wire = scopedXFRMAttr(wire, nl.XFRMA_SRCADDR, address[:])
	} else {
		id := nl.XfrmUserpolicyId{Sel: scopedXFRMSelector(key), Dir: key.Direction, Index: policyIndex}
		wire = append(wire, id.Serialize()...)
	}
	return scopedXFRMAttr(wire, nl.XFRMA_MARK, scopedXFRMMark(key.Mark))
}

func (d *scopedXFRMDriver) observe(ctx context.Context, key scopedXFRMKey) (scopedXFRMSnapshot, bool, error) {
	message, response := uint16(nl.XFRM_MSG_GETSA), uint16(nl.XFRM_MSG_NEWSA)
	if key.Kind == "policy" {
		message, response = nl.XFRM_MSG_GETPOLICY, nl.XFRM_MSG_NEWPOLICY
	}
	replies, err := d.exchange(ctx, message, unix.NLM_F_ACK, scopedXFRMRequestKey(key, 0), response, false)
	defer scopedXFRMClearReplies(replies)
	if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
		return scopedXFRMSnapshot{}, false, nil
	}
	if err != nil {
		return scopedXFRMSnapshot{}, false, err
	}
	if len(replies) != 1 {
		return scopedXFRMSnapshot{}, false, ErrXFRMSchema
	}
	snapshot, err := scopedXFRMSnapshotOf(key.Kind, replies[0])
	if err != nil {
		return scopedXFRMSnapshot{}, false, err
	}
	if !snapshot.matchesKey(key) {
		return scopedXFRMSnapshot{}, true, ErrXFRMConflict
	}
	return snapshot, true, nil
}

// Delete never infers ownership. Only call it with a complete fingerprint from
// an independently ACK-owned journal record. There is no kernel compare/delete
// CAS against a hostile CAP_NET_ADMIN peer; that boundary is not hidden here.
func (d *scopedXFRMDriver) Delete(ctx context.Context, key scopedXFRMKey, expectedFingerprint string) error {
	if validateScopedXFRMKey(key) != nil || len(expectedFingerprint) != 64 {
		return ErrXFRMSchema
	}
	if err := d.lock(ctx); err != nil {
		return err
	}
	defer d.unlock()
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	snapshot, exists, err := d.observe(bounded, key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	} // a negative GET ACK proves absence
	if snapshot.Fingerprint != expectedFingerprint {
		return ErrXFRMConflict
	}
	message := uint16(nl.XFRM_MSG_DELSA)
	if key.Kind == "policy" {
		message = nl.XFRM_MSG_DELPOLICY
	}
	_, err = d.exchange(bounded, message, unix.NLM_F_ACK, scopedXFRMRequestKey(key, snapshot.PolicyIndex), 0, false)
	return err
}

// ScopeVacant is a conservative reservation check under the owner's three
// cross-process locks. It checks all raw state/policy objects, including socket
// policies. Unrelated unmarked SAs do not occupy every possible mark.
func (d *scopedXFRMDriver) ScopeVacant(ctx context.Context, mark, clientReqID, serverReqID uint32) error {
	if mark == 0 || clientReqID == 0 || serverReqID == 0 || clientReqID == serverReqID {
		return ErrXFRMSchema
	}
	if err := d.lock(ctx); err != nil {
		return err
	}
	defer d.unlock()
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, kind := range []string{"state", "policy"} {
		message, response := uint16(nl.XFRM_MSG_GETSA), uint16(nl.XFRM_MSG_NEWSA)
		var body []byte
		if kind == "policy" {
			message, response = nl.XFRM_MSG_GETPOLICY, nl.XFRM_MSG_NEWPOLICY
			body = make([]byte, 16)
		}
		replies, err := d.exchange(bounded, message, unix.NLM_F_DUMP, body, response, true)
		if err != nil {
			scopedXFRMClearReplies(replies)
			return err
		}
		conflict := false
		for _, reply := range replies {
			snapshot, err := scopedXFRMSnapshotOf(kind, reply)
			if err != nil {
				scopedXFRMClearReplies(replies)
				return err
			}
			if snapshot.mask != 0 && mark&snapshot.mask == snapshot.mark {
				conflict = true
			}
			for _, reqid := range snapshot.reqids {
				if reqid == clientReqID || reqid == serverReqID {
					conflict = true
				}
			}
		}
		scopedXFRMClearReplies(replies)
		if conflict {
			return ErrXFRMConflict
		}
	}
	return nil
}

func scopedXFRMClearReplies(replies [][]byte) {
	for _, reply := range replies {
		clear(reply)
	}
}

// exchange owns every byte of the response, verifies kernel PID, port ID,
// sequence, ACK framing and dump completion, and never retries a mutation.
func (d *scopedXFRMDriver) exchange(parent context.Context, message, flags uint16, body []byte, response uint16, dump bool) (result [][]byte, err error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.sequence++
	if d.sequence == 0 {
		d.sequence++
	}
	sequence := d.sequence
	wire := make([]byte, 16+len(body))
	defer clear(wire)
	native := nl.NativeEndian()
	native.PutUint32(wire, uint32(len(wire)))
	native.PutUint16(wire[4:], message)
	native.PutUint16(wire[6:], flags|unix.NLM_F_REQUEST)
	native.PutUint32(wire[8:], sequence)
	native.PutUint32(wire[12:], d.portID)
	copy(wire[16:], body)
	unknown := func() ([][]byte, error) {
		d.uncertain = true
		scopedXFRMClearReplies(result)
		return nil, ErrXFRMUnknown
	}
	if err := d.wait(ctx, unix.POLLOUT); err != nil {
		return nil, err
	} // no send yet
	// Exactly one send attempt. Even EINTR/EAGAIN is conservatively uncertain;
	// never issue a second ADD based on an assumption that the first was lost.
	if unix.Sendto(d.fd, wire, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}) != nil {
		return unknown()
	}
	buffer := make([]byte, 65536)
	defer clear(buffer)
	totalReplyBytes := 0
	for {
		if d.wait(ctx, unix.POLLIN) != nil {
			return unknown()
		}
		n, _, recvFlags, from, recvErr := unix.Recvmsg(d.fd, buffer, nil, 0)
		if recvErr == unix.EAGAIN || recvErr == unix.EINTR {
			continue
		}
		peer, ok := from.(*unix.SockaddrNetlink)
		if recvErr != nil || !ok || peer.Pid != 0 || peer.Groups != 0 || recvFlags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n < 16 {
			return unknown()
		}
		packet := buffer[:n]
		for len(packet) != 0 {
			if len(packet) < 16 {
				return unknown()
			}
			length := int(native.Uint32(packet))
			aligned := (length + 3) &^ 3
			if length < 16 || aligned > len(packet) || !scopedXFRMZero(packet[length:aligned]) {
				return unknown()
			}
			typeID, replyFlags := native.Uint16(packet[4:]), native.Uint16(packet[6:])
			if native.Uint32(packet[8:]) != sequence || native.Uint32(packet[12:]) != d.portID || replyFlags&unix.NLM_F_DUMP_INTR != 0 {
				return unknown()
			}
			payload := packet[16:length]
			switch typeID {
			case unix.NLMSG_ERROR:
				if dump || len(payload) != 20 || replyFlags & ^uint16(unix.NLM_F_CAPPED) != 0 ||
					native.Uint32(payload[4:]) != uint32(len(wire)) || native.Uint16(payload[8:]) != message ||
					native.Uint16(payload[10:]) != flags|unix.NLM_F_REQUEST ||
					native.Uint32(payload[12:]) != sequence || native.Uint32(payload[16:]) != d.portID {
					return unknown()
				}
				code := int32(native.Uint32(payload))
				if aligned != len(packet) {
					return unknown()
				}
				if code == 0 {
					if response != 0 && len(result) != 1 {
						return unknown()
					}
					return result, nil
				}
				if code >= 0 || code < -4095 || len(result) != 0 {
					return unknown()
				}
				scopedXFRMClearReplies(result)
				result = nil
				errno := unix.Errno(-code)
				if errno == unix.EEXIST {
					return nil, errors.Join(ErrXFRMExists, errno)
				}
				return nil, errors.Join(ErrXFRMRejected, errno)
			case unix.NLMSG_DONE:
				if !dump || replyFlags & ^uint16(unix.NLM_F_MULTI) != 0 || len(payload) != 4 || native.Uint32(payload) != 0 || aligned != len(packet) {
					return unknown()
				}
				return result, nil
			default:
				allowedFlags := uint16(0)
				if dump {
					allowedFlags = unix.NLM_F_MULTI
				}
				totalReplyBytes += len(payload)
				if typeID != response || response == 0 || replyFlags & ^allowedFlags != 0 ||
					(!dump && len(result) != 0) || len(result) >= 4096 || totalReplyBytes > 4*1024*1024 {
					return unknown()
				}
				result = append(result, append([]byte(nil), payload...))
			}
			packet = packet[aligned:]
		}
	}
}

func (d *scopedXFRMDriver) wait(ctx context.Context, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := 50
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			if remaining < 50*time.Millisecond {
				timeout = int((remaining + time.Millisecond - 1) / time.Millisecond)
			}
		}
		fds := []unix.PollFd{{Fd: int32(d.fd), Events: events}}
		count, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if count != 0 {
			if fds[0].Revents&events != 0 {
				return nil
			}
			return ErrXFRMUnknown
		}
	}
}
