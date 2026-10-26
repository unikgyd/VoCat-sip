package device

import (
	"context"
	"testing"
)

type readerHomeLegacySession struct {
	nativeQMIVoWiFiSession
	closed int
}

func (*readerHomeLegacySession) GetICCID(context.Context) (string, error) {
	return "8901000000000000001", nil
}
func (*readerHomeLegacySession) GetIMSI(context.Context) (string, error) {
	return "999123123456789", nil
}
func (*readerHomeLegacySession) GetIMEI(context.Context) (string, error) {
	return "867530912345678", nil
}
func (*readerHomeLegacySession) GetNativeMCCMNC(context.Context) (string, string, error) {
	return "204", "04", nil
}
func (s *readerHomeLegacySession) Close() error { s.closed++; return nil }

func TestReaderHomeManagerNeverAcceptsForeignOPLAsAuthenticationHome(t *testing.T) {
	m, _, id := newStartedNativeQMITestManager(t)
	s := &readerHomeLegacySession{}
	m.qmiRadioOpener = func(context.Context, string) (qmiRadioSession, error) { return s, nil }
	_, _, _, mcc, mnc, err := m.ReadNativeQMIIdentity(context.Background(), id)
	if err == nil {
		t.Fatalf("foreign OPL accepted as SIM home: %s-%s", mcc, mnc)
	}
	if s.closed != 1 {
		t.Fatal("identity lease not released")
	}
}
