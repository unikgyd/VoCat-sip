//go:build linux

package hostims

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type endpointRoots struct{ dev, sys string }
type endpointNode struct {
	path string
	file *os.File
	stat unix.Stat_t
}
type qmiEndpointPin struct {
	mu                                      sync.Mutex
	control, physical, charTarget, charLink string
	nodes                                   []endpointNode
	closed                                  bool
}

func (*qmiEndpointPin) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private pinned QMI endpoint>")
}
func withinDirectory(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func sameEndpointNode(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Rdev == b.Rdev && a.Mode == b.Mode && a.Ctim == b.Ctim && b.Nlink > 0
}
func pinEndpointNode(path string, directory bool) (endpointNode, error) {
	flags := unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return endpointNode{}, ErrSelection
	}
	node := endpointNode{path: path, file: os.NewFile(uintptr(fd), "pinned-qmi-node")}
	if unix.Fstat(fd, &node.stat) != nil || node.stat.Nlink == 0 || !directory && node.stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		_ = node.file.Close()
		return endpointNode{}, ErrSelection
	}
	return node, nil
}
func pinQMIEndpoint(selection Selection, roots endpointRoots) (*qmiEndpointPin, error) {
	if roots.dev == "" || roots.sys == "" || !filepath.IsAbs(roots.dev) || !filepath.IsAbs(roots.sys) {
		return nil, ErrSelection
	}
	name, valid := mmPortName(selection.snapshot.QMIControl)
	if !valid || selection.snapshot.QMIControl != "/dev/"+name || !strings.HasPrefix(selection.snapshot.Binding.Physical, "/sys/devices/") {
		return nil, ErrSelection
	}
	p := &qmiEndpointPin{control: filepath.Join(roots.dev, name), physical: filepath.Join(roots.sys, strings.TrimPrefix(selection.snapshot.Binding.Physical, "/sys/"))}
	success := false
	defer func() {
		if !success {
			_ = p.Close()
		}
	}()
	canonical, err := filepath.EvalSymlinks(p.physical)
	if err != nil || canonical != p.physical || !withinDirectory(filepath.Join(roots.sys, "devices"), p.physical) {
		return nil, ErrSelection
	}
	physical, err := pinEndpointNode(p.physical, true)
	if err != nil {
		return nil, err
	}
	p.nodes = append(p.nodes, physical)
	control, err := pinEndpointNode(p.control, false)
	if err != nil {
		return nil, err
	}
	p.nodes = append(p.nodes, control)
	p.charLink = filepath.Join(roots.sys, "dev", "char", fmt.Sprintf("%d:%d", unix.Major(control.stat.Rdev), unix.Minor(control.stat.Rdev)))
	p.charTarget, err = filepath.EvalSymlinks(p.charLink)
	if err != nil || !withinDirectory(p.physical, p.charTarget) {
		return nil, ErrSelection
	}
	// Reject a /dev tty or other character port mislabeled as QMI. Both USB
	// cdc-wdm and native WWAN QMI nodes must have their actual sysfs class.
	subsystem, err := filepath.EvalSymlinks(filepath.Join(p.charTarget, "subsystem"))
	if err != nil {
		return nil, ErrSelection
	}
	switch filepath.Base(subsystem) {
	case "usbmisc", "wwan", "mhi":
	default:
		return nil, ErrSelection
	}
	charNode, err := pinEndpointNode(p.charTarget, true)
	if err != nil {
		return nil, err
	}
	p.nodes = append(p.nodes, charNode)
	if err := p.Verify(context.Background()); err != nil {
		return nil, err
	}
	success = true
	return p, nil
}
func (p *qmiEndpointPin) Path() string { return p.control }
func (p *qmiEndpointPin) Verify(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrChanged
	}
	for _, node := range p.nodes {
		var held, current unix.Stat_t
		if unix.Fstat(int(node.file.Fd()), &held) != nil || unix.Lstat(node.path, &current) != nil || !sameEndpointNode(node.stat, held) || !sameEndpointNode(node.stat, current) {
			return ErrChanged
		}
	}
	target, err := filepath.EvalSymlinks(p.charLink)
	if err != nil || target != p.charTarget || !withinDirectory(p.physical, target) {
		return ErrChanged
	}
	return nil
}
func (p *qmiEndpointPin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var failed bool
	for _, node := range p.nodes {
		if node.file.Close() != nil {
			failed = true
		}
	}
	if failed {
		return ErrTransport
	}
	return nil
}

// ProbeQMIEndpoint verifies only filesystem metadata (O_PATH); it does not
// open the driver for I/O, connect qmi-proxy, allocate CID, or read the UICC.
func ProbeQMIEndpoint(ctx context.Context, selection Selection) (string, error) {
	if selection.pool == nil {
		return "", ErrSelection
	}
	snapshot, err := selection.pool.verify(ctx, selection)
	if err != nil || snapshot.QMIControl != selection.snapshot.QMIControl {
		return "", ErrChanged
	}
	pin, err := pinQMIEndpoint(selection, endpointRoots{dev: "/dev", sys: "/sys"})
	if err != nil {
		return "", err
	}
	defer pin.Close()
	again, err := selection.pool.verify(ctx, selection)
	if err != nil || again.QMIControl != selection.snapshot.QMIControl {
		return "", ErrChanged
	}
	if err := pin.Verify(ctx); err != nil {
		return "", err
	}
	return filepath.Base(pin.Path()), nil
}
