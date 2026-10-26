package vowifi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"vocat/internal/modem"
	"vocat/internal/pcsc"
)

type readerHomeNativeController struct {
	NativeQMIController
	readErr      error
	imsi         string
	known        bool
	reads, auths int
}

func (c *readerHomeNativeController) ReadNativeQMIIdentity(context.Context, string) (string, string, string, string, string, error) {
	c.reads++
	imsi := c.imsi
	if imsi == "" {
		imsi = "999123123456789"
	}
	if c.known {
		return "8901000000000000001", imsi, "867530912345678", "999", "123", nil
	}
	return "8901000000000000001", imsi, "867530912345678", "999", "", c.readErr
}
func (*readerHomeNativeController) ProbeNativeQMIApplication(context.Context, string, string) ([]byte, string, error) {
	return []byte{0xA0, 0, 0, 0, 0x87, 0x10, 2}, "USIM", nil
}
func (c *readerHomeNativeController) AuthenticateNativeQMI(context.Context, string, []byte, []byte) ([]byte, error) {
	c.auths++
	return successfulUSIMResponse(), nil
}

func TestReaderHomeNativeQMIUnknownReachesPlannerAndCanAuthenticateLegalRealm(t *testing.T) {
	c := &readerHomeNativeController{readErr: modem.ErrHomeMNCLengthUnavailable}
	a, err := NewNativeQMIAdapter(c, nil, SIMReaderOptions{AllowUnresolvedMNCLength: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.ReadIdentity(context.Background(), "native")
	if err != nil || !id.HomeMNCUncertain {
		t.Fatalf("unknown evidence lost: %+v %v", id, err)
	}
	plans, err := BuildEPDGAttempts(id, EPDGDiscoveryOptions{})
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans=%v %v", plans, err)
	}
	if evidence, err := a.CheckReady(context.Background(), id); err != nil || !evidence.Ready {
		t.Fatalf("AKA readiness: %v", err)
	}
	selected := plans[1].Identity
	if selected.HomeMNC != "123" {
		t.Fatalf("three-digit candidate missing: %v", plans)
	}
	if _, err = a.Authenticate(context.Background(), selected, AKAChallenge{}); err != nil || c.auths != 1 {
		t.Fatalf("optional EF_AD blocked hardware AKA: %v", err)
	}
	c.known = true
	if _, err = a.Authenticate(context.Background(), plans[0].Identity, AKAChallenge{}); !errors.Is(err, ErrEC20IdentityChanged) || c.auths != 1 {
		t.Fatal("new explicit EF_AD did not fence an obsolete inferred realm")
	}
	c.imsi = "999123999999999"
	if _, err = a.Authenticate(context.Background(), selected, AKAChallenge{}); !errors.Is(err, ErrEC20IdentityChanged) || c.auths != 1 {
		t.Fatal("changed IMSI reached AUTHENTICATE")
	}
}

func TestReaderHomeNativeQMIStrictAndJoinedErrorsNeverPublishBindings(t *testing.T) {
	for _, v := range []struct {
		allow bool
		err   error
	}{
		{false, modem.ErrHomeMNCLengthUnavailable},
		{true, errors.Join(modem.ErrHomeMNCLengthUnavailable, context.Canceled)},
		{true, errors.Join(modem.ErrHomeMNCLengthUnavailable, errors.New("channel cleanup"))},
	} {
		c := &readerHomeNativeController{readErr: v.err}
		a, _ := NewNativeQMIAdapter(c, nil, SIMReaderOptions{AllowUnresolvedMNCLength: v.allow})
		if _, err := a.ReadIdentity(context.Background(), "native"); err == nil || len(a.bindings) != 0 {
			t.Fatal("strict/fatal read published identity")
		}
	}
}

func TestReaderHomeNativeQMIExplicitEFADDoesNotBecomeUncertain(t *testing.T) {
	c := &readerHomeNativeController{known: true}
	a, _ := NewNativeQMIAdapter(c, nil, SIMReaderOptions{AllowUnresolvedMNCLength: true})
	id, err := a.ReadIdentity(context.Background(), "native")
	if err != nil || id.HomeMNCUncertain || id.HomeMNC != "123" {
		t.Fatalf("explicit evidence lost: %+v %v", id, err)
	}
}

type readerHomeMutableCard struct {
	*readerHomeCard
	length                     byte
	imsiReads, changeAt, auths int
	cancel                     context.CancelFunc
	homeFaultOnSelect          bool
	homeErr                    error
	homeSW                     uint16
}

func (c *readerHomeMutableCard) Transmit(ctx context.Context, cmd []byte) ([]byte, uint16, error) {
	selectHome := len(cmd) >= 7 && cmd[1] == 0xA4 && cmd[2] == 0 && cmd[5] == 0x6F && cmd[6] == 0xAD
	readHome := len(cmd) >= 2 && cmd[1] == 0xB0 && c.file == 0x6FAD
	if (c.homeFaultOnSelect && selectHome) || (!c.homeFaultOnSelect && readHome) {
		if c.homeErr != nil || c.homeSW != 0 {
			return nil, c.homeSW, c.homeErr
		}
	}
	if len(cmd) >= 2 && cmd[1] == 0xB0 {
		if c.file == 0x6FAD {
			if c.cancel != nil {
				c.cancel()
			}
			return []byte{0, 0, 0, c.length}, 0x9000, nil
		}
		if c.file == 0x6F07 {
			c.imsiReads++
			if c.changeAt > 0 && c.imsiReads >= c.changeAt {
				return []byte{8, 0x99, 0x99, 0x21, 0x13, 0x32, 0x54, 0x76, 0x99}, 0x9000, nil
			}
		}
	}
	if len(cmd) >= 2 && cmd[1] == 0x88 {
		c.auths++
		raw := successfulUSIMResponse()
		return raw[:len(raw)-2], 0x9000, nil
	}
	return c.readerHomeCard.Transmit(ctx, cmd)
}

func readerHomePCSC(t *testing.T, c *readerHomeMutableCard) *PCSCAdapter {
	t.Helper()
	s := pcsc.NewWithBackend(&mockPCSCBackend{card: c})
	a, err := NewPCSCAdapter(s, func(context.Context, string) (pcsc.Selector, string, error) {
		return pcsc.Selector{USBPath: "1-1"}, "", nil
	}, SIMReaderOptions{AllowUnresolvedMNCLength: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestReaderHomePCSCUnknownAndKnownRemainDistinctThroughRealAPDUs(t *testing.T) {
	for _, length := range []byte{3, 0xFF} {
		c := &readerHomeMutableCard{readerHomeCard: &readerHomeCard{}, length: length}
		a := readerHomePCSC(t, c)
		id, err := a.ReadIdentity(context.Background(), "reader")
		if err != nil {
			t.Fatal(err)
		}
		if id.HomeMNCUncertain != (length == 0xFF) || c.imsiReads != 2 {
			t.Fatal("reader did not preserve/fence length evidence")
		}
		plans, err := BuildEPDGAttempts(id, EPDGDiscoveryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if length == 0xFF {
			want = 2
		}
		if len(plans) != want {
			t.Fatalf("plans=%v", plans)
		}
	}
}

func TestReaderHomePCSCProfileChangeAndCancellationNeverPublishBindings(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		c := &readerHomeMutableCard{readerHomeCard: &readerHomeCard{}, length: 0xFF, changeAt: 2}
		if cancelRead {
			c.changeAt = 0
			c.cancel = cancel
		}
		a := readerHomePCSC(t, c)
		_, err := a.ReadIdentity(ctx, "reader")
		cancel()
		if err == nil || len(a.bindings) != 0 || c.auths != 0 {
			t.Fatal("changed/canceled transaction published identity")
		}
		if cancelRead && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation hidden: %v", err)
		}
	}
}

func TestReaderHomePCSCStopsChangedSubscriberBeforeAKA(t *testing.T) {
	c := &readerHomeMutableCard{readerHomeCard: &readerHomeCard{}, length: 0xFF}
	a := readerHomePCSC(t, c)
	id, err := a.ReadIdentity(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	c.changeAt = c.imsiReads + 1
	if _, err = a.Authenticate(context.Background(), id, AKAChallenge{}); err == nil || c.auths != 0 {
		t.Fatal("profile switch reached AUTHENTICATE")
	}
}

func TestReaderHomePCSCChangedKnownEFADStopsBeforeAKA(t *testing.T) {
	c := &readerHomeMutableCard{readerHomeCard: &readerHomeCard{}, length: 3}
	a := readerHomePCSC(t, c)
	id, err := a.ReadIdentity(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	c.length = 2
	if _, err = a.Authenticate(context.Background(), id, AKAChallenge{}); err == nil || c.auths != 0 {
		t.Fatal("new explicit EF_AD did not fence an old known realm")
	}
}

func TestReaderHomePCSCFatalHomeReadsNeverBecomeOptionalLength(t *testing.T) {
	transport := errors.New("synthetic PCSC disconnect")
	for _, selectStage := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			err  error
			sw   uint16
		}{{"transport", transport, 0}, {"PIN denied", nil, 0x6982}, {"unexpected status", nil, 0x6F00}} {
			t.Run(fmt.Sprintf("select=%v/%s", selectStage, tc.name), func(t *testing.T) {
				c := &readerHomeMutableCard{readerHomeCard: &readerHomeCard{}, length: 0xFF, homeFaultOnSelect: selectStage, homeErr: tc.err, homeSW: tc.sw}
				a := readerHomePCSC(t, c)
				_, err := a.ReadIdentity(context.Background(), "reader")
				if err == nil || len(a.bindings) != 0 || c.auths != 0 {
					t.Fatalf("fatal home read became tentative identity: err=%v bindings=%d", err, len(a.bindings))
				}
				if tc.err != nil && !errors.Is(err, tc.err) {
					t.Fatalf("transport cause lost: %v", err)
				}
			})
		}
	}
}
