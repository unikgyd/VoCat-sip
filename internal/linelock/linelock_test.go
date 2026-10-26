package linelock

import "testing"

func TestForKeepsSharedPerDeviceIdentity(t *testing.T) {
	first, again, other := For("line-a"), For("line-a"), For("line-b")
	if first != again || first == other {
		t.Fatal("line lock identity is not stable/independent")
	}
	first.RLock()
	if again.TryLock() {
		again.Unlock()
		t.Fatal("writer entered active legacy read")
	}
	if !other.TryLock() {
		t.Fatal("one line blocked another line")
	}
	other.Unlock()
	first.RUnlock()
	if !again.TryLock() {
		t.Fatal("completed legacy read kept line fenced")
	}
	again.Unlock()
}
