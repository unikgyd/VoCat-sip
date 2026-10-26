//go:build linux

package vowifi

import (
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type systemIKERetryClock struct{}

func (systemIKERetryClock) Snapshot() (IKERetryTime, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return IKERetryTime{}, err
	}
	var uptime unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &uptime); err != nil {
		return IKERetryTime{}, err
	}
	return IKERetryTime{Wall: time.Now().UTC(), Boot: strings.TrimSpace(string(boot)), Uptime: time.Duration(uptime.Nano())}, nil
}
