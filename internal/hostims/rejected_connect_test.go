package hostims

import (
	"context"
	"errors"
	"testing"
)

func TestBearerDefinitiveConnectRejectionCleansOnlyOwnCreatedObject(t *testing.T) {
	p, backend, journal, selection := bearerFixture(t)
	backend.failOp, backend.failError = "connect", ErrRejected
	if _, err := p.Acquire(context.Background(), selection, BearerOptions{}); !errors.Is(err, ErrRejected) {
		t.Fatal("modem rejection was not retained", err)
	}
	if backend.count("create") != 1 || backend.count("connect") != 1 || backend.count("delete") != 1 || backend.count("disconnect") != 0 {
		t.Fatal("negative reply did not clean exactly its own disconnected bearer", backend.calls)
	}
	if raw, _ := journal.Read(context.Background(), selection.key); raw != nil {
		t.Fatal("acknowledged cleanup kept an uncertain claim")
	}
	backend.failOp = ""
	lease, err := p.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal("clean rejection prevented a later explicitly requested connection", err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
