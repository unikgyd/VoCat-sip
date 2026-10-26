package device

import (
	"context"
	"errors"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

type sharedSIMTestSession struct {
	nativeQMIVoWiFiSession
	identityErr, channelErr, closeErr error
	closed, opened, channelClosed     int
}

func (s *sharedSIMTestSession) GetICCID(context.Context) (string, error) {
	return "8944100000000000001", s.identityErr
}
func (*sharedSIMTestSession) GetIMSI(context.Context) (string, error) { return "234150000000001", nil }
func (*sharedSIMTestSession) GetIMEI(context.Context) (string, error) { return "867530912345678", nil }
func (*sharedSIMTestSession) GetNativeMCCMNC(context.Context) (string, string, error) {
	return "234", "15", nil
}
func (*sharedSIMTestSession) GetUSIMAID(context.Context) ([]byte, error) {
	return []byte{0xa0, 0, 0, 0, 0x87, 0x10, 2}, nil
}
func (*sharedSIMTestSession) GetISIMAID(context.Context) ([]byte, error) {
	return nil, errors.New("no ISIM")
}
func (s *sharedSIMTestSession) OpenLogicalChannel(context.Context, uint8, []byte) (byte, error) {
	s.opened++
	return 7, s.channelErr
}
func (*sharedSIMTestSession) SendAPDU(context.Context, uint8, uint8, []byte) ([]byte, error) {
	return []byte{0x90, 0}, nil
}
func (s *sharedSIMTestSession) CloseLogicalChannel(context.Context, uint8, uint8) error {
	s.channelClosed++
	return nil
}
func (s *sharedSIMTestSession) Close() error { s.closed++; return s.closeErr }

// Embedded unimplemented methods panic if a coexist operation tries RF,
// attachment, SIM power, card recovery or UIM reset.

func sharedSIMTestManager(t *testing.T, session *sharedSIMTestSession) (*Manager, *staticOpener, string) {
	t.Helper()
	m, at, id := newStartedNativeQMITestManager(t)
	m.qmiRadioOpener = func(context.Context, string) (qmiRadioSession, error) {
		t.Fatal("coexist used the legacy Sync/recovery transport")
		return nil, nil
	}
	m.qmiSharedSIMOpener = func(context.Context, string) (qmiRadioSession, error) { return session, nil }
	return m, at, id
}

func TestNativeQMICoexistReadsAndAKAUseOnlySharedSIMTransport(t *testing.T) {
	s := &sharedSIMTestSession{}
	m, at, id := sharedSIMTestManager(t, s)
	iccid, imsi, _, mcc, mnc, err := m.ReadNativeQMIIdentityCoexist(context.Background(), id)
	if err != nil || iccid != "8944100000000000001" || imsi != "234150000000001" || mcc != "234" || mnc != "15" {
		t.Fatalf("identity: %s %s %s %s %v", iccid, imsi, mcc, mnc, err)
	}
	aid, app, err := m.ProbeNativeQMIApplicationCoexist(context.Background(), id, "")
	if err != nil || len(aid) == 0 || app != "USIM" {
		t.Fatalf("application: %s %v", app, err)
	}
	if _, err := m.AuthenticateNativeQMICoexist(context.Background(), id, aid, []byte{0, 0x88}); err != nil {
		t.Fatal(err)
	}
	if s.closed != 3 || s.opened != 1 || s.channelClosed != 1 || at.openCount != 0 {
		t.Fatalf("unexpected ownership: %+v AT=%d", s, at.openCount)
	}
}

func TestNativeQMICoexistFailureNeverRecoversRFOrCyclesSIM(t *testing.T) {
	for _, mode := range []string{"identity-unavailable", "channel-resources", "close-failed"} {
		t.Run(mode, func(t *testing.T) {
			s := &sharedSIMTestSession{}
			m, at, id := sharedSIMTestManager(t, s)
			switch mode {
			case "identity-unavailable":
				s.identityErr = errors.New("no ATR / SIM unavailable")
				iccid, imsi, imei, mcc, mnc, err := m.ReadNativeQMIIdentityCoexist(context.Background(), id)
				if err == nil || iccid+imsi+imei+mcc+mnc != "" {
					t.Fatal("failed identity was published or recovered")
				}
			case "channel-resources":
				s.channelErr = &qmi.QMIError{Service: qmi.ServiceUIM, ErrorCode: 0x0044}
				if _, err := m.AuthenticateNativeQMICoexist(context.Background(), id, []byte{0xa0}, []byte{0, 0x88}); err == nil {
					t.Fatal("channel resource failure was hidden")
				}
				if s.opened != 1 || s.channelClosed != 0 {
					t.Fatal("channel was retried or unowned channel closed")
				}
			case "close-failed":
				s.closeErr = errors.New("owned CID cleanup failed")
				iccid, imsi, _, _, _, err := m.ReadNativeQMIIdentityCoexist(context.Background(), id)
				if !errors.Is(err, s.closeErr) || iccid != "" || imsi != "" {
					t.Fatal("cleanup failure was discarded")
				}
			}
			if s.closed != 1 || at.openCount != 0 {
				t.Fatalf("legacy recovery or leaked session: close=%d AT=%d", s.closed, at.openCount)
			}
		})
	}
}
