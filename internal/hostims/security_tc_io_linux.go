//go:build linux

package hostims

import (
	"os"
	"syscall"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Small explicit transport: the namespace helper checks restoration itself and
// destroys an incorrectly restored OS thread. There is never a library handle
// fallback to the calling goroutine's current network namespace.
type securityTCNetlink struct {
	file     *os.File
	fd       int
	deadline time.Time
}

func openSecurityTCNetlink(namespace *os.File) (*securityTCNetlink, error) {
	fd, err := openScopedNetlinkFD(namespace, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, ErrSecurityTCClosed
	}
	fail := func() (*securityTCNetlink, error) {
		unix.Close(fd)
		return nil, ErrSecurityTCClosed
	}
	if unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}) != nil ||
		unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_CAP_ACK, 1) != nil ||
		unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_EXT_ACK, 0) != nil {
		return fail()
	}
	address, err := unix.Getsockname(fd)
	local, ok := address.(*unix.SockaddrNetlink)
	if err != nil || !ok || local.Pid == 0 || local.Groups != 0 {
		return fail()
	}
	return &securityTCNetlink{file: os.NewFile(uintptr(fd), "private-host-ims-tc"), fd: fd}, nil
}

func (s *securityTCNetlink) GetFd() int                      { return s.fd }
func (s *securityTCNetlink) BeginRequest(deadline time.Time) { s.deadline = deadline }
func (s *securityTCNetlink) GetPid() (uint32, error) {
	address, err := unix.Getsockname(s.fd)
	local, ok := address.(*unix.SockaddrNetlink)
	if err != nil || !ok || local.Pid == 0 || local.Groups != 0 {
		return 0, ErrSecurityTCClosed
	}
	return local.Pid, nil
}
func (s *securityTCNetlink) Close() { s.file.Close() }

func (s *securityTCNetlink) Send(request *nl.NetlinkRequest) (bool, error) {
	connection, err := s.file.SyscallConn()
	if err != nil {
		return false, err
	}
	if err := s.file.SetWriteDeadline(s.deadline); err != nil {
		return false, err
	}
	packet := request.Serialize()
	sent := false
	var operationErr error
	err = connection.Write(func(fd uintptr) bool {
		for {
			operationErr = unix.Sendto(int(fd), packet, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
			if operationErr != unix.EINTR || !time.Now().Before(s.deadline) {
				break
			}
		}
		if operationErr == nil {
			sent = true
		}
		return operationErr != unix.EWOULDBLOCK && operationErr != unix.EAGAIN
	})
	if operationErr != nil {
		return sent, operationErr
	}
	return sent, err
}

func (s *securityTCNetlink) Receive() ([]syscall.NetlinkMessage, *unix.SockaddrNetlink, error) {
	connection, err := s.file.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	if err := s.file.SetReadDeadline(s.deadline); err != nil {
		return nil, nil, err
	}
	buffer := make([]byte, 65536)
	var count, flags int
	var from unix.Sockaddr
	var operationErr error
	err = connection.Read(func(fd uintptr) bool {
		for {
			count, _, flags, from, operationErr = unix.Recvmsg(int(fd), buffer, nil, 0)
			if operationErr != unix.EINTR || !time.Now().Before(s.deadline) {
				break
			}
		}
		return operationErr != unix.EWOULDBLOCK && operationErr != unix.EAGAIN
	})
	if err != nil || operationErr != nil || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || count <= 0 {
		return nil, nil, ErrSecurityTCClosed
	}
	address, ok := from.(*unix.SockaddrNetlink)
	if !ok || address.Pid != 0 || address.Groups != 0 {
		return nil, nil, ErrSecurityTCClosed
	}
	messages, err := syscall.ParseNetlinkMessage(buffer[:count])
	return messages, address, err
}
