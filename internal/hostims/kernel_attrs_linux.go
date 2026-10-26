//go:build linux

package hostims

import (
	"encoding/binary"
	"syscall"

	"github.com/vishvananda/netlink/nl"
)

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
