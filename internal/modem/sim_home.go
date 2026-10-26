package modem

import "errors"

// ErrHomeMNCLengthUnavailable means that a local, completed EF_AD read did not
// establish a length. It never supplies a default MNC or a serving/OPL PLMN.
var ErrHomeMNCLengthUnavailable = errors.New("SIM home MNC length is unavailable")

// OnlyHomeMNCLengthUnavailable prevents errors.Join(optional, cleanup/fatal)
// from accidentally being treated as an optional-file failure.
func OnlyHomeMNCLengthUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrHomeMNCLengthUnavailable {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !OnlyHomeMNCLengthUnavailable(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return OnlyHomeMNCLengthUnavailable(wrapped.Unwrap())
	}
	return false
}
