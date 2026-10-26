package modem

import (
	"context"
	"errors"
	"testing"
)

func TestUSBCallGuardRejectsKnownEndWithoutWritingAnotherCall(t *testing.T) {
	transport := &transcriptTransport{}
	session := newTestSession(t, transport)
	for _, line := range []string{"NO CARRIER", "BUSY", "NO ANSWER", "RDY", "CALL READY"} {
		before := session.CallEndRevision()
		if err := session.lock(context.Background()); err != nil {
			t.Fatal(err)
		}
		session.consumeLineLocked(line, nil)
		session.unlock()
		if session.CallEndRevision() != before+1 {
			t.Fatalf("lost lifecycle indication %s", line)
		}
		if _, err := session.ExecuteForCall(context.Background(), before, "AT+CHLD=11"); !errors.Is(err, ErrCallLifecycleChanged) {
			t.Fatalf("stale release %s: %v", line, err)
		}
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, op := range transport.ops {
		if len(op) >= 6 && op[:6] == "write:" {
			t.Fatal("stale call command reached serial transport", op)
		}
	}
}

func TestUSBCallGuardAllowsCurrentRevisionAndPreservesURCs(t *testing.T) {
	transport := &transcriptTransport{steps: []transportStep{{write: "AT+CLCC\r", chunks: []string{"\r\nOK\r\n"}}}}
	session := newTestSession(t, transport)
	if err := session.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	session.consumeLineLocked("RING", nil)
	session.unlock()
	if session.CallEndRevision() != 0 {
		t.Fatal("repeat ringing invalidated the same incoming call")
	}
	if _, err := session.ExecuteForCall(context.Background(), 0, "AT+CLCC"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.WaitURC(context.Background(), func(line string) bool { return line == "RING" }); err != nil {
		t.Fatal(err)
	}
}

func TestUSBCallGuardDrainsAlreadyBufferedTerminalBeforeWireSubmission(t *testing.T) {
	transport := &transcriptTransport{}
	session := newTestSession(t, transport)
	before := session.CallEndRevision()
	if err := session.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	session.readBuf = append(session.readBuf, []byte("NO CARRIER\r\n")...)
	session.unlock()
	if _, err := session.ExecuteForCall(context.Background(), before, "ATA"); !errors.Is(err, ErrCallLifecycleChanged) {
		t.Fatalf("buffered end did not veto ATA: %v", err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.unexpected != nil {
		t.Fatal("stale ATA was written", transport.unexpected)
	}
}
