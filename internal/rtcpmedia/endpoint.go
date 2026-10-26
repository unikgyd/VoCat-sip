// Package rtcpmedia implements the bounded, unicast RTCP side of a call.
// RTP validation and media ownership remain with the original call transport.
package rtcpmedia

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// Endpoint interprets one media-level RFC 3605 attribute. An absent attribute
// uses RTP+1. This implementation negotiates separate RTP/RTCP sockets.
func Endpoint(attribute string, fallback net.IP, rtpPort int) (*net.UDPAddr, error) {
	port, ip := rtpPort+1, fallback
	if attribute != "" {
		fields := strings.Fields(attribute)
		if len(fields) != 1 && len(fields) != 4 {
			return nil, errors.New("invalid RTCP attribute")
		}
		var err error
		port, err = strconv.Atoi(fields[0])
		if err != nil {
			return nil, errors.New("invalid RTCP port")
		}
		if len(fields) == 4 {
			ip = net.ParseIP(fields[3])
			if fields[1] != "IN" || ip == nil || fields[2] != "IP4" && fields[2] != "IP6" ||
				fields[2] == "IP4" && ip.To4() == nil || fields[2] == "IP6" && ip.To4() != nil {
				return nil, errors.New("invalid RTCP address")
			}
		}
	}
	if port < 1 || port > 65535 || ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) ||
		fallback == nil || (ip.To4() == nil) != (fallback.To4() == nil) || ip.Equal(fallback) && port == rtpPort {
		return nil, errors.New("unusable separate RTCP endpoint")
	}
	return &net.UDPAddr{IP: append(net.IP(nil), ip...), Port: port}, nil
}
