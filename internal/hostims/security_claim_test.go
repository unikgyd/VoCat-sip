package hostims

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func securityClaimFixture() *securityClaim {
	return &securityClaim{Token: hashText("scope"), Phase: "listening", Mark: 0x60000001, ClientReqID: 0x80000001, ServerReqID: 0x80000002,
		Flows:      []securityFlow{{Local: netip.MustParseAddr("192.0.2.10"), Remote: netip.MustParseAddr("198.51.100.20")}},
		Operations: []securityOperation{{Kind: "tc", Ordinal: 0, Hash: hashText("qdisc")}, {Kind: "tc", Ordinal: 1, Hash: hashText("flower")}}}
}

func TestSecurityClaimValidationCloneAndSIMFence(t *testing.T) {
	r := securityClaimFixture()
	if !validSecurityClaim(r) {
		t.Fatal("valid bounded security journal refused")
	}
	for name, mutate := range map[string]func(*securityClaim){
		"unknown phase":       func(r *securityClaim) { r.Phase = "ready" },
		"shared reqid":        func(r *securityClaim) { r.ServerReqID = r.ClientReqID },
		"unreserved mark":     func(r *securityClaim) { r.Mark = 1 },
		"unreserved reqid":    func(r *securityClaim) { r.ClientReqID = 1 },
		"bad token":           func(r *securityClaim) { r.Token = "not-a-token" },
		"duplicate flow":      func(r *securityClaim) { r.Flows = append(r.Flows, r.Flows[0]) },
		"mixed flow":          func(r *securityClaim) { r.Flows[0].Remote = netip.MustParseAddr("2001:db8::2") },
		"same endpoint":       func(r *securityClaim) { r.Flows[0].Remote = r.Flows[0].Local },
		"missing snapshot":    func(r *securityClaim) { r.Operations[0].Hash = "" },
		"unknown object":      func(r *securityClaim) { r.Operations[0].Kind = "flush" },
		"duplicate operation": func(r *securityClaim) { r.Operations = append(r.Operations, r.Operations[0]) },
		"unknown removal": func(r *securityClaim) {
			r.Pending = &securityOperation{Kind: "remove-tc", Ordinal: 2, Hash: hashText("foreign")}
		},
		"oversized list": func(r *securityClaim) { r.Operations = make([]securityOperation, 22) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := cloneSecurityClaim(r)
			mutate(copy)
			if validSecurityClaim(copy) {
				t.Fatal("invalid security journal accepted")
			}
		})
	}
	n := claimNetworkFixture()
	n.Security = r
	for _, phase := range []string{"reserving", "installing", "cleaning", "uncertain"} {
		n.Security.Phase = phase
		if networkReady(n) {
			t.Fatal("SIM allowed across security mutation/uncertainty", phase)
		}
	}
	n.Security.Phase = "listening"
	if !networkReady(n) {
		t.Fatal("AKA must be allowed after listener scope preparation")
	}
	copy := cloneNetworkClaim(n)
	copy.Security.Flows[0].Remote = netip.MustParseAddr("198.51.100.21")
	copy.Security.Operations[0].Hash = hashText("mutated")
	if copy.Security.Flows[0] == n.Security.Flows[0] || copy.Security.Operations[0].Hash == n.Security.Operations[0].Hash {
		t.Fatal("security journal aliases the live claim")
	}
	op := n.Security.Operations[0]
	op.Kind = "remove-tc"
	n.Security.Pending = &op
	if !validNetworkClaim(n) || networkReady(n) {
		t.Fatal("pending security removal lost its durable fence")
	}
}

func TestSecurityClaimNeverSerializesKeyMaterial(t *testing.T) {
	r := securityClaimFixture()
	r.Agreement = &securityAgreementRecord{Local: r.Flows[0].Local, Remote: r.Flows[0].Remote, Encryption: "aes-cbc", Integrity: "hmac-sha-1-96",
		UEClientSPI: 1001, UEServerSPI: 1002, PCSCFClientSPI: 2001, PCSCFServerSPI: 2002, UEClientPort: 62001, UEServerPort: 62002, PCSCFClientPort: 62003, PCSCFServerPort: 62004}
	r.Operations = append(r.Operations, securityOperation{Kind: "xfrm", Ordinal: 0, Key: json.RawMessage(`{"Kind":"state"}`), Shape: hashText("desired"), Hash: hashText("observed")})
	if !validSecurityClaim(r) {
		t.Fatal("portable scalar journal rejected")
	}
	data, err := json.Marshal(r)
	if err != nil || strings.Contains(string(data), "IntegrityKey") || strings.Contains(string(data), "EncryptionKey") || strings.Contains(string(data), "wireAdd") {
		t.Fatal("secret-bearing runtime fields reached the durable schema")
	}
	copy := cloneSecurityClaim(r)
	copy.Operations[2].Key[0] = '['
	if r.Operations[2].Key[0] != '{' {
		t.Fatal("opaque scalar key aliases journal storage")
	}
}
