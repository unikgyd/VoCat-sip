package sipgateway

import (
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

type isolatedPeerTransaction struct {
	sip.ServerTransaction // Unused transaction methods intentionally have no implementation/I/O.
	responses             []*sip.Response
}

func (tx *isolatedPeerTransaction) Respond(response *sip.Response) error {
	tx.responses = append(tx.responses, response)
	return nil
}
func peerTestCredential(t *testing.T, r *runtime, req *sip.Request, username, password, source string) string {
	t.Helper()
	challenge, err := digest.ParseChallenge(r.challenge(source, false))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := digest.Digest(challenge, digest.Options{Method: string(req.Method), URI: req.Recipient.String(), Username: username, Password: password, Count: 1, Cnonce: "offline-inbound-client"})
	if err != nil {
		t.Fatal(err)
	}
	return credential.String()
}
func TestPeerBusinessPrincipalUsesIndependentAuthNotCallerDisplay(t *testing.T) {
	r := isolatedPeerRuntime(t)
	req := isolatedPeerRequest(t, r, sip.INVITE)
	req.SetSource(r.config.Config.Peer.Endpoint)
	req.From().Address.User = "+639001234567"
	tx := &isolatedPeerTransaction{}
	if _, ok := r.authorizeBusiness(req, tx); ok || len(tx.responses) != 1 || tx.responses[0].StatusCode != 401 {
		t.Fatal("unauthenticated peer business accepted")
	}
	req.AppendHeader(sip.NewHeader("Authorization", peerTestCredential(t, r, req, "pbx-in", peerInboundPassword, req.Source())))
	principal, ok := r.authorizeBusiness(req, tx)
	if !ok || !principal.peer || principal.account.Username != "peer:pbx-in" || !principal.account.AllowCalls || !principal.account.AllowMessages || len(principal.account.OutgoingLines) != 1 || principal.account.OutgoingLines[0] != "line-a" {
		t.Fatal("peer business principal confused authentication and displayed caller")
	}
	principal.account.OutgoingLines[0] = "changed"
	if r.config.Config.Peer.OutgoingLines[0] != "line-a" {
		t.Fatal("principal permissions aliased configuration")
	}
	if _, ok := r.authorizeBusiness(req, tx); ok {
		t.Fatal("peer digest replay accepted")
	}
}
func TestPeerBusinessDoesNotBorrowLocalCredentialFromSameUsername(t *testing.T) {
	config := isolatedPeerConfig()
	config.Peer.InboundAuth.Username = "1001"
	config.Accounts = []Account{{Username: "1001", Password: testPassword, AllowCalls: true}}
	prepared, err := prepare(config, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, peerPassword := range []bool{false, true} {
		r := &runtime{config: prepared, opaque: "offline", replays: map[string]replay{}}
		req := isolatedPeerRequest(t, r, sip.INVITE)
		req.SetSource(prepared.Config.Peer.Endpoint)
		req.From().Address.User = "1001"
		password := testPassword
		if peerPassword {
			password = peerInboundPassword
		}
		req.AppendHeader(sip.NewHeader("Authorization", peerTestCredential(t, r, req, "1001", password, req.Source())))
		principal, ok := r.authorizeBusiness(req, &isolatedPeerTransaction{})
		if ok != peerPassword || ok && (!principal.peer || principal.account.Username != "peer:1001") {
			t.Fatal("peer borrowed/impersonated a local account")
		}
	}
}
func TestPeerBusinessExactEndpointDirectionAndRegistrationAreFenced(t *testing.T) {
	for _, scenario := range []string{"foreign-port", "disabled-direction", "register", "duplicate-auth"} {
		t.Run(scenario, func(t *testing.T) {
			r := isolatedPeerRuntime(t)
			req := isolatedPeerRequest(t, r, sip.MESSAGE)
			req.SetSource(r.config.Config.Peer.Endpoint)
			if scenario == "register" {
				req.Method = sip.REGISTER
				req.CSeq().MethodName = sip.REGISTER
			}
			auth := peerTestCredential(t, r, req, "pbx-in", peerInboundPassword, req.Source())
			req.AppendHeader(sip.NewHeader("Authorization", auth))
			switch scenario {
			case "foreign-port":
				req.SetSource("127.0.0.1:15063")
			case "disabled-direction":
				r.config.Config.Peer.AllowSMSFromPeer = false
			case "duplicate-auth":
				req.AppendHeader(sip.NewHeader("Authorization", auth))
			}
			tx := &isolatedPeerTransaction{}
			if _, ok := r.authorizeBusiness(req, tx); ok {
				t.Fatal("peer escaped source/business-role fence")
			}
			if len(tx.responses) != 1 {
				t.Fatal("rejected peer did not receive one deterministic response")
			}
		})
	}
}
func TestPeerAndLocalDeliveryTargetsDoNotCreateRegistrationsOrAliasContacts(t *testing.T) {
	r := isolatedPeerRuntime(t)
	for _, user := range []string{"ring-all", "1001", "+8613800000000"} {
		target, ok := r.peerDeliveryTarget(user)
		if !ok || !target.peer || target.source != "127.0.0.1:15062" || target.contact.Address.User != user || target.contact.Address.Host != "127.0.0.1" || target.contact.Address.Port != 15062 {
			t.Fatal("fixed peer target invalid")
		}
		target.contact.Address.User = "changed"
		next, _ := r.peerDeliveryTarget(user)
		if next.contact.Address.User != user {
			t.Fatal("peer target was shared mutable contact")
		}
	}
	if len(r.registrations) != 0 {
		t.Fatal("peer fabricated a registrar binding")
	}
	for _, user := range []string{"sip:1001@foreign", "1001@foreign", "a/b", "", "bad\r\n"} {
		if _, ok := r.peerDeliveryTarget(user); ok {
			t.Fatal("unsafe peer target accepted")
		}
	}
	contact := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "1001", Host: "private.invalid", UriParams: sip.NewParams()}}
	contact.Address.UriParams.Add("transport", "udp")
	entry := registration{contact: contact, source: "127.0.0.1:15064", until: time.Now().Add(time.Minute)}
	local := localDeliveryTarget(entry)
	local.contact.Address.UriParams.Add("transport", "tcp")
	if local.peer || local.source != entry.source || contact.Address.UriParams.GetOr("transport", "") != "udp" {
		t.Fatal("local contact clone/source policy changed")
	}
}
