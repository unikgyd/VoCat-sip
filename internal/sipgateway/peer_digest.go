package sipgateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode"

	"github.com/emiago/sipgo/sip"
)

type peerChallenge struct {
	realm, nonce, algorithm, opaque string
	qop                             bool
}

func parsePeerChallenge(value string) (peerChallenge, bool) {
	fields, err := parseDigestFields(value, true)
	if err != nil {
		return peerChallenge{}, false
	}
	realm, hasRealm := fields["realm"]
	if !hasRealm || fields["nonce"] == "" || fields["userhash"] != "" || fields["charset"] != "" && !strings.EqualFold(fields["charset"], "UTF-8") {
		return peerChallenge{}, false
	}
	algorithm := strings.ToUpper(fields["algorithm"])
	if algorithm == "" {
		algorithm = "MD5"
	}
	if algorithm != "MD5" && algorithm != "SHA-256" {
		return peerChallenge{}, false
	}
	qop := false
	if offered, exists := fields["qop"]; exists {
		for _, option := range strings.Split(offered, ",") {
			if strings.EqualFold(strings.TrimSpace(option), "auth") {
				qop = true
			}
		}
		if !qop {
			return peerChallenge{}, false
		}
	}
	return peerChallenge{realm: realm, nonce: fields["nonce"], algorithm: algorithm, opaque: fields["opaque"], qop: qop}, true
}
func quotePeerDigest(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func uniquePeerBranch(via *sip.ViaHeader) (string, bool) {
	branch := ""
	for _, parameter := range via.Params {
		if !strings.EqualFold(parameter.K, "branch") {
			continue
		}
		if branch != "" || !digestToken(parameter.V) || len(parameter.V) > 256 {
			return "", false
		}
		branch = parameter.V
	}
	return branch, branch != ""
}
func matchingPeerVia(request, response *sip.ViaHeader) bool {
	left, lok := uniquePeerBranch(request)
	right, rok := uniquePeerBranch(response)
	if !(lok && rok && left == right && strings.EqualFold(request.ProtocolName, response.ProtocolName) &&
		strings.EqualFold(request.ProtocolVersion, response.ProtocolVersion) && strings.EqualFold(request.Transport, response.Transport) &&
		strings.EqualFold(request.Host, response.Host) && request.Port == response.Port) {
		return false
	}
	stable := func(via *sip.ViaHeader, response bool) (map[string]string, bool) {
		all, fields := map[string]bool{}, map[string]string{}
		for _, parameter := range via.Params {
			key := strings.ToLower(parameter.K)
			if !digestToken(key) || all[key] {
				return nil, false
			}
			all[key] = true
			if key == "received" {
				if response {
					if address, err := netip.ParseAddr(parameter.V); err != nil || address.Zone() != "" {
						return nil, false
					}
				}
				continue
			}
			if key == "rport" {
				if parameter.V != "" {
					if port, err := strconv.ParseUint(parameter.V, 10, 16); err != nil || port == 0 {
						return nil, false
					}
				}
				continue
			}
			fields[key] = parameter.V
		}
		return fields, true
	}
	a, aok := stable(request, false)
	b, bok := stable(response, true)
	if !aok || !bok || len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, found := b[key]; !found || value != other {
			return false
		}
	}
	return true
}
func safePeerDigestURI(req *sip.Request) bool {
	uri := req.Recipient
	if uri.Scheme != "sip" || uri.Wildcard || uri.HierarhicalSlashes || uri.Host == "" || uri.Password != "" || len(uri.Headers) != 0 || len(uri.String()) > 2048 || strings.ContainsFunc(uri.String(), unicode.IsControl) {
		return false
	}
	if req.Method != sip.BYE && !validPeerTargetUser(uri.User) {
		return false
	}
	seen := map[string]bool{}
	for _, parameter := range uri.UriParams {
		name := strings.ToLower(parameter.K)
		if name == "" || seen[name] || strings.ContainsAny(name, " \t,;=\"\\") || strings.ContainsFunc(name+parameter.V, unicode.IsControl) ||
			strings.ContainsAny(parameter.V, " \t,;\"\\") || name == "transport" && !strings.EqualFold(parameter.V, "UDP") || name == "maddr" || name == "ttl" || name == "method" {
			return false
		}
		seen[name] = true
	}
	return true
}

// This only computes one response. Callers own transaction identity changes,
// ACK/CANCEL behavior and their original total budget; it performs no I/O.
func (r *runtime) peerAuthorization(req *sip.Request, res *sip.Response, source string) (headerName, value string, ok bool) {
	if req == nil || res == nil || !r.isPeerSource(source) || (res.StatusCode != 401 && res.StatusCode != 407) ||
		(req.Method != sip.INVITE && req.Method != sip.MESSAGE && req.Method != sip.BYE) || len(req.GetHeaders("Authorization"))+len(req.GetHeaders("Proxy-Authorization")) != 0 {
		return "", "", false
	}
	peer := r.config.Config.Peer
	if req.Method == sip.INVITE && !peer.AllowCallsToPeer || req.Method == sip.BYE && !peer.AllowCallsToPeer && !peer.AllowCallsFromPeer || req.Method == sip.MESSAGE && !peer.AllowSMSToPeer || r.config.PeerSecrets == nil {
		return "", "", false
	}
	if !r.isPeerSource(req.Destination()) || !safePeerDigestURI(req) || req.Transport() != "UDP" || res.Source() != "" && !r.isPeerSource(res.Source()) {
		return "", "", false
	}
	// Do not turn a challenge from another SIP transaction into credentials.
	for _, name := range []string{"Via", "From", "To", "Call-ID", "CSeq"} {
		if len(req.GetHeaders(name)) != 1 || len(res.GetHeaders(name)) != 1 {
			return "", "", false
		}
	}
	if _, ok := req.GetHeaders("Via")[0].(*sip.ViaHeader); !ok {
		return "", "", false
	}
	if _, ok := res.GetHeaders("Via")[0].(*sip.ViaHeader); !ok {
		return "", "", false
	}
	if _, ok := req.GetHeaders("From")[0].(*sip.FromHeader); !ok {
		return "", "", false
	}
	if _, ok := res.GetHeaders("From")[0].(*sip.FromHeader); !ok {
		return "", "", false
	}
	if _, ok := req.GetHeaders("To")[0].(*sip.ToHeader); !ok {
		return "", "", false
	}
	if _, ok := res.GetHeaders("To")[0].(*sip.ToHeader); !ok {
		return "", "", false
	}
	if _, ok := req.GetHeaders("Call-ID")[0].(*sip.CallIDHeader); !ok {
		return "", "", false
	}
	if _, ok := res.GetHeaders("Call-ID")[0].(*sip.CallIDHeader); !ok {
		return "", "", false
	}
	if _, ok := req.GetHeaders("CSeq")[0].(*sip.CSeqHeader); !ok {
		return "", "", false
	}
	if _, ok := res.GetHeaders("CSeq")[0].(*sip.CSeqHeader); !ok {
		return "", "", false
	}
	if req.Via() == nil || res.Via() == nil || req.From() == nil || res.From() == nil || req.To() == nil || res.To() == nil || req.CallID() == nil || res.CallID() == nil || req.CSeq() == nil || res.CSeq() == nil ||
		req.CallID().Value() != res.CallID().Value() || req.CSeq().SeqNo != res.CSeq().SeqNo || req.CSeq().MethodName != req.Method || res.CSeq().MethodName != req.Method ||
		req.From().Value() != res.From().Value() || req.To().Address.String() != res.To().Address.String() ||
		!matchingPeerVia(req.Via(), res.Via()) {
		return "", "", false
	}
	if tag, exists := req.To().Params.Get("tag"); exists && tag != res.To().Params.GetOr("tag", "") {
		return "", "", false
	}
	challenge, supported := r.pinnedPeerChallenge(res)
	if !supported {
		return "", "", false
	}
	headerName = "Authorization"
	if res.StatusCode == 407 {
		headerName = "Proxy-Authorization"
	}
	ha1 := r.config.PeerSecrets.Outbound.SHA256
	if challenge.algorithm == "MD5" {
		ha1 = r.config.PeerSecrets.Outbound.MD5
	}
	uri := req.Recipient.String()
	ha2 := digestHash(challenge.algorithm, string(req.Method)+":"+uri)
	response := ""
	cnonce := ""
	if challenge.qop {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", "", false
		}
		cnonce = hex.EncodeToString(random[:])
		response = digestHash(challenge.algorithm, ha1+":"+challenge.nonce+":00000001:"+cnonce+":auth:"+ha2)
	} else {
		response = digestHash(challenge.algorithm, ha1+":"+challenge.nonce+":"+ha2)
	}
	value = fmt.Sprintf("Digest username=%s,realm=%s,nonce=%s,uri=%s,response=%s,algorithm=%s", quotePeerDigest(peer.OutboundAuth.Username), quotePeerDigest(challenge.realm), quotePeerDigest(challenge.nonce), quotePeerDigest(uri), quotePeerDigest(response), challenge.algorithm)
	if challenge.qop {
		value += ",qop=auth,nc=00000001,cnonce=" + quotePeerDigest(cnonce)
	}
	if challenge.opaque != "" {
		value += ",opaque=" + quotePeerDigest(challenge.opaque)
	}
	return headerName, value, true
}

// Pure credential-policy recognition shared with bodyless readiness. This
// neither authorizes a SIP method nor computes/sends a Digest proof. The caller
// must first validate the exact response source and transaction identity.
func (r *runtime) pinnedPeerChallenge(res *sip.Response) (peerChallenge, bool) {
	peer := r.config.Config.Peer
	if res == nil || peer == nil || r.config.PeerSecrets == nil ||
		(res.StatusCode != 401 && res.StatusCode != 407) || !validPeerSecret(r.config.PeerSecrets.Outbound) {
		return peerChallenge{}, false
	}
	name := "WWW-Authenticate"
	if res.StatusCode == 407 {
		name = "Proxy-Authenticate"
	}
	headers := res.GetHeaders(name)
	if len(headers) != 1 || len(res.GetHeaders("WWW-Authenticate"))+len(res.GetHeaders("Proxy-Authenticate")) != 1 {
		return peerChallenge{}, false
	}
	challenge, supported := parsePeerChallenge(headers[0].Value())
	if !supported || challenge.realm != peer.OutboundAuth.Realm {
		return peerChallenge{}, false
	}
	for _, algorithm := range peer.OutboundAuth.Algorithms {
		if algorithm == challenge.algorithm {
			return challenge, true
		}
	}
	return peerChallenge{}, false
}
