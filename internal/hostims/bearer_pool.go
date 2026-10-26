package hostims

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"
)

type AuthorizeBearer func(context.Context, Selection) error

// BearerPool must be application-wide, shared across line/mode replacements.
// It creates only IMS APN bearers; it never enables/resets RF, changes profiles,
// restores baseband IMS, configures host networking, or registers SIP.
type BearerPool struct {
	backend   bearerBackend
	journal   ClaimJournal
	authorize AuthorizeBearer
	mu        sync.Mutex
	claims    map[string]*bearerClaim
}
type bearerClaim struct {
	mu             sync.Mutex
	busy           bool
	lock           io.Closer
	record         claimRecord
	raw            []byte
	storageFailed  bool
	adopted        bool
	networkHolds   map[string]any
	retained       []any
	preparedAccess *PreparedAccess
	pendingCleanup *PendingBearerCleanup
}
type claimRecord struct {
	Version                            int
	Token, Binding, Owner, Modem, Path string
	Before                             []string
	Options                            BearerOptions
	Family                             uint32
	Stage                              string
	Pending                            string
	Uncertain                          bool
	SIMPending, SIMUncertain           bool
	RuntimePending, RuntimeUncertain   bool
	SIMChallenges                      []string
	Network                            *networkClaim
}
type BearerLease struct {
	pool          *BearerPool
	selection     Selection
	claim         *bearerClaim
	token         string
	configuration BearerConfiguration
}

func NewBearerPool(backend bearerBackend, journal ClaimJournal, authorize AuthorizeBearer) (*BearerPool, error) {
	if backend == nil || journal == nil || authorize == nil {
		return nil, ErrSelection
	}
	return &BearerPool{backend: backend, journal: journal, authorize: authorize, claims: make(map[string]*bearerClaim)}, nil
}

func (p *BearerPool) Select(ctx context.Context, path string) (Selection, error) {
	if !objectPath(path, "Modem") {
		return Selection{}, ErrSelection
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	snapshot, err := p.backend.Modem(ctx, path)
	if err != nil || snapshot.Path != path || !validOwner(snapshot.Owner) || !snapshot.Binding.valid() {
		return Selection{}, ErrSelection
	}
	paths, err := p.backend.Modems(ctx)
	if err != nil || len(paths) > 64 {
		return Selection{}, ErrSelection
	}
	count := 0
	for candidate, equipment := range paths {
		if !objectPath(candidate, "Modem") {
			return Selection{}, ErrSelection
		}
		if equipment == snapshot.Binding.Equipment {
			count++
		}
	}
	if count != 1 {
		return Selection{}, ErrSelection
	}
	snapshot.Bearers = append([]string(nil), snapshot.Bearers...)
	s := Selection{pool: p, snapshot: snapshot, key: hashText("imei:" + snapshot.Binding.Equipment), fingerprint: snapshot.Binding.fingerprint(snapshot.Owner, path)}
	if _, err := p.verify(ctx, s); err != nil {
		return Selection{}, err
	}
	return s, nil
}

func (p *BearerPool) verify(ctx context.Context, s Selection) (modemSnapshot, error) {
	if s.pool != p {
		return modemSnapshot{}, ErrSelection
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	snapshot, err := p.backend.Modem(ctx, s.snapshot.Path)
	if err != nil || ctx.Err() != nil || snapshot.Path != s.snapshot.Path || snapshot.Owner != s.snapshot.Owner || snapshot.Binding != s.snapshot.Binding {
		return modemSnapshot{}, ErrChanged
	}
	if len(snapshot.Bearers) > 64 {
		return modemSnapshot{}, ErrChanged
	}
	seen := make(map[string]bool)
	for _, path := range snapshot.Bearers {
		if !objectPath(path, "Bearer") || seen[path] {
			return modemSnapshot{}, ErrChanged
		}
		seen[path] = true
	}
	return snapshot, nil
}

func (p *BearerPool) slot(key string) (*bearerClaim, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value := p.claims[key]; value != nil {
		return value, nil
	}
	if len(p.claims) >= 128 {
		return nil, ErrBusy
	}
	value := &bearerClaim{}
	p.claims[key] = value
	return value, nil
}

// enter pins the cross-process advisory lock. A pre-existing durable record is
// never silently reused; only explicit Recover may act on a confirmed path.
func (p *BearerPool) enter(ctx context.Context, s Selection, recovering bool) (*bearerClaim, error) {
	if s.pool != p {
		return nil, ErrSelection
	}
	c, err := p.slot(s.key)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy || c.preparedAccess != nil || c.pendingCleanup != nil {
		return nil, ErrBusy
	}
	if c.storageFailed {
		return nil, ErrJournal
	}
	if c.lock == nil {
		lock, err := p.journal.Lock(s.key)
		if err != nil {
			return nil, err
		}
		raw, err := p.journal.Read(ctx, s.key)
		if err != nil {
			lock.Close()
			return nil, ErrJournal
		}
		if len(raw) > 0 {
			r, err := decodeClaim(raw)
			if err != nil {
				lock.Close()
				return nil, ErrJournal
			}
			c.record, c.raw = r, append([]byte(nil), raw...)
			c.adopted = true
		}
		c.lock = lock
	}
	if recovering {
		if c.record.SIMPending || c.record.SIMUncertain || c.record.RuntimePending || c.record.RuntimeUncertain || c.record.Network != nil {
			return nil, ErrUncertain
		}
		if c.raw == nil || c.record.Pending != "" || c.record.Path == "" && c.record.Stage != "reserved" {
			return nil, ErrUncertain
		}
		if !c.adopted && !c.record.Uncertain {
			return nil, ErrBusy
		}
		if c.record.Stage != "reserved" && (c.record.Binding != s.fingerprint || c.record.Owner != s.snapshot.Owner || c.record.Modem != s.snapshot.Path) {
			return nil, ErrChanged
		}
	} else if c.raw != nil {
		if c.record.Uncertain || c.record.Pending != "" {
			return nil, ErrUncertain
		}
		return nil, ErrBusy
	}
	c.busy = true
	return c, nil
}

func (p *BearerPool) finish(c *bearerClaim) { c.mu.Lock(); c.busy = false; c.mu.Unlock() }

func (p *BearerPool) saveLocked(ctx context.Context, key string, c *bearerClaim, next claimRecord) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > maxClaimBytes {
		return ErrJournal
	}
	ok, err := p.journal.Compare(ctx, key, c.raw, raw)
	if err != nil || !ok {
		c.storageFailed = true
		return ErrJournal
	}
	c.record, c.raw = next, raw
	return nil
}

func (p *BearerPool) clear(ctx context.Context, key string, c *bearerClaim) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ok, err := p.journal.Compare(ctx, key, c.raw, nil)
	if err != nil || !ok {
		c.storageFailed = true
		return ErrJournal
	}
	c.record = claimRecord{}
	c.raw = nil
	c.adopted = false
	if c.lock != nil {
		_ = c.lock.Close()
		c.lock = nil
	}
	return nil
}

func (p *BearerPool) uncertain(key string, c *bearerClaim) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw == nil || c.storageFailed {
		return
	}
	next := c.record
	next.Uncertain = true
	_ = p.saveLocked(context.Background(), key, c, next)
}

func (p *BearerPool) Acquire(ctx context.Context, s Selection, options BearerOptions) (*BearerLease, error) {
	if s.pool != p {
		return nil, ErrSelection
	}
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	if p.authorize(ctx, s) != nil {
		return nil, ErrDenied
	}
	c, err := p.enter(ctx, s, false)
	if err != nil {
		return nil, err
	}
	defer p.finish(c)
	mutated := false
	completed := false
	defer func() {
		if completed {
			return
		}
		c.mu.Lock()
		hasRecord := c.raw != nil
		pending := c.record.Pending
		path := c.record.Path
		c.mu.Unlock()
		if !hasRecord {
			c.mu.Lock()
			if c.lock != nil {
				_ = c.lock.Close()
				c.lock = nil
			}
			c.mu.Unlock()
			return
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if !mutated {
			_ = p.clear(cleanupCtx, s.key, c)
			return
		}
		if pending == "" && path != "" {
			if p.cleanup(cleanupCtx, s, c) == nil {
				return
			}
		}
		p.uncertain(s.key, c)
	}()
	snapshot, err := p.verify(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := p.conflicts(ctx, s, snapshot, options, ""); err != nil {
		return nil, err
	}
	snapshot, err = p.waitRegistered(ctx, s, snapshot)
	if err != nil {
		return nil, err
	}
	if err = p.conflicts(ctx, s, snapshot, options, ""); err != nil {
		return nil, err
	}
	family := options.IPFamily
	if family == 0 {
		for _, candidate := range []uint32{4, 2, 1} {
			if snapshot.Families&candidate != 0 {
				family = candidate
				break
			}
		}
	}
	if family == 0 || snapshot.Families&family == 0 {
		return nil, ErrParameters
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, ErrJournal
	}
	r := claimRecord{Version: 1, Token: hex.EncodeToString(token[:]), Binding: s.fingerprint, Owner: snapshot.Owner, Modem: snapshot.Path,
		Before: append([]string(nil), snapshot.Bearers...), Options: options, Family: family, Stage: "reserved"}
	c.mu.Lock()
	err = p.saveLocked(ctx, s.key, c, r)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if p.authorize(ctx, s) != nil {
		return nil, ErrDenied
	}
	if _, err := p.verify(ctx, s); err != nil {
		return nil, err
	}
	mutated = true
	if err := p.mutate(ctx, s, c, "create"); err != nil {
		return nil, err
	}
	created, err := p.owned(ctx, s, c)
	if err != nil {
		return nil, err
	}
	if created.Connected {
		c.mu.Lock()
		next := c.record
		next.Pending = "unknown"
		next.Uncertain = true
		_ = p.saveLocked(context.Background(), s.key, c, next)
		c.mu.Unlock()
		return nil, ErrChanged
	}
	if p.authorize(ctx, s) != nil {
		return nil, ErrDenied
	}
	if err := p.mutate(ctx, s, c, "connect"); err != nil {
		return nil, err
	}
	bearer, err := p.owned(ctx, s, c)
	if err != nil {
		return nil, err
	}
	if !bearer.Connected || bearer.Suspended || !options.SinglePDN && !bearer.Multiplexed || !validConfiguration(bearer) {
		return nil, ErrParameters
	}
	c.mu.Lock()
	next := c.record
	next.Stage = "active"
	err = p.saveLocked(ctx, s.key, c, next)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	completed = true
	return &BearerLease{pool: p, selection: s, claim: c, token: r.Token, configuration: BearerConfiguration{Interface: bearer.Interface, Addresses: append([]BearerAddress(nil), bearer.Addresses...)}}, nil
}

func propertiesFor(r claimRecord) bearerProperties {
	mux := uint32(3)
	if r.Options.SinglePDN {
		mux = 1
	}
	return bearerProperties{APN: r.Options.APN, APNType: 4, IPFamily: r.Family, Multiplex: mux, AllowRoaming: r.Options.AllowRoaming, ProfileID: r.Options.ProfileID}
}

func (p *BearerPool) conflicts(ctx context.Context, s Selection, snapshot modemSnapshot, o BearerOptions, owned string) error {
	for _, path := range snapshot.Bearers {
		if path == owned {
			continue
		}
		if o.SinglePDN {
			return ErrConflict
		}
		other, err := p.backend.Bearer(ctx, snapshot.Owner, path)
		if err != nil {
			return ErrConflict
		}
		if other.Properties.APNType&4 != 0 || equalAPN(other.Properties.APN, o.APN) {
			return ErrConflict
		}
	}
	return nil
}

func (p *BearerPool) owned(ctx context.Context, s Selection, c *bearerClaim) (bearerSnapshot, error) {
	c.mu.Lock()
	r := c.record
	failed := c.storageFailed
	c.mu.Unlock()
	if failed {
		return bearerSnapshot{}, ErrJournal
	}
	if r.Binding != s.fingerprint || r.Pending != "" || !objectPath(r.Path, "Bearer") {
		return bearerSnapshot{}, ErrUncertain
	}
	snapshot, err := p.verify(ctx, s)
	if err != nil {
		return bearerSnapshot{}, err
	}
	found := false
	for _, path := range snapshot.Bearers {
		if path == r.Path {
			found = true
		}
	}
	if !found {
		return bearerSnapshot{}, ErrChanged
	}
	if err := p.conflicts(ctx, s, snapshot, r.Options, r.Path); err != nil {
		return bearerSnapshot{}, err
	}
	bearer, err := p.backend.Bearer(ctx, r.Owner, r.Path)
	expected := propertiesFor(r)
	actual := bearer.Properties
	if expected.ProfileID == 0 {
		actual.ProfileID = 0 // An automatic profile number is not a requested identity.
	}
	if err != nil || actual != expected {
		return bearerSnapshot{}, ErrChanged
	}
	if _, err := p.verify(ctx, s); err != nil {
		return bearerSnapshot{}, err
	}
	return bearer, nil
}

// mutate retains its own reply observer after caller cancellation. A D-Bus
// caller timeout does NOT cancel daemon execution. No overlapping cleanup or
// retry is allowed while the intent is pending or its outcome is unknown.
func (p *BearerPool) mutate(ctx context.Context, s Selection, c *bearerClaim, op string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	if c.record.Pending != "" || c.storageFailed {
		c.mu.Unlock()
		return ErrUncertain
	}
	next := c.record
	next.Pending = op
	if err := p.saveLocked(ctx, s.key, c, next); err != nil {
		c.mu.Unlock()
		return err
	}
	r := c.record
	c.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		operationCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var path string
		var err error
		switch op {
		case "create":
			path, err = p.backend.Create(operationCtx, r.Owner, r.Modem, propertiesFor(r))
		case "connect":
			err = p.backend.Connect(operationCtx, r.Owner, r.Path)
		case "disconnect":
			err = p.backend.Disconnect(operationCtx, r.Owner, r.Path)
		case "delete":
			err = p.backend.Delete(operationCtx, r.Owner, r.Modem, r.Path)
		default:
			err = ErrSelection
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		next := c.record
		if next.Token != r.Token || next.Pending != op {
			done <- ErrUncertain
			return
		}
		if op == "connect" && operationCtx.Err() == nil && errors.Is(err, ErrRejected) {
			// The daemon acknowledged a failed Connect on our known object.
			// Acquire's cleanup still revalidates the original owner and exact
			// bearer, disconnects if needed, and requires a Delete ACK.
			next.Pending = ""
		} else if err != nil || operationCtx.Err() != nil {
			next.Pending = "unknown"
			next.Uncertain = true
			err = ErrUncertain
		} else {
			next.Pending = ""
			switch op {
			case "create":
				valid := objectPath(path, "Bearer")
				for _, previous := range r.Before {
					if previous == path {
						valid = false
					}
				}
				if !valid {
					next.Pending = "unknown"
					next.Uncertain = true
					err = ErrUncertain
				} else {
					next.Path = path
					next.Stage = "created"
				}
			case "connect":
				next.Stage = "connected"
			case "disconnect":
				next.Stage = "disconnected"
			case "delete":
				next.Stage = "deleted"
			}
		}
		if saveErr := p.saveLocked(context.Background(), s.key, c, next); saveErr != nil {
			done <- saveErr
			return
		}
		if err != nil {
			done <- err
		} else {
			done <- nil
		}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		p.uncertain(s.key, c)
		return ErrUncertain
	}
}

func (p *BearerPool) cleanup(ctx context.Context, s Selection, c *bearerClaim) error {
	c.mu.Lock()
	r := c.record
	c.mu.Unlock()
	if r.Stage == "reserved" && r.Pending == "" && r.Path == "" {
		// The write-ahead protocol proves no daemon mutation was issued. A
		// process crash here needs no modem operation, even after a SIM swap.
		return p.clear(ctx, s.key, c)
	}
	if r.Stage == "deleted" {
		snapshot, err := p.verify(ctx, s)
		if err != nil {
			return err
		}
		for _, path := range snapshot.Bearers {
			if path == r.Path {
				return ErrChanged
			}
		}
		return p.clear(ctx, s.key, c)
	}
	bearer, err := p.owned(ctx, s, c)
	if err != nil {
		return err
	}
	if bearer.Connected {
		if err := p.mutate(ctx, s, c, "disconnect"); err != nil {
			return err
		}
	}
	bearer, err = p.owned(ctx, s, c)
	if err != nil {
		return err
	}
	if bearer.Connected {
		return ErrChanged
	}
	if err := p.mutate(ctx, s, c, "delete"); err != nil {
		return err
	}
	return p.cleanup(ctx, s, c)
}

// Recover is explicit. It may resume cleanup only after acquiring the live
// process lock and matching the exact original service/hardware/SIM binding.
// Lost Create replies / unknown mutation errors are never reconstructed by APN.
func (p *BearerPool) Recover(ctx context.Context, s Selection) error {
	c, err := p.enter(ctx, s, true)
	if err != nil {
		return err
	}
	defer p.finish(c)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.cleanup(ctx, s, c); err != nil {
		p.uncertain(s.key, c)
		return err
	}
	return nil
}

func (l *BearerLease) Configuration() BearerConfiguration {
	return BearerConfiguration{Interface: l.configuration.Interface, Addresses: append([]BearerAddress(nil), l.configuration.Addresses...)}
}
func (l *BearerLease) Verify(ctx context.Context) error {
	l.claim.mu.Lock()
	valid := l.claim.record.Token == l.token && l.claim.record.Stage == "active" && !l.claim.record.Uncertain && !l.claim.record.SIMPending && !l.claim.record.SIMUncertain && !l.claim.record.RuntimePending && !l.claim.record.RuntimeUncertain && l.claim.record.Pending == "" && !l.claim.storageFailed && !l.claim.busy && networkReady(l.claim.record.Network)
	single := l.claim.record.Options.SinglePDN
	l.claim.mu.Unlock()
	if !valid {
		return ErrChanged
	}
	bearer, err := l.pool.owned(ctx, l.selection, l.claim)
	if err != nil {
		return err
	}
	if !bearer.Connected || bearer.Suspended || !single && !bearer.Multiplexed || !validConfiguration(bearer) || bearer.Interface != l.configuration.Interface || !reflect.DeepEqual(bearer.Addresses, l.configuration.Addresses) {
		return ErrChanged
	}
	return nil
}
func (l *BearerLease) Close(ctx context.Context) error {
	return l.close(ctx, nil)
}

// Only the rooted access owner can retire its PDN after local cleanup.
func (l *BearerLease) close(ctx context.Context, access *PreparedAccess) error {
	l.claim.mu.Lock()
	if l.claim.pendingCleanup != nil || l.claim.preparedAccess != nil && l.claim.preparedAccess != access || access != nil && l.claim.preparedAccess != access {
		l.claim.mu.Unlock()
		return ErrBusy
	}
	if l.claim.record.SIMPending || l.claim.record.SIMUncertain || l.claim.record.RuntimePending || l.claim.record.RuntimeUncertain || l.claim.record.Network != nil {
		l.claim.mu.Unlock()
		return ErrUncertain
	}
	if l.claim.record.Token == "" {
		l.claim.mu.Unlock()
		return nil
	}
	if l.claim.record.Token != l.token || l.claim.busy {
		l.claim.mu.Unlock()
		return ErrBusy
	}
	l.claim.busy = true
	l.claim.mu.Unlock()
	defer l.pool.finish(l.claim)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := l.pool.cleanup(ctx, l.selection, l.claim); err != nil {
		l.pool.uncertain(l.selection.key, l.claim)
		return err
	}
	return nil
}

func decodeClaim(raw []byte) (claimRecord, error) {
	var r claimRecord
	if len(raw) > maxClaimBytes {
		return r, ErrJournal
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || r.Version != 1 || !hashString(r.Token) || !hashString(r.Binding) || !validOwner(r.Owner) || !objectPath(r.Modem, "Modem") || r.Path != "" && !objectPath(r.Path, "Bearer") || len(r.Before) > 64 {
		return r, ErrJournal
	}
	if decoder.Decode(new(any)) != io.EOF {
		return r, ErrJournal
	}
	if normalized, err := r.Options.normalized(); err != nil || normalized != r.Options {
		return r, ErrJournal
	}
	if r.Family != 1 && r.Family != 2 && r.Family != 4 {
		return r, ErrJournal
	}
	switch r.Stage {
	case "reserved", "created", "connected", "active", "disconnected", "deleted":
	default:
		return r, ErrJournal
	}
	switch r.Pending {
	case "", "create", "connect", "disconnect", "delete", "unknown":
	default:
		return r, ErrJournal
	}
	for _, path := range r.Before {
		if !objectPath(path, "Bearer") {
			return r, ErrJournal
		}
	}
	if r.Stage != "reserved" && r.Path == "" {
		return r, ErrJournal
	}
	if len(r.SIMChallenges) > 128 {
		return r, ErrJournal
	}
	if !validNetworkClaim(r.Network) {
		return r, ErrJournal
	}
	seen := map[string]bool{}
	for _, challenge := range r.SIMChallenges {
		if !hashString(challenge) || seen[challenge] {
			return r, ErrJournal
		}
		seen[challenge] = true
	}
	return r, nil
}
func hashString(s string) bool  { v, e := hex.DecodeString(s); return e == nil && len(v) == 32 }
func equalAPN(a, b string) bool { return strings.EqualFold(a, b) }
func validConfiguration(b bearerSnapshot) bool {
	if b.ConfigurationError || !validInterface(b.Interface) || len(b.Addresses) == 0 || len(b.Addresses) > 2 {
		return false
	}
	seen := make(map[bool]bool)
	for _, value := range b.Addresses {
		ip := value.Address.Addr()
		v4 := ip.Is4()
		if !value.Address.IsValid() || !usable(ip) || seen[v4] || value.MTU != 0 && (value.MTU < 68 || value.MTU > 65535) || !v4 && value.MTU != 0 && value.MTU < 1280 {
			return false
		}
		if value.Gateway.IsValid() && (value.Gateway.Is4() != v4 || value.Gateway.Is4In6() || value.Gateway.IsMulticast() || value.Gateway.IsLoopback() || value.Gateway.IsUnspecified()) {
			return false
		}
		seen[v4] = true
	}
	return true
}
func validInterface(s string) bool {
	if s == "" || len(s) > 15 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return s != "." && s != ".."
}
