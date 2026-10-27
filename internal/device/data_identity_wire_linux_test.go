//go:build linux

package device

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// Use the pinned production QMI client against a one-slot DMS pool. A leaked
// identity CID must prevent the next request, as it did on the real modem.
func TestDataIdentityDMSClientReleasedOnWire(t *testing.T) {
	for _, releaseFailure := range []bool{false, true} {
		name := "repeated_success_and_read_errors"
		if releaseFailure {
			name = "release_error_is_returned"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "q")
			listener, err := net.Listen("unix", "@"+path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			counts := make(chan [4]int, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				allocations, releases, reads, active := 0, 0, 0, 0
				for {
					header := make([]byte, 6)
					_, err := io.ReadFull(conn, header)
					if err == io.EOF {
						counts <- [4]int{allocations, releases, reads, active}
						done <- nil
						return
					}
					if err != nil {
						done <- err
						return
					}
					size := int(binary.LittleEndian.Uint16(header[1:3])) + 1
					if size < 12 || size > 32768 {
						done <- fmt.Errorf("invalid QMUX size")
						return
					}
					raw := append(header, make([]byte, size-6)...)
					if _, err = io.ReadFull(conn, raw[6:]); err != nil {
						done <- err
						return
					}
					r, err := qmi.UnmarshalPacket(raw)
					if err != nil {
						done <- err
						return
					}
					tlvs := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}
					switch {
					case r.ServiceType == qmi.ServiceControl && r.MessageID == 0xFF00:
					case r.ServiceType == qmi.ServiceControl && r.MessageID == 0x0022:
						service := qmi.FindTLV(r.TLVs, 1)
						if service == nil || len(service.Value) != 1 || service.Value[0] != qmi.ServiceDMS || active != 0 {
							done <- fmt.Errorf("DMS pool exhausted or unexpected allocation")
							return
						}
						allocations++
						active = 1
						l := qmi.TLV{Type: 1, Value: []byte{qmi.ServiceDMS, 7}}
						tlvs = append(tlvs, l)
					case r.ServiceType == qmi.ServiceControl && r.MessageID == 0x0023:
						service := qmi.FindTLV(r.TLVs, 1)
						if service == nil || len(service.Value) != 2 || service.Value[0] != qmi.ServiceDMS || service.Value[1] != 7 || active != 1 {
							done <- fmt.Errorf("release did not target the original DMS CID")
							return
						}
						releases++
						active = 0
						if releaseFailure {
							tlvs[0].Value = []byte{1, 0, 3, 0}
						}
					case r.ServiceType == qmi.ServiceDMS && r.ClientID == 7 && r.MessageID == qmi.DMSGetDeviceSerialNumbers && active == 1:
						reads++
						if !releaseFailure && reads%3 == 0 {
							tlvs[0].Value = []byte{1, 0, 3, 0}
						} else {
							tlvs = append(tlvs, qmi.TLV{Type: 0x11, Value: []byte("999999999999999")})
						}
					default:
						done <- fmt.Errorf("unexpected QMI service/message %d/%04x", r.ServiceType, r.MessageID)
						return
					}
					response := (&qmi.Packet{ServiceType: r.ServiceType, ClientID: r.ClientID, TransactionID: r.TransactionID, MessageID: r.MessageID, TLVs: tlvs}).Marshal()
					response[3], response[6] = 0x80, 2
					if r.ServiceType == qmi.ServiceControl {
						response[6] = 1
					}
					if _, err := conn.Write(response); err != nil {
						done <- err
						return
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			opts := sharedSIMClientOptions()
			opts.ProxyPath = path
			opts.ProxyExecutable = filepath.Join(t.TempDir(), "no-external-helper")
			opts.DefaultRequestTimeout = time.Second
			opts.ReadDeadline = 25 * time.Millisecond
			client, err := qmi.NewClientWithOptions(ctx, "/dev/synthetic-never-opened", opts)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session := &productionQMIDataSession{client: client}
			attempts := 24
			if releaseFailure {
				attempts = 1
			}
			for i := 1; i <= attempts; i++ {
				identity, err := session.GetIMEI(ctx)
				wantError := releaseFailure || i%3 == 0
				if (err != nil) != wantError || !wantError && identity != "999999999999999" {
					t.Fatalf("identity attempt %d: expected error=%t, got error=%v", i, wantError, err)
				}
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := <-counts; got != [4]int{attempts, attempts, attempts, 0} {
				t.Fatalf("allocations/releases/reads/remaining=%v", got)
			}
		})
	}
}
