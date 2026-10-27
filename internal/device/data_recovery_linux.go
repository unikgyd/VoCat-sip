//go:build linux

package device

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vocat/internal/modem"
	"vocat/internal/modulecall"
)

type qmiDataTransportState interface{ TransportLost() bool }

type dataRecoveryAT struct {
	modem.Client
	uncertain bool
}

func (c *dataRecoveryAT) Execute(ctx context.Context, command string) (modem.Response, error) {
	response, err := c.Client.Execute(ctx, command)
	if isCommandInterrupted(err) {
		c.uncertain = true
	}
	if poisoned, ok := c.Client.(modem.PoisonedClient); ok && poisoned.Poisoned() {
		c.uncertain = true
	}
	return response, err
}

func (s *productionQMIDataSession) noteTransportLoss(err error) {
	// A timeout, protocol error or disconnected packet call is insufficient.
	// These are errors from the original socket, not a replacement proxy probe.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ENOTCONN) {
		s.transportLost.Store(true)
	}
}

func (s *productionQMIDataSession) TransportLost() bool { return s.transportLost.Load() }

// RecoverOwnedQMITransport is opt-in at the independent owner's entry point.
// It can consume only this live owner's durable intent, using a separately
// configured secondary AT port of the exact original USB instance and SIM.
// Claim release still requires ordinary cleanup after generation disappearance.
func (m *Manager) RecoverOwnedQMITransport(ctx context.Context, id, configuredPort string) (bool, error) {
	return m.recoverOwnedQMITransport(ctx, id, configuredPort, verifyDataRecoveryPort)
}

func (m *Manager) recoverOwnedQMITransport(ctx context.Context, id, configuredPort string, verifyPort func(modem.Candidate, modem.Port) error) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	state, err := m.lookup(id)
	if err != nil {
		return false, err
	}
	if err := lockMutexContext(ctx, &state.opMu); err != nil {
		return false, err
	}
	defer state.opMu.Unlock()
	if err := lockMutexContext(ctx, &state.dataMu); err != nil {
		return false, err
	}
	defer state.dataMu.Unlock()
	owner := state.dataOwner
	if owner == nil || owner.backend != "qmi" || !owner.handleKnown || owner.packetStopped ||
		owner.radioStopped || owner.guard == nil || owner.recoveryClient != nil ||
		!m.ownedNetworkPhysicalCurrent(state, owner) || m.allowDataRecovery == nil || !m.allowDataRecovery(ctx, owner.candidate) {
		return false, ErrDataCleanupPending
	}
	transport, ok := state.dataSession.(qmiDataTransportState)
	if !ok || !transport.TransportLost() {
		return false, ErrDataCleanupPending
	}
	claim, ok := owner.claim.(DataRecoveryClaim)
	if !ok || !m.OwnsDataClaim(owner.candidate) || m.requireNoModuleVoice(state) != nil {
		return false, ErrDataCleanupPending
	}
	port, err := dataRecoveryPort(owner.candidate, configuredPort)
	if err != nil || verifyPort(owner.candidate, port) != nil {
		return false, ErrSIMObservation
	}
	opened, err := m.opener.Open(ctx, port)
	if err != nil {
		return false, ErrDataCleanupPending
	}
	client := &dataRecoveryAT{Client: opened}
	retained := false
	defer func() {
		if !retained {
			if client.uncertain {
				owner.recoveryClient = client
			} else if err := client.Close(); err != nil {
				owner.recoveryClient = client
			}
		}
	}()
	if err := owner.guard.verify(ctx, atSIMWriteReader{client}, true); err != nil {
		return false, err
	}
	identity, err := client.Execute(ctx, "AT+CGSN")
	if err != nil || !identity.OK() {
		return false, ErrSIMObservation
	}
	equipment := parseIdentifier(identity, []string{"+CGSN:", "+GSN:"}, 15, 15)
	if err := m.recoveryRestartPreflight(ctx, client); err != nil {
		return false, err
	}
	if err := verifyPort(owner.candidate, port); err != nil || !m.ownedNetworkPhysicalCurrent(state, owner) || !m.allowDataRecovery(ctx, owner.candidate) {
		return false, ErrSIMObservation
	}
	admitted, err := claim.BeginRecovery(ctx, owner.candidate, equipment, owner.guard.binding.iccid, owner.guard.binding.imsi)
	if err != nil || !admitted {
		return false, errors.Join(ErrDataCleanupPending, err)
	}
	// Persist before the write. Even a rejected or timed-out reset consumes the
	// intent; neither a later stop request nor process restart may replay it.
	if err := owner.guard.verify(ctx, atSIMWriteReader{client}, true); err != nil {
		return false, err
	}
	again, err := client.Execute(ctx, "AT+CGSN")
	if err != nil || !again.OK() || parseIdentifier(again, []string{"+CGSN:", "+GSN:"}, 15, 15) != equipment {
		return false, ErrSIMObservation
	}
	if err := m.recoveryRestartPreflight(ctx, client); err != nil {
		return false, err
	}
	if err := verifyPort(owner.candidate, port); err != nil || !m.ownedNetworkPhysicalCurrent(state, owner) || !m.allowDataRecovery(ctx, owner.candidate) {
		return false, ErrSIMObservation
	}
	owner.recoveryClient, retained = client, true
	if m.logger != nil {
		m.logger.Warn("automatic original-modem data recovery submitted", "device_id", id, "category", "data_owner")
	}
	_, _ = client.Execute(ctx, "AT+CFUN=1,1")
	return true, ErrDataCleanupPending // a command response never proves cleanup
}

func (m *Manager) recoveryRestartPreflight(ctx context.Context, client modem.Client) error {
	if err := m.requireOnlineModemRestart(ctx, client); err != nil {
		return err
	}
	response, err := client.Execute(ctx, "AT+CLCC")
	if err != nil {
		return modulecall.ErrUncertain
	}
	rows, err := modulecall.ParseRecords(response)
	if err != nil || len(modulecall.VoiceRecords(rows)) != 0 {
		return modulecall.ErrBusy
	}
	return nil
}

func dataRecoveryPort(candidate modem.Candidate, configured string) (modem.Port, error) {
	if configured == "" || !filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
		return modem.Port{}, ErrSIMObservation
	}
	for _, port := range candidate.Ports {
		if configured != port.Path && configured != port.StablePath {
			continue
		}
		if port.Path == candidate.ATPort.Path || port.OpenPath() == candidate.ATPort.OpenPath() ||
			port.Role != modem.PortRoleAT && port.Role != modem.PortRoleModem {
			return modem.Port{}, ErrSIMObservation
		}
		return port, nil
	}
	return modem.Port{}, ErrSIMObservation
}

func verifyDataRecoveryPort(candidate modem.Candidate, port modem.Port) error {
	resolved, err := filepath.EvalSymlinks(port.OpenPath())
	if err != nil {
		return ErrSIMObservation
	}
	info, err := os.Stat(resolved)
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return ErrSIMObservation
	}
	primary, err := filepath.EvalSymlinks(candidate.ATPort.OpenPath())
	if err != nil || primary == resolved {
		return ErrSIMObservation
	}
	return verifyDataRecoveryTopology(candidate, filepath.Base(resolved), "/sys")
}

func verifyDataRecoveryTopology(candidate modem.Candidate, tty, sysRoot string) error {
	usb, err := filepath.EvalSymlinks(candidate.USBPath)
	if err != nil || candidate.USBGeneration == "" {
		return ErrSIMObservation
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class", "tty", tty, "device"))
	if err != nil || !strings.HasPrefix(parent, usb+string(filepath.Separator)) {
		return ErrSIMObservation
	}
	bus, err := os.ReadFile(filepath.Join(usb, "busnum"))
	if err != nil {
		return ErrSIMObservation
	}
	dev, err := os.ReadFile(filepath.Join(usb, "devnum"))
	if err != nil || strings.TrimSpace(string(bus))+":"+strings.TrimSpace(string(dev)) != candidate.USBGeneration {
		return ErrSIMObservation
	}
	return nil
}
