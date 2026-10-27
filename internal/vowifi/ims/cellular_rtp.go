package ims

import (
	"context"
	"io"
	"net"
	"net/netip"
	"time"

	"vocat/internal/vowifi"
)

const cellularRTPTimeout = 15 * time.Second
const maxRetiredCellularRoutes = 32
const cellularRouteCleanupPasses = 3
const cellularRouteCleanupRetryDelay = 500 * time.Millisecond

type cellularRTPRoute struct {
	route vowifi.CellularMediaRoute
	err   error // cleanupMu protects this; raw owner errors never leave this boundary
}

func (media *rtpMedia) beginRemoteChange(parent context.Context) (context.Context, func(), error) {
	select {
	case <-media.closed:
		return nil, nil, io.EOF
	default:
	}
	ctx, cancel := context.WithTimeout(parent, cellularRTPTimeout)
	stop := context.AfterFunc(media.lifetime, cancel)
	select {
	case <-ctx.Done():
		stop()
		cancel()
		return nil, nil, ctx.Err()
	case <-media.configureGate:
	}
	return ctx, func() { stop(); cancel(); media.configureGate <- struct{}{} }, nil
}

func (media *rtpMedia) acquireRemoteRoute(ctx context.Context, remote net.IP) (vowifi.CellularMediaRoute, error) {
	if media.network == nil {
		return nil, nil
	}
	local := addressIP(media.conn.LocalAddr())
	localAddress, localOK := netip.AddrFromSlice(local)
	remoteAddress, remoteOK := netip.AddrFromSlice(remote)
	if !localOK || !remoteOK || local.IsUnspecified() || localAddress.Unmap().Is4() != remoteAddress.Unmap().Is4() {
		return nil, ErrCellularAccess
	}
	media.cleanupMu.Lock()
	// Reserve space for both active RTP/RTCP leases and a late new pair
	// racing Close. Never let failed retirement grow an unbounded queue.
	full := len(media.retiredRoutes) >= maxRetiredCellularRoutes-4
	media.cleanupMu.Unlock()
	if full {
		return nil, ErrCellularAccess
	}
	route, err := media.network.AcquireMediaRoute(ctx, localAddress.Unmap(), remoteAddress.Unmap())
	if err != nil || route == nil || ctx.Err() != nil {
		if route != nil {
			media.retireCellularRoute(route)
		}
		select {
		case <-media.closed:
			return nil, io.EOF
		default:
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrCellularAccess
	}
	return route, nil
}

func (media *rtpMedia) retireCellularRoute(route vowifi.CellularMediaRoute) {
	media.cleanupMu.Lock()
	media.retiredRoutes = append(media.retiredRoutes, &cellularRTPRoute{route: route})
	media.cleanupMu.Unlock()
	media.startCellularRouteCleanup()
}

// One worker per media makes at most three passes under one shared deadline.
// A transient original-owner rejection must not strand an ended call's routes
// until the entire IMS session stops. Every retry uses the same retained handle;
// failed handles remain owned after the bounded worker finishes.
func (media *rtpMedia) startCellularRouteCleanup() {
	media.cleanupMu.Lock()
	if media.cleanupRunning || len(media.retiredRoutes) == 0 {
		media.cleanupMu.Unlock()
		return
	}
	media.cleanupRunning = true
	done := make(chan struct{})
	media.cleanupDone = done
	media.cleanupMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cellularRTPTimeout)
		defer cancel()
		for pass := 0; pass < cellularRouteCleanupPasses && ctx.Err() == nil; pass++ {
			if pass > 0 {
				timer := time.NewTimer(time.Duration(pass) * cellularRouteCleanupRetryDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
			}
			attempted := make(map[*cellularRTPRoute]bool)
			for ctx.Err() == nil && len(attempted) < maxRetiredCellularRoutes {
				media.cleanupMu.Lock()
				var current *cellularRTPRoute
				for _, entry := range media.retiredRoutes {
					if !attempted[entry] {
						current = entry
						break
					}
				}
				media.cleanupMu.Unlock()
				if current == nil {
					// Check and publish the end of this pass under the same lock as
					// retireCellularRoute, so a newly queued handle cannot be stranded.
					media.cleanupMu.Lock()
					for _, entry := range media.retiredRoutes {
						if !attempted[entry] {
							current = entry
							break
						}
					}
					if current == nil {
						finished := len(media.retiredRoutes) == 0 || pass == cellularRouteCleanupPasses-1
						if finished {
							media.cleanupRunning = false
							close(done)
						}
						media.cleanupMu.Unlock()
						if finished {
							return
						}
						break
					}
					media.cleanupMu.Unlock()
				}
				attempted[current] = true
				err := current.route.Close(ctx)
				media.cleanupMu.Lock()
				current.err = err
				if err == nil {
					for index, entry := range media.retiredRoutes {
						if entry == current {
							media.retiredRoutes = append(media.retiredRoutes[:index], media.retiredRoutes[index+1:]...)
							break
						}
					}
				}
				media.cleanupMu.Unlock()
			}
		}
		media.cleanupMu.Lock()
		media.cleanupRunning = false
		close(done)
		media.cleanupMu.Unlock()
	}()
}

func (media *rtpMedia) cellularRouteCleanupError() error {
	media.cleanupMu.Lock()
	defer media.cleanupMu.Unlock()
	for _, route := range media.retiredRoutes {
		if route.err != nil {
			return ErrCellularAccess
		}
	}
	return nil
}

func (media *rtpMedia) hasCellularRouteCleanup() bool {
	if media.control != nil && media.control.Closing() {
		select {
		case <-media.control.Done():
		default:
			return true
		}
	}
	if media.network != nil {
		select {
		case <-media.configureGate:
			media.configureGate <- struct{}{}
		default:
			return true // a late acquisition may still hand back a route
		}
	}
	media.cleanupMu.Lock()
	defer media.cleanupMu.Unlock()
	return len(media.retiredRoutes) != 0 || media.cleanupRunning
}

// DrainCellularRoutes joins retired routes outside callMu/signaling locks.
// After Close it also joins the final RTCP send before releasing its routes.
// An active dialog can drain a canceled SDP change without ending its RTCP.
func (media *rtpMedia) DrainCellularRoutes(ctx context.Context) error {
	if media.control != nil && media.control.Closing() {
		select {
		case <-media.control.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Join an in-flight acquisition before inspecting retirement. Close first
	// canceled it; a late result is queued before configureGate is released.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-media.configureGate:
		media.configureGate <- struct{}{}
	}
	media.startCellularRouteCleanup()
	media.cleanupMu.Lock()
	done := media.cleanupDone
	media.cleanupMu.Unlock()
	if done != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
	if media.hasCellularRouteCleanup() {
		return ErrCellularAccess
	}
	return nil
}
