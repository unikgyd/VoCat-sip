package ims

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCellularSocketBindFailureIsClosedAndRedacted(t *testing.T) {
	for _, operation := range []string{"dial-tcp", "dial-udp", "listen-tcp", "listen-udp"} {
		t.Run(operation, func(t *testing.T) {
			bearer := &fakeCellularBearer{bindErr: errors.New("secret SIM hardware information")}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			switch operation {
			case "dial-tcp", "dial-udp":
				var connection net.Conn
				connection, err = dialSIPOnAccess(ctx, strings.TrimPrefix(operation, "dial-"), "127.0.0.1", 0, "127.0.0.1:9", true, bearer)
				if connection != nil {
					connection.Close()
					t.Fatal("failed binder produced a socket")
				}
			case "listen-tcp":
				var listener *net.TCPListener
				listener, err = listenAccessTCP(ctx, net.IPv4(127, 0, 0, 1), 0, bearer)
				if listener != nil {
					listener.Close()
					t.Fatal("failed binder produced a listener")
				}
			case "listen-udp":
				var connection *net.UDPConn
				connection, err = listenAccessUDP(ctx, net.IPv4(127, 0, 0, 1), 0, bearer)
				if connection != nil {
					connection.Close()
					t.Fatal("failed binder produced a packet socket")
				}
			}
			if !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "secret") || len(bearer.bindings) != 1 {
				t.Fatalf("binder bypass/error disclosure: %v, hooks=%d", err, len(bearer.bindings))
			}
		})
	}
}

func TestCellularDialerComposesExistingControlHooks(t *testing.T) {
	for _, contextual := range []bool{false, true} {
		bearer := &fakeCellularBearer{}
		called := 0
		dialer := net.Dialer{Control: func(string, string, syscall.RawConn) error { called++; return nil }}
		if contextual {
			dialer.Control = func(string, string, syscall.RawConn) error {
				t.Fatal("Control must not override ControlContext")
				return nil
			}
			dialer.ControlContext = func(context.Context, string, string, syscall.RawConn) error { called++; return nil }
		}
		bindCellularDialer(&dialer, bearer)
		// A real raw socket ensures the hook can access its descriptor.
		listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := listener.SyscallConn()
		err = dialer.ControlContext(context.Background(), "udp4", "127.0.0.1:9", raw)
		listener.Close()
		if err != nil || called != 1 || len(bearer.bindings) != 1 {
			t.Fatalf("hook composition: %v %d", err, called)
		}
	}
}

func TestCellularSocketSuccessfulHooksKeepExplicitSource(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			bearer := &fakeCellularBearer{}
			if transport == "tcp" {
				server, err := listenAccessTCP(ctx, net.IPv4(127, 0, 0, 1), 0, bearer)
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				connection, err := dialSIPOnAccess(ctx, "tcp", "127.0.0.1", 0, server.Addr().String(), true, bearer)
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				accepted, err := server.AcceptTCP()
				if err != nil {
					t.Fatal(err)
				}
				defer accepted.Close()
				if !addressIP(connection.LocalAddr()).Equal(net.IPv4(127, 0, 0, 1)) {
					t.Fatal("source lost")
				}
			} else {
				server, err := listenAccessUDP(ctx, net.IPv4(127, 0, 0, 1), 0, bearer)
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				connection, err := dialSIPOnAccess(ctx, "udp", "127.0.0.1", 0, server.LocalAddr().String(), false, bearer)
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				if !addressIP(connection.LocalAddr()).Equal(net.IPv4(127, 0, 0, 1)) {
					t.Fatal("source lost")
				}
			}
			if len(bearer.bindings) != 2 {
				t.Fatalf("missing hook: %#v", bearer.bindings)
			}
		})
	}
}

func TestCellularAcceptedSocketRechecksBindingAndSource(t *testing.T) {
	ctx := context.Background()
	listener, err := listenAccessTCP(ctx, net.IPv4(127, 0, 0, 1), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	bearer := &fakeCellularBearer{}
	request := testCellularRequest()
	request.Cellular = bearer
	session := &Session{request: request, conn: accepted,
		securityProposal: securityProposal{portServer: listener.Addr().(*net.TCPAddr).Port}}
	if err := session.bindAcceptedCellularTCP(accepted); err != nil || len(bearer.bindings) != 1 {
		t.Fatal(err)
	}
	bearer.bindErr = errors.New("removed interface")
	if err := session.bindAcceptedCellularTCP(accepted); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("accepted binding loss ignored")
	}
	session.securityProposal.portServer++
	if err := session.bindAcceptedCellularTCP(accepted); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("wrong local port accepted")
	}
}

func TestProviderCellularBindingFailurePreventsREGISTERAndAKA(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var address string
			if transport == "tcp" {
				listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.Addr().String()
			} else {
				listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.LocalAddr().String()
				defer func() {
					listener.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
					if n, _, _ := listener.ReadFromUDP(make([]byte, 4096)); n != 0 {
						t.Fatal("REGISTER sent after failed binding")
					}
				}()
			}
			request := testCellularRequest()
			bearer := request.Cellular.(*fakeCellularBearer)
			bearer.evidence.LocalIPv4, bearer.evidence.LocalIPv6 = "127.0.0.1", ""
			bearer.evidence.PCSCF = []string{address}
			bearer.bindErr = errors.New("private hardware detail")
			installer, aka := &fakeIPSecInstaller{}, &recordingAKA{}
			provider, err := NewProvider(aka, Config{Transport: transport, SecurityMode: SecurityRequired, IPSecInstaller: installer})
			if err != nil {
				t.Fatal(err)
			}
			if session, err := provider.Start(ctx, request); session != nil || !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "private") {
				t.Fatalf("provider did not fail closed: %v", err)
			}
			if len(bearer.bindings) != 1 || len(aka.challenges) != 0 || len(installer.installed()) != 0 {
				t.Fatal("authentication continued after binding failure")
			}
		})
	}
}

func TestCellularDefaultProtectedPortProbesAreAlsoBound(t *testing.T) {
	bearer := &fakeCellularBearer{}
	proposal, err := newSecurityProposalOnAccess(context.Background(), net.IPv4(127, 0, 0, 1), 0, 0, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if !validProtectedPort(proposal.portClient) || !validProtectedPort(proposal.portServer) || proposal.portClient == proposal.portServer {
		t.Fatal("invalid automatic ports")
	}
	if len(bearer.bindings) < 4 || len(bearer.bindings)%2 != 0 {
		t.Fatalf("automatic probe bypass: %#v", bearer.bindings)
	}
	for index, binding := range bearer.bindings {
		want := "tcp"
		if index%2 == 1 {
			want = "udp"
		}
		if !strings.HasPrefix(binding.network, want) {
			t.Fatal("unbound/misordered port probe")
		}
	}
	bearer.bindings = nil
	bearer.bindErr = errors.New("removed card")
	if _, err := newSecurityProposalOnAccess(context.Background(), net.IPv4(127, 0, 0, 1), 0, 0, bearer); !errors.Is(err, ErrCellularAccess) || len(bearer.bindings) != 1 {
		t.Fatal("failed capability was retried/ignored", err)
	}
}

func TestCellularSocketsDisableMultipathTCPWithoutChangingVoWiFi(t *testing.T) {
	t.Setenv("GODEBUG", "multipathtcp=1")
	dialer := net.Dialer{}
	bindCellularDialer(&dialer, &fakeCellularBearer{})
	config := cellularListenConfig(context.Background(), &fakeCellularBearer{})
	if dialer.MultipathTCP() || config.MultipathTCP() {
		t.Fatal("MPTCP escaped a single owned IMS bearer")
	}
	wifi := net.Dialer{}
	bindCellularDialer(&wifi, nil)
	wifiListen := cellularListenConfig(context.Background(), nil)
	if !wifi.MultipathTCP() || !wifiListen.MultipathTCP() {
		t.Fatal("cellular policy changed VoWiFi's network behavior")
	}
}
