package sipgateway

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Real Asterisk/PJSIP removes the To URI port in an OPTIONS response. Sending
// that logical address initially permits exact matching without accepting a
// modified response URI or weakening source/transaction checks.
func TestSMSPeerReadinessLogicalToPreservesPinnedTransport(t *testing.T) {
	for _, algorithm := range []string{"SHA-256", "MD5"} {
		for _, status := range []int{200, 401, 407} {
			t.Run(fmt.Sprintf("%s/%d", algorithm, status), func(t *testing.T) {
				manager, backend, peer := newPeerSMSTest(t, func(c *Config) {
					c.Peer.OutboundAuth.Algorithms = []string{algorithm}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				result := smsPeerLifecycleStart(ctx, manager.runtime)
				probe, source := smsPeerLifecycleReadProbe(t, peer)
				endpoint := peer.LocalAddr().(*net.UDPAddr)
				if probe.Recipient.Port != endpoint.Port || probe.Recipient.Host != endpoint.IP.String() ||
					probe.To().Address.Port != 0 || probe.To().Address.Host != endpoint.IP.String() ||
					probe.To().Address.User != "keepalive" {
					t.Fatal("logical To or actual fixed transport endpoint changed")
				}
				response := sip.NewResponseFromRequest(probe, status, "Synthetic", nil)
				response.To().Params.Add("tag", "synthetic-options-recipient")
				if status != 200 {
					name := "WWW-Authenticate"
					if status == 407 {
						name = "Proxy-Authenticate"
					}
					response.AppendHeader(sip.NewHeader(name, fmt.Sprintf(`Digest realm="%s",nonce="synthetic-options-nonce",algorithm=%s,qop="auth"`, messagePeerRealm, algorithm)))
				}
				write := func(socket *net.UDPConn, value *sip.Response) {
					t.Helper()
					if _, err := socket.WriteToUDP([]byte(value.String()), source); err != nil {
						t.Fatal(err)
					}
				}
				mustStillWait := func() {
					t.Helper()
					select {
					case <-result:
						t.Fatal("foreign source or modified To completed readiness")
					case <-time.After(50 * time.Millisecond):
					}
				}
				write(newMessagePeerSocket(t), response)
				mustStillWait()
				changed := response.Clone()
				changed.To().Address.Port = endpoint.Port
				write(peer, changed)
				mustStillWait()
				write(peer, response)
				smsPeerLifecycleResult(t, ctx, result, true)
				smsPeerLifecycleDrainProbe(t, peer, probe)
				if backend.count() != 0 || len(response.Body()) != 0 {
					t.Fatal("readiness performed SMS business or exported a payload")
				}
			})
		}
	}
}
