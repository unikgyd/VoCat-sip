package vowifi

import (
	"context"
	"errors"
	"sync"
	"time"
)

type tunnelAPNProvider interface{ TunnelAPN() string }

func (o *Orchestrator) ikeRetryTarget(candidate EPDGCandidate) IKERetryTarget {
	apn := "ims"
	if provider, ok := o.deps.Tunnel.(tunnelAPNProvider); ok {
		apn = provider.TunnelAPN()
	}
	return IKERetryTarget{Host: candidate.Host, PLMN: candidate.PLMN, APN: apn}
}

func ikeRetryStatusForError(err error) *IKERetryStatus {
	var blocked *IKERetryBlockedError
	if errors.As(err, &blocked) {
		status := blocked.Status
		return &status
	}
	if errors.Is(err, ErrIKERetryStore) {
		return &IKERetryStatus{Source: "store-unavailable", UntilCardEvent: true}
	}
	if errors.Is(err, ErrIKERetryBinding) {
		return &IKERetryStatus{Source: "binding-unavailable", UntilCardEvent: true}
	}
	return nil
}

func (o *Orchestrator) filterIKERetryPlan(ctx context.Context, card IKERetryCard, attempts []EPDGAttempt) ([]EPDGAttempt, error) {
	allowed := make([]EPDGAttempt, 0, len(attempts))
	var blocked error
	for _, attempt := range attempts {
		err := o.ikeRetry.Permit(ctx, card, o.ikeRetryTarget(attempt.Candidate), "")
		if err == nil {
			allowed = append(allowed, attempt)
			continue
		}
		if !errors.Is(err, ErrIKERetryBlocked) {
			return nil, err
		}
		blocked = errors.Join(blocked, err)
	}
	if len(allowed) == 0 && blocked != nil {
		return nil, blocked
	}
	return allowed, nil
}

func (o *Orchestrator) startGovernedTunnel(ctx context.Context, card IKERetryCard, candidate EPDGCandidate, request TunnelRequest) (TunnelSession, error) {
	target := o.ikeRetryTarget(candidate)
	token, err := o.ikeRetry.Begin(ctx, card, target)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	observed, closed := false, false
	var observeErr error
	observe := func(rejection EPDGNetworkRejection) error {
		// Commit independently of a canceled HTTP/lifecycle request. Failure
		// leaves the already-durable admission as an unknown outcome.
		save, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := o.ikeRetry.Observe(save, card, target, token, rejection)
		observed = true
		observeErr = errors.Join(observeErr, err)
		return err
	}
	request.BeforeIKE = func(send context.Context) error { return o.ikeRetry.Permit(send, card, target, token) }
	request.BeforeIKEEndpoint = func(send context.Context, peer string) error {
		return o.ikeRetry.PermitEndpoint(send, card, target, token, peer)
	}
	request.ObserveNetworkRejection = func(rejection EPDGNetworkRejection) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return ErrIKERetryBinding
		}
		return observe(rejection)
	}
	tunnel, startErr := o.deps.Tunnel.Start(ctx, request)
	mu.Lock()
	closed = true
	if startErr != nil && !observed {
		var rejection *EPDGNetworkRejection
		if errors.As(startErr, &rejection) {
			_ = observe(*rejection)
		}
	}
	observationFailure := observeErr
	mu.Unlock()
	if observationFailure != nil {
		return tunnel, errors.Join(startErr, observationFailure, &IKERetryBlockedError{IKERetryStatus{Source: "uncertain-attempt", UntilCardEvent: true}})
	}
	finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	finishErr := o.ikeRetry.Complete(finish, card, token)
	cancel()
	if finishErr != nil {
		return tunnel, errors.Join(startErr, finishErr)
	}
	if startErr != nil {
		check, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return tunnel, errors.Join(startErr, o.ikeRetry.Permit(check, card, target, ""))
	}
	return tunnel, nil
}
