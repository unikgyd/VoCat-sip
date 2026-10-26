package hostims

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func claimNetworkFixture() *networkClaim {
	return &networkClaim{Token: hashText("network"), Phase: "active", Namespace: hashText("netns"), Interface: "qmimux0", Index: 4, Anchor: hashText("anchor"), Table: 0x41234567, Configuration: hashText("configuration"), Operations: []networkOperation{{Kind: "address", Local: netip.MustParsePrefix("192.0.2.1/32"), Hash: hashText("address"), Owned: true}}}
}

func TestNetworkClaimValidationAndClone(t *testing.T) {
	base := claimNetworkFixture()
	if !validNetworkClaim(base) || !networkReady(base) {
		t.Fatal("valid active journal rejected")
	}
	for name, mutate := range map[string]func(*networkClaim){
		"table":                func(r *networkClaim) { r.Table = 254 },
		"namespace":            func(r *networkClaim) { r.Namespace = "net:[1234]" },
		"interface":            func(r *networkClaim) { r.Interface = "../wwan0" },
		"phase":                func(r *networkClaim) { r.Phase = "ready" },
		"negative-index":       func(r *networkClaim) { r.Index = -1 },
		"borrowed-rule":        func(r *networkClaim) { r.Operations[0].Kind = "rule"; r.Operations[0].Owned = false },
		"borrowed-no-snapshot": func(r *networkClaim) { r.Operations[0].Owned = false; r.Operations[0].Hash = "" },
		"address-mtu":          func(r *networkClaim) { r.Operations[0].MTU = 1500 },
		"address-media":        func(r *networkClaim) { r.Operations[0].Media = true },
		"address-remote":       func(r *networkClaim) { r.Operations[0].Remote = netip.MustParseAddr("198.51.100.1") },
		"removed-operation":    func(r *networkClaim) { r.Operations[0].Kind = "remove-address" },
		"duplicate-operation":  func(r *networkClaim) { r.Operations = append(r.Operations, r.Operations[0]) },
		"oversize":             func(r *networkClaim) { r.Operations = make([]networkOperation, 24) },
		"unknown-removal": func(r *networkClaim) {
			r.Pending = &networkOperation{Kind: "remove-route", Local: r.Operations[0].Local, Remote: netip.MustParseAddr("198.51.100.1"), Owned: true, Hash: hashText("unknown")}
		},
		"bad-route-family": func(r *networkClaim) {
			r.Pending = &networkOperation{Kind: "route", Local: r.Operations[0].Local, Remote: netip.MustParseAddr("2001:db8::1"), Owned: true}
		},
		"bad-gateway": func(r *networkClaim) {
			r.Pending = &networkOperation{Kind: "route", Local: r.Operations[0].Local, Remote: netip.MustParseAddr("198.51.100.1"), Gateway: netip.MustParseAddr("127.0.0.1"), Owned: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := cloneNetworkClaim(base)
			mutate(r)
			if validNetworkClaim(r) {
				t.Fatal("invalid journal accepted")
			}
		})
	}
	pending := base.Operations[0]
	pending.Kind = "remove-address"
	base.Pending = &pending
	if !validNetworkClaim(base) || networkReady(base) {
		t.Fatal("valid pending removal lost its fence")
	}
	copy := cloneNetworkClaim(base)
	copy.Operations[0].Hash = hashText("changed")
	copy.Pending.Hash = hashText("changed")
	if copy.Operations[0].Hash == base.Operations[0].Hash || copy.Pending.Hash == base.Pending.Hash {
		t.Fatal("mutable clone aliases the live record")
	}
	base.Pending = nil
	base.Operations[0].Hash = ""
	if !validNetworkClaim(base) || networkReady(base) {
		t.Fatal("acknowledged but unread add must stay fenced")
	}
}

func TestNetworkClaimLargestSupportedJournalFitsStorage(t *testing.T) {
	pool, _, journal, selection := bearerFixture(t)
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{APN: strings.Repeat("a", 50) + "." + strings.Repeat("b", 49)})
	if err != nil {
		t.Fatal(err)
	}
	r := journal.record(selection.key)
	r.Owner = ":1234567890.1234567890"
	r.Modem = "/org/freedesktop/ModemManager1/Modem/1234567890"
	r.Path = "/org/freedesktop/ModemManager1/Bearer/1234567890"
	r.Before = nil
	for index := 0; index < 128; index++ {
		r.SIMChallenges = append(r.SIMChallenges, hashText(fmt.Sprint(index)))
	}
	n := claimNetworkFixture()
	local := netip.MustParsePrefix("2001:db8:abcd:ef01:1234:5678:9abc:def0/128")
	for index := 1; index < 20; index++ {
		n.Operations = append(n.Operations, networkOperation{Kind: "route", Local: local, Remote: netip.MustParseAddr(fmt.Sprintf("2001:db8:abcd:ef01:1234:5678:9abc:%x", index)), Gateway: netip.MustParseAddr("2001:db8:abcd:ef01:1234:5678:9abc:ffff"), MTU: 65535, Hash: hashText(fmt.Sprint(index)), Owned: true, Media: true})
	}
	pending := n.Operations[len(n.Operations)-1]
	pending.Kind = "remove-route"
	n.Pending = &pending
	n.Phase = "uncertain"
	r.Network = n
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 16384 {
		t.Fatal("supported journal exceeds storage limit", len(raw), err)
	}
	if _, err := decodeClaim(raw); err != nil {
		t.Fatal("supported journal cannot be recovered as a fence", err)
	}
	t.Logf("worst-case compact claim: %d / 16384 bytes", len(raw))
	_ = lease.Close(context.Background())
}

func TestNetworkClaimSIMGateRejectsPartialOrMutatingNetwork(t *testing.T) {
	pool, _, _, selection := bearerFixture(t)
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &SIMProvider{lease: lease}
	for _, phase := range []string{"preparing", "cleaning", "uncertain"} {
		lease.claim.record.Network = claimNetworkFixture()
		lease.claim.record.Network.Phase = phase
		if provider.currentLocked(lease.claim) {
			t.Fatal("SIM allowed during", phase)
		}
	}
	lease.claim.record.Network = claimNetworkFixture()
	lease.claim.record.Network.Operations[0].Hash = ""
	if provider.currentLocked(lease.claim) {
		t.Fatal("SIM allowed before snapshot was durable")
	}
	lease.claim.record.Network = nil
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
