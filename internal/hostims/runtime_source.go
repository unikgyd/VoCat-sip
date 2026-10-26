package hostims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
)

type runtimeSession interface {
	Query(context.Context, byte) (Runtime, error)
	BindingKey() string
	Close(context.Context) error
	Uncertain() bool
}

// RuntimeSource performs short-lived, serialized WDS reads. It retains unknown
// resources in the application-wide bearer claim, not only in the caller's GC
// graph. No RF, Start Network, profile, baseband IMS or SIP API is exposed.
type RuntimeSource struct {
	lease *BearerLease
	open  func(context.Context, func(context.Context) error) (runtimeSession, error)
}

type RuntimeProof struct {
	source      *RuntimeSource
	runtime     Runtime
	fingerprint string
}

func (RuntimeSource) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private owned WDS source>")
}
func (RuntimeProof) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private verified cellular runtime>")
}

func newRuntimeSource(lease *BearerLease, open func(context.Context, func(context.Context) error) (runtimeSession, error)) (*RuntimeSource, error) {
	if lease == nil || lease.pool == nil || lease.claim == nil || open == nil {
		return nil, ErrSelection
	}
	return &RuntimeSource{lease: lease, open: open}, nil
}

func (s *RuntimeSource) currentLocked(c *bearerClaim) bool {
	r := c.record
	return r.Token == s.lease.token && r.Stage == "active" && !r.Uncertain && r.Pending == "" && !r.SIMPending && !r.SIMUncertain && !r.RuntimeUncertain && !c.busy && !c.storageFailed && networkReady(r.Network)
}
func (s *RuntimeSource) verify(ctx context.Context) error {
	c := s.lease.claim
	c.mu.Lock()
	valid := s.currentLocked(c) && c.record.RuntimePending
	c.mu.Unlock()
	if !valid {
		return ErrChanged
	}
	current, err := s.lease.pool.owned(ctx, s.lease.selection, c)
	if err != nil || !current.Connected || current.Suspended || !validConfiguration(current) || current.Interface != s.lease.configuration.Interface || !reflect.DeepEqual(current.Addresses, s.lease.configuration.Addresses) {
		return ErrChanged
	}
	snapshot, err := s.lease.pool.verify(ctx, s.lease.selection)
	if err != nil || snapshot.QMIControl != s.lease.selection.snapshot.QMIControl {
		return ErrChanged
	}
	return nil
}

func (s *RuntimeSource) begin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.lease.pool.authorize(ctx, s.lease.selection) != nil {
		return ErrDenied
	}
	s.lease.claim.mu.Lock()
	pending := s.lease.claim.record.RuntimePending
	uncertain := s.lease.claim.record.RuntimeUncertain
	storageFailed := s.lease.claim.storageFailed
	s.lease.claim.mu.Unlock()
	if storageFailed {
		return ErrJournal
	}
	if uncertain {
		return ErrUncertain
	}
	if pending {
		return ErrBusy
	}
	if err := s.lease.Verify(ctx); err != nil {
		return err
	}
	c := s.lease.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record.RuntimePending {
		return ErrBusy
	}
	if !s.currentLocked(c) {
		return ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := c.record
	next.RuntimePending = true
	return s.lease.pool.saveLocked(ctx, s.lease.selection.key, c, next)
}

func canonicalRuntime(runtime Runtime) Runtime {
	runtime = cloneRuntime(runtime)
	runtime.APN = strings.ToLower(runtime.APN)
	sort.Slice(runtime.PCSCF, func(i, j int) bool { return runtime.PCSCF[i].Compare(runtime.PCSCF[j]) < 0 })
	sort.Strings(runtime.PCSCFDomains)
	return runtime
}
func runtimeProofKey(rows []Runtime, binding string) string {
	canonical := make([]Runtime, len(rows))
	for index, row := range rows {
		canonical[index] = canonicalRuntime(row)
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Family < canonical[j].Family })
	raw, _ := json.Marshal(canonical)
	return hashText(binding + ":" + string(raw))
}

func (s *RuntimeSource) Read(ctx context.Context) (*RuntimeProof, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	caller, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.begin(caller); err != nil {
		return nil, err
	}
	s.lease.claim.mu.Lock()
	apn := s.lease.claim.record.Options.APN
	profileID := s.lease.claim.record.Options.ProfileID
	s.lease.claim.mu.Unlock()
	type outcome struct {
		proof *RuntimeProof
		err   error
	}
	done := make(chan outcome)
	go func() {
		worker, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		out := outcome{}
		var session runtimeSession
		var rows []Runtime
		if err := caller.Err(); err != nil {
			out.err = err
		} else {
			session, out.err = s.open(worker, s.verify)
		}
		if out.err == nil && session == nil {
			out.err = ErrRuntime
		}
		if out.err == nil {
			for _, address := range s.lease.configuration.Addresses {
				if err := caller.Err(); err != nil {
					out.err = err
					break
				}
				if s.lease.pool.authorize(worker, s.lease.selection) != nil {
					out.err = ErrDenied
					break
				}
				family := byte(6)
				if address.Address.Addr().Is4() {
					family = 4
				}
				row, err := session.Query(worker, family)
				if err != nil {
					out.err = err
					break
				}
				if profileID != 0 && (row.Profile == nil || row.Profile.Type != 0 || uint32(row.Profile.Index) != profileID) {
					out.err = ErrRuntime
					break
				}
				if err := matchBoundRuntime(row, address, apn); err != nil {
					out.err = err
					break
				}
				rows = append(rows, row)
			}
		}
		var binding string
		uncertain := session == nil && errors.Is(out.err, ErrUncertain)
		if session != nil {
			uncertain = session.Uncertain()
			if !uncertain {
				cleanup, finish := context.WithTimeout(context.Background(), 4*time.Second)
				if session.Close(cleanup) != nil {
					uncertain = true
				}
				finish()
			}
			binding = session.BindingKey()
		}
		if uncertain {
			out.err = ErrUncertain
		}
		if out.err == nil && s.lease.pool.authorize(worker, s.lease.selection) != nil {
			out.err = ErrDenied
		}
		if out.err == nil {
			if !hashString(binding) || s.verify(worker) != nil {
				out.err = ErrChanged
			} else {
				merged, err := mergeBoundRuntimes(rows, s.lease.configuration, apn)
				if err != nil {
					out.err = err
				} else {
					out.proof = &RuntimeProof{source: s, runtime: canonicalRuntime(merged), fingerprint: runtimeProofKey(rows, binding+":"+s.lease.token)}
				}
			}
		}
		c := s.lease.claim
		c.mu.Lock()
		if uncertain {
			c.retained = append(c.retained, session)
		}
		next := c.record
		next.RuntimePending = false
		next.RuntimeUncertain = next.RuntimeUncertain || uncertain
		if c.record.Token != s.lease.token || s.lease.pool.saveLocked(context.Background(), s.lease.selection.key, c, next) != nil {
			out.err = ErrJournal
			if session != nil && !uncertain {
				c.retained = append(c.retained, session)
			}
		}
		c.mu.Unlock()
		out.err = safeRuntimeError(out.err)
		if out.err != nil {
			out.proof = nil
		}
		select {
		case done <- out:
		case <-caller.Done():
		}
	}()
	select {
	case out := <-done:
		if err := caller.Err(); err != nil {
			return nil, err
		}
		return out.proof, out.err
	case <-caller.Done():
		return nil, caller.Err()
	}
}

func safeRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrServingCellUnavailable, ErrRuntime, ErrPCSCF, ErrWDSUnavailable, ErrQMIEndpoint, ErrQMIProxy, ErrUncertain, ErrJournal, ErrChanged, ErrDenied, ErrBusy, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrRuntime
}
func (p *RuntimeProof) Runtime() Runtime {
	if p == nil {
		return Runtime{}
	}
	return cloneRuntime(p.runtime)
}
func (p *RuntimeProof) Endpoints() []Endpoint {
	if p == nil {
		return nil
	}
	endpoints, _ := p.runtime.Endpoints(p.runtime.APN)
	return endpoints
}
func (p *RuntimeProof) Verify(ctx context.Context) error {
	if p == nil || p.source == nil || !hashString(p.fingerprint) {
		return ErrRuntime
	}
	current, err := p.source.Read(ctx)
	if err != nil {
		return err
	}
	if current.fingerprint != p.fingerprint {
		return ErrChanged
	}
	return nil
}
