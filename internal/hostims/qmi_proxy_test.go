package hostims

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func readTestProxyRequest(reader io.Reader) (*qmi.Packet, error) {
	header := make([]byte, 6)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	size := int(binary.LittleEndian.Uint16(header[1:3])) + 1
	if size < 12 || size > 32768 {
		return nil, ErrParameters
	}
	frame := make([]byte, size)
	copy(frame, header)
	if _, err := io.ReadFull(reader, frame[6:]); err != nil {
		return nil, err
	}
	if frame[6] != 0 {
		return nil, ErrParameters
	}
	return qmi.UnmarshalPacket(frame)
}
func proxyResponse(request *qmi.Packet, tlvs []qmi.TLV) []byte {
	frame, err := encodeProxyRequest(request.ServiceType, request.ClientID, request.MessageID, request.TransactionID, tlvs)
	if err != nil {
		panic("invalid synthetic proxy frame")
	}
	frame[3] = 0x80
	frame[6] = 2
	if request.ServiceType == qmi.ServiceControl {
		frame[6] = 1
	}
	return frame
}
func proxyTestPair(t *testing.T) (*proxyQMI, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	link := newProxyQMI(client)
	t.Cleanup(func() { _ = server.Close(); _ = link.Close() })
	return link, server
}
func TestQMIProxyStrictRoundTripAndBoundedClose(t *testing.T) {
	link, server := proxyTestPair(t)
	finished := make(chan error, 1)
	go func() {
		request, err := readTestProxyRequest(server)
		if err != nil {
			finished <- err
			return
		}
		_, err = server.Write(proxyResponse(request, []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}, {Type: 0x10, Value: []byte{2, 0, 0x90, 0}}}))
		finished <- err
	}()
	packet, err := link.SendRequest(context.Background(), qmi.ServiceUIM, 7, 0x003b, []qmi.TLV{{Type: 1, Value: []byte{1}}})
	if err != nil || packet.ServiceType != qmi.ServiceUIM || packet.ClientID != 7 || packet.MessageID != 0x003b || packet.TransactionID != 1 {
		t.Fatal("proxy response mismatch", err)
	}
	clearProxyPacket(packet)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := link.Close(); err != nil {
		t.Fatal(err)
	}
	if link.healthy() {
		t.Fatal("closed connection still healthy")
	}
}
func TestQMIProxyRejectsMismatchedAndMalformedWireReplies(t *testing.T) {
	for _, mode := range []string{"cid", "service", "transaction", "message", "request-flags", "trailer", "duplicate", "size", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			link, server := proxyTestPair(t)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				request, err := readTestProxyRequest(server)
				if err != nil {
					return
				}
				raw := proxyResponse(request, []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}})
				switch mode {
				case "cid":
					raw[5]++
				case "service":
					raw[4]++
				case "transaction":
					raw[7]++
				case "message":
					raw[9]++
				case "request-flags":
					raw[6] = 0
				case "trailer":
					raw = append(raw, 0)
					binary.LittleEndian.PutUint16(raw[1:3], uint16(len(raw)-1))
				case "duplicate":
					raw = append(raw, 2, 4, 0, 0, 0, 0, 0)
					binary.LittleEndian.PutUint16(raw[1:3], uint16(len(raw)-1))
					binary.LittleEndian.PutUint16(raw[11:13], 14)
				case "size":
					raw[1] = 0xff
					raw[2] = 0xff
				case "truncated":
					raw = raw[:8]
				}
				_, _ = server.Write(raw)
				_ = server.Close()
			}()
			packet, err := link.SendRequest(context.Background(), qmi.ServiceUIM, 7, 0x002f, nil)
			if err == nil || packet != nil || link.healthy() {
				t.Fatal("invalid response accepted")
			}
			<-finished
			if _, err := link.SendRequest(context.Background(), qmi.ServiceUIM, 7, 0x002f, nil); err == nil {
				t.Fatal("broken transport reused")
			}
		})
	}
}

type notifyingProxyConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *notifyingProxyConn) Write(raw []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(raw)
}
func TestQMIProxyCancelDuringReadAndBlockedWriteDoesNotReplay(t *testing.T) {
	for _, mode := range []string{"read", "write"} {
		t.Run(mode, func(t *testing.T) {
			link, server := proxyTestPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			entered := make(chan struct{})
			done := make(chan error, 1)
			if mode == "read" {
				go func() { _, _ = readTestProxyRequest(server); close(entered) }()
			} else {
				client, blockedServer := net.Pipe()
				link = newProxyQMI(&notifyingProxyConn{Conn: client, entered: entered})
				t.Cleanup(func() { _ = blockedServer.Close(); _ = link.Close() })
			}
			go func() { _, err := link.SendRequest(ctx, qmi.ServiceUIM, 7, 0x003b, nil); done <- err }()
			<-entered
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled I/O succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation stuck in I/O")
			}
			if link.healthy() {
				t.Fatal("partly issued request left reusable transport")
			}
		})
	}
}
func TestQMIProxyInvalidationWhileIdlePreventsFurtherIOMutations(t *testing.T) {
	link, server := proxyTestPair(t)
	go func() {
		request, err := readTestProxyRequest(server)
		if err != nil {
			return
		}
		_, _ = server.Write(proxyResponse(request, []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}))
		raw := proxyResponse(&qmi.Packet{ServiceType: qmi.ServiceControl, MessageID: 0x24}, []qmi.TLV{{Type: 1, Value: []byte{qmi.ServiceUIM, 7}}})
		raw[6] = 2
		_, _ = server.Write(raw)
	}()
	if _, err := link.SendRequest(context.Background(), qmi.ServiceControl, 0, 0xff00, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-link.readDone:
	case <-time.After(time.Second):
		t.Fatal("idle CID revoke not observed")
	}
	if _, err := link.SendRequest(context.Background(), qmi.ServiceUIM, 7, 0x003b, nil); err == nil {
		t.Fatal("AUTH allowed after revoke")
	}
}
func TestQMIProxyAlreadyCancelledRequestSendsNothing(t *testing.T) {
	link, _ := proxyTestPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := link.SendRequest(ctx, qmi.ServiceUIM, 7, 0x003b, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled request accepted", err)
	}
	if !link.healthy() {
		t.Fatal("unissued cancellation destroyed healthy transport")
	}
}

func TestQMIProxyCancellationErasesQueuedAuthenticationReply(t *testing.T) {
	for i := 0; i < 20; i++ {
		link, server := proxyTestPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() { _, _ = readTestProxyRequest(server); close(entered) }()
		go func() { _, err := link.SendRequest(ctx, qmi.ServiceUIM, 7, 0x003b, nil); done <- err }()
		<-entered
		secret := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		link.mu.Lock()
		pending := link.pending
		if pending == nil {
			link.mu.Unlock()
			t.Fatal("request not pending")
		}
		cancel()
		pending.reply <- proxyReply{packet: &qmi.Packet{TLVs: []qmi.TLV{{Type: 0x10, Value: secret}}}}
		link.mu.Unlock()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled reply exported")
			}
		case <-time.After(time.Second):
			t.Fatal("reply cleanup stuck")
		}
		for _, b := range secret {
			if b != 0 {
				t.Fatal("abandoned AKA reply remained buffered")
			}
		}
		_ = server.Close()
		_ = link.Close()
	}
}
func TestQMIProxyTransactionIDsNeverWrapWithinConnection(t *testing.T) {
	for _, ctl := range []bool{false, true} {
		link, _ := proxyTestPair(t)
		link.mu.Lock()
		if ctl {
			link.ctl = 255
		} else {
			link.service = 65535
		}
		link.mu.Unlock()
		service := byte(qmi.ServiceUIM)
		if ctl {
			service = qmi.ServiceControl
		}
		if _, err := link.SendRequest(context.Background(), service, 0, 0x22, nil); err == nil || link.healthy() {
			t.Fatal("transaction counter wrapped on live connection")
		}
	}
}
func FuzzQMIProxyFrameDecoder(f *testing.F) {
	f.Add(proxyResponse(&qmi.Packet{ServiceType: qmi.ServiceControl, MessageID: 0x22, TransactionID: 1}, []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}, {Type: 1, Value: []byte{qmi.ServiceUIM, 7}}}))
	f.Add([]byte{1, 0, 0})
	f.Fuzz(func(t *testing.T, raw []byte) { packet, _ := decodeProxyPacket(raw); clearProxyPacket(packet) })
}
