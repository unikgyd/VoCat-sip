package sipgateway

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestSMSMessageWireActualTwoContactsBothRevalidateOneCommittedSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	databases := smsCASStores(t)
	manager, backend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, nil)
	smsWireRetransmitTimeout(manager)
	phones := []*wireClient{registerMessagePeer(t, manager.runtime, "1001"), registerMessagePeer(t, manager.runtime, "1001")}
	saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("two-real-contacts-bound-source", "synthetic source for two contacts"))
	if err != nil {
		t.Fatal(err)
	}
	result := smsWireRetransmitStart(t, ctx, manager)
	for _, phone := range phones {
		request, _, err := readFixedPeerMessage(phone.conn)
		if err != nil || request == nil || string(request.Body()) != saved.Body {
			t.Fatal("actual captured source did not reach both legitimate Contacts:", err)
		}
	}
	_, original := smsCaptureSavedInbox(t, databases[1], saved)
	smsCaptureWireMutateSource(t, ctx, databases[1], saved, "body")
	for _, phone := range phones {
		smsWireRetransmitNoUDP(t, phone.conn)
	}
	smsCASDeliveryResult(t, ctx, result)
	smsWireRetransmitRequireSameUnknown(t, databases[1], saved, "1001", original)
	smsMessageWireReleased(t, manager.runtime)
	// Both destination paths, not just one, must still deliver a fresh source.
	fresh, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("fresh-two-real-contacts-bound-source", "synthetic fresh source for both contacts"))
	if err != nil {
		t.Fatal(err)
	}
	first, second := replyOneMessage(phones[0].conn, 200), replyOneMessage(phones[1].conn, 200)
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, response := range []peerResult{<-first, <-second} {
		if response.err != nil || response.request == nil || string(response.request.Body()) != fresh.Body {
			t.Fatal("a fresh bound source did not reach each still-registered Contact:", response.err)
		}
	}
	_, accepted := smsCaptureSavedInbox(t, databases[1], fresh)
	if accepted.Targets["1001"].Code != 200 || !validSMSAttemptToken(accepted.Targets["1001"].AttemptToken) || backend.count() != 0 {
		t.Fatal("two-Contact control lost its durable acceptance or reached a paid carrier")
	}
	smsMessageWireReleased(t, manager.runtime)
}

func TestSMSMessageWireActualManagersKeepIndependentSourceAndUDPAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	firstDB, secondDB := smsCASStores(t), smsCASStores(t)
	first, firstBackend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: firstDB[0]}, nil)
	second, secondBackend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: secondDB[0]}, nil)
	smsWireRetransmitTimeout(first)
	smsWireRetransmitTimeout(second)
	firstPhone, secondPhone := registerMessagePeer(t, first.runtime, "1001"), registerMessagePeer(t, second.runtime, "1001")
	message := smsCaptureMessage("same-logical-source-in-independent-databases", "synthetic independent Manager source")
	firstSaved, err := firstDB[0].SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	secondSaved, err := secondDB[0].SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	firstResult, secondResult := smsWireRetransmitStart(t, ctx, first), smsWireRetransmitStart(t, ctx, second)
	firstRequest, _, err := readFixedPeerMessage(firstPhone.conn)
	if err != nil || firstRequest == nil || string(firstRequest.Body()) != message.Body {
		t.Fatal("first real Manager did not send its legitimate source:", err)
	}
	secondRequest, _, err := readFixedPeerMessage(secondPhone.conn)
	if err != nil || secondRequest == nil || string(secondRequest.Body()) != message.Body {
		t.Fatal("second real Manager did not send its legitimate source:", err)
	}
	_, unknown := smsCaptureSavedInbox(t, firstDB[1], firstSaved)
	smsCaptureWireMutateSource(t, ctx, firstDB[1], firstSaved, "delete")
	// The unrelated Manager still owns a valid source and MUST retransmit.
	repeated, _, err := readFixedPeerMessage(secondPhone.conn)
	if err != nil || repeated == nil || repeated.String() != secondRequest.String() {
		t.Fatal("vetoing one Manager disabled another Manager's permitted UDP retransmission:", err)
	}
	response := sip.NewResponseFromRequest(repeated, 200, "Synthetic connected phone", nil)
	if _, err := secondPhone.conn.Write([]byte(response.String())); err != nil {
		t.Fatal(err)
	}
	smsWireRetransmitNoUDP(t, firstPhone.conn)
	smsCASDeliveryResult(t, ctx, firstResult)
	smsCASDeliveryResult(t, ctx, secondResult)
	smsWireRetransmitRequireSameUnknown(t, firstDB[1], firstSaved, "1001", unknown)
	_, accepted := smsCaptureSavedInbox(t, secondDB[1], secondSaved)
	if accepted.Targets["1001"].Code != 200 || !validSMSAttemptToken(accepted.Targets["1001"].AttemptToken) {
		t.Fatal("the independent Manager did not retain its own accepted source")
	}
	smsCaptureFreshWireControl(t, ctx, firstDB[0], first, firstPhone, "fresh-after-independent-manager-source-veto")
	smsMessageWireReleased(t, first.runtime)
	smsMessageWireReleased(t, second.runtime)
	if firstBackend.count() != 0 || secondBackend.count() != 0 {
		t.Fatal("independent MT wire source checks reached a paid carrier sender")
	}
}

func TestSMSMessageWireActualAuthenticationRetiresOldResponseBranch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	databases := smsCASStores(t)
	peer := newMessagePeerSocket(t)
	manager, backend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, peer)
	smsWireRetransmitTimeout(manager)
	saved, err := databases[0].SaveSMSMessage(ctx, smsCaptureMessage("auth-response-owner-retirement", "synthetic authenticated response ownership body"))
	if err != nil {
		t.Fatal(err)
	}
	result := smsWireRetransmitStart(t, ctx, manager)
	initial, source, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixedPeerResponse(peer, initial, source, 401, sip.NewHeader("WWW-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
		t.Fatal(err)
	}
	authenticated, remote, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPeerMessageDigest(authenticated, "Authorization"); err != nil {
		t.Fatal(err)
	}
	oldBranch, newBranch := initial.Via().Params.GetOr("branch", ""), authenticated.Via().Params.GetOr("branch", "")
	manager.runtime.mu.Lock()
	oldExists, newExists, count := manager.runtime.mtMessageWires[oldBranch] != nil, manager.runtime.mtMessageWires[newBranch] != nil, len(manager.runtime.mtMessageWires)
	manager.runtime.mu.Unlock()
	if oldBranch == newBranch || oldExists || !newExists || count != 1 {
		t.Fatal("authentication did not retire only the previous exact wire branch")
	}
	_, unknown := smsCaptureSavedInbox(t, databases[1], saved)
	if err := fixedPeerResponse(peer, initial, source, 200); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatal("a final on the retired initial branch completed the authenticated transaction:", err)
	case <-time.After(75 * time.Millisecond):
	}
	smsWireRetransmitRequireSameUnknown(t, databases[1], saved, peerSMSRecipientKey("sms-in"), unknown)
	if err := fixedPeerResponse(peer, authenticated, remote, 200); err != nil {
		t.Fatal(err)
	}
	smsCASDeliveryResult(t, ctx, result)
	_, accepted := smsCaptureSavedInbox(t, databases[1], saved)
	if accepted.Targets[peerSMSRecipientKey("sms-in")].Code != 200 || backend.count() != 0 {
		t.Fatal("the exact authenticated response was not accepted or invoked a carrier sender")
	}
	smsMessageWireReleased(t, manager.runtime)
}

func TestSMSMessageWireAuthenticatedRetryKeepsOriginalTotalDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	databases := smsCASStores(t)
	peer := newMessagePeerSocket(t)
	manager, _ := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, peer)
	r := manager.runtime
	target, valid := r.peerDeliveryTarget("sms-in")
	if !valid {
		t.Fatal("actual admitted peer did not provide its fixed destination")
	}
	max := sip.MaxForwardsHeader(70)
	incoming := sip.NewRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", Host: r.identityDomain()})
	incoming.AppendHeader(&max)
	// This is a wire-only timing control. The actual claim/Inbox authority is
	// exercised by the frozen dual-Store tests; here the passed absolute
	// deadline must survive both authentication and all real retransmissions.
	delivery, stop := context.WithTimeout(ctx, 2700*time.Millisecond)
	defer stop()
	result := make(chan messageOutcome, 1)
	go func() {
		result <- r.deliverMessage(delivery, incoming, Account{Username: "synthetic-source"}, Account{Username: "sms-in"}, target,
			[]byte("synthetic original-deadline peer body"), func() bool { return delivery.Err() == nil })
	}()
	first, _, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for a REAL permitted T1 retransmission before challenging, rather
	// than resetting the outer deadline with a sleep or a second invocation.
	repeated, source, err := readFixedPeerMessage(peer)
	if err != nil || repeated == nil || repeated.String() != first.String() {
		t.Fatal("the original transaction did not really retransmit before authentication:", err)
	}
	if err := fixedPeerResponse(peer, repeated, source, 407, sip.NewHeader("Proxy-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
		t.Fatal(err)
	}
	authenticated, _, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPeerMessageDigest(authenticated, "Proxy-Authorization"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		retry, _, err := readFixedPeerMessage(peer)
		if err != nil || retry == nil || retry.String() != authenticated.String() {
			t.Fatal("authenticated transaction did not retain its own real UDP retransmission identity:", err)
		}
	}
	<-delivery.Done()
	select {
	case outcome := <-result:
		if outcome.code != 504 {
			t.Fatal("an unanswered authenticated transaction was falsely confirmed:", outcome.code)
		}
	case <-time.After(350 * time.Millisecond):
		t.Fatal("authentication reset or extended the original overall MESSAGE deadline")
	}
	smsMessageWireReleased(t, r)
	smsCaptureWireNoUDP(t, peer)
}
