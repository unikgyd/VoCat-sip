package device

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/modem"
	"vocat/internal/modemaudio"
	"vocat/internal/modulecall"
)

type voiceInspection struct{}

func (voiceInspection) Inspect(context.Context, modem.Candidate) (modemaudio.Info, error) {
	return modemaudio.Info{Pairing: "bound", BackendAvailable: true}, nil
}
func (voiceInspection) Open(context.Context, modem.Candidate, func() bool) (*modemaudio.Session, error) {
	return nil, modemaudio.ErrUnavailable
}

type voiceClient struct {
	revision    atomic.Uint64
	iccid, imsi string
	commands    []string
	rows        []string
	beforeGuard func(string)
}

func (c *voiceClient) CallEndRevision() uint64 { return c.revision.Load() }
func (c *voiceClient) ExecuteForCall(ctx context.Context, revision uint64, command string) (modem.Response, error) {
	if c.beforeGuard != nil {
		c.beforeGuard(command)
	}
	if c.CallEndRevision() != revision {
		return modem.Response{}, modem.ErrCallLifecycleChanged
	}
	return c.Execute(ctx, command)
}
func (c *voiceClient) Execute(ctx context.Context, command string) (modem.Response, error) {
	if err := ctx.Err(); err != nil {
		return modem.Response{}, err
	}
	c.commands = append(c.commands, command)
	switch command {
	case "AT+CCID":
		return modem.Response{Final: "OK", Lines: []string{"+CCID: " + c.iccid}}, nil
	case "AT+CIMI":
		return modem.Response{Final: "OK", Lines: []string{c.imsi}}, nil
	case "AT+CLCC":
		return modem.Response{Final: "OK", Lines: append([]string(nil), c.rows...)}, nil
	}
	return modem.Response{Final: "OK"}, nil
}
func (c *voiceClient) Close() error { c.revision.Add(1); return nil }
func (c *voiceClient) WaitURC(context.Context, func(string) bool) (string, error) {
	return "", errors.New("not used")
}
func voiceDevice(t *testing.T) (*Manager, *managedDevice, *voiceClient) {
	t.Helper()
	client := &voiceClient{iccid: "89880000000000000001", imsi: "515660000000001"}
	state := &managedDevice{discovered: true, client: client, candidate: modem.Candidate{ID: "physical-a", USBPath: "/synthetic/usb/1-2", USBGeneration: "1:4", VendorID: "2c7c", ProductID: "0125", ATPort: modem.Port{Path: "/synthetic/at-a"}}, snapshot: &Snapshot{SIMReady: true, IMSI: client.imsi, ICCID: client.iccid, RegistrationStatus: 1}}
	manager := &Manager{started: true, devices: map[string]*managedDevice{"physical-a": state}, audioInspector: voiceInspection{}, commandTimeout: time.Second}
	return manager, state, client
}
func TestUSBModuleVoiceDeviceFencesRealSIMBeforeAnyCallWrite(t *testing.T) {
	for _, change := range []string{"iccid", "imsi", "usb", "terminal-before-write", "none"} {
		t.Run(change, func(t *testing.T) {
			m, state, client := voiceDevice(t)
			line, err := m.AcquireModuleVoice(context.Background(), "physical-a")
			if err != nil {
				t.Fatal(err)
			}
			defer line.Close()
			switch change {
			case "iccid":
				client.iccid = "89880000000000000002"
			case "imsi":
				client.imsi = "515660000000002"
			case "usb":
				m.mu.Lock()
				state.candidate.USBGeneration = "1:5"
				m.mu.Unlock()
			case "terminal-before-write":
				client.beforeGuard = func(command string) {
					if strings.HasPrefix(command, "ATD") {
						client.revision.Add(1)
					}
				}
			}
			attempted, err := line.Execute(context.Background(), modulecall.Action{Kind: "dial", Number: "+15550000001"})
			if change == "none" {
				if !attempted || err != nil {
					t.Fatal(attempted, err)
				}
			} else {
				if attempted || err == nil {
					t.Fatal("unproven call was submitted", attempted, err)
				}
				for _, command := range client.commands {
					if strings.HasPrefix(command, "ATD") {
						t.Fatal("changed source reached ATD")
					}
				}
			}
		})
	}
}
func TestUSBModuleVoiceDeviceRejectsStaleOrAmbiguousCallIndex(t *testing.T) {
	m, _, client := voiceDevice(t)
	line, err := m.AcquireModuleVoice(context.Background(), "physical-a")
	if err != nil {
		t.Fatal(err)
	}
	defer line.Close()
	client.rows = []string{`+CLCC: 2,1,0,0,0,"15550000001",145`}
	rows, err := line.Read(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Number != "+15550000001" {
		t.Fatal(rows, err)
	}
	old := rows[0]
	old.Index = 1
	if attempted, err := line.Execute(context.Background(), modulecall.Action{Kind: "hangup", Expected: &old}); attempted || err == nil {
		t.Fatal(attempted, err)
	}
	if attempted, err := line.Execute(context.Background(), modulecall.Action{Kind: "hangup", Expected: &rows[0]}); !attempted || err != nil {
		t.Fatal(attempted, err)
	}
	if client.commands[len(client.commands)-1] != "AT+CHLD=12" {
		t.Fatal("did not release the selected call index", client.commands)
	}
	client.rows = append(client.rows, `+CLCC: 3,1,5,0,0,"15550000002",145`)
	if attempted, err := line.Execute(context.Background(), modulecall.Action{Kind: "dtmf", Expected: &rows[0], Digit: '1'}); attempted || err == nil {
		t.Fatal("ambiguous key submission", attempted, err)
	}
}
func TestUSBModuleVoiceDeviceReservationBlocksRawCallCommands(t *testing.T) {
	m, state, _ := voiceDevice(t)
	line, err := m.AcquireModuleVoice(context.Background(), "physical-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.AcquireModuleVoice(context.Background(), "physical-a"); !errors.Is(err, modulecall.ErrBusy) {
		t.Fatal("duplicate physical reservation", err)
	}
	if _, err = m.ExecuteAT(context.Background(), "physical-a", "ATH"); !errors.Is(err, modulecall.ErrBusy) {
		t.Fatal("raw AT bypassed the call owner", err)
	}
	if _, err = m.ExecuteAT(context.Background(), "physical-a", "AT+CLCC"); err != nil {
		t.Fatal("read-only CLCC was blocked", err)
	}
	if err = m.Reboot(context.Background(), "physical-a"); !errors.Is(err, modulecall.ErrBusy) {
		t.Fatal("reboot bypassed the owner", err)
	}
	if _, err = m.SetFlight(context.Background(), "physical-a", true); !errors.Is(err, modulecall.ErrBusy) {
		t.Fatal("RF-off bypassed the owner", err)
	}
	if _, err = m.openEuiccOnceAID(context.Background(), "physical-a", isdRAID); !errors.Is(err, modulecall.ErrBusy) {
		t.Fatal("SIM maintenance bypassed the owner", err)
	}
	line.Close()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if state.voiceLease != nil {
		t.Fatal("confirmed source reservation was not released")
	}
}

func TestUSBModuleVoiceMaintenanceCannotBypassPhysicalOwner(t *testing.T) {
	m, state, client := voiceDevice(t)
	m.longTimeout = time.Second
	state.candidate.Product = "EC25"
	line, err := m.AcquireModuleVoice(context.Background(), "physical-a")
	if err != nil {
		t.Fatal(err)
	}
	defer line.Close()
	ctx := context.Background()
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"sensitive AT", func() error { _, err := m.ExecuteSensitiveAT(ctx, "physical-a", "ATH"); return err }},
		{"cellular IMS reboot", func() error { _, err := m.SetCellularIMS(ctx, "physical-a", CellularIMSModeForceEnabled); return err }},
		{"SIM reset", func() error { return m.ResetSIM(ctx, "physical-a") }},
		{"USB mode by device", func() error { _, err := m.SetUSBNetMode(ctx, "physical-a", 1); return err }},
		{"USB mode by port", func() error { _, err := m.SetUSBNetModeByPort(ctx, state.candidate.ATPort.Path, 1); return err }},
		{"operator scan", func() error { _, err := m.ScanOperators(ctx, "physical-a"); return err }},
		{"operator change", func() error { _, err := m.SetOperatorSelection(ctx, "physical-a", true, "", nil); return err }},
		{"operator reconnect", func() error { _, err := m.ReRegisterOperator(ctx, "physical-a"); return err }},
		{"APN attach preparation", func() error { return m.PrepareRegistration(ctx, "physical-a", "internet", "IP") }},
		{"USSD", func() error { _, err := m.USSD(ctx, "physical-a", "*123#"); return err }},
		{"cell lock", func() error {
			_, err := m.SetCellLock(ctx, "physical-a", &CellLockTarget{EARFCN: 1650, PCI: 0})
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			before := len(client.commands)
			if err := operation.run(); !errors.Is(err, modulecall.ErrBusy) {
				t.Fatal("maintenance did not respect the physical voice owner", err)
			}
			if len(client.commands) != before {
				t.Fatal("maintenance wrote to a reserved serial session", client.commands[before:])
			}
		})
	}
}
