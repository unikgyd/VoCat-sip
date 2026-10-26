//go:build linux

package hostims

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"vocat/internal/vowifi"
)

type servingCellWire struct {
	mu                    sync.Mutex
	backend               *fakeBearerBackend
	path                  string
	access, registration  uint32
	capabilities, enabled uint32
	operator              string
	values                map[uint32]dbus.Variant
	getCalls, setupCalls  int
	getError              *dbus.Error
	onGet                 func(int)
}

func (w *servingCellWire) GetLocation() (map[uint32]dbus.Variant, *dbus.Error) {
	w.mu.Lock()
	w.getCalls++
	count, callback, err := w.getCalls, w.onGet, w.getError
	values := make(map[uint32]dbus.Variant, len(w.values))
	for key, value := range w.values {
		values[key] = value
	}
	w.mu.Unlock()
	if callback != nil {
		callback(count)
	}
	return values, err
}

func (w *servingCellWire) Setup(uint32, bool) *dbus.Error {
	w.mu.Lock()
	w.setupCalls++
	w.mu.Unlock()
	return nil
}

func (w *servingCellWire) properties(iface string) (map[string]dbus.Variant, *dbus.Error) {
	w.mu.Lock()
	access, registration, capabilities, enabled, operator := w.access, w.registration, w.capabilities, w.enabled, w.operator
	w.mu.Unlock()
	switch iface {
	case mmModem:
		modem, err := w.backend.Modem(context.Background(), w.path)
		if err != nil {
			return nil, dbus.MakeFailedError(err)
		}
		props := wireModemProps(modem)
		props["AccessTechnologies"] = dbus.MakeVariant(access)
		return props, nil
	case mm3gpp:
		return map[string]dbus.Variant{"RegistrationState": dbus.MakeVariant(registration), "OperatorCode": dbus.MakeVariant(operator)}, nil
	case mmLocation:
		return map[string]dbus.Variant{"Capabilities": dbus.MakeVariant(capabilities), "Enabled": dbus.MakeVariant(enabled),
			"SignalsLocation": dbus.MakeVariant(false),
			// The untrusted/disabled public property must never be used instead
			// of GetLocation. Extra invented fields cannot prove duplex or age.
			"Location":  dbus.MakeVariant(map[uint32]dbus.Variant{1: dbus.MakeVariant("999,99,0,7654321,FFFF")}),
			"Timestamp": dbus.MakeVariant(uint64(time.Now().Unix())), "Duplex": dbus.MakeVariant("FDD")}, nil
	default:
		return nil, dbus.MakeFailedError(errors.New("unexpected interface"))
	}
}

func servingCellWireFixture(t *testing.T) (*ServingCellSource, *servingCellWire, *dbus.Conn) {
	t.Helper()
	pool, backend, _, selection, server := mmWireFixture(t)
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := &servingCellWire{backend: backend, path: selection.snapshot.Path, access: mmAccessLTE,
		registration: 5, capabilities: 1, enabled: 1, operator: "20404",
		values: map[uint32]dbus.Variant{1: dbus.MakeVariant("204,04,0,1234567,ABCD")}}
	if err := server.Export(&wireProperties{read: state.properties}, dbus.ObjectPath(state.path), "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	if err := server.Export(state, dbus.ObjectPath(state.path), mmLocation); err != nil {
		t.Fatal(err)
	}
	source, err := NewModemManagerServingCellSource(lease)
	if err != nil {
		t.Fatal(err)
	}
	return source, state, server
}

func TestServingCellModemManagerReadsOnlyAuthenticatedCachedLocation(t *testing.T) {
	source, state, _ := servingCellWireFixture(t)
	cell, err := source.ObserveCached(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mcc, mnc := cell.PLMN(); mcc != "204" || mnc != "04" || cell.TrackingAreaCode() != 0xabcd || cell.EUTRANCellID() != 0x1234567 {
		t.Fatal("used home IMSI, disabled public property or swapped TAC/ECI")
	}
	if cell, err := source.Read(context.Background()); cell != (vowifi.LTECell{}) || !errors.Is(err, ErrServingCellUnavailable) {
		t.Fatal("MM cache promoted to usable LTE evidence")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.getCalls != 4 || state.setupCalls != 0 {
		t.Fatal("unexpected query count or enabled location")
	}
}

func TestServingCellModemManagerRejectsUnavailableUnregisteredAndAmbiguousLocation(t *testing.T) {
	for _, mode := range []string{"disabled", "unsupported", "no-source", "typed-not-string", "legacy-no-tac", "nr-tac", "wide-ci", "operator-mismatch", "mnc-width-mismatch", "sms-only", "emergency-only", "unregistered", "non-lte", "mixed-rat", "unknown-rat", "error", "large-dictionary"} {
		t.Run(mode, func(t *testing.T) {
			source, state, _ := servingCellWireFixture(t)
			state.mu.Lock()
			switch mode {
			case "disabled":
				state.enabled = 0
			case "unsupported":
				state.capabilities = 0
			case "no-source":
				state.values = map[uint32]dbus.Variant{2: dbus.MakeVariant("unrelated GPS data")}
			case "typed-not-string":
				state.values[1] = dbus.MakeVariant(uint32(123))
			case "legacy-no-tac":
				state.values[1] = dbus.MakeVariant("204,04,ABCD,1234567")
			case "nr-tac":
				state.values[1] = dbus.MakeVariant("204,04,0,1234567,10000")
			case "wide-ci":
				state.values[1] = dbus.MakeVariant("204,04,0,FFFFFFFF,ABCD")
			case "operator-mismatch":
				state.operator = "310260"
			case "mnc-width-mismatch":
				state.operator = "204004"
			case "sms-only":
				state.registration = 6
			case "emergency-only":
				state.registration = 8
			case "unregistered":
				state.registration = 2
			case "non-lte":
				state.access = 1 << 5
			case "mixed-rat":
				state.access = mmAccessLTE | 1<<15
			case "unknown-rat":
				state.access = 0
			case "error":
				state.getError = dbus.MakeFailedError(errors.New("private subscriber/area diagnostic"))
			case "large-dictionary":
				for i := uint32(2); i <= 17; i++ {
					state.values[i] = dbus.MakeVariant("unrelated")
				}
			}
			state.mu.Unlock()
			cell, err := source.ObserveCached(context.Background())
			if cell != (CachedServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
				t.Fatal("unsafe cell exposed", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("untrusted D-Bus text leaked")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.setupCalls != 0 {
				t.Fatal("source enabled location")
			}
			if (mode == "disabled" || mode == "unsupported") && state.getCalls != 0 {
				t.Fatal("queried disabled location cache")
			}
		})
	}
}

func TestServingCellModemManagerRejectsChangesDuringRead(t *testing.T) {
	for _, mode := range []string{"location", "operator", "registration", "access", "enabled", "SIM", "hardware-identity", "service-owner"} {
		t.Run(mode, func(t *testing.T) {
			source, state, server := servingCellWireFixture(t)
			state.mu.Lock()
			state.onGet = func(count int) {
				if count != 1 {
					return
				}
				state.mu.Lock()
				switch mode {
				case "location":
					state.values[1] = dbus.MakeVariant("204,04,0,1234568,ABCD")
				case "operator":
					state.operator = "20408"
				case "registration":
					state.registration = 1
				case "access":
					state.access = 1 << 15
				case "enabled":
					state.enabled = 0
				}
				state.mu.Unlock()
				if mode == "SIM" || mode == "hardware-identity" {
					state.backend.mu.Lock()
					modem := state.backend.modems[state.path]
					if mode == "SIM" {
						modem.Binding.ICCID = "8901000000000000099"
					} else {
						modem.Binding.Device = "replacement-hardware-identity"
					}
					state.backend.modems[state.path] = modem
					state.backend.mu.Unlock()
				}
				if mode == "service-owner" {
					_, _ = server.ReleaseName(mmService)
				}
			}
			state.mu.Unlock()
			if cell, err := source.ObserveCached(context.Background()); cell != (CachedServingCell{}) || !errors.Is(err, ErrChanged) {
				t.Fatal("mixed generation accepted", err)
			}
		})
	}
}

func TestServingCellModemManagerReadRespectsCallerCancellation(t *testing.T) {
	source, state, _ := servingCellWireFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	state.mu.Lock()
	state.onGet = func(int) { close(entered); <-release }
	state.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := source.ObserveCached(ctx); done <- err }()
	select {
	case <-entered:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("read not entered")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation hidden", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read ignored caller cancellation")
	}
}

func TestServingCellModemManagerSourceRejectsDifferentOrMissingBackend(t *testing.T) {
	source, _ := servingCellFixture(t)
	if _, err := NewModemManagerServingCellSource(source.lease); !errors.Is(err, ErrSelection) {
		t.Fatal("unrelated backend accepted")
	}
	if _, err := NewModemManagerServingCellSource(nil); !errors.Is(err, ErrSelection) {
		t.Fatal("nil lease accepted")
	}
}
