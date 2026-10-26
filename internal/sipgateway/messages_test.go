package sipgateway

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

func testMessageRuntime(t *testing.T, changes ...func(*Config)) *runtime {
	t.Helper()
	config := testConfig(t, "SHA-256")
	config.Config.MessagesEnabled = true
	config.Config.IdentityDomain = "voice.example.test"
	for i := range config.Config.Accounts {
		config.Config.Accounts[i].AllowMessages = true
		config.Config.Accounts[i].DisplayName = "Desk " + config.Config.Accounts[i].Username
	}
	for _, change := range changes {
		if change != nil {
			change(&config.Config)
		}
	}
	r, err := start(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r
}
func registerMessagePeer(t *testing.T, r *runtime, user string) *wireClient {
	t.Helper()
	c := newWireClient(t, r)
	c.callID = "register-" + randomToken()
	res := c.exchange(c.packet("REGISTER", user, 1, "", ""))
	requireStatus(t, res, 401)
	challenge, err := digest.ParseChallenge(res.headers["www-authenticate"][0])
	if err != nil {
		t.Fatal(err)
	}
	c.challenge = challenge
	c.count = 1
	password := testPassword
	if user == "1002" {
		password += "-two"
	}
	credentials, err := digest.Digest(challenge, digest.Options{Method: "REGISTER", URI: "sip:vocat", Username: user, Password: password, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately unreachable Contact host. Only the observed source may be
	// used for sending, while the opaque Contact user is retained for the UAS.
	contact := fmt.Sprintf("Contact: <sip:device-%s@unreachable.invalid:%d>\r\n", user, c.conn.LocalAddr().(*net.UDPAddr).Port)
	requireStatus(t, c.exchange(c.packet("REGISTER", user, 2, "Authorization: "+credentials.String()+"\r\n", contact)), 200)
	return c
}
func (c *wireClient) messagePacket(target, body, mediaType, auth string, cseq int, extra string) string {
	c.serial++
	return fmt.Sprintf("MESSAGE sip:%s@stale-vpn.invalid SIP/2.0\r\nVia: SIP/2.0/UDP 192.0.2.10:9;branch=z9hG4bK-message-%s-%d;rport\r\nFrom: \"Untrusted display\" <sip:1001@stale-lan.invalid>;tag=message-from\r\nTo: <sip:%s@different-domain.invalid>\r\nCall-ID: message-operation\r\nCSeq: %d MESSAGE\r\nMax-Forwards: 70\r\nContent-Type: %s\r\n%s%sContent-Length: %d\r\n\r\n%s", target, c.branch, c.serial, target, cseq, mediaType, auth, extra, len(body), body)
}
func (c *wireClient) messageChallenge(target, body, mediaType string) {
	c.t.Helper()
	res := c.exchange(c.messagePacket(target, body, mediaType, "", 1, ""))
	requireStatus(c.t, res, 401)
	var err error
	c.challenge, err = digest.ParseChallenge(res.headers["www-authenticate"][0])
	if err != nil {
		c.t.Fatal(err)
	}
}
func (c *wireClient) messageAuthorization(target string) string {
	c.t.Helper()
	c.count++
	credentials, err := digest.Digest(c.challenge, digest.Options{Method: "MESSAGE", URI: "sip:" + target + "@stale-vpn.invalid", Username: "1001", Password: testPassword, Count: c.count, Cnonce: "message-test-cnonce"})
	if err != nil {
		c.t.Fatal(err)
	}
	return "Authorization: " + credentials.String() + "\r\n"
}
func readPeerMessage(conn *net.UDPConn) (*sip.Request, error) {
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 16*1024)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil, err
	}
	msg, err := registrarParser().ParseSIP(buffer[:n])
	if err != nil {
		return nil, err
	}
	request, ok := msg.(*sip.Request)
	if !ok {
		return nil, fmt.Errorf("not a request")
	}
	return request, nil
}

type peerResult struct {
	request *sip.Request
	err     error
}

func replyOneMessage(conn *net.UDPConn, code int) <-chan peerResult {
	result := make(chan peerResult, 1)
	go func() {
		request, err := readPeerMessage(conn)
		if err == nil {
			response := sip.NewResponseFromRequest(request, code, "Test final", nil)
			_, err = conn.Write([]byte(response.String()))
		}
		result <- peerResult{request, err}
	}()
	return result
}

func TestMessageWireRoutingIdentityAndDelivery(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	body := "分机消息 · hello"
	sender.messageChallenge("1002", body, "text/plain; charset=utf-8")
	delivery := replyOneMessage(receiver.conn, 200)
	extra := "Route: <sip:external.invalid;lr>\r\nP-Asserted-Identity: <sip:spoof@external.invalid>\r\nReply-To: <sip:spoof@external.invalid>\r\n"
	packet := sender.messagePacket("1002", body, "text/plain; charset=utf-8", sender.messageAuthorization("1002"), 2, extra)
	res := sender.exchange(packet)
	requireStatus(t, res, 200)
	actual := <-delivery
	if actual.err != nil {
		t.Fatal(actual.err)
	}
	request := actual.request
	if request.Recipient.User != "device-1002" || request.From().Address.User != "1001" || request.From().Address.Host != "voice.example.test" || request.To().Address.User != "1002" || request.To().Address.Host != "voice.example.test" || request.From().DisplayName != "Desk 1001" || string(request.Body()) != body {
		t.Fatalf("wrong forwarded identity/body: %s", request)
	}
	if len(request.GetHeaders("Authorization")) != 0 || len(request.GetHeaders("Route")) != 0 || len(request.GetHeaders("P-Asserted-Identity")) != 0 || len(request.GetHeaders("Reply-To")) != 0 {
		t.Fatal("untrusted hop or credentials forwarded")
	}
	if len(res.headers["contact"]) != 0 || !strings.HasSuffix(res.raw, "\r\n\r\n") {
		t.Fatal("MESSAGE success included a Contact or body")
	}
	if request.Via().Port != r.conn.LocalAddr().(*net.UDPAddr).Port {
		t.Fatal("delivery used a different signaling socket")
	}
	if stats := r.status().Messages; stats.Confirmed != 1 || stats.Active != 0 {
		t.Fatalf("wrong message stats %+v", stats)
	}
}

func TestMessageFinalResultsAreNotInvented(t *testing.T) {
	for _, scenario := range []struct{ remote, want int }{{200, 200}, {202, 202}, {401, 403}, {407, 403}, {302, 403}, {403, 403}, {415, 415}, {480, 480}, {503, 503}, {500, 502}} {
		t.Run(fmt.Sprint(scenario.remote), func(t *testing.T) {
			r := testMessageRuntime(t)
			receiver := registerMessagePeer(t, r, "1002")
			sender := newWireClient(t, r)
			sender.messageChallenge("1002", "hello", "text/plain")
			delivery := replyOneMessage(receiver.conn, scenario.remote)
			requireStatus(t, sender.exchange(sender.messagePacket("1002", "hello", "text/plain", sender.messageAuthorization("1002"), 2, "")), scenario.want)
			if result := <-delivery; result.err != nil {
				t.Fatal(result.err)
			}
		})
	}
}

func TestMessagePermissionsAndPayloadValidation(t *testing.T) {
	for _, scenario := range []struct {
		name                string
		change              func(*Config)
		target, body, media string
		want                int
	}{
		{"sender-permission", func(c *Config) { c.Accounts[0].AllowMessages = false }, "1002", "hello", "text/plain", 403},
		{"receiver-permission", func(c *Config) { c.Accounts[1].AllowMessages = false }, "1002", "hello", "text/plain", 403},
		{"unknown-user", nil, "1999", "hello", "text/plain", 404},
		{"offline-user", nil, "1002", "hello", "text/plain", 480},
		{"html", nil, "1002", "<b>hello</b>", "text/html", 415},
		{"cpim", nil, "1002", "hello", "message/cpim", 415},
		{"charset", nil, "1002", "hello", "text/plain; charset=gbk", 415},
		{"false-ascii", nil, "1002", "非ASCII", "text/plain; charset=us-ascii", 400},
		{"empty", nil, "1002", "", "text/plain", 400},
		{"invalid-utf8", nil, "1002", string([]byte{255, 254}), "text/plain", 400},
		{"nul", nil, "1002", "bad\x00text", "text/plain", 400},
		{"oversized", nil, "1002", strings.Repeat("x", 4097), "text/plain", 413},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := testMessageRuntime(t, scenario.change)
			c := newWireClient(t, r)
			c.messageChallenge(scenario.target, scenario.body, scenario.media)
			requireStatus(t, c.exchange(c.messagePacket(scenario.target, scenario.body, scenario.media, c.messageAuthorization(scenario.target), 2, "")), scenario.want)
		})
	}
}

func TestMessageRetryCacheDoesNotSendTwice(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "hello", "text/plain")
	delivery := replyOneMessage(receiver.conn, 200)
	packet := sender.messagePacket("1002", "hello", "text/plain", sender.messageAuthorization("1002"), 2, "")
	requireStatus(t, sender.exchange(packet), 200)
	if result := <-delivery; result.err != nil {
		t.Fatal(result.err)
	}
	requireStatus(t, sender.exchange(packet), 200)                                                                                          // same UDP transaction
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "hello", "text/plain", sender.messageAuthorization("1002"), 2, "")), 200) // new branch, same operation
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "changed", "text/plain", sender.messageAuthorization("1002"), 2, "")), 400)
	receiver.conn.SetDeadline(time.Now().Add(120 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("retry or conflict sent a duplicate")
	}
	if r.status().Messages.Confirmed != 1 {
		t.Fatal("retry counted as a new send")
	}
}

func TestMessageTimeoutIsUnknownAndDoesNotAutomaticallyRetry(t *testing.T) {
	r := testMessageRuntime(t)
	// Every MESSAGE admission takes r.mu before the delivery worker reads
	// the fixture timeout. Publish it before sending any MESSAGE.
	r.mu.Lock()
	r.messageTimeout = 100 * time.Millisecond
	r.mu.Unlock()
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "hello", "text/plain")
	got := make(chan peerResult, 1)
	go func() { request, err := readPeerMessage(receiver.conn); got <- peerResult{request, err} }()
	res := sender.exchange(sender.messagePacket("1002", "hello", "text/plain", sender.messageAuthorization("1002"), 2, ""))
	requireStatus(t, res, 504)
	if strings.Join(res.headers["x-vocat-delivery"], "") != "unknown" {
		t.Fatal("ambiguous delivery was not marked unknown")
	}
	if result := <-got; result.err != nil {
		t.Fatal(result.err)
	}
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "hello", "text/plain", sender.messageAuthorization("1002"), 2, "")), 504)
	receiver.conn.SetDeadline(time.Now().Add(120 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("ambiguous operation was sent again")
	}
	if r.status().Messages.Unknown != 1 {
		t.Fatal("unknown send counted incorrectly")
	}
}

func TestMessageFanoutToSeveralContacts(t *testing.T) {
	r := testMessageRuntime(t)
	one := registerMessagePeer(t, r, "1002")
	two := registerMessagePeer(t, r, "1002")
	if len(r.status().Bindings) != 2 {
		t.Fatal("two real registrations were not retained")
	}
	first, second := replyOneMessage(one.conn, 200), replyOneMessage(two.conn, 403)
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "fanout", "text/plain")
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "fanout", "text/plain", sender.messageAuthorization("1002"), 2, "")), 200)
	for _, result := range []<-chan peerResult{first, second} {
		if actual := <-result; actual.err != nil {
			t.Fatal(actual.err)
		}
	}
}

func TestMessageShutdownCancelsPendingDelivery(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "pending", "text/plain")
	packet := sender.messagePacket("1002", "pending", "text/plain", sender.messageAuthorization("1002"), 2, "")
	sender.conn.Write([]byte(packet))
	if _, err := readPeerMessage(receiver.conn); err != nil {
		t.Fatal(err)
	}
	var snapshots sync.WaitGroup
	snapshots.Add(1)
	go func() {
		defer snapshots.Done()
		for i := 0; i < 20; i++ {
			r.status()
		}
	}()
	started := time.Now()
	r.close()
	snapshots.Wait()
	if time.Since(started) > time.Second {
		t.Fatal("shutdown waited for remote peer timeout")
	}
	if r.status().Messages.Active != 0 {
		t.Fatal("shutdown retained pending jobs")
	}
}

func TestMessageForgedAndMismatchedResponsesCannotConfirmDelivery(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "verify reply", "text/plain")
	sender.conn.Write([]byte(sender.messagePacket("1002", "verify reply", "text/plain", sender.messageAuthorization("1002"), 2, "")))
	request, err := readPeerMessage(receiver.conn)
	if err != nil {
		t.Fatal(err)
	}
	response := sip.NewResponseFromRequest(request, 200, "OK", nil).String()
	foreign := newWireClient(t, r)
	foreign.conn.Write([]byte(response))
	badID := strings.Replace(response, "Call-ID: "+request.CallID().Value(), "Call-ID: forged-id", 1)
	receiver.conn.Write([]byte(badID))
	sender.conn.SetReadDeadline(time.Now().Add(70 * time.Millisecond))
	if _, err := sender.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("forged response caused a delivery result")
	}
	if r.status().Messages.Active != 1 {
		t.Fatal("forged response terminated the real transaction")
	}
	receiver.conn.Write([]byte(response))
	sender.conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 4096)
	n, err := sender.conn.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(buffer[:n]), "SIP/2.0 200 ") {
		t.Fatalf("valid response rejected: %s", buffer[:n])
	}
}

func TestMessageRejectsOverlappingDestinationWithoutDuplicate(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "first", "text/plain")
	first := sender.messagePacket("1002", "first", "text/plain", sender.messageAuthorization("1002"), 2, "")
	sender.conn.Write([]byte(first))
	request, err := readPeerMessage(receiver.conn)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "second", "text/plain", sender.messageAuthorization("1002"), 3, "")), 503)
	receiver.conn.SetReadDeadline(time.Now().Add(70 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("overlapping MESSAGE forwarded")
	}
	receiver.conn.Write([]byte(sip.NewResponseFromRequest(request, 200, "OK", nil).String()))
	sender.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := sender.conn.Read(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	delivery := replyOneMessage(receiver.conn, 200)
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "second", "text/plain", sender.messageAuthorization("1002"), 3, "")), 200)
	if actual := <-delivery; actual.err != nil {
		t.Fatal(actual.err)
	}
}

func TestMessageDuplicateSocketGetsOneCopy(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	credential, err := digest.Digest(receiver.challenge, digest.Options{Method: "REGISTER", URI: "sip:vocat", Username: "1002", Password: testPassword + "-two", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, receiver.exchange(receiver.packet("REGISTER", "1002", 3, "Authorization: "+credential.String()+"\r\n", "Contact: <sip:second-alias@unreachable.invalid:9>\r\n")), 200)
	if len(r.status().Bindings) != 2 {
		t.Fatal("alias registration not retained")
	}
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "one copy", "text/plain")
	delivery := replyOneMessage(receiver.conn, 200)
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "one copy", "text/plain", sender.messageAuthorization("1002"), 2, "")), 200)
	result := <-delivery
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.request.Recipient.User != "second-alias" {
		t.Fatal("most recent contact was not selected")
	}
	receiver.conn.SetReadDeadline(time.Now().Add(70 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("same socket received two copies")
	}
}

func TestMessageUDPSizeLimitDoesNotTruncateOrTransmit(t *testing.T) {
	r := testMessageRuntime(t)
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	body := strings.Repeat("界", 400)
	sender.messageChallenge("1002", body, "text/plain")
	requireStatus(t, sender.exchange(sender.messagePacket("1002", body, "text/plain", sender.messageAuthorization("1002"), 2, "")), 513)
	receiver.conn.SetReadDeadline(time.Now().Add(70 * time.Millisecond))
	if _, err := receiver.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("oversized UDP message was transmitted")
	}
}

func TestMessageWildcardListenerKeepsOriginalPort(t *testing.T) {
	config := testConfig(t, "SHA-256")
	config.Config.Listen = "0.0.0.0:0"
	config.Config.MessagesEnabled = true
	for i := range config.Config.Accounts {
		config.Config.Accounts[i].AllowMessages = true
	}
	r, err := start(config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	// Use a routable peer address rather than sending to the wildcard bind.
	address := fmt.Sprintf("127.0.0.1:%d", r.conn.LocalAddr().(*net.UDPAddr).Port)
	newPeer := func() *wireClient {
		remote, _ := net.ResolveUDPAddr("udp", address)
		conn, err := net.DialUDP("udp", nil, remote)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return &wireClient{t: t, conn: conn, callID: randomToken(), branch: randomToken()}
	}
	receiver := newPeer()
	res := receiver.exchange(receiver.packet("REGISTER", "1002", 1, "", ""))
	requireStatus(t, res, 401)
	challenge, err := digest.ParseChallenge(res.headers["www-authenticate"][0])
	if err != nil {
		t.Fatal(err)
	}
	credential, err := digest.Digest(challenge, digest.Options{Method: "REGISTER", URI: "sip:vocat", Username: "1002", Password: testPassword + "-two"})
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, receiver.exchange(receiver.packet("REGISTER", "1002", 2, "Authorization: "+credential.String()+"\r\n", "Contact: <sip:1002@unreachable.invalid:9>\r\n")), 200)
	sender := newPeer()
	sender.messageChallenge("1002", "wildcard", "text/plain")
	delivery := replyOneMessage(receiver.conn, 200)
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "wildcard", "text/plain", sender.messageAuthorization("1002"), 2, "")), 200)
	result := <-delivery
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.request.Via().Host != "127.0.0.1" || result.request.Via().Port != r.conn.LocalAddr().(*net.UDPAddr).Port {
		t.Fatal("wildcard listener advertised an unusable Via")
	}
}

func TestMessageIPv6ObservedSourceDelivery(t *testing.T) {
	config := testConfig(t, "SHA-256")
	config.Config.Listen = "[::1]:0"
	config.Config.MessagesEnabled = true
	for i := range config.Config.Accounts {
		config.Config.Accounts[i].AllowMessages = true
	}
	r, err := start(config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	receiver := registerMessagePeer(t, r, "1002")
	sender := newWireClient(t, r)
	sender.messageChallenge("1002", "IPv6 消息", "text/plain; charset=UTF-8")
	delivery := replyOneMessage(receiver.conn, 200)
	requireStatus(t, sender.exchange(sender.messagePacket("1002", "IPv6 消息", "text/plain; charset=UTF-8", sender.messageAuthorization("1002"), 2, "")), 200)
	if result := <-delivery; result.err != nil {
		t.Fatal(result.err)
	}
}
