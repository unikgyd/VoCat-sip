package hostims

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type fakeUIM struct {
	mu                                                                       sync.Mutex
	iccid, imsi, domain, impi, impu                                          string
	hasISIM, missingAD, badISIM, failOpen, failClose, failRelease, uncertain bool
	adMNC                                                                    byte
	opened, closed, released, auths                                          int
	appKinds                                                                 []string
	response                                                                 []byte
	smsp                                                                     []byte
	smspErr                                                                  error
	smspUncertain                                                            bool
	authErr                                                                  error
	authEntered, authRelease                                                 chan struct{}
	onAuth                                                                   func()
}
type fakeUIMSession struct {
	state   *fakeUIM
	channel byte
	isim    bool
	file    uint16
}

func newFakeUIM(s Selection) *fakeUIM {
	return &fakeUIM{iccid: s.snapshot.Binding.ICCID, imsi: s.snapshot.Binding.IMSI, hasISIM: true, adMNC: 3,
		domain: "ims.example.net", impi: "subscriber@private.example.net", impu: "sip:+15555550100@ims.example.net",
		response: []byte{0xdb, 4, 1, 2, 3, 4, 16, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 16, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 0x90, 0}}
}
func (f *fakeUIM) open(context.Context, Selection) (uimSession, error) {
	return &fakeUIMSession{state: f}, nil
}
func (s *fakeUIMSession) Card(_ context.Context, _ cardBinding) (uimCard, string, error) {
	f := s.state
	f.mu.Lock()
	defer f.mu.Unlock()
	return uimCard{Slot: 1, Count: 1, USIM: append([]byte(nil), usimPrefix...), HasISIM: f.hasISIM, ISIM: func() []byte {
		if f.hasISIM {
			return append([]byte(nil), isimPrefix...)
		}
		return nil
	}()}, f.iccid, nil
}
func (s *fakeUIMSession) Open(_ context.Context, slot byte, aid []byte) (byte, error) {
	f := s.state
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOpen {
		f.uncertain = true
		return 0, errors.New("private open diagnostic")
	}
	if slot != 1 || s.channel != 0 || !bytes.Equal(aid, usimPrefix) && !bytes.Equal(aid, isimPrefix) {
		return 0, ErrSIMApplication
	}
	s.channel, s.isim = 4, bytes.Equal(aid, isimPrefix)
	f.opened++
	kind := "USIM"
	if s.isim {
		kind = "ISIM"
	}
	f.appKinds = append(f.appKinds, kind)
	return 4, nil
}
func (s *fakeUIMSession) APDU(_ context.Context, slot, channel byte, command []byte) ([]byte, error) {
	f := s.state
	f.mu.Lock()
	if slot != 1 || channel != 4 || s.channel != channel || len(command) < 4 || command[0] != 0x40 {
		f.mu.Unlock()
		return nil, ErrSIM
	}
	if command[1] == 0x88 {
		f.auths++
		entered, release, hook := f.authEntered, f.authRelease, f.onAuth
		result, err := append([]byte(nil), f.response...), f.authErr
		if err != nil {
			f.uncertain = true
		}
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if release != nil {
			<-release
		}
		return result, err
	}
	defer f.mu.Unlock()
	if command[1] == 0xa4 && len(command) >= 7 {
		s.file = uint16(command[5])<<8 | uint16(command[6])
		return []byte{0x90, 0}, nil
	}
	if command[1] != 0xb0 && command[1] != 0xb2 {
		return nil, ErrSIM
	}
	var raw []byte
	if !s.isim {
		switch s.file {
		case 0x6f07:
			raw = encodeCardIMSI(f.imsi)
		case 0x6fad:
			if f.missingAD {
				return []byte{0x6a, 0x82}, nil
			}
			raw = []byte{0, 0, 0, f.adMNC}
		case 0x6f42:
			if f.smspErr != nil {
				f.uncertain = f.smspUncertain
				return nil, f.smspErr
			}
			if len(f.smsp) == 0 {
				return []byte{0x6a, 0x82}, nil
			}
			if command[1] != 0xb2 || command[2] != 1 || command[3] != 4 {
				return nil, ErrSIMIdentity
			}
			raw = append([]byte(nil), f.smsp...)
		default:
			return nil, ErrSIMIdentity
		}
	} else {
		if f.badISIM {
			return []byte{0x6a, 0x82}, nil
		}
		value := ""
		switch s.file {
		case 0x6f02:
			value = f.impi
		case 0x6f03:
			value = f.domain
		case 0x6f04:
			value = f.impu
		default:
			return nil, ErrSIMIdentity
		}
		raw = append([]byte{0x80, byte(len(value))}, []byte(value)...)
	}
	return append(raw, 0x90, 0), nil
}
func (s *fakeUIMSession) CloseChannel(_ context.Context, slot, channel byte) error {
	f := s.state
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClose {
		return errors.New("private close diagnostic")
	}
	if slot != 1 || channel != 4 || channel != s.channel {
		return ErrSIM
	}
	s.channel = 0
	f.closed++
	return nil
}
func (s *fakeUIMSession) Uncertain() bool {
	f := s.state
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uncertain
}
func (s *fakeUIMSession) Close() error {
	f := s.state
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.channel != 0 || f.failRelease {
		return errors.New("private release diagnostic")
	}
	f.released++
	return nil
}
func simFixture(t *testing.T) (*SIMProvider, *fakeUIM, *fakeBearerBackend, *memoryClaims) {
	t.Helper()
	p, b, j, s := bearerFixture(t)
	lease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeUIM(s)
	provider, err := newSIMProvider(lease, f.open)
	if err != nil {
		t.Fatal(err)
	}
	return provider, f, b, j
}
func assertNoAKA(t *testing.T, result vowifi.AKAResult) {
	t.Helper()
	if len(result.RES)+len(result.CK)+len(result.IK)+len(result.AUTS) != 0 || result.SynchronizationFailure {
		t.Fatal("failed operation returned authentication material")
	}
}
func TestCellularSIMReadsProvisionedIdentityThenAuthenticatesISIM(t *testing.T) {
	p, f, _, j := simFixture(t)
	card, err := p.ReadCard(context.Background())
	if err != nil || card.Application() != "ISIM" || card.IMSDomain() != f.domain || card.PrivateIdentity() != f.impi || card.PublicIdentity() != f.impu || card.Subscriber().HomeMNC != "260" {
		t.Fatal("card identity mismatch", err)
	}
	f.onAuth = func() {
		record := j.record(p.lease.selection.key)
		if !record.SIMPending || len(record.SIMChallenges) != 1 {
			t.Error("AUTH sent before durable intent")
		}
	}
	result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
	if err != nil || len(result.CK) != 16 || len(result.IK) != 16 || len(result.RES) != 4 {
		t.Fatal("ISIM AKA failed", err)
	}
	clearCardAKA(&result)
	if f.opened != f.closed || f.auths != 1 || f.appKinds[len(f.appKinds)-1] != "ISIM" || f.released != 2 {
		t.Fatal("incorrect application or cleanup")
	}
	if err := p.lease.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCellularSIMUSIMFallbackRequiresAbsentISIMAndExactMNC(t *testing.T) {
	for _, length := range []byte{2, 3, 0, 4} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			p, f, _, _ := simFixture(t)
			f.hasISIM = false
			f.adMNC = length
			card, err := p.ReadCard(context.Background())
			if length != 2 && length != 3 {
				if err == nil {
					t.Fatal("MNC guessed")
				}
				return
			}
			mnc := "260"
			if length == 2 {
				mnc = "026"
			}
			if err != nil || card.Application() != "USIM" || card.IMSDomain() != "ims.mnc"+mnc+".mcc310.3gppnetwork.org" {
				t.Fatal("wrong fallback identity", err)
			}
			result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
			if err != nil {
				t.Fatal(err)
			}
			clearCardAKA(&result)
			if f.appKinds[len(f.appKinds)-1] != "USIM" {
				t.Fatal("wrong app")
			}
		})
	}
	p, f, _, _ := simFixture(t)
	f.badISIM = true
	if _, err := p.ReadCard(context.Background()); err == nil || f.auths != 0 {
		t.Fatal("broken ISIM downgraded")
	}
	p, f, _, _ = simFixture(t)
	f.missingAD = true
	card, err := p.ReadCard(context.Background())
	if err != nil || card.Subscriber().HomeMNC != "" || card.Application() != "ISIM" {
		t.Fatal("ISIM must not invent MNC", err)
	}
}
func TestCellularSIMChangesStopBeforeChallenge(t *testing.T) {
	for _, field := range []string{"iccid", "imsi", "impi", "impu", "domain", "mnc", "interface", "address", "multiplex", "preference"} {
		t.Run(field, func(t *testing.T) {
			p, f, b, j := simFixture(t)
			card, err := p.ReadCard(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "iccid":
				f.iccid = "8901000000000000099"
			case "imsi":
				f.imsi = "310260123456789"
			case "impi":
				f.impi = "changed@private.example.net"
			case "impu":
				f.impu = "sip:+15555550101@ims.example.net"
			case "domain":
				f.domain = "other.example.net"
			case "mnc":
				f.adMNC = 2
			case "interface", "address", "multiplex":
				b.mu.Lock()
				value := b.bearers[p.lease.claim.record.Path]
				if field == "interface" {
					value.Interface = "wwan-other"
				}
				if field == "address" {
					value.Addresses = []BearerAddress{{Address: netip.MustParsePrefix("192.0.2.99/32")}}
				}
				if field == "multiplex" {
					value.Multiplexed = false
				}
				b.bearers[p.lease.claim.record.Path] = value
				b.mu.Unlock()
			}
			preference := ""
			if field == "preference" {
				preference = "usim"
			}
			result, err := p.AuthenticateWithPreference(context.Background(), card.Subscriber(), vowifi.AKAChallenge{}, preference)
			if err == nil || f.auths != 0 || len(j.record(p.lease.selection.key).SIMChallenges) != 0 {
				t.Fatal("changed binding reached authentication")
			}
			assertNoAKA(t, result)
		})
	}
}
func TestCellularSIMChallengeNeverReplayedAfterSuccessStatusOrTimeout(t *testing.T) {
	for _, mode := range []string{"success", "6c", "failure", "timeout", "sync"} {
		t.Run(mode, func(t *testing.T) {
			p, f, _, j := simFixture(t)
			card, err := p.ReadCard(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "6c":
				f.response = []byte{0x6c, 0x30}
			case "failure":
				f.response = []byte{0x98, 0x62}
			case "timeout":
				f.authErr = errors.New("secret raw AUTH request")
			case "sync":
				f.response = append(append([]byte{0xdc, 14}, bytes.Repeat([]byte{7}, 14)...), 0x90, 0)
			}
			result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
			if mode == "success" || mode == "sync" {
				if err != nil {
					t.Fatal(err)
				}
				if (mode == "sync") != result.SynchronizationFailure {
					t.Fatal("sync mismatch")
				}
				clearCardAKA(&result)
			} else {
				if err == nil {
					t.Fatal("failure accepted")
				}
				assertNoAKA(t, result)
			}
			result, err = p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
			if err == nil || f.auths != 1 || len(j.record(p.lease.selection.key).SIMChallenges) != 1 {
				t.Fatal("challenge replayed")
			}
			assertNoAKA(t, result)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("private error leaked")
			}
		})
	}
}
func TestCellularSIMCleanupFailureWithholdsIdentityAndKeys(t *testing.T) {
	for _, mode := range []string{"read-close", "auth-close", "release", "open"} {
		t.Run(mode, func(t *testing.T) {
			p, f, _, j := simFixture(t)
			var card CardIdentity
			if mode == "auth-close" {
				var err error
				card, err = p.ReadCard(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				f.onAuth = func() { f.mu.Lock(); f.failClose = true; f.mu.Unlock() }
			}
			if mode == "read-close" {
				f.failClose = true
			}
			if mode == "release" {
				f.failRelease = true
			}
			if mode == "open" {
				f.failOpen = true
			}
			if mode == "auth-close" {
				result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
				if !errors.Is(err, ErrUncertain) {
					t.Fatal("unsafe auth accepted", err)
				}
				assertNoAKA(t, result)
			} else {
				identity, err := p.ReadCard(context.Background())
				if !errors.Is(err, ErrUncertain) || identity.Application() != "" {
					t.Fatal("unsafe read accepted", err)
				}
			}
			record := j.record(p.lease.selection.key)
			if record.SIMPending || !record.SIMUncertain || len(p.retained) != 1 {
				t.Fatal("uncertainty not retained")
			}
			if err := p.lease.Close(context.Background()); !errors.Is(err, ErrUncertain) {
				t.Fatal("unsafe lease closed", err)
			}
			if err := p.lease.pool.Recover(context.Background(), p.lease.selection); !errors.Is(err, ErrUncertain) {
				t.Fatal("unsafe recovery accepted", err)
			}
		})
	}
}
func TestCellularSIMCancellationRetainsWorkerFenceUntilCleanup(t *testing.T) {
	p, f, _, j := simFixture(t)
	card, err := p.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.authEntered = make(chan struct{}, 1)
	f.authRelease = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		result, err := p.Authenticate(ctx, card.Subscriber(), vowifi.AKAChallenge{})
		assertNoAKA(t, result)
		done <- err
	}()
	select {
	case <-f.authEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("AUTH not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation blocked")
	}
	if !j.record(p.lease.selection.key).SIMPending {
		t.Fatal("pending lost before late reply")
	}
	if err := p.lease.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("lease closed during AUTH")
	}
	other, _ := newSIMProvider(p.lease, f.open)
	if _, err := other.ReadCard(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("parallel SIM operation started", err)
	}
	close(f.authRelease)
	awaitClaim(t, j, p.lease.selection.key, func(r claimRecord) bool { return !r.SIMPending })
	if j.record(p.lease.selection.key).SIMUncertain {
		t.Fatal("known late response mislabeled unknown")
	}
	if err := p.lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCellularSIMDurableJournalFailurePreventsOpeningCard(t *testing.T) {
	p, f, _, j := simFixture(t)
	j.mu.Lock()
	j.fail = true
	j.mu.Unlock()
	if _, err := p.ReadCard(context.Background()); !errors.Is(err, ErrJournal) || f.opened != 0 {
		t.Fatal("card opened without durable intent", err)
	}
}
func TestCellularSIMPrivateValuesAreRedacted(t *testing.T) {
	p, _, _, _ := simFixture(t)
	card, err := p.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{p, card, p.lease.selection.snapshot.Binding, uimCard{USIM: usimPrefix}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			printed := fmt.Sprintf(format, value)
			if strings.Contains(printed, card.Subscriber().IMSI) || strings.Contains(printed, card.PrivateIdentity()) || strings.Contains(printed, "A000") {
				t.Fatal("private fmt data leaked")
			}
		}
	}
	data, err := json.Marshal(card)
	if err != nil || string(data) != "{}" {
		t.Fatal("identity serialized implicitly")
	}
}

func TestCellularSIMCancelledBeforeStartDoesNotOpenCard(t *testing.T) {
	p, f, _, j := simFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ReadCard(ctx); !errors.Is(err, context.Canceled) || f.opened != 0 || j.record(p.lease.selection.key).SIMPending {
		t.Fatal("cancelled caller opened a SIM transaction", err)
	}
}
func TestCellularSIMTwoModemsProceedIndependently(t *testing.T) {
	p, f, b, j := simFixture(t)
	first, err := p.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.authEntered = make(chan struct{}, 1)
	f.authRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		result, err := p.Authenticate(context.Background(), first.Subscriber(), vowifi.AKAChallenge{})
		clearCardAKA(&result)
		done <- err
	}()
	select {
	case <-f.authEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first modem did not start")
	}
	selection, err := p.lease.pool.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/1")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := p.lease.pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secondState := newFakeUIM(selection)
	secondProvider, _ := newSIMProvider(lease, secondState.open)
	second, err := secondProvider.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := secondProvider.Authenticate(context.Background(), second.Subscriber(), vowifi.AKAChallenge{})
	if err != nil {
		t.Fatal("other modem blocked", err)
	}
	clearCardAKA(&result)
	if !j.record(p.lease.selection.key).SIMPending || j.record(selection.key).SIMPending || b.count("disconnect") != 0 {
		t.Fatal("cross-modem lifecycle interference")
	}
	close(f.authRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type scriptAPDU struct {
	uimSession
	send func([]byte) ([]byte, error)
}

func (s scriptAPDU) APDU(_ context.Context, _, _ byte, command []byte) ([]byte, error) {
	return s.send(command)
}
func TestCellularSIMAPDUChainsStayOnOwnedChannelAndNeverReplayAUTH(t *testing.T) {
	count := 0
	session := scriptAPDU{send: func(command []byte) ([]byte, error) {
		count++
		if command[0] != 0x40 {
			t.Fatal("GET RESPONSE used basic/wrong channel")
		}
		switch count {
		case 1:
			return []byte{1, 2, 0x61, 2}, nil
		case 2:
			if !bytes.Equal(command, []byte{0x40, 0xc0, 0, 0, 2}) {
				t.Fatal("wrong GET RESPONSE")
			}
			return []byte{3, 4, 0x9f, 1}, nil
		case 3:
			return []byte{5, 0x90, 0}, nil
		}
		t.Fatal("extra chained request")
		return nil, nil
	}}
	command := []byte{0, 0x88, 0, 0x81}
	raw, err := exchangeCard(context.Background(), session, 1, 4, command)
	if err != nil || !bytes.Equal(raw, []byte{1, 2, 3, 4, 5, 0x90, 0}) || command[0] != 0 {
		t.Fatal("chaining mismatch", err)
	}
	count = 0
	session.send = func(command []byte) ([]byte, error) { count++; return []byte{0x6c, 0x20}, nil }
	if _, err := exchangeCard(context.Background(), session, 1, 4, command); err != nil || count != 1 {
		t.Fatal("AUTH was replayed on corrected length")
	}
	count = 0
	session.send = func(command []byte) ([]byte, error) {
		count++
		if count == 1 {
			return []byte{0x6c, 3}, nil
		}
		if command[4] != 3 {
			t.Fatal("read Le not corrected")
		}
		return []byte{1, 2, 3, 0x90, 0}, nil
	}
	if _, err := exchangeCard(context.Background(), session, 1, 4, []byte{0, 0xb0, 0, 0, 0}); err != nil || count != 2 {
		t.Fatal("safe read correction failed", err)
	}
	count = 0
	session.send = func(command []byte) ([]byte, error) { count++; return []byte{0x61, 1}, nil }
	if _, err := exchangeCard(context.Background(), session, 1, 4, command); err == nil || count != 9 {
		t.Fatal("unbounded response chaining")
	}
}

func TestCellularSIMNewProviderCannotReplayLeaseChallenge(t *testing.T) {
	p, f, _, _ := simFixture(t)
	card, err := p.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	clearCardAKA(&result)
	other, _ := newSIMProvider(p.lease, f.open)
	card, err = other.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err = other.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
	if !errors.Is(err, ErrSIMReplay) || f.auths != 1 {
		t.Fatal("new adapter replayed persisted challenge", err)
	}
	assertNoAKA(t, result)
}
func TestCellularSIMRestartCannotForgetPendingOrUnknownTransactions(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			p, _, b, j := simFixture(t)
			c := p.lease.claim
			c.mu.Lock()
			next := c.record
			next.SIMPending = !unknown
			next.SIMUncertain = unknown
			if err := p.lease.pool.saveLocked(context.Background(), p.lease.selection.key, c, next); err != nil {
				t.Fatal(err)
			}
			// Synthetic crash: release only the process lock, never clear the journal.
			_ = c.lock.Close()
			c.mu.Unlock()
			restarted, _ := NewBearerPool(b, j, func(context.Context, Selection) error { return nil })
			selection, err := restarted.Select(context.Background(), p.lease.selection.snapshot.Path)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Recover(context.Background(), selection); !errors.Is(err, ErrUncertain) {
				t.Fatal("pending SIM state silently recovered", err)
			}
			if _, err := restarted.Acquire(context.Background(), selection, BearerOptions{}); err == nil {
				t.Fatal("pending SIM state silently replaced")
			}
			if b.count("disconnect") != 0 || b.count("delete") != 0 {
				t.Fatal("restart modified a fenced bearer")
			}
		})
	}
}
