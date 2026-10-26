package hostims

import (
	"context"
	"time"
)

type modemEnabler interface {
	Enable(context.Context, string, string) error
}

// The caller holds the per-equipment process/journal gate. Enabling the radio
// is persisted user intent, not a temporary RF checkpoint owned by the bearer.
// It never resets the modem, switches the SIM, or opens a raw QMI transport.
func (p *BearerPool) waitRegistered(ctx context.Context, s Selection, initial modemSnapshot) (modemSnapshot, error) {
	current := initial
	enableSent := false
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		switch current.State {
		case 8, 11:
			return current, nil
		case 9, 10:
			return modemSnapshot{}, ErrBusy
		case 3:
			if !enableSent {
				backend, ok := p.backend.(modemEnabler)
				if !ok {
					return modemSnapshot{}, ErrSelection
				}
				if p.authorize(ctx, s) != nil {
					return modemSnapshot{}, ErrDenied
				}
				if _, err := p.verify(ctx, s); err != nil {
					return modemSnapshot{}, err
				}
				enableSent = true
				if err := backend.Enable(ctx, current.Owner, current.Path); err != nil {
					return modemSnapshot{}, err
				}
			}
		case 5, 6, 7: // Enabling, enabled, or searching; observe instead of resetting.
		default:
			return modemSnapshot{}, ErrSelection
		}
		select {
		case <-ctx.Done():
			return modemSnapshot{}, ctx.Err()
		case <-ticker.C:
		}
		var err error
		current, err = p.verify(ctx, s)
		if err != nil {
			return modemSnapshot{}, err
		}
	}
}
