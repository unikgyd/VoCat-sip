package sipgateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/rtpevent"
)

// Legacy application/dtmf-relay compatibility at the trusted extension/peer
// boundary. The carrier operation remains the exact original call's negotiated
// RTP event or module VTS; INFO is never an automatic retry after a failed event.
func parseDTMFRelay(body []byte) (byte, bool) {
	if len(body) == 0 || len(body) > 256 {
		return 0, false
	}
	for _, c := range body {
		if c != '\r' && c != '\n' && c != '\t' && (c < 32 || c > 126) {
			return 0, false
		}
	}
	var digit byte
	signal, duration := false, false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return 0, false
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "signal":
			if signal || len(value) != 1 || !strings.ContainsRune(rtpevent.Alphabet, rune(value[0])) {
				return 0, false
			}
			signal, digit = true, value[0]
		case "duration":
			if duration || value == "" || strings.Trim(value, "0123456789") != "" {
				return 0, false
			}
			milliseconds, err := strconv.Atoi(value)
			if err != nil || milliseconds < 40 || milliseconds > 5000 {
				return 0, false
			}
			duration = true
		default:
			return 0, false
		}
	}
	// Optional duration is validated, but the existing carrier API sends its
	// bounded 160 ms event. No key queue or arbitrary-duration modem operation.
	return digit, signal
}

type infoRequestKey struct {
	leg, branch string
	sequence    uint32
}
type infoReceipt struct {
	fingerprint [32]byte
	status      int // zero while the one original operation is pending
	responders  []func(int)
	created     time.Time
}

func (r *runtime) info(req *sip.Request, tx sip.ServerTransaction) {
	if !r.validRequest(req, tx) {
		return
	}
	leg := r.checkCallDialog(req, req.Source(), false, false)
	if leg == nil {
		r.respond(req, tx, 481, "Unknown Dialog")
		return
	}
	if len(req.GetHeaders("Info-Package")) != 0 {
		r.respond(req, tx, 469, "Bad Info Package")
		return
	}
	types := req.GetHeaders("Content-Type")
	if len(types) != 1 || len(req.GetHeaders("Content-Encoding")) != 0 {
		r.respond(req, tx, 415, "application/dtmf-relay Required")
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(types[0].Value())
	if err != nil || mediaType != "application/dtmf-relay" || len(parameters) > 1 || len(parameters) == 1 &&
		!strings.EqualFold(parameters["charset"], "utf-8") && !strings.EqualFold(parameters["charset"], "us-ascii") {
		r.respond(req, tx, 415, "Unsupported INFO Media Type")
		return
	}
	digit, valid := parseDTMFRelay(req.Body())
	if !valid {
		r.respond(req, tx, 400, "Invalid DTMF Relay")
		return
	}
	branch, _ := req.Via().Params.Get("branch")
	key := infoRequestKey{leg: leg.id, branch: branch, sequence: req.CSeq().SeqNo}
	fingerprint := sha256.Sum256([]byte(req.Recipient.String() + "\x00" + types[0].Value() + "\x00" + string(req.Body())))
	respond := func(code int) {
		reason := map[int]string{200: "OK", 481: "Unknown Dialog", 488: "DTMF Unavailable", 491: "Request Pending", 503: "DTMF Result Unconfirmed"}[code]
		r.respond(req, tx, code, reason)
	}
	c := leg.call
	c.mu.Lock()
	if prior := c.infoRequests[key]; prior != nil {
		if prior.fingerprint != fingerprint {
			c.mu.Unlock()
			r.respond(req, tx, 400, "Conflicting INFO Replay")
			return
		}
		status := prior.status
		if status == 0 && len(prior.responders) < 4 {
			prior.responders = append(prior.responders, respond)
		}
		c.mu.Unlock()
		if status != 0 {
			respond(status)
		}
		return
	}
	if c.stopped || c.phase != "active" || key.sequence <= leg.remoteSeq || branch == "" || c.inbound != nil && c.winner != leg.branch || c.carrier != nil && leg.branch != nil {
		c.mu.Unlock()
		respond(481)
		return
	}
	if c.infoRequests == nil {
		c.infoRequests = make(map[infoRequestKey]*infoReceipt)
	}
	if len(c.infoRequests) >= 16 {
		var oldest *infoReceipt
		var oldestKey infoRequestKey
		for k, entry := range c.infoRequests {
			if entry.status != 0 && (oldest == nil || entry.created.Before(oldest.created)) {
				oldest, oldestKey = entry, k
			}
		}
		if oldest == nil {
			c.mu.Unlock()
			respond(491)
			return
		}
		delete(c.infoRequests, oldestKey)
	}
	receipt := &infoReceipt{fingerprint: fingerprint, responders: []func(int){respond}, created: time.Now()}
	c.infoRequests[key] = receipt
	leg.remoteSeq, leg.remoteBranch, leg.remoteMethod = key.sequence, branch, sip.INFO
	var bridge *carrierPCMBridge
	if c.carrier != nil {
		bridge = c.carrier.bridge.Load()
	}
	if c.inbound != nil {
		bridge = c.inbound.bridge.Load()
	}
	c.mu.Unlock()
	finish := func(status int) {
		c.mu.Lock()
		receipt.status = status
		responders := receipt.responders
		receipt.responders = nil
		c.mu.Unlock()
		for _, response := range responders {
			response(status)
		}
	}
	if bridge == nil || bridge.sendCarrierDTMF == nil {
		finish(488)
		return
	}
	if _, allowed := bridge.carrierEvents.Event(digit); !allowed {
		finish(488)
		return
	}
	if !bridge.dtmfBusy.CompareAndSwap(false, true) {
		finish(491)
		return
	}
	r.mu.Lock()
	if r.closed.Load() {
		r.mu.Unlock()
		bridge.dtmfBusy.Store(false)
		finish(481)
		return
	}
	r.work.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.work.Done()
		defer bridge.dtmfBusy.Store(false)
		ctx, cancel := context.WithTimeout(bridge.ctx, 2*time.Second)
		defer cancel()
		if err := ctx.Err(); err != nil {
			finish(481)
			return
		}
		err := bridge.sendCarrierDTMF(ctx, digit)
		code := 200
		if err != nil {
			code = 503
		}
		if errors.Is(err, context.Canceled) || bridge.ctx.Err() != nil {
			code = 481
		}
		finish(code)
	}()
}
