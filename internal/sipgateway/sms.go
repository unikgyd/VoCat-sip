package sipgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

const SMSLedgerSettingKey = "sip.gateway.sms-ledger.private.v1"

var ErrSMSRateLimited = errors.New("SMS rate limit reached")
var ErrSMSInvalid = errors.New("SMS input invalid")

// The subscription token is opaque and is checked AGAIN immediately before
// submission. No AT fallback, automatic registration, or alternate SIM is allowed.
type CarrierSMSBackend interface {
	SMSReady(context.Context, string) (string, error)
	SendCarrierSMS(context.Context, string, string, string, string, string) (vowifi.SMSSubmitResult, error)
}

func WithCarrierSMSBackend(backend CarrierSMSBackend) Option {
	return func(m *Manager) { m.sms = newSMSService(m.db, backend) }
}

type SMSStatistics struct {
	OutgoingAccepted int    `json:"outgoing_accepted"`
	OutgoingUnknown  int    `json:"outgoing_unknown"`
	IncomingQueued   int    `json:"incoming_queued"`
	IncomingAccepted int    `json:"incoming_accepted"`
	IncomingUnknown  int    `json:"incoming_unknown"`
	IncomingRejected int    `json:"incoming_rejected"`
	Error            string `json:"error,omitempty"`
}
type smsAttempt struct {
	Fingerprint  string    `json:"fingerprint"`
	Code         int       `json:"code"`
	Until        time.Time `json:"until"`
	AttemptToken string    `json:"attempt_token,omitempty"`
}
type smsRecipient struct {
	Identity     string `json:"identity"`
	Code         int    `json:"code"`
	Kind         string `json:"kind,omitempty"`
	User         string `json:"user,omitempty"`
	AttemptToken string `json:"attempt_token,omitempty"`
}
type IncomingSMS struct {
	ID           string `json:"id"`
	DeviceID     string `json:"device_id"`
	Subscription string `json:"subscription"`
	Sender       string `json:"sender"`
	Text         string `json:"text"`
}
type smsInbox struct {
	Message           IncomingSMS             `json:"message"`
	CanonicalReply    *string                 `json:"canonical_reply,omitempty"`
	Until             time.Time               `json:"until"`
	Targets           map[string]smsRecipient `json:"targets"`
	Rejected          bool                    `json:"rejected,omitempty"`
	InboxID           int64                   `json:"inbox_id,omitempty"`
	SourceKey         string                  `json:"source_key,omitempty"`
	SourceFingerprint string                  `json:"source_fingerprint,omitempty"`
	SourceIndependent bool                    `json:"source_independent,omitempty"`
}
type smsLedger struct {
	Version           int                   `json:"version"`
	Cursor            int64                 `json:"cursor"`
	CursorInitialized bool                  `json:"cursor_initialized"`
	Attempts          map[string]smsAttempt `json:"attempts"`
	Inbox             map[string]smsInbox   `json:"inbox"`
	raw               json.RawMessage
	migrate           bool
	guards            []store.SMSForwardInboxGuard
}
type smsService struct {
	db      repository
	backend CarrierSMSBackend
	mu      sync.Mutex
	busy    map[string]bool // Manager lifetime, including old listener shutdown.
	stats   atomic.Pointer[SMSStatistics]
}

func newSMSService(db repository, backend CarrierSMSBackend) *smsService {
	return &smsService{db: db, backend: backend, busy: map[string]bool{}}
}
func (s *smsService) statistics() SMSStatistics {
	if s == nil {
		return SMSStatistics{}
	}
	if value := s.stats.Load(); value != nil {
		return *value
	}
	return SMSStatistics{}
}
func (s *smsService) publish(state smsLedger, err error) {
	stats := SMSStatistics{}
	if err != nil {
		stats.Error = "SIP SMS state could not be persisted; no automatic retry"
	}
	for _, a := range state.Attempts {
		if a.Code == 200 || a.Code == 202 {
			stats.OutgoingAccepted++
		} else if a.Code == 504 {
			stats.OutgoingUnknown++
		}
	}
	for _, in := range state.Inbox {
		for _, target := range in.Targets {
			switch target.Code {
			case 0:
				stats.IncomingQueued++
			case 200, 202:
				stats.IncomingAccepted++
			case 504:
				stats.IncomingUnknown++
			default:
				stats.IncomingRejected++
			}
		}
	}
	s.stats.Store(&stats)
}
func smsHash(value string) bool { b, err := hex.DecodeString(value); return err == nil && len(b) == 32 }
func smsCode(code int) bool {
	switch code {
	case 0, 200, 202, 400, 403, 404, 405, 413, 415, 480, 484, 502, 503, 504, 513:
		return true
	}
	return false
}
func (s *smsService) load(ctx context.Context) (smsLedger, error) {
	state, err := s.readLedger(ctx)
	if s != nil {
		s.publish(state, err)
	}
	return state, err
}
func (s *smsService) readLedger(ctx context.Context) (smsLedger, error) {
	state := smsLedger{Version: 3, Attempts: map[string]smsAttempt{}, Inbox: map[string]smsInbox{}}
	if s == nil || s.db == nil {
		return state, errors.New("SMS store unavailable")
	}
	value, err := s.db.AppSetting(ctx, SMSLedgerSettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state = smsLedger{} // Missing version/maps are corrupt history, not an empty ledger.
	if !value.Sensitive || len(value.Value) > 4<<20 || json.Unmarshal(value.Value, &state) != nil || state.Version != 1 && state.Version != 2 && state.Version != 3 || state.Attempts == nil || state.Inbox == nil || state.Cursor < 0 || len(state.Attempts) > 2048 || len(state.Inbox) > 256 {
		return state, errors.New("invalid SMS state")
	}
	state.raw = append(json.RawMessage(nil), value.Value...)
	legacy := state.Version == 1 // Only v1 may acquire previously absent attempt tokens.
	legacyReply := state.Version < 3
	state.Version, state.migrate = 3, legacyReply
	now := time.Now()
	for key, a := range state.Attempts {
		if !smsHash(key) || !smsHash(a.Fingerprint) || !smsCode(a.Code) || a.Code == 0 || a.Until.IsZero() ||
			legacy && a.AttemptToken != "" || !legacy && !validSMSAttemptToken(a.AttemptToken) {
			return state, errors.New("invalid SMS attempt")
		}
		if legacy {
			token, err := newSMSAttemptToken()
			if err != nil {
				return state, err
			}
			a.AttemptToken = token
			state.Attempts[key] = a
		}
		if !a.Until.After(now) {
			delete(state.Attempts, key)
		}
	}
	for key, in := range state.Inbox {
		if legacyReply {
			// No v1/v2 document ever durably captured the country policy. Only
			// its already strict international sender is safe to reuse. Never
			// guess a national, short or alphanumeric sender using today's line.
			if in.CanonicalReply != nil {
				return state, errors.New("mixed-version SMS reply state")
			}
			reply := ""
			if !in.Rejected && internationalNumberPattern.MatchString(in.Message.Sender) {
				reply = in.Message.Sender
			}
			in.CanonicalReply = &reply
		} else if in.CanonicalReply == nil ||
			*in.CanonicalReply != "" && (!internationalNumberPattern.MatchString(*in.CanonicalReply) || in.Rejected) {
			// Missing/null is not an explicit read-only decision. Version 3 is
			// mandatory so old binaries reject it, rather than ignoring this field.
			return state, errors.New("invalid SMS canonical reply")
		}
		validMessage := validIncomingSMS(in.Message) || in.Rejected && validIncomingMetadata(in.Message) && in.Message.Text == ""
		bound := in.InboxID > 0 && smsHash(in.SourceKey) && smsHash(in.SourceFingerprint)
		unbound := in.InboxID == 0 && in.SourceKey == "" && in.SourceFingerprint == ""
		if !smsHash(key) || in.Until.IsZero() || in.Targets == nil || len(in.Targets) > 16 || !validMessage || !bound && !unbound || bound && in.SourceIndependent {
			return state, errors.New("invalid SMS inbox")
		}
		for user, target := range in.Targets {
			// Empty kind/user is the existing v1 local-extension format. Peer
			// recipients have a disjoint key and can never alias an account.
			local := target.Kind == "" && target.User == "" && extensionPattern.MatchString(user)
			peer := target.Kind == "peer" && validPeerTargetUser(target.User) && user == peerSMSRecipientKey(target.User)
			if (!local && !peer) || !smsHash(target.Identity) || !smsCode(target.Code) || in.Rejected && target.Code == 0 {
				return state, errors.New("invalid SMS recipient")
			}
			if legacy && target.AttemptToken != "" || target.Code == 0 && target.AttemptToken != "" ||
				!legacy && (target.AttemptToken != "" && !validSMSAttemptToken(target.AttemptToken) ||
					(target.Code == 504 || target.Code == 200 || target.Code == 202) && target.AttemptToken == "") {
				return state, errors.New("invalid SMS recipient attempt")
			}
			if legacy && (target.Code == 504 || target.Code == 200 || target.Code == 202) {
				token, err := newSMSAttemptToken()
				if err != nil {
					return state, err
				}
				target.AttemptToken = token
				in.Targets[user] = target
			}
			// Old poll-created copies have no actual Inbox reference or explicit
			// source-independent admission. Do not replay them after upgrading:
			// deletion/reassignment since that old poll cannot be proven safe.
			if unbound && !in.SourceIndependent && target.Code == 0 {
				target.Code = 403
				in.Targets[user] = target
				state.migrate = true
			}
		}
		state.Inbox[key] = in
		if !in.Until.After(now) {
			delete(state.Inbox, key)
		}
	}
	return state, nil
}
func (s *smsService) save(ctx context.Context, state smsLedger) error {
	data, err := json.Marshal(state)
	if err == nil && len(data) > 4<<20 {
		err = errors.New("SMS state capacity reached")
	}
	if err == nil {
		cas, ok := s.db.(interface {
			CompareSMSLedger(context.Context, json.RawMessage, json.RawMessage) (bool, error)
		})
		if !ok {
			err = errors.New("durable SMS compare-and-swap store unavailable")
		} else {
			var changed bool
			if len(state.guards) != 0 {
				guarded, supported := s.db.(interface {
					CompareSMSLedgerWithInboxGuard(context.Context, json.RawMessage, json.RawMessage, ...store.SMSForwardInboxGuard) (bool, error)
				})
				if !supported {
					err = errors.New("durable SMS source-guarded CAS store unavailable")
				} else {
					changed, err = guarded.CompareSMSLedgerWithInboxGuard(ctx, state.raw, data, state.guards...)
				}
			} else {
				changed, err = cas.CompareSMSLedger(ctx, state.raw, data)
			}
			if err == nil && !changed {
				err = errSMSLedgerConflict
			}
		}
	}
	if !errors.Is(err, errSMSLedgerConflict) {
		s.publish(state, err)
	}
	return err
}

var errSMSLedgerConflict = errors.New("SMS state changed concurrently")

func newSMSAttemptToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func validSMSAttemptToken(token string) bool {
	value, err := hex.DecodeString(token)
	return err == nil && len(value) == 16 && strings.ToLower(token) == token
}

// The closure is reapplied to a fresh snapshot after every failed CAS. It must
// not perform network/device I/O or retain a claim from a losing iteration.
// A successful FULL commit, not the per-service mutex, is the send boundary.
func (s *smsService) mutate(ctx context.Context, change func(*smsLedger) (bool, error)) (smsLedger, error) {
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return smsLedger{}, err
		}
		state, err := s.load(ctx)
		if err != nil {
			return state, err
		}
		changed, err := change(&state)
		if err != nil {
			s.publish(state, err)
			return state, err
		}
		if !changed && !state.migrate {
			s.publish(state, nil)
			return state, nil
		}
		err = s.save(ctx, state)
		if errors.Is(err, errSMSLedgerConflict) {
			continue
		}
		return state, err
	}
	err := errors.New("SMS state contention limit reached")
	s.publish(smsLedger{}, err)
	return smsLedger{}, err
}
func (s *smsService) attempt(ctx context.Context, operation, fingerprint, device, number, text, expectedSubscription string, authority ...func(context.Context) error) int {
	if !smsHash(operation) || !smsHash(fingerprint) {
		return 400
	}
	// Reserve and persist unknown BEFORE any carrier submission. A crash or
	// expired response remains unknown, never safe to silently send again.
	s.mu.Lock()
	state, err := s.load(ctx)
	if err != nil {
		s.publish(state, err)
		s.mu.Unlock()
		return 503
	}
	if previous, ok := state.Attempts[operation]; ok {
		s.mu.Unlock()
		if previous.Fingerprint != fingerprint {
			return 400
		}
		return previous.Code
	}
	if s.backend == nil || s.busy[device] || len(s.busy) >= 32 || len(state.Attempts) >= 2048 {
		s.mu.Unlock()
		return 503
	}
	s.busy[device] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.busy, device); s.mu.Unlock() }()
	subscription, err := s.backend.SMSReady(ctx, device)
	if err != nil {
		return carrierErrorCode(err)
	}
	if expectedSubscription != "" && subscription != expectedSubscription {
		return 403
	}
	for _, check := range authority {
		if err := check(ctx); err != nil {
			if errors.Is(err, ErrCarrierForbidden) {
				return 403
			}
			return 503
		}
	}
	token, err := newSMSAttemptToken()
	if err != nil {
		return 503
	}
	s.mu.Lock()
	reserved, previousCode := false, 503
	_, err = s.mutate(ctx, func(state *smsLedger) (bool, error) {
		reserved = false // A losing CAS iteration never owns the backend admission.
		if previous, exists := state.Attempts[operation]; exists {
			previousCode = previous.Code
			if previous.Fingerprint != fingerprint {
				previousCode = 400
			}
			return false, nil
		}
		if len(state.Attempts) >= 2048 {
			previousCode = 503
			return false, nil
		}
		state.Attempts[operation] = smsAttempt{Fingerprint: fingerprint, Code: 504,
			Until: time.Now().Add(24 * time.Hour), AttemptToken: token}
		reserved = true
		return true, nil
	})
	s.mu.Unlock()
	if err != nil {
		return 503
	}
	if !reserved {
		return previousCode
	}
	// Configuration may have been revoked while the durable claim committed.
	// Once claimed, a stop/revocation stays Unknown even without transport I/O.
	for _, check := range authority {
		if err := check(ctx); err != nil {
			return 504
		}
	}
	if ctx.Err() != nil {
		return 504
	}
	result, sendErr := s.backend.SendCarrierSMS(ctx, device, number, text, operation, subscription)
	code := 504
	if sendErr == nil && result.PartsTotal > 0 && result.AllPartsAccepted && result.PartsAccepted == result.PartsTotal && result.PartsAttempted == result.PartsTotal {
		code = 202
		if result.DeliveryConfirmed {
			code = 200
		}
	} else if result.PartsAttempted == 0 && result.PartsAccepted == 0 {
		switch {
		case errors.Is(sendErr, ErrSMSInvalid):
			code = 400
		case errors.Is(sendErr, ErrSMSRateLimited):
			code = 503
		case errors.Is(sendErr, ErrCarrierForbidden):
			code = 403
		case errors.Is(sendErr, ErrCarrierUnavailable):
			code = 480
		}
	}
	finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	completed := false
	_, err = s.mutate(finish, func(state *smsLedger) (bool, error) {
		completed = false
		a, ok := state.Attempts[operation]
		if !ok || a.Fingerprint != fingerprint || a.AttemptToken != token || a.Code != 504 {
			return false, nil
		}
		a.Code = code
		state.Attempts[operation] = a
		completed = true
		return true, nil
	})
	if err != nil || !completed {
		return 504
	}
	return code
}
func (r *runtime) carrierSMS(req *sip.Request, tx sip.ServerTransaction, account Account, body []byte) {
	if !r.config.Config.SMSEnabled {
		r.respond(req, tx, 404, "Unknown Extension")
		return
	}
	line, number, code := smsRoute(r.config.Config, account, req.Recipient.User)
	if code != 0 {
		r.respond(req, tx, code, "SMS Route Not Permitted")
		return
	}
	if r.sms == nil {
		r.messageResult(req, tx, 503)
		return
	}
	operation := smsOperationID(r.config.Config.Realm, account.Username, req.CallID().Value(), req.CSeq().SeqNo)
	fingerprint := hashCallOperation(line.DeviceID + "\x00" + number + "\x00" + string(body))
	if strings.HasPrefix(account.Username, "peer:") {
		// Keep the stable operation ID across credential replacement. A new
		// authority must conflict with the previous paid attempt, not turn it
		// into a fresh operation or inherit its submission evidence.
		revision := peerSMSAuthorityRevision(r.config, account.Username)
		if revision == "" {
			r.messageResult(req, tx, 503)
			return
		}
		fingerprint = hashCallOperation(fingerprint + "\x00peer-authority\x00" + revision)
	} else {
		revision := recipientIdentity(r.config, account.Username)
		if revision == "" {
			r.messageResult(req, tx, 503)
			return
		}
		fingerprint = hashCallOperation(fingerprint + "\x00local-authority\x00" + revision)
	}
	expectedSubscription := smsReplyBinding(req.Recipient.User)
	if expectedSubscription != "" {
		fingerprint = hashCallOperation(fingerprint + "\x00" + expectedSubscription)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 25*time.Second)
	defer cancel()
	result := make(chan int, 1)
	// A non-responsive modem cannot keep SIP shutdown waiting. The service's
	// device fence survives listener replacement until this worker really ends.
	go func() {
		result <- r.sms.attempt(ctx, operation, fingerprint, line.DeviceID, number, string(body), expectedSubscription,
			func(check context.Context) error {
				return r.sms.checkPaidAuthority(check, r, account, req.Recipient.User, line.DeviceID, number)
			})
	}()
	select {
	case code = <-result:
	case <-ctx.Done():
		code = 504
	}
	r.messageResult(req, tx, code)
}

func validIncomingSMS(message IncomingSMS) bool {
	return validIncomingMetadata(message) && validMTMessageText(message.Text)
}
func validIncomingMetadata(message IncomingSMS) bool {
	return message.ID != "" && len(message.ID) <= 512 && deviceIDPattern.MatchString(message.DeviceID) && len(message.Subscription) <= 256 && message.Sender != "" && len(message.Sender) <= 80 && utf8.ValidString(message.Sender) && !strings.ContainsAny(message.Sender, "\"\\") && !strings.ContainsFunc(message.Sender, unicode.IsControl)
}
func recipientIdentity(config stored, user string) string {
	return localRecipientIdentity(config, user)
}

func (s *smsService) checkPaidAuthority(ctx context.Context, r *runtime, account Account, target, device, number string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.closed.Load() {
		return ErrCarrierForbidden
	}
	current, err := readCurrentConfigAuthority(ctx, s.db, r.config)
	if err != nil {
		return err
	}
	if !current.Config.Enabled || !current.Config.SMSEnabled {
		return ErrCarrierForbidden
	}
	var principal Account
	if strings.HasPrefix(account.Username, "peer:") {
		if peerSMSAuthorityRevision(current, account.Username) == "" ||
			peerSMSAuthorityRevision(current, account.Username) != peerSMSAuthorityRevision(r.config, account.Username) {
			return ErrCarrierForbidden
		}
		principal = Account{Username: account.Username, SMSSendLines: current.Config.Peer.SMSSendLines}
	} else {
		if identity := recipientIdentity(current, account.Username); identity == "" || identity != recipientIdentity(r.config, account.Username) {
			return ErrCarrierForbidden
		}
		for _, candidate := range current.Config.Accounts {
			if candidate.Username == account.Username {
				principal = candidate
				break
			}
		}
	}
	selected, resolved, code := smsRoute(current.Config, principal, target)
	if code != 0 || selected.DeviceID != device || resolved != number {
		return ErrCarrierForbidden
	}
	return nil
}

func peerSMSRecipientKey(user string) string { return "peer:" + hashCallOperation(user) }

func peerSMSAuthorityRevision(config stored, principal string) string {
	peer, secrets := config.Config.Peer, config.PeerSecrets
	if peer == nil || !peer.Enabled || !peer.AllowSMSFromPeer || principal != "peer:"+peer.InboundAuth.Username || secrets == nil || !validPeerSecret(secrets.Inbound) {
		return ""
	}
	epoch, err := hex.DecodeString(secrets.CredentialEpoch)
	if err != nil || len(epoch) != 16 {
		return ""
	}
	if _, ok := usablePeerEndpoint(peer.Endpoint); !ok {
		return ""
	}
	return hashCallOperation("peer-sms-authority-v1\x00" + peer.Endpoint + "\x00" + config.Config.Realm + "\x00" + peer.InboundAuth.Username + "\x00" + secrets.CredentialEpoch)
}

// Both the line's original receive grant and the independent peer direction
// must be present. A call route must never implicitly enable SMS forwarding.
func peerSMSReceiveTarget(config Config, device string) (string, bool) {
	peer := config.Peer
	if !config.SMSEnabled || peer == nil || !peer.Enabled || !peer.AllowSMSToPeer {
		return "", false
	}
	allowed := false
	for _, line := range config.CarrierLines {
		if line.DeviceID == device && line.AllowSMSReceive {
			allowed = true
			break
		}
	}
	if allowed {
		for _, route := range peer.IncomingRoutes {
			if route.DeviceID == device && validPeerTargetUser(route.SMSTarget) {
				return route.SMSTarget, true
			}
		}
	}
	return "", false
}

// Cursor and enqueue are committed together. Offline recipients remain queued;
// a recipient that might have seen a MESSAGE is never replayed automatically.
func (m *Manager) SMSCursor(ctx context.Context, initial int64) (int64, error) {
	m.sms.mu.Lock()
	defer m.sms.mu.Unlock()
	state, err := m.sms.mutate(ctx, func(state *smsLedger) (bool, error) {
		if state.CursorInitialized {
			return false, nil
		}
		state.CursorInitialized = true
		state.Cursor = initial
		return true, nil
	})
	return state.Cursor, err
}
func (m *Manager) QueueSMS(ctx context.Context, cursor int64, message *IncomingSMS) error {
	if message != nil {
		original := *message // Retain one event/policy snapshot across CAS retries.
		message = &original
	}
	m.mu.Lock()
	config := m.current
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return errors.New("SIP service is closed")
	}
	s := m.sms
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.mutate(ctx, func(state *smsLedger) (bool, error) {
		if cursor <= state.Cursor && state.CursorInitialized {
			return false, nil
		}
		if message != nil && config.Config.Enabled && config.Config.SMSEnabled {
			key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
			if _, exists := state.Inbox[key]; !exists {
				in, targeted, planErr := makeSMSInbox(config, *message, time.Now())
				if planErr != nil {
					return false, planErr
				}
				if targeted {
					if len(state.Inbox) >= 256 {
						return false, errors.New("SMS inbox forwarding capacity reached")
					}
					// Explicit library/event admission, not the application's Inbox
					// polling path. The real application uses Store's atomic planner.
					in.SourceIndependent = true
					state.Inbox[key] = in
				}
			}
		}
		state.CursorInitialized = true
		state.Cursor = cursor
		return true, nil
	})
	return err
}
func (m *Manager) DeliverSMSOnce(ctx context.Context) error {
	m.mu.Lock()
	r := m.runtime
	m.mu.Unlock()
	if r == nil || r.closed.Load() || !r.config.Config.SMSEnabled {
		return nil
	}
	s := m.sms
	s.mu.Lock()
	state, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(state.Inbox))
	for key := range state.Inbox {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tasks := make([][2]string, 0, 16)
	users := map[string]bool{}
	peerSelected := false
	// Fill local work first. Many aliases of one offline fixed endpoint must
	// not consume the bounded task list before a later registered local user.
	for _, peerPass := range []bool{false, true} {
		for _, key := range keys {
			for user, target := range state.Inbox[key].Targets {
				isPeer := target.Kind == "peer"
				if isPeer != peerPass || target.Code != 0 || users[user] || isPeer && peerSelected {
					continue
				}
				if len(tasks) >= 16 {
					break
				}
				online := isPeer // A candidate, not proof of peer readiness.
				if !isPeer {
					r.mu.Lock()
					r.pruneLocked(time.Now())
					online = len(r.registrations[user]) > 0
					r.mu.Unlock()
				}
				if !online {
					continue
				}
				users[user] = true
				peerSelected = peerSelected || isPeer
				tasks = append(tasks, [2]string{key, user})
			}
		}
	}
	// Independent recipients are concurrent. One unreachable registered phone
	// must not add five seconds of latency for every later extension.
	var workers sync.WaitGroup
	results := make(chan error, len(tasks))
	for _, task := range tasks {
		workers.Add(1)
		go func(task [2]string) { defer workers.Done(); results <- s.deliverInbox(ctx, r, task[0], task[1]) }(task)
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *smsService) deliverInbox(ctx context.Context, r *runtime, key, user string) error {
	r.mu.Lock()
	timeout := r.messageTimeout
	r.mu.Unlock()
	if timeout == 0 {
		// A zero-value runtime has the same default as start(). Negative values
		// remain expired budgets; no cancelled parent gains a fresh timeout.
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if parentDeadline, found := ctx.Deadline(); found && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	// Caller cancellation synchronously invalidates the admission and wire
	// context; it must not depend on an AfterFunc goroutine being scheduled.
	// Runtime shutdown also has the synchronous closed fence at every boundary.
	delivery, stop := context.WithDeadline(ctx, deadline)
	unhook := context.AfterFunc(r.ctx, stop)
	defer stop()
	defer unhook()
	if ctx.Err() != nil || delivery.Err() != nil || r.closed.Load() {
		return nil
	}
	s.mu.Lock()
	state, err := s.load(delivery)
	s.mu.Unlock()
	if err != nil {
		if delivery.Err() != nil {
			return nil
		}
		return err
	}
	in, exists := state.Inbox[key]
	target, found := in.Targets[user]
	if !exists || !found || target.Code != 0 {
		return nil
	}
	payloadFingerprint := smsInboxPayloadFingerprint(in)
	identity := target.Identity
	account, _, allowed := smsReceiveAuthority(r.config, in, user, target)
	var peerTarget deliveryTarget
	peerReady := target.Kind != "peer"
	if target.Kind == "peer" {
		var permitted bool
		peerTarget, permitted = r.peerDeliveryTarget(target.User)
		allowed = allowed && permitted
		r.mu.Lock()
		if r.closed.Load() || r.smsPeerWorker {
			r.mu.Unlock()
			return nil
		}
		r.smsPeerWorker = true
		r.mu.Unlock()
		defer func() { r.mu.Lock(); r.smsPeerWorker = false; r.mu.Unlock() }()
		// A fresh durable grant must permit this original recipient before any
		// neutral probe is started. Waiting holds neither service/runtime lock
		// nor a database transaction. The actual claim re-reads everything.
		current, readErr := readCurrentConfigAuthority(delivery, s.db, r.config)
		if readErr != nil {
			if delivery.Err() != nil {
				return nil
			}
			return readErr
		}
		_, _, currentAllowed := smsReceiveAuthority(current, in, user, target)
		if allowed && currentAllowed && current.Config.Enabled && current.Config.SMSEnabled {
			peerReady = r.smsPeerReady(delivery)
		}
	}
	s.mu.Lock()
	r.mu.Lock()
	r.pruneLocked(time.Now())
	sources := map[string]deliveryTarget{}
	if target.Kind == "peer" {
		sources[peerTarget.source] = peerTarget
	} else {
		registrations := map[string]registration{}
		for _, entry := range r.registrations[user] {
			old, found := registrations[entry.source]
			if !found || entry.order > old.order {
				registrations[entry.source] = entry
			}
		}
		for source, entry := range registrations {
			sources[source] = localDeliveryTarget(entry)
		}
	}
	available := !r.closed.Load() && delivery.Err() == nil && len(sources) > 0 && len(r.messageTargets) < 32 && !r.messageTargets[user]
	if available {
		r.messageTargets[user] = true
	}
	r.mu.Unlock()
	if !available {
		s.mu.Unlock()
		return nil
	} // Definitely not attempted: safe to wait for REGISTER.
	defer func() { r.mu.Lock(); delete(r.messageTargets, user); r.mu.Unlock() }()
	token, err := newSMSAttemptToken()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	claimed := false
	_, err = s.mutate(delivery, func(state *smsLedger) (bool, error) {
		claimed = false
		latest, found := state.Inbox[key]
		next, recipientFound := latest.Targets[user]
		if !found || !recipientFound || next.Code != 0 || next.Identity != identity ||
			smsInboxPayloadFingerprint(latest) != payloadFingerprint || !smsInboxSameSource(latest, in) || !latest.Until.Equal(in.Until) || r.closed.Load() || delivery.Err() != nil {
			return false, nil
		}
		current, readErr := readCurrentConfigAuthority(delivery, s.db, r.config)
		if readErr != nil {
			return false, readErr
		}
		// A normal stop/feature toggle is not revocation of the original plan.
		if !current.Config.Enabled || !current.Config.SMSEnabled {
			return false, nil
		}
		_, _, currentAllowed := smsReceiveAuthority(current, latest, user, next)
		if !allowed || !currentAllowed {
			next.Code = 403
		} else {
			// Failed/unavailable readiness is not an attempted delivery. Also
			// fences a preflight denial that was regranted before this CAS: that
			// fresh permission must pass a new probe on a later worker pass.
			if !peerReady {
				return false, nil
			}
			next.Code, next.AttemptToken = 504, token
			if latest.InboxID > 0 {
				state.guards = append(state.guards, store.SMSForwardInboxGuard{ID: latest.InboxID, Fingerprint: latest.SourceFingerprint})
			}
			claimed = true
		}
		latest.Targets[user] = next
		state.Inbox[key] = latest
		return true, nil
	})
	if errors.Is(err, store.ErrSMSForwardSourceChanged) {
		// The guard and Unknown claim were one transaction, so no payload was
		// attempted. Cancel only the same original queued record; a replacement
		// must not be completed or rejected by this stale worker.
		_, err = s.mutate(delivery, func(state *smsLedger) (bool, error) {
			latest, found := state.Inbox[key]
			next, recipientFound := latest.Targets[user]
			if !found || !recipientFound || next.Code != 0 || next.Identity != identity ||
				smsInboxPayloadFingerprint(latest) != payloadFingerprint || latest.InboxID != in.InboxID ||
				latest.SourceKey != in.SourceKey || latest.SourceFingerprint != in.SourceFingerprint || !latest.Until.Equal(in.Until) {
				return false, nil
			}
			next.Code = 403
			latest.Targets[user] = next
			state.Inbox[key] = latest
			return true, nil
		})
		claimed = false
	}
	s.mu.Unlock()
	if err != nil || !claimed {
		return err
	}
	max := sip.MaxForwardsHeader(70)
	incoming := sip.NewRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", Host: r.identityDomain()})
	incoming.AppendHeader(&max)
	fromUser := "received." + hashCallOperation(in.Message.DeviceID + "\x00" + in.Message.Sender)[:32]
	if in.CanonicalReply != nil && *in.CanonicalReply != "" {
		fromUser = smsReplyUser(in.Message.DeviceID, in.Message.Subscription, *in.CanonicalReply)
	}
	sender := Account{Username: fromUser, DisplayName: in.Message.Sender}
	r.mu.Lock()
	if r.closed.Load() || delivery.Err() != nil {
		r.mu.Unlock()
		// The durable claim is irreversible, including cancellation before the
		// transport. Restoring Queued here could revive an ABA replacement.
		return nil
	}
	r.work.Add(1)
	r.mu.Unlock()
	results := make(chan messageOutcome, len(sources))
	var work sync.WaitGroup
	for _, entry := range sources {
		work.Add(1)
		go func(entry deliveryTarget) {
			defer work.Done()
			results <- r.deliverMTMessageParts(delivery, incoming, sender, account, entry, in.Message.Text, func() bool {
				return s.smsWireClaimCurrent(delivery, r, key, user, token, identity, payloadFingerprint, in)
			}, func() bool {
				// This closure performs no database access. It follows the last
				// coherent Settings/Ledger/Inbox snapshot at every actual datagram.
				if delivery.Err() != nil || r.closed.Load() || !in.Until.After(time.Now()) {
					return false
				}
				if !entry.peer {
					registered := false
					r.mu.Lock()
					r.pruneLocked(time.Now())
					for _, registration := range r.registrations[user] {
						if registration.source == entry.source && registration.contact != nil && entry.contact != nil && registration.contact.Address.String() == entry.contact.Address.String() {
							registered = true
							break
						}
					}
					r.mu.Unlock()
					if !registered {
						return false
					}
				}
				return delivery.Err() == nil && !r.closed.Load() && in.Until.After(time.Now())
			})
		}(entry)
	}
	work.Wait()
	stop()
	unhook()
	close(results)
	code := aggregateMessages(results)
	r.work.Done()
	finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.mutate(finish, func(state *smsLedger) (bool, error) {
		latest, found := state.Inbox[key]
		next, recipientFound := latest.Targets[user]
		if !found || !recipientFound || next.Code != 504 || next.AttemptToken != token || next.Identity != identity ||
			smsInboxPayloadFingerprint(latest) != payloadFingerprint || !smsInboxSameSource(latest, in) || !latest.Until.Equal(in.Until) {
			return false, nil
		}
		next.Code = code
		latest.Targets[user] = next
		state.Inbox[key] = latest
		return true, nil
	})
	return err
}

func smsIncomingFingerprint(message IncomingSMS) string {
	data, _ := json.Marshal(message)
	return hashCallOperation("sms-inbox-payload-v1\x00" + string(data))
}

func smsInboxPayloadFingerprint(in smsInbox) string {
	data, _ := json.Marshal(struct {
		Message        IncomingSMS `json:"message"`
		CanonicalReply *string     `json:"canonical_reply"`
	}{in.Message, in.CanonicalReply})
	return hashCallOperation("sms-inbox-payload-v2\x00" + string(data))
}

// A wire check must never assemble authorization from different writer
// revisions, nor silently repair legacy/corrupt history while exposing a body.
type smsWireSnapshotRepository struct{ snapshot store.SMSForwardWireSnapshot }

func (repository smsWireSnapshotRepository) AppSetting(ctx context.Context, key string) (store.AppSetting, error) {
	if err := ctx.Err(); err != nil {
		return store.AppSetting{}, err
	}
	switch key {
	case SettingKey:
		if repository.snapshot.SettingsFound {
			return repository.snapshot.Settings, nil
		}
	case SMSLedgerSettingKey:
		if repository.snapshot.LedgerFound {
			return repository.snapshot.Ledger, nil
		}
	}
	return store.AppSetting{Key: key}, store.ErrNotFound
}

func (smsWireSnapshotRepository) UpsertAppSetting(context.Context, store.AppSetting) error {
	return errors.New("SMS wire snapshot is read-only")
}

func (s *smsService) smsWireClaimCurrent(ctx context.Context, r *runtime, key, user, token, identity, fingerprint string, original smsInbox) bool {
	if ctx.Err() != nil || r.closed.Load() || !original.Until.After(time.Now()) {
		return false
	}
	reader, supported := s.db.(interface {
		ReadSMSForwardWireSnapshot(context.Context, *store.SMSForwardInboxGuard) (store.SMSForwardWireSnapshot, error)
	})
	if !supported {
		return false
	}
	var guard *store.SMSForwardInboxGuard
	if original.InboxID > 0 && !original.SourceIndependent && smsHash(original.SourceKey) && smsHash(original.SourceFingerprint) {
		guard = &store.SMSForwardInboxGuard{ID: original.InboxID, Fingerprint: original.SourceFingerprint}
	} else if !original.SourceIndependent || original.InboxID != 0 || original.SourceKey != "" || original.SourceFingerprint != "" {
		return false
	}
	snapshot, err := reader.ReadSMSForwardWireSnapshot(ctx, guard)
	if err != nil || guard != nil && !snapshot.InboxMatched {
		return false
	}
	var document struct {
		Version int `json:"version"`
	}
	if !snapshot.LedgerFound || json.Unmarshal(snapshot.Ledger.Value, &document) != nil || document.Version != 3 {
		return false // Do not even generate legacy migration tokens on the wire path.
	}
	repository := smsWireSnapshotRepository{snapshot: snapshot}
	current, err := readCurrentConfigAuthority(ctx, repository, r.config)
	if err != nil || !current.Config.Enabled || !current.Config.SMSEnabled {
		return false
	}
	state, err := newSMSService(repository, nil).readLedger(ctx)
	if err != nil || state.migrate {
		return false // Migration must have committed before the original claim.
	}
	latest, exists := state.Inbox[key]
	target, found := latest.Targets[user]
	if !exists || !found || target.Code != 504 || target.AttemptToken != token || target.Identity != identity ||
		!validSMSAttemptToken(token) || !latest.Until.Equal(original.Until) || !latest.Until.After(time.Now()) ||
		!smsInboxSameSource(latest, original) || smsInboxPayloadFingerprint(latest) != fingerprint ||
		latest.CanonicalReply == nil || original.CanonicalReply == nil || *latest.CanonicalReply != *original.CanonicalReply {
		return false
	}
	if _, _, allowed := smsReceiveAuthority(current, latest, user, target); !allowed {
		return false
	}
	return ctx.Err() == nil && !r.closed.Load() && latest.Until.After(time.Now())
}

func smsInboxSameSource(a, b smsInbox) bool {
	return a.InboxID == b.InboxID && a.SourceKey == b.SourceKey && a.SourceFingerprint == b.SourceFingerprint && a.SourceIndependent == b.SourceIndependent
}

// The received message's original grant and identity must still exist both in
// the admitted listener and in the latest durable settings. Neither an active
// Contact nor a reused account/password can supply a replacement authority.
func smsReceiveAuthority(config stored, in smsInbox, user string, target smsRecipient) (Account, *CarrierLine, bool) {
	var account Account
	identity := ""
	if target.Kind == "peer" {
		targetUser, allowed := peerSMSReceiveTarget(config.Config, in.Message.DeviceID)
		if !allowed || targetUser != target.User || user != peerSMSRecipientKey(target.User) {
			return account, nil, false
		}
		identity = peerRecipientIdentity(config, target.User)
		account = Account{Username: target.User}
	} else {
		identity = recipientIdentity(config, user)
		for _, candidate := range config.Config.Accounts {
			if candidate.Username == user {
				account = candidate
				break
			}
		}
	}
	if identity == "" || identity != target.Identity {
		return account, nil, false
	}
	for i := range config.Config.CarrierLines {
		line := &config.Config.CarrierLines[i]
		if line.DeviceID != in.Message.DeviceID || !line.AllowSMSReceive {
			continue
		}
		if target.Kind == "peer" {
			return account, line, true
		}
		for _, recipient := range line.SMSRecipients {
			if recipient == user {
				return account, line, true
			}
		}
	}
	return account, nil, false
}
