package hostims

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type closeFunc struct {
	once sync.Once
	f    func()
}

func (c *closeFunc) Close() error { c.once.Do(c.f); return nil }

type memoryClaims struct {
	mu         sync.Mutex
	values     map[string][]byte
	locks      map[string]bool
	changed    chan struct{}
	fail       bool
	failIntent string
}

func newMemoryClaims() *memoryClaims {
	return &memoryClaims{values: map[string][]byte{}, locks: map[string]bool{}, changed: make(chan struct{}, 128)}
}
func (j *memoryClaims) Lock(key string) (io.Closer, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.locks[key] {
		return nil, ErrBusy
	}
	j.locks[key] = true
	return &closeFunc{f: func() { j.mu.Lock(); j.locks[key] = false; j.mu.Unlock() }}, nil
}
func (j *memoryClaims) Read(_ context.Context, key string) ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail {
		return nil, ErrJournal
	}
	return append([]byte(nil), j.values[key]...), nil
}
func (j *memoryClaims) Compare(_ context.Context, key string, before, after []byte) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail {
		return false, ErrJournal
	}
	if j.failIntent != "" && after != nil {
		var record claimRecord
		_ = json.Unmarshal(after, &record)
		if record.Pending == j.failIntent {
			return false, ErrJournal
		}
	}
	if !bytes.Equal(j.values[key], before) {
		return false, nil
	}
	if after == nil {
		delete(j.values, key)
	} else {
		j.values[key] = append([]byte(nil), after...)
	}
	select {
	case j.changed <- struct{}{}:
	default:
	}
	return true, nil
}
func (j *memoryClaims) record(key string) claimRecord {
	raw, _ := j.Read(context.Background(), key)
	var r claimRecord
	_ = json.Unmarshal(raw, &r)
	return r
}

type fakeBearerBackend struct {
	mu               sync.Mutex
	modems           map[string]modemSnapshot
	bearers          map[string]bearerSnapshot
	calls            []string
	next             int
	blockOp          string
	entered          chan struct{}
	release          chan struct{}
	failOp           string
	noMux            bool
	badConfiguration bool
}

func newFakeBearerBackend() *fakeBearerBackend {
	b := &fakeBearerBackend{modems: map[string]modemSnapshot{}, bearers: map[string]bearerSnapshot{}, next: 10}
	for i := 0; i < 2; i++ {
		path := fmt.Sprintf("/org/freedesktop/ModemManager1/Modem/%d", i)
		b.modems[path] = modemSnapshot{Owner: ":1.7", Path: path, State: 8, Families: 7, Binding: cardBinding{
			Equipment: fmt.Sprintf("12345678901234%d", i), Device: fmt.Sprintf("modem-%d", i), SIMPath: fmt.Sprintf("/org/freedesktop/ModemManager1/SIM/%d", i),
			ICCID: fmt.Sprintf("890100000000000000%d", i), IMSI: fmt.Sprintf("31026012345678%d", i), Physical: "/sys/devices/virtual/modem-test", PhysicalToken: fmt.Sprint(i), Slot: 1}}
	}
	return b
}
func (b *fakeBearerBackend) Modems(context.Context) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := map[string]string{}
	for path, m := range b.modems {
		result[path] = m.Binding.Equipment
	}
	return result, nil
}
func (b *fakeBearerBackend) Modem(_ context.Context, path string) (modemSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.modems[path]
	if !ok {
		return m, ErrSelection
	}
	m.Bearers = append([]string(nil), m.Bearers...)
	return m, nil
}
func (b *fakeBearerBackend) Bearer(_ context.Context, owner, path string) (bearerSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.bearers[path]
	if !ok {
		return value, ErrChanged
	}
	value.Addresses = append([]BearerAddress(nil), value.Addresses...)
	return value, nil
}
func (b *fakeBearerBackend) before(op, path string) error {
	b.mu.Lock()
	b.calls = append(b.calls, op+":"+path)
	block := b.blockOp == op
	entered, release := b.entered, b.release
	fail := b.failOp == op
	b.mu.Unlock()
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	if fail {
		return errors.New("private SIM diagnostic must not escape")
	}
	return nil
}
func (b *fakeBearerBackend) Create(_ context.Context, owner, modem string, props bearerProperties) (string, error) {
	if err := b.before("create", modem); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	path := fmt.Sprintf("/org/freedesktop/ModemManager1/Bearer/%d", b.next)
	b.next++
	b.bearers[path] = bearerSnapshot{Properties: props}
	m := b.modems[modem]
	m.Bearers = append(m.Bearers, path)
	b.modems[modem] = m
	return path, nil
}
func (b *fakeBearerBackend) Connect(_ context.Context, owner, path string) error {
	if err := b.before("connect", path); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.bearers[path]
	value.Connected = true
	value.ConfigurationError = b.badConfiguration
	value.Multiplexed = !b.noMux
	value.Interface = "wwan-test"
	value.Addresses = []BearerAddress{{Address: netip.MustParsePrefix("192.0.2.10/32"), MTU: 1500}, {Address: netip.MustParsePrefix("2001:db8::10/64"), MTU: 1500}}
	b.bearers[path] = value
	return nil
}
func (b *fakeBearerBackend) Disconnect(_ context.Context, owner, path string) error {
	if err := b.before("disconnect", path); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.bearers[path]
	value.Connected = false
	b.bearers[path] = value
	return nil
}
func (b *fakeBearerBackend) Delete(_ context.Context, owner, modem, path string) error {
	if err := b.before("delete", path); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.bearers, path)
	m := b.modems[modem]
	var paths []string
	for _, value := range m.Bearers {
		if value != path {
			paths = append(paths, value)
		}
	}
	m.Bearers = paths
	b.modems[modem] = m
	return nil
}
func (b *fakeBearerBackend) count(op string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, call := range b.calls {
		if strings.HasPrefix(call, op+":") {
			count++
		}
	}
	return count
}

func bearerFixture(t *testing.T) (*BearerPool, *fakeBearerBackend, *memoryClaims, Selection) {
	t.Helper()
	backend, journal := newFakeBearerBackend(), newMemoryClaims()
	pool, err := NewBearerPool(backend, journal, func(context.Context, Selection) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s, err := pool.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/0")
	if err != nil {
		t.Fatal(err)
	}
	return pool, backend, journal, s
}
func awaitClaim(t *testing.T, j *memoryClaims, key string, test func(claimRecord) bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if test(j.record(key)) {
			return
		}
		select {
		case <-j.changed:
		case <-deadline.C:
			t.Fatal("claim transition timed out")
		}
	}
}

func TestBearerOwnerCreatesVerifiesAndReleasesOnlyOwnPath(t *testing.T) {
	p, b, j, s := bearerFixture(t)
	lease, err := p.Acquire(context.Background(), s, BearerOptions{APN: "IMS"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := j.record(s.key)
	if r.Stage != "active" || r.Pending != "" || r.Uncertain || r.Family != 4 || r.Options.APN != "ims" {
		t.Fatalf("unexpected claim: stage=%s,pending=%s", r.Stage, r.Pending)
	}
	config := lease.Configuration()
	config.Addresses[0].MTU = 99
	if lease.Configuration().Addresses[0].MTU != 1500 {
		t.Fatal("lease configuration is mutable")
	}
	if err := p.Recover(context.Background(), s); !errors.Is(err, ErrBusy) {
		t.Fatal("recovery stole a live lease")
	}
	if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate line admitted")
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.count("create") != 1 || b.count("connect") != 1 || b.count("disconnect") != 1 || b.count("delete") != 1 {
		t.Fatal("lifecycle repeated a mutation")
	}
	if raw, _ := j.Read(context.Background(), s.key); raw != nil {
		t.Fatal("released durable claim retained")
	}
	newLease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("stale lease released the new generation")
	}
	if err := newLease.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := newLease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBearerOwnerPolicyAndJournalGateBeforeIO(t *testing.T) {
	for _, name := range []string{"policy", "journal", "foreign_selection"} {
		t.Run(name, func(t *testing.T) {
			p, b, j, s := bearerFixture(t)
			switch name {
			case "policy":
				p.authorize = func(context.Context, Selection) error { return errors.New("blocked region") }
			case "journal":
				j.fail = true
			case "foreign_selection":
				s.pool = nil
			}
			if _, err := p.Acquire(context.Background(), s, BearerOptions{}); err == nil {
				t.Fatal("unsafe acquisition accepted")
			}
			if len(b.calls) != 0 {
				t.Fatal("gate rejection performed bearer I/O")
			}
		})
	}
}

func TestBearerOwnerPersistsIntentBeforeEveryMutation(t *testing.T) {
	for _, op := range []string{"create", "connect", "disconnect", "delete"} {
		t.Run(op, func(t *testing.T) {
			p, b, j, s := bearerFixture(t)
			var err error
			if op == "create" || op == "connect" {
				j.failIntent = op
				_, err = p.Acquire(context.Background(), s, BearerOptions{})
			} else {
				lease, acquireErr := p.Acquire(context.Background(), s, BearerOptions{})
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				j.failIntent = op
				err = lease.Close(context.Background())
			}
			if err == nil {
				t.Fatal("failed durable intent accepted")
			}
			if b.count(op) != 0 {
				t.Fatal("daemon mutated without a persisted intent")
			}
		})
	}
}

func TestBearerOwnerRecoversUnmutatedReservationWithoutTouchingModem(t *testing.T) {
	p, b, j, s := bearerFixture(t)
	options, _ := (BearerOptions{}).normalized()
	r := claimRecord{Version: 1, Token: hashText("old-process"), Binding: hashText("old-sim"), Owner: ":1.1", Modem: s.snapshot.Path, Options: options, Family: 4, Stage: "reserved"}
	raw, _ := json.Marshal(r)
	j.values[s.key] = raw
	if err := p.Recover(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 0 {
		t.Fatal("unmutated reservation recovery touched radio/bearers")
	}
	if raw, _ := j.Read(context.Background(), s.key); raw != nil {
		t.Fatal("reservation retained")
	}
}

func TestBearerOwnerPolicyRevocationBeforeConnectCleansOwnCreate(t *testing.T) {
	p, b, _, s := bearerFixture(t)
	checks := 0
	p.authorize = func(context.Context, Selection) error {
		checks++
		if checks == 3 {
			return ErrDenied
		}
		return nil
	}
	if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked policy connected")
	}
	if b.count("create") != 1 || b.count("connect") != 0 || b.count("delete") != 1 || b.count("disconnect") != 0 {
		t.Fatal("revocation cleanup was not limited to owned unconnected bearer")
	}
}

func TestBearerOwnerCorruptJournalFailsClosed(t *testing.T) {
	p, b, j, s := bearerFixture(t)
	j.values[s.key] = []byte(`{"Version":99}`)
	if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrJournal) {
		t.Fatal("corrupt claim accepted")
	}
	if len(b.calls) != 0 {
		t.Fatal("corrupt ledger allowed mutation")
	}
}

func TestBearerOwnerRefusesDaemonTransitionAndMissingPolicy(t *testing.T) {
	p, b, j, s := bearerFixture(t)
	if _, err := NewBearerPool(b, j, nil); err == nil {
		t.Fatal("missing policy callback accepted")
	}
	for _, state := range []int32{9, 10} {
		m := b.modems[s.snapshot.Path]
		m.State = state
		b.modems[s.snapshot.Path] = m
		if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrBusy) {
			t.Fatal("daemon transition admitted")
		}
	}
	if len(b.calls) != 0 {
		t.Fatal("daemon transition caused mutations")
	}
}

func TestBearerOwnerUnsupportedAddressingStillReleasesOwnBearer(t *testing.T) {
	p, b, _, s := bearerFixture(t)
	b.badConfiguration = true
	if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrParameters) {
		t.Fatalf("bad host addressing admitted: %v", err)
	}
	if b.count("create") != 1 || b.count("connect") != 1 || b.count("disconnect") != 1 || b.count("delete") != 1 {
		t.Fatal("address rejection did not safely release the proven owned bearer")
	}
}

func TestBearerOwnerDoesNotBlockOnAnotherUnreadySIM(t *testing.T) {
	p, b, _, s := bearerFixture(t)
	path := "/org/freedesktop/ModemManager1/Modem/1"
	m := b.modems[path]
	m.Binding.IMSI = ""
	m.Binding.ICCID = ""
	b.modems[path] = m
	if _, err := p.Select(context.Background(), s.snapshot.Path); err != nil {
		t.Fatal("unready second SIM blocked healthy modem")
	}
	m.Binding.Equipment = s.snapshot.Binding.Equipment
	b.modems[path] = m
	if _, err := p.Select(context.Background(), s.snapshot.Path); err == nil {
		t.Fatal("duplicate equipment identity accepted")
	}
}

func TestBearerOwnerForeignBearerPolicyAndMux(t *testing.T) {
	for _, name := range []string{"ims", "same_apn", "single_pdn", "no_mux", "internet_allowed"} {
		t.Run(name, func(t *testing.T) {
			p, b, _, s := bearerFixture(t)
			options := BearerOptions{}
			if name == "no_mux" {
				b.noMux = true
			} else {
				path := "/org/freedesktop/ModemManager1/Bearer/1"
				props := bearerProperties{APN: "internet", APNType: 2}
				if name == "ims" {
					props.APNType = 4
				}
				if name == "same_apn" {
					props.APN = "IMS"
				}
				if name == "single_pdn" {
					options.SinglePDN = true
				}
				b.bearers[path] = bearerSnapshot{Properties: props, Connected: true}
				m := b.modems[s.snapshot.Path]
				m.Bearers = []string{path}
				b.modems[s.snapshot.Path] = m
			}
			lease, err := p.Acquire(context.Background(), s, options)
			if name == "internet_allowed" {
				if err != nil {
					t.Fatal(err)
				}
				if err := lease.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("conflicting bearer admitted")
			}
			if name != "no_mux" && name != "internet_allowed" && b.count("create") != 0 {
				t.Fatal("conflict caused a Create")
			}
			if _, ok := b.bearers["/org/freedesktop/ModemManager1/Bearer/1"]; name != "no_mux" && !ok {
				t.Fatal("foreign bearer deleted")
			}
		})
	}
}

func TestBearerOwnerCancellationRetainsMutationObserver(t *testing.T) {
	for _, op := range []string{"create", "connect", "delete"} {
		t.Run(op, func(t *testing.T) {
			p, b, j, s := bearerFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var lease *BearerLease
			var err error
			if op == "delete" {
				lease, err = p.Acquire(context.Background(), s, BearerOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			b.mu.Lock()
			b.blockOp = op
			b.entered = make(chan struct{}, 1)
			b.release = make(chan struct{})
			b.mu.Unlock()
			defer func() {
				select {
				case <-b.release:
				default:
					close(b.release)
				}
			}()
			done := make(chan error, 1)
			go func() {
				if op == "delete" {
					done <- lease.Close(ctx)
				} else {
					_, err := p.Acquire(ctx, s, BearerOptions{})
					done <- err
				}
			}()
			select {
			case <-b.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("mutation not issued")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled operation accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("foreground waited for daemon completion")
			}
			if _, err := p.Acquire(context.Background(), s, BearerOptions{}); err == nil {
				t.Fatal("uncertain mutation was retried")
			}
			if err := p.Recover(context.Background(), s); err == nil {
				t.Fatal("pending mutation overlapped recovery")
			}
			if j.record(s.key).Pending != op {
				t.Fatal("intent was not retained")
			}
			close(b.release)
			awaitClaim(t, j, s.key, func(r claimRecord) bool { return r.Pending == "" && r.Uncertain })
			if err := p.Recover(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if b.count("create") != 1 || b.count("delete") != 1 {
				t.Fatal("late reply caused duplicate Create/Delete")
			}
			if op == "create" && b.count("connect") != 0 {
				t.Fatal("late Create automatically connected")
			}
		})
	}
}

func TestBearerOwnerUnknownErrorsNeverRetryOrGuessCleanup(t *testing.T) {
	for _, op := range []string{"create", "connect", "disconnect", "delete"} {
		t.Run(op, func(t *testing.T) {
			p, b, j, s := bearerFixture(t)
			var err error
			if op == "create" || op == "connect" {
				b.failOp = op
				_, err = p.Acquire(context.Background(), s, BearerOptions{})
			} else {
				lease, acquireErr := p.Acquire(context.Background(), s, BearerOptions{})
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				b.failOp = op
				err = lease.Close(context.Background())
			}
			if !errors.Is(err, ErrUncertain) || strings.Contains(err.Error(), "private SIM") {
				t.Fatalf("error=%v", err)
			}
			if j.record(s.key).Pending != "unknown" {
				t.Fatal("failed daemon outcome was forgotten")
			}
			before := append([]string(nil), b.calls...)
			if err := p.Recover(context.Background(), s); !errors.Is(err, ErrUncertain) {
				t.Fatal("unknown result recovered without proof")
			}
			_, _ = p.Acquire(context.Background(), s, BearerOptions{})
			if !reflect.DeepEqual(before, b.calls) {
				t.Fatal("unknown mutation was retried or cleaned")
			}
		})
	}
}

func TestBearerOwnerPinsServiceHardwareSIMAndProperties(t *testing.T) {
	for _, name := range []string{"owner", "imsi", "iccid", "slot", "physical", "apn", "family", "interface", "address", "configuration"} {
		t.Run(name, func(t *testing.T) {
			p, b, _, s := bearerFixture(t)
			lease, err := p.Acquire(context.Background(), s, BearerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			m := b.modems[s.snapshot.Path]
			originalModem := m
			path := m.Bearers[0]
			bearer := b.bearers[path]
			originalBearer := bearer
			bearer.Addresses = append([]BearerAddress(nil), bearer.Addresses...)
			switch name {
			case "owner":
				m.Owner = ":1.99"
			case "imsi":
				m.Binding.IMSI = "310260123456799"
			case "iccid":
				m.Binding.ICCID = "8901000000000000099"
			case "slot":
				m.Binding.Slot++
			case "physical":
				m.Binding.PhysicalToken = "replacement"
			case "apn":
				bearer.Properties.APN = "internet"
			case "family":
				bearer.Properties.IPFamily = 1
			case "interface":
				bearer.Interface = "other0"
			case "address":
				bearer.Addresses[0].Address = netip.MustParsePrefix("192.0.2.11/32")
			case "configuration":
				bearer.ConfigurationError = true
			}
			b.modems[s.snapshot.Path], b.bearers[path] = m, bearer
			if err := lease.Verify(context.Background()); err == nil {
				t.Fatal("changed ownership verified")
			}
			// Configuration changes can still safely release the exact own PDN;
			// changed service/SIM/properties must not authorize any mutation.
			if name != "interface" && name != "address" && name != "configuration" {
				if err := lease.Close(context.Background()); err == nil {
					t.Fatal("changed owner was released")
				}
				if b.count("disconnect") != 0 || b.count("delete") != 0 {
					t.Fatal("cleanup touched replacement owner")
				}
			}
			b.modems[s.snapshot.Path], b.bearers[path] = originalModem, originalBearer
			if err := lease.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBearerOwnerMultiModemOperationsDoNotShareGate(t *testing.T) {
	p, b, _, s := bearerFixture(t)
	other, err := p.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	b.blockOp = "connect"
	b.entered = make(chan struct{}, 2)
	b.release = make(chan struct{})
	defer func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	}()
	type result struct {
		lease *BearerLease
		err   error
	}
	done := make(chan result, 2)
	for _, selection := range []Selection{s, other} {
		go func(value Selection) {
			lease, err := p.Acquire(context.Background(), value, BearerOptions{})
			done <- result{lease, err}
		}(selection)
	}
	for range 2 {
		select {
		case <-b.entered:
		case <-time.After(time.Second):
			t.Fatal("one modem blocked the other modem's setup")
		}
	}
	close(b.release)
	for range 2 {
		result := <-done
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := result.lease.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBearerOwnerRedactsSelectionAndBinding(t *testing.T) {
	_, _, _, s := bearerFixture(t)
	for _, value := range []any{s, s.snapshot, s.snapshot.Binding} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			text := fmt.Sprintf(format, value)
			if strings.Contains(text, s.snapshot.Binding.IMSI) || strings.Contains(text, s.snapshot.Binding.ICCID) {
				t.Fatal("formatted identity leaked")
			}
		}
	}
	encoded, err := json.Marshal(s)
	if err != nil || strings.Contains(string(encoded), s.snapshot.Binding.IMSI) {
		t.Fatal("JSON identity leaked")
	}
}
