package ims

import (
	"context"
	"errors"
	"net"
	"strings"

	"vocat/internal/rtcpmedia"
	"vocat/internal/vowifi"
)

func audioControlEndpoint(body []byte, address net.IP, port int) (*net.UDPAddr, error) {
	selected, err := selectAudioSDP(body)
	if err != nil {
		return nil, err
	}
	attribute, seen, media := "", false, false
	for _, raw := range strings.Split(string(selected), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "m=") {
			media = true
		}
		if line == "a=rtcp-mux-only" {
			return nil, errors.New("ims: separate RTCP transport is required")
		}
		if strings.HasPrefix(line, "a=rtcp:") {
			if !media || seen {
				return nil, errors.New("ims: invalid or duplicate RTCP attribute")
			}
			seen, attribute = true, strings.TrimSpace(strings.TrimPrefix(line, "a=rtcp:"))
			if attribute == "" {
				return nil, errors.New("ims: empty RTCP attribute")
			}
		}
	}
	return rtcpmedia.Endpoint(attribute, address, port)
}

// Closing a call is nonblocking on the SIP path. Its bearer routes remain
// owned until the final RTCP send and socket workers have actually exited.
type controlDrainRoute struct {
	route vowifi.CellularMediaRoute
	done  <-chan struct{}
}

func (r *controlDrainRoute) Close(ctx context.Context) error {
	select {
	case <-r.done:
		return r.route.Close(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}
