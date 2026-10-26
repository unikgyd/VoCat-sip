//go:build !windows

package qmiport

import "os"

func openLeaseTestPort(path string) (portHandle, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}
