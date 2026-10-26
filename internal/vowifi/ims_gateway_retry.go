package vowifi

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

var ErrEPDGAddressesExhausted = errors.New("vowifi: all ePDG addresses were excluded for this authentication home")

// IMSTransportError is proof from the IMS provider: no matching SIP response
// was received, and its abandoned session was completely removed. A generic
// timeout alone cannot authorize rebuilding a gateway after authentication.
type IMSTransportError struct{ Cause error }

func (e *IMSTransportError) Error() string {
	if e == nil || e.Cause == nil {
		return "IMS access transport unavailable"
	}
	return "IMS access transport unavailable: " + e.Cause.Error()
}
func (e *IMSTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IMSFallbackAllowed(err error) bool {
	return everyErrorCause(err, func(cause error) bool {
		e, ok := cause.(*IMSTransportError)
		return ok && e != nil && imsNetworkFailure(e.Cause)
	})
}

// Joined errors authorize fallback only when every branch supplies proof.
func everyErrorCause(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	if match(err) {
		return true
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		causes := many.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !everyErrorCause(cause, match) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return everyErrorCause(one.Unwrap(), match)
	}
	return false
}

func imsNetworkFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		causes := many.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !imsNetworkFailure(cause) {
				return false
			}
		}
		return true
	}
	if err == context.DeadlineExceeded || err == io.EOF || err == io.ErrUnexpectedEOF ||
		err == syscall.ECONNREFUSED || err == syscall.ECONNRESET || err == syscall.ECONNABORTED || err == syscall.ENETUNREACH || err == syscall.EHOSTUNREACH || err == syscall.EPIPE || imsPlatformNetworkFailure(err) {
		return true
	}
	if dns, ok := err.(*net.DNSError); ok && (dns.Timeout() || dns.IsNotFound || dns.IsTemporary) {
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return imsNetworkFailure(one.Unwrap())
	}
	if network, ok := err.(net.Error); ok {
		return network.Timeout()
	}
	return false
}
