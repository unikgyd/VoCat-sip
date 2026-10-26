package ims

import (
	"context"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type profileIMSPhysicalAKA struct {
	recordingAKA
	physical vowifi.SIMIdentity
}

func (p *profileIMSPhysicalAKA) Authenticate(ctx context.Context, id vowifi.SIMIdentity, c vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	p.physical = id
	return p.recordingAKA.Authenticate(ctx, id, c)
}
func profileIMSIdentity(t *testing.T) (vowifi.SIMIdentity, vowifi.SIMIdentity) {
	t.Helper()
	live := vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150123456789", IMEI: "867530912345678", HomeMCC: "234", HomeMNC: "15"}
	selected := live
	selected.IMSI = "001010987654321"
	selected.HomeMCC = "001"
	selected.HomeMNC = "01"
	bound, err := vowifi.ProfileAuthenticationIdentity(live, selected)
	if err != nil {
		t.Fatal(err)
	}
	return live, bound
}

func TestProfileIdentityActualRegisterAndInboundSMSKeepThePhysicalSource(t *testing.T) {
	live, bound := profileIMSIdentity(t)
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(10 * time.Second))
	received := make(chan ReceivedSMS, 1)
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 32))
	go func() { serverDone <- serveInboundSMS(listener, nonce, ready) }()
	aka := &profileIMSPhysicalAKA{recordingAKA: recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}}}}
	provider, err := NewProvider(aka, Config{PCSCF: listener.LocalAddr().String(), LocalAddress: "127.0.0.1", Transport: "udp", TransactionTimeout: 3 * time.Second, SecurityMode: SecurityDisabled, OnSMS: func(_ context.Context, sms ReceivedSMS) error { received <- sms; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := deriveIdentities(bound, provider.config)
	if err != nil || !strings.Contains(identities.private, bound.IMSI) || strings.Contains(identities.private, live.IMSI) {
		t.Fatalf("wrong SIP identity: %v", err)
	}
	session, err := provider.Start(context.Background(), vowifi.IMSRequest{DeviceID: "profile", Identity: bound, Tunnel: evidenceTunnel{evidence: vowifi.TunnelEvidence{Established: true, LocalIPv4: "127.0.0.1", PCSCF: []string{listener.LocalAddr().String()}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	select {
	case sms := <-received:
		if sms.IMSI != live.IMSI || sms.ICCID != live.ICCID || sms.Text != "HELLO" {
			t.Fatal("inbound SMS lost its physical source")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMS timeout")
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("RP-ACK timeout")
	}
	if aka.physical.IMSI != live.IMSI || len(aka.challenges) != 1 {
		t.Fatal("IMS AKA used wire IMSI as the card binding")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestProfileIdentityActualIncomingCallCallbackUsesPhysicalSource(t *testing.T) {
	live, bound := profileIMSIdentity(t)
	session, _ := newCallTestSession(t)
	session.request = vowifi.IMSRequest{DeviceID: "profile", Identity: bound}
	received := make(chan ReceivedCall, 1)
	session.provider.config.OnIncomingCall = func(_ context.Context, call ReceivedCall) error { received <- call; return nil }
	if !session.handleIncomingInvite(incomingTestInvite(t, "profile-incoming"), func([]byte) error { return nil }) {
		t.Fatal("incoming INVITE not handled")
	}
	select {
	case call := <-received:
		if call.IMSI != live.IMSI || call.DeviceID != "profile" {
			t.Fatal("call callback used the authentication identity as physical source")
		}
	case <-time.After(time.Second):
		t.Fatal("incoming call callback timeout")
	}
}

func TestProfileIdentityMutationIsRejectedBeforeIMSNetworkAccess(t *testing.T) {
	_, bound := profileIMSIdentity(t)
	bound.IMSI = "001010987654320"
	provider, _ := NewProvider(&recordingAKA{}, Config{SecurityMode: SecurityDisabled})
	if _, err := provider.Start(context.Background(), vowifi.IMSRequest{Identity: bound}); err != vowifi.ErrProfileIdentityBinding {
		t.Fatalf("mutated binding reached access discovery: %v", err)
	}
}
