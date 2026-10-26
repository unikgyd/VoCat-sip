package device

import (
	"context"
	"errors"
	"strings"

	"vocat/internal/modem"
)

type simWriteContextKey struct{}

// Background registration carries the identity selected by its originating
// request. It must not acquire a replacement card merely because that card has
// become the current policy by the time the worker obtains the device lock.
type simWriteBinding struct {
	iccid, imsi, policyICCID string
	candidate                modem.Candidate
	physicalPinned           bool
	policyPinned             bool
	quarantine               bool
}

type simWriteGuard struct {
	manager *Manager
	binding simWriteBinding
}

type simWriteIdentityReader interface {
	GetICCID(context.Context) (string, error)
	GetIMSI(context.Context) (string, error)
}

func sameSIMWritePhysical(a, b modem.Candidate) bool {
	return a.ID == b.ID && a.USBPath == b.USBPath && a.USBGeneration == b.USBGeneration &&
		a.QMIControl == b.QMIControl && a.ATPort.OpenPath() == b.ATPort.OpenPath() &&
		a.ReaderName == b.ReaderName && a.HardwareKind == b.HardwareKind
}

func (m *Manager) newSIMWriteGuard(ctx context.Context, candidate modem.Candidate) (*simWriteGuard, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	binding, pinned := ctx.Value(simWriteContextKey{}).(simWriteBinding)
	if !pinned && m.legacyWriteICCID == nil {
		return nil, nil // retain the standalone/legacy adapter contract
	}
	if binding.physicalPinned && !sameSIMWritePhysical(binding.candidate, candidate) {
		return nil, ErrSIMObservation
	}
	if m.legacyWriteICCID != nil {
		current, err := m.legacyWriteICCID(ctx, candidate)
		if err != nil {
			return nil, errors.Join(ErrSIMObservation, err)
		}
		current = strings.TrimSpace(current)
		if binding.policyPinned && binding.policyICCID != current || !binding.quarantine && current != "" && binding.iccid != "" && current != binding.iccid {
			return nil, ErrSIMObservation
		}
		binding.policyICCID, binding.policyPinned = current, true
		if binding.iccid == "" {
			binding.iccid = current
		}
	}
	binding.candidate, binding.physicalPinned = copyCandidate(candidate), true
	guard := &simWriteGuard{manager: m, binding: binding}
	if err := guard.current(ctx); err != nil {
		return nil, err
	}
	return guard, nil
}

func (g *simWriteGuard) context(ctx context.Context) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, simWriteContextKey{}, g.binding)
}

func (g *simWriteGuard) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	m := g.manager
	state, err := m.lookup(g.binding.candidate.ID)
	if err != nil || m.validateActive(g.binding.candidate.ID, state) != nil || !sameSIMWritePhysical(g.binding.candidate, m.candidateFor(state)) {
		return ErrSIMObservation
	}
	if g.binding.policyPinned && m.legacyWriteICCID != nil {
		iccid, err := m.legacyWriteICCID(ctx, g.binding.candidate)
		if err != nil {
			return errors.Join(ErrSIMObservation, err)
		}
		if strings.TrimSpace(iccid) != g.binding.policyICCID {
			return ErrSIMObservation
		}
	}
	return ctx.Err()
}

// A cached AT snapshot is never proof for a write. Re-read the SIM through the
// same transport under the caller's device/QMI lease, with ICCID bracketing the
// IMSI read, then recheck discovery and current application ownership.
func (g *simWriteGuard) verify(ctx context.Context, source any, requireEligible bool) error {
	if g == nil {
		return ctx.Err()
	}
	if g.binding.quarantine && requireEligible {
		return ErrRegionBlocked
	}
	if err := g.current(ctx); err != nil {
		return err
	}
	if g.binding.iccid == "" {
		return nil
	}
	reader, ok := source.(simWriteIdentityReader)
	if !ok {
		return ErrSIMObservation
	}
	iccid, err := reader.GetICCID(ctx)
	if err != nil || strings.TrimSpace(iccid) != g.binding.iccid {
		return ErrSIMObservation
	}
	imsi, err := reader.GetIMSI(ctx)
	imsi = strings.TrimSpace(imsi)
	if err != nil || len(imsi) < 10 || len(imsi) > 18 || requireEligible && IsPlaceholderIMSI(imsi) || strings.IndexFunc(imsi, func(r rune) bool { return r < '0' || r > '9' }) >= 0 || g.binding.imsi != "" && g.binding.imsi != imsi {
		return ErrSIMObservation
	}
	again, err := reader.GetICCID(ctx)
	if err != nil || strings.TrimSpace(again) != g.binding.iccid {
		return ErrSIMObservation
	}
	if requireEligible && RegionBlockReason(imsi) != "" {
		return ErrRegionBlocked
	}
	if err := g.current(ctx); err != nil {
		return err
	}
	g.binding.imsi = imsi
	return nil
}

type atSIMWriteReader struct{ client modem.Client }

func (r atSIMWriteReader) GetICCID(ctx context.Context) (string, error) {
	for _, command := range []string{"AT+CCID", "AT+QCCID", "AT+MCCID"} {
		response, err := r.client.Execute(ctx, command)
		if err == nil && response.OK() {
			if value := parseICCIDIdentifier(response, []string{"+CCID:", "+QCCID:", "+MCCID:"}, 10, 22); value != "" {
				return value, nil
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", ErrSIMObservation
}

func (r atSIMWriteReader) GetIMSI(ctx context.Context) (string, error) {
	response, err := r.client.Execute(ctx, "AT+CIMI")
	if err != nil || !response.OK() {
		return "", ErrSIMObservation
	}
	return parseIdentifier(response, []string{"+CIMI:"}, 10, 18), nil
}

func contextForSIMWrite(ctx context.Context, iccid, imsi string) (context.Context, error) {
	iccid, imsi = strings.TrimSpace(iccid), strings.TrimSpace(imsi)
	if len(iccid) < 10 || len(iccid) > 22 || len(imsi) < 10 || len(imsi) > 18 ||
		strings.IndexFunc(iccid+imsi, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return nil, errors.New("a verified SIM identity is required for modem control")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, simWriteContextKey{}, simWriteBinding{iccid: iccid, imsi: imsi}), nil
}

// SetFlightForSIM is used by independent access activation and reconnect. The
// hardware boundary verifies this exact subscription even if a hot swap occurs
// after application admission. RF-off may quarantine a region-blocked SIM;
// RF-on additionally requires the existing regional admission check.
func (m *Manager) SetFlightForSIM(ctx context.Context, id, iccid, imsi string, enabled bool) (FlightResult, error) {
	bound, err := contextForSIMWrite(ctx, iccid, imsi)
	if err != nil {
		return FlightResult{}, err
	}
	return m.SetFlight(bound, id, enabled)
}

// Regional quarantine cannot wait for a blocked replacement SIM to become an
// eligible saved access policy. It still verifies that exact live SIM and USB
// generation, respects the Host IMS ownership guard, and permits RF-off only.
func (m *Manager) QuarantineSIM(ctx context.Context, id, iccid, imsi string) (FlightResult, error) {
	if RegionBlockReason(strings.TrimSpace(imsi)) == "" {
		return FlightResult{}, errors.New("regional quarantine requires a blocked SIM")
	}
	bound, err := contextForSIMWrite(ctx, iccid, imsi)
	if err != nil {
		return FlightResult{}, err
	}
	binding := bound.Value(simWriteContextKey{}).(simWriteBinding)
	binding.quarantine = true
	return m.SetFlight(context.WithValue(bound, simWriteContextKey{}, binding), id, true)
}

// RF may already be online when the previous NAS attempt failed or its SIM
// was replaced. Retry registration without toggling RF or waiting in the
// application's per-line writer gate. AT-only modems keep their own automatic
// baseband registration; this worker is specific to native WWAN NAS.
func (m *Manager) ReconcileRegistrationForSIM(ctx context.Context, id, iccid, imsi string) error {
	bound, err := contextForSIMWrite(ctx, iccid, imsi)
	if err != nil {
		return err
	}
	state, err := m.lookup(id)
	if err != nil {
		return err
	}
	candidate := m.candidateFor(state)
	if !isNativeQMICandidate(candidate) {
		return nil
	}
	if _, err := m.newSIMWriteGuard(bound, candidate); err != nil {
		return err
	}
	m.startNativeQMIRegistrationReconcile(id, bound)
	return nil
}
