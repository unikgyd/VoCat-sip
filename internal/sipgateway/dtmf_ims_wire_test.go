package sipgateway

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
)

// The registrar and AKA identity are synthetic. Both IMS/SIP dialogs and RTP
// endpoints are real production components on UDP loopback; no SIM or tunnel.
type dtmfWireAKA struct{}

func (dtmfWireAKA) CheckReady(context.Context, vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	return vowifi.AKAEvidence{Ready: true, Application: "USIM"}, nil
}
func (dtmfWireAKA) Authenticate(context.Context, vowifi.SIMIdentity, vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	return vowifi.AKAResult{}, errors.New("unexpected AKA in local no-auth registrar")
}

type dtmfWireTunnel struct{ address string }

func (t dtmfWireTunnel) Evidence() vowifi.TunnelEvidence {
	return vowifi.TunnelEvidence{Established: true, LocalIPv4: "127.0.0.1", PCSCF: []string{t.address}}
}
func (dtmfWireTunnel) Close(context.Context) error { return nil }

type dtmfIMSBackend struct{ session *ims.Session }

func (b dtmfIMSBackend) Ready(context.Context, string) error { return nil }
func (b dtmfIMSBackend) Calls(_ context.Context, id string) ([]vowifi.Call, error) {
	if id != "module-a" {
		return nil, ErrCarrierUnavailable
	}
	return b.session.Calls(), nil
}
func (b dtmfIMSBackend) Dial(ctx context.Context, id, number string) (vowifi.Call, error) {
	if id != "module-a" {
		return vowifi.Call{}, ErrCarrierUnavailable
	}
	return b.session.DialCall(ctx, number)
}
func (b dtmfIMSBackend) Media(ctx context.Context, id, cid string) (vowifi.CallMedia, error) {
	if id != "module-a" {
		return nil, ErrCarrierUnavailable
	}
	return b.session.CallMedia(ctx, cid)
}
func (b dtmfIMSBackend) Hangup(ctx context.Context, id, cid string) error {
	if id != "module-a" {
		return ErrCarrierUnavailable
	}
	return b.session.HangupCall(ctx, cid)
}
func (b dtmfIMSBackend) SendDTMF(ctx context.Context, id, cid string, digit byte) error {
	if id != "module-a" {
		return ErrCarrierUnavailable
	}
	return b.session.SendDTMF(ctx, cid, digit)
}

type dtmfWireInvite struct {
	request *sip.Request
	rtp     audioDescription
}

func newDTMFIMS(t *testing.T) (*ims.Session, *net.UDPConn, <-chan dtmfWireInvite) {
	t.Helper()
	signal, voice := testMediaSocket(t), testMediaSocket(t)
	invites := make(chan dtmfWireInvite, 2)
	var work sync.WaitGroup
	work.Add(1)
	go func() {
		defer work.Done()
		buffer := make([]byte, 65536)
		for {
			n, source, err := signal.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			message, err := registrarParser().ParseSIP(buffer[:n])
			if err != nil {
				continue
			}
			req, ok := message.(*sip.Request)
			if !ok || req.Method == sip.ACK {
				continue
			}
			response := sip.NewResponseFromRequest(req, 200, "OK", nil)
			response.To().Params.Add("tag", "carrier-dialog")
			if req.Method == sip.REGISTER {
				response.AppendHeader(sip.NewHeader("Contact", req.Contact().Value()+";expires=600"))
				response.AppendHeader(sip.NewHeader("P-Associated-URI", "<sip:+15550000001@ims.mnc001.mcc001.3gppnetwork.org>"))
			}
			if req.Method == sip.INVITE {
				desc, err := parseCallSDP(req.Body())
				if err != nil {
					continue
				}
				select {
				case invites <- dtmfWireInvite{req, desc}:
				default:
				}
				response.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "carrier", Host: "127.0.0.1", Port: signal.LocalAddr().(*net.UDPAddr).Port}})
				response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				response.SetBody(eventSDP(voice.LocalAddr().(*net.UDPAddr).Port, 101, "0-15"))
			}
			signal.WriteToUDP([]byte(response.String()), source)
		}
	}()
	t.Cleanup(func() { signal.Close(); work.Wait() })
	provider, err := ims.NewProvider(dtmfWireAKA{}, ims.Config{PCSCF: signal.LocalAddr().String(), LocalAddress: "127.0.0.1", Transport: "udp", TransactionTimeout: time.Second, SecurityMode: ims.SecurityDisabled})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := provider.Start(ctx, vowifi.IMSRequest{DeviceID: "module-a", Identity: vowifi.SIMIdentity{IMSI: "001010123456789", ICCID: "8901000000000000000", HomeMCC: "001", HomeMNC: "01"}, Tunnel: dtmfWireTunnel{signal.LocalAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	concrete := session.(*ims.Session)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = concrete.Close(ctx)
	})
	return concrete, voice, invites
}
func assertWireEvent(t *testing.T, conn *net.UDPConn, pt, event byte) {
	t.Helper()
	var first []byte
	for i := 0; i < 10; i++ {
		p := readEventPacket(t, conn)
		if i == 0 {
			first = p
		}
		if len(p) != 16 || p[1]&127 != pt || p[12] != event || (p[13]&128 != 0) != (i >= 7) || binary.BigEndian.Uint16(p[14:16]) != uint16(min(i+1, 8)*160) || binary.BigEndian.Uint16(p[2:4]) != binary.BigEndian.Uint16(first[2:4])+uint16(i) || string(p[4:12]) != string(first[4:12]) || (p[1]&128 != 0) != (i == 0) {
			t.Fatal("incorrect RFC 4733 wire event", i, p)
		}
	}
}
func TestSIPDTMFCarrierWireCrossesActualIMSInBothDirections(t *testing.T) {
	session, carrierRTP, invites := newDTMFIMS(t)
	r, err := start(carrierTestConfig(t), runtimeOptions{carrier: dtmfIMSBackend{session}, journal: &callJournal{db: carrierTestStore(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	caller, cm, invite := prepareCaller(t, r, "7700900001")
	invite.SetBody(eventSDP(cm.LocalAddr().(*net.UDPAddr).Port, 110, "0-15"))
	writeCall(t, caller, invite)
	var carrierInvite dtmfWireInvite
	select {
	case carrierInvite = <-invites:
	case <-time.After(2 * time.Second):
		t.Fatal("real IMS INVITE missing")
	}
	answer := readCallFinal(t, caller, 200)
	description, err := parseCallSDP(answer.Body())
	if err != nil || description.events.Payload != 110 || description.events.Mask != 0xffff {
		t.Fatal(description, err)
	}
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.ACK, true, 2))
	waitCarrier(t, func() bool {
		r.mu.Lock()
		leg := r.callLegs[invite.CallID().Value()]
		r.mu.Unlock()
		return leg != nil && leg.call.media.pcm.Load() != nil
	})
	toGateway := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: description.port}
	for i := 0; i < 3; i++ {
		cm.WriteToUDP(eventPacket(110, 5, uint16(30+i), 10000, true), toGateway)
	}
	assertWireEvent(t, carrierRTP, 101, 5)
	noEventPacket(t, carrierRTP) // Three finals caused one exact IMS send.
	toIMS := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: carrierInvite.rtp.port}
	for i := 0; i < 3; i++ {
		carrierRTP.WriteToUDP(eventPacket(101, 11, uint16(100+i), 20000, true), toIMS)
	}
	assertWireEvent(t, cm, 110, 11)
	noEventPacket(t, cm)
	writeCall(t, caller, dialogRequest(t, invite, answer, sip.BYE, true, 3))
	readCallFinal(t, caller, 200)
	waitCalls(t, r, 0)
	waitCarrier(t, func() bool { calls := session.Calls(); return len(calls) == 1 && calls[0].EndedAt != nil })
	for i := 0; i < 3; i++ {
		carrierRTP.WriteToUDP(eventPacket(101, 1, uint16(200+i), 30000, true), toIMS)
	}
	noEventPacket(t, cm)
}
