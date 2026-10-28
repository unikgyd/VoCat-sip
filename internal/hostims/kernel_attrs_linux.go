//go:build linux

package hostims

import (
	"encoding/binary"
	"os"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func netnsHandle(namespace *os.File) netns.NsHandle {
	if namespace == nil {
		return netns.None()
	}
	var current, target unix.Stat_t
	if unix.Stat("/proc/self/ns/net", &current) == nil && unix.Fstat(int(namespace.Fd()), &target) == nil {
		if current.Dev == target.Dev && current.Ino == target.Ino {
			return netns.None()
		}
	}
	return netns.NsHandle(namespace.Fd())
}

// The library accepts a trailing fragment smaller than rtattr. Validate the
// complete aligned stream before using its typed decoder for ownership proof.
func strictKernelAttrs(raw []byte) ([]syscall.NetlinkRouteAttr, error) {
	if len(raw) > 65536 {
		return nil, ErrParameters
	}
	count := 0
	for offset := 0; offset < len(raw); {
		if len(raw)-offset < 4 || count >= 256 {
			return nil, ErrParameters
		}
		size := int(binary.NativeEndian.Uint16(raw[offset : offset+2]))
		aligned := (size + 3) &^ 3
		if size < 4 || size > len(raw)-offset || aligned > len(raw)-offset {
			return nil, ErrParameters
		}
		offset += aligned
		count++
	}
	return nl.ParseRouteAttr(raw)
}
