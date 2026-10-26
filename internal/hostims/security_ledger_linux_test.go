//go:build linux

package hostims

import (
	"context"
	"testing"
)

func TestKernelSecurityDuplicateLedgerCannotMaskMissingObject(t *testing.T) {
	for _, kind := range []string{"tc", "xfrm"} {
		t.Run(kind, func(t *testing.T) {
			n, s, x, tc, _ := securityOwnerFixture(t)
			if _, err := s.InstallIPSec(context.Background(), securityOwnerConfig()); err != nil {
				t.Fatal(err)
			}
			if err := n.locked(context.Background(), func(context.Context) error {
				r := n.record.Security
				if kind == "xfrm" {
					// Three TC entries precede the four SAs. Preserve the
					// total count while duplicating one ordinal over a missing SA.
					missing, err := decodeSecurityXFRMKey(r.Operations[6])
					if err != nil {
						return err
					}
					r.Operations[6] = r.Operations[5]
					x.mu.Lock()
					delete(x.objects, missing)
					x.mu.Unlock()
				} else {
					r.Operations[2] = r.Operations[1]
					delete(tc.objects, securityTCOperation{Kind: "flower", Ordinal: 2})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := n.Verify(context.Background()); err == nil {
				t.Fatal("duplicate ordinal masked a missing kernel object")
			}
			if err := s.Close(context.Background()); err == nil || x.deletes != 0 || tc.deletes != 0 {
				t.Fatal("invalid ledger permitted partial cleanup")
			}
		})
	}
}
