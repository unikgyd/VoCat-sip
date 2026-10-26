package sipgateway

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

// This file intentionally uses only the preceding accepted Manager, Store and
// SIP APIs. The only synthetic endpoint is the isolated fixed UDP peer; the
// Inbox, original plan, both SQLite handles, claim and transport are real.
func smsPeerReadinessFixture(t *testing.T, local bool) (context.Context, [2]*store.Store, *Manager, *syntheticSMS, *net.UDPConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	stores := smsCASStores(t)
	peer := newMessagePeerSocket(t)
	backend := newSyntheticSMS()
	backend.subscription = hashCallOperation(smsCaptureICCID)
	manager := New(ctx, stores[0], WithCarrierSMSBackend(backend))
	t.Cleanup(manager.Close)
	config := messagePeerConfig(t, peer).Config
	config.Listen = unusedAddress(t)
	for index := range config.Accounts {
		config.Accounts[index].Password = testPassword
	}
	config.Peer.InboundAuth.Password = messagePeerPassword
	config.Peer.OutboundAuth.Password = messagePeerOutboundPassword
	config.Peer.OutboundAuth.Algorithms = []string{"SHA-256"}
	if local {
		config.CarrierLines[0].SMSRecipients = []string{"1001"}
	}
	view, err := manager.Apply(ctx, config)
	if err != nil || !view.Status.Running || manager.runtime == nil || !manager.runtime.config.requireDurableAuthority {
		t.Fatal("actual durable Manager could not start the peer-readiness fixture:", err)
	}
	manager.runtime.mu.Lock()
	manager.runtime.messageTimeout = 1250 * time.Millisecond
	manager.runtime.mu.Unlock()
	return ctx, stores, manager, backend, peer
}

func smsPeerReadinessRead(peer *net.UDPConn, wait time.Duration) (*sip.Request, *net.UDPAddr, []byte, error) {
	if err := peer.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return nil, nil, nil, err
	}
	buffer := make([]byte, 16*1024)
	count, source, err := peer.ReadFromUDP(buffer)
	if err != nil {
		return nil, nil, nil, err
	}
	raw := append([]byte(nil), buffer[:count]...)
	message, err := registrarParser().ParseSIP(raw)
	if err != nil {
		return nil, nil, raw, err
	}
	request, valid := message.(*sip.Request)
	if !valid {
		return nil, nil, raw, fmt.Errorf("fixed peer received a response instead of a request")
	}
	request.SetSource(source.String())
	request.SetTransport("UDP")
	return request, source, raw, nil
}

func smsPeerReadinessRequireProbe(t *testing.T, request *sip.Request, raw []byte, message store.SMSMessage) {
	t.Helper()
	if request == nil {
		t.Fatal("fixed peer received no parsable request")
	}
	if request.Method != sip.OPTIONS || len(request.Body()) != 0 {
		t.Errorf("unverified fixed peer received method=%s/body-bytes=%d before a bodyless OPTIONS readiness result", request.Method, len(request.Body()))
	}
	if len(request.GetHeaders("Authorization"))+len(request.GetHeaders("Proxy-Authorization")) != 0 {
		t.Error("readiness probing exposed an outbound Digest credential")
	}
	text := string(raw)
	for _, private := range []string{message.Body, message.Peer, message.ICCID, hashCallOperation(message.ICCID),
		smsReplyUser(message.DeviceID, message.ICCID, message.Peer), "sms2-"} {
		if private != "" && strings.Contains(text, private) {
			t.Error("unverified readiness packet exposed SMS body, sender, SIM identity or a reply alias")
			break
		}
	}
}

func smsPeerReadinessSaved(t *testing.T, ctx context.Context, stores [2]*store.Store, id string) (store.SMSMessage, smsInbox) {
	t.Helper()
	saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage(id, "synthetic private SMS body requiring peer readiness"))
	if err != nil {
		t.Fatal(err)
	}
	_, original := smsCaptureSavedInbox(t, stores[1], saved)
	if len(original.Targets) == 0 || original.Targets[peerSMSRecipientKey("sms-in")].Code != 0 ||
		original.Targets[peerSMSRecipientKey("sms-in")].AttemptToken != "" {
		t.Fatal("actual receive did not start with its original unclaimed fixed-peer plan")
	}
	return saved, original
}

func smsPeerReadinessQueued(t *testing.T, database *store.Store, saved store.SMSMessage, original smsInbox) {
	t.Helper()
	_, current := smsCaptureSavedInbox(t, database, saved)
	target := current.Targets[peerSMSRecipientKey("sms-in")]
	before := original.Targets[peerSMSRecipientKey("sms-in")]
	if target.Code != 0 || target.AttemptToken != "" || target.Identity != before.Identity ||
		!current.Until.Equal(original.Until) || !smsInboxSameSource(current, original) ||
		smsInboxPayloadFingerprint(current) != smsInboxPayloadFingerprint(original) {
		t.Errorf("unverified peer changed the original Queued plan/token/payload/expiry: code=%d token-present=%t expiry-preserved=%t", target.Code, target.AttemptToken != "", current.Until.Equal(original.Until))
	}
}

// Drain all permitted OPTIONS retransmissions until the real worker finishes.
// A lost/untrusted readiness result must never turn into an authenticated
// OPTIONS or a MESSAGE, including after the first packet and on later polls.
func smsPeerReadinessFinishNegative(t *testing.T, ctx context.Context, peer *net.UDPConn, done <-chan error, message store.SMSMessage) {
	t.Helper()
	finished := false
	for {
		if !finished {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("peer-readiness attempt did not finish cleanly:", err)
				}
				finished = true
			default:
			}
		}
		request, _, raw, err := smsPeerReadinessRead(peer, 75*time.Millisecond)
		if err == nil {
			smsPeerReadinessRequireProbe(t, request, raw, message)
			continue
		}
		if timeout, valid := err.(net.Error); !valid || !timeout.Timeout() {
			t.Fatal("could not inspect the fixed-peer readiness boundary:", err)
		}
		if finished {
			return
		}
		if ctx.Err() != nil {
			t.Fatal("peer-readiness worker did not terminate within its synthetic test budget:", ctx.Err())
		}
	}
}

func TestSMSPeerReadinessLostOPTIONSKeepsQueuedAndOriginalExpiry(t *testing.T) {
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
	saved, original := smsPeerReadinessSaved(t, ctx, stores, "readiness-lost-options-source")
	done := make(chan error, 1)
	go func() { done <- manager.DeliverSMSOnce(ctx) }()
	first, _, raw, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil {
		t.Fatal("actual fixed peer received no readiness probe:", err)
	}
	smsPeerReadinessRequireProbe(t, first, raw, saved)
	smsPeerReadinessQueued(t, stores[1], saved, original)
	// Deliberately send no response. Loss is not evidence that a MESSAGE body
	// was attempted, and cannot mint Unknown or extend the original 24h TTL.
	smsPeerReadinessFinishNegative(t, ctx, peer, done, saved)
	smsPeerReadinessQueued(t, stores[1], saved, original)
	second := make(chan error, 1)
	go func() { second <- manager.DeliverSMSOnce(ctx) }()
	smsPeerReadinessFinishNegative(t, ctx, peer, second, saved)
	smsPeerReadinessQueued(t, stores[1], saved, original)
	if backend.count() != 0 {
		t.Fatal("fixed-peer readiness submitted a paid carrier SMS")
	}
}

func TestSMSPeerReadinessUntrustedChallengeNeverAuthenticatesOrExposesMessage(t *testing.T) {
	for _, scenario := range []struct {
		name, realm, algorithm string
		code                   int
	}{
		{"wrong-realm", "untrusted-other-realm", "SHA-256", 401},
		{"disallowed-algorithm", messagePeerRealm, "MD5", 407},
		{"unsupported-algorithm", messagePeerRealm, "SHA-512-256", 401},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
			saved, original := smsPeerReadinessSaved(t, ctx, stores, "readiness-untrusted-challenge-source")
			done := make(chan error, 1)
			go func() { done <- manager.DeliverSMSOnce(ctx) }()
			first, source, raw, err := smsPeerReadinessRead(peer, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			smsPeerReadinessRequireProbe(t, first, raw, saved)
			smsPeerReadinessQueued(t, stores[1], saved, original)
			header := "WWW-Authenticate"
			if scenario.code == 407 {
				header = "Proxy-Authenticate"
			}
			challenge := fmt.Sprintf(`Digest realm="%s",nonce="synthetic-readiness-nonce",algorithm=%s,qop="auth"`, scenario.realm, scenario.algorithm)
			if err := fixedPeerResponse(peer, first, source, scenario.code, sip.NewHeader(header, challenge)); err != nil {
				t.Fatal(err)
			}
			smsPeerReadinessFinishNegative(t, ctx, peer, done, saved)
			smsPeerReadinessQueued(t, stores[1], saved, original)
			if backend.count() != 0 {
				t.Fatal("untrusted OPTIONS challenge reached a paid SMS backend")
			}
		})
	}
}

func TestSMSPeerReadinessReadyResponseCommitsUnknownBeforeFirstPayload(t *testing.T) {
	for _, readyStatus := range []int{200, 401, 407} {
		t.Run(fmt.Sprint(readyStatus), func(t *testing.T) {
			ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
			saved, original := smsPeerReadinessSaved(t, ctx, stores, "readiness-valid-result-source")
			done := make(chan error, 1)
			go func() { done <- manager.DeliverSMSOnce(ctx) }()
			probe, source, raw, err := smsPeerReadinessRead(peer, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			smsPeerReadinessRequireProbe(t, probe, raw, saved)
			smsPeerReadinessQueued(t, stores[1], saved, original)
			var headers []sip.Header
			if readyStatus != 200 {
				name := "WWW-Authenticate"
				if readyStatus == 407 {
					name = "Proxy-Authenticate"
				}
				headers = []sip.Header{sip.NewHeader(name, `Digest realm="`+messagePeerRealm+`",nonce="synthetic-readiness-nonce",algorithm=SHA-256,qop="auth"`)}
			}
			if err := fixedPeerResponse(peer, probe, source, readyStatus, headers...); err != nil {
				t.Fatal(err)
			}
			message, destination, _, err := smsPeerReadinessRead(peer, 2*time.Second)
			if err != nil || message == nil || message.Method != sip.MESSAGE || string(message.Body()) != saved.Body {
				t.Fatal("trusted readiness did not release the original actual MESSAGE:", err)
			}
			_, unknown := smsCaptureSavedInbox(t, stores[1], saved)
			target := unknown.Targets[peerSMSRecipientKey("sms-in")]
			if target.Code != 504 || !validSMSAttemptToken(target.AttemptToken) || target.Identity != original.Targets[peerSMSRecipientKey("sms-in")].Identity ||
				!unknown.Until.Equal(original.Until) || !smsInboxSameSource(unknown, original) || message.From() == nil ||
				message.From().Address.User != smsReplyUser(saved.DeviceID, saved.ICCID, saved.Peer) {
				t.Fatal("first private payload preceded its actual FULL Unknown commit or lost the original source/card/TTL")
			}
			if err := fixedPeerResponse(peer, message, destination, 200); err != nil {
				t.Fatal(err)
			}
			smsCASDeliveryResult(t, ctx, done)
			_, accepted := smsCaptureSavedInbox(t, stores[1], saved)
			if accepted.Targets[peerSMSRecipientKey("sms-in")].Code != 200 ||
				accepted.Targets[peerSMSRecipientKey("sms-in")].AttemptToken != target.AttemptToken || backend.count() != 0 {
				t.Fatal("ready peer did not retain its one owned accepted delivery")
			}
		})
	}
}

func TestSMSPeerReadinessSlowPeerDoesNotBlockRegisteredLocalRecipient(t *testing.T) {
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, true)
	phone := registerMessagePeer(t, manager.runtime, "1001")
	saved, original := smsPeerReadinessSaved(t, ctx, stores, "readiness-slow-peer-with-local-source")
	local := replyOneMessage(phone.conn, 200)
	done := make(chan error, 1)
	go func() { done <- manager.DeliverSMSOnce(ctx) }()
	probe, _, raw, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	smsPeerReadinessRequireProbe(t, probe, raw, saved)
	// This is shorter than the actual unanswered peer budget, and starts only
	// after that peer's real first datagram. No synthetic sleep releases it.
	select {
	case actual := <-local:
		if actual.err != nil || actual.request == nil || string(actual.request.Body()) != saved.Body {
			t.Fatal("registered local recipient failed while the fixed peer was slow:", actual.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unanswered fixed-peer readiness blocked the independent registered local recipient")
	}
	smsPeerReadinessQueued(t, stores[1], saved, original)
	smsPeerReadinessFinishNegative(t, ctx, peer, done, saved)
	smsPeerReadinessQueued(t, stores[1], saved, original)
	_, completed := smsCaptureSavedInbox(t, stores[1], saved)
	if completed.Targets["1001"].Code != 200 || !validSMSAttemptToken(completed.Targets["1001"].AttemptToken) || backend.count() != 0 {
		t.Fatal("slow-peer isolation lost the actual local acceptance or reached a paid carrier SMS sender")
	}
}
