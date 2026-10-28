package hostims

import (
	"context"
	"strings"
	"time"
)

// Subscription distinguishes a positively observed empty physical SIM slot
// from unavailable metadata. It performs no RF, QMI or bearer mutation.
func (p *BearerPool) Subscription(ctx context.Context, target ModemTarget) (iccid, imsi string, err error) {
	if p == nil || !validModemTarget(target) {
		return "", "", ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	paths, err := p.backend.Modems(ctx)
	if err != nil {
		return "", "", err
	}
	if len(paths) == 0 || len(paths) > 64 {
		return "", "", ErrSelection
	}
	selected := ""
	for path, equipment := range paths {
		if !objectPath(path, "Modem") {
			return "", "", ErrSelection
		}
		if equipment == target.Equipment {
			if selected != "" {
				return "", "", ErrSelection
			}
			selected = path
		}
	}
	if selected == "" {
		return "", "", ErrSelection
	}
	first, err := p.backend.Modem(ctx, selected)
	if err != nil {
		return "", "", err
	}
	b := first.Binding
	if first.Path != selected || !validOwner(first.Owner) || b.Equipment != target.Equipment || b.Device == "" || len(b.Device) > 256 || b.PhysicalToken == "" ||
		!strings.HasPrefix(b.Physical, "/sys/devices/") || !samePhysicalTree(b.Physical, target.Physical) || first.QMIControl != target.QMIControl {
		return "", "", ErrChanged
	}
	again, err := p.backend.Modem(ctx, selected)
	if err != nil || ctx.Err() != nil || again.Owner != first.Owner || again.Path != first.Path || again.Binding != b || again.SIMMissing != first.SIMMissing || again.QMIControl != first.QMIControl {
		return "", "", ErrChanged
	}
	if first.SIMMissing {
		if b.SIMPath != "/" || b.ICCID != "" || b.IMSI != "" {
			return "", "", ErrSelection
		}
		return "", "", nil
	}
	if !b.valid() {
		return "", "", ErrSelection
	}
	return b.ICCID, b.IMSI, nil
}

type RadioObservation struct {
	ICCID              string
	IMSI               string
	IMEI               string
	Manufacturer       string
	Model              string
	Firmware           string
	OperatorCode       string
	OperatorName       string
	AccessTech         string
	RegistrationStatus int
	PSAttached         bool
	OperatingMode      int
	ModeKnown          bool
	FlightMode         bool
	SignalPercent      *int
	RSSIDBm            *int
}

type radioObserverBackend interface {
	ObserveModemRadio(ctx context.Context, path string) (RadioObservation, error)
}

type deviceInhibitorBackend interface {
	SyncDeviceInhibition(ctx context.Context, desired map[string]bool) error
}

func (p *BearerPool) ObserveRadio(ctx context.Context, target ModemTarget) (RadioObservation, error) {
	iccid, imsi, err := p.Subscription(ctx, target)
	if err != nil {
		return RadioObservation{}, err
	}
	obs := RadioObservation{
		ICCID:         iccid,
		IMSI:          imsi,
		IMEI:          target.Equipment,
		OperatingMode: 1,
		ModeKnown:     true,
	}
	observer, ok := p.backend.(radioObserverBackend)
	if !ok {
		return obs, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	paths, err := p.backend.Modems(ctx)
	if err != nil {
		return obs, nil
	}
	selected := ""
	for path, equipment := range paths {
		if equipment == target.Equipment {
			selected = path
			break
		}
	}
	if selected == "" {
		return obs, nil
	}
	radio, err := observer.ObserveModemRadio(ctx, selected)
	if err != nil {
		return obs, nil
	}
	radio.ICCID = iccid
	radio.IMSI = imsi
	if radio.IMEI == "" {
		radio.IMEI = target.Equipment
	}
	return radio, nil
}

func (p *BearerPool) SyncDeviceInhibition(ctx context.Context, desired map[string]bool) error {
	if p == nil {
		return ErrSelection
	}
	inhibitor, ok := p.backend.(deviceInhibitorBackend)
	if !ok {
		return nil
	}
	return inhibitor.SyncDeviceInhibition(ctx, desired)
}

