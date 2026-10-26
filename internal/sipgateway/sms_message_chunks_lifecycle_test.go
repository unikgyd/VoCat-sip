package sipgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

func smsChunksRead(t *testing.T, connection *net.UDPConn) (*sip.Request, *net.UDPAddr, []byte) {
	t.Helper()
	request, source, raw, err := smsPeerReadinessRead(connection, 2*time.Second)
	if err != nil || request == nil {
		t.Fatal("actual application chunk was not received:", err)
	}
	return request, source, raw
}

func smsChunksPeerProbe(t *testing.T, peer *net.UDPConn, saved store.SMSMessage) {
	t.Helper()
	request, source, raw := smsChunksRead(t, peer)
	smsPeerReadinessRequireProbe(t, request, raw, saved)
	if request.Method != sip.OPTIONS {
		t.Fatal("MT peer did not start with a neutral readiness probe")
	}
	if err := fixedPeerResponse(peer, request, source, 200); err != nil {
		t.Fatal(err)
	}
}

func smsChunksPeerState(t *testing.T, database *store.Store, saved store.SMSMessage, original smsInbox, code int, token string) string {
	t.Helper()
	_, current := smsCaptureSavedInbox(t, database, saved)
	user := peerSMSRecipientKey("sms-in")
	target, exists := current.Targets[user]
	if !exists || target.Code != code || !validSMSAttemptToken(target.AttemptToken) || token != "" && target.AttemptToken != token ||
		target.Identity != original.Targets[user].Identity || !current.Until.Equal(original.Until) ||
		!smsInboxSameSource(current, original) || smsInboxPayloadFingerprint(current) != smsInboxPayloadFingerprint(original) {
		t.Fatalf("whole peer message changed its outcome/owner/original identity/TTL/source: code=%d want=%d", target.Code, code)
	}
	return target.AttemptToken
}

func smsChunksPeerPart(t *testing.T, request *sip.Request, raw []byte, saved store.SMSMessage, content string, index, total int) {
	t.Helper()
	if request.Method != sip.MESSAGE || request.CSeq() == nil || request.CSeq().SeqNo != 1 || request.CSeq().MethodName != sip.MESSAGE ||
		request.CallID() == nil || request.From() == nil || request.To() == nil || request.Via() == nil ||
		request.From().Address.User != smsReplyUser(saved.DeviceID, saved.ICCID, saved.Peer) || request.From().DisplayName != saved.Peer ||
		request.To().Address.User != "sms-in" || request.Recipient.User != "sms-in" ||
		string(request.Body()) != fmt.Sprintf("[%d/%d] %s", index, total, content) || len(raw) > 1300 ||
		len(request.GetHeaders("Authorization"))+len(request.GetHeaders("Proxy-Authorization")) != 0 {
		t.Fatal("actual peer part lost its bounded raw chunk, original card reply identity or fresh unauthenticated MESSAGE transaction")
	}
}

func smsChunksChallenge(t *testing.T, peer *net.UDPConn, request *sip.Request, source *net.UDPAddr, status int) {
	t.Helper()
	header := "WWW-Authenticate"
	if status == 407 {
		header = "Proxy-Authenticate"
	}
	if err := fixedPeerResponse(peer, request, source, status, sip.NewHeader(header, `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
		t.Fatal(err)
	}
}

func smsChunksReadAuthenticated(t *testing.T, peer *net.UDPConn, first *sip.Request, status int) (*sip.Request, *net.UDPAddr) {
	t.Helper()
	authenticated, source, raw := smsChunksRead(t, peer)
	header, other := "Authorization", "Proxy-Authorization"
	if status == 407 {
		header, other = other, header
	}
	if err := verifyPeerMessageDigest(authenticated, header); err != nil {
		t.Fatal(err)
	}
	if authenticated.CSeq().SeqNo != 2 || authenticated.CallID().Value() != first.CallID().Value() ||
		authenticated.Via().Params.GetOr("branch", "") == first.Via().Params.GetOr("branch", "") ||
		authenticated.From().Value() != first.From().Value() || authenticated.To().Value() != first.To().Value() ||
		string(authenticated.Body()) != string(first.Body()) || len(authenticated.GetHeaders(other)) != 0 || len(raw) > 1300 {
		t.Fatal("part authentication changed content/source, reused its first branch or exceeded the authenticated UDP bound")
	}
	return authenticated, source
}

func smsChunksPeerNoReplay(t *testing.T, ctx context.Context, manager *Manager, peer *net.UDPConn) {
	t.Helper()
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	smsCaptureWireNoUDP(t, peer)
}

func TestSMSMessageChunksPeerDigestPerPartKeepsOneDurableWholeClaim(t *testing.T) {
	for _, status := range []int{401, 407} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
			manager.runtime.mu.Lock()
			manager.runtime.messageTimeout = 6 * time.Second
			manager.runtime.mu.Unlock()
			saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("chunk-digest-whole-claim", strings.Repeat("x", 600)))
			if err != nil {
				t.Fatal(err)
			}
			_, original := smsCaptureSavedInbox(t, stores[1], saved)
			done := smsWireRetransmitStart(t, ctx, manager)
			smsChunksPeerProbe(t, peer, saved)
			// These are explicit fixed expected contents, not splitter output.
			contents := []string{strings.Repeat("x", 280), strings.Repeat("x", 280), strings.Repeat("x", 40)}
			ids, tags, branches := map[string]bool{}, map[string]bool{}, map[string]bool{}
			token := ""
			for index, content := range contents {
				first, source, raw := smsChunksRead(t, peer)
				smsChunksPeerPart(t, first, raw, saved, content, index+1, 3)
				id, tag, branch := first.CallID().Value(), first.From().Params.GetOr("tag", ""), first.Via().Params.GetOr("branch", "")
				if id == "" || tag == "" || branch == "" || ids[id] || tags[tag] || branches[branch] {
					t.Fatal("different parts reused a SIP transaction identity")
				}
				ids[id], tags[tag], branches[branch] = true, true, true
				token = smsChunksPeerState(t, stores[1], saved, original, 504, token)
				smsChunksChallenge(t, peer, first, source, status)
				authenticated, destination := smsChunksReadAuthenticated(t, peer, first, status)
				if branches[authenticated.Via().Params.GetOr("branch", "")] {
					t.Fatal("an authentication branch reused another part's transaction")
				}
				branches[authenticated.Via().Params.GetOr("branch", "")] = true
				smsChunksPeerState(t, stores[1], saved, original, 504, token)
				if err := fixedPeerResponse(peer, authenticated, destination, 200); err != nil {
					t.Fatal(err)
				}
			}
			smsCASDeliveryResult(t, ctx, done)
			smsChunksPeerState(t, stores[1], saved, original, 200, token)
			smsChunksPeerNoReplay(t, ctx, manager, peer)
			if backend.count() != 0 {
				t.Fatal("MT peer chunking invoked a paid carrier submission")
			}
		})
	}
}

func TestSMSMessageChunksPeerAuthenticationRetryRechecksMidwayAuthorityAndInbox(t *testing.T) {
	for _, mutation := range []string{"peer-grant", "actual-inbox-body"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
			manager.runtime.mu.Lock()
			manager.runtime.messageTimeout = 5 * time.Second
			manager.runtime.mu.Unlock()
			saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("chunk-peer-auth-source-veto", strings.Repeat("y", 600)))
			if err != nil {
				t.Fatal(err)
			}
			_, original := smsCaptureSavedInbox(t, stores[1], saved)
			done := smsWireRetransmitStart(t, ctx, manager)
			smsChunksPeerProbe(t, peer, saved)
			first, source, raw := smsChunksRead(t, peer)
			smsChunksPeerPart(t, first, raw, saved, strings.Repeat("y", 280), 1, 3)
			token := smsChunksPeerState(t, stores[1], saved, original, 504, "")
			smsChunksChallenge(t, peer, first, source, 401)
			authenticated, destination := smsChunksReadAuthenticated(t, peer, first, 401)
			if err := fixedPeerResponse(peer, authenticated, destination, 200); err != nil {
				t.Fatal(err)
			}
			second, remote, raw := smsChunksRead(t, peer)
			smsChunksPeerPart(t, second, raw, saved, strings.Repeat("y", 280), 2, 3)
			// A real second handle commits the change after part one's final,
			// but before the part-two authentication guard may emit credentials.
			if mutation == "peer-grant" {
				setting, readErr := stores[1].AppSetting(ctx, SettingKey)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var config stored
				if !setting.Sensitive || json.Unmarshal(setting.Value, &config) != nil || config.Config.Peer == nil {
					t.Fatal("actual private settings could not be inspected")
				}
				config.Config.Peer.AllowSMSToPeer = false
				setting.Value, err = json.Marshal(config)
				if err == nil {
					err = stores[1].UpsertAppSetting(ctx, setting)
				}
			} else {
				changed := saved
				changed.Body = "actual source edited after a real accepted application part"
				_, err = stores[1].SaveSMSMessage(ctx, changed)
			}
			if err != nil {
				t.Fatal(err)
			}
			smsChunksChallenge(t, peer, second, remote, 407)
			smsCASDeliveryResult(t, ctx, done)
			smsCaptureWireNoUDP(t, peer)
			smsChunksPeerState(t, stores[1], saved, original, 504, token)
			smsChunksPeerNoReplay(t, ctx, manager, peer)
			if backend.count() != 0 {
				t.Fatal("revoked/changed multipart MT reached the paid backend")
			}
		})
	}
}

func TestSMSMessageChunksCallerBudgetIncludesEarlierPartsReadinessAndDigest(t *testing.T) {
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
	manager.runtime.mu.Lock()
	manager.runtime.messageTimeout = 8 * time.Second
	manager.runtime.mu.Unlock()
	saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("chunk-one-total-caller-deadline", strings.Repeat("z", 600)))
	if err != nil {
		t.Fatal(err)
	}
	_, original := smsCaptureSavedInbox(t, stores[1], saved)
	caller, cancel := context.WithTimeout(ctx, 1700*time.Millisecond)
	defer cancel()
	deadline, _ := caller.Deadline()
	done := smsWireRetransmitStart(t, caller, manager)
	smsChunksPeerProbe(t, peer, saved)
	first, _, raw := smsChunksRead(t, peer)
	smsChunksPeerPart(t, first, raw, saved, strings.Repeat("z", 280), 1, 3)
	// Spend real budget in a real SIP T1 retry, not a newly invented timeout.
	repeated, source, _ := smsChunksRead(t, peer)
	if repeated.String() != first.String() {
		t.Fatal("part one did not retain its real retransmission identity")
	}
	if err := fixedPeerResponse(peer, repeated, source, 200); err != nil {
		t.Fatal(err)
	}
	second, destination, raw := smsChunksRead(t, peer)
	smsChunksPeerPart(t, second, raw, saved, strings.Repeat("z", 280), 2, 3)
	token := smsChunksPeerState(t, stores[1], saved, original, 504, "")
	smsChunksChallenge(t, peer, second, destination, 407)
	authenticated, _, _ := smsChunksRead(t, peer)
	if err := verifyPeerMessageDigest(authenticated, "Proxy-Authorization"); err != nil {
		t.Fatal(err)
	}
	// All actual authenticated retries may continue only until the SAME
	// caller deadline. No response is supplied for part two.
	limit := time.NewTimer(time.Until(deadline.Add(350 * time.Millisecond)))
	defer limit.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-limit.C:
		t.Fatal("a later application part or Digest reset the original caller budget")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if time.Now().Before(deadline.Add(-100 * time.Millisecond)) {
		t.Fatal("unanswered valid part did not use the remaining caller budget")
	}
	smsChunksPeerState(t, stores[1], saved, original, 504, token)
	// Drain only datagrams actually emitted before the deadline. None may be
	// another part, a new transaction, or traffic observed after this barrier.
	for {
		retry, _, _, readErr := smsPeerReadinessRead(peer, 40*time.Millisecond)
		if readErr != nil {
			if timeout, ok := readErr.(net.Error); !ok || !timeout.Timeout() {
				t.Fatal(readErr)
			}
			break
		}
		if retry.String() != authenticated.String() {
			t.Fatal("expired caller emitted a fresh part or changed its authenticated retry")
		}
	}
	smsCaptureWireNoUDP(t, peer)
	smsChunksPeerNoReplay(t, ctx, manager, peer)
	if backend.count() != 0 {
		t.Fatal("timed-out multipart MT reached a paid backend")
	}
}

func TestSMSMessageChunksOneCompleteContactAndLiteralSinglePartControls(t *testing.T) {
	t.Run("one-complete-contact", func(t *testing.T) {
		ctx, stores, manager, backend, _ := smsMessagePartsFixture(t)
		first, second := registerMessagePeer(t, manager.runtime, "1001"), registerMessagePeer(t, manager.runtime, "1001")
		saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("chunk-complete-one-contact", strings.Repeat("p", 600)))
		if err != nil {
			t.Fatal(err)
		}
		_, original := smsCaptureSavedInbox(t, stores[1], saved)
		done := smsWireRetransmitStart(t, ctx, manager)
		requestA, sourceA, rawA := smsChunksRead(t, first.conn)
		requestB, sourceB, rawB := smsChunksRead(t, second.conn)
		expected := []string{strings.Repeat("p", 280), strings.Repeat("p", 280), strings.Repeat("p", 40)}
		if !smsMessagePartsCheck(t, requestA, rawA, saved, expected, 0) || !smsMessagePartsCheck(t, requestB, rawB, saved, expected, 0) {
			t.Fatal("positive control received no actual multipart bodies")
		}
		token := smsMessagePartsState(t, stores[1], saved, original, 504, "")
		if !smsMessagePartsReply(t, second.conn, requestB, sourceB, 403) || !smsMessagePartsReply(t, first.conn, requestA, sourceA, 200) {
			t.Fatal("actual receiver could not return its final")
		}
		for index := 1; index < 3; index++ {
			request, source, raw := smsChunksRead(t, first.conn)
			if !smsMessagePartsCheck(t, request, raw, saved, expected, index) {
				t.Fatal("complete Contact did not receive its next actual part")
			}
			smsMessagePartsState(t, stores[1], saved, original, 504, token)
			if !smsMessagePartsReply(t, first.conn, request, source, 200) {
				t.Fatal("actual complete Contact final failed")
			}
		}
		smsCASDeliveryResult(t, ctx, done)
		smsMessagePartsState(t, stores[1], saved, original, 200, token)
		smsMessagePartsNoReplay(t, ctx, manager, first, second)
		if backend.count() != 0 {
			t.Fatal("complete-contact MT reached a paid backend")
		}
	})
	t.Run("literal-prefix-single-part-unchanged", func(t *testing.T) {
		ctx, stores, manager, backend, _ := smsMessagePartsFixture(t)
		phone := registerMessagePeer(t, manager.runtime, "1001")
		saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("chunk-literal-prefix-ordinary-content", "[1/2] not protocol grouping"))
		if err != nil {
			t.Fatal(err)
		}
		_, original := smsCaptureSavedInbox(t, stores[1], saved)
		done := smsWireRetransmitStart(t, ctx, manager)
		request, source, raw := smsChunksRead(t, phone.conn)
		if request.Method != sip.MESSAGE || string(request.Body()) != saved.Body || len(raw) > 1300 {
			t.Fatal("short literal prefix was rewritten as segmentation metadata")
		}
		token := smsMessagePartsState(t, stores[1], saved, original, 504, "")
		if !smsMessagePartsReply(t, phone.conn, request, source, 200) {
			t.Fatal("single-part actual response failed")
		}
		smsCASDeliveryResult(t, ctx, done)
		smsMessagePartsState(t, stores[1], saved, original, 200, token)
		smsMessagePartsNoReplay(t, ctx, manager, phone)
		if backend.count() != 0 {
			t.Fatal("literal MT control reached a paid backend")
		}
	})
}

func TestSMSMessageChunksDoNotSplitOrdinaryMESSAGEOrExpandMOAdmission(t *testing.T) {
	t.Run("ordinary-extension-message-stays-one-body", func(t *testing.T) {
		r := testMessageRuntime(t)
		receiver, sender := registerMessagePeer(t, r, "1002"), newWireClient(t, r)
		body := strings.Repeat("q", 600)
		sender.messageChallenge("1002", body, "text/plain")
		delivery := replyOneMessage(receiver.conn, 200)
		requireStatus(t, sender.exchange(sender.messagePacket("1002", body, "text/plain", sender.messageAuthorization("1002"), 2, "")), 200)
		actual := <-delivery
		if actual.err != nil || actual.request == nil || string(actual.request.Body()) != body {
			t.Fatal("ordinary authenticated MESSAGE was incorrectly split or rewritten:", actual.err)
		}
		smsCaptureWireNoUDP(t, receiver.conn)
		oversized := strings.Repeat("r", 4097)
		sender.messageChallenge("1002", oversized, "text/plain")
		requireStatus(t, sender.exchange(sender.messagePacket("1002", oversized, "text/plain", sender.messageAuthorization("1002"), 4, "")), 413)
		smsCaptureWireNoUDP(t, receiver.conn)
	})
	t.Run("carrier-mo-still-rejects-4097-before-backend", func(t *testing.T) {
		_, _, manager, backend, _ := smsMessagePartsFixture(t)
		sender := newWireClient(t, manager.runtime)
		body, target := strings.Repeat("s", 4097), "+15550000002"
		sender.messageChallenge(target, body, "text/plain")
		requireStatus(t, sender.exchange(sender.messagePacket(target, body, "text/plain", sender.messageAuthorization(target), 2, "")), 413)
		if backend.count() != 0 {
			t.Fatal("MT admission expansion leaked into oversized paid MO submission")
		}
	})
}
