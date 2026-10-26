//go:build !linux

package hostims

import "context"

func reopenPendingNetwork(context.Context, *PendingBearerCleanup) (interface{ Close(context.Context) error }, error) {
	return nil, ErrUncertain
}
