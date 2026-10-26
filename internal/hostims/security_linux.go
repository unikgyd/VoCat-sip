//go:build linux

package hostims

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"

	"golang.org/x/sys/unix"
)

var ErrSecurityScope = staticError("host IMS: owned security scope verification failed")

type scopedXFRMAccess interface {
	ScopeVacant(context.Context, uint32, uint32, uint32) error
	Add(context.Context, scopedXFRMOperation) error
	Observe(context.Context, scopedXFRMKey) (scopedXFRMSnapshot, bool, error)
	Delete(context.Context, scopedXFRMKey, string) error
	Close() error
}
type scopedTCAccess interface {
	operations() []securityTCOperation
	observe(securityTCOperation) (string, bool, error)
	apply(securityTCOperation) error
	remove(securityTCOperation, string) error
	close()
}
type kernelSecurityFactory func(*securityClaim) (scopedXFRMAccess, scopedTCAccess, error)

// This is the long-lived owner, not a SIM or Established evidence factory.
// It stays rooted by the app-wide network claim even if an IMS Start is lost.
type kernelSecurityLease struct {
	network      *kernelNetworkLease
	token        string
	xfrm         scopedXFRMAccess
	tc           scopedTCAccess
	reservations *securityReservations
	blocked      atomic.Bool
	closed       bool // network.gate
	active       *kernelSecuritySAHandle
	identity     securityScopeIdentity // immutable after reservation intent
}
type securityScopeIdentity struct {
	mark, client, server uint32
	flows                []securityFlow
	socketOutbound       bool
}
type kernelSecuritySAHandle struct {
	owner  *kernelSecurityLease
	closed atomic.Bool
}

var _ vowifi.CellularIMSNetwork = (*kernelSecurityLease)(nil)
var _ vowifi.CellularIMSSecurity = (*kernelSecurityLease)(nil)

// The production constructor accepts only the proven network, never UI mark,
// namespace, interface or P-CSCF inputs. The fixture constructor is private.
func prepareKernelSecurity(ctx context.Context, n *kernelNetworkLease) (*kernelSecurityLease, error) {
	if n == nil || n.proof == nil {
		return nil, ErrRuntime
	}
	k, ok := n.driver.(*linuxNetworkKernel)
	if !ok || k == nil {
		return nil, ErrNetwork
	}
	return prepareKernelSecurityWith(ctx, n, securityReservationDirectory, func(r *securityClaim) (scopedXFRMAccess, scopedTCAccess, error) {
		if err := k.verify(ctx); err != nil {
			return nil, nil, err
		}
		return openKernelSecurityDrivers(k, r)
	})
}

func openKernelSecurityDrivers(k *linuxNetworkKernel, r *securityClaim) (scopedXFRMAccess, scopedTCAccess, error) {
	if k == nil || r == nil {
		return nil, nil, ErrSecurityScope
	}
	xfrm, err := openScopedXFRM(k.namespace, k.namespaceCookie)
	if err != nil {
		return nil, nil, err
	}
	spec := securityTCSpec{InterfaceIndex: k.index, Mark: r.Mark, Token: r.Token}
	for _, flow := range r.Flows {
		spec.Flows = append(spec.Flows, securityTCFlow{Local: flow.Local, Remote: flow.Remote})
	}
	tc, err := openLinuxSecurityTC(k.namespace, k.namespaceCookie, spec)
	if err != nil {
		xfrm.Close()
		return nil, nil, err
	}
	return xfrm, tc, nil
}

// Explicit cleanup-only reopen under an already re-established original
// network lease. This is not a cold-boot network/PDN recovery implementation.
// Unknown ACKs are deliberately not inferred from matching kernel objects.
func recoverKernelSecurity(ctx context.Context, n *kernelNetworkLease) error {
	if n == nil {
		return ErrSecurityScope
	}
	k, ok := n.driver.(*linuxNetworkKernel)
	if !ok {
		return ErrNetwork
	}
	return recoverKernelSecurityWith(ctx, n, securityReservationDirectory, func(r *securityClaim) (scopedXFRMAccess, scopedTCAccess, error) {
		return openKernelSecurityDrivers(k, r)
	})
}

func recoverKernelSecurityWith(ctx context.Context, n *kernelNetworkLease, directory string, open kernelSecurityFactory) error {
	if open == nil {
		return ErrSecurityScope
	}
	return runKernelSecurity(ctx, n, func(worker context.Context) error {
		if n.security != nil {
			return ErrBusy
		}
		r := cloneSecurityClaim(n.record.Security)
		if r == nil {
			return nil
		}
		if !validSecurityClaim(r) {
			return ErrJournal
		}
		if r.Pending != nil || r.Phase == "uncertain" {
			return ErrUncertain
		}
		xfrm, tc, err := open(r)
		if err != nil {
			return err
		}
		reservations, err := openSecurityReservations(directory, n.record.Namespace, r.Token, r.Mark, r.ClientReqID, r.ServerReqID, true)
		if err != nil || len(r.Operations) > 0 && len(reservations.files) != 3 {
			if reservations != nil {
				reservations.closeFDs()
			}
			xfrm.Close()
			tc.close()
			if err != nil {
				return err
			}
			return ErrUncertain
		}
		s := &kernelSecurityLease{network: n, token: r.Token, xfrm: xfrm, tc: tc, reservations: reservations,
			identity: securityScopeIdentity{mark: r.Mark, client: r.ClientReqID, server: r.ServerReqID, flows: append([]securityFlow(nil), r.Flows...), socketOutbound: r.SocketOutbound}}
		n.security = s
		return s.cleanup(worker)
	}, nil, nil)
}

// A release callback runs exactly once, in the worker if launched. In
// particular caller cancellation must not erase CK/IK while Add still runs.
func runKernelSecurity(ctx context.Context, n *kernelNetworkLease, action func(context.Context) error,
	abandoned func(context.Context), release func()) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if n == nil || action == nil {
		if release != nil {
			release()
		}
		return ErrSecurityScope
	}
	select {
	case <-ctx.Done():
		if release != nil {
			release()
		}
		return ctx.Err()
	case <-n.gate:
	}
	if ctx.Err() != nil {
		n.gate <- struct{}{}
		if release != nil {
			release()
		}
		return ctx.Err()
	}
	done := make(chan error)
	go func() {
		defer func() {
			if release != nil {
				release()
			}
			n.gate <- struct{}{}
		}()
		worker, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := action(worker)
		cancel()
		select {
		case done <- err:
		case <-ctx.Done():
			if abandoned != nil {
				cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
				abandoned(cleanup)
				finish()
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newSecurityClaim(endpoints []Endpoint) (*securityClaim, error) {
	seed := make([]byte, 44)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, ErrSecurityScope
	}
	defer clear(seed)
	r := &securityClaim{Token: hex.EncodeToString(seed[:32]), Phase: "reserving", SocketOutbound: true,
		Mark:        0x60000000 | binary.LittleEndian.Uint32(seed[32:36])&0x0fffffff,
		ClientReqID: 0x80000000 | binary.LittleEndian.Uint32(seed[36:40])&0x3fffffff,
		ServerReqID: 0x80000000 | binary.LittleEndian.Uint32(seed[40:44])&0x3fffffff}
	if r.ClientReqID == r.ServerReqID {
		r.ServerReqID ^= 1
	}
	for _, endpoint := range endpoints {
		r.Flows = append(r.Flows, securityFlow{Local: endpoint.Local, Remote: endpoint.Remote})
	}
	sort.Slice(r.Flows, func(i, j int) bool {
		if c := r.Flows[i].Local.Compare(r.Flows[j].Local); c != 0 {
			return c < 0
		}
		return r.Flows[i].Remote.Compare(r.Flows[j].Remote) < 0
	})
	unique := r.Flows[:0]
	for _, flow := range r.Flows {
		if len(unique) == 0 || flow != unique[len(unique)-1] {
			unique = append(unique, flow)
		}
	}
	r.Flows = unique
	if !validSecurityClaim(r) {
		return nil, ErrSecurityScope
	}
	return r, nil
}

func prepareKernelSecurityWith(ctx context.Context, n *kernelNetworkLease, directory string, open kernelSecurityFactory) (*kernelSecurityLease, error) {
	var result *kernelSecurityLease
	var working *kernelSecurityLease
	err := runKernelSecurity(ctx, n, func(worker context.Context) error {
		if open == nil || n.record.Security != nil || n.security != nil {
			return ErrBusy
		}
		if err := n.verify(worker); err != nil {
			return err
		}
		for attempt := 0; attempt < 8; attempt++ {
			r, err := newSecurityClaim(n.signaling)
			if err != nil {
				return err
			}
			s := &kernelSecurityLease{network: n, token: r.Token, identity: securityScopeIdentity{
				mark: r.Mark, client: r.ClientReqID, server: r.ServerReqID, flows: append([]securityFlow(nil), r.Flows...), socketOutbound: r.SocketOutbound}}
			working, n.security = s, s
			if err := s.save(worker, r, "preparing"); err != nil {
				n.security = nil
				return err
			}
			s.xfrm, s.tc, err = open(r)
			if err == nil {
				s.reservations, err = openSecurityReservations(directory, n.record.Namespace, r.Token, r.Mark, r.ClientReqID, r.ServerReqID, false)
			}
			if err == nil {
				err = s.xfrm.ScopeVacant(worker, r.Mark, r.ClientReqID, r.ServerReqID)
			}
			if err != nil {
				if errors.Is(err, ErrUncertain) || errors.Is(err, ErrXFRMUnknown) {
					s.poison(worker)
					return ErrUncertain
				}
				if cleanupErr := s.cleanup(worker); cleanupErr != nil {
					return cleanupErr
				}
				if errors.Is(err, ErrBusy) || errors.Is(err, ErrXFRMConflict) {
					continue
				}
				return err
			}
			for _, op := range s.tc.operations() {
				if ctx != nil && ctx.Err() != nil {
					err = ctx.Err()
					break
				}
				if err = s.addTC(worker, op); err != nil {
					break
				}
			}
			if err == nil {
				r = s.record()
				r.Phase = "listening"
				err = s.save(worker, r, "active")
			}
			if err != nil {
				cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
				cleanupErr := s.cleanup(cleanup)
				finish()
				return errors.Join(err, cleanupErr)
			}
			result = s
			return nil
		}
		return ErrBusy
	}, func(cleanup context.Context) {
		if working != nil && !working.closed {
			_ = working.cleanup(cleanup)
		}
	}, nil)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *kernelSecurityLease) record() *securityClaim {
	if s == nil || s.network == nil || s.network.record.Security == nil || s.network.record.Security.Token != s.token {
		return nil
	}
	r := s.network.record.Security
	if !validSecurityClaim(r) || r.Mark != s.identity.mark || r.ClientReqID != s.identity.client || r.ServerReqID != s.identity.server || r.SocketOutbound != s.identity.socketOutbound || !sameSecurityFlows(r.Flows, s.identity.flows) {
		return nil
	}
	return cloneSecurityClaim(r)
}

func sameSecurityFlows(a, b []securityFlow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *kernelSecurityLease) selectedFlowsMatch(r *securityClaim) bool {
	flows := make([]securityFlow, 0, len(s.network.signaling))
	for _, endpoint := range s.network.signaling {
		flows = append(flows, securityFlow{endpoint.Local, endpoint.Remote})
	}
	sort.Slice(flows, func(i, j int) bool {
		if c := flows[i].Local.Compare(flows[j].Local); c != 0 {
			return c < 0
		}
		return flows[i].Remote.Compare(flows[j].Remote) < 0
	})
	unique := flows[:0]
	for _, flow := range flows {
		if len(unique) == 0 || flow != unique[len(unique)-1] {
			unique = append(unique, flow)
		}
	}
	return sameSecurityFlows(r.Flows, unique)
}
func (s *kernelSecurityLease) save(ctx context.Context, r *securityClaim, phase string) error {
	next := cloneNetworkClaim(s.network.record)
	next.Security, next.Phase = cloneSecurityClaim(r), phase
	if s.network.uncertain || s.network.record.Pending != nil || s.network.record.Phase == "uncertain" {
		next.Phase = "uncertain"
	}
	return s.network.save(ctx, next)
}
func (s *kernelSecurityLease) poison(ctx context.Context) {
	s.blocked.Store(true)
	if r := s.record(); r != nil {
		r.Phase = "uncertain"
		_ = s.save(ctx, r, "preparing")
	}
}

func (s *kernelSecurityLease) parent(ctx context.Context) error {
	if s == nil || s.closed || s.record() == nil {
		return ErrSecurityScope
	}
	if s.network.bearer.pool.authorize(ctx, s.network.bearer.selection) != nil {
		return ErrDenied
	}
	return s.network.parent(ctx)
}

// Cleanup does not need a live/connected PDN or the currently inserted SIM.
// It does need the original durable claim, namespace and netdev generation.
// This never starts/modifies a modem: only already ACK-owned local objects are
// eligible, with a complete fingerprint check in their pinned transport.
func (s *kernelSecurityLease) cleanupParent(ctx context.Context) (bool, error) {
	if s == nil || s.closed || s.record() == nil || ctx.Err() != nil {
		return false, ErrSecurityScope
	}
	n := s.network
	if n.bearer.pool.authorize(ctx, n.bearer.selection) != nil {
		return false, ErrDenied
	}
	c := n.bearer.claim
	c.mu.Lock()
	valid := !c.storageFailed && !c.busy && c.record.Token == n.bearer.token && c.record.Network != nil &&
		c.record.Network.Token == n.record.Token && c.record.Network.Security != nil && c.record.Network.Security.Token == s.token
	c.mu.Unlock()
	if !valid {
		return false, ErrJournal
	}
	driver, ok := n.driver.(interface {
		cleanupIdentity(context.Context) (bool, error)
	})
	if !ok {
		return false, ErrNetwork
	}
	return driver.cleanupIdentity(ctx)
}

func (s *kernelSecurityLease) addTC(ctx context.Context, op securityTCOperation) error {
	if err := s.parent(ctx); err != nil {
		return err
	}
	_, present, err := s.tc.observe(op)
	if err != nil || present {
		return ErrSecurityScope
	}
	r := s.record()
	r.Pending = &securityOperation{Kind: "tc", Ordinal: op.Ordinal}
	if err := s.save(ctx, r, "preparing"); err != nil {
		return err
	}
	if err := s.tc.apply(op); err != nil {
		if errors.Is(err, ErrSecurityTCConflict) || errors.Is(err, ErrSecurityTCMissing) {
			r = s.record()
			r.Pending = nil
			if saveErr := s.save(ctx, r, "preparing"); saveErr != nil {
				return saveErr
			}
			return err
		}
		s.poison(ctx)
		return ErrUncertain
	}
	hash, present, err := s.tc.observe(op)
	if err != nil || !present || !hashString(hash) {
		s.poison(ctx)
		return ErrUncertain
	}
	r = s.record()
	r.Operations = append(r.Operations, securityOperation{Kind: "tc", Ordinal: op.Ordinal, Hash: hash})
	r.Pending = nil
	return s.save(ctx, r, "preparing")
}

func tcLedgerOperation(op securityOperation) securityTCOperation {
	if op.Ordinal == 0 {
		return securityTCOperation{Kind: "clsact"}
	}
	return securityTCOperation{Kind: "flower", Ordinal: op.Ordinal}
}

func securityAgreement(config imssec.Config) securityAgreementRecord {
	local, _ := netip.AddrFromSlice(config.LocalIP)
	remote, _ := netip.AddrFromSlice(config.RemoteIP)
	return securityAgreementRecord{Local: local.Unmap(), Remote: remote.Unmap(), Integrity: imssec.IntegrityAlgorithm(config), Encryption: imssec.EncryptionAlgorithm(config),
		UEClientSPI: config.UEClientSPI, UEServerSPI: config.UEServerSPI, PCSCFClientSPI: config.PCSCFClientSPI, PCSCFServerSPI: config.PCSCFServerSPI,
		UEClientPort: config.UEClientPort, UEServerPort: config.UEServerPort, PCSCFClientPort: config.PCSCFClientPort, PCSCFServerPort: config.PCSCFServerPort}
}

func decodeSecurityXFRMKey(op securityOperation) (scopedXFRMKey, error) {
	var key scopedXFRMKey
	decoder := json.NewDecoder(bytes.NewReader(op.Key))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&key) != nil || decoder.Decode(new(any)) != io.EOF || validateScopedXFRMKey(key) != nil {
		return key, ErrSecurityScope
	}
	return key, nil
}

func expectedSecurityXFRMKey(record *securityClaim, ordinal uint8) (scopedXFRMKey, error) {
	if record == nil || record.Agreement == nil || ordinal >= 12 {
		return scopedXFRMKey{}, ErrSecurityScope
	}
	a := record.Agreement
	config := imssec.Config{LocalIP: a.Local.AsSlice(), RemoteIP: a.Remote.AsSlice(),
		EncryptionAlgorithm: a.Encryption, IntegrityAlgorithm: a.Integrity,
		UEClientSPI: a.UEClientSPI, UEServerSPI: a.UEServerSPI, PCSCFClientSPI: a.PCSCFClientSPI, PCSCFServerSPI: a.PCSCFServerSPI,
		UEClientPort: a.UEClientPort, UEServerPort: a.UEServerPort, PCSCFClientPort: a.PCSCFClientPort, PCSCFServerPort: a.PCSCFServerPort}
	// The portable selector planner validates key lengths but never reads or
	// retains key bytes. Public zero placeholders reconstruct selectors only;
	// they must NEVER be passed to Add/Install or treated as recovered keys.
	config.EncryptionKey = make([]byte, map[string]int{"null": 0, "aes-cbc": 16, "des-ede3-cbc": 24}[a.Encryption])
	config.IntegrityKey = make([]byte, map[string]int{"hmac-md5-96": 16, "hmac-sha-1-96": 20}[a.Integrity])
	plan, err := imssec.BuildScopedPlan(config, record.ClientReqID, record.ServerReqID)
	if err != nil {
		return scopedXFRMKey{}, ErrSecurityScope
	}
	if ordinal < 4 {
		state := plan.States[ordinal]
		mark := record.Mark
		if record.SocketOutbound && (ordinal == 0 || ordinal == 3) {
			mark = 0
		}
		return scopedXFRMKey{Kind: "state", Src: state.Src, Dst: state.Dst, SPI: state.SPI, Protocol: unix.IPPROTO_ESP, Mark: mark}, nil
	}
	p := plan.Policies[ordinal-4]
	direction := uint8(0)
	if p.Direction == "out" {
		direction = 1
	}
	return scopedXFRMKey{Kind: "policy", Src: p.Src.Addr(), Dst: p.Dst.Addr(), SrcPort: p.SrcPort, DstPort: p.DstPort,
		Protocol: p.Protocol, Direction: direction, Mark: record.Mark}, nil
}

func securityLedgerKey(record *securityClaim, op securityOperation) (scopedXFRMKey, error) {
	key, err := decodeSecurityXFRMKey(op)
	expected, expectedErr := expectedSecurityXFRMKey(record, op.Ordinal)
	if err != nil || expectedErr != nil || key != expected {
		return key, ErrSecurityScope
	}
	return key, nil
}

func securitySnapshotMatches(record *securityClaim, op securityOperation, snapshot scopedXFRMSnapshot) bool {
	if record == nil || record.Agreement == nil || snapshot.Fingerprint != op.Hash || snapshot.MatchFingerprint != op.Shape || len(snapshot.reqids) != 1 {
		return false
	}
	clientPair := op.Ordinal < 2 || op.Ordinal >= 4 && op.Ordinal < 8
	reqid := record.ServerReqID
	if clientPair {
		reqid = record.ClientReqID
	}
	if snapshot.reqids[0] != reqid {
		return false
	}
	return op.Ordinal >= 4 || snapshot.EncryptionAlgorithm == record.Agreement.Encryption && snapshot.IntegrityAlgorithm == record.Agreement.Integrity
}

// Validate the complete acknowledged XFRM prefix before deleting its first
// object. A corrupted agreement must not delete policies and only later fail
// when reaching a mismatched state at the end of reverse-order cleanup.
func (s *kernelSecurityLease) validateXFRMLedger(ctx context.Context) error {
	r := s.record()
	if r == nil {
		return ErrSecurityScope
	}
	for _, op := range r.Operations {
		if op.Kind != "xfrm" {
			continue
		}
		key, err := securityLedgerKey(r, op)
		if err != nil {
			return err
		}
		snapshot, present, err := s.xfrm.Observe(ctx, key)
		if err != nil || present && !securitySnapshotMatches(r, op, snapshot) {
			return ErrSecurityScope
		}
	}
	return nil
}

func (s *kernelSecurityLease) addXFRM(ctx context.Context, ordinal uint8, op scopedXFRMOperation) error {
	if err := s.parent(ctx); err != nil {
		return err
	}
	_, present, err := s.xfrm.Observe(ctx, op.Key)
	if err != nil || present {
		return ErrXFRMConflict
	}
	key, err := json.Marshal(op.Key)
	if err != nil {
		return ErrJournal
	}
	entry := securityOperation{Kind: "xfrm", Ordinal: ordinal, Key: key, Shape: op.Fingerprint}
	r := s.record()
	r.Pending = &entry
	if err := s.save(ctx, r, "preparing"); err != nil {
		return err
	}
	if err := s.xfrm.Add(ctx, op); err != nil {
		if errors.Is(err, ErrXFRMExists) || errors.Is(err, ErrXFRMRejected) || errors.Is(err, ErrXFRMSchema) {
			r = s.record()
			r.Pending = nil
			if saveErr := s.save(ctx, r, "preparing"); saveErr != nil {
				return saveErr
			}
			return err
		}
		s.poison(ctx)
		return ErrUncertain
	}
	snapshot, present, err := s.xfrm.Observe(ctx, op.Key)
	if err != nil || !present || snapshot.MatchFingerprint != op.Fingerprint || !hashString(snapshot.Fingerprint) {
		s.poison(ctx)
		return ErrUncertain
	}
	entry.Hash = snapshot.Fingerprint
	r = s.record()
	r.Operations = append(r.Operations, entry)
	r.Pending = nil
	return s.save(ctx, r, "preparing")
}

func (s *kernelSecurityLease) InstallIPSec(ctx context.Context, config imssec.Config) (imssec.Handle, error) {
	copyConfig := imssec.CloneConfig(config)
	var result *kernelSecuritySAHandle
	ownedMutation := false // touched only by this gate-owning worker/callback
	err := runKernelSecurity(ctx, s.network, func(worker context.Context) error {
		if err := s.network.verify(worker); err != nil || s.blocked.Load() {
			return ErrSecurityScope
		}
		r := s.record()
		if r == nil || r.Phase != "listening" || r.Pending != nil || s.active != nil || r.Agreement != nil {
			return ErrBusy
		}
		agreement := securityAgreement(copyConfig)
		if !validSecurityAgreement(agreement, r.Flows) {
			return ErrSecurityScope
		}
		plan, err := buildScopedXFRMPlanMode(copyConfig, r.Mark, r.ClientReqID, r.ServerReqID, r.SocketOutbound)
		if err != nil {
			return err
		}
		defer plan.Clear()
		r.Agreement, r.Phase = &agreement, "installing"
		if err := s.save(worker, r, "preparing"); err != nil {
			return err
		}
		ownedMutation = true
		for index, op := range plan.Operations {
			if ctx != nil && ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			if err = s.addXFRM(worker, uint8(index), op); err != nil {
				break
			}
		}
		if err != nil {
			cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
			cleanupErr := s.removeXFRM(cleanup)
			finish()
			return errors.Join(err, cleanupErr)
		}
		r = s.record()
		r.Phase = "active"
		if err := s.save(worker, r, "active"); err != nil {
			return err
		}
		result = &kernelSecuritySAHandle{owner: s}
		s.active = result
		return nil
	}, func(cleanup context.Context) {
		// A cancelled, rejected second Install never acquired this agreement.
		// In particular it must not dismantle an already active session.
		if ownedMutation {
			_ = s.removeXFRM(cleanup)
		}
	}, func() {
		clear(copyConfig.EncryptionKey)
		clear(copyConfig.IntegrityKey)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *kernelSecurityLease) RetainIPSec(handle imssec.Handle) {
	// No I/O, mutex wait or retry. The network claim already roots this owner
	// and all recorded kernel objects. Never turn failed Start into GC cleanup.
	s.blocked.Store(true)
}

func (h *kernelSecuritySAHandle) Close(ctx context.Context) error {
	if h == nil || h.closed.Load() {
		return nil
	}
	return runKernelSecurity(ctx, h.owner.network, func(worker context.Context) error {
		if h.closed.Load() {
			return nil
		}
		if h.owner.active != h {
			return ErrSecurityScope
		}
		return h.owner.removeXFRM(worker)
	}, nil, nil)
}

func (s *kernelSecurityLease) removeOne(ctx context.Context, index int) error {
	alive, err := s.cleanupParent(ctx)
	if err != nil {
		return err
	}
	r := s.record()
	if r == nil || r.Pending != nil || r.Phase == "uncertain" || index < 0 || index >= len(r.Operations) {
		return ErrUncertain
	}
	op := r.Operations[index]
	var present bool
	var hash string
	var key scopedXFRMKey
	if op.Kind == "tc" {
		if alive {
			hash, present, err = s.tc.observe(tcLedgerOperation(op))
		}
		// A proven deleted original netdev lost its TC objects with it. Do not
		// query/delete a replacement interface that reused the same ifindex.
	} else {
		key, err = securityLedgerKey(r, op)
		if err != nil {
			return ErrSecurityScope
		}
		var snapshot scopedXFRMSnapshot
		snapshot, present, err = s.xfrm.Observe(ctx, key)
		hash = snapshot.Fingerprint
		if present && !securitySnapshotMatches(r, op, snapshot) {
			return ErrSecurityScope
		}
	}
	if err != nil || present && hash != op.Hash {
		return ErrSecurityScope
	}
	if present {
		pending := op
		pending.Kind = "remove-" + pending.Kind
		r.Pending = &pending
		if err := s.save(ctx, r, "preparing"); err != nil {
			return err
		}
		if op.Kind == "tc" {
			err = s.tc.remove(tcLedgerOperation(op), op.Hash)
		} else {
			err = s.xfrm.Delete(ctx, key, op.Hash)
		}
		if err != nil {
			if errors.Is(err, ErrSecurityTCConflict) || errors.Is(err, ErrXFRMRejected) || errors.Is(err, ErrXFRMConflict) {
				r = s.record()
				r.Pending = nil
				if saveErr := s.save(ctx, r, "preparing"); saveErr != nil {
					return saveErr
				}
				return err
			}
			s.poison(ctx)
			return ErrUncertain
		}
	}
	r = s.record()
	r.Pending = nil
	r.Operations = append(r.Operations[:index], r.Operations[index+1:]...)
	return s.save(ctx, r, "preparing")
}

func (s *kernelSecurityLease) removeXFRM(ctx context.Context) error {
	r := s.record()
	if r == nil || r.Pending != nil || r.Phase == "uncertain" {
		return ErrUncertain
	}
	if err := s.validateXFRMLedger(ctx); err != nil {
		return err
	}
	r.Phase = "cleaning"
	if err := s.save(ctx, r, "preparing"); err != nil {
		return err
	}
	for index := len(r.Operations) - 1; index >= 0; index-- {
		if r.Operations[index].Kind == "xfrm" {
			if err := s.removeOne(ctx, index); err != nil {
				s.blocked.Store(true)
				return err
			}
		}
	}
	r = s.record()
	r.Agreement, r.Phase = nil, "listening"
	if err := s.save(ctx, r, "active"); err != nil {
		return err
	}
	if s.active != nil {
		s.active.closed.Store(true)
		s.active = nil
	}
	s.blocked.Store(false)
	return nil
}

func (s *kernelSecurityLease) cleanup(ctx context.Context) error {
	if s == nil || s.closed {
		return nil
	}
	r := s.record()
	if r == nil || r.Pending != nil || r.Phase == "uncertain" {
		return ErrUncertain
	}
	if err := s.validateXFRMLedger(ctx); err != nil {
		return err
	}
	s.blocked.Store(true)
	r.Phase = "cleaning"
	if err := s.save(ctx, r, "preparing"); err != nil {
		return err
	}
	for index := len(r.Operations) - 1; index >= 0; index-- {
		if err := s.removeOne(ctx, index); err != nil {
			return err
		}
	}
	if err := s.reservations.release(); err != nil {
		return err
	}
	if s.xfrm != nil {
		s.xfrm.Close()
	}
	if s.tc != nil {
		s.tc.close()
	}
	if err := s.save(ctx, nil, "active"); err != nil {
		return err
	}
	if s.active != nil {
		s.active.closed.Store(true)
		s.active = nil
	}
	s.closed, s.network.security = true, nil
	return nil
}

func (s *kernelSecurityLease) Close(ctx context.Context) error {
	return runKernelSecurity(ctx, s.network, s.cleanup, nil, nil)
}
func (n *kernelNetworkLease) closeSecurity(ctx context.Context) error {
	return runKernelSecurity(ctx, n, func(worker context.Context) error {
		if n.security == nil {
			if n.record.Security != nil {
				return ErrUncertain
			}
			return nil
		}
		return n.security.cleanup(worker)
	}, nil, nil)
}

func (s *kernelSecurityLease) verifyHeld(ctx context.Context) error {
	r := s.record()
	if r == nil || !s.selectedFlowsMatch(r) || s.closed || s.blocked.Load() || r.Pending != nil || r.Phase != "listening" && r.Phase != "active" {
		return ErrSecurityScope
	}
	tcCount, xfrmCount := 0, 0
	for _, op := range r.Operations {
		var hash string
		var exists bool
		var err error
		if op.Kind == "tc" {
			tcCount++
			hash, exists, err = s.tc.observe(tcLedgerOperation(op))
		} else {
			xfrmCount++
			key, decodeErr := securityLedgerKey(r, op)
			if decodeErr != nil {
				return ErrSecurityScope
			}
			snapshot, present, readErr := s.xfrm.Observe(ctx, key)
			hash, exists, err = snapshot.Fingerprint, present, readErr
			if present && !securitySnapshotMatches(r, op, snapshot) {
				return ErrSecurityScope
			}
		}
		if err != nil || !exists || hash != op.Hash {
			return ErrSecurityScope
		}
	}
	if tcCount != len(r.Flows)+1 || r.Phase == "listening" && (xfrmCount != 0 || r.Agreement != nil) ||
		r.Phase == "active" && (xfrmCount != 12 || r.Agreement == nil) {
		return ErrSecurityScope
	}
	return nil
}

func (s *kernelSecurityLease) markSocket(raw syscall.RawConn) error {
	r := s.record()
	if r == nil || s.closed || s.blocked.Load() || raw == nil {
		return ErrSecurityScope
	}
	result := ErrSecurityScope
	if err := raw.Control(func(fd uintptr) {
		old, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
		if err != nil || old != 0 && uint32(old) != r.Mark {
			return
		}
		if unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(r.Mark)) != nil {
			return
		}
		got, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
		if err == nil && uint32(got) == r.Mark {
			result = ""
		}
	}); err != nil || result != "" {
		return ErrSecurityScope
	}
	return nil
}

func (s *kernelSecurityLease) BindSocket(ctx context.Context, network, address string, raw syscall.RawConn) error {
	return s.network.BindSocket(ctx, network, address, raw)
}
func (s *kernelSecurityLease) AcquireMediaRoute(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
	return s.network.AcquireMediaRoute(ctx, local, remote)
}
func (s *kernelSecurityLease) BindProtectedSocket(ctx context.Context, transport string, binding imssec.SocketBinding, raw syscall.RawConn) error {
	if binding.Validate(transport) != nil {
		return ErrParameters
	}
	return runKernelSecurity(ctx, s.network, func(worker context.Context) error {
		if err := s.network.verify(worker); err != nil {
			return err
		}
		r := s.record()
		found := false
		for _, flow := range r.Flows {
			found = found || flow.Local == binding.Local.Addr() && flow.Remote == binding.Remote
		}
		if !found {
			return ErrPCSCF
		}
		stream := strings.HasPrefix(transport, "tcp")
		if err := s.network.driver.bindSocket(raw, binding.Local.Addr().Is4(), stream); err != nil {
			return err
		}
		if err := s.markSocket(raw); err != nil {
			return err
		}
		role, reqid := SocketSecurityClient, r.ClientReqID
		if binding.Role == imssec.ServerSocket {
			role, reqid = SocketSecurityServer, r.ServerReqID
		}
		protocol := uint8(unix.IPPROTO_UDP)
		if stream {
			protocol = unix.IPPROTO_TCP
		}
		return RequireSocketESP(raw, SocketSecurityPolicy{Local: binding.Local.Addr(), Remote: binding.Remote,
			LocalPort: binding.Local.Port(), Protocol: protocol, Role: role, ReqID: reqid})
	}, nil, nil)
}

func (n *kernelNetworkLease) lookupEndpoint(endpoint Endpoint) error {
	if n.record.Security == nil {
		return n.driver.lookup(endpoint, n.record.Table)
	}
	k, ok := n.driver.(interface {
		lookupMarked(Endpoint, int, uint32) error
	})
	if !ok || n.security == nil {
		return ErrSecurityScope
	}
	return k.lookupMarked(endpoint, n.record.Table, n.record.Security.Mark)
}
