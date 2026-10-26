//go:build windows

package vowifi

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestIMSFallbackWinsockStillRequiresProviderProofAndCleanup(t *testing.T) {
	for _, errno := range []syscall.Errno{windows.WSAECONNABORTED, windows.WSAECONNRESET, windows.WSAECONNREFUSED, windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH} {
		err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", errno)}
		if IMSFallbackAllowed(err) {
			t.Fatal("raw OS error authorized a gateway rebuild", errno)
		}
		if !IMSFallbackAllowed(&IMSTransportError{Cause: err}) {
			t.Fatal("provider-certified transport silence was lost", errno)
		}
		if IMSFallbackAllowed(errors.Join(&IMSTransportError{Cause: err}, context.Canceled)) {
			t.Fatal("cancellation was overridden", errno)
		}
	}
}
