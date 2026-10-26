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

// Execute the actual production method and pinned UIM client over a private
// Unix proxy. The fake endpoint rejects OPL, NAS, writes and AUTHENTICATE.
func TestReaderHomeProductionQMIWire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q")
	listener, err := net.Listen("unix", "@"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	counts := make(chan [2]int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		apdus, closes := 0, 0
		for {
			header := make([]byte, 6)
			_, err := io.ReadFull(conn, header)
			if err == io.EOF {
				counts <- [2]int{apdus, closes}
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
			size := int(binary.LittleEndian.Uint16(header[1:3])) + 1
			if size < 12 || size > 32768 {
				done <- fmt.Errorf("invalid test QMUX size")
				return
			}
			raw := make([]byte, size)
			copy(raw, header)
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
			if r.ServiceType == qmi.ServiceControl {
				switch r.MessageID {
				case 0xFF00, 0x0023:
				case 0x0022:
					tlvs = append(tlvs, qmi.TLV{Type: 1, Value: []byte{qmi.ServiceUIM, 7}})
				default:
					done <- fmt.Errorf("unexpected CTL message %04x", r.MessageID)
					return
				}
			} else if r.ServiceType == qmi.ServiceUIM {
				switch r.MessageID {
				case qmi.UIMReadTransparent:
					file := qmi.FindTLV(r.TLVs, 2)
					if file == nil || len(file.Value) < 2 || binary.LittleEndian.Uint16(file.Value) != 0x6F07 {
						done <- fmt.Errorf("production requested a non-IMSI transparent file (OPL/default shortcut)")
						return
					}
					tlvs = append(tlvs, qmi.TLV{Type: 0x11, Value: []byte{9, 0, 8, 0x99, 0x99, 0x21, 0x13, 0x32, 0x54, 0x76, 0x98}})
				case qmi.UIMGetCardStatus:
					status := make([]byte, 15)
					status[8], status[9], status[14] = 1, 1, 1
					status = append(status, 2, 7, 0, 0, 0, 0, 7, 0xA0, 0, 0, 0, 0x87, 0x10, 2)
					status = append(status, 0, 2, 3, 10, 2, 3, 10)
					tlvs = append(tlvs, qmi.TLV{Type: 0x10, Value: status})
				case qmi.UIMOpenLogicalChannel:
					tlvs = append(tlvs, qmi.TLV{Type: 0x10, Value: []byte{3}})
				case qmi.UIMSendAPDU:
					command := qmi.FindTLV(r.TLVs, 2)
					if command == nil || len(command.Value) < 4 {
						done <- fmt.Errorf("missing APDU")
						return
					}
					cmd := command.Value[2:]
					var body []byte
					if apdus == 0 && len(cmd) == 7 && cmd[1] == 0xA4 && cmd[5] == 0x6F && cmd[6] == 7 {
						body = []byte{0x90, 0}
					} else if apdus == 1 && len(cmd) == 5 && cmd[1] == 0xB0 && cmd[4] == 9 {
						body = []byte{8, 0x99, 0x99, 0x21, 0x13, 0x32, 0x54, 0x76, 0x98, 0x90, 0}
					} else if apdus == 2 && len(cmd) == 7 && cmd[1] == 0xA4 && cmd[5] == 0x6F && cmd[6] == 0xAD {
						body = []byte{0x90, 0}
					} else if apdus == 3 && len(cmd) == 5 && cmd[1] == 0xB0 && cmd[4] == 4 {
						body = []byte{0, 0, 0, 3, 0x90, 0}
					} else {
						done <- fmt.Errorf("unexpected/non-read APDU")
						return
					}
					apdus++
					value := binary.LittleEndian.AppendUint16(nil, uint16(len(body)))
					value = append(value, body...)
					tlvs = append(tlvs, qmi.TLV{Type: 0x10, Value: value})
				case qmi.UIMCloseLogicalChannel:
					closes++
				default:
					done <- fmt.Errorf("unexpected UIM message %04x", r.MessageID)
					return
				}
			} else {
				done <- fmt.Errorf("unexpected service %d", r.ServiceType)
				return
			}
			response := (&qmi.Packet{ServiceType: r.ServiceType, ClientID: r.ClientID, TransactionID: r.TransactionID, MessageID: r.MessageID, TLVs: tlvs}).Marshal()
			response[3] = 0x80
			response[6] = 2
			if r.ServiceType == qmi.ServiceControl {
				response[6] = 1
			}
			if _, err = conn.Write(response); err != nil {
				done <- err
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	client, err := qmi.NewClientWithOptions(ctx, "/dev/vowin-synthetic-never-opened", qmi.ClientOptions{UseProxy: true, ProxyPath: path, ProxyExecutable: filepath.Join(filepath.Dir(path), "no-external-helper"), DefaultRequestTimeout: time.Second, ReadDeadline: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s := &productionQMIRadioSession{client: client}
	mcc, mnc, err := s.GetNativeMCCMNC(ctx)
	closeErr := s.Close()
	if err != nil || closeErr != nil || mcc != "999" || mnc != "123" {
		t.Fatalf("actual production UIM path: %s-%s err=%v close=%v", mcc, mnc, err, closeErr)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := <-counts; got != [2]int{4, 1} {
		t.Fatalf("APDUs/channel closes=%v", got)
	}
}
