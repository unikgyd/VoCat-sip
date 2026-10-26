package ims

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestIMSGatewayRealSIPOnlySilenceAllowsTransportAndGatewayFallback(t *testing.T) {
	for _, code := range []int{0, 100, 403, -1} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var udp *net.UDPConn
			var tcp *net.TCPListener
			var err error
			for attempt := 0; attempt < 32; attempt++ {
				tcp, err = net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				udp, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tcp.Addr().(*net.TCPAddr).Port})
				if err == nil {
					break
				}
				_ = tcp.Close()
				if !errors.Is(err, syscall.EADDRINUSE) {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal("no free paired TCP/UDP test port", err)
			}
			defer udp.Close()
			defer tcp.Close()
			var tcpCalls atomic.Int32
			doneTCP := make(chan struct{})
			go func() {
				defer close(doneTCP)
				for {
					c, err := tcp.Accept()
					if err != nil {
						return
					}
					tcpCalls.Add(1)
					_ = c.Close()
				}
			}()
			doneUDP := make(chan error, 1)
			go func() {
				b := make([]byte, 65535)
				n, peer, err := udp.ReadFromUDP(b)
				if err != nil {
					doneUDP <- err
					return
				}
				_, h, err := parseTestRequest(b[:n])
				if err != nil {
					doneUDP <- err
					return
				}
				if code != 0 {
					response := []byte("malformed SIP data")
					if code > 0 {
						response = testResponse(code, "Synthetic", h["call-id"], h["cseq"], nil)
					}
					_, err = udp.WriteToUDP(response, peer)
				}
				doneUDP <- err
			}()
			aka := &recordingAKA{}
			p, err := NewProvider(aka, Config{Transport: "udp", AutoTransportFallback: true, TransactionTimeout: time.Second, SecurityMode: SecurityDisabled, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			session, err := p.Start(ctx, vowifi.IMSRequest{Identity: vowifi.SIMIdentity{ICCID: "8901000000000000000", IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"}, Tunnel: evidenceTunnel{vowifi.TunnelEvidence{Established: true, LocalIPv4: "127.0.0.1", PCSCF: []string{udp.LocalAddr().String()}}}})
			_ = tcp.Close()
			<-doneTCP
			if serverErr := <-doneUDP; serverErr != nil {
				t.Fatal(serverErr)
			}
			want := int32(0)
			if code == 0 {
				want = 1
			}
			if err == nil || session != nil || tcpCalls.Load() != want || vowifi.IMSFallbackAllowed(err) != (code == 0) || len(aka.challenges) != 0 {
				t.Fatalf("code=%d error=%v TCP=%d AKA=%d", code, err, tcpCalls.Load(), len(aka.challenges))
			}
		})
	}
}
