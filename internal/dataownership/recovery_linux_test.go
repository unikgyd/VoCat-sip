//go:build linux

package dataownership

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func TestRecoveryIntentIsDurableOnceAndOriginalReleaseStillWorks(t *testing.T) {
	j := fixtureJournal(t, t.TempDir())
	original, err := acquireFixture(j)
	if err != nil {
		t.Fatal(err)
	}
	c := original.(device.DataRecoveryClaim)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		admitted, err := c.BeginRecovery(ctx, fixtureCandidate(), fixtureEquipment, "1234567890123456789", "123456789012345")
		if err != nil || admitted != (i == 0) {
			t.Fatalf("restart admission %d: %v %v", i, admitted, err)
		}
	}
	stored, err := j.Database.AppSetting(ctx, store.DataBearerClaimPrefix+digest("imei:"+fixtureEquipment))
	var actual record
	if err != nil || json.Unmarshal(stored.Value, &actual) != nil || !actual.RecoveryAttempted || !stored.Sensitive {
		t.Fatal("restart intent was not private and durable", err)
	}
	if err := c.Release(ctx); err != nil {
		t.Fatal("original release did not use its updated record", err)
	}
	if allowed, err := c.BeginRecovery(ctx, fixtureCandidate(), fixtureEquipment, "1234567890123456789", "123456789012345"); allowed || !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("released owner restarted a modem", err)
	}
	if pending, err := j.Pending(ctx, fixtureCandidate()); err != nil || pending {
		t.Fatal("updated original claim remained after acknowledged release", err)
	}
}

func TestRecoveryIntentRejectsChangedOriginalIdentity(t *testing.T) {
	for _, scenario := range []string{"equipment", "card", "subscriber", "generation", "interface", "physical"} {
		t.Run(scenario, func(t *testing.T) {
			j := fixtureJournal(t, t.TempDir())
			original, err := acquireFixture(j)
			if err != nil {
				t.Fatal(err)
			}
			defer original.Release(context.Background())
			c := original.(device.DataRecoveryClaim)
			candidate, equipment, iccid, imsi := fixtureCandidate(), fixtureEquipment, "1234567890123456789", "123456789012345"
			switch scenario {
			case "equipment":
				equipment = "123456789012346"
			case "card":
				iccid = "1234567890123456790"
			case "subscriber":
				imsi = "123456789012346"
			case "generation":
				candidate.USBGeneration = "1:8"
			case "interface":
				candidate.NetworkInterface = "replacement0"
			case "physical":
				candidate.ID = "replacement"
			}
			if admitted, err := c.BeginRecovery(context.Background(), candidate, equipment, iccid, imsi); admitted || !errors.Is(err, device.ErrDataCleanupPending) {
				t.Fatal("changed original binding authorized restart", err)
			}
			if admitted, err := c.BeginRecovery(context.Background(), fixtureCandidate(), fixtureEquipment, "1234567890123456789", "123456789012345"); err != nil || !admitted {
				t.Fatal("a rejected identity consumed the valid owner's intent", err)
			}
		})
	}
}

func TestRecoveryDoesNotOverwriteChangedClaimOrConflictWithHostIMS(t *testing.T) {
	for _, scenario := range []string{"changed claim", "Host IMS conflict"} {
		t.Run(scenario, func(t *testing.T) {
			j := fixtureJournal(t, t.TempDir())
			original, err := acquireFixture(j)
			if err != nil {
				t.Fatal(err)
			}
			c := original.(*claim)
			defer c.guard.Close()
			ctx := context.Background()
			if scenario == "changed claim" {
				var changed record
				if err := json.Unmarshal(c.raw, &changed); err != nil {
					t.Fatal(err)
				}
				changed.Token = "changed-owner-token"
				raw, _ := json.Marshal(changed)
				if ok, err := j.Database.CompareDataBearerClaim(ctx, c.key, c.raw, raw); err != nil || !ok {
					t.Fatal(err)
				}
			} else {
				if ok, err := j.Database.CompareHostIMSClaim(ctx, "host.ims.claim.private.v1."+digest("imei:"+fixtureEquipment), nil, json.RawMessage(`{"conflict":true}`)); err != nil || !ok {
					t.Fatal(err)
				}
			}
			if admitted, err := c.BeginRecovery(ctx, fixtureCandidate(), fixtureEquipment, "1234567890123456789", "123456789012345"); admitted || !errors.Is(err, device.ErrDataCleanupPending) {
				t.Fatal("conflicting ownership authorized recovery", err)
			}
			if pending, err := j.Pending(ctx, modem.Candidate{ID: fixtureCandidate().ID}); err != nil || !pending {
				t.Fatal("original fence disappeared", err)
			}
		})
	}
}
