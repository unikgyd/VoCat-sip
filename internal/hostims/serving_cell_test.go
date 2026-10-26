package hostims

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"vocat/internal/vowifi"
)

func TestServingCellLocationPreservesServingPLMNAndLTEFields(t *testing.T) {
	for _, row := range []struct {
		raw, operator, mcc, mnc string
		tac                     uint16
		eci                     uint32
	}{
		{"310,26,0,1234567,ABCD", "31026", "310", "26", 0xabcd, 0x1234567},
		{"310,260,3456,FFFFFFF,FFFF", "310260", "310", "260", 0xffff, 0x0fffffff},
		{"001,01,1,1,2", "00101", "001", "01", 2, 1},
		{"001,001,1,2,1", "001001", "001", "001", 1, 2},
	} {
		cell, err := parseCachedLTELocation(row.raw, row.operator)
		if err != nil {
			t.Fatal("valid diagnostic location rejected", err)
		}
		mcc, mnc := cell.PLMN()
		if mcc != row.mcc || mnc != row.mnc || cell.TrackingAreaCode() != row.tac || cell.EUTRANCellID() != row.eci {
			t.Fatal("serving PLMN width or TAC/ECI was changed")
		}
		if pani, err := cell.PAccessNetworkInfo(); pani != "" || !errors.Is(err, ErrServingCellUnavailable) {
			t.Fatal("cached location became PANI")
		}
	}
}

func TestServingCellLocationRejectsMissingAmbiguousAndNonLTEValues(t *testing.T) {
	for _, row := range []struct{ raw, operator string }{
		{"310,260,ABCD,1234567", "310260"}, // LAC is not TAC.
		{"310,260,0,1234567,", "310260"},
		{"310,260,,1234567,ABCD", "310260"},
		{"310,260,0,1234567,10000", "310260"}, // 5G TAC must not be truncated.
		{"310,260,0,10000000,ABCD", "310260"},
		{"310,260,10000,1234567,ABCD", "310260"},
		{"310,260,0,1234567,ABCD,EXTRA", "310260"},
		{"310,260,0,0,ABCD", "310260"},
		{"310,260,0,1,0", "310260"},
		{"310,260,0,-1,ABCD", "310260"},
		{"310,260,0,+1,ABCD", "310260"},
		{"310,260,0,0x1,ABCD", "310260"},
		{"310,260,0,1234567,abcd", "310260"},
		{"310,260,0,1234567, ABCD", "310260"},
		{"310,260,0,1234567,ABCD\x00", "310260"},
		{"31,260,0,1234567,ABCD", "31260"},
		{"310,2,0,1234567,ABCD", "3102"},
		{"310,2600,0,1234567,ABCD", "3102600"},
		{"310,26,0,1234567,ABCD", "310026"},
		{"310,260,0,1234567,ABCD", "20404"}, // Roaming != IMSI home PLMN.
		{strings.Repeat("1", 65), "310260"},
		{"", ""},
	} {
		cell, err := parseCachedLTELocation(row.raw, row.operator)
		if cell != (CachedServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
			t.Fatal("invalid location exposed")
		}
	}
}

func servingCellFixture(t *testing.T) (*ServingCellSource, *fakeBearerBackend) {
	t.Helper()
	pool, backend, _, selection := bearerFixture(t)
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := newServingCellSource(lease, func(_ context.Context, owner, path string) (servingCellSnapshot, error) {
		if owner != selection.snapshot.Owner || path != selection.snapshot.Path {
			t.Fatal("location read escaped selected service/modem")
		}
		cell, _ := parseCachedLTELocation("204,04,0,1234567,ABCD", "20404")
		return servingCellSnapshot{cell: cell, operator: "20404", state: 11,
			access: 1 << 14, registration: 5, capabilities: 1, enabled: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, backend
}

func TestServingCellStableCacheIsDiagnosticOnlyAndNeverReusableProof(t *testing.T) {
	source, _ := servingCellFixture(t)
	cell, err := source.ObserveCached(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mcc, mnc := cell.PLMN(); mcc != "204" || mnc != "04" {
		t.Fatal("SIM home PLMN replaced roaming serving PLMN")
	}
	if got, err := source.Read(context.Background()); got != (vowifi.LTECell{}) || !errors.Is(err, ErrServingCellUnavailable) {
		t.Fatal("cached location became LTE evidence")
	}
	source.read = func(context.Context, string, string) (servingCellSnapshot, error) {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	if got, err := source.ObserveCached(context.Background()); got != (CachedServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
		t.Fatal("old cache retained after unavailable read")
	}
	for _, object := range []any{source, *source, cell, &cell} {
		output := fmt.Sprintf("%v %+v %#v", object, object, object)
		for _, private := range []string{"204", "1234567", "ABCD", source.lease.selection.snapshot.Binding.ICCID, source.lease.selection.snapshot.Binding.IMSI} {
			if strings.Contains(output, private) {
				t.Fatal("diagnostic formatting leaked location or subscriber")
			}
		}
	}
}

func TestServingCellChecksOwnerSIMHardwareBearerAndClaimAfterRead(t *testing.T) {
	for _, mode := range []string{"owner", "SIM", "slot", "physical-generation", "bearer-address", "disconnected", "suspended", "removed-bearer", "claim-token", "runtime-pending"} {
		t.Run(mode, func(t *testing.T) {
			source, backend := servingCellFixture(t)
			read := source.read
			calls := 0
			source.read = func(ctx context.Context, owner, path string) (servingCellSnapshot, error) {
				calls++
				out, err := read(ctx, owner, path)
				if calls != 1 {
					return out, err
				}
				backend.mu.Lock()
				modem := backend.modems[path]
				source.lease.claim.mu.Lock()
				bearerPath := source.lease.claim.record.Path
				bearer := backend.bearers[bearerPath]
				switch mode {
				case "owner":
					modem.Owner = ":1.99"
				case "SIM":
					modem.Binding.ICCID = "8901000000000000099"
				case "slot":
					modem.Binding.Slot++
				case "physical-generation":
					modem.Binding.PhysicalToken = "replaced-hardware"
				case "bearer-address":
					bearer.Addresses = nil
				case "disconnected":
					bearer.Connected = false
				case "suspended":
					bearer.Suspended = true
				case "removed-bearer":
					modem.Bearers = nil
				case "claim-token":
					source.lease.claim.record.Token = hashText("replacement owner")
				case "runtime-pending":
					source.lease.claim.record.RuntimePending = true
				}
				source.lease.claim.mu.Unlock()
				backend.modems[path] = modem
				backend.bearers[bearerPath] = bearer
				backend.mu.Unlock()
				return out, err
			}
			if cell, err := source.ObserveCached(context.Background()); cell != (CachedServingCell{}) || !errors.Is(err, ErrChanged) {
				t.Fatal("changed binding accepted", err)
			}
			if calls != 1 {
				t.Fatal("read continued after binding change")
			}
		})
	}
}

func TestServingCellObservedCellGenerationChangesAreRejected(t *testing.T) {
	for _, mode := range []string{"mnc-width", "tac", "eci", "operator", "state", "access", "registration", "capabilities", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			source, _ := servingCellFixture(t)
			read, calls := source.read, 0
			source.read = func(ctx context.Context, owner, path string) (servingCellSnapshot, error) {
				row, err := read(ctx, owner, path)
				calls++
				if calls == 2 {
					switch mode {
					case "mnc-width":
						row.cell.mnc = "004"
					case "tac":
						row.cell.tac++
					case "eci":
						row.cell.eci++
					case "operator":
						row.operator = "20408"
					case "state":
						row.state = 8
					case "access":
						row.access = 1 << 15
					case "registration":
						row.registration = 1
					case "capabilities":
						row.capabilities = 3
					case "enabled":
						row.enabled = 0
					}
				}
				return row, err
			}
			if cell, err := source.ObserveCached(context.Background()); cell != (CachedServingCell{}) || !errors.Is(err, ErrChanged) {
				t.Fatal("different cell/state snapshots combined", err)
			}
		})
	}
}

func TestServingCellAuthorizationCancellationAndErrorsAreFailClosed(t *testing.T) {
	for _, mode := range []string{"denied-before", "denied-after", "canceled-before", "canceled-after", "untrusted-error"} {
		t.Run(mode, func(t *testing.T) {
			source, _ := servingCellFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			read, reads, auth := source.read, 0, 0
			source.lease.pool.authorize = func(context.Context, Selection) error {
				auth++
				if mode == "denied-before" || mode == "denied-after" && auth > 1 {
					return errors.New("private subscriber diagnostic")
				}
				return nil
			}
			source.read = func(ctx context.Context, owner, path string) (servingCellSnapshot, error) {
				reads++
				if mode == "canceled-after" {
					cancel()
				}
				if mode == "untrusted-error" {
					return servingCellSnapshot{}, errors.New("private subscriber diagnostic")
				}
				return read(ctx, owner, path)
			}
			want := error(ErrServingCellUnavailable)
			if strings.HasPrefix(mode, "denied") {
				want = ErrDenied
			}
			if strings.HasPrefix(mode, "canceled") {
				want = context.Canceled
			}
			if mode == "canceled-before" {
				cancel()
			}
			if cell, err := source.ObserveCached(ctx); cell != (CachedServingCell{}) || !errors.Is(err, want) {
				t.Fatal("unsafe result", err)
			}
			if strings.HasSuffix(mode, "before") && reads != 0 {
				t.Fatal("read started after denied/canceled preflight")
			}
		})
	}
	if _, err := newServingCellSource(nil, nil); !errors.Is(err, ErrSelection) {
		t.Fatal("nil source accepted")
	}
	var source *ServingCellSource
	if cell, err := source.Read(nil); !reflect.DeepEqual(cell, vowifi.LTECell{}) || !errors.Is(err, ErrSelection) {
		t.Fatal("nil source panicked or became proof")
	}
}

func FuzzServingCellLocation(f *testing.F) {
	for _, value := range []string{"310,260,0,1234567,ABCD", "001,01,0,FFFFFFF,FFFF", "310,260,ABCD,1234567", "310,260,0,10000000,10000", ""} {
		f.Add(value, "310260")
	}
	f.Fuzz(func(t *testing.T, raw, operator string) {
		cell, err := parseCachedLTELocation(raw, operator)
		if err == nil {
			mcc, mnc := cell.PLMN()
			if !asciiDigits(mcc, 3, 3) || !asciiDigits(mnc, 2, 3) || mcc+mnc != operator || cell.eci == 0 || cell.eci > 0xfffffff || cell.tac == 0 {
				t.Fatal("invalid location accepted")
			}
		} else if cell != (CachedServingCell{}) {
			t.Fatal("failure exposed partial location")
		}
		if pani, err := cell.PAccessNetworkInfo(); pani != "" || !errors.Is(err, ErrServingCellUnavailable) {
			t.Fatal("cache promoted to PANI")
		}
	})
}
