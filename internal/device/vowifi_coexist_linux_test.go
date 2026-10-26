//go:build linux

package device

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func TestNativeQMICoexistProductionOptionsNeverSendCTLSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.sock")
	listener, err := net.Listen("unix", "@"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		read := func() ([]byte, error) {
			header := make([]byte, 6)
			if _, err := io.ReadFull(conn, header); err != nil {
				return nil, err
			}
			length := int(binary.LittleEndian.Uint16(header[1:3])) + 1
			if length < 12 || length > 4096 {
				return nil, fmt.Errorf("bad QMUX length: %d", length)
			}
			frame := append(header, make([]byte, length-6)...)
			_, err := io.ReadFull(conn, frame[6:])
			return frame, err
		}
		frame, err := read()
		if err != nil {
			done <- err
			return
		}
		if frame[4] != 0 || binary.LittleEndian.Uint16(frame[8:10]) != qmi.CTLInternalProxyOpen {
			done <- errors.New("expected proxy-open as the first request")
			return
		}
		response := []byte{1, 18, 0, 0x80, 0, 0, 1, frame[7], frame[8], frame[9], 7, 0, 2, 4, 0, 0, 0, 0, 0}
		if _, err := conn.Write(response); err != nil {
			done <- err
			return
		}
		frame, err = read()
		if !errors.Is(err, io.EOF) {
			done <- fmt.Errorf("shared open emitted a forbidden additional CTL request: frame=%x err=%v", frame, err)
			return
		}
		done <- nil
	}()
	opts := sharedSIMClientOptions()
	opts.ProxyPath = path
	// Even a fixture connection failure must never start the machine's real
	// qmi-proxy. This non-existent private executable makes that fail closed.
	opts.ProxyExecutable = filepath.Join(t.TempDir(), "no-proxy-process")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := qmi.NewClientWithOptions(ctx, "/dev/cdc-wdm-coexist-fixture", opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
