package hostims

import (
	"context"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"testing"
	"time"
)

func ownedCloseNotice() *qmi.Packet {
	return &qmi.Packet{ServiceType: qmi.ServiceUIM, ClientID: 7, MessageID: 0x43, IsIndication: true, TLVs: []qmi.TLV{{Type: 1, Value: []byte{1}}, {Type: 0x11, Value: []byte{4}}, {Type: 0x13, Value: []byte{1, 0, 0, 0}}}}
}

func TestUIMOwnedCloseRequiresExactFreshSingleUseNotice(t *testing.T) {
	for name, mutate := range map[string]func(*qmi.Packet){
		"cid": func(p *qmi.Packet) { p.ClientID++ }, "slot": func(p *qmi.Packet) { p.TLVs[0].Value[0] = 2 }, "channel": func(p *qmi.Packet) { p.TLVs[1].Value[0] = 3 },
		"basic": func(p *qmi.Packet) { p.TLVs[1].Value[0] = 0 }, "status": func(p *qmi.Packet) { p.MessageID = 0x32 }, "refresh": func(p *qmi.Packet) { p.MessageID = 0x33 }, "slots": func(p *qmi.Packet) { p.MessageID = 0x48 },
		"unknown": func(p *qmi.Packet) { p.MessageID = 0x99 }, "response": func(p *qmi.Packet) { p.IsIndication = false }, "no-channel": func(p *qmi.Packet) { p.TLVs = p.TLVs[:1] },
		"extension-size": func(p *qmi.Packet) { p.TLVs[2].Value = []byte{1} }, "extension-value": func(p *qmi.Packet) { p.TLVs[2].Value[0] = 2 },
		"duplicate": func(p *qmi.Packet) { p.TLVs = append(p.TLVs, p.TLVs[0]) }, "unrecognized-tlv": func(p *qmi.Packet) { p.TLVs = append(p.TLVs, qmi.TLV{Type: 0x12, Value: []byte{0}}) },
		"aid": func(p *qmi.Packet) {
			p.TLVs = append(p.TLVs, qmi.TLV{Type: 0x10, Value: append([]byte{7}, isimPrefix...)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			link, _ := proxyTestPair(t)
			if err := link.expectOwnedUIMClose(7, 1, 4, usimPrefix); err != nil {
				t.Fatal(err)
			}
			bad := ownedCloseNotice()
			mutate(bad)
			if link.consumeOwnedUIMClose(bad) {
				t.Fatal("foreign or malformed close accepted")
			}
			if !link.consumeOwnedUIMClose(ownedCloseNotice()) || link.consumeOwnedUIMClose(ownedCloseNotice()) {
				t.Fatal("expected close lost or replay accepted")
			}
		})
	}
	link, _ := proxyTestPair(t)
	if link.consumeOwnedUIMClose(ownedCloseNotice()) {
		t.Fatal("unsolicited close accepted")
	}
	if err := link.expectOwnedUIMClose(7, 1, 4, usimPrefix); err != nil {
		t.Fatal(err)
	}
	link.mu.Lock()
	link.uimCloses[0].expires = time.Now().Add(-time.Second)
	link.mu.Unlock()
	if link.consumeOwnedUIMClose(ownedCloseNotice()) {
		t.Fatal("expired expectation accepted")
	}
}

func TestUIMOwnedCloseNoticeDoesNotReplaceMethodACK(t *testing.T) {
	for _, reply := range []bool{true, false} {
		link, server := proxyTestPair(t)
		if err := link.expectOwnedUIMClose(7, 1, 4, usimPrefix); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			request, err := readTestProxyRequest(server)
			if err != nil {
				return
			}
			notice := ownedCloseNotice()
			frame := proxyResponse(notice, notice.TLVs)
			frame[6] = 4
			if _, err := server.Write(frame); err != nil {
				return
			}
			if reply {
				_, _ = server.Write(proxyResponse(request, []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		packet, err := link.SendRequest(ctx, qmi.ServiceUIM, 7, 0x3f, nil)
		cancel()
		<-done
		if reply {
			if err != nil || packet == nil || !link.healthy() {
				t.Fatal("own close poisoned acknowledged reply", err)
			}
		} else if err == nil || packet != nil || link.healthy() {
			t.Fatal("notification substituted for missing ACK")
		}
		clearProxyPacket(packet)
	}
}
