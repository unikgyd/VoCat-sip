package ike

import (
	"context"
	"errors"
	"net"
	"testing"

	"vocat/internal/vowifi"
)

type aliasReviewDevice struct{}

func (aliasReviewDevice) ReadIdentity(context.Context, string) (vowifi.SIMIdentity, error) {
	return vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"}, nil
}
func (aliasReviewDevice) CheckReady(context.Context, vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	return vowifi.AKAEvidence{Ready: true}, nil
}
func (aliasReviewDevice) Authenticate(context.Context, vowifi.SIMIdentity, vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	panic("alias rejection test must not consume AKA")
}
func (aliasReviewDevice) Snapshot(context.Context, string) (vowifi.RadioSnapshot, error) {
	return vowifi.RadioSnapshot{OperatingMode: 1}, nil
}
func (aliasReviewDevice) EnterVoWiFiRFOff(context.Context, string) error              { return nil }
func (aliasReviewDevice) StopCellularData(context.Context, string) error              { return nil }
func (aliasReviewDevice) Restore(context.Context, string, vowifi.RadioSnapshot) error { return nil }
func (aliasReviewDevice) Resolve(context.Context, vowifi.ProxyRequest) (vowifi.ProxyRoute, error) {
	return vowifi.ProxyRoute{Mode: vowifi.ProxyModeDirect}, nil
}
func (aliasReviewDevice) Start(context.Context, vowifi.IMSRequest) (vowifi.IMSSession, error) {
	panic("IMS must not start after rejection")
}
func (aliasReviewDevice) SaveAssociatedNumber(context.Context, vowifi.PhoneRecord) error { return nil }

type aliasReviewDiscovery struct{}

func (aliasReviewDiscovery) LoadDiscovery(context.Context, vowifi.SIMIdentity) (vowifi.EPDGDiscoveryOptions, error) {
	return vowifi.EPDGDiscoveryOptions{PreferredAddresses: []string{"first.example", "alias.example"}, ManualOnly: true}, nil
}
func (aliasReviewDiscovery) RememberRegistered(context.Context, vowifi.SIMIdentity, vowifi.EPDGCandidate) error {
	panic("rejected IKE cannot become success history")
}

type aliasReviewWire struct {
	*backoffTranscript
	remote      net.IP
	initPackets *int
}

func (tr *aliasReviewWire) RemoteAddr() *net.UDPAddr { return &net.UDPAddr{IP: tr.remote, Port: 500} }
func (tr *aliasReviewWire) RoundTrip(ctx context.Context, packet []byte) ([]byte, error) {
	*tr.initPackets++
	return tr.backoffTranscript.RoundTrip(ctx, packet)
}

// Uses the real provider (DH, certificate signature, encrypted AUTH and DELETE)
// under the real Orchestrator. The fake datagram factory models DNS aliases;
// no API that doesn't exist in the original source is presumed by the test.
func TestEPDGAliasReviewSamePeerCannotBypassAuthenticatedNetworkBackoff(t *testing.T) {
	key, der, roots := backoffCertificates(t)
	for _, samePeer := range []bool{true, false} {
		name := "same-peer"
		if !samePeer {
			name = "different-peer-control"
		}
		t.Run(name, func(t *testing.T) {
			provider, err := NewProvider(Config{RootCAs: roots, ServerName: "epdg.example", Installer: unusedInstaller{}})
			if err != nil {
				t.Fatal(err)
			}
			packets := 0
			var wires []*aliasReviewWire
			provider.transportFactory = func(_ context.Context, _ transportConfig, _ vowifi.ProxyRoute, host string) (datagramTransport, error) {
				ip := net.ParseIP("192.0.2.9")
				if !samePeer && host == "alias.example" {
					ip = net.ParseIP("192.0.2.10")
				}
				tr := &aliasReviewWire{backoffTranscript: &backoffTranscript{initialAuthTransport: &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa", key: key, certificate: der}, behavior: "network"}, remote: ip, initPackets: &packets}
				wires = append(wires, tr)
				return tr, nil
			}
			d := aliasReviewDevice{}
			o, err := vowifi.New(vowifi.Dependencies{SIM: d, AKA: d, Radio: d, Proxy: d, Tunnel: provider, IMS: d, Phones: d, Discovery: aliasReviewDiscovery{}}, vowifi.Options{DeviceID: "alias-review"})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Close(context.Background())
			first, err := o.Enable(context.Background())
			var proof *vowifi.EPDGNetworkRejection
			if err == nil || !errors.As(err, &proof) || packets != 1 || len(wires) != 1 || !wires[0].deleted || wires[0].closes != 1 || first.IKERetry == nil {
				t.Fatalf("first authenticated exchange not proven: packets=%d wires=%d status=%+v err=%v", packets, len(wires), first.IKERetry, err)
			}
			second, err := o.Enable(context.Background())
			if err == nil || second.IMSReady {
				t.Fatal("rejected alias incorrectly registered")
			}
			want := 2
			if samePeer {
				want = 1
			}
			if packets != want {
				t.Fatalf("same-peer=%v alias sent %d initial IKE packets, want %d after authenticated NETWORK_FAILURE", samePeer, packets, want)
			}
			if !samePeer && (len(wires) != 2 || !wires[1].deleted || wires[1].closes != 1) {
				t.Fatal("distinct-peer control did not complete its authenticated rejection/cleanup")
			}
		})
	}
}
