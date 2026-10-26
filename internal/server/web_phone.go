package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"vocat/internal/sipgateway"
	"vocat/internal/vowifi"
)

// A lease is scoped to one exact IMS call, never to the modem's next call.
// Tokens are memory-only and travel in a WS subprotocol, not a URL/log.
type phoneLease struct {
	device, call, client, token string
	attached, readySeen         bool
	deadline                    time.Time
	cancel                      context.CancelFunc
	controlContext              context.Context
	controlCancel               context.CancelFunc
}
type webPhone struct {
	s      *Server
	mu     sync.Mutex
	leases map[string]*phoneLease
	closed bool
	stop   chan struct{}
}

var phoneOpaqueID = regexp.MustCompile(`^[a-zA-Z0-9_-]{20,80}$`)
var phoneE164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func newWebPhone(s *Server) *webPhone {
	p := &webPhone{s: s, leases: map[string]*phoneLease{}, stop: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case now := <-ticker.C:
				p.sweep(now)
			}
		}
	}()
	return p
}
func phoneClient(r *http.Request) string {
	id := r.Header.Get("X-VoCat-Phone-Client")
	if !phoneOpaqueID.MatchString(id) {
		return ""
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	h := sha256.Sum256([]byte(cookie.Value + "\n" + id))
	return hex.EncodeToString(h[:])
}
func (p *webPhone) reserve(device, client string) (*phoneLease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.leases[device] != nil {
		return nil, errors.New("line already in use")
	}
	for _, l := range p.leases {
		if l.client == client {
			return nil, errors.New("this browser already has a call")
		}
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	l := &phoneLease{device: device, client: client, token: hex.EncodeToString(token[:])}
	p.leases[device] = l // Reserve even while Dial/Answer is in flight.
	return l, nil
}
func (p *webPhone) drop(l *phoneLease, hangup bool) {
	p.mu.Lock()
	if p.leases[l.device] != l {
		p.mu.Unlock()
		return
	}
	delete(p.leases, l.device)
	call, cancel, controlCancel := l.call, l.cancel, l.controlCancel
	p.mu.Unlock()
	if controlCancel != nil {
		controlCancel()
	}
	if cancel != nil {
		cancel()
	}
	if hangup && call != "" {
		go p.hangup(l.device, call)
	}
}
func (p *webPhone) hangup(device, call string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = (sipCarrierBackend{p.s}).Hangup(ctx, device, call)
}
func (p *webPhone) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	leases := make([]*phoneLease, 0, len(p.leases))
	for _, l := range p.leases {
		leases = append(leases, l)
	}
	p.mu.Unlock()
	for _, l := range leases {
		p.drop(l, true)
	}
}
func (p *webPhone) sweep(now time.Time) {
	p.mu.Lock()
	leases := make([]*phoneLease, 0, len(p.leases))
	for _, l := range p.leases {
		if l.call != "" {
			leases = append(leases, l)
		}
	}
	p.mu.Unlock()
	for _, l := range leases {
		calls, err := (sipCarrierBackend{p.s}).Calls(context.Background(), l.device)
		var call *vowifi.Call
		for i := range calls {
			if calls[i].ID == l.call {
				call = &calls[i]
				break
			}
		}
		if err == nil && (call == nil || call.EndedAt != nil || call.State == "ended" || call.State == "failed") {
			p.drop(l, false)
			continue
		}
		p.mu.Lock()
		if p.leases[l.device] != l {
			p.mu.Unlock()
			continue
		}
		if call != nil && call.State == "active" && call.MediaReady && !l.readySeen && !l.attached {
			l.readySeen = true
			l.deadline = now.Add(15 * time.Second)
		}
		expired := !l.attached && !l.deadline.IsZero() && !now.Before(l.deadline)
		p.mu.Unlock()
		if expired {
			p.drop(l, true)
		}
	}
}

type phoneCallView struct {
	vowifi.Call
	AudioOwner string `json:"audio_owner"`
}
type phoneLineView struct {
	DeviceID  string          `json:"device_id"`
	Name      string          `json:"name"`
	Ready     bool            `json:"ready"`
	Calls     []phoneCallView `json:"calls"`
	Transport string          `json:"transport"`
}

func (s *Server) routePhoneAPI(w http.ResponseWriter, r *http.Request) bool {
	tail := strings.TrimPrefix(r.URL.Path, "/api/phone")
	if tail == r.URL.Path || tail != "" && !strings.HasPrefix(tail, "/") {
		return false
	}
	if s.webPhone == nil {
		writeError(w, 503, "phone_unavailable", "web phone unavailable")
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	if tail == "/media" {
		config, err := s.store.Device(r.Context(), r.URL.Query().Get("device_id"))
		if err != nil {
			writeError(w, 404, "device_not_found", "device not found")
			return true
		}
		return s.handleCallMedia(w, r, config)
	}
	if tail == "" {
		if !requireMethod(w, r, http.MethodGet) {
			return true
		}
		configs, err := s.store.ListDevices(r.Context())
		if err != nil {
			writeError(w, 503, "phone_unavailable", "device list unavailable")
			return true
		}
		lines := []phoneLineView{}
		backend := sipCarrierBackend{s}
		client := phoneClient(r)
		for _, config := range configs {
			line := phoneLineView{DeviceID: config.ID, Name: config.Name, Ready: backend.registered(r.Context(), config.ID) == nil, Calls: []phoneCallView{}, Transport: s.callTransportForConfig(config)}
			// This is memory-only. Never poll AT+CLCC or SIM APDUs here.
			calls, _ := backend.Calls(r.Context(), config.ID)
			for _, call := range calls {
				if call.EndedAt != nil || call.State == "ended" || call.State == "failed" {
					continue
				}
				owner := ""
				if s.sipGateway != nil && s.sipGateway.CarrierOwnsMedia(config.ID, call.ID) {
					owner = "sip"
				}
				s.webPhone.mu.Lock()
				if l := s.webPhone.leases[config.ID]; l != nil && l.call == call.ID {
					owner = "other_browser"
					if l.client == client {
						owner = "this_browser"
					}
				}
				s.webPhone.mu.Unlock()
				line.Calls = append(line.Calls, phoneCallView{Call: call, AudioOwner: owner})
			}
			lines = append(lines, line)
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"lines": lines}})
		return true
	}
	if tail == "/dtmf" {
		return s.handlePhoneDTMF(w, r)
	}
	if tail == "/hold" {
		return s.handlePhoneHold(w, r)
	}
	if tail != "/dial" && tail != "/answer" && tail != "/hangup" {
		writeError(w, 404, "not_found", "phone endpoint not found")
		return true
	}
	if !requireMethod(w, r, http.MethodPost) {
		return true
	}
	client := phoneClient(r)
	if client == "" {
		writeError(w, 400, "invalid_phone_client", "a per-tab phone client ID is required")
		return true
	}
	var body struct {
		DeviceID    string `json:"device_id"`
		CallID      string `json:"call_id"`
		Number      string `json:"number"`
		OperationID string `json:"operation_id"`
	}
	if s.decodeJSON(w, r, &body) != nil || body.DeviceID == "" || len(body.DeviceID) > 256 || len(body.CallID) > 256 {
		writeError(w, 400, "invalid_phone_request", "invalid phone request")
		return true
	}
	backend := sipCarrierBackend{s}
	if tail == "/hangup" {
		calls, err := backend.Calls(r.Context(), body.DeviceID)
		found := false
		for _, c := range calls {
			if c.ID == body.CallID && c.EndedAt == nil {
				found = true
			}
		}
		if err != nil || !found {
			writeError(w, 409, "call_not_found", "the exact call no longer exists")
			return true
		}
		if err := backend.Hangup(r.Context(), body.DeviceID, body.CallID); err != nil {
			writeError(w, 502, "hangup_failed", "hangup failed; check call status")
			return true
		}
		s.webPhone.mu.Lock()
		l := s.webPhone.leases[body.DeviceID]
		s.webPhone.mu.Unlock()
		if l != nil && l.call == body.CallID {
			s.webPhone.drop(l, false)
		}
		writeJSON(w, 202, map[string]any{"data": map[string]any{"accepted": true}})
		return true
	}
	if tail == "/dial" {
		if !phoneE164.MatchString(body.Number) || !phoneOpaqueID.MatchString(body.OperationID) {
			writeError(w, 400, "invalid_number", "use an international +country-code number and a unique operation ID")
			return true
		}
		if err := backend.Ready(r.Context(), body.DeviceID); err != nil {
			writeError(w, 409, "line_unavailable", "a verified idle voice line with its configured media source is required")
			return true
		}
	} else {
		if _, err := backend.Incoming(r.Context(), body.DeviceID, body.CallID); err != nil {
			writeError(w, 409, "call_unavailable", "incoming call is no longer available")
			return true
		}
	}
	l, err := s.webPhone.reserve(body.DeviceID, client)
	if err != nil {
		writeError(w, 409, "phone_busy", "this line or browser already has a call")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.webPhone.mu.Lock()
	if s.webPhone.leases[l.device] == l {
		l.cancel = cancel
	} else {
		cancel()
	}
	s.webPhone.mu.Unlock()
	var call vowifi.Call
	answerAttempted := false
	if tail == "/dial" {
		err = s.sipGateway.ReserveWebCall(ctx, body.OperationID, body.DeviceID, body.Number)
		if err == nil {
			call, err = backend.Dial(ctx, body.DeviceID, body.Number)
		}
	} else {
		err = s.claimLocalSIPAnswer(body.DeviceID, body.CallID)
		if err == nil {
			answerAttempted = true
			call, err = backend.Answer(ctx, body.DeviceID, body.CallID)
		}
	}
	if call.ID == "" || ctx.Err() != nil || err != nil {
		if call.ID == "" && answerAttempted && !errors.Is(err, sipgateway.ErrCarrierAnswerNotAttempted) {
			call.ID = body.CallID
		}
		// An uncertain submission is not replayed. Clean up only a known call.
		if call.ID != "" {
			go s.webPhone.hangup(body.DeviceID, call.ID)
		}
		s.webPhone.drop(l, false)
		writeError(w, 409, "phone_action_failed", "call not started; outcome may be uncertain, check status before starting a new call")
		return true
	}
	s.webPhone.mu.Lock()
	current := !s.webPhone.closed && s.webPhone.leases[l.device] == l
	if current {
		l.call = call.ID
		l.cancel = nil
		l.deadline = time.Now().Add(90 * time.Second)
	}
	s.webPhone.mu.Unlock()
	if !current {
		go s.webPhone.hangup(body.DeviceID, call.ID)
		writeError(w, 503, "phone_closed", "web phone stopped")
		return true
	}
	writeJSON(w, 202, map[string]any{"data": map[string]any{"call": call, "audio_token": l.token}})
	return true
}

func (p *webPhone) attach(device, call, token string, cancel context.CancelFunc) (*phoneLease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.leases[device]
	if p.closed || l == nil || l.call != call || l.attached || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(l.token)) != 1 {
		return nil, false
	}
	l.attached = true
	l.cancel = cancel
	l.controlContext, l.controlCancel = context.WithCancel(context.Background())
	return l, true
}
func (p *webPhone) detach(l *phoneLease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.leases[l.device] == l {
		if l.controlCancel != nil {
			l.controlCancel()
		}
		l.attached = false
		l.cancel = nil
		l.deadline = time.Now().Add(3 * time.Second)
		l.readySeen = true
	}
}
