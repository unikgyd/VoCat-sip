package ims

import "context"

// DrainCellularMedia retries retained route cleanup without reopening SIP or
// touching modem mode. Failure is never reported as successful shutdown. The
// bearer owner must retain/fence its own resources until its final Close too;
// a caller discarding this Session cannot transfer ownership to the kernel.
func (session *Session) DrainCellularMedia(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, cellularRTPTimeout)
	defer cancel()
	session.callMu.Lock()
	var media []*rtpMedia
	for _, call := range session.calls {
		if call.media != nil && call.media.network != nil {
			media = append(media, call.media)
		}
	}
	session.callMu.Unlock()
	failed := false
	for _, item := range media {
		if err := item.DrainCellularRoutes(ctx); err != nil {
			failed = true
		}
	}
	if failed {
		return ErrCellularAccess
	}
	return nil
}

func (session *Session) drainCellularMediaOnClose() error {
	// SIP deregistration may have consumed the caller's deadline. Local owned
	// resource cleanup gets one fresh shared budget, not 15 seconds per call.
	return session.DrainCellularMedia(context.Background())
}
