package sipgateway

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/icholy/digest"
)

// The old 1500-byte MT body is now valid segmented content, not an oversized
// SIP packet. Preserve its historical terminal-size/full-text/revocation
// assertions by using a REAL, legal registered Contact whose URI, MT headers
// and first chunk cannot fit the unchanged 1300-byte whole-packet cap. Its
// REGISTER response must still fit that same sipgo UDP cap to reach the client.
// The separate 4097-ASCII admission fixture is upgraded to 16385 to test the
// new UTF-16 bound; neither compatibility adjustment weakens any assertion.
func registerMessagePeerWithOversizedRequestURI(t *testing.T, r *runtime, user string) *wireClient {
	t.Helper()
	phone := newWireClient(t, r)
	phone.callID = "register-large-contact-" + randomToken()
	response := phone.exchange(phone.packet("REGISTER", user, 1, "", ""))
	requireStatus(t, response, 401)
	challenge, err := digest.ParseChallenge(response.headers["www-authenticate"][0])
	if err != nil {
		t.Fatal(err)
	}
	phone.challenge, phone.count = challenge, 1
	password := testPassword
	if user == "1002" {
		password += "-two"
	}
	credentials, err := digest.Digest(challenge, digest.Options{Method: "REGISTER", URI: "sip:vocat", Username: user, Password: password, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	uri := fmt.Sprintf("sip:device-%s-%s@unreachable.invalid:%d", user, strings.Repeat("p", 650), phone.conn.LocalAddr().(*net.UDPAddr).Port)
	if len(uri) < 600 || len(uri) > 850 || len("<"+uri+">") > 2048 {
		t.Fatal("oversized request URI fixture does not fit the registrar's legal Contact budget")
	}
	contact := "Contact: <" + uri + ">\r\n"
	registered := phone.exchange(phone.packet("REGISTER", user, 2, "Authorization: "+credentials.String()+"\r\n", contact))
	requireStatus(t, registered, 200)
	if len(registered.raw) > 1300 {
		t.Fatal("actual REGISTER response exceeded the unchanged sipgo UDP cap")
	}
	if !strings.Contains(strings.Join(registered.headers["contact"], "\n"), uri) {
		t.Fatal("actual REGISTER response truncated the legal oversized Contact URI")
	}
	r.mu.Lock()
	found := false
	for _, entry := range r.registrations[user] {
		if entry.contact != nil && entry.contact.Address.String() == uri && entry.source == phone.conn.LocalAddr().String() {
			found = true
		}
	}
	r.mu.Unlock()
	if !found {
		t.Fatal("actual registrar did not retain the complete URI and observed UDP source")
	}
	// Registered after newWireClient's close cleanup, so this strict check runs
	// first on a still-open socket. Never consume or answer a business packet.
	t.Cleanup(func() { smsCaptureWireNoUDP(t, phone.conn) })
	return phone
}
