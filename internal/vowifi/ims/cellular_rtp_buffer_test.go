package ims

import (
	"context"
	"net"
	"testing"
)

func TestCellularRTPRemoteChangeDiscardsPreviousPeerPartialPCM(t *testing.T) {
	for _, next := range []struct {
		host string
		port int
	}{{"127.0.0.1", 10001}, {"127.0.0.2", 10000}} {
		media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := media.configureRemote(rtpTestSDP("127.0.0.1", 10000)); err != nil {
			t.Fatal(err)
		}
		if err := media.WritePCM(make([]int16, 80)); err != nil {
			t.Fatal(err)
		}
		if err := media.configureRemote(rtpTestSDP(next.host, next.port)); err != nil {
			t.Fatal(err)
		}
		media.writeMu.Lock()
		pending := len(media.pending)
		media.writeMu.Unlock()
		if pending != 0 {
			t.Fatal("previous peer's partial audio survived target switch")
		}
		media.Close()
		if err := media.DrainCellularRoutes(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
