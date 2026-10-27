package hostims

import (
	"context"
	"errors"

	"vocat/internal/vowifi"
)

var _ vowifi.SMSCenterReader = (*PreparedAccess)(nil)

// ReadSMSCenter stays on this access owner's original UIM lease and logical
// channel. The device name cannot select another modem or fall back to raw AT.
// Re-read provisioning so an empty cached record is not a permanent failure.
func (a *PreparedAccess) ReadSMSCenter(ctx context.Context, _ string) (string, error) {
	if a == nil {
		return "", ErrHostAccessUnavailable
	}
	if err := a.Verify(ctx, a.CardIdentity().Subscriber()); err != nil {
		return "", err
	}
	center, err := runPreparedAccess(a, ctx, func(worker context.Context) (string, error) {
		if err := a.readyError(); err != nil {
			return "", err
		}
		card, err := a.deps.sim.ReadCard(worker)
		if err != nil {
			return "", err
		}
		defer clear(card.aid)
		if !sameCardIdentity(card, a.CardIdentity()) || !a.matchesIdentity(card.Subscriber()) {
			return "", ErrChanged
		}
		if err := a.continueOperation(worker); err != nil {
			return "", err
		}
		if err := a.readyError(); err != nil {
			return "", err
		}
		if card.subscriber.SMSC == "" {
			return "", ErrSIMSMSCenter
		}
		return card.subscriber.SMSC, nil
	}, nil)
	if err != nil && !errors.Is(err, ErrSIMSMSCenter) {
		a.stopped.Store(true)
	}
	return center, err
}
