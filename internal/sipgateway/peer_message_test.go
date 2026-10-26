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
	"github.com/icholy/digest"
	"vocat/internal/store"
)

const messagePeerPassword = "synthetic-peer-message-password"
const messagePeerOutboundPassword = "synthetic-peer-outbound-password"
const messagePeerRealm = "synthetic-pbx"

// This is an isolated UDP UAS, not Asterisk, a modem, or a carrier endpoint.
func newMessagePeerSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func messagePeerConfig(t *testing.T, peer *net.UDPConn, changes ...func(*Config)) stored {
	t.Helper()
	old := smsTestConfig(t)
	c := cloneConfig(old.Config)
	c.MessagesEnabled = true
	c.Peer = &PeerConfig{Enabled: true, Endpoint: peer.LocalAddr().String(), AllowSMSFromPeer: true, AllowSMSToPeer: true,
		SMSSendLines: []string{"module-a", "module-b"}, IncomingRoutes: []PeerIncomingRoute{{DeviceID: "module-a", SMSTarget: "sms-in"}, {DeviceID: "module-b", SMSTarget: "sms-in"}}}
	c.Peer.InboundAuth.Username = "pbx-in"
	c.Peer.InboundAuth.Password = messagePeerPassword
	c.Peer.OutboundAuth.Username = "gateway-out"
	c.Peer.OutboundAuth.Realm = messagePeerRealm
	c.Peer.OutboundAuth.Password = messagePeerOutboundPassword
	c.Peer.OutboundAuth.Algorithms = []string{"SHA-256", "MD5"}
	for i := range c.CarrierLines {
		c.CarrierLines[i].SMSRecipients = nil
	}
	for _, change := range changes {
		change(&c)
	}
	prepared, err := prepare(c, old, true)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func newPeerSMSTest(t *testing.T, changes ...func(*Config)) (*Manager, *syntheticSMS, *net.UDPConn) {
	t.Helper()
	peer := newMessagePeerSocket(t)
	config := messagePeerConfig(t, peer, changes...)
	db := carrierTestStore(t)
	b := newSyntheticSMS()
	s := newSMSService(db, b)
	r, err := start(config, runtimeOptions{sms: s})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{db: db, current: config, runtime: r, sms: s}
	t.Cleanup(m.Close)
	smsPeerReadinessOptIn(t, peer, r.conn.LocalAddr().String())
	return m, b, peer
}

func peerSMSPacket(target, from, callID, auth, body string, sequence int) string {
	return fmt.Sprintf("MESSAGE sip:%s@old-vpn.invalid SIP/2.0\r\nVia: SIP/2.0/UDP 192.0.2.1:9;branch=z9hG4bK-%s;rport\r\nFrom: \"External display\" <sip:%s@old-lan.invalid>;tag=peer-sender\r\nTo: <sip:%s@another.invalid>\r\nCall-ID: %s\r\nCSeq: %d MESSAGE\r\nMax-Forwards: 70\r\nContent-Type: text/plain; charset=utf-8\r\n%sContent-Length: %d\r\n\r\n%s", target, randomToken(), from, target, callID, sequence, auth, len(body), body)
}

func peerSMSExchange(t *testing.T, conn *net.UDPConn, r *runtime, packet string) *sip.Response {
	t.Helper()
	remote, err := net.ResolveUDPAddr("udp", r.conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WriteToUDP([]byte(packet), remote); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 16*1024)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			t.Fatal(err)
		}
		message, err := registrarParser().ParseSIP(buffer[:n])
		if err != nil {
			t.Fatal(err)
		}
		response, ok := message.(*sip.Response)
		if !ok {
			t.Fatal("unexpected request during inbound SMS test")
		}
		if response.StatusCode >= 200 {
			return response
		}
	}
}

func peerSMSAuth(t *testing.T, challenge *digest.Challenge, target, username, password string, count int) string {
	t.Helper()
	credentials, err := digest.Digest(challenge, digest.Options{Method: "MESSAGE", URI: "sip:" + target + "@old-vpn.invalid", Username: username, Password: password, Count: count, Cnonce: "peer-message-cnonce"})
	if err != nil {
		t.Fatal(err)
	}
	return "Authorization: " + credentials.String() + "\r\n"
}

func peerSMSChallenge(t *testing.T, peer *net.UDPConn, r *runtime, target string) *digest.Challenge {
	t.Helper()
	response := peerSMSExchange(t, peer, r, peerSMSPacket(target, "+15550000099", "challenge-"+randomToken(), "", "synthetic body", 1))
	if response.StatusCode != 401 {
		t.Fatalf("expected peer digest challenge, got %d", response.StatusCode)
	}
	challenge, err := digest.ParseChallenge(response.GetHeader("WWW-Authenticate").Value())
	if err != nil {
		t.Fatal(err)
	}
	return challenge
}

func TestPeerMessageCarrierSMSUsesIndependentPrincipalAndDurableRetry(t *testing.T) {
	m, b, peer := newPeerSMSTest(t, func(c *Config) { c.Accounts = nil })
	target := "82+15550000002"
	challenge := peerSMSChallenge(t, peer, m.runtime, target)
	for count := 1; count <= 2; count++ {
		auth := peerSMSAuth(t, challenge, target, "pbx-in", messagePeerPassword, count)
		from := "+15550000099"
		if count == 2 {
			from = "1001" // Presentation aliases do not create a new principal.
		}
		response := peerSMSExchange(t, peer, m.runtime, peerSMSPacket(target, from, "same-paid-operation", auth, "peer synthetic SMS", 2))
		if response.StatusCode != 202 {
			t.Fatalf("paid SMS status %d", response.StatusCode)
		}
	}
	if sent := b.sentSnapshot(); len(sent) != 1 || sent[0] != [3]string{"module-b", "+15550000002", "peer synthetic SMS"} {
		t.Fatal("peer did not retain exact line/operation", sent)
	}
	auth := peerSMSAuth(t, challenge, target, "pbx-in", messagePeerPassword, 3)
	response := peerSMSExchange(t, peer, m.runtime, peerSMSPacket(target, "+15550000099", "same-paid-operation", auth, "changed private body", 2))
	if response.StatusCode != 400 || b.count() != 1 {
		t.Fatal("conflicting retry created a paid SMS", response.StatusCode, b.count())
	}
	if len(m.runtime.status().Bindings) != 0 {
		t.Fatal("fixed peer was inserted into local registrations")
	}
}

func TestPeerMessageInboundGrantsAndObservedSourceCannotBeBypassed(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		target string
		change func(*Config)
		other  bool
	}{
		{name: "local-extension", target: "1002"},
		{name: "line-denied", target: "82+15550000002", change: func(c *Config) { c.Peer.SMSSendLines = []string{"module-a"} }},
		{name: "direction-denied", target: "+15550000002", change: func(c *Config) { c.Peer.AllowSMSFromPeer = false }},
		{name: "foreign-source", target: "+15550000002", other: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var changes []func(*Config)
			if scenario.change != nil {
				changes = append(changes, scenario.change)
			}
			m, b, peer := newPeerSMSTest(t, changes...)
			if scenario.name == "direction-denied" {
				response := peerSMSExchange(t, peer, m.runtime, peerSMSPacket(scenario.target, "1001", "no-permission", "", "not submitted", 2))
				if response.StatusCode != 403 || b.count() != 0 {
					t.Fatal("disabled peer SMS reached authentication or transport")
				}
				return
			}
			challenge := peerSMSChallenge(t, peer, m.runtime, "+15550000002")
			auth := peerSMSAuth(t, challenge, scenario.target, "pbx-in", messagePeerPassword, 1)
			if scenario.other {
				peer = newMessagePeerSocket(t)
			}
			response := peerSMSExchange(t, peer, m.runtime, peerSMSPacket(scenario.target, "1001", "no-permission", auth, "not submitted", 2))
			want := 403
			if scenario.other {
				want = 401 // Foreign sockets never enter the peer auth realm.
			}
			if response.StatusCode != want || b.count() != 0 {
				t.Fatal("peer authorization bypass", response.StatusCode, b.count())
			}
		})
	}
}

func TestPeerMessageOfflineLocalExtensionNeverFallsBackToFixedPeer(t *testing.T) {
	m, b, peer := newPeerSMSTest(t, func(c *Config) {
		for i := range c.Accounts {
			c.Accounts[i].AllowMessages = true
		}
	})
	c := newWireClient(t, m.runtime)
	c.messageChallenge("1002", "local message", "text/plain")
	response := c.exchange(c.messagePacket("1002", "local message", "text/plain", c.messageAuthorization("1002"), 2, ""))
	requireStatus(t, response, 480)
	if b.count() != 0 {
		t.Fatal("offline extension escaped to carrier SMS")
	}
	assertNoFixedPeerMessage(t, peer)
}

func readFixedPeerMessage(conn *net.UDPConn) (*sip.Request, *net.UDPAddr, error) {
	return readOptedInFixedPeerMessage(conn)
}

func fixedPeerResponse(conn *net.UDPConn, request *sip.Request, source *net.UDPAddr, code int, headers ...sip.Header) error {
	response := sip.NewResponseFromRequest(request, code, "Synthetic fixed peer", nil)
	for _, header := range headers {
		response.AppendHeader(header)
	}
	_, err := conn.WriteToUDP([]byte(response.String()), source)
	return err
}

func verifyPeerMessageDigest(request *sip.Request, header string) error {
	values := request.GetHeaders(header)
	if len(values) != 1 {
		return fmt.Errorf("missing/duplicate outbound digest")
	}
	fields, err := parseDigest(values[0].Value())
	if err != nil {
		return err
	}
	if fields["username"] != "gateway-out" || fields["realm"] != messagePeerRealm || fields["uri"] != request.Recipient.String() || fields["algorithm"] != "SHA-256" || fields["qop"] != "auth" || fields["nonce"] != "synthetic-message-nonce" {
		return fmt.Errorf("incorrect independent outbound digest fields")
	}
	ha1 := digestHash("SHA-256", "gateway-out:"+messagePeerRealm+":"+messagePeerOutboundPassword)
	want := digestHash("SHA-256", ha1+":"+fields["nonce"]+":"+fields["nc"]+":"+fields["cnonce"]+":auth:"+digestHash("SHA-256", "MESSAGE:"+request.Recipient.String()))
	if fields["response"] != want {
		return fmt.Errorf("outbound digest did not use independent credential")
	}
	return nil
}

type fixedMessageResult struct {
	first, second *sip.Request
	err           error
}

func TestPeerMessageIncomingSMSAuthenticatedRetryPreservesLogicalDelivery(t *testing.T) {
	for _, code := range []int{401, 407} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			m, _, peer := newPeerSMSTest(t, func(c *Config) { c.Accounts = nil })
			message := IncomingSMS{ID: "received-synthetic", DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "private synthetic SMS"}
			if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
				t.Fatal(err)
			}
			results := make(chan fixedMessageResult, 1)
			go func() {
				result := fixedMessageResult{}
				first, source, err := readFixedPeerMessage(peer)
				result.first, result.err = first, err
				challengeHeader, authorizationHeader := "WWW-Authenticate", "Authorization"
				if code == 407 {
					challengeHeader, authorizationHeader = "Proxy-Authenticate", "Proxy-Authorization"
				}
				if err == nil {
					err = fixedPeerResponse(peer, first, source, code, sip.NewHeader(challengeHeader, `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`))
				}
				if err == nil {
					result.second, source, err = readFixedPeerMessage(peer)
				}
				if err == nil {
					err = verifyPeerMessageDigest(result.second, authorizationHeader)
				}
				if err == nil {
					err = fixedPeerResponse(peer, result.second, source, 202)
				}
				result.err = err
				results <- result
			}()
			if err := m.DeliverSMSOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			first, second := result.first, result.second
			if first.Recipient.User != "sms-in" || first.To().Address.User != "sms-in" || first.From().Address.User != smsReplyUser(message.DeviceID, message.Subscription, message.Sender) || string(first.Body()) != message.Text || first.From().DisplayName != message.Sender {
				t.Fatal("wrong SMS peer identity")
			}
			if first.CallID().Value() != second.CallID().Value() || first.CSeq().SeqNo+1 != second.CSeq().SeqNo || first.Via().Params.GetOr("branch", "") == second.Via().Params.GetOr("branch", "") || first.From().Value() != second.From().Value() || first.To().Value() != second.To().Value() || string(first.Body()) != string(second.Body()) {
				t.Fatal("authentication retry changed logical delivery or reused branch")
			}
			if stats := m.sms.statistics(); stats.IncomingAccepted != 1 || stats.IncomingQueued != 0 || stats.IncomingUnknown != 0 {
				t.Fatal(stats)
			}
			if err := m.DeliverSMSOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertNoFixedPeerMessage(t, peer)
		})
	}
}

func assertNoFixedPeerMessage(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, _, err := conn.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatal("unexpected peer packet or duplicate private SMS")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
}

func TestPeerMessageBadRealmOrRepeatedChallengeNeverGetsMoreCredentials(t *testing.T) {
	for _, scenario := range []string{"wrong-realm", "repeated-challenge"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, peer := newPeerSMSTest(t)
			message := IncomingSMS{ID: scenario, DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "private synthetic SMS"}
			if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				request, source, err := readFixedPeerMessage(peer)
				realm := messagePeerRealm
				if scenario == "wrong-realm" {
					realm = "foreign-realm"
				}
				challenge := sip.NewHeader("WWW-Authenticate", `Digest realm="`+realm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)
				if err == nil {
					err = fixedPeerResponse(peer, request, source, 401, challenge)
				}
				if err == nil && scenario == "repeated-challenge" {
					request, source, err = readFixedPeerMessage(peer)
					if err == nil {
						err = verifyPeerMessageDigest(request, "Authorization")
					}
					if err == nil {
						err = fixedPeerResponse(peer, request, source, 401, challenge)
					}
				}
				result <- err
			}()
			if err := m.DeliverSMSOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if m.sms.statistics().IncomingRejected != 1 {
				t.Fatal("invalid challenge was accepted", m.sms.statistics())
			}
			assertNoFixedPeerMessage(t, peer)
		})
	}
}

func restartMessagePeerRuntime(t *testing.T, m *Manager, config stored) {
	t.Helper()
	m.runtime.close()
	s := newSMSService(m.db, m.sms.backend)
	r, err := start(config, runtimeOptions{sms: s})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.runtime, m.current, m.sms = r, config, s
	m.mu.Unlock()
	if config.Config.Peer != nil {
		smsPeerReadinessRefreshOptIn(t, config.Config.Peer.Endpoint, r.conn.LocalAddr().String())
	}
}

func TestPeerSMSQueuedPrivateRecipientIsPinnedAcrossRotation(t *testing.T) {
	for _, scenario := range []string{"outbound-password", "inbound-password", "endpoint", "realm", "target", "direction", "line"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, peer := newPeerSMSTest(t)
			message := IncomingSMS{ID: "queued-private", DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "must not inherit"}
			if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
				t.Fatal(err)
			}
			c := cloneConfig(m.current.Config)
			var nextPeer *net.UDPConn
			switch scenario {
			case "outbound-password":
				c.Peer.OutboundAuth.Password = messagePeerOutboundPassword + "-rotated"
			case "inbound-password":
				c.Peer.InboundAuth.Password = messagePeerPassword + "-rotated"
			case "endpoint":
				nextPeer = newMessagePeerSocket(t)
				c.Peer.Endpoint = nextPeer.LocalAddr().String()
			case "realm":
				c.Peer.OutboundAuth.Realm = "new-synthetic-realm"
				c.Peer.OutboundAuth.Password = messagePeerOutboundPassword
			case "target":
				c.Peer.IncomingRoutes[0].SMSTarget = "new-target"
			case "direction":
				c.Peer.AllowSMSToPeer = false
			case "line":
				c.CarrierLines[0].AllowSMSReceive = false
			}
			prepared, err := prepare(c, m.current, true)
			if err != nil {
				t.Fatal(err)
			}
			restartMessagePeerRuntime(t, m, prepared)
			if err := m.DeliverSMSOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stats := m.sms.statistics(); stats.IncomingRejected != 1 || stats.IncomingQueued != 0 {
				t.Fatal("new peer inherited queued private SMS", stats)
			}
			assertNoFixedPeerMessage(t, peer)
			if nextPeer != nil {
				assertNoFixedPeerMessage(t, nextPeer)
			}
		})
	}
}

func TestPeerSMSUnknownRemainsTerminalAcrossStoreAndListenerRestart(t *testing.T) {
	m, _, peer := newPeerSMSTest(t)
	message := IncomingSMS{ID: "unknown-private", DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "not replayed"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.DeliverSMSOnce(ctx) }()
	if _, _, err := readFixedPeerMessage(peer); err != nil {
		t.Fatal(err)
	}
	// Cancellation after a real packet was observed is definitely an unknown
	// attempt, not a fixture timeout that may have fired before any UDP send.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown delivery did not stop")
	}
	if m.sms.statistics().IncomingUnknown != 1 {
		t.Fatal(m.sms.statistics())
	}
	restartMessagePeerRuntime(t, m, m.current)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.sms.statistics().IncomingUnknown != 1 {
		t.Fatal("unknown recipient was lost on restart")
	}
	assertNoFixedPeerMessage(t, peer)
}

func TestPeerSMSCredentialRotationCannotResubmitOrInheritPaidOperation(t *testing.T) {
	m, b, peer := newPeerSMSTest(t)
	target := "+15550000001"
	challenge := peerSMSChallenge(t, peer, m.runtime, target)
	auth := peerSMSAuth(t, challenge, target, "pbx-in", messagePeerPassword, 1)
	packet := peerSMSPacket(target, "+15550000099", "stable-paid-operation", auth, "same private body", 2)
	if response := peerSMSExchange(t, peer, m.runtime, packet); response.StatusCode != 202 || b.count() != 1 {
		t.Fatal("initial paid operation not recorded", response.StatusCode, b.count())
	}
	c := cloneConfig(m.current.Config)
	c.Peer.InboundAuth.Password = messagePeerPassword + "-rotated"
	prepared, err := prepare(c, m.current, true)
	if err != nil {
		t.Fatal(err)
	}
	restartMessagePeerRuntime(t, m, prepared)
	challenge = peerSMSChallenge(t, peer, m.runtime, target)
	auth = peerSMSAuth(t, challenge, target, "pbx-in", messagePeerPassword+"-rotated", 1)
	packet = peerSMSPacket(target, "+15550000099", "stable-paid-operation", auth, "same private body", 2)
	if response := peerSMSExchange(t, peer, m.runtime, packet); response.StatusCode != 400 || b.count() != 1 {
		t.Fatal("new authority inherited/repeated previous paid operation", response.StatusCode, b.count())
	}
}

func TestPeerSMSMalformedRecipientCannotMasqueradeAsLegacyLocal(t *testing.T) {
	m, _, _ := newPeerSMSTest(t)
	for _, recipient := range []smsRecipient{
		{Identity: strings.Repeat("a", 64), Kind: "peer", User: "sms-in"},
		{Identity: strings.Repeat("a", 64), Kind: "other"},
		{Identity: strings.Repeat("a", 64), User: "sms-in"},
	} {
		state := smsLedger{Version: 1, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{
			strings.Repeat("a", 64): {Message: IncomingSMS{ID: "corrupt", DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "private"}, Until: time.Now().Add(time.Hour), Targets: map[string]smsRecipient{"1001": recipient}},
		}}
		// Deliberate corrupt-history fixture, not a production blind overwrite.
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.db.UpsertAppSetting(context.Background(), store.AppSetting{Key: SMSLedgerSettingKey, Value: data, Sensitive: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.sms.load(context.Background()); err == nil {
			t.Fatal("invalid peer/local recipient accepted")
		}
	}
}

func TestPeerMessageForeignSourceOrMismatchedChallengeCannotObtainCredentials(t *testing.T) {
	m, _, peer := newPeerSMSTest(t)
	message := IncomingSMS{ID: "strict-response-identity", DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "private synthetic SMS"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.DeliverSMSOnce(context.Background()) }()
	request, source, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	challenge := sip.NewResponseFromRequest(request, 401, "Synthetic challenge", nil)
	challenge.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`))
	foreign := newMessagePeerSocket(t)
	if _, err := foreign.WriteToUDP([]byte(challenge.String()), source); err != nil {
		t.Fatal(err)
	}
	assertNoFixedPeerMessage(t, peer)
	for _, mutate := range []func(*sip.Response){
		func(response *sip.Response) {
			// sipgo treats Call-ID as immutable and headerClone shares its
			// pointer. Replace the header, never mutate the original challenge.
			response.RemoveHeader("Call-ID")
			value := sip.CallIDHeader("foreign-call-id")
			response.AppendHeader(&value)
		},
		func(response *sip.Response) { response.CSeq().SeqNo++ },
		func(response *sip.Response) { response.From().Params.Add("tag", "foreign-tag") },
	} {
		bad := challenge.Clone()
		mutate(bad)
		if _, err := peer.WriteToUDP([]byte(bad.String()), source); err != nil {
			t.Fatal(err)
		}
		assertNoFixedPeerMessage(t, peer)
	}
	if _, err := peer.WriteToUDP([]byte(challenge.String()), source); err != nil {
		t.Fatal(err)
	}
	var retry *sip.Request
	for i := 0; i < 4; i++ {
		retry, source, err = readFixedPeerMessage(peer)
		if err != nil {
			t.Fatal(err)
		}
		if retry.CSeq().SeqNo != request.CSeq().SeqNo {
			break
		}
		// A previously queued retransmission of the initial UDP request is
		// not a new authenticated attempt and does not invalidate the gate.
		if len(retry.GetHeaders("Authorization"))+len(retry.GetHeaders("Proxy-Authorization")) != 0 || retry.Via().Params.GetOr("branch", "") != request.Via().Params.GetOr("branch", "") {
			t.Fatal("foreign challenge created an authenticated attempt")
		}
	}
	if err := verifyPeerMessageDigest(retry, "Authorization"); err != nil {
		t.Fatal(err)
	}
	if err := fixedPeerResponse(peer, retry, source, 200); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("valid exact-identity peer response was not accepted")
	}
	if m.sms.statistics().IncomingAccepted != 1 {
		t.Fatal(m.sms.statistics())
	}
}

func TestPeerMessageAuthenticatedRetryStillUsesOriginalCancellation(t *testing.T) {
	m, _, peer := newPeerSMSTest(t)
	message := IncomingSMS{ID: "cancel-authenticated-retry", DeviceID: "module-a", Subscription: "fixed-sim", Sender: "+15550000001", Text: "unknown private SMS"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.DeliverSMSOnce(ctx) }()
	first, source, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixedPeerResponse(peer, first, source, 401, sip.NewHeader("WWW-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
		t.Fatal(err)
	}
	second, _, err := readFixedPeerMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPeerMessageDigest(second, "Authorization"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("authentication retry detached from caller cancellation")
	}
	if stats := m.sms.statistics(); stats.IncomingUnknown != 1 || stats.IncomingQueued != 0 {
		t.Fatal("cancelled wire attempt was made safe to replay", stats)
	}
	restartMessagePeerRuntime(t, m, m.current)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoFixedPeerMessage(t, peer)
}

func TestPeerSMSRestartKeepsExplicitLineTargetAndLegacyLocalRecipient(t *testing.T) {
	m, _, peer := newPeerSMSTest(t, func(c *Config) {
		c.CarrierLines[1].SMSRecipients = []string{"1002"}
		c.Peer.IncomingRoutes[1].SMSTarget = "line-b-sms"
	})
	message := IncomingSMS{ID: "restart-queued", DeviceID: "module-b", Subscription: "fixed-sim", Sender: "+15550000002", Text: "private queued SMS"}
	if err := m.QueueSMS(context.Background(), 1, &message); err != nil {
		t.Fatal(err)
	}
	restartMessagePeerRuntime(t, m, m.current)
	result := make(chan error, 1)
	go func() {
		request, source, err := readFixedPeerMessage(peer)
		if err == nil && (request.Recipient.User != "line-b-sms" || string(request.Body()) != message.Text || request.From().Address.User != smsReplyUser(message.DeviceID, message.Subscription, message.Sender)) {
			err = fmt.Errorf("restarted delivery lost explicit line target or reply alias")
		}
		if err == nil {
			err = fixedPeerResponse(peer, request, source, 200)
		}
		result <- err
	}()
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if stats := m.sms.statistics(); stats.IncomingQueued != 1 || stats.IncomingAccepted != 1 {
		t.Fatal("peer delivery consumed the offline local recipient", stats)
	}
	local := registerMessagePeer(t, m.runtime, "1002")
	delivery := replyOneMessage(local.conn, 200)
	if err := m.DeliverSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := <-delivery; result.err != nil || result.request.To().Address.User != "1002" {
		t.Fatal("legacy local queue changed", result.err)
	}
	if stats := m.sms.statistics(); stats.IncomingQueued != 0 || stats.IncomingAccepted != 2 {
		t.Fatal(stats)
	}
	assertNoFixedPeerMessage(t, peer)
}

type peerSMSReadyBarrier struct {
	CarrierSMSBackend
	entered chan string
	release <-chan struct{}
}

func (b *peerSMSReadyBarrier) SMSReady(ctx context.Context, id string) (string, error) {
	b.entered <- id
	select {
	case <-b.release:
		return b.CarrierSMSBackend.SMSReady(ctx, id)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestPeerSMSConcurrentResolvedLineChangeCannotReserveTheSamePaidOperationTwice(t *testing.T) {
	backend := newSyntheticSMS()
	release := make(chan struct{})
	barrier := &peerSMSReadyBarrier{CarrierSMSBackend: backend, entered: make(chan string, 2), release: release}
	service := newSMSService(carrierTestStore(t), barrier)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan int, 2)
	operation := hashCallOperation("same-peer-paid-operation")
	for _, id := range []string{"module-a", "module-b"} {
		go func(id string) {
			results <- service.attempt(ctx, operation, hashCallOperation(id+"-resolved-body"), id, "+15550000001", "synthetic SMS", "")
		}(id)
	}
	// Both operations have passed the initial journal read before either
	// readiness call returns. No scheduling/sleep assumption is involved.
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.entered:
		case <-ctx.Done():
			t.Fatal("both line readiness barriers were not entered")
		}
	}
	close(release)
	codes := map[int]int{}
	for i := 0; i < 2; i++ {
		select {
		case code := <-results:
			codes[code]++
		case <-ctx.Done():
			t.Fatal("SMS attempts did not finish")
		}
	}
	if codes[202] != 1 || codes[400] != 1 || backend.count() != 1 {
		t.Fatal("same paid operation crossed line concurrency fence", codes, backend.count())
	}
}
