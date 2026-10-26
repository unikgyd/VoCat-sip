package sipgateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func eventSDP(port int, pt int, events string) []byte {
	return []byte(fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 8 %d\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:%d telephone-event/8000\r\na=fmtp:%d %s\r\na=sendrecv\r\n", port, pt, pt, pt, events))
}
func eventPacket(pt byte, event byte, sequence uint16, timestamp uint32, ended bool) []byte {
	p := []byte{0x80, pt, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, event, 10, 5, 0}
	if ended {
		p[13] |= 0x80
	}
	binary.BigEndian.PutUint16(p[2:4], sequence)
	binary.BigEndian.PutUint32(p[4:8], timestamp)
	return p
}
func noEventPacket(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Millisecond))
	buffer := make([]byte, 2048)
	if _, _, err := conn.ReadFromUDP(buffer); err == nil {
		t.Fatal("unnegotiated/duplicate/unowned event escaped")
	}
}
func readEventPacket(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buffer[:n]...)
}
func TestSIPDTMFSDPUsesOnlySelectedAudioAndValidEventSet(t *testing.T) {
	for _, test := range []struct {
		suffix string
		mask   uint16
	}{
		{"", 0xffff},
		{"m=video 9999 RTP/AVP 110\r\na=rtpmap:110 telephone-event/8000\r\na=fmtp:110 1\r\n", 0xffff},
		{"a=fmtp:110 1\r\n", 0},
	} {
		desc, err := parseCallSDP(append(eventSDP(9000, 110, "0-15"), []byte(test.suffix)...))
		if err != nil || desc.events.Mask != test.mask {
			t.Fatal(desc, err)
		}
	}
	for _, events := range []string{"15-0", "0-99999", "0-15;bad=1", "+1"} {
		desc, err := parseCallSDP(eventSDP(9000, 110, events))
		if err != nil || desc.format.codec != "PCMA" || desc.events.Mask != 0 {
			t.Fatal("optional invalid events broke audio or were enabled", desc, err)
		}
	}
	desc, err := parseCallSDP(eventSDP(9000, 110, "0-9,11"))
	if err != nil {
		t.Fatal(err)
	}
	answer := makeCallAnswer(desc, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9001})
	if !bytes.Contains(answer, []byte("RTP/AVP 8 110\r\n")) || !bytes.Contains(answer, []byte("a=fmtp:110 0,1,2,3,4,5,6,7,8,9,11\r\n")) {
		t.Fatal(string(answer))
	}
}

func TestSIPDTMFInternalWireRemapsBothDirectionsAndPreservesReports(t *testing.T) {
	r := testCallRuntime(t)
	callee := registerMessagePeer(t, r, "1002")
	caller, cm, invite := prepareCaller(t, r, "1002")
	invite.SetBody(eventSDP(cm.LocalAddr().(*net.UDPAddr).Port, 110, "0-9,11"))
	writeCall(t, caller, invite)
	out := readCallRequest(t, callee, sip.INVITE)
	if !strings.Contains(string(out.Body()), "telephone-event/8000") {
		t.Fatal("gateway did not offer events")
	}
	dm := testMediaSocket(t)
	remote := calleeResponse(t, callee, out, dm, 200)
	remote.SetBody(eventSDP(dm.LocalAddr().(*net.UDPAddr).Port, 101, "5-12"))
	writeCall(t, callee, remote)
	readCallRequest(t, callee, sip.ACK)
	answer := readCallFinal(t, caller, 200)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	left, err := parseCallSDP(answer.Body())
	if err != nil {
		t.Fatal(err)
	}
	right, err := parseCallSDP(out.Body())
	if err != nil {
		t.Fatal(err)
	}
	if left.events.Payload != 110 || left.events.Mask != 0x0be0 {
		t.Fatal("answer did not intersect both legs", left.events)
	}
	leftAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: left.port}
	rightAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: right.port}
	for i := 0; i < 3; i++ {
		packet := eventPacket(110, 7, uint16(20+i), 12345, true)
		if _, err = cm.WriteToUDP(packet, leftAddr); err != nil {
			t.Fatal(err)
		}
		got := readEventPacket(t, dm)
		packet[1] = 101
		if !bytes.Equal(got, packet) {
			t.Fatal("RTP event headers or redundancy changed", got, packet)
		}
	}
	packet := eventPacket(101, 11, 99, 54321, true)
	dm.WriteToUDP(packet, rightAddr)
	got := readEventPacket(t, cm)
	packet[1] = 110
	if !bytes.Equal(got, packet) {
		t.Fatal("reverse event payload number was not remapped", got)
	}
	dm.WriteToUDP(eventPacket(101, 10, 100, 54322, true), rightAddr)
	noEventPacket(t, cm)
	wrong := testMediaSocket(t)
	wrong.WriteToUDP(eventPacket(110, 7, 100, 54323, true), leftAddr)
	noEventPacket(t, dm)
	cm.WriteToUDP(eventPacket(110, 8, 101, 54324, true), leftAddr)
	readEventPacket(t, dm) // Rejected event did not capture the RTP latch.
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	replyCallOK(t, callee, readCallRequest(t, callee, sip.BYE))
	waitCalls(t, r, 0)
}

func TestSIPDTMFSenderSharesPCMSequenceAndCancelsOnClose(t *testing.T) {
	peer := testMediaSocket(t)
	media, err := newCallMedia(peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer media.close()
	desc, _ := parseCallSDP(eventSDP(peer.LocalAddr().(*net.UDPAddr).Port, 110, "0-15"))
	if err = media.configure(peer.LocalAddr().String(), desc); err != nil {
		t.Fatal(err)
	}
	pcm := &syntheticCarrierMedia{down: make(chan []int16, 20), up: make(chan []int16, 1)}
	bridge, err := newCarrierPCMBridge(context.Background(), media, pcm)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	bridge.start()
	pcm.down <- make([]int16, 160)
	first := readEventPacket(t, peer)
	done := make(chan error, 1)
	go func() { done <- bridge.sendDTMF(context.Background(), '#') }()
	firstEvent := readEventPacket(t, peer)
	for i := 0; i < 8; i++ {
		pcm.down <- make([]int16, 160)
	}
	packets := [][]byte{firstEvent}
	for i := 1; i < 10; i++ {
		packets = append(packets, readEventPacket(t, peer))
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	for i, p := range packets {
		if len(p) != 16 || p[1]&127 != 110 || p[12] != 11 || (p[13]&128 != 0) != (i >= 7) || binary.BigEndian.Uint16(p[2:4]) != binary.BigEndian.Uint16(first[2:4])+uint16(i+1) || !bytes.Equal(p[8:12], first[8:12]) || !bytes.Equal(p[4:8], firstEvent[4:8]) || binary.BigEndian.Uint16(p[14:16]) != uint16(min(i+1, 8)*160) {
			t.Fatal("event did not share the RTP sender", i, p)
		}
	}
	pcm.down <- make([]int16, 160)
	last := readEventPacket(t, peer)
	if last[1]&127 != 8 || binary.BigEndian.Uint16(last[2:4]) != binary.BigEndian.Uint16(first[2:4])+11 || binary.BigEndian.Uint32(last[4:8])-binary.BigEndian.Uint32(firstEvent[4:8]) < 1280 {
		t.Fatal("audio resumed with stale timestamp/sequence", last)
	}
	go func() { done <- bridge.sendDTMF(context.Background(), '1') }()
	readEventPacket(t, peer)
	bridge.close()
	if err = <-done; err == nil {
		t.Fatal("closed media finished an event")
	}
	noEventPacket(t, peer)
}

func TestSIPDTMFSenderDoesNotMigrateAnInFlightKeyToNewMedia(t *testing.T) {
	peer, next := testMediaSocket(t), testMediaSocket(t)
	media, err := newCallMedia(peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer media.close()
	desc, _ := parseCallSDP(eventSDP(peer.LocalAddr().(*net.UDPAddr).Port, 110, "0-15"))
	media.configure(peer.LocalAddr().String(), desc)
	pcm := &syntheticCarrierMedia{down: make(chan []int16), up: make(chan []int16)}
	bridge, err := newCarrierPCMBridge(context.Background(), media, pcm)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	done := make(chan error, 1)
	go func() { done <- bridge.sendDTMF(context.Background(), '1') }()
	readEventPacket(t, peer)
	desc.port = next.LocalAddr().(*net.UDPAddr).Port
	media.configure(next.LocalAddr().String(), desc)
	if err = <-done; err == nil {
		t.Fatal("changed source accepted an in-flight event")
	}
	noEventPacket(t, next)
}
