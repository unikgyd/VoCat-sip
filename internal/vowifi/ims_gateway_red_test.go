package vowifi

import (
	"context"
	"errors"
	"testing"
)

type gatewayErrorIMS struct{ environment *fakeEnvironment }

func (p gatewayErrorIMS) Start(context.Context, IMSRequest) (IMSSession, error) {
	return &fakeIMSSession{p.environment}, errors.New("synthetic partial IMS setup failure")
}

func TestIMSGatewayPartialSessionErrorIsClosed(t *testing.T) {
	e := newFakeEnvironment()
	d := discoveryDependencies(e)
	d.IMS = gatewayErrorIMS{e}
	o, err := New(d, Options{DeviceID: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	if _, err := o.Enable(context.Background()); err == nil {
		t.Fatal("partial setup accepted")
	}
	if e.callCount("ims.close") != 1 || e.callCount("tunnel.close") != 1 {
		t.Fatalf("partial IMS resource leaked: %v", e.callsSnapshot())
	}
}
