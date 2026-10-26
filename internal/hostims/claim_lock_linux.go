//go:build linux

package hostims

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Every production SQLite journal uses the same machine-wide directory. The
// advisory FD stays open through active/uncertain operations; no TTL takeover.
func lockClaimAt(directory, key string) (io.Closer, error) {
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 || !filepath.IsAbs(directory) || filepath.Clean(directory) == "/" {
		return nil, fmt.Errorf("%w: invalid lock selection", ErrJournal)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create lock directory", ErrJournal)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: private lock directory permissions", ErrJournal)
	}
	dirfd, err := unix.Open(directory, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open lock directory", ErrJournal)
	}
	defer unix.Close(dirfd)
	var dirStat unix.Stat_t
	if unix.Fstat(dirfd, &dirStat) != nil || int(dirStat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("%w: lock directory owner", ErrJournal)
	}
	fd, err := unix.Openat(dirfd, key+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open claim lock", ErrJournal)
	}
	var fileStat unix.Stat_t
	if unix.Fstat(fd, &fileStat) != nil || fileStat.Mode&unix.S_IFMT != unix.S_IFREG || int(fileStat.Uid) != os.Geteuid() || fileStat.Nlink != 1 || fileStat.Mode&0o077 != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: claim lock owner/type/permissions", ErrJournal)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return nil, ErrBusy
	}
	return os.NewFile(uintptr(fd), filepath.Join(directory, key+".lock")), nil
}
