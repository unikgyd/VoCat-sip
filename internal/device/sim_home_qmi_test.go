package device

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/modem"
)

type qmiHomeTranscript struct {
	commands          [][]byte
	replies           [][]byte
	sendErr, closeErr error
	closed            int
	cancel            context.CancelFunc
	cleanup           context.Context
}

func (*qmiHomeTranscript) GetIMSI(context.Context) (string, error) { return "999123123456789", nil }
func (*qmiHomeTranscript) GetUSIMAID(context.Context) ([]byte, error) {
	return []byte{0xA0, 0, 0, 0, 0x87, 0x10, 2}, nil
}
func (*qmiHomeTranscript) OpenLogicalChannel(context.Context, uint8, []byte) (byte, error) {
	return 3, nil
}
func (s *qmiHomeTranscript) CloseLogicalChannel(ctx context.Context, slot, ch uint8) error {
	s.closed++
	s.cleanup = ctx
	if ctx.Err() != nil || slot != 1 || ch != 3 {
		return errors.New("invalid cleanup scope")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
		return errors.New("unbounded cleanup")
	}
	return s.closeErr
}
func (s *qmiHomeTranscript) SendAPDU(ctx context.Context, slot, ch uint8, cmd []byte) ([]byte, error) {
	if slot != 1 || ch != 3 {
		return nil, errors.New("wrong UICC scope")
	}
	s.commands = append(s.commands, append([]byte(nil), cmd...))
	if s.cancel != nil {
		s.cancel()
		return nil, ctx.Err()
	}
	if s.sendErr != nil {
		return nil, s.sendErr
	}
	if bytes.Equal(cmd, []byte{0, 0xA4, 0, 0x0C, 2, 0x6F, 7}) {
		return []byte{0x90, 0}, nil
	}
	if bytes.Equal(cmd, []byte{0, 0xB0, 0, 0, 9}) {
		return []byte{8, 0x99, 0x99, 0x21, 0x13, 0x32, 0x54, 0x76, 0x98, 0x90, 0}, nil
	}
	if len(s.replies) == 0 {
		return nil, errors.New("unexpected APDU")
	}
	raw := s.replies[0]
	s.replies = s.replies[1:]
	return raw, nil
}

func TestReaderHomeQMIReadsFourthEFADOctetWithoutOPLOrGuess(t *testing.T) {
	for _, v := range []struct {
		name    string
		data    []byte
		want    string
		unknown bool
	}{
		{"two", []byte{0, 0, 0, 2}, "12", false}, {"three", []byte{0, 0, 0, 3}, "123", false},
		{"extension", []byte{0, 0, 0, 2, 3}, "12", false}, {"rfu", []byte{0, 0, 0, 0xF3}, "123", false},
		{"short", []byte{0, 0, 2}, "", true}, {"unknown", []byte{0, 0, 0, 0xFF}, "", true},
	} {
		t.Run(v.name, func(t *testing.T) {
			s := &qmiHomeTranscript{replies: [][]byte{{0x90, 0}, append(append([]byte(nil), v.data...), 0x90, 0)}}
			mcc, mnc, err := readQMIHomePLMN(context.Background(), s)
			if mcc != "999" || mnc != v.want || (err != nil) != v.unknown || v.unknown && !modem.OnlyHomeMNCLengthUnavailable(err) {
				t.Fatalf("home=%s-%s err=%v", mcc, mnc, err)
			}
			if s.closed != 1 || len(s.replies) != 0 || len(s.commands) != 4 || !bytes.Equal(s.commands[2], []byte{0, 0xA4, 0, 0x0C, 2, 0x6F, 0xAD}) || !bytes.Equal(s.commands[3], []byte{0, 0xB0, 0, 0, 4}) {
				t.Fatal("read was not scoped to EF_AD and closed")
			}
		})
	}
}

func TestReaderHomeQMIMissingFileIsOptionalButPINAndMalformedAreFatal(t *testing.T) {
	for _, v := range []struct {
		name     string
		replies  [][]byte
		optional bool
	}{
		{"missing", [][]byte{{0x6A, 0x82}}, true}, {"PIN", [][]byte{{0x69, 0x82}}, false},
		{"missing-status", [][]byte{{0x90}}, false}, {"read-error", [][]byte{{0x90, 0}, {0x6A, 0x86}}, false},
		{"short-eof", [][]byte{{0x90, 0}, {0, 0, 2, 0x62, 0x82}}, true},
	} {
		t.Run(v.name, func(t *testing.T) {
			s := &qmiHomeTranscript{replies: v.replies}
			_, _, err := readQMIHomePLMN(context.Background(), s)
			if err == nil || modem.OnlyHomeMNCLengthUnavailable(err) != v.optional || s.closed != 1 {
				t.Fatalf("classification=%v close=%d", err, s.closed)
			}
		})
	}
}

func TestReaderHomeQMICleanupSurvivesCancellationAndNeverBecomesOptional(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &qmiHomeTranscript{cancel: cancel}
	_, _, err := readQMIHomePLMN(ctx, s)
	if !errors.Is(err, context.Canceled) || s.closed != 1 || s.cleanup == nil || modem.OnlyHomeMNCLengthUnavailable(err) {
		t.Fatalf("cancellation=%v cleanup=%d", err, s.closed)
	}
	s = &qmiHomeTranscript{replies: [][]byte{{0x6A, 0x82}}, closeErr: errors.New("close failed")}
	_, _, err = readQMIHomePLMN(context.Background(), s)
	if !errors.Is(err, modem.ErrHomeMNCLengthUnavailable) || modem.OnlyHomeMNCLengthUnavailable(err) {
		t.Fatal("cleanup error was swallowed as optional")
	}
}

func TestReaderHomeQMIContinuationIsReadOnlyAndBounded(t *testing.T) {
	s := &qmiHomeTranscript{replies: [][]byte{{0x61, 2}, {0x90, 0}, {0x6C, 4}, {0, 0, 0, 3, 0x90, 0}}}
	_, mnc, err := readQMIHomePLMN(context.Background(), s)
	if err != nil || mnc != "123" || s.commands[3][1] != 0xC0 {
		t.Fatalf("continuation failed: %v", err)
	}
	s = &qmiHomeTranscript{replies: [][]byte{{0x61, 2}, {0x61, 2}, {0x61, 2}, {0x61, 2}}}
	_, _, err = readQMIHomePLMN(context.Background(), s)
	if err == nil || len(s.commands) != 6 || s.closed != 1 {
		t.Fatal("continuation was not bounded")
	}
	s = &qmiHomeTranscript{replies: [][]byte{{0x90, 0}, {0, 0, 0x61, 2}, {0, 3, 0x90, 0}}}
	_, mnc, err = readQMIHomePLMN(context.Background(), s)
	if err != nil || mnc != "123" {
		t.Fatal("GET RESPONSE lost partial EF_AD data", err)
	}
}

type qmiHomeWrongApplication struct{ qmiHomeTranscript }

func (s *qmiHomeWrongApplication) SendAPDU(ctx context.Context, slot, ch uint8, cmd []byte) ([]byte, error) {
	raw, err := s.qmiHomeTranscript.SendAPDU(ctx, slot, ch, cmd)
	if bytes.Equal(cmd, []byte{0, 0xB0, 0, 0, 9}) && len(raw) > 8 {
		raw[8] = 0x99
	}
	return raw, err
}
func TestReaderHomeQMIWrongApplicationAndPreCanceledReadsNeverReachEFAD(t *testing.T) {
	s := &qmiHomeWrongApplication{}
	_, _, err := readQMIHomePLMN(context.Background(), s)
	if err == nil || modem.OnlyHomeMNCLengthUnavailable(err) || len(s.commands) != 2 || s.closed != 1 {
		t.Fatal("wrong USIM application was used for EF_AD")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base := &qmiHomeTranscript{}
	if _, _, err = readQMIHomePLMN(ctx, base); !errors.Is(err, context.Canceled) || len(base.commands) != 0 || base.closed != 0 {
		t.Fatal("pre-canceled query touched UICC")
	}
}

type readerHomeFencedSession struct {
	readerHomeLegacySession
	homeErr error
	known   bool
	changed bool
	reads   int
}

func (s *readerHomeFencedSession) GetNativeMCCMNC(context.Context) (string, string, error) {
	if s.known {
		return "999", "123", nil
	}
	return "999", "", s.homeErr
}
func (s *readerHomeFencedSession) GetICCID(context.Context) (string, error) {
	s.reads++
	if s.changed && s.reads > 1 {
		return "8901000000000000002", nil
	}
	return "8901000000000000001", nil
}

func TestReaderHomeManagerOnlyReturnsPartialUnknownAfterLiveFence(t *testing.T) {
	for _, v := range []struct {
		name           string
		known, changed bool
		err            error
	}{
		{"known", true, false, nil}, {"unknown", false, false, modem.ErrHomeMNCLengthUnavailable},
		{"changed", false, true, modem.ErrHomeMNCLengthUnavailable}, {"cleanup", false, false, errors.Join(modem.ErrHomeMNCLengthUnavailable, errors.New("cleanup"))},
	} {
		t.Run(v.name, func(t *testing.T) {
			m, _, id := newStartedNativeQMITestManager(t)
			s := &readerHomeFencedSession{known: v.known, changed: v.changed, homeErr: v.err}
			m.qmiRadioOpener = func(context.Context, string) (qmiRadioSession, error) { return s, nil }
			iccid, imsi, _, _, mnc, err := m.ReadNativeQMIIdentity(context.Background(), id)
			if v.known {
				if err != nil || mnc != "123" {
					t.Fatalf("known=%v", err)
				}
			} else if v.name == "unknown" {
				if !modem.OnlyHomeMNCLengthUnavailable(err) || iccid == "" || imsi == "" {
					t.Fatal("verified partial unknown was lost")
				}
			} else {
				if err == nil || modem.OnlyHomeMNCLengthUnavailable(err) || iccid != "" || imsi != "" {
					t.Fatal("fatal error exposed partial identity")
				}
			}
			if s.closed != 1 {
				t.Fatal("lease leaked")
			}
		})
	}
}
