//go:build linux

package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"vocat/internal/modem"
)

const recoveryCard = "8901000000000000001"
const recoverySubscriber = "310260000000001"
const recoveryEquipment = "123456789012345"

type recoveryAT struct {
	iccid, imsi, equipment, rows string
	mode                         int
	resets, closes               int
	resetErr, closeErr           error
	readErrorCommand             string
}

func (c *recoveryAT) Execute(ctx context.Context, command string) (modem.Response, error) {
	if err := ctx.Err(); err != nil {
		return modem.Response{}, err
	}
	if command == c.readErrorCommand {
		return modem.Response{}, modem.ErrCommandTimeout
	}
	r := modem.Response{Final: "OK"}
	switch command {
	case "AT+CCID":
		r.Lines = []string{"+CCID: " + c.iccid}
	case "AT+CIMI":
		r.Lines = []string{c.imsi}
	case "AT+CGSN":
		r.Lines = []string{c.equipment}
	case "AT+CFUN?":
		r.Lines = []string{fmt.Sprintf("+CFUN: %d", c.mode)}
	case "AT+CLCC":
		if c.rows != "" {
			r.Lines = []string{c.rows}
		}
	case "AT+CFUN=1,1":
		c.resets++
		return r, c.resetErr
	default:
		return modem.Response{}, errors.New("unexpected recovery command")
	}
	return r, nil
}
func (c *recoveryAT) WaitURC(context.Context, func(string) bool) (string, error) { return "", io.EOF }
func (c *recoveryAT) Close() error                                               { c.closes++; return c.closeErr }

type recoveryOpener struct {
	client *recoveryAT
	opens  int
}

func (o *recoveryOpener) Open(context.Context, modem.Port) (modem.Client, error) {
	o.opens++
	return o.client, nil
}

type recoveryClaim struct {
	attempted        bool
	begins, releases int
	err              error
	afterBegin       func()
}

func (c *recoveryClaim) Release(context.Context) error { c.releases++; return nil }
func (c *recoveryClaim) BeginRecovery(_ context.Context, _ modem.Candidate, equipment, iccid, imsi string) (bool, error) {
	c.begins++
	if c.err != nil {
		return false, c.err
	}
	if equipment != recoveryEquipment || iccid != recoveryCard || imsi != recoverySubscriber {
		return false, ErrSIMObservation
	}
	if c.attempted {
		return false, nil
	}
	c.attempted = true
	if c.afterBegin != nil {
		c.afterBegin()
	}
	return true, nil
}

type lostRecoveryData struct {
	fakeQMIDataSession
	lost bool
}

func (s *lostRecoveryData) TransportLost() bool { return s.lost }

type recoveryFixture struct {
	m         *Manager
	state     *managedDevice
	claim     *recoveryClaim
	at        *recoveryAT
	opener    *recoveryOpener
	session   *lostRecoveryData
	candidate modem.Candidate
}

func newRecoveryFixture() recoveryFixture {
	p := modem.Port{Path: "/dev/recovery-secondary", Role: modem.PortRoleModem}
	primary := modem.Port{Path: "/dev/recovery-primary", Role: modem.PortRoleAT}
	candidate := modem.Candidate{ID: "original", USBPath: "/synthetic/usb/1-2", USBGeneration: "1:7", QMIControl: "/dev/original-qmi", NetworkInterface: "fixture0", ATPort: primary, Ports: []modem.Port{primary, p}}
	at := &recoveryAT{iccid: recoveryCard, imsi: recoverySubscriber, equipment: recoveryEquipment, mode: 1, rows: "+CLCC: 1,1,0,1,0"}
	opener := &recoveryOpener{client: at}
	claim := &recoveryClaim{}
	session := &lostRecoveryData{lost: true}
	state := &managedDevice{candidate: candidate, discovered: true, dataSession: session, dataSessionHandle: 42}
	m := &Manager{started: true, devices: map[string]*managedDevice{candidate.ID: state}, opener: opener, commandTimeout: time.Second, longTimeout: time.Second, allowDataRecovery: func(context.Context, modem.Candidate) bool { return true }}
	guard := &simWriteGuard{manager: m, binding: simWriteBinding{candidate: candidate, physicalPinned: true, iccid: recoveryCard, imsi: recoverySubscriber}}
	state.dataOwner = &ownedNetwork{candidate: candidate, backend: "qmi", claim: claim, guard: guard, handleKnown: true}
	m.retainDataClaim(candidate, claim)
	return recoveryFixture{m, state, claim, at, opener, session, candidate}
}

func (f recoveryFixture) recover() (bool, error) {
	return f.m.recoverOwnedQMITransport(context.Background(), f.candidate.ID, "/dev/recovery-secondary", func(modem.Candidate, modem.Port) error { return nil })
}

func TestDataRecoverySubmitsOnceAndRetainsOriginalClaimUntilGenerationLoss(t *testing.T) {
	f := newRecoveryFixture()
	if submitted, err := f.recover(); !submitted || !errors.Is(err, ErrDataCleanupPending) {
		t.Fatal(submitted, err)
	}
	if f.at.resets != 1 || f.at.closes != 0 || f.claim.releases != 0 || !f.claim.attempted || !f.m.OwnsDataClaim(f.candidate) {
		t.Fatal("restart released the original capability")
	}
	if submitted, err := f.recover(); submitted || !errors.Is(err, ErrDataCleanupPending) || f.at.resets != 1 || f.opener.opens != 1 {
		t.Fatal("restart was replayed", err)
	}
	f.state.discovered = false
	if err := f.m.StopOwnedNetwork(context.Background(), f.candidate.ID); err != nil {
		t.Fatal(err)
	}
	if f.claim.releases != 1 || f.at.closes != 1 || f.state.dataOwner != nil || f.m.OwnsDataClaim(f.candidate) {
		t.Fatal("old-generation cleanup did not release its original resources")
	}
}

func TestDataRecoveryRejectsUnconfirmedOrChangedOwnership(t *testing.T) {
	for _, scenario := range []string{"disabled", "healthy transport", "unknown start", "new generation", "no recovery claim", "radio off", "voice call", "malformed calls", "changed card", "changed subscriber", "changed equipment", "changed policy", "journal failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveryFixture()
			switch scenario {
			case "disabled":
				f.m.allowDataRecovery = nil
			case "healthy transport":
				f.session.lost = false
			case "unknown start":
				f.state.dataOwner.handleKnown = false
			case "new generation":
				f.state.candidate.USBGeneration = "1:8"
			case "no recovery claim":
				f.state.dataOwner.claim = &fakeDurableDataClaim{}
			case "radio off":
				f.at.mode = 4
			case "voice call":
				f.at.rows = "+CLCC: 1,1,0,0,0"
			case "malformed calls":
				f.at.rows = "+CLCC: incomplete"
			case "changed card":
				f.at.iccid = "8901000000000000002"
			case "changed subscriber":
				f.at.imsi = "310260000000002"
			case "changed equipment":
				f.at.equipment = "123456789012346"
			case "changed policy":
				f.state.dataOwner.guard.binding.policyPinned = true
				f.state.dataOwner.guard.binding.policyICCID = recoveryCard
				f.m.legacyWriteICCID = func(context.Context, modem.Candidate) (string, error) { return "replacement", nil }
			case "journal failure":
				f.claim.err = errors.New("storage unavailable")
			}
			if submitted, err := f.recover(); submitted || err == nil || f.at.resets != 0 || f.claim.releases != 0 {
				t.Fatal("unsafe restart or release", submitted, err)
			}
		})
	}
}

func TestDataRecoveryRechecksIdentityAndCallsAfterDurableIntent(t *testing.T) {
	for _, scenario := range []string{"card", "equipment", "generation", "voice", "RF", "revoked permission"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveryFixture()
			f.claim.afterBegin = func() {
				switch scenario {
				case "revoked permission":
					f.m.allowDataRecovery = func(context.Context, modem.Candidate) bool { return false }
				case "card":
					f.at.iccid = "8901000000000000002"
				case "equipment":
					f.at.equipment = "123456789012346"
				case "generation":
					f.state.candidate.USBGeneration = "1:8"
				case "voice":
					f.at.rows = "+CLCC: 1,1,0,0,0"
				case "RF":
					f.at.mode = 4
				}
			}
			if submitted, err := f.recover(); submitted || err == nil || f.at.resets != 0 || !f.claim.attempted {
				t.Fatal("post-intent change reached reset", err)
			}
			f.at.iccid, f.at.equipment, f.at.rows, f.at.mode = recoveryCard, recoveryEquipment, "", 1
			f.state.candidate = f.candidate
			f.m.allowDataRecovery = func(context.Context, modem.Candidate) bool { return true }
			if submitted, _ := f.recover(); submitted || f.at.resets != 0 {
				t.Fatal("consumed intent replayed")
			}
		})
	}
}

func TestDataRecoveryUnknownResetReplyKeepsSerialAndOriginalFence(t *testing.T) {
	f := newRecoveryFixture()
	f.at.resetErr = modem.ErrCommandTimeout
	if submitted, err := f.recover(); !submitted || !errors.Is(err, ErrDataCleanupPending) {
		t.Fatal(submitted, err)
	}
	if submitted, _ := f.recover(); submitted || f.at.resets != 1 || f.at.closes != 0 || f.claim.releases != 0 {
		t.Fatal("unknown reset replayed or late serial ownership discarded")
	}
	f.state.discovered = false
	f.at.closeErr = errors.New("serial close unconfirmed")
	if err := f.m.StopOwnedNetwork(context.Background(), f.candidate.ID); !errors.Is(err, ErrDataCleanupPending) || f.claim.releases != 0 {
		t.Fatal("serial close failure released fence", err)
	}
	f.at.closeErr = nil
	if err := f.m.StopOwnedNetwork(context.Background(), f.candidate.ID); err != nil || f.claim.releases != 1 {
		t.Fatal("original close acknowledgement did not release", err)
	}
}

func TestDataRecoveryUnknownPreflightReplyCannotBeReopenedOrReused(t *testing.T) {
	f := newRecoveryFixture()
	f.at.readErrorCommand = "AT+CGSN"
	if submitted, err := f.recover(); submitted || err == nil || f.at.closes != 0 || f.claim.attempted {
		t.Fatal("unknown preflight response discarded or restart authorized", err)
	}
	f.at.readErrorCommand = ""
	if submitted, _ := f.recover(); submitted || f.opener.opens != 1 || f.at.resets != 0 {
		t.Fatal("unknown serial dialogue reopened")
	}
	f.state.discovered = false
	if err := f.m.StopOwnedNetwork(context.Background(), f.candidate.ID); err != nil || f.at.closes != 1 || f.claim.releases != 1 {
		t.Fatal("old-generation cleanup did not close its original probe", err)
	}
}

func TestDataRecoveryPortMustBeExplicitSecondaryAndOriginalTopology(t *testing.T) {
	f := newRecoveryFixture()
	for _, path := range []string{"", "relative", "/dev/recovery-primary", "/dev/unrelated"} {
		if _, err := dataRecoveryPort(f.candidate, path); err == nil {
			t.Fatal("unsafe port accepted", path)
		}
	}
	root := t.TempDir()
	usb := filepath.Join(root, "devices", "usb1", "1-2")
	parent := filepath.Join(usb, "1-2:1.3")
	class := filepath.Join(root, "class", "tty", "ttyRECOVERY")
	for _, path := range []string{parent, class} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, text := range map[string]string{"busnum": "1\n", "devnum": "7\n"} {
		if err := os.WriteFile(filepath.Join(usb, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(parent, filepath.Join(class, "device")); err != nil {
		t.Fatal(err)
	}
	f.candidate.USBPath = usb
	if err := verifyDataRecoveryTopology(f.candidate, "ttyRECOVERY", root); err != nil {
		t.Fatal("original topology denied", err)
	}
	f.candidate.USBGeneration = "1:8"
	if err := verifyDataRecoveryTopology(f.candidate, "ttyRECOVERY", root); err == nil {
		t.Fatal("recycled USB instance accepted")
	}
	f.candidate.USBGeneration = "1:7"
	f.candidate.USBPath = filepath.Join(root, "devices", "usb1", "1-20")
	if err := verifyDataRecoveryTopology(f.candidate, "ttyRECOVERY", root); err == nil {
		t.Fatal("another physical device accepted")
	}
}

func TestDataTransportLossRequiresTerminalSocketError(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("connection closed"), errors.New("protocol error")} {
		s := &productionQMIDataSession{}
		s.noteTransportLoss(err)
		if s.TransportLost() {
			t.Fatal("unproven transport loss", err)
		}
	}
	for _, err := range []error{io.EOF, net.ErrClosed, &net.OpError{Op: "write", Net: "unix", Err: syscall.EPIPE}, fmt.Errorf("wrapped: %w", syscall.ECONNRESET)} {
		s := &productionQMIDataSession{}
		s.noteTransportLoss(err)
		if !s.TransportLost() {
			t.Fatal("terminal original socket error not retained", err)
		}
	}
	a, b := &lostRecoveryData{lost: true}, &lostRecoveryData{}
	dual := &dualQMIDataSession{parts: [2]qmiDataSession{a, b}, attempted: [2]bool{true, true}}
	if dual.TransportLost() {
		t.Fatal("healthy family was classified lost")
	}
	b.lost = true
	if !dual.TransportLost() {
		t.Fatal("both original family transports were not classified lost")
	}
	dual.attempted = [2]bool{}
	if dual.TransportLost() {
		t.Fatal("no original active family was classified lost")
	}
}
