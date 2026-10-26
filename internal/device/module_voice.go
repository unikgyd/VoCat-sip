package device

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"vocat/internal/modem"
	"vocat/internal/modemaudio"
	"vocat/internal/modulecall"
)

type moduleAudioOpener interface {
	Open(context.Context, modem.Candidate, func() bool) (*modemaudio.Session, error)
}
type callControlClient interface {
	modem.Client
	CallEndRevision() uint64
	ExecuteForCall(context.Context, uint64, string) (modem.Response, error)
}

// moduleVoiceLease is opaque outside the device layer. The same opMu orders
// physical/SIM validation, CLCC validation and the guarded serial submission.
type moduleVoiceLease struct {
	manager         *Manager
	state           *managedDevice
	id, iccid, imsi string
	candidate       modem.Candidate
	client          callControlClient
	revision        uint64
	close           sync.Once
}

func (l *moduleVoiceLease) RecordingICCID() string { return l.iccid }

func (m *Manager) ModuleVoiceAvailable(ctx context.Context, id string) error {
	state, err := m.lookup(id)
	if err != nil {
		return err
	}
	m.mu.RLock()
	snapshot := state.snapshot
	valid := snapshot != nil && snapshot.SIMReady && snapshot.ICCID != "" && snapshot.IMSI != "" && !snapshot.FlightMode && !snapshot.RadioOff && (snapshot.RegistrationStatus == 1 || snapshot.RegistrationStatus == 5)
	var imsi string
	if snapshot != nil {
		imsi = snapshot.IMSI
	}
	m.mu.RUnlock()
	if !valid || IsPlaceholderIMSI(imsi) {
		return modulecall.ErrUnavailable
	}
	if RegionBlockReason(imsi) != "" {
		return ErrRegionBlocked
	}
	if _, ok := m.audioInspector.(moduleAudioOpener); !ok {
		return modulecall.ErrUnavailable
	}
	info, err := m.USBAudioBinding(ctx, id)
	if err != nil {
		return err
	}
	if info.Pairing != "bound" || !info.BackendAvailable {
		return modulecall.ErrUnavailable
	}
	return nil
}
func (m *Manager) AcquireModuleVoice(ctx context.Context, id string) (modulecall.Lease, error) {
	if err := m.ModuleVoiceAvailable(ctx, id); err != nil {
		return nil, err
	}
	state, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	if err = lockMutexContext(ctx, &m.uiccMu); err != nil {
		return nil, err
	}
	defer m.uiccMu.Unlock()
	if err = lockMutexContext(ctx, &state.opMu); err != nil {
		return nil, err
	}
	defer state.opMu.Unlock()
	if err = m.validateActive(id, state); err != nil {
		return nil, err
	}
	client, err := m.clientLocked(ctx, state, m.candidateFor(state))
	if err != nil {
		return nil, err
	}
	guarded, ok := client.(callControlClient)
	if !ok {
		return nil, modulecall.ErrUnavailable
	}
	m.mu.Lock()
	if state.voiceLease != nil || state.snapshot == nil {
		m.mu.Unlock()
		return nil, modulecall.ErrBusy
	}
	lease := &moduleVoiceLease{manager: m, state: state, id: id, iccid: state.snapshot.ICCID, imsi: state.snapshot.IMSI, candidate: copyCandidate(state.candidate), client: guarded, revision: guarded.CallEndRevision()}
	state.voiceLease = lease
	m.mu.Unlock()
	if err = lease.verifySIM(ctx); err != nil {
		lease.Close()
		return nil, err
	}
	return lease, nil
}
func (l *moduleVoiceLease) Current() bool {
	if poisoned, ok := l.client.(modem.PoisonedClient); ok && poisoned.Poisoned() {
		return false
	}
	m := l.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := l.state
	return m.started && m.devices[l.id] == s && s.discovered && s.voiceLease == l &&
		s.candidate.USBPath == l.candidate.USBPath && s.candidate.USBGeneration == l.candidate.USBGeneration && s.candidate.ATPort.OpenPath() == l.candidate.ATPort.OpenPath() &&
		s.snapshot != nil && s.snapshot.SIMReady && s.snapshot.ICCID == l.iccid && s.snapshot.IMSI == l.imsi &&
		l.client.CallEndRevision() == l.revision
}

func (m *Manager) requireNoModuleVoice(state *managedDevice) error {
	m.mu.RLock()
	busy := state.voiceLease != nil
	m.mu.RUnlock()
	if busy {
		return modulecall.ErrBusy
	}
	return nil
}
func (l *moduleVoiceLease) Close() {
	l.close.Do(func() {
		l.manager.mu.Lock()
		if l.state.voiceLease == l {
			l.state.voiceLease = nil
		}
		l.manager.mu.Unlock()
	})
}
func (l *moduleVoiceLease) verifySIM(ctx context.Context) error {
	if !l.Current() {
		return modulecall.ErrChanged
	}
	var iccid string
	for _, command := range []string{"AT+CCID", "AT+QCCID", "AT+MCCID"} {
		response, err := l.client.Execute(ctx, command)
		if err == nil && response.OK() {
			iccid = parseICCIDIdentifier(response, []string{"+CCID:", "+QCCID:", "+MCCID:"}, 18, 22)
			if iccid != "" {
				break
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	response, err := l.client.Execute(ctx, "AT+CIMI")
	if err != nil || !response.OK() {
		return modulecall.ErrChanged
	}
	imsi := parseIdentifier(response, []string{"+CIMI:"}, 10, 18)
	if iccid != l.iccid || imsi != l.imsi || !l.Current() {
		return modulecall.ErrChanged
	}
	if RegionBlockReason(imsi) != "" {
		return ErrRegionBlocked
	}
	return nil
}
func (l *moduleVoiceLease) readLocked(ctx context.Context) ([]modulecall.Record, error) {
	if !l.Current() {
		return nil, modulecall.ErrChanged
	}
	response, err := l.client.ExecuteForCall(ctx, l.revision, "AT+CLCC")
	if err != nil {
		return nil, err
	}
	if !l.Current() {
		return nil, modulecall.ErrChanged
	}
	return modulecall.ParseRecords(response)
}
func (l *moduleVoiceLease) Read(ctx context.Context) ([]modulecall.Record, error) {
	if err := lockMutexContext(ctx, &l.state.opMu); err != nil {
		return nil, err
	}
	defer l.state.opMu.Unlock()
	if l.state.client != l.client {
		return nil, modulecall.ErrChanged
	}
	return l.readLocked(ctx)
}
func (l *moduleVoiceLease) OpenAudio(ctx context.Context, current func() bool) (modulecall.Audio, error) {
	if !l.Current() || current == nil || !current() {
		return nil, modulecall.ErrChanged
	}
	opener, ok := l.manager.audioInspector.(moduleAudioOpener)
	if !ok {
		return nil, modulecall.ErrUnavailable
	}
	return opener.Open(ctx, l.candidate, func() bool { return l.Current() && current() })
}
func (l *moduleVoiceLease) Execute(ctx context.Context, a modulecall.Action) (bool, error) {
	if err := lockMutexContext(ctx, &l.state.opMu); err != nil {
		return false, err
	}
	defer l.state.opMu.Unlock()
	if l.state.client != l.client {
		return false, modulecall.ErrChanged
	}
	if err := l.verifySIM(ctx); err != nil {
		return false, err
	}
	rows, err := l.readLocked(ctx)
	if err != nil {
		return false, err
	}
	voices := modulecall.VoiceRecords(rows)
	command := ""
	if a.Kind == "dial" {
		if !modulecall.ValidNumber(a.Number) {
			return false, modulecall.ErrState
		}
		if len(voices) != 0 {
			return false, modulecall.ErrBusy
		}
		command = "ATD" + a.Number + ";"
	} else {
		if a.Expected == nil || len(voices) != 1 || voices[0] != *a.Expected || voices[0].Multiparty != 0 {
			return false, modulecall.ErrState
		}
		r := voices[0]
		switch a.Kind {
		case "answer":
			if r.Direction != 1 || r.State != 4 && r.State != 5 {
				return false, modulecall.ErrState
			}
			command = "ATA"
		case "hangup":
			command = "AT+CHLD=1" + strconv.Itoa(r.Index)
		case "dtmf":
			if r.State != 0 || !strings.ContainsRune("0123456789*#ABCD", rune(a.Digit)) {
				return false, modulecall.ErrState
			}
			command = "AT+VTS=" + string(a.Digit)
		default:
			return false, modulecall.ErrState
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	response, err := l.client.ExecuteForCall(ctx, l.revision, command)
	if errors.Is(err, modem.ErrCallLifecycleChanged) {
		return false, modulecall.ErrChanged
	}
	if err != nil {
		return true, err
	}
	if !response.OK() {
		return true, modulecall.ErrUncertain
	}
	return true, nil
}
