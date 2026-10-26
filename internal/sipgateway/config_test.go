package sipgateway

import (
	"strings"
	"testing"
)

func TestConfigRejectsUnsafeAndMissingFields(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Realm = "bad\"realm" },
		func(c *Config) { c.Listen = "localhost:5060" },
		func(c *Config) { c.Listen = "127.0.0.1:0" },
		func(c *Config) { c.Listen = "224.0.0.1:5060" },
		func(c *Config) { c.Algorithm = "none" },
		func(c *Config) { c.AllowedCIDRs = nil },
		func(c *Config) { c.Accounts[0].Password = "short" },
		func(c *Config) { c.Accounts[0].Username = "1001@foreign" },
		func(c *Config) { c.Accounts[0].DisplayName = "bad\r\nFrom: spoof" },
		func(c *Config) { c.Accounts = append(c.Accounts, c.Accounts[0]) },
		func(c *Config) { c.Accounts = nil; c.Enabled = true },
	} {
		c := DefaultConfig()
		c.Accounts = []Account{{Username: "1001", Password: testPassword}}
		mutate(&c)
		if _, err := prepare(c, stored{}, false); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
func TestConfigPasswordIsWriteOnlyAndPreserved(t *testing.T) {
	c := DefaultConfig()
	c.Accounts = []Account{{Username: "1001", Password: testPassword}}
	first, err := prepare(c, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Config.Accounts[0].Password != "" || !first.Config.Accounts[0].HasPassword || strings.Contains(first.Secrets["1001"].SHA256, testPassword) {
		t.Fatal("plaintext retained")
	}
	second, err := prepare(first.Config, first, false)
	if err != nil || second.Secrets["1001"] != first.Secrets["1001"] {
		t.Fatal("blank password failed to preserve secret")
	}
	c = first.Config
	c.Realm = "changed"
	if _, err := prepare(c, first, false); err == nil {
		t.Fatal("realm change accepted without replacement password")
	}
	c = first.Config
	c.Accounts = append([]Account{}, c.Accounts...)
	c.Accounts[0].Username = "1002"
	if _, err := prepare(c, first, false); err == nil {
		t.Fatal("new user reused another account's password")
	}
}
func TestDigestParserRejectsAmbiguity(t *testing.T) {
	for _, value := range []string{
		`Basic anything`, `Digest username="1001",username="1002"`, `Digest nonce="unterminated`,
		`Digest username="1001",`, `Digest username=""`, "Digest user\r\nname=x", "Digest username=bad\r\nvalue",
	} {
		if _, err := parseDigest(value); err == nil {
			t.Fatalf("accepted malformed digest: %q", value)
		}
	}
}
func TestDigestNonceSourceURIAndReplayChecks(t *testing.T) {
	r := testRuntime(t, "SHA-256")
	c := newWireClient(t, r)
	c.challengeRegister()
	auth := strings.TrimSpace(strings.TrimPrefix(c.authorization(testPassword), "Authorization: "))
	if _, ok := r.authenticate("REGISTER", "sip:vocat", "192.0.2.20", auth); ok {
		t.Fatal("nonce accepted from different source")
	}
	if _, ok := r.authenticate("REGISTER", "sip:wrong", "127.0.0.1", auth); ok {
		t.Fatal("URI mismatch accepted")
	}
	if _, ok := r.authenticate("REGISTER", "sip:vocat", "127.0.0.1", auth); !ok {
		t.Fatal("independent client's digest rejected")
	}
	if _, ok := r.authenticate("REGISTER", "sip:vocat", "127.0.0.1", auth); ok {
		t.Fatal("digest replay accepted")
	}
}

func TestIdentityDomainValidation(t *testing.T) {
	for _, domain := range []string{"voice.example.test", "127.0.0.1", "2001:db8::1", "vocat"} {
		c := DefaultConfig()
		c.IdentityDomain = domain
		if _, err := prepare(c, stored{}, false); err != nil {
			t.Fatalf("valid domain %q: %v", domain, err)
		}
	}
	for _, domain := range []string{"sip:voice.example", "voice.example:5060", "u@voice.example", "voice.example\r\nFrom: spoof", ".example", "bad..example", "-bad.example", "0.0.0.0", "[::1]", "fe80::1%eth0"} {
		c := DefaultConfig()
		c.IdentityDomain = domain
		if _, err := prepare(c, stored{}, false); err == nil {
			t.Fatalf("invalid domain accepted: %q", domain)
		}
	}
}
