package sipgateway

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

// Only the placement of a real settings read is controlled. All writer/CAS,
// planner installation, actual Inbox guards and persistence are the promoted
// methods of the actual file Store, not synthetic authorization results.
type smsCaptureWireAuthorityPause struct {
	*store.Store
	mu            sync.Mutex
	remaining     int
	armAfterClaim bool
	reached       chan struct{}
	release       <-chan struct{}
}

func (database *smsCaptureWireAuthorityPause) AppSetting(ctx context.Context, key string) (store.AppSetting, error) {
	value, err := database.Store.AppSetting(ctx, key)
	if key != SettingKey {
		return value, err
	}
	database.mu.Lock()
	var reached chan struct{}
	var release <-chan struct{}
	if database.remaining > 0 {
		database.remaining--
		if database.remaining == 0 {
			reached, release = database.reached, database.release
		}
	}
	database.mu.Unlock()
	if reached != nil {
		close(reached)
		select {
		case <-release:
		case <-ctx.Done():
			return store.AppSetting{}, ctx.Err()
		}
	}
	return value, err
}

func (database *smsCaptureWireAuthorityPause) CompareSMSLedgerWithInboxGuard(ctx context.Context, before, after json.RawMessage, guards ...store.SMSForwardInboxGuard) (bool, error) {
	changed, err := database.Store.CompareSMSLedgerWithInboxGuard(ctx, before, after, guards...)
	if err == nil && changed && len(guards) > 0 {
		database.mu.Lock()
		if database.armAfterClaim {
			database.remaining = 1
			database.armAfterClaim = false
		}
		database.mu.Unlock()
	}
	return changed, err
}

func smsCaptureWireManager(t *testing.T, ctx context.Context, database *smsCaptureWireAuthorityPause, peer *net.UDPConn) (*Manager, *syntheticSMS) {
	t.Helper()
	backend := newSyntheticSMS()
	backend.subscription = hashCallOperation(smsCaptureICCID)
	manager := New(ctx, database, WithCarrierSMSBackend(backend))
	t.Cleanup(manager.Close)
	config := smsAuthorityConfig(t)
	if peer != nil {
		config = messagePeerConfig(t, peer).Config
		config.Listen = unusedAddress(t)
		for index := range config.Accounts {
			config.Accounts[index].Password = testPassword
		}
		config.Peer.InboundAuth.Password = messagePeerPassword
		config.Peer.OutboundAuth.Password = messagePeerOutboundPassword
	}
	view, err := manager.Apply(ctx, config)
	if err != nil || !view.Status.Running || manager.runtime == nil || !manager.runtime.config.requireDurableAuthority {
		t.Fatal("actual Manager did not admit the source-guard wire fixture:", err)
	}
	manager.runtime.mu.Lock()
	manager.runtime.messageTimeout = time.Second
	manager.runtime.mu.Unlock()
	if peer != nil {
		smsPeerReadinessOptIn(t, peer, manager.runtime.conn.LocalAddr().String())
	}
	return manager, backend
}

func smsCaptureWireMutateSource(t *testing.T, ctx context.Context, database *store.Store, saved store.SMSMessage, mutation string) {
	t.Helper()
	if mutation == "delete" {
		if err := database.DeleteSMSMessage(ctx, saved.ID); err != nil {
			t.Fatal(err)
		}
		return
	}
	saved.Body = "synthetic edited Inbox while authority read is paused"
	if _, err := database.SaveSMSMessage(ctx, saved); err != nil {
		t.Fatal(err)
	}
}

func smsCaptureWireRequireUnknown(t *testing.T, database *store.Store, saved store.SMSMessage, recipient string) {
	t.Helper()
	_, in := smsCaptureSavedInbox(t, database, saved)
	if target := in.Targets[recipient]; target.Code != 504 || !validSMSAttemptToken(target.AttemptToken) {
		t.Fatal("wire source veto restored Queued or falsely accepted its durable Unknown claim", target.Code)
	}
}

func smsCaptureWireNoUDP(t *testing.T, connection *net.UDPConn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(75 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connection.ReadFromUDP(make([]byte, 8192)); err == nil {
		t.Fatal("Inbox changed during the awaited authority read, but stale private payload still reached UDP")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("could not verify source-veto UDP boundary:", err)
	}
}

func TestSMSCaptureWireGuardAfterAwaitedAuthorityReadVetoesInitialPayload(t *testing.T) {
	for _, mutation := range []string{"delete", "body"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			databases := smsCASStores(t)
			wrapped := &smsCaptureWireAuthorityPause{Store: databases[0]}
			manager, backend := smsCaptureWireManager(t, ctx, wrapped, nil)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("wire-source-before-first-packet", "synthetic body must never reach first UDP"))
			if err != nil {
				t.Fatal(err)
			}
			// Arm only after the actual guarded FULL claim commit succeeds. This
			// cannot accidentally pause a pre-claim authority check instead.
			gate := newSMSCASRelease()
			t.Cleanup(gate.close)
			wrapped.mu.Lock()
			wrapped.armAfterClaim = true
			wrapped.reached = make(chan struct{})
			wrapped.release = gate.ch
			reached := wrapped.reached
			wrapped.mu.Unlock()
			done := make(chan error, 1)
			go func() { done <- manager.DeliverSMSOnce(ctx) }()
			smsCASWait(t, ctx, reached, 1, "initial per-wire authority read after durable claim")
			smsCaptureWireRequireUnknown(t, databases[1], saved, "1001")
			smsCaptureWireMutateSource(t, ctx, databases[1], saved, mutation)
			gate.close()
			smsCASDeliveryResult(t, ctx, done)
			smsCaptureWireRequireUnknown(t, databases[1], saved, "1001")
			smsCaptureWireNoUDP(t, phone.conn)
			if err := manager.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			smsCaptureWireNoUDP(t, phone.conn)
			// A source veto must not be a disabled receiver or broken SIP service.
			smsCaptureFreshWireControl(t, ctx, databases[0], manager, phone, "wire-source-fresh-after-initial-veto")
			if backend.count() != 0 {
				t.Fatal("MT source guard invoked a paid carrier backend")
			}
		})
	}
}

func TestSMSCaptureWireGuardAfterAwaitedAuthorityReadVetoesAuthenticatedRetry(t *testing.T) {
	for _, mutation := range []string{"delete", "body"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			databases := smsCASStores(t)
			wrapped := &smsCaptureWireAuthorityPause{Store: databases[0]}
			peer := newMessagePeerSocket(t)
			manager, backend := smsCaptureWireManager(t, ctx, wrapped, peer)
			saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("wire-source-before-auth-retry", "synthetic original challenged SMS body"))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- manager.DeliverSMSOnce(ctx) }()
			first, source, err := readFixedPeerMessage(peer)
			if err != nil || first == nil || string(first.Body()) != saved.Body {
				t.Fatal("actual bound source did not reach the initial peer challenge:", err)
			}
			gate := newSMSCASRelease()
			t.Cleanup(gate.close)
			wrapped.mu.Lock()
			wrapped.remaining = 1
			wrapped.reached = make(chan struct{})
			wrapped.release = gate.ch
			reached := wrapped.reached
			wrapped.mu.Unlock()
			if err := fixedPeerResponse(peer, first, source, 401, sip.NewHeader("WWW-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-source-guard-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
				t.Fatal(err)
			}
			smsCASWait(t, ctx, reached, 1, "authenticated retry authority read")
			smsCaptureWireRequireUnknown(t, databases[1], saved, peerSMSRecipientKey("sms-in"))
			smsCaptureWireMutateSource(t, ctx, databases[1], saved, mutation)
			gate.close()
			smsCASDeliveryResult(t, ctx, done)
			smsCaptureWireRequireUnknown(t, databases[1], saved, peerSMSRecipientKey("sms-in"))
			smsCaptureWireNoUDP(t, peer)
			if err := manager.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			smsCaptureWireNoUDP(t, peer)
			fresh := smsCaptureMessage("wire-source-fresh-after-auth-veto", "synthetic fresh peer positive control")
			freshSaved, err := databases[0].SaveSMSMessage(ctx, fresh)
			if err != nil {
				t.Fatal(err)
			}
			positive := make(chan error, 1)
			go func() { positive <- manager.DeliverSMSOnce(ctx) }()
			request, remote, err := readFixedPeerMessage(peer)
			if err != nil || request == nil || string(request.Body()) != fresh.Body {
				t.Fatal("fresh bound source did not traverse the still-working peer:", err)
			}
			if err := fixedPeerResponse(peer, request, remote, 200); err != nil {
				t.Fatal(err)
			}
			smsCASDeliveryResult(t, ctx, positive)
			_, accepted := smsCaptureSavedInbox(t, databases[0], freshSaved)
			if target := accepted.Targets[peerSMSRecipientKey("sms-in")]; target.Code != 200 || !validSMSAttemptToken(target.AttemptToken) {
				t.Fatal("fresh actual peer source was not durably accepted")
			}
			if backend.count() != 0 {
				t.Fatal("MT retry source guard invoked a paid carrier backend")
			}
		})
	}
}
