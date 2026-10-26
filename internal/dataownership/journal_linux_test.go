//go:build linux

package dataownership

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/modem"
	"vocat/internal/store"
)

const fixtureEquipment = "123456789012345"

func fixtureCandidate() modem.Candidate {
	return modem.Candidate{ID: "fixture-modem", USBGeneration: "1:7", NetworkInterface: "fixture0"}
}
func fixtureJournal(t *testing.T, directory string) Journal {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(directory, "claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return Journal{Database: db, lock: func(key string) (io.Closer, error) {
		fd, err := unix.Open(filepath.Join(directory, key+".lock"), unix.O_CREAT|unix.O_CLOEXEC|unix.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		return os.NewFile(uintptr(fd), key), nil
	}}
}
func acquireFixture(j Journal) (device.DataClaim, error) {
	return j.Acquire(context.Background(), fixtureCandidate(), fixtureEquipment, "1234567890123456789", "123456789012345")
}

func TestJournalOriginalOwnerReleaseAndIndependentEquipment(t *testing.T) {
	j := fixtureJournal(t, t.TempDir())
	ctx := context.Background()
	claim, err := acquireFixture(j)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := j.Pending(ctx, fixtureCandidate()); err != nil || !pending {
		t.Fatal("active record not fenced", err)
	}
	if _, err := acquireFixture(j); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("duplicate acquired", err)
	}
	if pending, err := j.Pending(ctx, modem.Candidate{ID: "other"}); err != nil || pending {
		t.Fatal("another physical modem fenced", err)
	}
	if _, err := (hostims.SQLiteClaimJournal{Database: j.Database}).Read(ctx, digest("imei:"+fixtureEquipment)); !errors.Is(err, hostims.ErrConflict) {
		t.Fatal("IMS admitted ordinary data owner", err)
	}
	if err := claim.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := claim.Release(ctx); err != nil {
		t.Fatal("release is not idempotent", err)
	}
	if pending, err := j.Pending(ctx, fixtureCandidate()); err != nil || pending {
		t.Fatal("released record remained", err)
	}
	newClaim, err := acquireFixture(j)
	if err != nil {
		t.Fatal("successor refused", err)
	}
	defer newClaim.Release(ctx)
	if err := claim.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, _ := j.Pending(ctx, fixtureCandidate()); !pending {
		t.Fatal("old release cleared successor")
	}
}

func TestJournalProcessExitDoesNotReleaseOwnership(t *testing.T) {
	if directory := os.Getenv("VOCAT_DATA_CLAIM_CHILD"); directory != "" {
		j := fixtureJournal(t, directory)
		if _, err := acquireFixture(j); err != nil {
			t.Fatal(err)
		}
		// Exit without Release: the OS releases flock, not the durable fence.
		os.Exit(0)
	}
	directory := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestJournalProcessExitDoesNotReleaseOwnership$")
	child.Env = append(os.Environ(), "VOCAT_DATA_CLAIM_CHILD="+directory)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v: %s", err, output)
	}
	j := fixtureJournal(t, directory)
	if pending, err := j.Pending(context.Background(), fixtureCandidate()); err != nil || !pending {
		t.Fatal("process death removed ownership", err)
	}
	if _, err := acquireFixture(j); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("new process adopted an orphan", err)
	}
}

func TestJournalRejectsHostClaimAndMalformedPrivateRecord(t *testing.T) {
	j := fixtureJournal(t, t.TempDir())
	ctx := context.Background()
	equipmentKey := digest("imei:" + fixtureEquipment)
	hostKey := "host.ims.claim.private.v1." + equipmentKey
	hostRaw := json.RawMessage(`{"pending":"unknown"}`)
	if ok, err := j.Database.CompareHostIMSClaim(ctx, hostKey, nil, hostRaw); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := acquireFixture(j); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("acquired conflicting IMS owner", err)
	}
	if ok, err := j.Database.CompareHostIMSClaim(ctx, hostKey, hostRaw, nil); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := j.Database.CompareDataBearerClaim(ctx, store.DataBearerClaimPrefix+equipmentKey, nil, json.RawMessage(`{}`)); err != nil || !ok {
		t.Fatal(err)
	}
	if pending, err := j.Pending(ctx, modem.Candidate{ID: "other"}); err != nil || !pending {
		t.Fatal("unknown record treated as unrelated", err)
	}
	if _, err := acquireFixture(j); !errors.Is(err, device.ErrDataCleanupPending) {
		t.Fatal("malformed record overwritten", err)
	}
}
