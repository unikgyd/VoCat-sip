package vowifi

import (
	"context"
	"errors"
	"testing"
)

func TestReaderHomeProvisioningDoesNotReceiveTentativeAuthenticationHome(t *testing.T) {
	e := newFakeEnvironment()
	e.identity = uncertainMNCFixture(t)
	o, err := New(discoveryDependencies(e), Options{DeviceID: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = o.WithSIM(context.Background(), func(SIMIdentity, AKAProvider) error { called = true; return nil })
	if !errors.Is(err, ErrEC20MNCUnavailable) || called {
		t.Fatalf("tentative home escaped to non-planner provisioning: called=%v err=%v", called, err)
	}
}
