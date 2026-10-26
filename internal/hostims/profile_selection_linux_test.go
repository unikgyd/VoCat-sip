//go:build linux

package hostims

import (
	"context"
	"errors"
	"testing"
)

func TestMMExplicitIMSProfileIsBoundThroughCreateVerifyAndCleanup(t *testing.T) {
	pool, backend, _, selection, _ := mmWireFixture(t)
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{ProfileID: 5})
	if err != nil {
		t.Fatal(err)
	}
	path := lease.claim.record.Path
	backend.mu.Lock()
	row := backend.bearers[path]
	backend.mu.Unlock()
	if row.Properties.ProfileID != 5 {
		t.Fatal("explicit IMS profile was not sent through the actual D-Bus backend")
	}
	changed := row
	changed.Properties.ProfileID = 6
	backend.mu.Lock()
	backend.bearers[path] = changed
	backend.mu.Unlock()
	if err := lease.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatal("replacement profile was accepted", err)
	}
	if err := lease.Close(context.Background()); err == nil {
		t.Fatal("changed profile was deleted")
	}
	backend.mu.Lock()
	backend.bearers[path] = row
	backend.mu.Unlock()
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal("original profile could not be cleaned", err)
	}
}
