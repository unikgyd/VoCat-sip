//go:build !linux

package hostims

import "context"

func PrepareAccess(context.Context, *BearerLease) (*PreparedAccess, error) {
	return nil, ErrHostAccessUnavailable
}
