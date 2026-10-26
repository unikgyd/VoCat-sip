package sipgateway

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestSMSMessageWireActualAuthenticatedPayloadDoesNotEnterDiagnosticLogger(t *testing.T) {
	// No parallel test: temporarily capture the process diagnostic logger at
	// DEBUG. Runtime transports must continue using their private discard logger.
	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	databases := smsCASStores(t)
	peer := newMessagePeerSocket(t)
	manager, backend := smsCaptureWireManager(t, ctx, &smsCaptureWireAuthorityPause{Store: databases[0]}, peer)
	smsWireRetransmitTimeout(manager)
	message := smsCaptureMessage("diagnostic-wire-private-source", "synthetic private body diagnostic canary q7m4")
	saved, err := databases[0].SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	result := smsWireRetransmitStart(t, ctx, manager)
	initial, source, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixedPeerResponse(peer, initial, source, 407, sip.NewHeader("Proxy-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
		t.Fatal(err)
	}
	authenticated, remote, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPeerMessageDigest(authenticated, "Proxy-Authorization"); err != nil {
		t.Fatal(err)
	}
	if err := fixedPeerResponse(peer, authenticated, remote, 200); err != nil {
		t.Fatal(err)
	}
	smsCASDeliveryResult(t, ctx, result)
	_, accepted := smsCaptureSavedInbox(t, databases[1], saved)
	if accepted.Targets[peerSMSRecipientKey("sms-in")].Code != 200 || backend.count() != 0 {
		t.Fatal("private diagnostic fixture did not traverse only the real MT path")
	}
	manager.Close() // Drain this runtime before reading the logger's buffer.
	for _, private := range []string{message.Body, message.Peer, message.ICCID, authenticated.CallID().Value(),
		authenticated.From().Address.User, authenticated.GetHeader("Proxy-Authorization").Value(), messagePeerOutboundPassword} {
		if private != "" && strings.Contains(captured.String(), private) {
			t.Fatal("a private MT source, reply identity, body or credential appeared in diagnostic logs")
		}
	}
}
