//go:build linux

package hostims

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityReservationPersistsAcrossProcessLifetimeAndRequiresOwnerToken(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "scopes")
	namespace, token := hashText("namespace"), hashText("owner")
	open := func(recover bool, owner string) (*securityReservations, error) {
		return openSecurityReservations(directory, namespace, owner, 0x60000001, 0x80000001, 0x80000002, recover)
	}
	first, err := open(false, token)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := open(false, hashText("other")); second != nil || !errors.Is(err, ErrBusy) {
		t.Fatal("live scope ID reused", err)
	}
	if second, err := open(true, token); second != nil || !errors.Is(err, ErrBusy) {
		t.Fatal("recovery stole live scope lock", err)
	}
	first.closeFDs() // process teardown does NOT release persistent reservation
	if second, err := open(false, hashText("other")); second != nil || !errors.Is(err, ErrBusy) {
		t.Fatal("crashed scope ID recycled", err)
	}
	if second, err := open(true, hashText("other")); second != nil || !errors.Is(err, ErrJournal) {
		t.Fatal("wrong owner recovered scope", err)
	}
	owned, err := open(true, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.release(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("confirmed scope reservation not removed", err)
	}
}

func TestSecurityReservationRejectsSymlinksAndUnsafeDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "scopes")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	namespace, token := hashText("namespace"), hashText("owner")
	name := hashText(namespace+":mark:1610612737") + ".scope"
	target := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(target, []byte("foreign untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
	if r, err := openSecurityReservations(directory, namespace, token, 0x60000001, 0x80000001, 0x80000002, true); r != nil || err == nil {
		t.Fatal("symlink scope accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "foreign untouched" {
		t.Fatal("foreign target modified")
	}
	unsafe := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(unsafe, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0777); err != nil {
		t.Fatal(err)
	}
	if r, err := openSecurityReservations(unsafe, namespace, token, 0x60000001, 0x80000001, 0x80000002, false); r != nil || !errors.Is(err, ErrJournal) {
		t.Fatal("untrusted reservation directory accepted", err)
	}
}
