//go:build linux

package dataowner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"vocat/internal/device"
)

type Driver interface {
	Set(context.Context, device.NetworkRequest) (device.NetworkResult, error)
	Status(context.Context) (device.NetworkStatus, error)
	Stop(context.Context) error
	Close(context.Context) error // closes only this driver's original capability
}
type ServerOptions struct {
	SocketPath, InstanceLockPath, DatabasePath string
	Factory                                    func(context.Context, Open) (Driver, error)
	Idle                                       func(context.Context, string) error
}
type Server struct {
	opts     ServerOptions
	listener *net.UnixListener
	guard    *os.File
	mu       sync.Mutex
	peers    map[*peer]bool
	active   map[string]*peer
	closing  bool
	group    sync.WaitGroup
}
type peer struct {
	conn   *net.UnixConn
	cancel context.CancelFunc
	id     string
}

func Listen(opts ServerOptions) (*Server, error) {
	if !validPath(opts.SocketPath) || !validPath(opts.InstanceLockPath) || !validPath(opts.DatabasePath) || opts.Factory == nil || opts.Idle == nil {
		return nil, device.ErrDataBackendUnavailable
	}
	dir := filepath.Dir(opts.SocketPath)
	if err := os.MkdirAll(dir, 0700); err != nil || !privateDirectory(dir) {
		return nil, device.ErrDataBackendUnavailable
	}
	fd, err := unix.Open(filepath.Join(dir, "vocat-data-owner.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	guard := os.NewFile(uintptr(fd), "data-owner-singleton")
	if !sameFile(guard, filepath.Join(dir, "vocat-data-owner.lock")) || unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = guard.Close()
		return nil, device.ErrDataCleanupPending
	}
	if info, err := os.Lstat(opts.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			_ = guard.Close()
			return nil, device.ErrDataBackendUnavailable
		}
		if err := os.Remove(opts.SocketPath); err != nil {
			_ = guard.Close()
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = guard.Close()
		return nil, err
	}
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: opts.SocketPath, Net: "unixpacket"})
	if err != nil {
		_ = guard.Close()
		return nil, err
	}
	if err := os.Chmod(opts.SocketPath, 0600); err != nil {
		_ = listener.Close()
		_ = guard.Close()
		return nil, err
	}
	return &Server{opts: opts, listener: listener, guard: guard, peers: make(map[*peer]bool), active: make(map[string]*peer)}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = s.listener.Close() })
	defer stop()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		peerCtx, cancel := context.WithCancel(context.Background())
		p := &peer{conn: conn, cancel: cancel}
		s.mu.Lock()
		if s.closing || len(s.peers) >= 128 {
			s.mu.Unlock()
			cancel()
			_ = conn.Close()
			continue
		}
		s.peers[p] = true
		s.group.Add(1)
		s.mu.Unlock()
		go func() { defer s.group.Done(); s.handle(peerCtx, p) }()
	}
}

func (s *Server) remove(p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, p)
	if s.active[p.id] == p {
		delete(s.active, p.id)
	}
}

func (s *Server) handle(ctx context.Context, p *peer) {
	defer p.cancel()
	defer p.conn.Close()
	defer s.remove(p)
	_ = p.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, files, err := readFrame(p.conn)
	if err != nil {
		return
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	cred, err := credentials(p.conn)
	if err != nil || cred == nil || cred.Pid <= 0 || int(cred.Uid) != os.Geteuid() || len(files) != 2 {
		return
	}
	selfNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return
	}
	peerNS, err := os.Readlink("/proc/" + strconv.Itoa(int(cred.Pid)) + "/ns/net")
	if err != nil || peerNS != selfNS || !sameDatabaseDirectory(files[1], s.opts.DatabasePath, first.DatabaseDevice, first.DatabaseInode) || !verifyInstanceLock(files[0], s.opts.InstanceLockPath) {
		return
	}
	if first.Op == "ping" {
		_ = writeFrame(p.conn, frame{Op: "ping"}, nil)
		return
	}
	if first.ID == "" || len(first.ID) > 1024 {
		return
	}
	s.mu.Lock()
	busy := s.active[first.ID] != nil || s.closing
	if first.Op == "open" && !busy {
		p.id = first.ID
		s.active[p.id] = p
	}
	s.mu.Unlock()
	if first.Op == "idle" {
		response := frame{Op: "idle"}
		if busy {
			response.Error = "pending"
		} else if err := s.opts.Idle(ctx, first.ID); err != nil {
			response.Error = setError(err)
		}
		_ = writeFrame(p.conn, response, nil)
		return
	}
	if first.Op != "open" || first.Open == nil || first.Open.Candidate.ID != first.ID || busy {
		_ = writeFrame(p.conn, frame{Op: "open", Error: "pending"}, nil)
		return
	}
	_ = p.conn.SetReadDeadline(time.Time{})
	// Continuously receive even during Start. EOF cancels that operation and
	// cleanup below joins it while retaining the original transport and flock FD.
	requests := make(chan frame, 1)
	go func() {
		defer p.cancel()
		defer close(requests)
		for {
			f, extra, err := readFrame(p.conn)
			for _, file := range extra {
				_ = file.Close()
			}
			if err != nil || len(extra) != 0 {
				return
			}
			select {
			case requests <- f:
			case <-ctx.Done():
				return
			default:
				return // pipelining must not prevent EOF cancellation
			}
		}
	}()
	prepare, stopPrepare := context.WithTimeout(ctx, 10*time.Second)
	driver, err := s.opts.Factory(prepare, *first.Open)
	stopPrepare()
	if err != nil || driver == nil {
		_ = writeFrame(p.conn, frame{Op: "open", Error: setError(err)}, nil)
		return
	}
	defer func() {
		for {
			cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			err := driver.Close(cleanup)
			cancel()
			if err == nil {
				break
			}
			// No expiry, descriptor transfer or fresh-client stop. Server shutdown
			// may time out, but the durable journal still prevents takeover.
			time.Sleep(time.Second)
		}
	}()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return
	}
	token := hex.EncodeToString(nonce[:])
	if writeFrame(p.conn, frame{Op: "open", Token: token}, nil) != nil {
		return
	}
	for {
		var f frame
		select {
		case <-ctx.Done():
			return
		case next, ok := <-requests:
			if !ok {
				return
			}
			f = next
		}
		if f.Token != token || f.ID != p.id {
			return
		}
		response := frame{Op: f.Op, Token: token, ID: p.id}
		op, cancel := context.WithTimeout(ctx, 100*time.Second)
		switch f.Op {
		case "set":
			if f.Request == nil || !f.Request.Enabled {
				cancel()
				return
			}
			result, err := driver.Set(op, *f.Request)
			response.Result = &result
			if err != nil {
				response.Error = setError(err)
			}
		case "status":
			status, err := driver.Status(op)
			response.Status = &status
			if err != nil {
				response.Error = setError(err)
			}
		case "stop":
			err := driver.Stop(op)
			if err != nil {
				response.Error = setError(err)
			}
			if err == nil {
				err = driver.Close(op)
				if err != nil {
					response.Error = setError(err)
				}
			}
			if err == nil {
				s.remove(p)
				_ = writeFrame(p.conn, response, nil)
				cancel()
				return
			}
		default:
			cancel()
			return
		}
		cancel()
		if writeFrame(p.conn, response, nil) != nil {
			return
		}
	}
}

func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	for p := range s.peers {
		p.cancel()
		_ = p.conn.Close()
	}
	s.mu.Unlock()
	_ = s.listener.Close()
	done := make(chan struct{})
	go func() { s.group.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return device.ErrDataCleanupPending
	case <-done:
		s.mu.Lock()
		guard := s.guard
		s.guard = nil
		s.mu.Unlock()
		if guard != nil {
			return guard.Close()
		}
		return nil
	}
}
