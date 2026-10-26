package sipgateway

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const peerInboundPassword = "peer-inbound-offline-password"
const peerOutboundPassword = "peer-outbound-offline-password"

func isolatedPeerConfig() Config {
	config := DefaultConfig()
	config.Enabled, config.Listen = true, "127.0.0.1:15060"
	config.CarrierLines = []CarrierLine{{DeviceID: "line-a"}}
	config.Peer = &PeerConfig{Enabled: true, Endpoint: "127.0.0.1:15062",
		InboundAuth:        PeerInboundAuth{Username: "pbx-in", Password: peerInboundPassword},
		OutboundAuth:       PeerOutboundAuth{Username: "gateway-out", Realm: "asterisk", Password: peerOutboundPassword},
		AllowCallsFromPeer: true, AllowSMSFromPeer: true, AllowCallsToPeer: true, AllowSMSToPeer: true,
		OutgoingLines: []string{"line-a"}, SMSSendLines: []string{"line-a"},
		IncomingRoutes: []PeerIncomingRoute{{DeviceID: "line-a", CallTarget: "ring-all", SMSTarget: "inbox"}}}
	return config
}
func preparedIsolatedPeer(t *testing.T) stored {
	t.Helper()
	prepared, err := prepare(isolatedPeerConfig(), stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}
func TestPeerConfigDefaultsRemainOffAndCanSaveDisabledEmptyPeer(t *testing.T) {
	config := DefaultConfig()
	if config.Peer != nil || config.Enabled {
		t.Fatal("default peer/service became enabled")
	}
	config.Peer = &PeerConfig{}
	prepared, err := prepare(config, stored{}, false)
	if err != nil || prepared.PeerSecrets != nil || prepared.Config.Peer.Enabled || !reflect.DeepEqual(prepared.Config.Peer.OutboundAuth.Algorithms, []string{"SHA-256"}) {
		t.Fatal("disabled peer defaults invalid", err)
	}
	if len(config.Peer.OutboundAuth.Algorithms) != 0 {
		t.Fatal("prepare mutated caller")
	}
}
func TestPeerConfigIndependentWriteOnlyCredentialsAndReload(t *testing.T) {
	prepared := preparedIsolatedPeer(t)
	if len(prepared.Config.Accounts) != 0 || len(prepared.Secrets) != 0 || prepared.PeerSecrets == nil {
		t.Fatal("peer became a local account")
	}
	public, err := json.Marshal(prepared.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{peerInboundPassword, peerOutboundPassword, prepared.PeerSecrets.Inbound.SHA256, prepared.PeerSecrets.Outbound.MD5, prepared.PeerSecrets.CredentialEpoch} {
		if strings.Contains(string(public), private) {
			t.Fatal("private peer material exposed")
		}
	}
	if !prepared.Config.Peer.InboundAuth.HasPassword || !prepared.Config.Peer.OutboundAuth.HasPassword || prepared.PeerSecrets.Inbound == prepared.PeerSecrets.Outbound {
		t.Fatal("peer credential separation missing")
	}
	encoded, _ := json.Marshal(prepared)
	var saved stored
	if json.Unmarshal(encoded, &saved) != nil {
		t.Fatal("private setting not reloadable")
	}
	reloaded, err := prepare(saved.Config, saved, false)
	if err != nil || !reflect.DeepEqual(reloaded, prepared) {
		t.Fatal("blank password/reload changed credential authority", err)
	}
	for _, direction := range []string{"inbound", "outbound"} {
		t.Run(direction, func(t *testing.T) {
			request := cloneConfig(prepared.Config)
			if direction == "inbound" {
				request.Peer.InboundAuth.Password = peerInboundPassword
			} else {
				request.Peer.OutboundAuth.Password = peerOutboundPassword
			}
			next, err := prepare(request, prepared, false)
			if err != nil || next.PeerSecrets.CredentialEpoch == prepared.PeerSecrets.CredentialEpoch {
				t.Fatal("explicit password reset did not rotate credential epoch", err)
			}
		})
	}
	request := cloneConfig(prepared.Config)
	request.Peer = nil
	cleared, err := prepare(request, prepared, false)
	if err == nil {
		t.Fatal("enabled peer-only service could be cleared without disabling it")
	}
	request.Enabled = false
	cleared, err = prepare(request, prepared, false)
	if err != nil || cleared.PeerSecrets != nil {
		t.Fatal("peer removal retained secrets", err)
	}
}
func TestPeerConfigRejectsUnsafeEndpointsPermissionsTargetsAndCredentialReuse(t *testing.T) {
	mutations := map[string]func(*Config){
		"hostname":              func(c *Config) { c.Peer.Endpoint = "localhost:5060" },
		"public":                func(c *Config) { c.Peer.Endpoint = "8.8.8.8:5060" },
		"wildcard":              func(c *Config) { c.Peer.Endpoint = "0.0.0.0:5060" },
		"multicast":             func(c *Config) { c.Peer.Endpoint = "239.1.1.1:5060" },
		"linklocal":             func(c *Config) { c.Peer.Endpoint = "169.254.1.1:5060" },
		"mapped":                func(c *Config) { c.Peer.Endpoint = "[::ffff:127.0.0.1]:5060" },
		"zone":                  func(c *Config) { c.Peer.Endpoint = "[fe80::1%eth0]:5060" },
		"zero-port":             func(c *Config) { c.Peer.Endpoint = "127.0.0.1:0" },
		"family":                func(c *Config) { c.Peer.Endpoint = "[::1]:5060" },
		"self":                  func(c *Config) { c.Peer.Endpoint = c.Listen },
		"self-wildcard":         func(c *Config) { c.Listen = "0.0.0.0:15062" },
		"outside-cidr":          func(c *Config) { c.AllowedCIDRs = []string{"10.0.0.0/8"} },
		"unknown-line":          func(c *Config) { c.Peer.OutgoingLines = []string{"other"} },
		"duplicate-line":        func(c *Config) { c.Peer.SMSSendLines = []string{"line-a", "line-a"} },
		"unknown-route":         func(c *Config) { c.Peer.IncomingRoutes[0].DeviceID = "other" },
		"duplicate-route":       func(c *Config) { c.Peer.IncomingRoutes = append(c.Peer.IncomingRoutes, c.Peer.IncomingRoutes[0]) },
		"uri-route":             func(c *Config) { c.Peer.IncomingRoutes[0].SMSTarget = "sip:1001@foreign" },
		"empty-route":           func(c *Config) { c.Peer.IncomingRoutes[0].CallTarget, c.Peer.IncomingRoutes[0].SMSTarget = "", "" },
		"invalid-user":          func(c *Config) { c.Peer.InboundAuth.Username = "bad:user" },
		"short-inbound":         func(c *Config) { c.Peer.InboundAuth.Password = "short" },
		"short-outbound":        func(c *Config) { c.Peer.OutboundAuth.Password = "short" },
		"invalid-realm":         func(c *Config) { c.Peer.OutboundAuth.Realm = "bad\r\nrealm" },
		"unsupported-algorithm": func(c *Config) { c.Peer.OutboundAuth.Algorithms = []string{"SHA-512-256"} },
		"duplicate-algorithm":   func(c *Config) { c.Peer.OutboundAuth.Algorithms = []string{"MD5", "MD5"} },
		"fake-has-password":     func(c *Config) { c.Peer.InboundAuth.Password = ""; c.Peer.InboundAuth.HasPassword = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			config := isolatedPeerConfig()
			mutate(&config)
			if _, err := prepare(config, stored{}, false); err == nil {
				t.Fatal("unsafe peer configuration accepted")
			}
		})
	}
	prepared := preparedIsolatedPeer(t)
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Realm = "changed" },
		func(c *Config) { c.Peer.InboundAuth.Username = "other" },
		func(c *Config) { c.Peer.OutboundAuth.Username = "other" },
		func(c *Config) { c.Peer.OutboundAuth.Realm = "changed" },
	} {
		request := cloneConfig(prepared.Config)
		mutate(&request)
		if _, err := prepare(request, prepared, false); err == nil {
			t.Fatal("peer credentials reused for different username/realm")
		}
	}
}
func TestPeerConfigDirectionalCredentialsAndActualListenerFence(t *testing.T) {
	config := isolatedPeerConfig()
	config.Peer.AllowCallsFromPeer, config.Peer.AllowCallsToPeer, config.Peer.AllowSMSToPeer = false, false, false
	config.Peer.OutboundAuth = PeerOutboundAuth{}
	if _, err := prepare(config, stored{}, false); err != nil {
		t.Fatal("SMS-from-only required unused outbound credential", err)
	}
	config.Peer.AllowCallsFromPeer = true
	if _, err := prepare(config, stored{}, false); err == nil {
		t.Fatal("incoming calls lacked BYE authentication credentials")
	}
	config = isolatedPeerConfig()
	config.Listen = "127.0.0.1:0"
	prepared, err := prepare(config, stored{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if validatePeerListener(prepared.Config.Peer, "127.0.0.1:15062") == nil {
		t.Fatal("actual ephemeral listener self-loop accepted")
	}
	if validatePeerListener(prepared.Config.Peer, "127.0.0.1:15061") != nil {
		t.Fatal("independent bound peer rejected")
	}
	config = isolatedPeerConfig()
	config.Listen, config.Peer.Endpoint = "[::1]:15060", "[::1]:15062"
	if _, err := prepare(config, stored{}, false); err != nil {
		t.Fatal("same-family IPv6 peer rejected", err)
	}
}
func TestPeerConfigDeepCopiesAndSMSRecipientBound(t *testing.T) {
	config := isolatedPeerConfig()
	prepared, err := prepare(config, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	copy := cloneConfig(prepared.Config)
	copy.Peer.IncomingRoutes[0].SMSTarget = "changed"
	copy.Peer.OutgoingLines[0], copy.Peer.SMSSendLines[0], copy.Peer.OutboundAuth.Algorithms[0] = "other", "other", "MD5"
	if prepared.Config.Peer.IncomingRoutes[0].SMSTarget != "inbox" || prepared.Config.Peer.OutgoingLines[0] != "line-a" || prepared.Config.Peer.SMSSendLines[0] != "line-a" || prepared.Config.Peer.OutboundAuth.Algorithms[0] != "SHA-256" || config.Peer.InboundAuth.Password != peerInboundPassword {
		t.Fatal("peer config slices/inputs were aliased")
	}
	for i := 0; i < 16; i++ {
		user := fmt.Sprintf("%04d", 1000+i)
		config.Accounts = append(config.Accounts, Account{Username: user, Password: testPassword})
		config.CarrierLines[0].SMSRecipients = append(config.CarrierLines[0].SMSRecipients, user)
	}
	if _, err := prepare(config, stored{}, false); err == nil {
		t.Fatal("peer exceeded durable recipient bound")
	}
	config.CarrierLines[0].SMSRecipients = config.CarrierLines[0].SMSRecipients[:15]
	if _, err := prepare(config, stored{}, false); err != nil {
		t.Fatal("16 total SMS recipients rejected", err)
	}
}
func TestPeerRecipientIdentityBindsAuthorityEpochAndTarget(t *testing.T) {
	prepared := preparedIsolatedPeer(t)
	identity := peerRecipientIdentity(prepared, "inbox")
	if len(identity) != 64 || peerRecipientIdentity(prepared, "other") == identity {
		t.Fatal("peer recipient target identity missing")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Peer.Endpoint = "127.0.0.1:15063" },
		func(c *Config) { c.Peer.OutboundAuth.Password = peerOutboundPassword },
		func(c *Config) {
			c.Peer.OutboundAuth.Realm = "changed"
			c.Peer.OutboundAuth.Password = peerOutboundPassword
		},
	} {
		config := cloneConfig(prepared.Config)
		mutate(&config)
		next, err := prepare(config, prepared, false)
		if err != nil || peerRecipientIdentity(next, "inbox") == identity {
			t.Fatal("recipient authority change reused durable identity", err)
		}
	}
	prepared.Config.Peer.Enabled = false
	if peerRecipientIdentity(prepared, "inbox") != "" {
		t.Fatal("disabled peer retained delivery identity")
	}
}
