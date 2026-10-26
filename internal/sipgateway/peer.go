package sipgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/emiago/sipgo/sip"
	"net/netip"
)

type deliveryTarget struct {
	contact *sip.ContactHeader
	source  string
	peer    bool
}

func localDeliveryTarget(entry registration) deliveryTarget {
	var contact *sip.ContactHeader
	if entry.contact != nil {
		contact = entry.contact.Clone()
	}
	return deliveryTarget{contact: contact, source: entry.source}
}
func (r *runtime) isPeerSource(source string) bool {
	peer := r.config.Config.Peer
	if peer == nil || !peer.Enabled {
		return false
	}
	expected, ok := usablePeerEndpoint(peer.Endpoint)
	actual, err := netip.ParseAddrPort(source)
	return ok && err == nil && actual.Addr().Zone() == "" && actual == expected
}
func (r *runtime) peerDeliveryTarget(user string) (deliveryTarget, bool) {
	peer := r.config.Config.Peer
	if peer == nil || !peer.Enabled || !validPeerTargetUser(user) {
		return deliveryTarget{}, false
	}
	ep, ok := usablePeerEndpoint(peer.Endpoint)
	if !ok {
		return deliveryTarget{}, false
	}
	return deliveryTarget{contact: &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: ep.Addr().String(), Port: int(ep.Port())}}, source: ep.String(), peer: true}, true
}

type businessPrincipal struct {
	account Account
	peer    bool
}

func (r *runtime) authorizeBusiness(req *sip.Request, tx sip.ServerTransaction) (businessPrincipal, bool) {
	// Registration continues to use the existing local account policy, even
	// from the peer's source. A fixed trunk never becomes a registrar account.
	if req.Method == sip.REGISTER || !r.isPeerSource(req.Source()) {
		username, ok := r.authorize(req, tx)
		if !ok {
			return businessPrincipal{}, false
		}
		account, ok := r.account(username)
		return businessPrincipal{account: account}, ok
	}
	if !r.validRequest(req, tx) {
		return businessPrincipal{}, false
	}
	peer := r.config.Config.Peer
	if req.Method != sip.INVITE && req.Method != sip.MESSAGE || req.Method == sip.INVITE && !peer.AllowCallsFromPeer || req.Method == sip.MESSAGE && !peer.AllowSMSFromPeer {
		r.respond(req, tx, 403, "Peer Operation Not Permitted")
		return businessPrincipal{}, false
	}
	headers := req.GetHeaders("Authorization")
	if len(headers) > 1 || len(req.GetHeaders("Proxy-Authorization")) != 0 {
		r.respond(req, tx, 400, "Bad Request")
		return businessPrincipal{}, false
	}
	header := ""
	if len(headers) == 1 {
		header = headers[0].Value()
	}
	secrets := r.config.PeerSecrets
	_, ok := r.authenticateCredentials(string(req.Method), req.Recipient.String(), req.Source(), header, "peer", func(username string) (secret, bool) {
		if secrets == nil || username != peer.InboundAuth.Username || !validPeerSecret(secrets.Inbound) {
			return secret{}, false
		}
		return secrets.Inbound, true
	})
	if !ok {
		r.respond(req, tx, 401, "Unauthorized", sip.NewHeader("WWW-Authenticate", r.challenge(req.Source(), false)))
		return businessPrincipal{}, false
	}
	return businessPrincipal{peer: true, account: Account{Username: "peer:" + peer.InboundAuth.Username,
		AllowCalls: peer.AllowCallsFromPeer, AllowMessages: peer.AllowSMSFromPeer,
		OutgoingLines: append([]string{}, peer.OutgoingLines...), SMSSendLines: append([]string{}, peer.SMSSendLines...)}}, true
}

// Binds a durable SMS recipient to the exact fixed authority and credential
// generation. No password/HA1 is disclosed by the public opaque identity.
func peerRecipientIdentity(config stored, targetUser string) string {
	peer := config.Config.Peer
	if peer == nil || !peer.Enabled || !peer.AllowSMSToPeer || config.PeerSecrets == nil || !validPeerTargetUser(targetUser) {
		return ""
	}
	if _, ok := usablePeerEndpoint(peer.Endpoint); !ok || !validPeerSecret(config.PeerSecrets.Outbound) || len(config.PeerSecrets.CredentialEpoch) != 32 {
		return ""
	}
	sum := sha256.Sum256([]byte("fixed-peer-v1\x00" + peer.Endpoint + "\x00" + peer.OutboundAuth.Realm + "\x00" + config.PeerSecrets.CredentialEpoch + "\x00" + targetUser))
	return hex.EncodeToString(sum[:])
}
