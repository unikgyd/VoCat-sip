//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Replace this service process rather than spawning qmi-proxy in the HTTP
// service's cgroup. A process group/session alone does not survive systemd's
// default control-group cleanup when the HTTP process dies.
func runQMIProxy(args []string) error {
	if len(args) == 1 && args[0] == "--check" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("unix", "\x00qmi-proxy", time.Second)
			if err == nil {
				return conn.Close()
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("shared QMI proxy is not ready: %w", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if len(args) != 0 {
		return errors.New("usage: vocat qmi-proxy [--check]")
	}
	if os.Geteuid() != 0 {
		return errors.New("qmi-proxy service requires root")
	}
	conn, err := net.DialTimeout("unix", "\x00qmi-proxy", time.Second)
	if err == nil {
		_ = conn.Close()
		return errors.New("an existing qmi-proxy already serves this namespace; drain its clients and supervise its original lifecycle before enabling the optional native owner layout")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot establish that the shared QMI proxy socket is unused: %w", err)
	}
	paths := []string{"/usr/libexec/qmi-proxy", "/usr/lib/qmi-proxy", "/usr/lib/libqmi-glib/qmi-proxy"}
	if path, err := exec.LookPath("qmi-proxy"); err == nil {
		paths = append([]string{path}, paths...)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return syscall.Exec(path, []string{path, "--no-exit"}, os.Environ())
		}
	}
	return errors.New("qmi-proxy executable is unavailable; install the libqmi proxy package")
}
