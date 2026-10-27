package hostims

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func smsCenterRecord() []byte {
	record := bytes.Repeat([]byte{0xff}, 40)
	record[12] = 0xfd
	copy(record[25:], []byte{4, 0x91, 0x21, 0x43, 0xf5})
	return record
}

func TestCellularSMSCenterRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
		want   string
	}{
		{"international", func(b []byte) []byte { return b }, "+12345"},
		{"no_alpha", func(b []byte) []byte { return b[12:] }, "+12345"},
		{"national", func(b []byte) []byte { b[26] = 0xa1; return b }, "12345"},
		{"unknown_ton", func(b []byte) []byte { b[26] = 0x81; return b }, "12345"},
		{"even", func(b []byte) []byte { b[29] = 0x65; return b }, "+123456"},
		{"short", func(b []byte) []byte { return b[:27] }, ""},
		{"oversize", func(b []byte) []byte { return bytes.Repeat([]byte{0}, 256) }, ""},
		{"absent_indicator", func(b []byte) []byte { b[12] = 0xff; return b }, ""},
		{"empty_address", func(b []byte) []byte { b[25] = 1; return b }, ""},
		{"oversize_address", func(b []byte) []byte { b[25] = 12; return b }, ""},
		{"bad_extension", func(b []byte) []byte { b[26] = 0x11; return b }, ""},
		{"bad_plan", func(b []byte) []byte { b[26] = 0x99; return b }, ""},
		{"alphanumeric", func(b []byte) []byte { b[26] = 0xd1; return b }, ""},
		{"bad_low_digit", func(b []byte) []byte { b[27] = 0x2a; return b }, ""},
		{"bad_high_digit", func(b []byte) []byte { b[27] = 0xa1; return b }, ""},
		{"early_filler", func(b []byte) []byte { b[27] = 0xf1; return b }, ""},
		{"all_filler", func(b []byte) []byte { b[27] = 0xff; return b }, ""},
		{"too_few_digits", func(b []byte) []byte { b[25] = 2; return b }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.change(smsCenterRecord())
			before := append([]byte(nil), raw...)
			if got := decodeCardSMSCenter(raw); got != tc.want || !bytes.Equal(raw, before) {
				t.Fatal("wrong service-centre decode or mutated UICC record")
			}
		})
	}
}

func TestCellularSMSCenterProvisioningAndUncertainRead(t *testing.T) {
	for _, mode := range []string{"present", "absent", "invalid", "transport", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			p, f, _, journal := simFixture(t)
			f.smsp = smsCenterRecord()
			switch mode {
			case "absent":
				f.smsp = nil
			case "invalid":
				f.smsp[12] = 0xff
			case "transport", "uncertain":
				f.smspErr = ErrTransport
				f.smspUncertain = mode == "uncertain"
			}
			card, err := p.ReadCard(context.Background())
			if mode == "uncertain" {
				if !errors.Is(err, ErrUncertain) || !journal.record(p.lease.selection.key).SIMUncertain {
					t.Fatal("uncertain optional read lost its cleanup fence", err)
				}
				return
			}
			if mode == "transport" {
				if !errors.Is(err, ErrTransport) {
					t.Fatal("transport failure was treated as missing provisioning", err)
				}
				return
			}
			want := ""
			if mode == "present" {
				want = "+12345"
			}
			if err != nil || card.Subscriber().SMSC != want || card.Application() != "ISIM" || f.opened != f.closed || f.auths != 0 {
				t.Fatal("optional USIM SMSP changed IMS provisioning or ownership", err)
			}
		})
	}
}

func TestPreparedAccessSMSCenterUsesOriginalLiveCard(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	if got, err := a.ReadSMSCenter(context.Background(), "line"); got != "" || !errors.Is(err, ErrSIMSMSCenter) || !a.Evidence().Established {
		t.Fatal("missing SMSP stopped the voice access or invented an address", err)
	}
	f.uim.mu.Lock()
	f.uim.smsp = smsCenterRecord()
	f.uim.mu.Unlock()
	if got, err := a.ReadSMSCenter(context.Background(), "line"); err != nil || got != "+12345" {
		t.Fatal("reader reused an empty provisioning cache", err)
	}
	f.uim.mu.Lock()
	f.uim.smsp[27], f.uim.smsp[28], f.uim.smsp[29] = 0x45, 0x23, 0xf1
	f.uim.mu.Unlock()
	if got, err := a.ReadSMSCenter(context.Background(), "unrelated-name"); err != nil || got != "+54321" {
		t.Fatal("name changed the original access owner or provisioning was cached", err)
	}
}

func TestPreparedAccessSMSCenterRejectsChangedProvisioning(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	f.uim.mu.Lock()
	f.uim.smsp = smsCenterRecord()
	f.uim.impu = "sip:+15555550101@ims.example.net"
	f.uim.mu.Unlock()
	if got, err := a.ReadSMSCenter(context.Background(), "line"); err == nil || got != "" || a.Evidence().Established {
		t.Fatal("service-centre address escaped a changed original identity", err)
	}
}

func TestPreparedAccessSMSCenterCancellationDoesNotOpenUIM(t *testing.T) {
	f := accessFixture(t)
	a := f.prepare(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.uim.mu.Lock()
	opened := f.uim.opened
	f.uim.mu.Unlock()
	if got, err := a.ReadSMSCenter(ctx, "line"); !errors.Is(err, context.Canceled) || got != "" {
		t.Fatal("canceled read produced provisioning", err)
	}
	f.uim.mu.Lock()
	defer f.uim.mu.Unlock()
	if f.uim.opened != opened {
		t.Fatal("canceled read opened a UIM transaction")
	}
}
