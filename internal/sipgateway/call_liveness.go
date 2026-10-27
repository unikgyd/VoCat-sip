package sipgateway

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
)

var errDialogSequence = errors.New("dialog local sequence exhausted")

// Local and remote sequence spaces are independent. Only the original accepted
// dialog shares this counter with its eventual BYE; losing forks do not.
func nextDialogSequence(counter *atomic.Uint32, initial uint32) (uint32, error) {
	for {
		previous := counter.Load()
		base := max(previous, initial)
		if base >= 1<<31-1 {
			return 0, errDialogSequence
		}
		if counter.CompareAndSwap(previous, base+1) {
			return base + 1, nil
		}
	}
}

type dialogProbeTarget struct {
	source  string
	request func() (*sip.Request, error)
}

// Called only after the original answer/ACK is active. No registration lookup,
// Contact redirect, media inactivity inference, carrier redial or SMS is used.
func (c *sipCall) startDialogProbes() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.phase != "active" || c.probeCancel != nil {
		return
	}
	targets := make([]dialogProbeTarget, 0, 2)
	if c.inbound == nil && c.callerLeg != nil {
		targets = append(targets, dialogProbeTarget{source: c.callerLeg.source, request: func() (*sip.Request, error) { return c.callerRequest(sip.OPTIONS) }})
	}
	if b := c.winner; b != nil {
		targets = append(targets, dialogProbeTarget{source: b.entry.source, request: func() (*sip.Request, error) {
			b.mu.Lock()
			invite, response := b.invite, b.response
			b.mu.Unlock()
			if invite == nil || response == nil {
				return nil, errors.New("original dialog unavailable")
			}
			return b.dialogRequest(sip.OPTIONS, invite, response)
		}})
	}
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(c.r.ctx)
	c.probeCancel = cancel
	// One bounded worker per accepted SIP leg (at most two). Each has its own
	// timer; the other leg's delayed response cannot postpone the next probe.
	for _, target := range targets {
		c.probeWork.Add(1)
		go func() {
			defer c.probeWork.Done()
			timer := time.NewTimer(c.r.callProbeInterval)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					probe, done := context.WithTimeout(ctx, c.r.callProbeTimeout)
					err := c.probeDialog(probe, target)
					done()
					if err != nil {
						if ctx.Err() == nil {
							c.stopCall("dialog_unreachable")
						}
						return
					}
					timer.Reset(c.r.callProbeInterval)
				}
			}
		}()
	}
}

func (c *sipCall) stopDialogProbes() {
	c.mu.Lock()
	if c.probeCancel != nil {
		c.probeCancel()
	}
	c.mu.Unlock()
	c.probeWork.Wait()
}

func (c *sipCall) probeDialog(ctx context.Context, target dialogProbeTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := target.request()
	if err != nil {
		return err
	}
	request.AppendHeader(sip.NewHeader("Accept", "application/sdp"))
	response, err := c.r.callDoResponse(ctx, request, target.source)
	if err != nil {
		return err
	}
	// Timeout/missing-dialog replies terminate the original dialog. A matched
	// 404/410 also invalidates its pinned target: a restarted UA can answer
	// OPTIONS while its old Contact no longer exists. A newer registration
	// cannot adopt this call. Other replies never redirect, authenticate a
	// retry, or feed optional SDP back into media.
	switch response.StatusCode {
	case 404, 408, 410, 481:
		return errors.New("original SIP target or dialog no longer available")
	}
	return nil
}

// Unlike an out-of-dialog capability query, tagged OPTIONS must not claim an
// ended/missing dialog exists. Its pre-transaction check also prevents a forged
// source/tag/sequence from poisoning the original peer's transaction cache.
func (r *runtime) validOptionsDialog(request *sip.Request, source string, commit bool) bool {
	leg := r.checkCallDialog(request, source, false, commit)
	if leg == nil {
		return false
	}
	leg.call.mu.Lock()
	defer leg.call.mu.Unlock()
	return !leg.call.stopped
}
