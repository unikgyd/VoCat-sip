//go:build linux

package exportproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type dnsSocketDial func(context.Context, string, string) (net.Conn, error)
type dnsReply struct {
	packet []byte
	soft   bool
	err    error
}

// A UDP connect does not establish that a DNS server answers. Race the actual
// query across the bounded carrier-provided list, including both IP families.
// Every socket uses the original marked/interface-bound dialer. Close cancels
// and joins all workers before the enclosing cellular transfer releases its lease.
// This private connection carries one net.Resolver exchange, not general traffic.
type dnsRaceConn struct {
	ctx        context.Context
	cancel     context.CancelFunc
	network    string
	servers    []string
	dial       dnsSocketDial
	mu         sync.Mutex
	started    bool
	closed     bool
	readUntil  time.Time
	writeUntil time.Time
	changed    chan struct{}
	active     map[net.Conn]struct{}
	workers    sync.WaitGroup
	closeOnce  sync.Once
	readMu     sync.Mutex
	replies    chan dnsReply
	remaining  int
	buffer     []byte
}

// PacketConn's presence tells net.Resolver to use DNS datagrams rather than
// length-prefixed TCP messages. TCP deliberately returns only dnsRaceConn.
type dnsPacketRaceConn struct{ *dnsRaceConn }

func newDNSRaceConn(ctx context.Context, network string, servers []string, dial dnsSocketDial) (net.Conn, error) {
	if dial == nil || network != "udp" && network != "tcp" && network != "udp4" && network != "udp6" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("unsupported cellular DNS transport")
	}
	selected := make([]string, 0, 8)
	seen := make(map[string]bool)
	for _, server := range servers {
		host, port, err := net.SplitHostPort(server)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || port == "" || seen[server] || strings.HasSuffix(network, "4") && ip.To4() == nil || strings.HasSuffix(network, "6") && ip.To4() != nil {
			continue
		}
		seen[server] = true
		selected = append(selected, server)
		if len(selected) == 8 {
			break
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("no cellular DNS servers")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	c := &dnsRaceConn{ctx: lifetime, cancel: cancel, network: network, servers: selected, dial: dial, changed: make(chan struct{}), active: make(map[net.Conn]struct{}), replies: make(chan dnsReply, len(selected))}
	if strings.HasPrefix(network, "udp") {
		return &dnsPacketRaceConn{c}, nil
	}
	return c, nil
}

func (c *dnsRaceConn) Write(packet []byte) (int, error) {
	query := packet
	if strings.HasPrefix(c.network, "tcp") {
		if len(packet) < 14 || int(binary.BigEndian.Uint16(packet)) != len(packet)-2 {
			return 0, errors.New("invalid DNS stream query")
		}
		query = packet[2:]
	}
	if len(query) < 12 || len(query) > 65535 {
		return 0, errors.New("invalid DNS query size")
	}
	c.mu.Lock()
	if c.closed || c.ctx.Err() != nil {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if c.started {
		c.mu.Unlock()
		return 0, errors.New("DNS exchange already submitted")
	}
	if !c.writeUntil.IsZero() && !time.Now().Before(c.writeUntil) {
		c.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	c.started = true
	c.remaining = len(c.servers)
	c.workers.Add(len(c.servers)) // Added before Close can begin waiting.
	c.mu.Unlock()
	wire := append([]byte(nil), packet...)
	request := append([]byte(nil), query...)
	for _, server := range c.servers {
		go c.exchange(server, wire, request)
	}
	return len(packet), nil
}

func (c *dnsRaceConn) exchange(server string, wire, query []byte) {
	defer c.workers.Done()
	packet, err := c.queryServer(server, wire, query)
	soft := false
	if err == nil {
		dns := packet
		if strings.HasPrefix(c.network, "tcp") {
			dns = dns[2:]
		}
		code := dns[3] & 15
		soft = code == 2 || code == 5 // SERVFAIL/REFUSED must not beat a usable answer.
	}
	c.replies <- dnsReply{packet: packet, soft: soft, err: err}
}

func (c *dnsRaceConn) queryServer(server string, wire, query []byte) ([]byte, error) {
	c.mu.Lock()
	deadline := c.readUntil
	if !c.writeUntil.IsZero() && (deadline.IsZero() || c.writeUntil.Before(deadline)) {
		deadline = c.writeUntil
	}
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(c.ctx)
	if !deadline.IsZero() {
		cancel()
		ctx, cancel = context.WithDeadline(c.ctx, deadline)
	}
	defer cancel()
	conn, err := c.dial(ctx, c.network, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(c.ctx, func() { _ = conn.Close() })
	defer stop()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	c.active[conn] = struct{}{}
	_ = conn.SetReadDeadline(c.readUntil)
	_ = conn.SetWriteDeadline(c.writeUntil)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.active, conn); c.mu.Unlock() }()
	for remaining := wire; len(remaining) != 0; {
		n, err := conn.Write(remaining)
		if err != nil {
			return nil, err
		}
		if n == 0 || strings.HasPrefix(c.network, "udp") && n != len(remaining) {
			return nil, io.ErrShortWrite
		}
		remaining = remaining[n:]
	}
	if strings.HasPrefix(c.network, "tcp") {
		header := make([]byte, 2)
		if _, err := io.ReadFull(conn, header); err != nil {
			return nil, err
		}
		length := int(binary.BigEndian.Uint16(header))
		if length < 12 {
			return nil, errors.New("invalid DNS stream response")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, err
		}
		if !dnsResponseMatches(query, body) {
			return nil, errors.New("mismatched DNS stream response")
		}
		return append(header, body...), nil
	}
	buffer := make([]byte, 65535)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			return nil, err
		}
		if dnsResponseMatches(query, buffer[:n]) {
			return append([]byte(nil), buffer[:n]...), nil
		}
	}
}

func (c *dnsRaceConn) Read(buffer []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(buffer) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	var rejected []byte
	var failures []error
	for len(c.buffer) == 0 {
		c.mu.Lock()
		closed, deadline, changed := c.closed, c.readUntil, c.changed
		started, remaining := c.started, c.remaining
		c.mu.Unlock()
		if closed {
			return 0, net.ErrClosed
		}
		if err := c.ctx.Err(); err != nil {
			return 0, err
		}
		if !started || remaining == 0 {
			if rejected != nil {
				c.buffer = rejected
				break
			}
			if len(failures) != 0 {
				return 0, errors.Join(failures...)
			}
			return 0, io.EOF
		}
		var timer *time.Timer
		var timedOut <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timedOut = timer.C
		}
		select {
		case reply := <-c.replies:
			c.mu.Lock()
			c.remaining--
			c.mu.Unlock()
			if reply.err != nil {
				failures = append(failures, reply.err)
			} else if reply.soft {
				if rejected == nil {
					rejected = reply.packet
				}
			} else {
				c.buffer = reply.packet
			}
		case <-c.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, c.ctx.Err()
		case <-timedOut:
			return 0, os.ErrDeadlineExceeded
		case <-changed:
		}
		if timer != nil {
			timer.Stop()
		}
	}
	n := copy(buffer, c.buffer)
	if strings.HasPrefix(c.network, "udp") {
		c.buffer = nil // UDP truncates a datagram; it does not split it into reads.
	} else {
		c.buffer = c.buffer[n:]
	}
	return n, nil
}

func (c *dnsRaceConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		close(c.changed)
		for conn := range c.active {
			_ = conn.Close()
		}
		c.mu.Unlock()
		c.cancel()
		c.workers.Wait()
	})
	return nil
}
func (c *dnsRaceConn) LocalAddr() net.Addr {
	if strings.HasPrefix(c.network, "tcp") {
		return &net.TCPAddr{}
	}
	return &net.UDPAddr{}
}
func (c *dnsRaceConn) RemoteAddr() net.Addr {
	if strings.HasPrefix(c.network, "tcp") {
		address, _ := net.ResolveTCPAddr(c.network, c.servers[0])
		return address
	}
	address, _ := net.ResolveUDPAddr(c.network, c.servers[0])
	return address
}
func (c *dnsRaceConn) SetDeadline(deadline time.Time) error {
	return c.setDeadline(deadline, true, true)
}
func (c *dnsRaceConn) SetReadDeadline(deadline time.Time) error {
	return c.setDeadline(deadline, true, false)
}
func (c *dnsRaceConn) SetWriteDeadline(deadline time.Time) error {
	return c.setDeadline(deadline, false, true)
}
func (c *dnsRaceConn) setDeadline(deadline time.Time, read, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if read {
		c.readUntil = deadline
		close(c.changed)
		c.changed = make(chan struct{})
	}
	if write {
		c.writeUntil = deadline
	}
	for conn := range c.active {
		if read {
			_ = conn.SetReadDeadline(deadline)
		}
		if write {
			_ = conn.SetWriteDeadline(deadline)
		}
	}
	return nil
}
func (c *dnsPacketRaceConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	n, err := c.Read(buffer)
	return n, c.RemoteAddr(), err
}
func (c *dnsPacketRaceConn) WriteTo(buffer []byte, address net.Addr) (int, error) {
	if address != nil && address.String() != c.RemoteAddr().String() {
		return 0, errors.New("foreign DNS destination")
	}
	return c.Write(buffer)
}

// Validate only the transaction/question needed to select a response. The Go
// resolver remains responsible for parsing DNS answers and truncation fallback.
func dnsResponseMatches(query, response []byte) bool {
	if len(query) < 12 || len(response) < 12 || !bytes.Equal(query[:2], response[:2]) || response[2]&0x80 == 0 || query[2]&0x78 != response[2]&0x78 || binary.BigEndian.Uint16(query[4:6]) != 1 || binary.BigEndian.Uint16(response[4:6]) != 1 {
		return false
	}
	qname, qend, qok := dnsQuestionName(query)
	rname, rend, rok := dnsQuestionName(response)
	return qok && rok && qname == rname && bytes.Equal(query[qend:qend+4], response[rend:rend+4])
}
func dnsQuestionName(packet []byte) (string, int, bool) {
	offset, end, total := 12, 0, 0
	name := make([]byte, 0, 64)
	for steps := 0; steps < 128; steps++ {
		if offset >= len(packet) {
			return "", 0, false
		}
		size := int(packet[offset])
		if size&0xc0 == 0xc0 {
			if offset+1 >= len(packet) {
				return "", 0, false
			}
			target := (size&0x3f)<<8 | int(packet[offset+1])
			if target >= offset {
				return "", 0, false
			}
			if end == 0 {
				end = offset + 2
			}
			offset = target
			continue
		}
		if size > 63 {
			return "", 0, false
		}
		offset++
		if size == 0 {
			if end == 0 {
				end = offset
			}
			if end+4 > len(packet) {
				return "", 0, false
			}
			return string(append(name, 0)), end, true
		}
		total += size + 1
		if offset+size > len(packet) || total > 254 {
			return "", 0, false
		}
		name = append(name, byte(size))
		for _, b := range packet[offset : offset+size] {
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			name = append(name, b)
		}
		offset += size
	}
	return "", 0, false
}
