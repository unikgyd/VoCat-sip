package vowifi

import (
	"context"
	"errors"
	"testing"
)

type boundTestIMSSession struct {
	IMSSession
	sent int
}

func (s *boundTestIMSSession) SendSMS(context.Context, SMSSubmitRequest) (SMSSubmitResult, error) {
	s.sent++
	return SMSSubmitResult{AllPartsAccepted: true}, nil
}
func TestSMSSubscriptionGuardRunsUnderLifecycleLock(t *testing.T) {
	session := &boundTestIMSSession{}
	o := &Orchestrator{operation: make(chan struct{}, 1), state: State{Enabled: true, SIMReady: true, IMSReady: true, SMSReady: true, IMSI: "001010000000001", ICCID: "sim-a"}, resources: &runtimeResources{ims: session}}
	o.operation <- struct{}{}
	for _, identity := range [][2]string{{"", ""}, {"001010000000001", "sim-b"}, {"other-imsi", "sim-a"}} {
		if _, err := o.SendSMSForSubscription(context.Background(), identity[0], identity[1], SMSSubmitRequest{}); !errors.Is(err, ErrSMSNotReady) {
			t.Fatal("wrong SIM accepted", err)
		}
	}
	if session.sent != 0 {
		t.Fatal("identity check after submission")
	}
	if _, err := o.SendSMSForSubscription(context.Background(), "001010000000001", "sim-a", SMSSubmitRequest{}); err != nil || session.sent != 1 {
		t.Fatal(err)
	}
}
