package sipgateway

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Only legacy fixtures which exercise actual deliverInbox opt their own socket
// in. Raw wire/deadline/negative readiness readers retain strict packet access;
// this is not a background responder and cannot swallow their NoUDP evidence.
type smsPeerReadinessTestAdapter struct{ gateway string }

var smsPeerReadinessTestAdapters = struct {
	sync.Mutex
	peers map[*net.UDPConn]*smsPeerReadinessTestAdapter
}{peers: map[*net.UDPConn]*smsPeerReadinessTestAdapter{}}

func smsPeerReadinessOptIn(t *testing.T, connection *net.UDPConn, gateway string) {
	t.Helper()
	adapter := &smsPeerReadinessTestAdapter{gateway: gateway}
	smsPeerReadinessTestAdapters.Lock()
	smsPeerReadinessTestAdapters.peers[connection] = adapter
	smsPeerReadinessTestAdapters.Unlock()
	t.Cleanup(func() {
		smsPeerReadinessTestAdapters.Lock()
		if smsPeerReadinessTestAdapters.peers[connection] == adapter {
			delete(smsPeerReadinessTestAdapters.peers, connection)
		}
		smsPeerReadinessTestAdapters.Unlock()
	})
}

func smsPeerReadinessRefreshOptIn(t *testing.T, peerAddress, gateway string) {
	t.Helper()
	var connections []*net.UDPConn
	smsPeerReadinessTestAdapters.Lock()
	for connection := range smsPeerReadinessTestAdapters.peers {
		if connection.LocalAddr().String() == peerAddress {
			connections = append(connections, connection)
		}
	}
	smsPeerReadinessTestAdapters.Unlock()
	for _, connection := range connections {
		smsPeerReadinessOptIn(t, connection, gateway)
	}
}

func readOptedInFixedPeerMessage(connection *net.UDPConn) (*sip.Request, *net.UDPAddr, error) {
	// One absolute read budget covers every readiness packet and the MESSAGE.
	// Neither a probe retransmission nor an unrelated packet renews this window.
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, nil, err
	}
	smsPeerReadinessTestAdapters.Lock()
	adapter := smsPeerReadinessTestAdapters.peers[connection]
	smsPeerReadinessTestAdapters.Unlock()
	buffer := make([]byte, 16*1024)
	for {
		n, source, err := connection.ReadFromUDP(buffer)
		if err != nil {
			return nil, nil, err
		}
		message, err := registrarParser().ParseSIP(buffer[:n])
		if err != nil {
			return nil, nil, err
		}
		request, ok := message.(*sip.Request)
		if !ok {
			return nil, nil, fmt.Errorf("expected MESSAGE")
		}
		if request.Method == sip.OPTIONS && adapter != nil {
			if err := smsPeerReadinessValidateLegacyProbe(request, source, adapter); err != nil {
				return nil, nil, err
			}
			if err := fixedPeerResponse(connection, request, source, 200); err != nil {
				return nil, nil, err
			}
			continue
		}
		if request.Method != sip.MESSAGE {
			return nil, nil, fmt.Errorf("expected MESSAGE")
		}
		request.SetSource(source.String())
		request.SetTransport("UDP")
		return request, source, nil
	}
}

func smsPeerReadinessValidateLegacyProbe(request *sip.Request, source *net.UDPAddr, adapter *smsPeerReadinessTestAdapter) error {
	if source.String() != adapter.gateway || len(request.Body()) != 0 ||
		len(request.GetHeaders("Authorization"))+len(request.GetHeaders("Proxy-Authorization")) != 0 {
		return fmt.Errorf("unexpected source, private body or authorization in legacy readiness probe")
	}
	for _, name := range []string{"Via", "From", "To", "Call-ID", "CSeq", "Content-Length"} {
		if len(request.GetHeaders(name)) != 1 {
			return fmt.Errorf("invalid readiness probe header %s", name)
		}
	}
	if request.Via() == nil || request.From() == nil || request.To() == nil || request.CallID() == nil ||
		request.CSeq() == nil || request.CSeq().MethodName != sip.OPTIONS || request.ContentLength() == nil ||
		int(*request.ContentLength()) != 0 || request.Via().Params.GetOr("branch", "") == "" {
		return fmt.Errorf("invalid readiness probe transaction")
	}
	return nil
}
