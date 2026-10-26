package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"vocat/internal/sipgateway"
)

type callAnswerClaims struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// A submission with an uncertain result remains claimed. Do not replay an
// answer merely because its HTTP/Telegram/SIP requester timed out.
func (s *Server) claimCallAnswer(ctx context.Context, device, call string) (func(bool), error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(sipgateway.ErrCarrierAnswerNotAttempted, err)
	}
	c := &s.answerClaims
	key := device + "\x00" + call
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]time.Time)
	}
	now := time.Now()
	for k, at := range c.seen {
		if now.Sub(at) > 24*time.Hour {
			delete(c.seen, k)
		}
	}
	if _, exists := c.seen[key]; exists || len(c.seen) >= 8192 {
		return nil, errors.Join(sipgateway.ErrCarrierAnswerNotAttempted, sipgateway.ErrCarrierBusy)
	}
	c.seen[key] = now
	return func(notAttempted bool) {
		if notAttempted {
			c.mu.Lock()
			if c.seen[key] == now {
				delete(c.seen, key)
			}
			c.mu.Unlock()
		}
	}, nil
}
