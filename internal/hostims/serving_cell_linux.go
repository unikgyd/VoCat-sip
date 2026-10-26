//go:build linux

package hostims

import (
	"context"

	"github.com/godbus/dbus/v5"
)

const (
	mmLocation     = mmModem + ".Location"
	mm3gpp         = mmModem + ".Modem3gpp"
	mmLocation3gpp = uint32(1)
	mmAccessLTE    = uint32(1 << 14)
)

// NewModemManagerServingCellSource uses the exact MM backend that owns lease,
// not a separately opened bus, replacement service, caller-supplied location,
// AT fallback, or the SIM's home PLMN. It never activates the service.
func NewModemManagerServingCellSource(lease *BearerLease) (*ServingCellSource, error) {
	if lease == nil || lease.pool == nil {
		return nil, ErrSelection
	}
	backend, ok := lease.pool.backend.(*modemManagerBackend)
	if !ok || backend == nil || backend.connection == nil || !backend.connection.Connected() {
		return nil, ErrSelection
	}
	return newServingCellSource(lease, backend.cachedServingCell)
}

func (b *modemManagerBackend) servingCellState(ctx context.Context, owner, path string) (servingCellSnapshot, error) {
	modem, err := b.properties(ctx, owner, path, mmModem)
	if err != nil {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	state, ok := modem["State"].Value().(int32)
	access, accessErr := mmUint(modem, "AccessTechnologies")
	if !ok || (state != 8 && state != 11) || accessErr != nil || access != mmAccessLTE {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	gpp, err := b.properties(ctx, owner, path, mm3gpp)
	if err != nil {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	registration, registrationErr := mmUint(gpp, "RegistrationState")
	operator, operatorErr := mmText(gpp, "OperatorCode")
	if registrationErr != nil || registration != 1 && registration != 5 || operatorErr != nil || !asciiDigits(operator, 5, 6) {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	location, err := b.properties(ctx, owner, path, mmLocation)
	if err != nil {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	capabilities, capabilitiesErr := mmUint(location, "Capabilities")
	enabled, enabledErr := mmUint(location, "Enabled")
	if capabilitiesErr != nil || enabledErr != nil || capabilities&mmLocation3gpp == 0 || enabled&mmLocation3gpp == 0 {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	return servingCellSnapshot{operator: operator, state: state, access: access,
		registration: registration, capabilities: capabilities, enabled: enabled}, nil
}

func (b *modemManagerBackend) cachedServingCell(ctx context.Context, owner, path string) (servingCellSnapshot, error) {
	if !validOwner(owner) || !objectPath(path, "Modem") {
		return servingCellSnapshot{}, ErrSelection
	}
	currentOwner, err := b.owner(ctx)
	if err != nil || currentOwner != owner {
		return servingCellSnapshot{}, ErrChanged
	}
	before, err := b.servingCellState(ctx, owner, path)
	if err != nil {
		return servingCellSnapshot{}, err
	}
	// Do not use the Location property: it may be disabled independently of
	// gathering, and GetLocation is the authenticated read-only location API.
	// Importantly, GetLocation reads MM's cache, not a fresh modem measurement.
	call := b.connection.Object(owner, dbus.ObjectPath(path)).CallWithContext(ctx, mmLocation+".GetLocation", 0)
	if call.Err != nil || len(call.Body) != 1 {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	values, ok := call.Body[0].(map[uint32]dbus.Variant)
	if !ok || len(values) > 16 {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	value, ok := values[mmLocation3gpp]
	if !ok || value.Signature().String() != "s" {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	raw, ok := value.Value().(string)
	if !ok {
		return servingCellSnapshot{}, ErrServingCellUnavailable
	}
	cell, err := parseCachedLTELocation(raw, before.operator)
	if err != nil {
		return servingCellSnapshot{}, err
	}
	after, err := b.servingCellState(ctx, owner, path)
	if err != nil || after != before {
		return servingCellSnapshot{}, ErrChanged
	}
	currentOwner, err = b.owner(ctx)
	if err != nil || currentOwner != owner {
		return servingCellSnapshot{}, ErrChanged
	}
	after.cell = cell
	return after, nil
}
