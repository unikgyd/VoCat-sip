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
