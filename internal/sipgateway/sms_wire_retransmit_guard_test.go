package sipgateway

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

// The RED fixture uses the actual Manager, actual atomic receive planner, two
// independent SQLite handles and the live UDP listener. No guard result or
// transport write is mocked. The default sipgo T1 timer must really fire.
func smsWireRetransmitStart(t *testing.T, ctx context.Context, manager *Manager) <-chan error {
	t.Helper()
	delivery, cancel := context.WithCancel(ctx)
	result, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		result <- manager.DeliverSMSOnce(delivery)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("actual MT worker did not stop after test cancellation")
		}
	})
	return result
}

func smsWireRetransmitTimeout(manager *Manager) {
	manager.runtime.mu.Lock()
	manager.runtime.messageTimeout = 5 * time.Second
	manager.runtime.mu.Unlock()
}

func smsWireRetransmitNoUDP(t *testing.T, connection *net.UDPConn) {
	t.Helper()
	// This crosses sipgo's real 500 ms initial retransmission timer, rather
	// than merely checking that the logical auth-retry loop ran its guard.
	if err := connection.SetReadDeadline(time.Now().Add(1200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connection.ReadFromUDP(make([]byte, 16*1024)); err == nil {
		t.Fatal("a real UDP retransmission exposed the old MT payload after its source or runtime became invalid")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("could not observe the actual UDP retransmission boundary:", err)
	}
}

func smsWireRetransmitRequireSameUnknown(t *testing.T, database *store.Store, saved store.SMSMessage, recipient string, original smsInbox) {
	t.Helper()
	_, current := smsCaptureSavedInbox(t, database, saved)
	before, after := original.Targets[recipient], current.Targets[recipient]
	if before.Code != 504 || !validSMSAttemptToken(before.AttemptToken) || after.Code != 504 ||
		after.AttemptToken != before.AttemptToken || after.Identity != before.Identity ||
		!current.Until.Equal(original.Until) || !smsInboxSameSource(current, original) {
		t.Fatal("a wire veto revived or replaced its committed Unknown claim")
	}
}

func smsWireRetransmitPeerPositive(t *testing.T, ctx context.Context, database *store.Store, manager *Manager, peer *net.UDPConn, id string) {
	t.Helper()
	fresh := smsCaptureMessage(id, "synthetic fresh peer source after retransmission veto")
	saved, err := database.SaveSMSMessage(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	result := smsWireRetransmitStart(t, ctx, manager)
	request, source, err := readFixedPeerMessage(peer)
	if err != nil || request == nil || string(request.Body()) != fresh.Body {
		t.Fatal("fresh actual source did not reach the still-working peer:", err)
	}
	if err := fixedPeerResponse(peer, request, source, 200); err != nil {
		t.Fatal(err)
	}
	smsCASDeliveryResult(t, ctx, result)
	_, accepted := smsCaptureSavedInbox(t, database, saved)
	if target := accepted.Targets[peerSMSRecipientKey("sms-in")]; target.Code != 200 || !validSMSAttemptToken(target.AttemptToken) {
		t.Fatal("fresh peer positive control was not durably accepted")
	}
}

func TestSMSWireRetransmitGuardActualInboxMutationVetoesEveryUDPResend(t *testing.T) {
	for _, mutation := range []string{"delete", "body"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			databases := smsCASStores(t)
			manager, backend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, nil)
			smsWireRetransmitTimeout(manager)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("actual-local-retransmit-source", "synthetic original local retransmission body"))
			if err != nil {
				t.Fatal(err)
			}
			result := smsWireRetransmitStart(t, ctx, manager)
			request, _, err := readFixedPeerMessage(phone.conn)
			if err != nil || request == nil || string(request.Body()) != saved.Body {
				t.Fatal("legitimate actual source did not reach its first UDP packet:", err)
			}
			_, unknown := smsCaptureSavedInbox(t, databases[1], saved)
			smsCaptureWireRequireUnknown(t, databases[1], saved, "1001")
			smsCaptureWireMutateSource(t, ctx, databases[1], saved, mutation)
			// No response is sent: a transaction-layer retransmission is now due.
			smsWireRetransmitNoUDP(t, phone.conn)
			smsCASDeliveryResult(t, ctx, result)
			smsWireRetransmitRequireSameUnknown(t, databases[1], saved, "1001", unknown)
			if err := manager.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			smsCaptureWireNoUDP(t, phone.conn)
			smsWireRetransmitRequireSameUnknown(t, databases[1], saved, "1001", unknown)
			smsCaptureFreshWireControl(t, ctx, databases[0], manager, phone, "fresh-local-after-retransmit-veto")
			if backend.count() != 0 {
				t.Fatal("MT UDP retransmission test invoked a paid carrier sender")
			}
		})
	}
}

func TestSMSWireRetransmitGuardActualAuthenticatedInboxMutationVetoesEveryUDPResend(t *testing.T) {
	for _, status := range []int{401, 407} {
		for _, mutation := range []string{"delete", "body"} {
			t.Run(fmt.Sprintf("challenge=%d/%s", status, mutation), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				databases := smsCASStores(t)
				peer := newMessagePeerSocket(t)
				manager, backend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, peer)
				smsWireRetransmitTimeout(manager)
				saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("actual-authenticated-retransmit-source", "synthetic original authenticated retransmission body"))
				if err != nil {
					t.Fatal(err)
				}
				result := smsWireRetransmitStart(t, ctx, manager)
				initial, source, err := readFixedPeerMessage(peer)
				if err != nil || initial == nil || string(initial.Body()) != saved.Body {
					t.Fatal("legitimate actual peer source did not reach its initial UDP packet:", err)
				}
				challenge, authorization := "WWW-Authenticate", "Authorization"
				if status == 407 {
					challenge, authorization = "Proxy-Authenticate", "Proxy-Authorization"
				}
				if err := fixedPeerResponse(peer, initial, source, status, sip.NewHeader(challenge, `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
					t.Fatal(err)
				}
				authenticated, _, err := readFixedPeerMessage(peer)
				if err != nil || authenticated == nil || string(authenticated.Body()) != saved.Body {
					t.Fatal("legitimate actual source did not reach its authenticated first packet:", err)
				}
				if err := verifyPeerMessageDigest(authenticated, authorization); err != nil {
					t.Fatal(err)
				}
				if authenticated.CallID().Value() != initial.CallID().Value() || authenticated.CSeq().SeqNo != initial.CSeq().SeqNo+1 ||
					authenticated.Via().Params.GetOr("branch", "") == initial.Via().Params.GetOr("branch", "") {
					t.Fatal("authenticated transaction lost its exact logical/wire identity")
				}
				recipient := peerSMSRecipientKey("sms-in")
				_, unknown := smsCaptureSavedInbox(t, databases[1], saved)
				smsCaptureWireRequireUnknown(t, databases[1], saved, recipient)
				smsCaptureWireMutateSource(t, ctx, databases[1], saved, mutation)
				// The authenticated transaction has no response. Its own first
				// timer-driven resend must revalidate the actual bound source.
				smsWireRetransmitNoUDP(t, peer)
				smsCASDeliveryResult(t, ctx, result)
				smsWireRetransmitRequireSameUnknown(t, databases[1], saved, recipient, unknown)
				if err := manager.DeliverSMSOnce(ctx); err != nil {
					t.Fatal(err)
				}
				smsCaptureWireNoUDP(t, peer)
				smsWireRetransmitRequireSameUnknown(t, databases[1], saved, recipient, unknown)
				smsWireRetransmitPeerPositive(t, ctx, databases[0], manager, peer, "fresh-peer-after-authenticated-retransmit-veto")
				if backend.count() != 0 {
					t.Fatal("authenticated MT UDP retransmission invoked a paid carrier sender")
				}
			})
		}
	}
}

// Pause the real close path after its atomic stopped fence but before socket
// deadlines/cancellation can mask an unguarded sipgo retransmission. All actual
// reads/writes remain on the original live PacketConn and are fully delegated.
type smsWireRetransmitClosePause struct {
	net.PacketConn
	once    sync.Once
	reached chan struct{}
	release <-chan struct{}
}

func (connection *smsWireRetransmitClosePause) SetWriteDeadline(deadline time.Time) error {
	connection.once.Do(func() {
		close(connection.reached)
		<-connection.release
	})
	return connection.PacketConn.SetWriteDeadline(deadline)
}

func TestSMSWireRetransmitGuardActualStopFenceVetoesResendBeforeTransportCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	databases := smsCASStores(t)
	manager, backend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, nil)
	smsWireRetransmitTimeout(manager)
	r := manager.runtime
	gate := newSMSCASRelease()
	defer gate.close()
	paused := &smsWireRetransmitClosePause{PacketConn: r.conn, reached: make(chan struct{}), release: gate.ch}
	r.conn = paused
	phone := registerMessagePeer(t, r, "1001")
	saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("actual-stop-retransmit-source", "synthetic original stop-fenced retransmission body"))
	if err != nil {
		t.Fatal(err)
	}
	result := smsWireRetransmitStart(t, ctx, manager)
	request, _, err := readFixedPeerMessage(phone.conn)
	if err != nil || request == nil || string(request.Body()) != saved.Body {
		t.Fatal("legitimate actual source did not reach its pre-stop UDP packet:", err)
	}
	_, unknown := smsCaptureSavedInbox(t, databases[1], saved)
	smsCaptureWireRequireUnknown(t, databases[1], saved, "1001")
	closed := make(chan struct{})
	go func() { manager.Close(); close(closed) }()
	smsCASWait(t, ctx, paused.reached, 1, "actual stopped fence before transport cancellation")
	if !r.closed.Load() || r.ctx.Err() != nil {
		t.Fatal("test did not isolate the actual stopped fence from transport cancellation")
	}
	smsWireRetransmitNoUDP(t, phone.conn)
	gate.close()
	smsCASWait(t, ctx, closed, 1, "actual Manager close completion")
	smsCASDeliveryResult(t, ctx, result)
	smsWireRetransmitRequireSameUnknown(t, databases[1], saved, "1001", unknown)
	// A genuinely restarted actual Manager must not resurrect Unknown, but a
	// new observed source must still traverse its new live UDP listener.
	restarted := New(ctx, databases[0], WithCarrierSMSBackend(backend))
	t.Cleanup(restarted.Close)
	if restarted.runtime == nil || restarted.runtime.closed.Load() {
		t.Fatal("actual Manager could not restart its saved durable listener")
	}
	smsWireRetransmitTimeout(restarted)
	freshPhone := registerMessagePeer(t, restarted.runtime, "1001")
	if err := restarted.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	smsCaptureWireNoUDP(t, freshPhone.conn)
	smsWireRetransmitRequireSameUnknown(t, databases[1], saved, "1001", unknown)
	smsCaptureFreshWireControl(t, ctx, databases[0], restarted, freshPhone, "fresh-source-after-actual-stop-restart")
	if backend.count() != 0 {
		t.Fatal("MT stop/restart UDP test invoked a paid carrier sender")
	}
}
