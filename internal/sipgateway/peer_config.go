package sipgateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
)

// Peer is a fixed, independently authenticated UDP trunk, never a local
// account or a synthetic REGISTER binding. All business permissions default off.
type PeerInboundAuth struct {
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"has_password"`
}
type PeerOutboundAuth struct {
	Username    string   `json:"username"`
	Realm       string   `json:"realm"`
	Password    string   `json:"password,omitempty"`
	HasPassword bool     `json:"has_password"`
	Algorithms  []string `json:"algorithms"`
}
type PeerIncomingRoute struct {
	DeviceID   string `json:"device_id"`
	CallTarget string `json:"call_target"`
	SMSTarget  string `json:"sms_target"`
}
type PeerConfig struct {
	Enabled            bool                `json:"enabled"`
	Endpoint           string              `json:"endpoint"`
	InboundAuth        PeerInboundAuth     `json:"inbound_auth"`
	OutboundAuth       PeerOutboundAuth    `json:"outbound_auth"`
	AllowCallsFromPeer bool                `json:"allow_calls_from_peer"`
	AllowSMSFromPeer   bool                `json:"allow_sms_from_peer"`
	AllowCallsToPeer   bool                `json:"allow_calls_to_peer"`
	AllowSMSToPeer     bool                `json:"allow_sms_to_peer"`
	OutgoingLines      []string            `json:"outgoing_lines"`
	SMSSendLines       []string            `json:"sms_send_lines"`
	IncomingRoutes     []PeerIncomingRoute `json:"incoming_routes"`
}

// Persisted only in the existing sensitive private setting, never Config/View.
type peerSecrets struct {
	Inbound         secret `json:"inbound"`
	Outbound        secret `json:"outbound"`
	CredentialEpoch string `json:"credential_epoch"`
}

var peerUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var peerTargetPattern = regexp.MustCompile(`^[A-Za-z0-9+][A-Za-z0-9_.+\-]{0,63}$`)

func validPeerTargetUser(user string) bool { return peerTargetPattern.MatchString(user) }
func clonePeerConfig(peer *PeerConfig) *PeerConfig {
	if peer == nil {
		return nil
	}
	out := *peer
	out.OutboundAuth.Algorithms = append([]string{}, peer.OutboundAuth.Algorithms...)
	out.OutgoingLines = append([]string{}, peer.OutgoingLines...)
	out.SMSSendLines = append([]string{}, peer.SMSSendLines...)
	out.IncomingRoutes = append([]PeerIncomingRoute{}, peer.IncomingRoutes...)
	return &out
}
func usablePeerEndpoint(value string) (netip.AddrPort, bool) {
	ep, err := netip.ParseAddrPort(value)
	if err != nil || ep.Port() == 0 || ep.Addr().Zone() != "" || ep.Addr().Is4In6() ||
		(!ep.Addr().IsPrivate() && !ep.Addr().IsLoopback()) || ep.Addr().IsUnspecified() || ep.Addr().IsMulticast() {
		return netip.AddrPort{}, false
	}
	return ep, true
}

// Called again after binding: an ephemeral listener's final port was unknown
// to prepare. No DNS lookup or outbound communication is performed.
func validatePeerListener(peer *PeerConfig, listenActual string) error {
	if peer == nil || !peer.Enabled {
		return nil
	}
	ep, ok := usablePeerEndpoint(peer.Endpoint)
	listen, err := netip.ParseAddrPort(listenActual)
	if !ok || err != nil || ep.Addr().Is4() != listen.Addr().Is4() {
		return errors.New("SIP peer and listener require valid same-family IP endpoints")
	}
	if ep.Port() != listen.Port() {
		return nil
	}
	if !listen.Addr().IsUnspecified() {
		if ep.Addr() == listen.Addr() {
			return errors.New("SIP peer must not point to this listener")
		}
		return nil
	}
	if ep.Addr().IsLoopback() {
		return errors.New("SIP peer must not point to this listener")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return errors.New("SIP listener identity could not be verified")
	}
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err != nil {
			return errors.New("SIP listener identity could not be verified")
		}
		if prefix.Addr().Unmap() == ep.Addr() {
			return errors.New("SIP peer must not point to this listener")
		}
	}
	return nil
}

func preparePeerConfig(out *stored, old stored) error {
	peer := clonePeerConfig(out.Config.Peer)
	out.Config.Peer = peer
	if peer == nil {
		return nil
	}
	if len(peer.OutboundAuth.Algorithms) == 0 {
		peer.OutboundAuth.Algorithms = []string{"SHA-256"}
	}
	algorithms := map[string]bool{}
	for _, algorithm := range peer.OutboundAuth.Algorithms {
		if algorithm != "SHA-256" && algorithm != "MD5" || algorithms[algorithm] {
			return errors.New("SIP peer algorithms must be unique SHA-256 or explicit MD5")
		}
		algorithms[algorithm] = true
	}
	if len(peer.IncomingRoutes) > 32 || len(peer.OutgoingLines) > 32 || len(peer.SMSSendLines) > 32 {
		return errors.New("SIP peer supports at most 32 line permissions and routes")
	}
	configured := peer.Enabled || peer.Endpoint != "" || peer.InboundAuth.Username != "" || peer.InboundAuth.Password != "" ||
		peer.OutboundAuth.Username != "" || peer.OutboundAuth.Realm != "" || peer.OutboundAuth.Password != "" ||
		peer.AllowCallsFromPeer || peer.AllowSMSFromPeer || peer.AllowCallsToPeer || peer.AllowSMSToPeer ||
		len(peer.OutgoingLines)+len(peer.SMSSendLines)+len(peer.IncomingRoutes) != 0
	if !configured {
		peer.InboundAuth.HasPassword = false
		peer.OutboundAuth.HasPassword = false
		return nil
	}
	ep, ok := usablePeerEndpoint(peer.Endpoint)
	if !ok {
		return errors.New("SIP peer endpoint must be an explicit private or loopback IP and nonzero port")
	}
	peer.Endpoint = ep.String()
	listen, err := netip.ParseAddrPort(out.Config.Listen)
	if err != nil || ep.Addr().Is4() != listen.Addr().Is4() {
		return errors.New("SIP peer must use the listener IP family")
	}
	allowed := false
	for _, cidr := range out.Config.AllowedCIDRs {
		if p, err := netip.ParsePrefix(cidr); err == nil && p.Contains(ep.Addr()) {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("SIP peer endpoint must be inside an allowed network")
	}
	// Validate even while disabled so a saved peer cannot hide a self-loop.
	check := *peer
	check.Enabled = true
	if err := validatePeerListener(&check, out.Config.Listen); err != nil {
		return err
	}
	lines := map[string]CarrierLine{}
	for _, line := range out.Config.CarrierLines {
		lines[line.DeviceID] = line
	}
	for _, ids := range [][]string{peer.OutgoingLines, peer.SMSSendLines} {
		seen := map[string]bool{}
		for _, id := range ids {
			if _, exists := lines[id]; !exists || seen[id] {
				return errors.New("SIP peer permissions must reference unique configured lines")
			}
			seen[id] = true
		}
	}
	seen := map[string]bool{}
	for _, route := range peer.IncomingRoutes {
		line, exists := lines[route.DeviceID]
		if !exists || seen[route.DeviceID] || route.CallTarget == "" && route.SMSTarget == "" ||
			route.CallTarget != "" && !validPeerTargetUser(route.CallTarget) || route.SMSTarget != "" && !validPeerTargetUser(route.SMSTarget) {
			return errors.New("SIP peer routes require unique configured lines and safe target users")
		}
		if route.SMSTarget != "" && len(line.SMSRecipients) >= 16 {
			return errors.New("A carrier line supports at most 16 local and peer SMS recipients combined")
		}
		seen[route.DeviceID] = true
	}
	previous := old.PeerSecrets
	oldPeer := old.Config.Peer
	var next peerSecrets
	preserveInbound := oldPeer != nil && previous != nil && oldPeer.InboundAuth.Username == peer.InboundAuth.Username && old.Config.Realm == out.Config.Realm
	preserveOutbound := oldPeer != nil && previous != nil && oldPeer.OutboundAuth.Username == peer.OutboundAuth.Username && oldPeer.OutboundAuth.Realm == peer.OutboundAuth.Realm
	credentials := func(username, realm, password string, required, preserve bool, prior secret) (secret, error) {
		if username == "" && password == "" && !required {
			return secret{}, nil
		}
		if !peerUsernamePattern.MatchString(username) || len(realm) == 0 || len(realm) > 128 || strings.ContainsFunc(realm, unicode.IsControl) {
			return secret{}, errors.New("SIP peer authentication username or realm is invalid")
		}
		if password != "" {
			if len(password) < 12 || len(password) > 128 {
				return secret{}, errors.New("SIP peer passwords must contain 12..128 bytes")
			}
			base := username + ":" + realm + ":" + password
			return secret{MD5: digestHash("MD5", base), SHA256: digestHash("SHA-256", base)}, nil
		}
		if preserve && validPeerSecret(prior) {
			return prior, nil
		}
		return secret{}, errors.New("New SIP peer credentials or a changed username/realm require a password")
	}
	var priorIn, priorOut secret
	if previous != nil {
		priorIn, priorOut = previous.Inbound, previous.Outbound
	}
	if next.Inbound, err = credentials(peer.InboundAuth.Username, out.Config.Realm, peer.InboundAuth.Password, peer.AllowCallsFromPeer || peer.AllowSMSFromPeer, preserveInbound, priorIn); err != nil {
		return err
	}
	if next.Outbound, err = credentials(peer.OutboundAuth.Username, peer.OutboundAuth.Realm, peer.OutboundAuth.Password, peer.AllowCallsFromPeer || peer.AllowCallsToPeer || peer.AllowSMSToPeer, preserveOutbound, priorOut); err != nil {
		return err
	}
	if previous != nil {
		next.CredentialEpoch = previous.CredentialEpoch
	}
	if len(next.CredentialEpoch) != 32 || next.Inbound != priorIn || next.Outbound != priorOut || peer.InboundAuth.Password != "" || peer.OutboundAuth.Password != "" {
		var epoch [16]byte
		if _, err := rand.Read(epoch[:]); err != nil {
			return errors.New("SIP peer credential identity could not be generated")
		}
		next.CredentialEpoch = hex.EncodeToString(epoch[:])
	}
	if _, err := hex.DecodeString(next.CredentialEpoch); err != nil {
		return errors.New("SIP peer credential identity is invalid")
	}
	peer.InboundAuth.Password, peer.OutboundAuth.Password = "", ""
	peer.InboundAuth.HasPassword, peer.OutboundAuth.HasPassword = validPeerSecret(next.Inbound), validPeerSecret(next.Outbound)
	out.PeerSecrets = &next
	return nil
}
func validPeerSecret(s secret) bool {
	md5, e1 := hex.DecodeString(s.MD5)
	sha, e2 := hex.DecodeString(s.SHA256)
	return e1 == nil && e2 == nil && len(md5) == 16 && len(sha) == 32
}
