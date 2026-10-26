//go:build windows

package vowifi

import "golang.org/x/sys/windows"

// Go's Windows POSIX errno constants differ from the Winsock errno values
// returned by net.Conn. This classifies only the transport cause; the caller
// still requires provider proof of no SIP response and completed cleanup.
func imsPlatformNetworkFailure(err error) bool {
	return err == windows.WSAECONNABORTED || err == windows.WSAECONNRESET || err == windows.WSAECONNREFUSED || err == windows.WSAENETUNREACH || err == windows.WSAEHOSTUNREACH
}
