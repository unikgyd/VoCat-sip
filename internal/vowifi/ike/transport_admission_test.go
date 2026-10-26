package ike

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func transportPeerLoopback(t *testing.T, mode string, before func(context.Context, string) error) (datagramTransport, *net.UDPConn, *net.UDPAddr) {
	t.Helper()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	config := transportConfig{Dialer: &net.Dialer{}, Timeout: 300 * time.Millisecond, BeforeIKEEndpoint: before}
	remote := cloneUDPAddr(server.LocalAddr().(*net.UDPAddr))
	var tr datagramTransport
	if mode == "direct" {
		tr, err = newDirectUDPCandidates(context.Background(), config, []*net.UDPAddr{remote, {IP: net.ParseIP("127.0.0.2"), Port: remote.Port}})
	} else {
		var c *net.UDPConn
		c, err = net.DialUDP("udp4", nil, remote)
		remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.9"), Port: 500}
		tr = &socks5UDP{config: config, udp: c, remote: cloneUDPAddr(remote), remotes: []*net.UDPAddr{remote, {IP: net.ParseIP("192.0.2.10"), Port: 500}}}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr, server, remote
}

func transportPeerNoPacket(t *testing.T, server *net.UDPConn) {
	t.Helper()
	server.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, _, err := server.ReadFromUDP(make([]byte, 2048)); !isNetworkTimeout(err) {
		t.Fatalf("unexpected datagram or read failure: %v", err)
	}
}

func TestTransportPeerAdmissionPolicyFailureDoesNotTryOtherCandidates(t *testing.T) {
	for _, mode := range []string{"direct", "socks5"} {
		for _, cause := range []error{vowifi.ErrIKERetryBlocked, vowifi.ErrIKERetryStore, vowifi.ErrIKERetryBinding} {
			t.Run(mode+"/"+cause.Error(), func(t *testing.T) {
				calls := 0
				denied := errors.Join(cause, deadlineError{})
				tr, server, remote := transportPeerLoopback(t, mode, func(_ context.Context, peer string) error {
					calls++
					if peer != remoteIPForTransportPeerTest(mode) {
						t.Errorf("policy invoked for a different destination: %s", peer)
					}
					return denied
				})
				request := ikeHeader{InitiatorSPI: [8]byte{1}, Exchange: exchangeIKEInit, Flags: flagInitiator}.marshal(nil)
				_, err := tr.RoundTrip(context.Background(), request)
				if !errors.Is(err, cause) || calls != 1 || !tr.RemoteAddr().IP.Equal(remote.IP) {
					t.Fatalf("policy error became a network fallback: calls=%d remote=%v err=%v", calls, tr.RemoteAddr(), err)
				}
				transportPeerNoPacket(t, server)
			})
		}
	}
}

func remoteIPForTransportPeerTest(mode string) string {
	if mode == "direct" {
		return "127.0.0.1"
	}
	return "192.0.2.9"
}

func TestTransportPeerAdmissionCancellationAfterCallbackDoesNotWrite(t *testing.T) {
	for _, mode := range []string{"direct", "socks5"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			tr, server, _ := transportPeerLoopback(t, mode, func(context.Context, string) error { calls++; cancel(); return nil })
			request := ikeHeader{InitiatorSPI: [8]byte{1}, Exchange: exchangeIKEInit, Flags: flagInitiator}.marshal(nil)
			_, err := tr.RoundTrip(ctx, request)
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("callback cancellation not retained: calls=%d err=%v", calls, err)
			}
			transportPeerNoPacket(t, server)
		})
	}
}

func TestTransportPeerAdmissionCookieEchoIsCheckedAgain(t *testing.T) {
	for _, mode := range []string{"direct", "socks5"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			tr, server, remote := transportPeerLoopback(t, mode, func(context.Context, string) error {
				calls++
				if calls > 1 {
					return vowifi.ErrIKERetryBlocked
				}
				return nil
			})
			done := make(chan error, 1)
			go func() {
				buffer := make([]byte, 2048)
				server.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, peer, err := server.ReadFromUDP(buffer)
				if err != nil {
					done <- err
					return
				}
				packet := buffer[:n]
				if mode == "socks5" {
					packet, _, err = parseSOCKS5Datagram(packet)
				}
				h, _, err := parseIKEPacket(packet)
				if err != nil {
					done <- err
					return
				}
				h.Flags = flagResponse
				first, body, err := marshalPayloadChain([]payload{makeNotify(notifyCookie, []byte{1, 2, 3, 4})})
				h.NextPayload = first
				wire := h.marshal(body)
				if mode == "socks5" {
					wire, err = marshalSOCKS5Datagram(remote, wire)
				}
				if err == nil {
					_, err = server.WriteToUDP(wire, peer)
				}
				done <- err
			}()
			h := ikeHeader{InitiatorSPI: [8]byte{1}, Exchange: exchangeIKEInit, Flags: flagInitiator}
			if _, err := tr.RoundTrip(context.Background(), h.marshal(nil)); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			h.NextPayload, _, _ = marshalPayloadChain([]payload{makeNotify(notifyCookie, []byte{1, 2, 3, 4})})
			_, body, _ := marshalPayloadChain([]payload{makeNotify(notifyCookie, []byte{1, 2, 3, 4})})
			_, err := tr.RoundTrip(context.Background(), h.marshal(body))
			if !errors.Is(err, vowifi.ErrIKERetryBlocked) || calls != 2 {
				t.Fatalf("COOKIE echo escaped gate: calls=%d err=%v", calls, err)
			}
			transportPeerNoPacket(t, server)
		})
	}
}

func TestTransportPeerAdmissionPreservesEncryptedAuthDeleteAndESP(t *testing.T) {
	for _, mode := range []string{"direct", "socks5"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			tr, server, remote := transportPeerLoopback(t, mode, func(context.Context, string) error { calls.Add(1); return vowifi.ErrIKERetryBlocked })
			// Already-floated state on a private ephemeral socket, never bind or
			// reconfigure a host's shared UDP/4500 or any kernel interface.
			switch c := tr.(type) {
			case *directUDP:
				c.floated = true
				c.pinned = true
			case *socks5UDP:
				c.floated = true
				c.pinned = true
			}
			suite := legacyTestSuite()
			spii, spir := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, [8]byte{8, 7, 6, 5, 4, 3, 2, 1}
			keys, err := deriveIKEKeys(suite, bytes.Repeat([]byte{0x42}, 32), bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), spii, spir)
			if err != nil {
				t.Fatal(err)
			}
			esp := []byte{0x11, 0x22, 0x33, 0x44, 0, 0, 0, 1, 9, 8, 7, 6}
			done := make(chan error, 1)
			go func() {
				buffer := make([]byte, 4096)
				server.SetReadDeadline(time.Now().Add(3 * time.Second))
				for i := 0; i < 3; i++ {
					n, peer, err := server.ReadFromUDP(buffer)
					if err != nil {
						done <- err
						return
					}
					packet := buffer[:n]
					if mode == "socks5" {
						var destination *net.UDPAddr
						packet, destination, err = parseSOCKS5Datagram(packet)
						if err != nil || !destination.IP.Equal(remote.IP) || destination.Port != remote.Port {
							done <- errors.New("wrong SOCKS destination")
							return
						}
					}
					if i == 2 {
						if !bytes.Equal(packet, esp) {
							done <- errors.New("ESP altered or blocked")
							return
						}
						continue
					}
					if !hasNonESPMarker(packet) {
						done <- errors.New("NAT-T IKE marker missing")
						return
					}
					h, items, err := decryptPayloads(packet[4:], suite, keys.SKei, keys.SKai)
					if err != nil {
						done <- err
						return
					}
					if i == 1 {
						if h.Exchange != exchangeInformational || len(items) != 1 || items[0].Type != payloadDelete || items[0].Body[0] != protocolIKE {
							done <- errors.New("encrypted DELETE missing")
							return
						}
						continue
					}
					if h.Exchange != exchangeIKEAuth || h.MessageID != 1 {
						done <- errors.New("encrypted AUTH missing")
						return
					}
					h.Flags = flagResponse
					wire, err := encryptPayloads(h, items, suite, keys.SKer, keys.SKar, rand.Reader)
					wire = append([]byte{0, 0, 0, 0}, wire...)
					if mode == "socks5" {
						wire, err = marshalSOCKS5Datagram(remote, wire)
					}
					if err == nil {
						_, err = server.WriteToUDP(wire, peer)
					}
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			h := ikeHeader{InitiatorSPI: spii, ResponderSPI: spir, Exchange: exchangeIKEAuth, Flags: flagInitiator, MessageID: 1}
			request, err := encryptPayloads(h, []payload{{Type: payloadEAP, Body: []byte{2, 1, 0, 4}}}, suite, keys.SKei, keys.SKai, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			response, err := tr.RoundTrip(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := decryptPayloads(response, suite, keys.SKer, keys.SKar); err != nil {
				t.Fatal(err)
			}
			if err := sendIKESADelete(context.Background(), tr, suite, keys, spii, spir, 2); err != nil {
				t.Fatal(err)
			}
			if err := tr.SendESP(context.Background(), esp); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatalf("INIT policy blocked established traffic: callbacks=%d", calls.Load())
			}
		})
	}
}

type transportPeerChangedAfterInit struct {
	*firstAuthCaptureTransport
	closes int
}

func (tr *transportPeerChangedAfterInit) RemoteAddr() *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, byte(9+tr.calls)), Port: 500}
}
func (tr *transportPeerChangedAfterInit) Close() error { tr.closes++; return nil }

func TestTransportPeerAdmissionRejectsPinnedAndCookiePeerChanges(t *testing.T) {
	for _, mode := range []string{"pinned", "cookie"} {
		t.Run(mode, func(t *testing.T) {
			provider, err := NewProvider(Config{Installer: unusedInstaller{}})
			if err != nil {
				t.Fatal(err)
			}
			tr := &transportPeerChangedAfterInit{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			request := familyRequest(&testAKAProvider{})
			wantCalls, wantMessage := 1, "changed the pinned ePDG"
			if mode == "pinned" {
				_, err = provider.startFamily(context.Background(), request, false, innerDual, "192.0.2.9")
			} else {
				tr.cookieChallenge = []byte{1, 2, 3, 4}
				wantCalls, wantMessage = 2, "changed the ePDG after a COOKIE"
				_, err = provider.Start(context.Background(), request)
			}
			if err == nil || !strings.Contains(err.Error(), wantMessage) || tr.calls != wantCalls || tr.closes != 1 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("changed peer contract ignored: calls=%d closes=%d err=%v", tr.calls, tr.closes, err)
			}
		})
	}
}

// End-to-end provider plus real direct/SOCKS UDP fallback, real DH, signed
// encrypted rejection, observer and encrypted DELETE on the actual peer.
func TestTransportPeerAdmissionActualWireAuthenticatedFallbackAndCleanup(t *testing.T) {
	key, der, roots := backoffCertificates(t)
	for _, mode := range []string{"direct", "socks5"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			listen := func(ip string) *net.UDPConn {
				c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			firstSocket, server := listen("127.0.0.1"), listen("127.0.0.2")
			first, second := cloneUDPAddr(firstSocket.LocalAddr().(*net.UDPAddr)), cloneUDPAddr(server.LocalAddr().(*net.UDPAddr))
			var firstPackets atomic.Int32
			firstFinished := make(chan struct{})
			if mode == "socks5" {
				first, second = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 500}, &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 500}
				close(firstFinished)
			} else {
				go func() {
					defer close(firstFinished)
					for {
						if _, _, err := firstSocket.ReadFromUDP(make([]byte, 4096)); err != nil {
							return
						}
						firstPackets.Add(1)
					}
				}()
			}
			transcript := &backoffTranscript{initialAuthTransport: &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa", key: key, certificate: der}, behavior: "network"}
			finished, result := make(chan struct{}), make(chan error, 1)
			go func() {
				defer close(finished)
				buf := make([]byte, 65535)
				server.SetReadDeadline(time.Now().Add(5 * time.Second))
				for {
					n, peer, err := server.ReadFromUDP(buf)
					if err != nil {
						result <- err
						return
					}
					packet, destination := buf[:n], cloneUDPAddr(server.LocalAddr().(*net.UDPAddr))
					if mode == "socks5" {
						packet, destination, err = parseSOCKS5Datagram(packet)
						if err != nil {
							result <- err
							return
						}
					}
					if destination.IP.Equal(first.IP) {
						firstPackets.Add(1)
						continue
					}
					if !destination.IP.Equal(second.IP) {
						result <- errors.New("unexpected actual gateway")
						return
					}
					if hasNonESPMarker(packet) {
						packet = packet[4:]
					}
					h, _, err := parseIKEPacket(packet)
					if err != nil {
						result <- err
						return
					}
					var response []byte
					switch h.Exchange {
					case exchangeIKEInit:
						response, err = transcript.RoundTrip(ctx, packet)
					case exchangeIKEAuth:
						var responses [][]byte
						responses, err = transcript.RoundTripExchange(ctx, [][]byte{packet})
						if err == nil {
							response = responses[0]
						}
					case exchangeInformational:
						result <- transcript.SendSessionPacket(ctx, packet, true)
						return
					default:
						result <- errors.New("unexpected IKE exchange")
						return
					}
					if err != nil {
						result <- err
						return
					}
					if mode == "socks5" {
						if h.Exchange != exchangeIKEInit {
							response = append([]byte{0, 0, 0, 0}, response...)
						}
						response, err = marshalSOCKS5Datagram(destination, response)
					}
					if err == nil {
						_, err = server.WriteToUDP(response, peer)
					}
					if err != nil {
						result <- err
						return
					}
				}
			}()
			t.Cleanup(func() { firstSocket.Close(); server.Close(); <-firstFinished; <-finished })
			provider, err := NewProvider(Config{RootCAs: roots, ServerName: "epdg.example", Installer: unusedInstaller{}})
			if err != nil {
				t.Fatal(err)
			}
			var actual datagramTransport
			factories := 0
			provider.transportFactory = func(ctx context.Context, config transportConfig, _ vowifi.ProxyRoute, _ string) (datagramTransport, error) {
				factories++
				config.Timeout = 400 * time.Millisecond
				if mode == "direct" {
					actual, err = newDirectUDPCandidates(ctx, config, []*net.UDPAddr{first, second})
				} else {
					var c *net.UDPConn
					c, err = net.DialUDP("udp4", nil, server.LocalAddr().(*net.UDPAddr))
					actual = &socks5UDP{config: config, udp: c, remote: cloneUDPAddr(first), remotes: cloneUDPAddrs([]*net.UDPAddr{first, second})}
				}
				return actual, err
			}
			var restricted atomic.Bool
			admissions, observations := 0, 0
			request := familyRequest(&testAKAProvider{})
			if mode == "socks5" {
				request.Proxy.Mode = vowifi.ProxyModeSOCKS5
			}
			request.BeforeIKEEndpoint = func(context.Context, string) error {
				admissions++
				if restricted.Load() {
					return vowifi.ErrIKERetryBlocked
				}
				return nil
			}
			request.ObserveNetworkRejection = func(proof vowifi.EPDGNetworkRejection) error {
				observations++
				restricted.Store(true)
				if proof.Endpoint != second.IP.String() {
					t.Errorf("observer got stale gateway %s, want %s", proof.Endpoint, second.IP)
				}
				return nil
			}
			_, err = provider.Start(ctx, request)
			var proof *vowifi.EPDGNetworkRejection
			if err == nil || !errors.As(err, &proof) || proof.Endpoint != second.IP.String() || proof.NotifyType != 10500 || observations != 1 || admissions != 3 || factories != 1 {
				t.Fatalf("actual wire evidence failed: admissions=%d observations=%d factories=%d proof=%+v err=%v", admissions, observations, factories, proof, err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if firstPackets.Load() != 1 || transcript.calls != 2 || !transcript.deleted || actual.LocalAddr() != nil {
				t.Fatalf("wire fallback/cleanup missing: first=%d exchanges=%d delete=%v local=%v", firstPackets.Load(), transcript.calls, transcript.deleted, actual.LocalAddr())
			}
		})
	}
}
