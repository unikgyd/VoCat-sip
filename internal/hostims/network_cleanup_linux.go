//go:build linux

package hostims

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// cleanupIdentity authorizes only cleanup of already acknowledged, journaled
// objects, NEVER new sockets, traffic, SAs or routes. Its caller serializes it
// against driver close using the network-lease gate. It intentionally does not
// consult modem/SIM/PDN state, interface UP/MTU, or the original interface name.
//
// alive=false is stronger than "LinkByIndex failed": the permanent ifindex
// attribute must be removed from the original held SYSFS inode AND the pinned
// route socket must find no current link backed by that inode. A namespace move,
// rename, inaccessible sysfs, transient race or mere name disappearance cannot
// falsely retire a TC ledger. When an index has been reused, the replacement's
// different inode is observed but NEVER authorized for deletion.
func (k *linuxNetworkKernel) cleanupIdentity(ctx context.Context) (alive bool, err error) {
	if ctx == nil {
		return false, ErrNetwork
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if k == nil || k.handle == nil || k.namespace == nil || k.ruleSocket == nil || k.ruleSocket.Socket == nil ||
		k.anchor.file == nil || k.index <= 0 || k.namespaceCookie == 0 {
		return false, ErrNetwork
	}
	namespace, socket, anchor := k.namespace, k.ruleSocket, k.anchor
	if !cleanupNamespaceMatches(namespace, k.namespaceKey) || !cleanupRouteSocketMatches(socket, k.namespaceCookie) {
		return false, ErrNetwork
	}
	var held unix.Stat_t
	if unix.Fstat(int(anchor.file.Fd()), &held) != nil || !sameCleanupNetdev(anchor.stat, held) || !cleanupSysfsFD(int(anchor.file.Fd())) {
		return false, ErrNetwork
	}
	index, missing, indexErr := cleanupSysfsIndex(int(anchor.file.Fd()))
	if indexErr != nil || !missing && index != k.index {
		return false, ErrNetwork
	}
	name, present, err := cleanupLinkName(ctx, socket, k.index)
	if err != nil {
		return false, err
	}
	if present {
		current, err := cleanupCurrentNetdev(name, k.index)
		if err != nil {
			return false, err
		}
		if sameCleanupNetdev(anchor.stat, current) {
			if missing || held.Nlink == 0 || current.Nlink == 0 {
				return false, ErrNetwork
			}
			// Recheck the held inode after the name-based snapshot. A rename
			// during the lookup may cause a retry, never an assumption of death.
			var after unix.Stat_t
			if unix.Fstat(int(anchor.file.Fd()), &after) != nil || !sameCleanupNetdev(anchor.stat, after) || after.Nlink == 0 {
				return false, ErrNetwork
			}
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return true, nil
		}
		// Another interface has reused the old index. Only the independent
		// original-inode tombstone below can establish that our old TC died.
	}
	// kernfs can retain st_nlink=2 and its old proc-fd path on a removed
	// O_PATH inode. Netdev core's permanent ifindex attribute disappears on
	// kobject removal, after qdisc shutdown; it does not disappear on rename,
	// link-down, MTU change or a mere missing name in our mount namespace.
	if !missing {
		return false, ErrNetwork
	}
	var after unix.Stat_t
	if unix.Fstat(int(anchor.file.Fd()), &after) != nil || !sameCleanupNetdev(anchor.stat, after) {
		return false, ErrNetwork
	}
	_, missingAgain, err := cleanupSysfsIndex(int(anchor.file.Fd()))
	if err != nil || !missingAgain || !cleanupNamespaceMatches(namespace, k.namespaceKey) || !cleanupRouteSocketMatches(socket, k.namespaceCookie) {
		return false, ErrNetwork
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, nil
}

func cleanupNamespaceMatches(namespace *os.File, expected string) bool {
	var stat unix.Stat_t
	var filesystem unix.Statfs_t
	return namespace != nil && unix.Fstat(int(namespace.Fd()), &stat) == nil && unix.Fstatfs(int(namespace.Fd()), &filesystem) == nil &&
		filesystem.Type == unix.NSFS_MAGIC && hashText(fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)) == expected
}

func cleanupRouteSocketMatches(socket *nl.SocketHandle, expected uint64) bool {
	if socket == nil || socket.Socket == nil || expected == 0 {
		return false
	}
	fd := socket.Socket.GetFd()
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil || cookie != expected {
		return false
	}
	family, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN)
	if err != nil || family != unix.AF_NETLINK {
		return false
	}
	protocol, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PROTOCOL)
	return err == nil && protocol == unix.NETLINK_ROUTE
}

func sameCleanupNetdev(original, current unix.Stat_t) bool {
	// kernfs keeps an unlinked inode pinned by O_PATH. Its ctime/nlink may
	// change during rename/removal, but dev+ino cannot be reused while held.
	return original.Dev == current.Dev && original.Ino == current.Ino && original.Rdev == current.Rdev &&
		original.Mode&unix.S_IFMT == unix.S_IFDIR && current.Mode&unix.S_IFMT == unix.S_IFDIR
}

func cleanupSysfsFD(fd int) bool {
	var filesystem unix.Statfs_t
	return unix.Fstatfs(fd, &filesystem) == nil && filesystem.Type == unix.SYSFS_MAGIC
}

func cleanupSysfsIndex(directory int) (index int, missing bool, err error) {
	fd, err := unix.Openat(directory, "ifindex", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODEV) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, ErrNetwork
	}
	file := os.NewFile(uintptr(fd), "original-netdev-ifindex")
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || !cleanupSysfsFD(fd) {
		return 0, false, ErrNetwork
	}
	value, err := io.ReadAll(io.LimitReader(file, 33))
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODEV) {
		return 0, true, nil
	}
	if err != nil || len(value) < 2 || len(value) > 32 || value[len(value)-1] != '\n' {
		return 0, false, ErrNetwork
	}
	index64, err := strconv.ParseUint(string(value[:len(value)-1]), 10, 31)
	if err != nil || index64 == 0 || strconv.FormatUint(index64, 10)+"\n" != string(value) {
		return 0, false, ErrNetwork
	}
	return int(index64), false, nil
}

func cleanupLinkName(ctx context.Context, socket *nl.SocketHandle, index int) (string, bool, error) {
	if ctx.Err() != nil || socket == nil || socket.Socket == nil {
		return "", false, ErrNetwork
	}
	request := nl.NewNetlinkRequest(unix.RTM_GETLINK, 0)
	// Use the held explicit socket, not Handle.LinkByIndex: Handle.Close clears
	// its socket map and can otherwise silently reopen in the current netns.
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: socket}
	message := nl.NewIfInfomsg(unix.AF_UNSPEC)
	message.Index = int32(index)
	request.AddData(message)
	rows, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	if errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
		return "", false, nil
	}
	if err != nil || len(rows) != 1 || len(rows[0]) < unix.SizeofIfInfomsg || rows[0][0] != unix.AF_UNSPEC || rows[0][1] != 0 ||
		int32(binary.NativeEndian.Uint32(rows[0][4:8])) != int32(index) {
		return "", false, ErrNetwork
	}
	attrs, err := strictKernelAttrs(rows[0][unix.SizeofIfInfomsg:])
	if err != nil {
		return "", false, ErrNetwork
	}
	var name string
	for _, attr := range attrs {
		if attr.Attr.Type != unix.IFLA_IFNAME {
			continue
		}
		if name != "" || len(attr.Value) < 2 || len(attr.Value) > unix.IFNAMSIZ || attr.Value[len(attr.Value)-1] != 0 ||
			bytes.IndexByte(attr.Value[:len(attr.Value)-1], 0) >= 0 {
			return "", false, ErrNetwork
		}
		name = string(attr.Value[:len(attr.Value)-1])
	}
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", false, ErrNetwork
	}
	return name, true, nil
}

func cleanupCurrentNetdev(name string, index int) (unix.Stat_t, error) {
	if name == "" || len(name) >= unix.IFNAMSIZ || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || index <= 0 {
		return unix.Stat_t{}, ErrNetwork
	}
	class, err := unix.Open("/sys/class/net", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return unix.Stat_t{}, ErrNetwork
	}
	defer unix.Close(class)
	if !cleanupSysfsFD(class) {
		return unix.Stat_t{}, ErrNetwork
	}
	// /sys/class/net entries are kernel symlinks. The final directory must
	// still be on SYSFS and later match the pinned object's inode exactly.
	fd, err := unix.Openat(class, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return unix.Stat_t{}, ErrNetwork
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Nlink == 0 || !cleanupSysfsFD(fd) {
		return unix.Stat_t{}, ErrNetwork
	}
	current, missing, err := cleanupSysfsIndex(fd)
	if err != nil || missing || current != index {
		return unix.Stat_t{}, ErrNetwork
	}
	return stat, nil
}
