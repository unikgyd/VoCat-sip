package ims

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type gatewayProvisionalConn struct {
	registerRetransmitConn
	read bool
}

func (c *gatewayProvisionalConn) Read(b []byte) (int, error) {
	if c.read {
		return 0, immediateTimeoutError{}
	}
	c.read = true
	return copy(b, []byte("SIP/2.0 100 Trying\r\nCall-ID: gateway-provisional\r\nCSeq: 7 REGISTER\r\nContent-Length: 0\r\n\r\n")), nil
}
func TestIMSGatewayProvisionalResponseIsNotSilence(t *testing.T) {
	s := &Session{provider: &Provider{config: Config{TransactionTimeout: time.Nanosecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}, request: vowifi.IMSRequest{Identity: vowifi.SIMIdentity{HomeMCC: "001", HomeMNC: "01"}}, transport: "udp", conn: &gatewayProvisionalConn{}, callID: "gateway-provisional"}
	_, err := s.exchange(context.Background(), []byte("synthetic REGISTER"), 7)
	if err == nil || s.evidence.LastSIPCode != 100 {
		t.Fatalf("100 Trying was treated as no SIP response: code=%d err=%v", s.evidence.LastSIPCode, err)
	}
}

type gatewaySIM struct{}

func (gatewaySIM) ReadIdentity(context.Context, string) (vowifi.SIMIdentity, error) {
	return vowifi.SIMIdentity{ICCID: "8901000000000000000", IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"}, nil
}

type gatewayRadio struct{ snapshots, off, restores int }

func (r *gatewayRadio) Snapshot(context.Context, string) (vowifi.RadioSnapshot, error) {
	r.snapshots++
	return vowifi.RadioSnapshot{OperatingMode: 1}, nil
}
func (r *gatewayRadio) StopCellularData(context.Context, string) error { return nil }
func (r *gatewayRadio) EnterVoWiFiRFOff(context.Context, string) error { r.off++; return nil }
func (r *gatewayRadio) Restore(context.Context, string, vowifi.RadioSnapshot) error {
	r.restores++
	return nil
}

type gatewayProxy struct{}

func (gatewayProxy) Resolve(context.Context, vowifi.ProxyRequest) (vowifi.ProxyRoute, error) {
	return vowifi.ProxyRoute{Mode: vowifi.ProxyModeDirect}, nil
}

type gatewayPhones struct{}

func (gatewayPhones) SaveAssociatedNumber(context.Context, vowifi.PhoneRecord) error { return nil }

type gatewayMemory struct{ remembered int }

func (*gatewayMemory) LoadDiscovery(context.Context, vowifi.SIMIdentity) (vowifi.EPDGDiscoveryOptions, error) {
	return vowifi.EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{"synthetic.example"}}, nil
}
func (m *gatewayMemory) RememberRegistered(context.Context, vowifi.SIMIdentity, vowifi.EPDGCandidate) error {
	m.remembered++
	return nil
}

type gatewayOuter struct {
	addresses      []string
	starts, closed int
}
type gatewayOuterSession struct {
	evidenceTunnel
	owner *gatewayOuter
}

func (s *gatewayOuterSession) Close(context.Context) error { s.owner.closed++; return nil }
func (p *gatewayOuter) Start(context.Context, vowifi.TunnelRequest) (vowifi.TunnelSession, error) {
	i := p.starts
	p.starts++
	if i >= len(p.addresses) {
		return nil, fmt.Errorf("synthetic endpoints exhausted")
	}
	return &gatewayOuterSession{evidenceTunnel{vowifi.TunnelEvidence{Established: true, EPDGAddress: fmt.Sprintf("203.0.113.%d", i+1), LocalIPv4: "127.0.0.1", PCSCF: []string{p.addresses[i]}, ResponderAUTH: vowifi.ResponderAUTHVerified, IKEEncryption: "aes-cbc-128", IKEIntegrity: "hmac-sha2-256", IKEDHGroup: "modp2048", ESPEncryption: "aes-cbc-128", ESPIntegrity: "hmac-sha1-96"}}, p}, nil
}

// Exercise the real Orchestrator -> IMS Provider -> UDP REGISTER chain. Only
// the outer tunnel and SIM/radio hardware are synthetic, no operator is used.
func TestIMSGatewayRealProviderRebuildsAfterSilentPCSCF(t *testing.T) {
	first, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var registered atomic.Int32
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, peer, err := second.ReadFromUDP(buffer)
			if err != nil {
				done <- nil
				return
			}
			_, h, err := parseTestRequest(buffer[:n])
			if err != nil {
				done <- err
				return
			}
			if h["expires"] != "0" {
				registered.Add(1)
			}
			_, err = second.WriteToUDP(testResponse(200, "OK", h["call-id"], h["cseq"], []string{"Contact: " + h["contact"], "Expires: 3600"}), peer)
			if err != nil {
				done <- err
				return
			}
		}
	}()
	defer func() {
		_ = second.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	aka := &recordingAKA{}
	p, err := NewProvider(aka, Config{Transport: "udp", TransactionTimeout: time.Second, SecurityMode: SecurityDisabled, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	outer := &gatewayOuter{addresses: []string{first.LocalAddr().String(), second.LocalAddr().String()}}
	radio, memory := &gatewayRadio{}, &gatewayMemory{}
	o, err := vowifi.New(vowifi.Dependencies{SIM: gatewaySIM{}, AKA: aka, Radio: radio, Proxy: gatewayProxy{}, Tunnel: outer, IMS: p, Phones: gatewayPhones{}, Discovery: memory}, vowifi.Options{DeviceID: "synthetic", AllowIMSWithoutSMS: true, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := o.Enable(ctx)
	if err != nil || !state.IMSReady || state.EPDGAddress != "203.0.113.2" || outer.starts != 2 || outer.closed != 1 || registered.Load() != 1 || memory.remembered != 1 {
		t.Fatalf("real IMS failover absent: err=%v state=%+v starts=%d closes=%d registered=%d remembered=%d", err, state, outer.starts, outer.closed, registered.Load(), memory.remembered)
	}
	if radio.snapshots != 1 || radio.off != 1 || radio.restores != 0 || len(aka.challenges) != 0 {
		t.Fatal("retry touched RF, restored cellular or consumed an IMS AKA vector")
	}
}
