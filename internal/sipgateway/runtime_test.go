package sipgateway

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/icholy/digest"
)

const testPassword = "local-test-secret-2026"

func testConfig(t *testing.T, algorithm string) stored {
	t.Helper()
	c := DefaultConfig()
	c.Enabled = true
	c.Listen = "127.0.0.1:0"
	c.Algorithm = algorithm
	c.Accounts = []Account{{Username: "1001", Password: testPassword}, {Username: "1002", Password: testPassword + "-two"}}
	prepared, err := prepare(c, stored{}, true)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

type wireResponse struct {
	code    int
	headers map[string][]string
	raw     string
}
type wireClient struct {
	t         *testing.T
	conn      *net.UDPConn
	serial    int
	challenge *digest.Challenge
	count     int
	callID    string
	branch    string
}

func newWireClient(t *testing.T, r *runtime) *wireClient {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", r.conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &wireClient{t: t, conn: conn, callID: "register-loopback", branch: randomToken()}
}
func (c *wireClient) packet(method, user string, cseq int, auth, extra string) string {
	c.serial++
	return fmt.Sprintf("%s sip:vocat SIP/2.0\r\nVia: SIP/2.0/UDP 192.0.2.10:9;branch=z9hG4bK-test-%s-%d;rport\r\nFrom: <sip:%s@vpn.invalid>;tag=test\r\nTo: <sip:%s@lan.invalid>\r\nCall-ID: %s\r\nCSeq: %d %s\r\nMax-Forwards: 70\r\n%s%sContent-Length: 0\r\n\r\n", method, c.branch, c.serial, user, user, c.callID, cseq, method, auth, extra)
}
func (c *wireClient) exchange(packet string) wireResponse {
	c.t.Helper()
	c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write([]byte(packet)); err != nil {
		c.t.Fatal(err)
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			c.t.Fatal(err)
		}
		raw := string(buf[:n])
		lines := strings.Split(raw, "\r\n")
		parts := strings.SplitN(lines[0], " ", 3)
		if len(parts) < 2 {
			c.t.Fatal("bad SIP response")
		}
		code, _ := strconv.Atoi(parts[1])
		if code < 200 {
			continue
		}
		result := wireResponse{code: code, headers: map[string][]string{}, raw: raw}
		for _, line := range lines[1:] {
			if line == "" {
				break
			}
			name, value, ok := strings.Cut(line, ":")
			if ok {
				key := strings.ToLower(name)
				result.headers[key] = append(result.headers[key], strings.TrimSpace(value))
			}
		}
		return result
	}
}
func (c *wireClient) challengeRegister() {
	c.t.Helper()
	res := c.exchange(c.packet("REGISTER", "1001", 1, "", ""))
	if res.code != 401 {
		c.t.Fatalf("challenge status %d", res.code)
	}
	var err error
	c.challenge, err = digest.ParseChallenge(res.headers["www-authenticate"][0])
	if err != nil {
		c.t.Fatal(err)
	}
}
func (c *wireClient) authorization(password string) string {
	c.t.Helper()
	c.count++
	cred, err := digest.Digest(c.challenge, digest.Options{Method: "REGISTER", URI: "sip:vocat", Username: "1001", Password: password, Count: c.count, Cnonce: "independent-test-client"})
	if err != nil {
		c.t.Fatal(err)
	}
	return "Authorization: " + cred.String() + "\r\n"
}
func (c *wireClient) register(cseq int, extra string) wireResponse {
	return c.exchange(c.packet("REGISTER", "1001", cseq, c.authorization(testPassword), extra))
}
func requireStatus(t *testing.T, res wireResponse, want int) {
	t.Helper()
	if res.code != want {
		t.Fatalf("status %d want %d; response %s", res.code, want, res.raw)
	}
}
func testRuntime(t *testing.T, algorithm string) *runtime {
	t.Helper()
	r, err := start(testConfig(t, algorithm))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r
}

func TestRegisterWireBothAlgorithms(t *testing.T) {
	for _, algorithm := range []string{"MD5", "SHA-256"} {
		t.Run(algorithm, func(t *testing.T) {
			r := testRuntime(t, algorithm)
			c := newWireClient(t, r)
			c.challengeRegister()
			if c.challenge.Algorithm != algorithm {
				t.Fatal("wrong algorithm")
			}
			res := c.register(2, "Contact: <sip:1001@unreachable.invalid:9>;expires=120\r\n")
			requireStatus(t, res, 200)
			status := r.status()
			if len(status.Bindings) != 1 || status.Bindings[0].Source != c.conn.LocalAddr().String() {
				t.Fatalf("not observed-source bound: %+v", status)
			}
			if status.Capabilities.Calls || status.Capabilities.Messages || status.Capabilities.CarrierBridge {
				t.Fatal("incomplete capabilities advertised")
			}
			res = c.register(3, "")
			requireStatus(t, res, 200)
			if len(res.headers["contact"]) != 1 {
				t.Fatal("query lost contact")
			}
			res = c.register(4, "Contact: *\r\nExpires: 0\r\n")
			requireStatus(t, res, 200)
			if len(res.headers["contact"]) != 0 || len(r.status().Bindings) != 0 {
				t.Fatal("deregistration retained bindings")
			}
		})
	}
}
func TestRegisterReplayRetransmissionAndSequence(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	c.challengeRegister()
	auth := c.authorization(testPassword)
	packet := c.packet("REGISTER", "1001", 2, auth, "Contact: <sip:1001@10.0.0.10>\r\n")
	first := c.exchange(packet)
	requireStatus(t, first, 200)
	second := c.exchange(packet)
	requireStatus(t, second, 200)
	if first.raw != second.raw {
		t.Fatal("transaction retransmission was not cached")
	}
	requireStatus(t, c.exchange(c.packet("REGISTER", "1001", 3, auth, "")), 401)
	requireStatus(t, c.register(3, "Contact: *\r\nExpires: 0\r\n"), 200)
	requireStatus(t, c.register(2, "Contact: <sip:1001@10.0.0.10>\r\n"), 500)
	if len(r.status().Bindings) != 0 {
		t.Fatal("old sequence resurrected a deleted binding")
	}
}
func TestRegisterRejectsWrongPasswordAndAORSpoof(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	c.challengeRegister()
	requireStatus(t, c.exchange(c.packet("REGISTER", "1001", 2, c.authorization("wrong-password"), "")), 401)
	requireStatus(t, c.exchange(c.packet("REGISTER", "1002", 3, c.authorization(testPassword), "Contact: <sip:1002@10.0.0.20>\r\n")), 403)
	if len(r.status().Bindings) != 0 {
		t.Fatal("unauthorized binding saved")
	}
}
func TestRegisterContactValidationIsAtomic(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		want        int
	}{
		{"short", "Contact: <sip:1001@10.0.0.10>;expires=30\r\n", 423},
		{"bad-expiry", "Contact: <sip:1001@10.0.0.10>;expires=-1\r\n", 400},
		{"duplicate-expiry", "Contact: <sip:1001@10.0.0.10>;expires=120;expires=240\r\n", 400},
		{"duplicate-global", "Expires: 0\r\nExpires: 120\r\nContact: *\r\n", 400},
		{"wildcard-with-positive", "Contact: *\r\nExpires: 120\r\n", 400},
		{"wildcard-mixed", "Contact: *, <sip:1001@10.0.0.10>\r\nExpires: 0\r\n", 400},
		{"uri-headers", "Contact: <sip:1001@10.0.0.10?Subject=unsafe>\r\n", 400},
		{"password-in-contact", "Contact: <sip:1001:unsafe@10.0.0.10>\r\n", 400},
		{"atomic", "Contact: <sip:1001@10.0.0.11>, <sip:1001@10.0.0.12>;expires=20\r\n", 423},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRuntime(t, "SHA-256")
			c := newWireClient(t, r)
			c.challengeRegister()
			requireStatus(t, c.register(2, "Contact: <sip:1001@10.0.0.10>\r\n"), 200)
			res := c.register(3, test.extra)
			requireStatus(t, res, test.want)
			if test.want == 423 && strings.Join(res.headers["min-expires"], "") != "60" {
				t.Fatal("missing minimum expiry")
			}
			if len(r.status().Bindings) != 1 {
				t.Fatal("failed registration changed bindings")
			}
		})
	}
}
func TestRegisterMultipleContactsExpiryAndRemoval(t *testing.T) {
	r := testRuntime(t, "MD5")
	c := newWireClient(t, r)
	c.challengeRegister()
	res := c.register(2, "Contact: <sip:1001@10.0.0.10>, <sip:1001@10.0.0.11>\r\nExpires: 7200\r\n")
	requireStatus(t, res, 200)
	if len(res.headers["contact"]) != 2 {
		t.Fatal("multiple contacts not returned")
	}
	for _, value := range res.headers["contact"] {
		if !strings.Contains(value, "expires=3600") {
			t.Fatal("expiry not clamped")
		}
	}
	requireStatus(t, c.register(3, "Contact: <sip:1001@10.0.0.10>;expires=0\r\n"), 200)
	if len(r.status().Bindings) != 1 {
		t.Fatal("single contact removal failed")
	}
	r.mu.Lock()
	for key, entry := range r.registrations["1001"] {
		entry.until = time.Now().Add(-time.Second)
		r.registrations["1001"][key] = entry
	}
	r.mu.Unlock()
	res = c.register(4, "")
	requireStatus(t, res, 200)
	if len(res.headers["contact"]) != 0 {
		t.Fatal("expired binding returned")
	}
}
func TestRegisterContactCapacity(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	c.challengeRegister()
	contacts := []string{}
	for i := 0; i < 8; i++ {
		contacts = append(contacts, fmt.Sprintf("<sip:1001@10.0.0.%d>", i+1))
	}
	requireStatus(t, c.register(2, "Contact: "+strings.Join(contacts, ", ")+"\r\n"), 200)
	requireStatus(t, c.register(3, "Contact: <sip:1001@10.0.0.99>\r\n"), 403)
	if len(r.status().Bindings) != 8 {
		t.Fatal("capacity failure mutated bindings")
	}
}
func TestRegisterOptionsAndUnsupportedMethods(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	res := c.exchange(c.packet("OPTIONS", "1001", 1, "", ""))
	requireStatus(t, res, 200)
	if strings.Join(res.headers["allow"], "") != "REGISTER, OPTIONS" {
		t.Fatal("unimplemented methods advertised")
	}
	requireStatus(t, c.exchange(c.packet("MESSAGE", "1001", 2, "", "")), 405)
}
func TestRegisterSourceAllowlist(t *testing.T) {
	config := testConfig(t, "SHA-256")
	config.Config.AllowedCIDRs = []string{"192.0.2.0/24"}
	r, err := start(config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	c := newWireClient(t, r)
	c.conn.SetDeadline(time.Now().Add(120 * time.Millisecond))
	c.conn.Write([]byte(c.packet("OPTIONS", "1001", 1, "", "")))
	if _, err := c.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("denied source received a response")
	}
}
func TestRegisterShutdownReleasesSocket(t *testing.T) {
	for i := 0; i < 8; i++ {
		r, err := start(testConfig(t, "SHA-256"))
		if err != nil {
			t.Fatal(err)
		}
		addr := r.conn.LocalAddr().String()
		started := time.Now()
		r.close()
		r.close()
		if time.Since(started) > time.Second {
			t.Fatal("local shutdown exceeded one second")
		}
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			t.Fatalf("socket not released: %v", err)
		}
		conn.Close()
	}
}

func TestRegisterIPv6Loopback(t *testing.T) {
	config := testConfig(t, "SHA-256")
	config.Config.Listen = "[::1]:0"
	r, err := start(config)
	if err != nil {
		t.Fatalf("IPv6 loopback listener: %v", err)
	}
	defer r.close()
	c := newWireClient(t, r)
	c.challengeRegister()
	requireStatus(t, c.register(2, "Contact: <sip:1001@[2001:db8::1]:9>\r\n"), 200)
	if bindings := r.status().Bindings; len(bindings) != 1 || bindings[0].Source != c.conn.LocalAddr().String() {
		t.Fatal("IPv6 observed source was not retained")
	}
}

func TestRegisterResponseCannotBeRedirectedByVia(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	packet := c.packet("OPTIONS", "1001", 1, "", "")
	packet = strings.Replace(packet, ";rport\r\n", ";received=192.0.2.99;rport=9\r\n", 1)
	requireStatus(t, c.exchange(packet), 200)
}

func TestRegisterRejectsUnsupportedRequiredExtension(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	res := c.exchange(c.packet("REGISTER", "1001", 1, "", "Require: outbound\r\n"))
	requireStatus(t, res, 420)
	if strings.Join(res.headers["unsupported"], "") != "outbound" {
		t.Fatal("required extension was silently ignored")
	}
}

func TestRegisterUnknownACKHasNoResponse(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	c.conn.SetDeadline(time.Now().Add(120 * time.Millisecond))
	c.conn.Write([]byte(c.packet("ACK", "1001", 1, "", "")))
	if _, err := c.conn.Read(make([]byte, 4096)); err == nil {
		t.Fatal("ACK received a response")
	}
}
