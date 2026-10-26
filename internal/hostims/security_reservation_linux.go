//go:build linux

package hostims

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Files survive a crash: a released flock alone never makes an uncertain
// scope reusable. Only confirmed teardown removes the matching reservation.
// Directory/filenames contain random scope hashes, never SIM identifiers.
const securityReservationDirectory = "/run/lock/vocat-host-ims-security"

type securityReservationFile struct {
	file    *os.File
	name    string
	content []byte
	stat    unix.Stat_t
}

type securityReservations struct {
	directory *os.File
	files     []*securityReservationFile
}

func openSecurityReservations(directory, namespace, token string, mark, client, server uint32, recovering bool) (*securityReservations, error) {
	if !hashString(namespace) || !hashString(token) || !validSecurityIDs(mark, client, server) {
		return nil, ErrParameters
	}
	if err := unix.Mkdir(directory, 0700); err != nil && err != unix.EEXIST {
		return nil, ErrJournal
	}
	dirfd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrJournal
	}
	var dirStat unix.Stat_t
	if unix.Fstat(dirfd, &dirStat) != nil || dirStat.Uid != uint32(os.Geteuid()) || dirStat.Mode&07777 != 0700 {
		unix.Close(dirfd)
		return nil, ErrJournal
	}
	r := &securityReservations{directory: os.NewFile(uintptr(dirfd), directory)}
	// Bound persistent orphan files as well as live scopes. Never garbage
	// collect a stale-looking reservation: it may represent an unknown ACK.
	entries, readErr := r.directory.ReadDir(4097)
	if readErr != nil && readErr != io.EOF || len(entries) > 4096 {
		r.closeFDs()
		return nil, ErrBusy
	}
	for index, id := range []uint32{mark, client, server} {
		kind := "reqid"
		if index == 0 {
			kind = "mark"
		}
		identifier := kind + ":" + strconv.FormatUint(uint64(id), 10)
		name := hashText(namespace+":"+identifier) + ".scope"
		content := []byte(token + "\n" + namespace + "\n" + identifier + "\n")
		flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if !recovering {
			flags |= unix.O_CREAT | unix.O_EXCL
		}
		fd, err := unix.Openat(dirfd, name, flags, 0600)
		if recovering && err == unix.ENOENT {
			// Recovery only cleans; it must not create a reservation or any new
			// kernel object to make an old journal appear complete.
			continue
		}
		if err != nil {
			if recovering {
				r.closeFDs()
			} else if r.release() != nil {
				return r, ErrUncertain
			}
			if err == unix.EEXIST || err == unix.EWOULDBLOCK {
				return nil, ErrBusy
			}
			return nil, ErrJournal
		}
		item := &securityReservationFile{file: os.NewFile(uintptr(fd), name), name: name, content: content}
		r.files = append(r.files, item)
		if unix.Fstat(fd, &item.stat) != nil || item.stat.Mode&unix.S_IFMT != unix.S_IFREG || item.stat.Uid != uint32(os.Geteuid()) || item.stat.Mode&07777 != 0600 || item.stat.Nlink != 1 ||
			unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
			if recovering {
				r.closeFDs()
				return nil, ErrBusy
			}
			return r, ErrUncertain
		}
		if recovering {
			data, readErr := io.ReadAll(io.LimitReader(item.file, 161))
			if readErr != nil || !bytes.Equal(data, content) {
				r.closeFDs()
				return nil, ErrJournal
			}
		} else {
			if _, err := item.file.Write(content); err != nil || item.file.Sync() != nil || unix.Fsync(dirfd) != nil {
				// The planned IDs/token were journaled before entry. Keep all
				// descriptors and names rooted for explicit cleanup/recovery.
				return r, ErrUncertain
			}
		}
	}
	return r, nil
}

func (r *securityReservations) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private security scope reservation>")
}

func (r *securityReservations) release() error {
	if r == nil || r.directory == nil {
		return nil
	}
	for len(r.files) > 0 {
		item := r.files[len(r.files)-1]
		fd, dirfd := int(item.file.Fd()), int(r.directory.Fd())
		var held, current unix.Stat_t
		if unix.Fstat(fd, &held) != nil || held.Dev != item.stat.Dev || held.Ino != item.stat.Ino || held.Nlink != 1 ||
			unix.Fstatat(dirfd, item.name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			current.Dev != held.Dev || current.Ino != held.Ino || current.Mode&07777 != 0600 || current.Uid != uint32(os.Geteuid()) {
			return ErrJournal
		}
		if _, err := item.file.Seek(0, io.SeekStart); err != nil {
			return ErrJournal
		}
		data, err := io.ReadAll(io.LimitReader(item.file, 161))
		if err != nil || !bytes.Equal(data, item.content) || unix.Unlinkat(dirfd, item.name, 0) != nil || unix.Fsync(dirfd) != nil {
			return ErrJournal
		}
		item.file.Close()
		r.files = r.files[:len(r.files)-1]
	}
	r.directory.Close()
	r.directory = nil
	return nil
}

// Only for process teardown or a failed read-only reopen. The durable names
// intentionally remain, so a later process cannot recycle uncertain IDs.
func (r *securityReservations) closeFDs() {
	if r == nil {
		return
	}
	for _, item := range r.files {
		item.file.Close()
	}
	if r.directory != nil {
		r.directory.Close()
	}
	r.directory = nil
}
