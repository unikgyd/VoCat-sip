//go:build linux

package hostims

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

// All modem, UIM, WDS, TC and XFRM operations below are synthetic. Only the
// private reservation files under t.TempDir use the real Linux filesystem.
type accessKernelTrace struct {
	mu     sync.Mutex
	events []string
}

func (p *accessKernelTrace) add(event string) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}
func (p *accessKernelTrace) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

type accessFakeNetwork struct {
	*securityFakeNetwork
	trace   *accessKernelTrace
	unknown bool
}

func (d *accessFakeNetwork) apply(op networkOperation) error {
	d.trace.add("network-add")
	if err := d.securityFakeNetwork.apply(op); err != nil {
		return err
	}
	if d.unknown {
		return errors.New("synthetic applied network mutation without ACK")
	}
	return nil
}
func (d *accessFakeNetwork) remove(op networkOperation) error {
	d.trace.add("network-remove")
	return d.securityFakeNetwork.remove(op)
}
func (d *accessFakeNetwork) close() { d.trace.add("network-close") }

type accessFakeTC struct {
	*securityFakeTC
	trace *accessKernelTrace
}

func (d *accessFakeTC) remove(op securityTCOperation, hash string) error {
	d.trace.add("security-remove")
	return d.securityFakeTC.remove(op, hash)
}
func (d *accessFakeTC) close() { d.trace.add("security-close") }

type accessKernelFixture struct {
	source       *RuntimeSource
	runtime      *runtimeFake
	journal      *memoryClaims
	sim          *SIMProvider
	identity     vowifi.SIMIdentity
	owner        *PreparedAccess
	backend      *kernelPreparedAccess
	network      *accessFakeNetwork
	tc           *accessFakeTC
	xfrm         *securityFakeXFRM
	trace        *accessKernelTrace
	mu           sync.Mutex
	cell         vowifi.LTECell
	samples      int
	last         accessSample
	foreign      *RuntimeProof
	networkOpens int
}

func newAccessKernelFixture(t *testing.T) *accessKernelFixture {
	t.Helper()
	source, runtime, journal := runtimeSourceFixture(t)
	uim := newFakeUIM(source.lease.selection)
	sim, err := newSIMProvider(source.lease, uim.open)
	if err != nil {
		t.Fatal(err)
	}
	card, err := sim.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	trace := &accessKernelTrace{}
	f := &accessKernelFixture{source: source, runtime: runtime, journal: journal, sim: sim,
		identity: card.Subscriber(), trace: trace,
		cell: vowifi.LTECell{MCC: "310", MNC: "260", Duplex: "FDD", TAC: 0xabcd, CellID: 0x1234567}}
	f.network = &accessFakeNetwork{securityFakeNetwork: &securityFakeNetwork{values: map[string]string{}}, trace: trace}
	f.xfrm = &securityFakeXFRM{objects: map[scopedXFRMKey]scopedXFRMSnapshot{}}
	f.tc = &accessFakeTC{securityFakeTC: &securityFakeTC{objects: map[securityTCOperation]string{}, failAt: 255}, trace: trace}
	f.backend = &kernelPreparedAccess{lease: source.lease, securityDirectory: filepath.Join(t.TempDir(), "reservations"),
		openNetwork: func() (kernelNetworkDriver, error) {
			f.mu.Lock()
			f.networkOpens++
			f.mu.Unlock()
			return f.network, nil
		},
		openSecurity: func(r *securityClaim) (scopedXFRMAccess, scopedTCAccess, error) {
			f.tc.operationsList = []securityTCOperation{{Kind: "clsact"}}
			for i := range r.Flows {
				f.tc.operationsList = append(f.tc.operationsList, securityTCOperation{Kind: "flower", Ordinal: uint8(i + 1)})
			}
			return f.xfrm, f.tc, nil
		}}
	t.Cleanup(func() {
		if f.owner != nil {
			_ = f.owner.Close(context.Background())
		} else {
			_ = f.backend.Close(context.Background())
		}
		if n := f.backend.network; n != nil {
			_ = n.locked(context.Background(), func(context.Context) error {
				if n.security != nil && n.security.reservations != nil {
					n.security.reservations.closeFDs()
				}
				return nil
			})
		}
		_ = source.lease.Close(context.Background())
	})
	return f
}

func (f *accessKernelFixture) observe(ctx context.Context) (accessSample, error) {
	start := time.Now()
	proof, err := f.source.Read(ctx)
	if err != nil {
		return accessSample{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples++
	if f.foreign != nil {
		proof = f.foreign
	}
	f.last = accessSample{runtime: proof, cell: f.cell, start: start, end: time.Now()}
	return f.last, nil
}
func (f *accessKernelFixture) prepare(ctx context.Context) (*PreparedAccess, error) {
	owner, err := prepareAccessWith(ctx, f.source.lease, accessDependencies{sim: f.sim, observe: f.observe, backend: f.backend})
	f.owner = owner
	return owner, err
}
func (f *accessKernelFixture) counts() (samples, queries int) {
	f.mu.Lock()
	samples = f.samples
	f.mu.Unlock()
	f.runtime.mu.Lock()
	queries = f.runtime.queries
	f.runtime.mu.Unlock()
	return samples, queries
}

func TestPreparedAccessKernelCarriesSameLeaseProofAndOneQueryPerVerify(t *testing.T) {
	f := newAccessKernelFixture(t)
	access, err := f.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.backend.network == nil || f.backend.network.proof == nil || f.backend.network.proof.source.lease != f.source.lease ||
		f.backend.network.proof.fingerprint != f.backend.fingerprint || f.backend.fingerprint != f.last.runtime.fingerprint {
		t.Fatal("prepared network lost same-lease WDS provenance")
	}
	if !access.Evidence().Established {
		t.Fatal("fully prepared synthetic access has no evidence")
	}
	samples, queries := f.counts()
	if err := access.Verify(context.Background(), f.identity); err != nil {
		t.Fatal(err)
	}
	if gotSamples, gotQueries := f.counts(); gotSamples != samples+1 || gotQueries != queries+2 {
		t.Fatalf("Verify did not consume exactly one dual-stack observation: samples %d->%d, WDS %d->%d", samples, gotSamples, queries, gotQueries)
	}
}

func TestPreparedAccessKernelRejectsChangedRuntimeAndLease(t *testing.T) {
	for _, mode := range []string{"WDS-binding", "foreign-lease", "hardware-generation", "SIM"} {
		t.Run(mode, func(t *testing.T) {
			f := newAccessKernelFixture(t)
			access, err := f.prepare(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "WDS-binding":
				f.runtime.mu.Lock()
				f.runtime.binding = hashText("replacement synthetic topology")
				f.runtime.mu.Unlock()
			case "foreign-lease":
				other, _, _ := runtimeSourceFixture(t)
				proof, err := other.Read(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				f.foreign = proof
				f.mu.Unlock()
			case "hardware-generation", "SIM":
				backend := f.source.lease.pool.backend.(*fakeBearerBackend)
				backend.mu.Lock()
				modem := backend.modems[f.source.lease.selection.snapshot.Path]
				if mode == "SIM" {
					modem.Binding.ICCID = "8901000000000000099"
				} else {
					modem.Binding.PhysicalToken = "replacement-generation"
				}
				backend.modems[modem.Path] = modem
				backend.mu.Unlock()
			}
			if err := access.Verify(context.Background(), f.identity); err == nil || access.Evidence().Established {
				t.Fatal("changed proof/lease remained established", err)
			}
		})
	}
}

func TestPreparedAccessKernelAllowsFreshCellOnSamePDN(t *testing.T) {
	f := newAccessKernelFixture(t)
	access, err := f.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := access.Evidence()
	f.mu.Lock()
	f.cell.CellID++
	want := f.cell
	f.mu.Unlock()
	if err := access.Verify(context.Background(), f.identity); err != nil {
		t.Fatal("ordinary cell change rejected on unchanged owned PDN", err)
	}
	if after := access.Evidence(); !after.Established || after.Cell != want || after.Cell == before.Cell {
		t.Fatal("fresh serving cell not published")
	}
}

func TestPreparedAccessKernelCloseStopsSecurityBeforeNetwork(t *testing.T) {
	f := newAccessKernelFixture(t)
	access, err := f.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if files, err := os.ReadDir(f.backend.securityDirectory); err != nil || len(files) != 3 {
		t.Fatal("real private scope reservations were not acquired", err)
	}
	if err := f.backend.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	securityClosed, networkRemoved := false, false
	for _, event := range f.trace.snapshot() {
		if event == "security-close" {
			securityClosed = true
		}
		if event == "network-remove" {
			networkRemoved = true
			if !securityClosed {
				t.Fatal("network removed before security owner stopped")
			}
		}
	}
	if !securityClosed || !networkRemoved || len(f.tc.objects) != 0 || len(f.network.values) != 0 || f.journal.record(f.source.lease.selection.key).Network != nil {
		t.Fatal("backend teardown did not release the complete owned scope")
	}
	if files, err := os.ReadDir(f.backend.securityDirectory); err != nil || len(files) != 0 {
		t.Fatal("clean scope left persistent reservations", err)
	}
	if err := access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedAccessKernelUnknownSecurityACKRetainsOwnerAndPDN(t *testing.T) {
	f := newAccessKernelFixture(t)
	f.tc.failAt, f.tc.unknown = 1, true
	access, err := f.prepare(context.Background())
	if err == nil || access == nil {
		t.Fatal("uncertain preparation did not return its retained owner", err)
	}
	if err := access.Close(context.Background()); err == nil {
		t.Fatal("uncertain security owner was reported clean")
	}
	record := f.journal.record(f.source.lease.selection.key)
	if record.Network == nil || record.Network.Security == nil || record.Network.Security.Pending == nil ||
		f.backend.network == nil || f.backend.network.security == nil {
		t.Fatal("unknown security mutation lost durable/in-memory ownership")
	}
	if err := f.source.lease.Close(context.Background()); !errors.Is(err, ErrUncertain) && !errors.Is(err, ErrBusy) {
		t.Fatal("PDN released below unknown security ACK", err)
	}
	backend := f.source.lease.pool.backend.(*fakeBearerBackend)
	if backend.count("disconnect") != 0 || backend.count("delete") != 0 {
		t.Fatal("uncertain preparation released modem resources")
	}
}

func TestPreparedAccessKernelFailedConstructorFindsOnlyExactHeldLease(t *testing.T) {
	f := newAccessKernelFixture(t)
	f.network.unknown = true
	sample, err := f.observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Prepare(context.Background(), sample); !errors.Is(err, ErrUncertain) || f.backend.network != nil {
		t.Fatal("fixture did not retain a failed undelivered network constructor", err)
	}
	c := f.source.lease.claim
	c.mu.Lock()
	var held *kernelNetworkLease
	for _, object := range c.networkHolds {
		if n, ok := object.(*kernelNetworkLease); ok && n.bearer == f.source.lease {
			held = n
		}
	}
	c.mu.Unlock()
	if held == nil {
		t.Fatal("constructor lost its independent in-memory owner")
	}
	if err := f.backend.Close(context.Background()); err == nil || f.backend.network != held {
		t.Fatal("cleanup did not retrieve the exact held lease", err)
	}
	f.mu.Lock()
	opens := f.networkOpens
	f.mu.Unlock()
	if opens != 1 || f.journal.record(f.source.lease.selection.key).Network == nil {
		t.Fatal("cleanup reopened/adopted kernel state or erased unknown mutation")
	}
	// A copied lease/token or a cold ledger is not ownership of the original
	// in-memory network. No kernel opener is used to turn either into an owner.
	copyLease := *f.source.lease
	other := &kernelPreparedAccess{lease: &copyLease}
	if err := other.Close(context.Background()); err != nil || other.network != nil {
		t.Fatal("cleanup adopted another lease's held kernel owner", err)
	}
}
