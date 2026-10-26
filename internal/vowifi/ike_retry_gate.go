package vowifi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const MaxIKERetryJournalBytes = 1 << 20
const maxIKERetrySeconds = 1116000 // maximum ordinary Tw3 (31 * 10 hours)

var (
	ErrIKERetryBlocked = errors.New("vowifi: IKE operator retry restriction")
	ErrIKERetryStore   = errors.New("vowifi: IKE retry journal unavailable or invalid")
	ErrIKERetryBinding = errors.New("vowifi: IKE retry attempt lost its SIM lifecycle binding")
)

// Exact raw-byte CAS must return success only after a confirmed durable commit.
// A missing record is nil, not an invalid/empty JSON document.
type IKERetryStore interface {
	LoadIKERetry(context.Context) (json.RawMessage, error)
	CompareIKERetry(context.Context, json.RawMessage, json.RawMessage) (bool, error)
}

type IKERetryTime struct {
	Wall   time.Time
	Boot   string
	Uptime time.Duration
}
type IKERetryClock interface{ Snapshot() (IKERetryTime, error) }

// Safe for status APIs: no subscriber, journal key, endpoint or attempt token.
type IKERetryStatus struct {
	Source         string     `json:"source"`
	Scope          string     `json:"scope,omitempty"`
	NotifyType     uint16     `json:"notify_type,omitempty"`
	AfterSeconds   int64      `json:"after_seconds,omitempty"`
	NotBefore      *time.Time `json:"not_before,omitempty"`
	UntilCardEvent bool       `json:"until_card_event,omitempty"`
}

type IKERetryBlockedError struct{ Status IKERetryStatus }

func (e *IKERetryBlockedError) Error() string {
	if e.Status.UntilCardEvent {
		return "vowifi: IKE retry paused pending verified SIM lifecycle change (" + e.Status.Source + ")"
	}
	return fmt.Sprintf("vowifi: IKE retry waits %d seconds (%s)", e.Status.AfterSeconds, e.Status.Source)
}
func (e *IKERetryBlockedError) Unwrap() error { return ErrIKERetryBlocked }

type IKERetryCard struct{ key, epoch string }
type IKERetryTarget struct{ Host, PLMN, APN string }

type ikeRetryStamp struct {
	Boot   string    `json:"boot"`
	Uptime int64     `json:"uptime_ns"`
	Wall   time.Time `json:"wall"`
}
type ikeRetryEntry struct {
	Card      string        `json:"card"`
	Epoch     string        `json:"epoch"`
	Scope     string        `json:"scope"`
	Host      string        `json:"host,omitempty"`
	Endpoint  string        `json:"endpoint,omitempty"`
	PLMN      string        `json:"plmn,omitempty"`
	APN       string        `json:"apn,omitempty"`
	Notify    uint16        `json:"notify"`
	Source    string        `json:"source"`
	Seconds   int64         `json:"seconds"`
	UntilCard bool          `json:"until_card"`
	Stamp     ikeRetryStamp `json:"stamp"`
}
type ikeRetryAdmission struct {
	Epoch  string         `json:"epoch"`
	Token  string         `json:"token"`
	Target IKERetryTarget `json:"target"`
}
type ikeRetryJournal struct {
	Version int                          `json:"version"`
	Cards   map[string]string            `json:"cards"`
	Devices map[string]string            `json:"devices"`
	Entries map[string]ikeRetryEntry     `json:"entries"`
	Active  map[string]ikeRetryAdmission `json:"active"`
}

type IKERetryGate struct {
	store IKERetryStore
	clock IKERetryClock
}

func NewIKERetryGate(store IKERetryStore, clock IKERetryClock) *IKERetryGate {
	if store == nil {
		store = &memoryIKERetryStore{}
	}
	if clock == nil {
		clock = systemIKERetryClock{}
	}
	return &IKERetryGate{store: store, clock: clock}
}

type memoryIKERetryStore struct {
	mu  sync.Mutex
	raw json.RawMessage
}

func (s *memoryIKERetryStore) LoadIKERetry(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.raw), nil
}
func (s *memoryIKERetryStore) CompareIKERetry(ctx context.Context, before, after json.RawMessage) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bytes.Equal(before, s.raw) {
		return false, nil
	}
	s.raw = bytes.Clone(after)
	return true, nil
}

func retryHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func retryToken() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func retryHex(s string, size int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == size && s == strings.ToLower(s)
}
func retryBoot(s string) bool {
	return len(s) >= 32 && len(s) <= 64 && strings.Trim(s, "0123456789abcdef-") == ""
}
func retryPLMN(s string) bool {
	return (len(s) == 5 || len(s) == 6) && strings.Trim(s, "0123456789") == ""
}
func retryAPN(s string) bool {
	return s != "" && len(s) <= 253 && s == strings.ToLower(s) && !strings.ContainsAny(s, " \t\r\n/:@")
}
func retryTarget(t IKERetryTarget) bool {
	h, err := NormalizeEPDGHost(t.Host)
	return err == nil && h == t.Host && (t.PLMN == "" || retryPLMN(t.PLMN)) && retryAPN(t.APN)
}
func retryEntryKey(e ikeRetryEntry) string {
	value := e.Card + "\n" + e.Scope + "\n" + e.Host + "\n" + e.PLMN + "\n" + e.APN
	if e.Scope == "epdg" && e.Endpoint != "" {
		value += "\n" + e.Endpoint
	}
	return retryHash(value)
}
func retryScope(code uint16) string {
	switch code {
	case 10500:
		return "epdg"
	case 9002:
		return "plmn_apn"
	case 9000, 9001, 9003, 9006, 11001, 11011:
		return "plmn"
	}
	return ""
}

func emptyIKERetryJournal() ikeRetryJournal {
	return ikeRetryJournal{1, map[string]string{}, map[string]string{}, map[string]ikeRetryEntry{}, map[string]ikeRetryAdmission{}}
}

// A private journal must not silently accept duplicate object keys, unknown
// fields, trailing JSON, excessive depth, or drop damaged restrictions.
func strictRetryJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 12 {
			return ErrIKERetryStore
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					name = strings.ToLower(name)
					if !ok || seen[name] {
						return ErrIKERetryStore
					}
					seen[name] = true
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return ErrIKERetryStore
				}
			case '[':
				for d.More() {
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return ErrIKERetryStore
				}
			default:
				return ErrIKERetryStore
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrIKERetryStore
	}
	return nil
}

func decodeIKERetryJournal(raw json.RawMessage) (ikeRetryJournal, error) {
	doc := emptyIKERetryJournal()
	if raw == nil {
		return doc, nil
	}
	if len(raw) == 0 || len(raw) > MaxIKERetryJournalBytes || strictRetryJSON(raw) != nil {
		return doc, ErrIKERetryStore
	}
	doc = ikeRetryJournal{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&doc) != nil || doc.Version != 1 || doc.Cards == nil || doc.Devices == nil || doc.Entries == nil || doc.Active == nil || len(doc.Cards) > 256 || len(doc.Devices) > 128 || len(doc.Entries) > 512 || len(doc.Active) > 128 {
		return doc, ErrIKERetryStore
	}
	for card, epoch := range doc.Cards {
		if !retryHex(card, 32) || !retryHex(epoch, 16) {
			return doc, ErrIKERetryStore
		}
	}
	for device, card := range doc.Devices {
		if !retryHex(device, 32) || doc.Cards[card] == "" {
			return doc, ErrIKERetryStore
		}
	}
	for key, e := range doc.Entries {
		if !retryHex(key, 32) || key != retryEntryKey(e) || doc.Cards[e.Card] != e.Epoch || e.Epoch == "" || retryScope(e.Notify) == "" || !retryBoot(e.Stamp.Boot) || e.Stamp.Uptime < 0 || e.Stamp.Wall.IsZero() {
			return doc, ErrIKERetryStore
		}
		if e.Source != "operator" && e.Source != "implementation-missing" && e.Source != "implementation-invalid" {
			return doc, ErrIKERetryStore
		}
		if e.UntilCard {
			if e.Seconds != 0 {
				return doc, ErrIKERetryStore
			}
		} else if e.Seconds <= 0 || e.Seconds > maxIKERetrySeconds {
			return doc, ErrIKERetryStore
		}
		switch e.Scope {
		case "epdg":
			if e.Endpoint != "" {
				ip := net.ParseIP(e.Endpoint)
				if !UsableEPDGAddress(ip) || ip.String() != e.Endpoint {
					return doc, ErrIKERetryStore
				}
			}
			if retryScope(e.Notify) != "epdg" || !retryTarget(IKERetryTarget{Host: e.Host, APN: "ims"}) || e.PLMN != "" || e.APN != "" {
				return doc, ErrIKERetryStore
			}
		case "plmn":
			if retryScope(e.Notify) != "plmn" || !retryPLMN(e.PLMN) || e.Host != "" || e.APN != "" || !e.UntilCard {
				return doc, ErrIKERetryStore
			}
		case "plmn_apn":
			if retryScope(e.Notify) != "plmn_apn" || !retryPLMN(e.PLMN) || !retryAPN(e.APN) || e.Host != "" {
				return doc, ErrIKERetryStore
			}
		case "unbound_plmn":
			if retryScope(e.Notify) != "plmn" || e.PLMN != "" || e.Host != "" || e.APN != "" || !e.UntilCard {
				return doc, ErrIKERetryStore
			}
		case "unbound_plmn_apn":
			if retryScope(e.Notify) != "plmn_apn" || e.PLMN != "" || e.Host != "" || !retryAPN(e.APN) {
				return doc, ErrIKERetryStore
			}
		default:
			return doc, ErrIKERetryStore
		}
		if e.Scope != "epdg" && e.Endpoint != "" {
			return doc, ErrIKERetryStore
		}
	}
	for card, a := range doc.Active {
		if doc.Cards[card] != a.Epoch || a.Epoch == "" || !retryHex(a.Token, 16) || !retryTarget(a.Target) {
			return doc, ErrIKERetryStore
		}
	}
	return doc, nil
}

func (g *IKERetryGate) update(ctx context.Context, change func(*ikeRetryJournal, IKERetryTime) error) error {
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := g.store.LoadIKERetry(ctx)
		if err != nil {
			return errors.Join(ErrIKERetryStore, err)
		}
		doc, err := decodeIKERetryJournal(raw)
		if err != nil {
			return err
		}
		now, err := g.clock.Snapshot()
		if err != nil || !retryBoot(now.Boot) || now.Uptime < 0 || now.Wall.IsZero() {
			return errors.Join(ErrIKERetryStore, err)
		}
		for key, e := range doc.Entries {
			if e.UntilCard {
				continue
			}
			if e.Stamp.Boot != now.Boot {
				e.Stamp = ikeRetryStamp{now.Boot, int64(now.Uptime), now.Wall.UTC()}
				doc.Entries[key] = e
			} else if int64(now.Uptime) < e.Stamp.Uptime {
				return ErrIKERetryStore
			} else if int64(now.Uptime)-e.Stamp.Uptime >= e.Seconds*int64(time.Second) {
				delete(doc.Entries, key)
			}
		}
		if err := change(&doc, now); err != nil {
			return err
		}
		after, err := json.Marshal(doc)
		if err != nil || len(after) > MaxIKERetryJournalBytes {
			return ErrIKERetryStore
		}
		if _, err := decodeIKERetryJournal(after); err != nil {
			return err
		}
		if bytes.Equal(raw, after) {
			return nil
		}
		ok, err := g.store.CompareIKERetry(ctx, raw, after)
		if err != nil {
			return errors.Join(ErrIKERetryStore, err)
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("%w: concurrent journal updates", ErrIKERetryStore)
}

// Only a new verified identity in the same device slot establishes a changed
// USIM lifecycle here. Stop/restart, read errors and host reboot never do.
func (g *IKERetryGate) Prepare(ctx context.Context, deviceID, iccid string) (IKERetryCard, error) {
	var result IKERetryCard
	iccid = strings.TrimSpace(iccid)
	if iccid == "" || len(iccid) > 64 || strings.ContainsAny(iccid, "\r\n") {
		return result, ErrInvalidIdentity
	}
	card, device := retryHash("ike-card-v1\n"+iccid), retryHash("ike-device-v1\n"+deviceID)
	epoch, err := retryToken()
	if err != nil {
		return result, errors.Join(ErrIKERetryStore, err)
	}
	err = g.update(ctx, func(doc *ikeRetryJournal, _ IKERetryTime) error {
		if previous := doc.Devices[device]; previous != "" && previous != card {
			delete(doc.Cards, previous)
			delete(doc.Active, previous)
			for key, e := range doc.Entries {
				if e.Card == previous {
					delete(doc.Entries, key)
				}
			}
			for key, value := range doc.Devices {
				if value == previous {
					delete(doc.Devices, key)
				}
			}
		}
		if doc.Cards[card] == "" {
			doc.Cards[card] = epoch
		}
		doc.Devices[device] = card
		result = IKERetryCard{card, doc.Cards[card]}
		return nil
	})
	return result, err
}

func retryRemaining(e ikeRetryEntry, now IKERetryTime) time.Duration {
	return time.Duration(e.Seconds)*time.Second - (now.Uptime - time.Duration(e.Stamp.Uptime))
}
func retryBlocked(doc *ikeRetryJournal, now IKERetryTime, card IKERetryCard, target IKERetryTarget, ownToken, resolvedPeer string) *IKERetryBlockedError {
	if active, ok := doc.Active[card.key]; ok && active.Token != ownToken {
		return &IKERetryBlockedError{IKERetryStatus{Source: "uncertain-attempt", UntilCardEvent: true}}
	}
	var status *IKERetryBlockedError
	for _, e := range doc.Entries {
		if e.Card != card.key {
			continue
		}
		// A new alias must not reach the same peer. Legacy records without an
		// IP cannot prove a different peer and stay conservative until expiry.
		peerMatch := e.Scope == "epdg" && (e.Host == target.Host || resolvedPeer != "" && (e.Endpoint == resolvedPeer || e.Endpoint == ""))
		matches := e.Scope == "unbound_plmn" || e.Scope == "unbound_plmn_apn" && e.APN == target.APN || peerMatch || e.Scope == "plmn" && (target.PLMN == "" || e.PLMN == target.PLMN) || e.Scope == "plmn_apn" && (target.PLMN == "" || e.PLMN == target.PLMN) && e.APN == target.APN
		if !matches {
			continue
		}
		next := IKERetryStatus{Source: e.Source, Scope: e.Scope, NotifyType: e.Notify, UntilCardEvent: e.UntilCard}
		if !e.UntilCard {
			remaining := retryRemaining(e, now)
			next.AfterSeconds = int64((remaining + time.Second - 1) / time.Second)
			at := now.Wall.UTC().Add(remaining)
			next.NotBefore = &at
		}
		if status == nil || next.UntilCardEvent || !status.Status.UntilCardEvent && next.AfterSeconds > status.Status.AfterSeconds {
			status = &IKERetryBlockedError{next}
		}
	}
	return status
}

func (g *IKERetryGate) Permit(ctx context.Context, card IKERetryCard, target IKERetryTarget, ownToken string) error {
	return g.permit(ctx, card, target, ownToken, "")
}

func (g *IKERetryGate) PermitEndpoint(ctx context.Context, card IKERetryCard, target IKERetryTarget, ownToken, peer string) error {
	ip := net.ParseIP(peer)
	if !UsableEPDGAddress(ip) {
		return ErrIKERetryBinding
	}
	return g.permit(ctx, card, target, ownToken, ip.String())
}

func (g *IKERetryGate) permit(ctx context.Context, card IKERetryCard, target IKERetryTarget, ownToken, peer string) error {
	if !retryTarget(target) {
		return ErrIKERetryBinding
	}
	var blocked *IKERetryBlockedError
	err := g.update(ctx, func(doc *ikeRetryJournal, now IKERetryTime) error {
		if doc.Cards[card.key] != card.epoch || card.epoch == "" {
			return ErrIKERetryBinding
		}
		blocked = retryBlocked(doc, now, card, target, ownToken, peer)
		return nil
	})
	if err != nil {
		return err
	}
	if blocked != nil {
		return blocked
	}
	return nil
}

func (g *IKERetryGate) Begin(ctx context.Context, card IKERetryCard, target IKERetryTarget) (string, error) {
	if !retryTarget(target) {
		return "", ErrIKERetryBinding
	}
	token, err := retryToken()
	if err != nil {
		return "", errors.Join(ErrIKERetryStore, err)
	}
	err = g.update(ctx, func(doc *ikeRetryJournal, now IKERetryTime) error {
		if doc.Cards[card.key] != card.epoch || card.epoch == "" {
			return ErrIKERetryBinding
		}
		if blocked := retryBlocked(doc, now, card, target, "", ""); blocked != nil {
			return blocked
		}
		doc.Active[card.key] = ikeRetryAdmission{card.epoch, token, target}
		return nil
	})
	return token, err
}

func (g *IKERetryGate) Complete(ctx context.Context, card IKERetryCard, token string) error {
	return g.update(ctx, func(doc *ikeRetryJournal, _ IKERetryTime) error {
		if active, ok := doc.Active[card.key]; ok && active.Token == token && active.Epoch == card.epoch {
			delete(doc.Active, card.key)
		}
		return nil
	})
}

func (g *IKERetryGate) Observe(ctx context.Context, card IKERetryCard, target IKERetryTarget, token string, rejection EPDGNetworkRejection) error {
	scope := retryScope(rejection.NotifyType)
	host, err := NormalizeEPDGHost(rejection.EPDG)
	endpoint := net.ParseIP(rejection.Endpoint)
	if endpoint == nil {
		ip, port, portErr := net.SplitHostPort(rejection.Endpoint)
		if portErr == nil && (port == "500" || port == "4500") {
			endpoint = net.ParseIP(ip)
		}
	}
	if err != nil || host != target.Host || scope == "" || string(rejection.Scope) != scope || rejection.APN != target.APN || !UsableEPDGAddress(endpoint) {
		return ErrIKERetryBinding
	}
	return g.update(ctx, func(doc *ikeRetryJournal, now IKERetryTime) error {
		active, ok := doc.Active[card.key]
		if !ok || active.Token != token || active.Epoch != card.epoch || doc.Cards[card.key] != card.epoch || active.Target != target {
			return ErrIKERetryBinding
		}
		e := ikeRetryEntry{Card: card.key, Epoch: card.epoch, Scope: scope, Notify: rejection.NotifyType, Source: "operator", Stamp: ikeRetryStamp{now.Boot, int64(now.Uptime), now.Wall.UTC()}}
		switch scope {
		case "epdg":
			e.Host = target.Host
			e.Endpoint = endpoint.String()
		case "plmn_apn":
			e.PLMN, e.APN = target.PLMN, target.APN
			if e.PLMN == "" {
				e.Scope = "unbound_plmn_apn"
			}
		case "plmn":
			e.PLMN = target.PLMN
			e.UntilCard = true
			if e.PLMN == "" {
				e.Scope = "unbound_plmn"
			}
		}
		if !e.UntilCard {
			timer := rejection.Backoff
			switch timer.Status {
			case IKEBackoffTimed:
				decoded := DecodeIKEBackoffTimer([]byte{1, timer.Encoded})
				if decoded != timer || timer.Duration < 0 || timer.Duration%time.Second != 0 {
					return ErrIKERetryBinding
				}
				e.Seconds = int64(timer.Duration / time.Second)
				if e.Seconds == 0 {
					return nil
				}
			case IKEBackoffDeactivated:
				if timer.Encoded>>5 != 7 || timer.Duration != 0 {
					return ErrIKERetryBinding
				}
				e.UntilCard = true
			case IKEBackoffMissing, IKEBackoffInvalid:
				e.Seconds = 60
				if scope == "plmn_apn" {
					e.Seconds = 900
				}
				e.Source = "implementation-" + string(timer.Status)
			default:
				return ErrIKERetryBinding
			}
		}
		key := retryEntryKey(e)
		if previous, ok := doc.Entries[key]; ok && (previous.UntilCard || !e.UntilCard && retryRemaining(previous, now) >= time.Duration(e.Seconds)*time.Second) {
			return nil
		}
		doc.Entries[key] = e
		return nil
	})
}
