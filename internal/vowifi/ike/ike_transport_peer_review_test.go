package ike

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type switchedPeerReview struct {
	*backoffTranscript
	switched bool
	change   bool
}

// Exercise the provider's existing callback through the actual direct/SOCKS
// datagram implementations, not a mock RoundTrip that invokes it for us.
func TestTransportPeerReviewEveryActualInitSendIsAdmitted(t *testing.T) {
	for _, mode := range []string{"direct", "socks5"} {
		for _, scenario := range []string{"candidate", "retransmit"} {
			for _, deny := range []bool{true, false} {
				name := mode + "/" + scenario + "/deny"
				if !deny {
					name = mode + "/" + scenario + "/allow-control"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					listen := func(ip string) *net.UDPConn {
						c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
						if err != nil {
							t.Fatal(err)
						}
						return c
					}
					firstSocket := listen("127.0.0.1")
					secondSocket := listen("127.0.0.2")
					first := cloneUDPAddr(firstSocket.LocalAddr().(*net.UDPAddr))
					second := cloneUDPAddr(secondSocket.LocalAddr().(*net.UDPAddr))
					var firstPackets, secondPackets atomic.Int32
					var firstSeen atomic.Bool
					serverErrors := make(chan error, 2)
					done := make(chan struct{}, 2)
					serve := func(c *net.UDPConn, socks bool) {
						defer func() { done <- struct{}{} }()
						buf := make([]byte, 65535)
						for {
							n, peer, err := c.ReadFromUDP(buf)
							if err != nil {
								return
							}
							packet, destination := buf[:n], cloneUDPAddr(c.LocalAddr().(*net.UDPAddr))
							if socks {
								packet, destination, err = parseSOCKS5Datagram(packet)
								if err != nil {
									serverErrors <- err
									return
								}
							}
							isFirst := destination.IP.Equal(first.IP)
							count := secondPackets.Add(1)
							if isFirst {
								secondPackets.Add(-1)
								count = firstPackets.Add(1)
								firstSeen.Store(true)
							}
							if scenario == "candidate" && isFirst || scenario == "retransmit" && count == 1 {
								continue
							}
							h, _, err := parseIKEPacket(packet)
							if err != nil {
								serverErrors <- err
								return
							}
							h.Flags, h.ResponderSPI = flagResponse, [8]byte{8, 7, 6, 5, 4, 3, 2, 1}
							var body []byte
							h.NextPayload, body, err = marshalPayloadChain([]payload{makeNotify(24, nil)})
							if err != nil {
								serverErrors <- err
								return
							}
							wire := h.marshal(body)
							if socks {
								wire, err = marshalSOCKS5Datagram(destination, wire)
							}
							if err == nil {
								_, err = c.WriteToUDP(wire, peer)
							}
							if err != nil {
								serverErrors <- err
								return
							}
						}
					}
					if mode == "socks5" {
						first, second = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 500}, &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 500}
						go serve(firstSocket, true)
					} else {
						go serve(firstSocket, false)
						go serve(secondSocket, false)
					}
					t.Cleanup(func() {
						firstSocket.Close()
						secondSocket.Close()
						<-done
						if mode == "direct" {
							<-done
						}
						select {
						case err := <-serverErrors:
							t.Error(err)
						default:
						}
					})
					provider, err := NewProvider(Config{Installer: unusedInstaller{}, AutoProposalFallback: true})
					if err != nil {
						t.Fatal(err)
					}
					factories := 0
					provider.transportFactory = func(ctx context.Context, config transportConfig, _ vowifi.ProxyRoute, _ string) (datagramTransport, error) {
						factories++
						config.Timeout = 100 * time.Millisecond
						remotes := []*net.UDPAddr{first, second}
						if scenario == "retransmit" {
							config.Timeout = 1400 * time.Millisecond
							remotes = remotes[:1]
						}
						if mode == "direct" {
							return newDirectUDPCandidates(ctx, config, remotes)
						}
						connection, err := net.DialUDP("udp4", nil, firstSocket.LocalAddr().(*net.UDPAddr))
						if err != nil {
							return nil, err
						}
						return &socks5UDP{config: config, udp: connection, remote: cloneUDPAddr(first), remotes: cloneUDPAddrs(remotes)}, nil
					}
					request := familyRequest(&testAKAProvider{})
					request.BeforeIKEEndpoint = func(ctx context.Context, peer string) error {
						if err := ctx.Err(); err != nil {
							return err
						}
						if deny && (scenario == "candidate" && peer == second.IP.String() || scenario == "retransmit" && firstSeen.Load()) {
							return vowifi.ErrIKERetryBlocked
						}
						return nil
					}
					_, err = provider.Start(ctx, request)
					if err == nil || factories != 1 {
						t.Fatalf("unexpected retry/success: factories=%d err=%v", factories, err)
					}
					if firstPackets.Load() < 1 {
						t.Fatal("first UDP packet never observed")
					}
					if deny {
						if !errors.Is(err, vowifi.ErrIKERetryBlocked) || firstPackets.Load() != 1 || secondPackets.Load() != 0 {
							t.Fatalf("denied actual INIT still sent: first=%d second=%d err=%v", firstPackets.Load(), secondPackets.Load(), err)
						}
					} else if scenario == "candidate" && (firstPackets.Load() != 1 || secondPackets.Load() != 1) || scenario == "retransmit" && (firstPackets.Load() != 2 || secondPackets.Load() != 0) || !errors.Is(err, vowifi.ErrEAPAuthenticationRejected) {
						t.Fatalf("allowed network control broken: first=%d second=%d err=%v", firstPackets.Load(), secondPackets.Load(), err)
					}
				})
			}
		}
	}
}

func (tr *switchedPeerReview) RemoteAddr() *net.UDPAddr {
	ip := "192.0.2.9"
	if tr.change && tr.switched {
		ip = "192.0.2.10"
	}
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: 500}
}
func (tr *switchedPeerReview) RoundTrip(ctx context.Context, packet []byte) ([]byte, error) {
	reply, err := tr.backoffTranscript.RoundTrip(ctx, packet)
	if err == nil {
		tr.switched = true
	}
	return reply, err
}

// The real transport may move to another resolved address before an INIT
// succeeds. Provider evidence must describe that actual peer, not the address
// exposed before RoundTrip. All DH/signature/encryption and cleanup are real.
func TestTransportPeerReviewAuthenticatedRejectionUsesActualAnsweringPeer(t *testing.T) {
	key, der, roots := backoffCertificates(t)
	for _, change := range []bool{true, false} {
		name := "switched"
		if !change {
			name = "same-peer-control"
		}
		t.Run(name, func(t *testing.T) {
			provider, err := NewProvider(Config{RootCAs: roots, ServerName: "epdg.example", Installer: unusedInstaller{}})
			if err != nil {
				t.Fatal(err)
			}
			tr := &switchedPeerReview{backoffTranscript: &backoffTranscript{initialAuthTransport: &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa", key: key, certificate: der}, behavior: "network"}, change: change}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			_, err = provider.Start(context.Background(), familyRequest(&testAKAProvider{}))
			var rejection *vowifi.EPDGNetworkRejection
			if err == nil || !errors.As(err, &rejection) || !tr.deleted || tr.closes != 1 || tr.calls != 2 {
				t.Fatalf("authenticated rejection/cleanup missing: calls=%d deletes=%v closes=%d err=%v", tr.calls, tr.deleted, tr.closes, err)
			}
			want := tr.RemoteAddr().IP.String()
			if rejection.Endpoint != want {
				t.Fatalf("authenticated rejection bound to stale peer %s, actual answering peer %s", rejection.Endpoint, want)
			}
		})
	}
}
