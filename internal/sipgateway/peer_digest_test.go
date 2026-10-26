package sipgateway

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

func TestPeerDigestSharedContract(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/sip-peer-digest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int `json:"version"`
		Cases   []struct {
			Name, Challenge, Realm, Nonce, Algorithm, Opaque string
			Supported, Qop                                   bool
		} `json:"cases"`
	}
	if json.Unmarshal(data, &fixture) != nil || fixture.Version != 1 || len(fixture.Cases) < 20 {
		t.Fatal("shared peer digest contract invalid")
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			challenge, ok := parsePeerChallenge(test.Challenge)
			if ok != test.Supported {
				t.Fatal("shared contract support mismatch")
			}
			if ok && (challenge.realm != test.Realm || challenge.nonce != test.Nonce || challenge.algorithm != test.Algorithm || challenge.opaque != test.Opaque || challenge.qop != test.Qop) {
				t.Fatal("shared contract challenge mismatch")
			}
		})
	}
	for _, value := range []string{`Digest realm=,nonce=n`, `Digest realm="",realm="a",nonce=n`, "Digest realm=\"a\",nonce=n\x01x", `Digest realm="a",nonce="n",qop="auth",userhash=true`} {
		if _, ok := parsePeerChallenge(value); ok {
			t.Fatal("malformed or unsupported challenge accepted")
		}
	}
}

func isolatedPeerRuntime(t *testing.T) *runtime {
	t.Helper()
	return &runtime{config: preparedIsolatedPeer(t), opaque: "offline-peer-opaque", replays: map[string]replay{}}
}
func isolatedPeerRequest(t *testing.T, r *runtime, method sip.RequestMethod) *sip.Request {
	t.Helper()
	wire := fmt.Sprintf("%s sip:ring-all@127.0.0.1:15062 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:15060;branch=z9hG4bK-peer-offline;rport\r\nFrom: <sip:caller@vocat>;tag=offline-local\r\nTo: <sip:ring-all@asterisk>\r\nCall-ID: Peer-Offline-Call\r\nCSeq: 1 %s\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n", method, method)
	message, err := registrarParser().ParseSIP([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	request := message.(*sip.Request)
	request.SetTransport("UDP")
	request.SetSource("127.0.0.1:15060")
	request.SetDestination(r.config.Config.Peer.Endpoint)
	return request
}
func isolatedPeerChallenge(req *sip.Request, code int, value string, source string) *sip.Response {
	response := sip.NewResponseFromRequest(req, code, "Authentication", nil)
	header := "WWW-Authenticate"
	if code == 407 {
		header = "Proxy-Authenticate"
	}
	response.AppendHeader(sip.NewHeader(header, value))
	response.SetSource(source)
	response.To().Params.Add("tag", "offline-remote")
	return response
}

func TestPeerAuthorizationUsesOnlyExpectedIndependentCredentials(t *testing.T) {
	for _, algorithm := range []string{"SHA-256", "MD5"} {
		for _, code := range []int{401, 407} {
			for _, qop := range []bool{true, false} {
				name := fmt.Sprintf("%s-%d-qop-%t", algorithm, code, qop)
				t.Run(name, func(t *testing.T) {
					r := isolatedPeerRuntime(t)
					r.config.Config.Peer.OutboundAuth.Algorithms = []string{algorithm}
					req := isolatedPeerRequest(t, r, sip.MESSAGE)
					challenge := fmt.Sprintf(`Digest realm="asterisk",nonce="offline-nonce",algorithm=%s,opaque="a,b"`, algorithm)
					if qop {
						challenge += `,qop="auth-int, auth"`
					}
					res := isolatedPeerChallenge(req, code, challenge, r.config.Config.Peer.Endpoint)
					header, value, ok := r.peerAuthorization(req, res, r.config.Config.Peer.Endpoint)
					wantHeader := "Authorization"
					if code == 407 {
						wantHeader = "Proxy-Authorization"
					}
					if !ok || header != wantHeader {
						t.Fatal("expected challenge rejected")
					}
					fields, err := parseDigest(value)
					if err != nil {
						t.Fatal(err)
					}
					parsed, err := digest.ParseChallenge(challenge)
					if err != nil {
						t.Fatal(err)
					}
					// The independent library does not trim a qop list's spaces.
					// Explicitly select the auth option this implementation chose.
					if qop {
						parsed.QOP = []string{"auth"}
					}
					// Independent library computes the response from plaintext retained
					// only by this test, never by production configuration.
					independent, err := digest.Digest(parsed, digest.Options{Method: string(req.Method), URI: req.Recipient.String(), Username: "gateway-out", Password: peerOutboundPassword, Count: 1, Cnonce: fields["cnonce"]})
					if err != nil || fields["response"] != independent.Response || fields["username"] != "gateway-out" || fields["opaque"] != "a,b" {
						t.Fatal("independent outbound credential calculation mismatch", err)
					}
					if qop && (fields["nc"] != "00000001" || len(fields["cnonce"]) != 32 || fields["qop"] != "auth") {
						t.Fatal("bounded first nonce-count/cnonce missing")
					}
					if !qop && (fields["nc"] != "" || fields["cnonce"] != "") {
						t.Fatal("legacy challenge acquired qop fields")
					}
					if len(req.GetHeaders("Authorization"))+len(req.GetHeaders("Proxy-Authorization")) != 0 {
						t.Fatal("helper mutated original transaction")
					}
					req.AppendHeader(sip.NewHeader(header, value))
					if _, _, ok := r.peerAuthorization(req, res, r.config.Config.Peer.Endpoint); ok {
						t.Fatal("credential-bearing request allowed another challenge retry")
					}
				})
			}
		}
	}
}

func TestPeerAuthorizationRejectsForeignSourceRealmAlgorithmsAndTransaction(t *testing.T) {
	mutations := map[string]func(*runtime, *sip.Request, *sip.Response, *string){
		"source":              func(r *runtime, q *sip.Request, s *sip.Response, source *string) { *source = "127.0.0.1:15063" },
		"response-source":     func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.SetSource("127.0.0.1:15063") },
		"request-destination": func(r *runtime, q *sip.Request, s *sip.Response, source *string) { q.SetDestination("127.0.0.1:15063") },
		"realm": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			r.config.Config.Peer.OutboundAuth.Realm = "different"
		},
		"algorithm-downgrade": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.ReplaceHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="asterisk",nonce="n",algorithm=MD5,qop=auth`))
		},
		"generic-call-id": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.ReplaceHeader(sip.NewHeader("Call-ID", "peer-offline-call"))
		},
		"call-id-case": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			id := sip.CallIDHeader("peer-offline-call")
			s.ReplaceHeader(&id)
		},
		"sequence": func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.CSeq().SeqNo++ },
		"method":   func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.CSeq().MethodName = sip.INVITE },
		"from-tag": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.From().Params.Add("tag", "different")
		},
		"to-uri": func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.To().Address.User = "different" },
		"branch-case": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.Via().Params.Add("branch", "z9hG4bK-peer-OFFLINE")
		},
		"branch-missing": func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.Via().Params.Remove("branch") },
		"branch-duplicate": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.Via().Params = append(s.Via().Params, sip.HeaderKV{K: "BRANCH", V: "z9hG4bK-peer-offline"})
		},
		"sent-by":      func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.Via().Port++ },
		"via-protocol": func(r *runtime, q *sip.Request, s *sip.Response, source *string) { s.Via().Transport = "TCP" },
		"duplicate-challenge": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.AppendHeader(sip.NewHeader("WWW-Authenticate", s.GetHeader("WWW-Authenticate").Value()))
		},
		"other-challenge": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			s.AppendHeader(sip.NewHeader("Proxy-Authenticate", s.GetHeader("WWW-Authenticate").Value()))
		},
		"request-transport": func(r *runtime, q *sip.Request, s *sip.Response, source *string) { q.SetTransport("TCP") },
		"uri-password":      func(r *runtime, q *sip.Request, s *sip.Response, source *string) { q.Recipient.Password = "secret" },
		"uri-headers": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			q.Recipient.Headers.Add("Route", "foreign")
		},
		"transport-parameter": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			q.Recipient.UriParams.Add("transport", "TCP")
		},
		"disabled": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			r.config.Config.Peer.Enabled = false
		},
		"permission": func(r *runtime, q *sip.Request, s *sip.Response, source *string) {
			r.config.Config.Peer.AllowSMSToPeer = false
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := isolatedPeerRuntime(t)
			req := isolatedPeerRequest(t, r, sip.MESSAGE)
			source := r.config.Config.Peer.Endpoint
			res := isolatedPeerChallenge(req, 401, `Digest realm="asterisk",nonce="n",algorithm=SHA-256,qop=auth`, source)
			mutate(r, req, res, &source)
			if _, _, ok := r.peerAuthorization(req, res, source); ok {
				t.Fatal("foreign/ambiguous challenge generated credentials")
			}
		})
	}
}
func TestPeerAuthorizationAllowsReceiverViaAndPinnedDialogURIForBye(t *testing.T) {
	r := isolatedPeerRuntime(t)
	r.config.Config.Peer.AllowCallsToPeer = false // Existing from-peer call may still need authenticated BYE.
	req := isolatedPeerRequest(t, r, sip.BYE)
	req.Recipient.Host, req.Recipient.Port = "asterisk-contact.invalid", 5099
	req.Recipient.UriParams.Add("transport", "UDP")
	req.Recipient.UriParams.Add("ob", "")
	req.To().Params.Add("tag", "dialog-remote")
	res := isolatedPeerChallenge(req, 401, `Digest realm="asterisk",nonce="n",algorithm=SHA-256,qop=auth`, r.config.Config.Peer.Endpoint)
	res.To().Params.Add("tag", "dialog-remote")
	res.Via().Params.Add("received", "127.0.0.1")
	res.Via().Params.Add("rport", "15060")
	_, value, ok := r.peerAuthorization(req, res, r.config.Config.Peer.Endpoint)
	if !ok || !strings.Contains(value, "asterisk-contact.invalid:5099;transport=UDP;ob") {
		t.Fatal("pinned BYE/standard receiver Via parameters rejected")
	}
	res.To().Params.Add("tag", "different-dialog")
	if _, _, ok := r.peerAuthorization(req, res, r.config.Config.Peer.Endpoint); ok {
		t.Fatal("different dialog challenge accepted")
	}
}
