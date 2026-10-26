//go:build windows

package qmiport

import (
	"os"

	"golang.org/x/sys/windows"
)

// The fixture models a Linux device-node unlink while its keeper is open.
// Windows regular files require explicit delete sharing for that operation;
// do not alter production Linux port ownership just to run this test here.
func openLeaseTestPort(path string) (portHandle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
