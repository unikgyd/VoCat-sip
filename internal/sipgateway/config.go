package sipgateway

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const SettingKey = "sip.gateway.private.v1"

type Account struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"has_password"`
	// ResetCredentialIdentity is a one-shot request, never persisted or returned.
	ResetCredentialIdentity bool     `json:"reset_credential_identity,omitempty"`
	AllowMessages           bool     `json:"allow_messages"`
	AllowCalls              bool     `json:"allow_calls"`
	OutgoingLines           []string `json:"outgoing_lines"`
	SMSSendLines            []string `json:"sms_send_lines"`
}
type Config struct {
	Enabled         bool          `json:"enabled"`
	Listen          string        `json:"listen"`
	Realm           string        `json:"realm"`
	IdentityDomain  string        `json:"identity_domain"`
	MessagesEnabled bool          `json:"messages_enabled"`
	CallsEnabled    bool          `json:"calls_enabled"`
	Algorithm       string        `json:"algorithm"`
	AllowedCIDRs    []string      `json:"allowed_cidrs"`
	Accounts        []Account     `json:"accounts"`
	CarrierEnabled  bool          `json:"carrier_enabled"`
	CarrierLines    []CarrierLine `json:"carrier_lines"`
	SMSEnabled      bool          `json:"sms_enabled"`
	Peer            *PeerConfig   `json:"peer,omitempty"`
}
type secret struct {
	MD5    string `json:"password_md5"`
	SHA256 string `json:"password_sha256"`
}
type stored struct {
	Version            int               `json:"version"`
	Config             Config            `json:"config"`
	Secrets            map[string]secret `json:"secrets"`
	PeerSecrets        *peerSecrets      `json:"peer_secrets,omitempty"`
	LocalAccountEpochs map[string]string `json:"local_account_epochs,omitempty"`
	ConfigRevision     string            `json:"config_revision,omitempty"`
	// Set only by Manager after durable admission; direct synthetic runtimes do
	// not impersonate a persisted production configuration.
	requireDurableAuthority bool
}

func DefaultConfig() Config {
	return Config{Listen: "0.0.0.0:5060", Realm: "vocat", Algorithm: "SHA-256", AllowedCIDRs: []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"}, Accounts: []Account{}}
}

var extensionPattern = regexp.MustCompile(`^[0-9]{2,12}$`)
var realmPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func digestHash(algorithm, value string) string {
	if algorithm == "MD5" {
		sum := md5.Sum([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func prepare(request Config, old stored, ephemeral bool) (stored, error) {
	if old.ConfigRevision != "" && !validLocalCredentialEpoch(old.ConfigRevision) {
		return stored{}, errors.New("SIP settings revision is invalid")
	}
	if !realmPattern.MatchString(request.Realm) || request.Algorithm != "SHA-256" && request.Algorithm != "MD5" {
		return stored{}, errors.New("SIP realm or algorithm is invalid")
	}
	if request.IdentityDomain != "" && !validIdentityDomain(request.IdentityDomain) {
		return stored{}, errors.New("SIP identity domain must be a DNS name or IP without a port")
	}
	host, port, err := net.SplitHostPort(request.Listen)
	if err != nil {
		return stored{}, errors.New("SIP listen address must be an IP and port")
	}
	ip, err := netip.ParseAddr(host)
	n, pErr := strconv.Atoi(port)
	if err != nil || ip.IsMulticast() || pErr != nil || n < 0 || n > 65535 || n == 0 && !ephemeral {
		return stored{}, errors.New("SIP listen address or port is invalid")
	}
	if len(request.AllowedCIDRs) == 0 || len(request.AllowedCIDRs) > 32 || len(request.Accounts) > 64 {
		return stored{}, errors.New("SIP requires 1..32 allowed networks and at most 64 accounts")
	}
	for _, cidr := range request.AllowedCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return stored{}, errors.New("SIP allowed network is invalid")
		}
	}
	out := stored{Version: 1, Config: request, Secrets: map[string]secret{}, LocalAccountEpochs: map[string]string{}, ConfigRevision: old.ConfigRevision}
	out.Config.Accounts = append([]Account(nil), request.Accounts...)
	out.Config.AllowedCIDRs = append([]string(nil), request.AllowedCIDRs...)
	if err := prepareCarrierConfig(&out.Config); err != nil {
		return stored{}, err
	}
	for i, a := range out.Config.Accounts {
		if !extensionPattern.MatchString(a.Username) || len(a.DisplayName) > 80 || strings.ContainsAny(a.DisplayName, "\"\\") || strings.ContainsFunc(a.DisplayName, unicode.IsControl) {
			return stored{}, errors.New("SIP extension must contain 2..12 digits and display name at most 80 safe bytes")
		}
		if _, exists := out.Secrets[a.Username]; exists {
			return stored{}, errors.New("duplicate SIP extension")
		}
		var credentials secret
		if a.Password != "" {
			if len(a.Password) < 12 || len(a.Password) > 128 {
				return stored{}, errors.New("SIP passwords must contain 12..128 bytes")
			}
			base := fmt.Sprintf("%s:%s:%s", a.Username, request.Realm, a.Password)
			credentials = secret{MD5: digestHash("MD5", base), SHA256: digestHash("SHA-256", base)}
		} else if request.Realm == old.Config.Realm {
			credentials = old.Secrets[a.Username]
		}
		if credentials.MD5 == "" || credentials.SHA256 == "" {
			return stored{}, errors.New("new accounts or a changed realm require new passwords")
		}
		out.Secrets[a.Username] = credentials
		epoch, err := prepareLocalCredentialEpoch(request.Realm, a, credentials, old)
		if err != nil {
			return stored{}, err
		}
		out.LocalAccountEpochs[a.Username] = epoch
		out.Config.Accounts[i].Password = ""
		out.Config.Accounts[i].HasPassword = true
		out.Config.Accounts[i].ResetCredentialIdentity = false
	}
	if err := preparePeerConfig(&out, old); err != nil {
		return stored{}, err
	}
	if request.Enabled && len(request.Accounts) == 0 && (out.Config.Peer == nil || !out.Config.Peer.Enabled) {
		return stored{}, errors.New("add a SIP extension before enabling the service")
	}
	return out, nil
}

func validIdentityDomain(domain string) bool {
	if addr, err := netip.ParseAddr(domain); err == nil {
		return addr.Zone() == "" && !addr.IsUnspecified() && !addr.IsMulticast()
	}
	if len(domain) > 253 || len(domain) == 0 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
