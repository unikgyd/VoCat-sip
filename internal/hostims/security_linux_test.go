//go:build linux

package hostims

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi/imssec"
)

type securityFakeNetwork struct {
	values map[string]string
	marked []uint32
}

func (*securityFakeNetwork) identity() (string, int, string, string) {
	return "wwan-test", 4, hashText("ns"), hashText("anchor")
}
func (*securityFakeNetwork) verify(ctx context.Context) error { return ctx.Err() }
func (*securityFakeNetwork) cleanupIdentity(ctx context.Context) (bool, error) {
	return true, ctx.Err()
}
func (*securityFakeNetwork) tableFree(int) error { return nil }
func securityFakeKey(op networkOperation) string {
	return op.Kind + ":" + op.Local.String() + ":" + op.Remote.String()
}
func (d *securityFakeNetwork) observe(op networkOperation) (string, bool, error) {
	if hash, ok := d.values[securityFakeKey(op)]; ok {
		return hash, true, nil
	}
	return "", false, ErrNetworkMissing
}
func (d *securityFakeNetwork) apply(op networkOperation) error {
	d.values[securityFakeKey(op)] = hashText(securityFakeKey(op))
	return nil
}
func (d *securityFakeNetwork) remove(op networkOperation) error {
	delete(d.values, securityFakeKey(op))
	return nil
}
func (*securityFakeNetwork) lookup(Endpoint, int) error { return nil }
func (d *securityFakeNetwork) lookupMarked(_ Endpoint, _ int, mark uint32) error {
	d.marked = append(d.marked, mark)
	return nil
}
func (*securityFakeNetwork) bindSocket(syscall.RawConn, bool, bool) error { return nil }
func (*securityFakeNetwork) close()                                       {}

type securityFakeXFRM struct {
	mu                             sync.Mutex
	objects                        map[scopedXFRMKey]scopedXFRMSnapshot
	adds, deletes                  int
	failAt                         int
	unknown                        bool
	entered, release               chan struct{}
	observeEntered, observeRelease chan struct{}
}

func (*securityFakeXFRM) ScopeVacant(context.Context, uint32, uint32, uint32) error { return nil }
func (f *securityFakeXFRM) Add(ctx context.Context, op scopedXFRMOperation) error {
	f.mu.Lock()
	f.adds++
	count := f.adds
	entered, release := f.entered, f.release
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ErrXFRMUnknown
		}
	}
	// Validate after a delayed ACK barrier too: caller cancellation/zeroing
	// must not erase the worker-owned plan/key bytes before kernel submission.
	snapshot, err := scopedXFRMSnapshotOf(op.Key.Kind, op.wireAdd)
	if err != nil || snapshot.MatchFingerprint != op.Fingerprint {
		return ErrXFRMSchema
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if count == f.failAt && !f.unknown {
		return ErrXFRMRejected
	}
	if _, exists := f.objects[op.Key]; exists {
		return ErrXFRMExists
	}
	snapshot.Fingerprint = hashText("assigned:" + op.Fingerprint)
	f.objects[op.Key] = snapshot
	if count == f.failAt {
		return ErrXFRMUnknown
	}
	return nil
}
func (f *securityFakeXFRM) Observe(_ context.Context, key scopedXFRMKey) (scopedXFRMSnapshot, bool, error) {
	f.mu.Lock()
	entered, release := f.observeEntered, f.observeRelease
	s, ok := f.objects[key]
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	return s, ok, nil
}
func (f *securityFakeXFRM) Delete(_ context.Context, key scopedXFRMKey, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.objects[key]
	if !ok || s.Fingerprint != hash {
		return ErrXFRMConflict
	}
	delete(f.objects, key)
	f.deletes++
	return nil
}
func (*securityFakeXFRM) Close() error { return nil }

type securityFakeTC struct {
	operationsList []securityTCOperation
	objects        map[securityTCOperation]string
	failAt         uint8
	unknown        bool
	deletes        int
}

func (f *securityFakeTC) operations() []securityTCOperation {
	return append([]securityTCOperation(nil), f.operationsList...)
}
func (f *securityFakeTC) observe(op securityTCOperation) (string, bool, error) {
	h, ok := f.objects[op]
	return h, ok, nil
}
func (f *securityFakeTC) apply(op securityTCOperation) error {
	if op.Ordinal == f.failAt && !f.unknown {
		return ErrSecurityTCConflict
	}
	f.objects[op] = hashText(fmt.Sprint(op))
	if op.Ordinal == f.failAt {
		return ErrSecurityTCUnknown
	}
	return nil
}
func (f *securityFakeTC) remove(op securityTCOperation, hash string) error {
	if f.objects[op] != hash {
		return ErrSecurityTCConflict
	}
	delete(f.objects, op)
	f.deletes++
	return nil
}
func (*securityFakeTC) close() {}

func securityOwnerFixture(t *testing.T) (*kernelNetworkLease, *kernelSecurityLease, *securityFakeXFRM, *securityFakeTC, *memoryClaims) {
	t.Helper()
	pool, _, journal, selection := bearerFixture(t)
	bearer, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	driver := &securityFakeNetwork{values: map[string]string{}}
	runtime := Runtime{APN: "ims", IPv4: bearer.configuration.Addresses[0].Address, IPv6: bearer.configuration.Addresses[1].Address, PCSCF: []netip.Addr{netip.MustParseAddr("198.51.100.20"), netip.MustParseAddr("2001:db8:2::20")}}
	n, err := prepareKernelNetworkWith(context.Background(), bearer, runtime, func() (kernelNetworkDriver, error) { return driver, nil })
	if err != nil {
		t.Fatal(err)
	}
	xfrm := &securityFakeXFRM{objects: map[scopedXFRMKey]scopedXFRMSnapshot{}}
	tc := &securityFakeTC{objects: map[securityTCOperation]string{}, failAt: 255}
	factory := func(r *securityClaim) (scopedXFRMAccess, scopedTCAccess, error) {
		tc.operationsList = []securityTCOperation{{Kind: "clsact"}}
		for i := range r.Flows {
			tc.operationsList = append(tc.operationsList, securityTCOperation{Kind: "flower", Ordinal: uint8(i + 1)})
		}
		return xfrm, tc, nil
	}
	s, err := prepareKernelSecurityWith(context.Background(), n, filepath.Join(t.TempDir(), "reservations"), factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if n.security != nil {
			n.security.reservations.closeFDs()
		}
	})
	return n, s, xfrm, tc, journal
}
func securityOwnerConfig() imssec.Config {
	return imssec.Config{LocalIP: netip.MustParseAddr("192.0.2.10").AsSlice(), RemoteIP: netip.MustParseAddr("198.51.100.20").AsSlice(), EncryptionAlgorithm: "aes-cbc", IntegrityAlgorithm: "hmac-sha-1-96", EncryptionKey: []byte(strings.Repeat("K", 16)), IntegrityKey: []byte(strings.Repeat("I", 20)), UEClientSPI: 1001, UEServerSPI: 1002, PCSCFClientSPI: 2001, PCSCFServerSPI: 2002, UEClientPort: 62001, UEServerPort: 62002, PCSCFClientPort: 62003, PCSCFServerPort: 62004}
}

func TestKernelSecurityLargestCombinedWALFitsPrivateJournal(t *testing.T) {
	ctx := context.Background()
	pool, _, memory, selection := bearerFixture(t)
	if _, err := pool.Acquire(ctx, selection, BearerOptions{APN: strings.Repeat("a", 50) + "." + strings.Repeat("b", 49)}); err != nil {
		t.Fatal(err)
	}
	parent := memory.record(selection.key)
	parent.Owner = ":1234567890.1234567890"
	parent.Modem = "/org/freedesktop/ModemManager1/Modem/1234567890"
	parent.Path = "/org/freedesktop/ModemManager1/Bearer/1234567890"
	parent.Before = nil // The active network owner discards its create inventory.
	for index := 0; index < 128; index++ {
		parent.SIMChallenges = append(parent.SIMChallenges, hashText(fmt.Sprintf("maximum challenge %d", index)))
	}

	// Long, synthetic private IPv6 addresses maximize canonical text length.
	// This test generates selectors locally; no packet/kernel/modem I/O occurs.
	local4 := netip.MustParsePrefix("192.0.2.250/32")
	local6 := netip.MustParsePrefix("fd12:3456:789a:bcde:1234:5678:9abc:def0/128")
	gateway6 := netip.MustParseAddr("fd12:3456:789a:bcde:1234:5678:9abc:ffff")
	var endpoints []Endpoint
	for index := 0; index < 8; index++ {
		endpoints = append(endpoints, Endpoint{Local: local6.Addr(), Remote: netip.MustParseAddr(fmt.Sprintf("fd98:7654:3210:fedc:1234:5678:9abc:%04x", 0x1000+index))})
	}
	security, err := newSecurityClaim(endpoints)
	if err != nil {
		t.Fatal(err)
	}
	security.Mark, security.ClientReqID, security.ServerReqID = 0x6fffffff, 0xbffffffe, 0xbfffffff
	config := securityOwnerConfig()
	config.LocalIP, config.RemoteIP = endpoints[0].Local.AsSlice(), endpoints[0].Remote.AsSlice()
	config.EncryptionAlgorithm, config.EncryptionKey = "des-ede3-cbc", []byte(strings.Repeat("K", 24))
	config.UEClientSPI, config.UEServerSPI, config.PCSCFClientSPI, config.PCSCFServerSPI = 0xffffffff, 0xfffffffe, 0xfffffffd, 0xfffffffc
	config.UEClientPort, config.UEServerPort, config.PCSCFClientPort, config.PCSCFServerPort = 65535, 65534, 65533, 65532
	defer clear(config.EncryptionKey)
	defer clear(config.IntegrityKey)
	agreement := securityAgreement(config)
	security.Agreement, security.Phase = &agreement, "active"
	for ordinal := 0; ordinal <= len(endpoints); ordinal++ {
		security.Operations = append(security.Operations, securityOperation{Kind: "tc", Ordinal: uint8(ordinal), Hash: hashText(fmt.Sprintf("maximum TC %d", ordinal))})
	}
	plan, err := buildScopedXFRMPlan(config, security.Mark, security.ClientReqID, security.ServerReqID)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Clear()
	for ordinal, operation := range plan.Operations {
		key, err := json.Marshal(operation.Key)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := scopedXFRMSnapshotOf(operation.Key.Kind, operation.wireAdd)
		if err != nil {
			t.Fatal(err)
		}
		entry := securityOperation{Kind: "xfrm", Ordinal: uint8(ordinal), Key: key, Shape: operation.Fingerprint, Hash: snapshot.Fingerprint}
		if decoded, err := securityLedgerKey(security, entry); err != nil || decoded != operation.Key {
			t.Fatal("generated key is not the canonical agreement selector", err)
		}
		security.Operations = append(security.Operations, entry)
	}
	network := claimNetworkFixture()
	network.Interface, network.Index, network.Table = "qmimux123456789", 2147483647, 0x4fffffff
	network.Security, network.Operations = security, nil
	for _, local := range []netip.Prefix{local4, local6} {
		for _, kind := range []string{"address", "rule"} {
			network.Operations = append(network.Operations, networkOperation{Kind: kind, Local: local, Hash: hashText(kind + local.String()), Owned: true})
		}
	}
	for index, endpoint := range endpoints {
		network.Operations = append(network.Operations, networkOperation{Kind: "route", Local: local6, Remote: endpoint.Remote, Gateway: gateway6, MTU: 65535, Hash: hashText(fmt.Sprintf("signaling %d", index)), Owned: true})
	}
	for index := 0; index < 8; index++ {
		remote := netip.MustParseAddr(fmt.Sprintf("fd98:7654:3210:fedc:1234:5678:9abc:%04x", 0x2000+index))
		network.Operations = append(network.Operations, networkOperation{Kind: "route", Local: local6, Remote: remote, Gateway: gateway6, MTU: 65535, Hash: hashText(fmt.Sprintf("media %d", index)), Owned: true, Media: true})
	}
	parent.Network = network
	if len(security.Flows) != 8 || len(security.Operations) != 21 || len(plan.Operations) != 12 || len(network.Operations) != 20 || len(parent.SIMChallenges) != 128 {
		t.Fatal("capacity fixture does not fill all supported collections")
	}

	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "maximum-private-wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	journal := SQLiteClaimJournal{Database: database}
	var previous []byte
	maximum := 0
	for _, mode := range []string{"active", "pending-security-removal", "pending-route-removal"} {
		t.Run(mode, func(t *testing.T) {
			record := parent
			record.Network = cloneNetworkClaim(network)
			switch mode {
			case "pending-security-removal":
				pending := record.Network.Security.Operations[20]
				pending.Kind = "remove-xfrm"
				record.Network.Security.Pending, record.Network.Security.Phase, record.Network.Phase = &pending, "cleaning", "preparing"
			case "pending-route-removal":
				pending := record.Network.Operations[19]
				pending.Kind = "remove-route"
				record.Network.Pending, record.Network.Phase = &pending, "preparing"
			}
			raw, err := json.Marshal(record)
			if err != nil || len(raw) > store.MaxHostIMSClaimBytes {
				t.Fatalf("combined supported WAL exceeds private limit: %d / %d: %v", len(raw), store.MaxHostIMSClaimBytes, err)
			}
			if _, err := decodeClaim(raw); err != nil {
				t.Fatal("maximum combined WAL is not schema-valid", err)
			}
			if ok, err := journal.Compare(ctx, selection.key, previous, raw); err != nil || !ok {
				t.Fatal("private journal refused maximum combined WAL", err)
			}
			stored, err := journal.Read(ctx, selection.key)
			if err != nil || !bytes.Equal(stored, raw) {
				t.Fatal("maximum combined WAL did not round-trip exactly", err)
			}
			decoded, err := decodeClaim(stored)
			if err != nil || len(decoded.Network.Security.Operations) != 21 || len(decoded.Network.Operations) != 20 || len(decoded.SIMChallenges) != 128 {
				t.Fatal("maximum combined WAL lost bounded collections", err)
			}
			for _, operation := range decoded.Network.Security.Operations {
				if operation.Kind == "xfrm" {
					if _, err := securityLedgerKey(decoded.Network.Security, operation); err != nil {
						t.Fatal("canonical selector changed across journal round-trip", err)
					}
				}
			}
			previous = append([]byte(nil), stored...)
			if len(raw) > maximum {
				maximum = len(raw)
			}
			t.Logf("combined WAL %s: %d / %d bytes", mode, len(raw), store.MaxHostIMSClaimBytes)
		})
	}
	if ok, err := journal.Compare(ctx, selection.key, previous, nil); err != nil || !ok {
		t.Fatal("maximum WAL exact-owner delete failed", err)
	}
	t.Logf("maximum combined supported WAL: %d / %d bytes", maximum, store.MaxHostIMSClaimBytes)
}

func TestKernelSecurityOwnerInstallsRecordsAndClosesOnlyOwnedObjects(t *testing.T) {
	n, s, x, tc, j := securityOwnerFixture(t)
	if err := n.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := s.InstallIPSec(context.Background(), securityOwnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallIPSec(context.Background(), securityOwnerConfig()); !errors.Is(err, ErrBusy) {
		t.Fatal("second agreement admitted", err)
	}
	r := j.record(n.bearer.selection.key).Network.Security
	if len(r.Operations) != 15 || r.Agreement == nil || r.Pending != nil || r.Phase != "active" {
		t.Fatal("incomplete acknowledged ledger")
	}
	if err := n.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("parent networking torn down before security", err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if x.deletes != 12 || s.record().Agreement != nil || s.record().Phase != "listening" {
		t.Fatal("SA cleanup deleted TC or lost ledger")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tc.deletes != 3 || n.record.Security != nil || n.security != nil {
		t.Fatal("scope not fully reclaimed")
	}
	if err := n.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKernelSecurityOwnerKnownRejectionRollsBackAcknowledgedPrefix(t *testing.T) {
	n, s, x, _, _ := securityOwnerFixture(t)
	x.failAt = 6
	if h, err := s.InstallIPSec(context.Background(), securityOwnerConfig()); h != nil || !errors.Is(err, ErrXFRMRejected) {
		t.Fatal(err)
	}
	if x.deletes != 5 || len(x.objects) != 0 || s.record().Phase != "listening" || s.record().Pending != nil {
		t.Fatal("known rejected object acquired ownership")
	}
	if err := n.Verify(context.Background()); err != nil {
		t.Fatal("known rejection permanently fenced clean owner", err)
	}
}

func TestKernelSecurityOwnerLostACKNeverGuessesOwnership(t *testing.T) {
	n, s, x, _, j := securityOwnerFixture(t)
	x.failAt, x.unknown = 3, true
	if h, err := s.InstallIPSec(context.Background(), securityOwnerConfig()); h != nil || !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	r := j.record(n.bearer.selection.key).Network.Security
	if r.Pending == nil || r.Pending.Kind != "xfrm" || r.Phase != "uncertain" || len(r.Operations) != 5 || x.deletes != 0 || len(x.objects) != 3 {
		t.Fatal("unknown matching kernel object was adopted or deleted")
	}
	if err := s.Close(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatal("unknown ACK cleared by retry", err)
	}
	if err := n.Close(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("parent released unresolved scope", err)
	}
}

func TestKernelSecurityOwnerCancellationObservesLateInstallAndErasesNoLivePlan(t *testing.T) {
	n, s, x, _, _ := securityOwnerFixture(t)
	x.entered = make(chan struct{}, 1)
	x.release = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	config := securityOwnerConfig()
	done := make(chan error, 1)
	go func() { _, err := s.InstallIPSec(ctx, config); done <- err }()
	select {
	case <-x.entered:
	case <-time.After(time.Second):
		t.Fatal("install did not enter owned worker")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for kernel")
	}
	clear(config.EncryptionKey)
	clear(config.IntegrityKey)
	close(x.release)
	if err := n.locked(context.Background(), func(context.Context) error {
		if s.record().Pending != nil || s.record().Phase != "listening" || len(s.record().Operations) != 3 {
			return ErrUncertain
		}
		return nil
	}); err != nil {
		t.Fatal("late install was lost or early key erasure corrupted the plan", err)
	}
	if x.deletes != 1 || len(x.objects) != 0 {
		t.Fatal("late acknowledged state not reclaimed")
	}
}

func TestKernelSecurityOwnerRetainAndForeignReplacementFence(t *testing.T) {
	n, s, x, _, _ := securityOwnerFixture(t)
	h, err := s.InstallIPSec(context.Background(), securityOwnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.RetainIPSec(h)
	if err := n.Verify(context.Background()); err == nil {
		t.Fatal("retained cleanup reported usable")
	}
	r := s.record()
	last := r.Operations[len(r.Operations)-1]
	key, _ := decodeSecurityXFRMKey(last)
	x.mu.Lock()
	original := x.objects[key]
	replacement := original
	replacement.Fingerprint = hashText("foreign replacement")
	x.objects[key] = replacement
	x.mu.Unlock()
	if err := h.Close(context.Background()); err == nil || x.deletes != 0 {
		t.Fatal("foreign replacement deleted")
	}
	x.mu.Lock()
	x.objects[key] = original
	x.mu.Unlock()
	if err := h.Close(context.Background()); err != nil {
		t.Fatal("known unmodified ledger could not be retried", err)
	}
	if err := n.Verify(context.Background()); err != nil {
		t.Fatal("ACK cleanup did not release retain fence", err)
	}
}

func TestKernelSecurityOwnerChangedPCSCFAndTamperedLedgerRefused(t *testing.T) {
	_, s, x, _, _ := securityOwnerFixture(t)
	config := securityOwnerConfig()
	config.RemoteIP = netip.MustParseAddr("198.51.100.99").AsSlice()
	if h, err := s.InstallIPSec(context.Background(), config); h != nil || err == nil || x.adds != 0 {
		t.Fatal("unproven peer installed")
	}
	h, err := s.InstallIPSec(context.Background(), securityOwnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := s.record()
	index := len(r.Operations) - 1
	entry := r.Operations[index]
	key, _ := decodeSecurityXFRMKey(entry)
	key.DstPort++
	// Corrupt in-memory scalar key while preserving its old hash; strict
	// agreement reconstruction must reject it BEFORE querying/deleting.
	encoded, _ := jsonMarshalSecurityKey(key)
	s.network.record.Security.Operations[index].Key = encoded
	if err := h.Close(context.Background()); err == nil || x.deletes != 0 {
		t.Fatal("journal key escaped agreed selectors")
	}
}

func jsonMarshalSecurityKey(k scopedXFRMKey) ([]byte, error) { return json.Marshal(k) }

func TestKernelSecurityCancelledRejectedInstallDoesNotTearDownExistingAgreement(t *testing.T) {
	n, s, x, _, _ := securityOwnerFixture(t)
	h, err := s.InstallIPSec(context.Background(), securityOwnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	x.observeEntered = make(chan struct{}, 1)
	x.observeRelease = make(chan struct{})
	entered, release := x.observeEntered, x.observeRelease
	x.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.InstallIPSec(ctx, securityOwnerConfig()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second install did not reach verification barrier")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := n.locked(context.Background(), func(context.Context) error {
		if s.record().Phase != "active" || s.active != h || x.deletes != 0 || len(s.record().Operations) != 15 {
			return ErrSecurityScope
		}
		return nil
	}); err != nil {
		t.Fatal("cancelled unadmitted install deleted old agreement", err)
	}
}

func TestKernelSecurityOwnerRejectsChangedScopeAndAgreementMetadata(t *testing.T) {
	for _, field := range []string{"client reqid", "server reqid", "mark", "flow", "encryption", "integrity", "shape"} {
		t.Run(field, func(t *testing.T) {
			n, s, x, _, _ := securityOwnerFixture(t)
			if field != "flow" {
				if _, err := s.InstallIPSec(context.Background(), securityOwnerConfig()); err != nil {
					t.Fatal(err)
				}
			}
			r := n.record.Security
			switch field {
			case "client reqid":
				r.ClientReqID ^= 16
			case "server reqid":
				r.ServerReqID ^= 16
			case "mark":
				r.Mark ^= 16
			case "flow":
				r.Flows[0].Remote = netip.MustParseAddr("198.51.100.99")
			case "encryption":
				r.Agreement.Encryption = "null"
			case "integrity":
				r.Agreement.Integrity = "hmac-md5-96"
			case "shape":
				r.Operations[len(r.Operations)-1].Shape = hashText("wrong desired shape")
			}
			if err := n.Verify(context.Background()); err == nil {
				t.Fatal("mismatched immutable scope/agreement verified")
			}
			if err := s.Close(context.Background()); err == nil || x.deletes != 0 {
				t.Fatal("corrupt ledger partially deleted objects")
			}
		})
	}
}

func TestKernelSecurityCleanupOnlyRecoveryUsesOriginalReservationsAndLedger(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			n, s, x, tc, j := securityOwnerFixture(t)
			if unknown {
				x.failAt, x.unknown = 3, true
			}
			_, err := s.InstallIPSec(context.Background(), securityOwnerConfig())
			if !unknown && err != nil {
				t.Fatal(err)
			}
			directory := s.reservations.directory.Name()
			s.reservations.closeFDs()
			s.xfrm.Close()
			s.tc.close()
			n.security = nil
			n.record = cloneNetworkClaim(j.record(n.bearer.selection.key).Network)
			err = recoverKernelSecurityWith(context.Background(), n, directory, func(*securityClaim) (scopedXFRMAccess, scopedTCAccess, error) { return x, tc, nil })
			if unknown {
				if !errors.Is(err, ErrUncertain) || x.deletes != 0 || n.record.Security == nil {
					t.Fatal("unknown ACK inferred owned during recovery", err)
				}
				return
			}
			if err != nil || x.deletes != 12 || tc.deletes != 3 || n.record.Security != nil {
				t.Fatal("known cleanup journal not recovered", err)
			}
		})
	}
}

type securityJournalFault struct {
	base *memoryClaims
	fail func(claimRecord) bool
}

func (j securityJournalFault) Lock(key string) (io.Closer, error) { return j.base.Lock(key) }
func (j securityJournalFault) Read(ctx context.Context, key string) ([]byte, error) {
	return j.base.Read(ctx, key)
}
func (j securityJournalFault) Compare(ctx context.Context, key string, before, after []byte) (bool, error) {
	var r claimRecord
	if after != nil && json.Unmarshal(after, &r) == nil && j.fail(r) {
		return false, ErrJournal
	}
	return j.base.Compare(ctx, key, before, after)
}

func TestKernelSecurityJournalFailureCannotHideKernelMutation(t *testing.T) {
	for _, afterACK := range []bool{false, true} {
		t.Run(fmt.Sprint(afterACK), func(t *testing.T) {
			n, s, x, _, j := securityOwnerFixture(t)
			n.bearer.pool.journal = securityJournalFault{base: j, fail: func(r claimRecord) bool {
				if r.Network == nil || r.Network.Security == nil {
					return false
				}
				q := r.Network.Security
				if !afterACK {
					return q.Agreement != nil && q.Phase == "installing"
				}
				return q.Pending == nil && len(q.Operations) > 3
			}}
			if h, err := s.InstallIPSec(context.Background(), securityOwnerConfig()); h != nil || !errors.Is(err, ErrJournal) {
				t.Fatal("failed CAS reported success", err)
			}
			r := j.record(n.bearer.selection.key).Network.Security
			if !afterACK && x.adds != 0 {
				t.Fatal("kernel ADD before WAL intent")
			}
			if afterACK && (x.adds != 1 || x.deletes != 0 || r.Pending == nil || len(r.Operations) != 3) {
				t.Fatal("ACK then failed commit discarded ownership barrier")
			}
			if n.bearer.claim.networkHolds[n.record.Token] != n {
				t.Fatal("failed worker not rooted in application claim")
			}
		})
	}
}
