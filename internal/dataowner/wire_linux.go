//go:build linux

// Package dataowner transfers requests, not QMI IDs or handles. The server
// retains the original live device manager and the main process's flock FD.
package dataowner

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"vocat/internal/device"
	"vocat/internal/modem"
)

const protocol = 1
const maxFrame = 32768

type Open struct {
	Candidate   modem.Candidate
	Generation  string
	ICCID, IMSI string
}
type frame struct {
	DatabaseDevice, DatabaseInode uint64
	Version                       int
	Op, Token, ID, Error          string
	Open                          *Open                  `json:",omitempty"`
	Request                       *device.NetworkRequest `json:",omitempty"`
	Result                        *device.NetworkResult  `json:",omitempty"`
	Status                        *device.NetworkStatus  `json:",omitempty"`
}

// Never open/close the database outside SQLite: POSIX close() drops that
// process's database locks. Pin the shared parent directory and compare stat
// metadata without creating another descriptor for the SQLite inode.
func sameDatabaseDirectory(directory *os.File, path string, deviceID, inode uint64) bool {
	actual, err := directory.Stat()
	if err != nil || !actual.IsDir() {
		return false
	}
	expected, err := os.Lstat(filepath.Dir(path))
	if err != nil || !expected.IsDir() || !os.SameFile(actual, expected) {
		return false
	}
	var st unix.Stat_t
	if unix.Fstatat(int(directory.Fd()), filepath.Base(path), &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 && int(st.Uid) == os.Geteuid() && st.Mode&0022 == 0 && uint64(st.Dev) == deviceID && st.Ino == inode
}

func writeFrame(c *net.UnixConn, f frame, rights []byte) error {
	f.Version = protocol
	raw, err := json.Marshal(f)
	if err != nil || len(raw) > maxFrame {
		return device.ErrDataCleanupPending
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	n, o, err := c.WriteMsgUnix(raw, rights, nil)
	if err != nil || n != len(raw) || o != len(rights) {
		return device.ErrDataCleanupPending
	}
	return nil
}

func readFrame(c *net.UnixConn) (frame, []*os.File, error) {
	var f frame
	raw := make([]byte, maxFrame+1)
	control := make([]byte, unix.CmsgSpace(8*4))
	n, o, flags, _, err := c.ReadMsgUnix(raw, control)
	var files []*os.File
	if o > 0 {
		messages, parseErr := unix.ParseSocketControlMessage(control[:o])
		if parseErr != nil {
			err = parseErr
		}
		for _, m := range messages {
			fds, parseErr := unix.ParseUnixRights(&m)
			if parseErr != nil {
				err = parseErr
				continue
			}
			for _, fd := range fds {
				unix.CloseOnExec(fd)
				files = append(files, os.NewFile(uintptr(fd), "data-owner-peer"))
			}
		}
	}
	if err == nil && (n == 0 || n > maxFrame || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0) {
		err = device.ErrDataCleanupPending
	}
	if err == nil {
		err = json.Unmarshal(raw[:n], &f)
		if err == nil && f.Version != protocol {
			err = device.ErrDataCleanupPending
		}
	}
	if err != nil {
		for _, file := range files {
			_ = file.Close()
		}
		return frame{}, nil, device.ErrDataCleanupPending
	}
	return f, files, nil
}

func credentials(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var peer *unix.Ucred
	var inner error
	err = raw.Control(func(fd uintptr) { peer, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	return peer, errors.Join(err, inner)
}
func sameFile(received *os.File, path string) bool {
	actual, err := received.Stat()
	if err != nil || !actual.Mode().IsRegular() {
		return false
	}
	expected, err := os.Lstat(path)
	if err != nil || !expected.Mode().IsRegular() || !os.SameFile(actual, expected) {
		return false
	}
	st, ok := actual.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1 && int(st.Uid) == os.Geteuid() && actual.Mode().Perm()&0022 == 0
}

// Prove the received descriptor carries the existing exclusive flock. A
// different open description cannot pass while that lock is held. Never issue
// LOCK_UN on a received descriptor: it is shared with the running main process.
func verifyInstanceLock(received *os.File, path string) bool {
	if !sameFile(received, path) {
		return false
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return false
	}
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		return false
	}
	return unix.Flock(int(received.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
}

func privateDirectory(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
func validPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) != "/"
}

func setError(err error) string {
	if errors.Is(err, device.ErrDataCleanupPending) {
		return "pending"
	}
	if errors.Is(err, device.ErrSIMObservation) {
		return "changed"
	}
	return "unavailable"
}
func getError(code string) error {
	if code == "" {
		return nil
	}
	if code == "changed" {
		return device.ErrSIMObservation
	}
	if code == "pending" {
		return device.ErrDataCleanupPending
	}
	return device.ErrDataBackendUnavailable
}
