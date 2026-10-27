//go:build linux

package device

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/modem"
)

type orphanResetClaim struct{ recoveryClaim }

func (c *orphanResetClaim) VerifyIdentity(_ modem.Candidate, equipment, iccid, imsi string) error {
	if equipment != recoveryEquipment || iccid != recoveryCard || imsi != recoverySubscriber {
		return ErrSIMObservation
	}
	return nil
}

func TestOrphanResetConsumesIntentAndRetainsUncertainSerial(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		f := newRecoveryFixture()
		if uncertain {
			f.at.resetErr = modem.ErrCommandTimeout
		}
		claim := &orphanResetClaim{}
		handle, submitted, err := restartOrphanData(context.Background(), f.candidate, "/dev/recovery-secondary", claim, func(context.Context, modem.Candidate, string) bool { return true }, f.opener, func(modem.Candidate, modem.Port) error { return nil })
		if handle == nil || !submitted || !errors.Is(err, ErrDataCleanupPending) || f.at.resets != 1 || !claim.attempted || f.at.closes != 0 {
			t.Fatal("reset lost its durable fence or serial capability", err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
		_, submitted, err = restartOrphanData(context.Background(), f.candidate, "/dev/recovery-secondary", claim, func(context.Context, modem.Candidate, string) bool { return true }, f.opener, func(modem.Candidate, modem.Port) error { return nil })
		if submitted || err == nil || f.at.resets != 1 {
			t.Fatal("consumed intent replayed", err)
		}
	}
}

func TestOrphanResetRechecksAllGuardsAfterCommit(t *testing.T) {
	for _, change := range []string{"ICCID", "IMSI", "equipment", "RF", "voice", "policy", "USB", "cancel", "commit error"} {
		t.Run(change, func(t *testing.T) {
			f := newRecoveryFixture()
			claim := &orphanResetClaim{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			allowed, portOK := true, true
			claim.afterBegin = func() {
				switch change {
				case "ICCID":
					f.at.iccid = "8901000000000000002"
				case "IMSI":
					f.at.imsi = "310260000000002"
				case "equipment":
					f.at.equipment = "123456789012346"
				case "RF":
					f.at.mode = 4
				case "voice":
					f.at.rows = "+CLCC: 1,1,0,0,0"
				case "policy":
					allowed = false
				case "USB":
					portOK = false
				case "cancel":
					cancel()
				}
			}
			if change == "commit error" {
				claim.err = errors.New("uncertain database result")
			}
			handle, submitted, err := restartOrphanData(ctx, f.candidate, "/dev/recovery-secondary", claim, func(context.Context, modem.Candidate, string) bool { return allowed }, f.opener, func(modem.Candidate, modem.Port) error {
				if !portOK {
					return ErrSIMObservation
				}
				return nil
			})
			if handle != nil {
				_ = handle.Close()
			}
			if submitted || err == nil || f.at.resets != 0 || claim.releases != 0 {
				t.Fatal("reset or release admitted after changed evidence", err)
			}
		})
	}
}

func TestOrphanResetRefusesPrimaryPortAndRetainsInterruptedProbe(t *testing.T) {
	f := newRecoveryFixture()
	claim := &orphanResetClaim{}
	allow := func(context.Context, modem.Candidate, string) bool { return true }
	verify := func(modem.Candidate, modem.Port) error { return nil }
	if _, submitted, err := restartOrphanData(context.Background(), f.candidate, "/dev/recovery-primary", claim, allow, f.opener, verify); submitted || err == nil || f.opener.opens != 0 {
		t.Fatal("primary port opened", err)
	}
	f.at.readErrorCommand = "AT+CIMI"
	handle, submitted, err := restartOrphanData(context.Background(), f.candidate, "/dev/recovery-secondary", claim, allow, f.opener, verify)
	if handle == nil || submitted || err == nil || f.at.closes != 0 || claim.begins != 0 || f.at.resets != 0 {
		t.Fatal("interrupted probe was reopened or consumed an intent", err)
	}
	_ = handle.Close()
}
