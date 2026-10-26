//go:build linux

package hostims

import (
	"context"
	"net/netip"
	"testing"
)

func recoveryNetworkFixture(t *testing.T) (*BearerPool, *fakeBearerBackend, *memoryClaims, Selection, *securityFakeNetwork) {
	t.Helper()
	p, b, j, s := bearerFixture(t)
	l, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := &securityFakeNetwork{values: map[string]string{}}
	r := Runtime{APN: "ims", IPv4: l.configuration.Addresses[0].Address, IPv6: l.configuration.Addresses[1].Address, PCSCF: []netip.Addr{netip.MustParseAddr("198.51.100.20")}}
	if _, err := prepareKernelNetworkWith(context.Background(), l, r, func() (kernelNetworkDriver, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	p, s = restartBearerPool(t, p, b, j, s)
	return p, b, j, s, d
}

func TestRestartNetworkCleanupRemovesOnlyJournaledObjectsBeforePDN(t *testing.T) {
	ctx := context.Background()
	p, b, j, s, d := recoveryNetworkFixture(t)
	d.values["foreign:route"] = "foreign fingerprint"
	owner, err := p.RecoveryCleanup(ctx, s)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	table := j.record(s.key).Network.Table
	recovery, err := reopenPendingNetworkWith(ctx, owner, func(_ *BearerLease, got int) (kernelNetworkDriver, error) {
		if got != table {
			t.Fatal("allocated another table")
		}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	owner.networkRecovery = recovery
	if err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(d.values) != 1 || d.values["foreign:route"] != "foreign fingerprint" {
		t.Fatal("cleanup removed foreign objects", d.values)
	}
	if b.count("create") != 1 || b.count("disconnect") != 1 || b.count("delete") != 1 || p.HasPendingClaim(s) {
		t.Fatal("recovery created/reused or abandoned a PDN")
	}
}

func TestRestartNetworkCleanupRefusesChangedOrUnknownRecords(t *testing.T) {
	for _, mode := range []string{"pending", "uncertain", "fingerprint", "configuration", "sim", "namespace"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			p, b, _, s, d := recoveryNetworkFixture(t)
			owner, err := p.RecoveryCleanup(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			c := owner.claim
			c.mu.Lock()
			switch mode {
			case "pending":
				op := c.record.Network.Operations[0]
				op.Kind = "remove-" + op.Kind
				c.record.Network.Pending = &op
			case "uncertain":
				c.record.Network.Phase = "uncertain"
			case "fingerprint":
				c.record.Network.Operations[0].Hash = ""
			case "configuration":
				c.record.Network.Configuration = hashText("different IP configuration")
			case "namespace":
				c.record.Network.Namespace = hashText("different namespace")
			case "sim":
				b.mu.Lock()
				m := b.modems[s.snapshot.Path]
				m.Binding.ICCID = "8901000000000000099"
				b.modems[s.snapshot.Path] = m
				b.mu.Unlock()
			}
			c.mu.Unlock()
			before := len(d.values)
			if _, err := reopenPendingNetworkWith(ctx, owner, func(*BearerLease, int) (kernelNetworkDriver, error) { return d, nil }); err == nil {
				t.Fatal("unproven ownership reopened")
			}
			if len(d.values) != before || b.count("delete") != 0 || b.count("disconnect") != 0 || !p.HasPendingClaim(s) {
				t.Fatal("rejection changed original resources")
			}
		})
	}
}
