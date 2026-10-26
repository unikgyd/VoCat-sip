package ike

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type familyLookup struct {
	call func(context.Context, string, string) ([]net.IP, error)
}

func (l familyLookup) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	return l.call(ctx, network, host)
}
func TestEPDGSystemFamiliesKeepWorkingFamilyAndFilterDuplicates(t *testing.T) {
	for _, success := range []string{"ip4", "ip6"} {
		t.Run(success, func(t *testing.T) {
			lookup := familyLookup{func(_ context.Context, family, host string) ([]net.IP, error) {
				if host != "gateway.example" {
					t.Error("wrong host")
				}
				if family != success {
					return nil, &net.DNSError{Err: "NXDOMAIN", Name: host, IsNotFound: true}
				}
				ip := net.ParseIP("203.0.113.9")
				if family == "ip6" {
					ip = net.ParseIP("2001:db8::9")
				}
				return []net.IP{ip, ip, net.ParseIP("127.0.0.1"), net.ParseIP("ff02::1"), net.ParseIP("fe80::1")}, nil
			}}
			got, err := resolveSystemFamilies(context.Background(), lookup, "gateway.example")
			if err != nil || len(got) != 1 || success == "ip4" && got[0].IP.To4() == nil || success == "ip6" && got[0].IP.To4() != nil {
				t.Fatalf("addresses %v %v", got, err)
			}
		})
	}
}
func TestEPDGDNSCancellationAndLiteralIPv6(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lookup := familyLookup{func(ctx context.Context, _, _ string) ([]net.IP, error) { <-ctx.Done(); return nil, ctx.Err() }}
	_, err := resolveSystemFamilies(ctx, lookup, "gateway.example")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("literal used DNS")
		return nil, errors.New("unexpected DNS")
	}}
	ips, err := resolveEPDG(context.Background(), resolver, "2001:db8::1")
	if err != nil || len(ips) != 1 || ips[0].IP.String() != "2001:db8::1" {
		t.Fatalf("literal %v %v", ips, err)
	}
}
func TestEPDGDefaultResolverDoesNotRequirePublicDNSFallback(t *testing.T) {
	resolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("configured DNS unavailable")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	before := time.Now()
	_, err := resolveEPDG(ctx, resolver, "gateway.example")
	if err == nil || !strings.Contains(err.Error(), "system ePDG DNS") || time.Since(before) > 500*time.Millisecond {
		t.Fatalf("default resolution unexpectedly used geographic fallback: %v", err)
	}
}
func TestEPDGExplicitGeographicLookupSupportsAAAAAndChecksType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "AAAA" {
			t.Error("not an AAAA query")
		}
		fmt.Fprint(w, `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::7"},{"type":28,"data":"2001:db8::7"},{"type":1,"data":"203.0.113.9"},{"type":28,"data":"ff02::1"},{"type":28,"data":"127.0.0.1"}]}`)
	}))
	defer server.Close()
	ips, err := resolveEPDGWithECSType(context.Background(), server.Client(), server.URL, "gateway.example", "", "AAAA")
	if err != nil || len(ips) != 1 || ips[0].IP.String() != "2001:db8::7" {
		t.Fatalf("AAAA %v %v", ips, err)
	}
}
func TestDirectEPDGInitialExchangeFallsBackAndPinsEndpoint(t *testing.T) {
	first, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	transport, err := newDirectUDPCandidates(ctx, transportConfig{Dialer: &net.Dialer{}, Timeout: 60 * time.Millisecond}, []*net.UDPAddr{first.LocalAddr().(*net.UDPAddr), second.LocalAddr().(*net.UDPAddr)})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	request := ikeHeader{InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, Exchange: exchangeIKEInit, Flags: flagInitiator}.marshal(nil)
	response := ikeHeader{InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, ResponderSPI: [8]byte{8, 7, 6, 5, 4, 3, 2, 1}, Exchange: exchangeIKEInit, Flags: flagResponse}.marshal(nil)
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		second.SetReadDeadline(time.Now().Add(time.Second))
		_, peer, err := second.ReadFromUDP(buffer)
		if err == nil {
			_, err = second.WriteToUDP(response, peer)
		}
		done <- err
	}()
	got, err := transport.RoundTrip(ctx, request)
	if err != nil || string(got) != string(response) || !transport.RemoteAddr().IP.Equal(second.LocalAddr().(*net.UDPAddr).IP) || transport.RemoteAddr().Port != second.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("fallback %v %v", transport.RemoteAddr(), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	first.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := first.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatal("first candidate not tried", err)
	}
	if _, err := transport.RoundTrip(ctx, request); err == nil {
		t.Fatal("second response should time out")
	}
	first.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if _, _, err := first.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatal("answered IKE endpoint was not pinned")
	}
}
func TestInitialNetworkFailureIsRetryableButAuthenticationIsNot(t *testing.T) {
	p, err := NewProvider(Config{Timeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	p.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return nil, deadlineError{}
	}
	request := vowifi.TunnelRequest{EPDG: "gateway.example", Identity: vowifi.SIMIdentity{IMSI: "234150000000000", HomeMCC: "234", HomeMNC: "15"}, AKA: &testAKAProvider{}}
	_, err = p.Start(context.Background(), request)
	if !vowifi.EPDGFallbackAllowed(err) {
		t.Fatalf("network error not classified: %v", err)
	}
}

type authNetworkFailureTransport struct{ *firstAuthCaptureTransport }

func (t *authNetworkFailureTransport) RoundTripExchange(ctx context.Context, packets [][]byte) ([][]byte, error) {
	header, _, _ := parseIKEPacket(packets[0])
	if header.Exchange == exchangeIKEAuth {
		return nil, deadlineError{}
	}
	return t.firstAuthCaptureTransport.RoundTripExchange(ctx, packets)
}
func TestEPDGDoesNotRetryNetworkFailureAfterAuthenticationStarted(t *testing.T) {
	provider, err := NewProvider(Config{Random: constantReader{value: 0x42}, Timeout: time.Second, Installer: unusedInstaller{}})
	if err != nil {
		t.Fatal(err)
	}
	capture := &authNetworkFailureTransport{&firstAuthCaptureTransport{t: t, wantEAPOnly: true}}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return capture, nil
	}
	_, err = provider.Start(context.Background(), vowifi.TunnelRequest{EPDG: "gateway.example", Identity: vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150000000000", HomeMCC: "234", HomeMNC: "15"}, AKA: &testAKAProvider{}})
	if err == nil || vowifi.EPDGFallbackAllowed(err) {
		t.Fatalf("authentication transport failure was downgraded to discovery: %v", err)
	}
}
func TestDirectEPDGCancellationInterruptsPendingUDPRead(t *testing.T) {
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	transport, err := newDirectUDP(context.Background(), transportConfig{Dialer: &net.Dialer{}, Timeout: 5 * time.Second}, peer.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		buffer := make([]byte, 4096)
		peer.SetReadDeadline(time.Now().Add(time.Second))
		peer.ReadFromUDP(buffer)
		cancel()
	}()
	before := time.Now()
	_, err = transport.RoundTrip(ctx, ikeHeader{InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, Exchange: exchangeIKEInit, Flags: flagInitiator}.marshal(nil))
	if !errors.Is(err, context.Canceled) || time.Since(before) > 250*time.Millisecond {
		t.Fatalf("cancellation waited for retransmission deadline: %v %v", time.Since(before), err)
	}
}
func TestDirectEPDGSupportsIPv6Socket(t *testing.T) {
	peer, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1")})
	if err != nil {
		t.Skip("IPv6 loopback unavailable")
	}
	defer peer.Close()
	transport, err := newDirectUDPCandidates(context.Background(), transportConfig{Dialer: &net.Dialer{}, Timeout: time.Second}, []*net.UDPAddr{peer.LocalAddr().(*net.UDPAddr)})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	request := ikeHeader{InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, Exchange: exchangeIKEInit, Flags: flagInitiator}.marshal(nil)
	response := ikeHeader{InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, ResponderSPI: [8]byte{8, 7, 6, 5, 4, 3, 2, 1}, Exchange: exchangeIKEInit, Flags: flagResponse}.marshal(nil)
	done := make(chan error, 1)
	go func() {
		peer.SetReadDeadline(time.Now().Add(time.Second))
		buffer := make([]byte, 4096)
		_, source, err := peer.ReadFromUDP(buffer)
		if err == nil {
			_, err = peer.WriteToUDP(response, source)
		}
		done <- err
	}()
	if _, err := transport.RoundTrip(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if transport.RemoteAddr().IP.To4() != nil {
		t.Fatal("IPv6 endpoint downgraded")
	}
}
func TestSOCKS5SourceAddressConstraintIsNotDropped(t *testing.T) {
	for _, address := range []net.Addr{&net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 1234}, &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1234}} {
		got, err := socksLocalAddress(address)
		if err != nil || got.Port != 1234 {
			t.Fatal("source constraint lost", err)
		}
		host, _, _ := net.SplitHostPort(address.String())
		if !got.IP.Equal(net.ParseIP(host)) {
			t.Fatal("source IP changed")
		}
	}
	if _, err := socksLocalAddress(&net.UnixAddr{Name: "not-an-IP", Net: "unix"}); err == nil {
		t.Fatal("unsupported local address silently ignored")
	}
}
