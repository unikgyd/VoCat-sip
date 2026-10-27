//go:build linux

package dataowner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
	"vocat/internal/device"
	"vocat/internal/modem"
)

type Client struct {
	socket, database string
	instance         *os.File
	mu               sync.Mutex
	entries          map[string]*entry
	closing          bool
}
type entry struct {
	candidate          modem.Candidate
	iccid, imsi, token string
	conn               *net.UnixConn
	ready              chan struct{}
	replies            chan frame
	done               chan struct{}
	gate               chan struct{}
	once               sync.Once
	alive              atomic.Bool
}

func NewClient(socket, database string, instance *os.File) (*Client, error) {
	if !validPath(socket) || !validPath(database) || instance == nil || !privateDirectory(filepath.Dir(socket)) {
		return nil, device.ErrDataBackendUnavailable
	}
	return &Client{socket: socket, database: database, instance: instance, entries: make(map[string]*entry)}, nil
}

func (e *entry) fail() {
	e.once.Do(func() {
		e.alive.Store(false)
		if e.conn != nil {
			_ = e.conn.Close()
		}
		close(e.done)
	})
}
func (e *entry) read() {
	defer e.fail()
	for {
		f, files, err := readFrame(e.conn)
		for _, file := range files {
			_ = file.Close()
		}
		if err != nil || len(files) != 0 {
			return
		}
		select {
		case e.replies <- f:
		case <-e.done:
			return
		default:
			return
		}
	}
}

func (e *entry) call(ctx context.Context, f frame, rights []byte) (frame, error) {
	select {
	case e.gate <- struct{}{}:
	case <-ctx.Done():
		return frame{}, errors.Join(device.ErrDataCleanupPending, ctx.Err())
	}
	defer func() { <-e.gate }()
	select {
	case <-e.done:
		return frame{}, device.ErrDataCleanupPending
	default:
	}
	if err := ctx.Err(); err != nil {
		return frame{}, errors.Join(device.ErrDataCleanupPending, err)
	}
	if writeFrame(e.conn, f, rights) != nil {
		e.fail()
		return frame{}, device.ErrDataCleanupPending
	}
	var response frame
	select {
	case <-ctx.Done():
		e.fail()
		return frame{}, errors.Join(device.ErrDataCleanupPending, ctx.Err())
	case <-e.done:
		select {
		case response = <-e.replies:
		default:
			return frame{}, device.ErrDataCleanupPending
		}
	case response = <-e.replies:
	}
	if response.Op != f.Op || f.Token != "" && (response.Token != f.Token || response.ID != f.ID) {
		e.fail()
		return frame{}, device.ErrDataCleanupPending
	}
	return response, getError(response.Error)
}

func (c *Client) connect(ctx context.Context, f frame) (*entry, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unixpacket", c.socket)
	if err != nil {
		return nil, device.ErrDataBackendUnavailable
	}
	conn, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return nil, device.ErrDataBackendUnavailable
	}
	peer, err := credentials(conn)
	if err != nil || peer == nil || int(peer.Uid) != os.Geteuid() {
		_ = conn.Close()
		return nil, device.ErrDataBackendUnavailable
	}
	directoryFD, err := unix.Open(filepath.Dir(c.database), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = conn.Close()
		return nil, device.ErrDataBackendUnavailable
	}
	directory := os.NewFile(uintptr(directoryFD), "data-owner-database-directory")
	defer directory.Close()
	info, err := os.Lstat(c.database)
	if err != nil || !info.Mode().IsRegular() {
		_ = conn.Close()
		return nil, device.ErrDataBackendUnavailable
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		_ = conn.Close()
		return nil, device.ErrDataBackendUnavailable
	}
	f.DatabaseDevice, f.DatabaseInode = uint64(st.Dev), st.Ino
	e := &entry{conn: conn, replies: make(chan frame, 1), done: make(chan struct{}), gate: make(chan struct{}, 1)}
	go e.read()
	response, err := e.call(ctx, f, unix.UnixRights(int(c.instance.Fd()), int(directory.Fd())))
	if err != nil {
		e.fail()
		return nil, err
	}
	if f.Op == "open" {
		if len(response.Token) != 64 {
			e.fail()
			return nil, device.ErrDataCleanupPending
		}
		e.token = response.Token
		e.alive.Store(true)
		select {
		case <-e.done:
			e.alive.Store(false)
			return nil, device.ErrDataCleanupPending
		default:
		}
	}
	return e, nil
}

func (c *Client) Ping(ctx context.Context) error {
	e, err := c.connect(ctx, frame{Op: "ping"})
	if e != nil {
		e.fail()
	}
	return err
}
func (c *Client) probe(ctx context.Context, id string) error {
	e, err := c.connect(ctx, frame{Op: "idle", ID: id})
	if e != nil {
		e.fail()
	}
	return err
}

func (c *Client) Owns(candidate modem.Candidate) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[candidate.ID]
	return e != nil && e.alive.Load() && device.SameDataPhysical(e.candidate, candidate)
}

func (c *Client) Admitted(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[id] != nil
}

func (c *Client) Set(ctx context.Context, candidate modem.Candidate, iccid, imsi string, request device.NetworkRequest) (device.NetworkResult, error) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return device.NetworkResult{}, device.ErrDataCleanupPending
	}
	e := c.entries[candidate.ID]
	if e != nil && (!device.SameDataPhysical(e.candidate, candidate) || e.iccid != iccid || e.imsi != imsi) {
		c.mu.Unlock()
		return device.NetworkResult{}, device.ErrSIMObservation
	}
	first := e == nil
	if first {
		e = &entry{candidate: candidate, iccid: iccid, imsi: imsi, ready: make(chan struct{})}
		c.entries[candidate.ID] = e
	}
	c.mu.Unlock()
	if first {
		wire, err := c.connect(ctx, frame{Op: "open", ID: candidate.ID, Open: &Open{Candidate: candidate, Generation: candidate.USBGeneration, ICCID: iccid, IMSI: imsi}})
		c.mu.Lock()
		if c.closing || c.entries[candidate.ID] != e {
			if wire != nil {
				wire.fail()
			}
			close(e.ready)
			c.mu.Unlock()
			return device.NetworkResult{}, device.ErrDataCleanupPending
		}
		if err == nil {
			// Keep one cancellation owner: all later calls use wire itself.
			wire.candidate = candidate
			wire.iccid = iccid
			wire.imsi = imsi
			wire.ready = e.ready
			c.entries[candidate.ID] = wire
			close(e.ready)
			e = wire
		} else {
			delete(c.entries, candidate.ID)
			close(e.ready)
		}
		c.mu.Unlock()
		if err != nil {
			return device.NetworkResult{}, err
		}
	} else {
		select {
		case <-ctx.Done():
			return device.NetworkResult{}, ctx.Err()
		case <-e.ready:
		}
		c.mu.Lock()
		e = c.entries[candidate.ID]
		c.mu.Unlock()
		if e == nil {
			return device.NetworkResult{}, device.ErrDataCleanupPending
		}
	}
	response, err := e.call(ctx, frame{Op: "set", ID: candidate.ID, Token: e.token, Request: &request}, nil)
	if err != nil {
		return device.NetworkResult{}, err
	}
	if response.Result == nil || response.Result.Enabled != request.Enabled || response.Result.Backend != "qmi" || response.Result.Interface != candidate.NetworkInterface || response.Result.ControlDevice != candidate.QMIControl {
		e.fail()
		return device.NetworkResult{}, device.ErrDataCleanupPending
	}
	return *response.Result, nil
}

func (c *Client) current(ctx context.Context, id string) (*entry, error) {
	c.mu.Lock()
	e := c.entries[id]
	c.mu.Unlock()
	if e == nil {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.ready:
	}
	c.mu.Lock()
	e = c.entries[id]
	c.mu.Unlock()
	return e, nil
}
func (c *Client) Stop(ctx context.Context, id string) error {
	e, err := c.current(ctx, id)
	if err != nil {
		return err
	}
	if e == nil || !e.alive.Load() {
		if e != nil {
			e.fail()
			c.mu.Lock()
			if c.entries[id] == e {
				delete(c.entries, id)
			}
			c.mu.Unlock()
		}
		return c.probe(ctx, id)
	}
	_, err = e.call(ctx, frame{Op: "stop", ID: id, Token: e.token}, nil)
	if err != nil {
		return err
	}
	e.fail()
	c.mu.Lock()
	if c.entries[id] == e {
		delete(c.entries, id)
	}
	c.mu.Unlock()
	return nil
}
func (c *Client) Status(ctx context.Context, id string) (device.NetworkStatus, error) {
	e, err := c.current(ctx, id)
	if err != nil {
		return device.NetworkStatus{}, err
	}
	if e == nil {
		return device.NetworkStatus{Backend: "qmi", Detail: "no delegated original data session"}, c.probe(ctx, id)
	}
	response, err := e.call(ctx, frame{Op: "status", ID: id, Token: e.token}, nil)
	if err != nil {
		return device.NetworkStatus{}, err
	}
	if response.Status == nil || response.Status.Backend != "qmi" || response.Status.Interface != e.candidate.NetworkInterface {
		e.fail()
		return device.NetworkStatus{}, device.ErrDataCleanupPending
	}
	return *response.Status, nil
}
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closing = true
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	results := make(chan error, len(ids))
	for _, id := range ids {
		go func(id string) { results <- c.Stop(ctx, id) }(id)
	}
	var all []error
	for range ids {
		select {
		case err := <-results:
			if err != nil {
				all = append(all, err)
			}
		case <-ctx.Done():
			all = append(all, ctx.Err())
		}
	}
	c.mu.Lock()
	for id, e := range c.entries {
		if e.done != nil {
			e.fail()
		}
		delete(c.entries, id)
	}
	c.mu.Unlock()
	return errors.Join(all...)
}
