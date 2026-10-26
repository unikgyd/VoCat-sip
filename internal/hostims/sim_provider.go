package hostims

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"vocat/internal/vowifi"
)

// SIMProvider is cellular-only and tied to an exact active bearer lease. It
// intentionally has no VoWiFi RadioController, PIN, power or slot-switch API.
type SIMProvider struct {
	lease    *BearerLease
	open     uimOpener
	mu       sync.Mutex
	identity *CardIdentity
	retained []uimSession
}

var _ vowifi.PreferredAKAProvider = (*SIMProvider)(nil)

func (*SIMProvider) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private cellular SIM provider>")
}

func newSIMProvider(lease *BearerLease, open uimOpener) (*SIMProvider, error) {
	if lease == nil || lease.pool == nil || open == nil {
		return nil, ErrSelection
	}
	return &SIMProvider{lease: lease, open: open}, nil
}
func (p *SIMProvider) verifyLease(ctx context.Context) error {
	c := p.lease.claim
	c.mu.Lock()
	valid := p.currentLocked(c)
	single := c.record.Options.SinglePDN
	c.mu.Unlock()
	if !valid {
		return ErrSIM
	}
	bearer, err := p.lease.pool.owned(ctx, p.lease.selection, c)
	if err != nil || !bearer.Connected || bearer.Suspended || !single && !bearer.Multiplexed || !validConfiguration(bearer) || bearer.Interface != p.lease.configuration.Interface || !reflect.DeepEqual(bearer.Addresses, p.lease.configuration.Addresses) {
		return ErrSIM
	}
	return nil
}

// claim.mu must be held. Recheck after backend I/O, since Close may have won
// the lifecycle gate while the snapshot was being read.
func (p *SIMProvider) currentLocked(c *bearerClaim) bool {
	return c.record.Token == p.lease.token && c.record.Stage == "active" && !c.record.Uncertain && !c.record.SIMUncertain && !c.record.RuntimePending && !c.record.RuntimeUncertain && c.record.Pending == "" && !c.storageFailed && networkReady(c.record.Network)
}
func (p *SIMProvider) begin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.lease.pool.authorize(ctx, p.lease.selection) != nil {
		return ErrDenied
	}
	p.lease.claim.mu.Lock()
	runtimePending, runtimeUncertain := p.lease.claim.record.RuntimePending, p.lease.claim.record.RuntimeUncertain
	p.lease.claim.mu.Unlock()
	if runtimeUncertain {
		return ErrUncertain
	}
	if runtimePending {
		return ErrBusy
	}
	if err := p.verifyLease(ctx); err != nil {
		return err
	}
	c := p.lease.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.busy || c.record.SIMPending {
		return ErrBusy
	}
	if c.record.SIMUncertain || c.storageFailed {
		return ErrUncertain
	}
	if !p.currentLocked(c) {
		return ErrSIM
	}
	next := c.record
	next.SIMPending = true
	return p.lease.pool.saveLocked(ctx, p.lease.selection.key, c, next)
}
func (p *SIMProvider) issueChallenge(ctx context.Context, challenge vowifi.AKAChallenge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.lease.pool.authorize(ctx, p.lease.selection) != nil {
		return ErrDenied
	}
	if err := p.verifyLease(ctx); err != nil {
		return err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(p.lease.selection.SubscriberKey()))
	_, _ = hash.Write(challenge.RAND[:])
	_, _ = hash.Write(challenge.AUTN[:])
	key := hex.EncodeToString(hash.Sum(nil))
	c := p.lease.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.record.SIMPending || !p.currentLocked(c) || c.busy {
		return ErrUncertain
	}
	for _, previous := range c.record.SIMChallenges {
		if previous == key {
			return ErrSIMReplay
		}
	}
	if len(c.record.SIMChallenges) >= 128 {
		return ErrSIMReplay
	}
	next := c.record
	next.SIMChallenges = append(append([]string(nil), next.SIMChallenges...), key)
	return p.lease.pool.saveLocked(ctx, p.lease.selection.key, c, next)
}

type simOutcome struct {
	card CardIdentity
	aka  vowifi.AKAResult
	err  error
}

func (out *simOutcome) clear() {
	clearCardAKA(&out.aka)
	clear(out.card.aid)
	out.card = CardIdentity{}
}

func (p *SIMProvider) run(ctx context.Context, identity *CardIdentity, challenge *vowifi.AKAChallenge) (simOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	caller, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.begin(caller); err != nil {
		return simOutcome{}, err
	}
	done := make(chan simOutcome)
	go func() {
		worker, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		out := simOutcome{}
		uncertain := false
		session, err := p.open(worker, p.lease.selection)
		if err != nil {
			out.err = safeSIMError(err)
			uncertain = errors.Is(err, ErrUncertain)
			if session != nil {
				uncertain = uncertain || session.Uncertain()
				if !uncertain && session.Close() != nil {
					uncertain = true
				}
				if uncertain {
					p.mu.Lock()
					p.retained = append(p.retained, session)
					p.mu.Unlock()
				}
			}
			if uncertain {
				out.err = ErrUncertain
			}
		} else if session == nil {
			out.err = ErrSIM
		} else {
			card, iccid, cardErr := session.Card(worker, p.lease.selection.snapshot.Binding)
			transaction := &cardTransaction{provider: p, session: session, card: card}
			if cardErr != nil || iccid != p.lease.selection.snapshot.Binding.ICCID {
				out.err = ErrSIM
			} else if caller.Err() != nil {
				out.err = caller.Err()
			} else if challenge == nil {
				out.card, out.err = transaction.readIdentity(worker)
			} else {
				out.aka, out.err = transaction.authenticate(worker, *identity, *challenge, caller)
			}
			cleanup, finish := context.WithTimeout(context.Background(), 2*time.Second)
			if err := transaction.closeChannel(cleanup); err != nil {
				out.err = ErrUncertain
				uncertain = true
			}
			finish()
			uncertain = uncertain || transaction.unsafe || session.Uncertain()
			if uncertain {
				p.mu.Lock()
				p.retained = append(p.retained, session)
				p.mu.Unlock()
				out.err = ErrUncertain
			} else if err := session.Close(); err != nil {
				uncertain = true
				out.err = ErrUncertain
				p.mu.Lock()
				p.retained = append(p.retained, session)
				p.mu.Unlock()
			}
			if out.err == nil {
				proof, finishProof := context.WithTimeout(context.Background(), 2*time.Second)
				if p.verifyLease(proof) != nil {
					out.err = ErrSIM
				}
				finishProof()
			}
		}
		c := p.lease.claim
		c.mu.Lock()
		next := c.record
		next.SIMPending = false
		next.SIMUncertain = next.SIMUncertain || uncertain
		if uncertain {
			c.retained = append(c.retained, p)
		}
		if c.record.Token != p.lease.token || p.lease.pool.saveLocked(context.Background(), p.lease.selection.key, c, next) != nil {
			out.err = ErrJournal
		}
		c.mu.Unlock()
		out.err = safeSIMError(out.err)
		if out.err != nil {
			out.clear()
		}
		select {
		case done <- out:
		case <-caller.Done():
			out.clear()
		}
	}()
	select {
	case out := <-done:
		if caller.Err() != nil {
			out.clear()
			return simOutcome{}, caller.Err()
		}
		return out, out.err
	case <-caller.Done():
		return simOutcome{}, caller.Err()
	}
}

// Never forward driver errors: their strings may include APDUs or identities.
func safeSIMError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrSIM, ErrSIMApplication, ErrSIMIdentity, ErrSIMAuthentication, ErrSIMReplay, ErrQMIProxy, ErrQMIEndpoint, ErrTransport, ErrChanged, ErrUncertain, ErrJournal, ErrBusy, ErrDenied, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrSIM
}
func (p *SIMProvider) ReadCard(ctx context.Context) (CardIdentity, error) {
	out, err := p.run(ctx, nil, nil)
	if err != nil {
		return CardIdentity{}, err
	}
	p.mu.Lock()
	identity := out.card
	identity.aid = append([]byte(nil), out.card.aid...)
	p.identity = &identity
	p.mu.Unlock()
	return out.card, nil
}
func (p *SIMProvider) CheckReady(ctx context.Context, identity vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	if identity.ICCID != p.lease.selection.snapshot.Binding.ICCID || identity.IMSI != p.lease.selection.snapshot.Binding.IMSI {
		return vowifi.AKAEvidence{}, ErrSIM
	}
	card, err := p.ReadCard(ctx)
	if err != nil {
		return vowifi.AKAEvidence{}, err
	}
	return vowifi.AKAEvidence{Ready: true, Application: card.application}, nil
}
func (p *SIMProvider) Authenticate(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	return p.AuthenticateWithPreference(ctx, identity, challenge, "")
}
func (p *SIMProvider) AuthenticateWithPreference(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge, preference string) (vowifi.AKAResult, error) {
	p.mu.Lock()
	var card CardIdentity
	if p.identity != nil {
		card = *p.identity
		card.aid = append([]byte(nil), card.aid...)
	}
	p.mu.Unlock()
	if card.application == "" || identity.ICCID != card.subscriber.ICCID || identity.IMSI != card.subscriber.IMSI || preference != "" && preference != "isim_strict" || preference == "isim_strict" && card.application != "ISIM" {
		return vowifi.AKAResult{}, ErrSIMApplication
	}
	out, err := p.run(ctx, &card, &challenge)
	if err != nil {
		return vowifi.AKAResult{}, err
	}
	return out.aka, nil
}
func sameCardIdentity(a, b CardIdentity) bool {
	return a.private == b.private && a.public == b.public && a.domain == b.domain && a.application == b.application && a.slot == b.slot && a.subscriber.ICCID == b.subscriber.ICCID && a.subscriber.IMSI == b.subscriber.IMSI && a.subscriber.HomeMNC == b.subscriber.HomeMNC && bytes.Equal(a.aid, b.aid)
}
