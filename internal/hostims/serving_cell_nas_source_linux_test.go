//go:build linux

package hostims

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"vocat/internal/vowifi"
)

// All test devices, addresses, CIDs and Unix endpoints below are synthetic.
// The fixture rejects RF/attach, UIM, Start/Stop and unknown NAS operations.
type nasSourceFixture struct {
	mode                                 string
	configuration                        BearerConfiguration
	mu                                   sync.Mutex
	connections                          []net.Conn
	wg                                   sync.WaitGroup
	errors                               chan error
	nasAlloc, wdsAlloc, nasFree, wdsFree atomic.Int32
	queries, registrations, wdsReads     atomic.Int32
	entered, release                     chan struct{}
	onCell                               func()
}

func nasSourceProductionFixture(t *testing.T, mode string) (*NASServingCellSource, *nasSourceFixture, *openerProbe, *memoryClaims) {
	t.Helper()
	pool, backend, journal, _ := bearerFixture(t)
	backend.mu.Lock()
	modem := backend.modems["/org/freedesktop/ModemManager1/Modem/0"]
	modem.QMIControl = "/dev/cdc-wdm-test"
	backend.modems[modem.Path] = modem
	backend.mu.Unlock()
	selection, err := pool.Select(context.Background(), modem.Path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{SinglePDN: mode == "single"})
	if err != nil {
		t.Fatal(err)
	}
	f := &nasSourceFixture{mode: mode, configuration: lease.Configuration(), errors: make(chan error, 16), entered: make(chan struct{}, 1), release: make(chan struct{})}
	path := "\x00vocat-nas-" + hashText(t.TempDir())[:32]
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.connections = append(f.connections, conn)
			f.mu.Unlock()
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer conn.Close()
				if err := f.serve(conn); err != nil {
					f.errors <- err
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		for _, conn := range f.connections {
			_ = conn.Close()
		}
		f.mu.Unlock()
		select {
		case <-f.release:
		default:
			close(f.release)
		}
		finished := make(chan struct{})
		go func() { f.wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("NAS fixture failed to close")
		}
		select {
		case err := <-f.errors:
			t.Error(err)
		default:
		}
	})
	probe := &openerProbe{}
	mux := byte(11)
	if mode == "single" {
		mux = 0
	}
	source, err := newLinuxNASServingCellSource(lease, runtimeOpenerDependencies{
		qmi: simOpenerDependencies{
			pin: func(Selection) (endpointGuard, error) { probe.pins.Add(1); return &fakeEndpointPin{probe: probe}, nil },
			acquire: func(context.Context, string) (sequenceGate, error) {
				probe.held.Add(1)
				return &fakeSequence{probe: probe}, nil
			},
			dial: func(ctx context.Context) (proxyChannel, error) {
				probe.dials.Add(1)
				conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
				if err != nil {
					return nil, err
				}
				if err := trustedProxyPeer(conn.(*net.UnixConn)); err != nil {
					_ = conn.Close()
					return nil, err
				}
				return newProxyQMI(conn), nil
			},
		},
		topology: func(*BearerLease, endpointGuard) (wdsTopology, error) {
			return &fakeWDSTopology{invalid: &probe.invalid, mux: mux}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, f, probe, journal
}

func (f *nasSourceFixture) indicate(conn net.Conn, cid byte, message uint16) error {
	frame := proxyResponse(&qmi.Packet{ServiceType: qmi.ServiceNAS, ClientID: cid, MessageID: message}, nil)
	frame[6] = 4
	_, err := conn.Write(frame)
	return err
}

func (f *nasSourceFixture) serve(conn net.Conn) error {
	f.mu.Lock()
	mode := f.mode
	f.mu.Unlock()
	opened, registered := false, false
	next := byte(7)
	type key struct{ service, cid byte }
	owned, families, bound := map[key]bool{}, map[byte]byte{}, map[byte]bool{}
	localReads := 0
	for {
		request, err := readTestProxyRequest(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		input := map[byte][]byte{}
		for _, tlv := range request.TLVs {
			input[tlv.Type] = tlv.Value
		}
		output := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}
		if request.ServiceType == qmi.ServiceControl {
			switch request.MessageID {
			case 0xff00:
				if opened || len(input) != 1 || string(input[1]) != "/dev/cdc-wdm-test" {
					return errors.New("fixture: wrong proxy device")
				}
				opened = true
			case 0x22:
				if !opened || len(input) != 1 || len(input[1]) != 1 || input[1][0] != qmi.ServiceWDS && input[1][0] != qmi.ServiceNAS {
					return errors.New("fixture: non-owned service allocation")
				}
				service := input[1][0]
				if service == qmi.ServiceNAS {
					f.nasAlloc.Add(1)
					if mode == "allocation-lost" {
						return nil
					}
				} else {
					f.wdsAlloc.Add(1)
				}
				owned[key{service, next}] = true
				bound[next] = mode == "single"
				info := []byte{service, next}
				if service == qmi.ServiceNAS && mode == "foreign-allocation" {
					info[0] = qmi.ServiceUIM
				}
				output = append(output, qmi.TLV{Type: 1, Value: info})
				next++
			case 0x23:
				if len(input) != 1 || len(input[1]) != 2 || !owned[key{input[1][0], input[1][1]}] {
					return errors.New("fixture: borrowed release")
				}
				service, cid := input[1][0], input[1][1]
				if service == qmi.ServiceNAS {
					f.nasFree.Add(1)
					if mode == "release-lost" {
						return nil
					}
					if mode == "release-negative" {
						output = []qmi.TLV{{Type: 2, Value: []byte{1, 0, 5, 0}}}
						break
					}
					if mode == "event-on-release" {
						if err := f.indicate(conn, cid, 0x24); err != nil {
							return err
						}
					}
				} else {
					f.wdsFree.Add(1)
					if mode == "event-on-wds-release" {
						if err := f.indicate(conn, 9, 0x4e); err != nil {
							return err
						}
					}
				}
				output = append(output, qmi.TLV{Type: 1, Value: []byte{service, cid}})
				delete(owned, key{service, cid})
			default:
				return errors.New("fixture: forbidden CTL mutation")
			}
		} else if request.ServiceType == qmi.ServiceNAS {
			if !owned[key{qmi.ServiceNAS, request.ClientID}] {
				return errors.New("fixture: borrowed NAS CID")
			}
			switch request.MessageID {
			case nasRegisterIndications:
				if registered || len(input) != 3 || !bytes.Equal(input[0x13], []byte{1}) || !bytes.Equal(input[0x18], []byte{1}) || !bytes.Equal(input[0x20], []byte{1}) {
					return errors.New("fixture: global/unexpected reporting change")
				}
				registered = true
				f.registrations.Add(1)
				if mode == "register-rejected" {
					output = []qmi.TLV{{Type: 2, Value: []byte{1, 0, 0x47, 0}}}
				}
			case nasGetServingSystem:
				if !registered || len(input) != 0 {
					return errors.New("fixture: serving system before reporting")
				}
				value := []byte{1, 1, 1, 2, 1, 8}
				if mode == "unregistered" {
					value[0] = 2
				}
				if mode == "detached" {
					value[2] = 2
				}
				if mode == "mixed-rat" {
					value[4] = 2
					value = append(value, 12)
				}
				if mode == "deregister-after-cell" && f.queries.Load() > 0 {
					value[0] = 0
				}
				output = append(output, qmi.TLV{Type: 1, Value: value})
			case nasGetCellLocationInfo:
				if !registered || len(input) != 0 {
					return errors.New("fixture: invalid cell query")
				}
				f.queries.Add(1)
				f.mu.Lock()
				onCell := f.onCell
				f.mu.Unlock()
				if onCell != nil {
					onCell()
				}
				if mode == "late-cell" || mode == "late-release" {
					select {
					case f.entered <- struct{}{}:
					default:
					}
					<-f.release
				}
				for _, row := range []struct {
					mode    string
					message uint16
				}{{"registration-event", 0x24}, {"system-event", 0x4e}, {"rf-event", 0x02}, {"unknown-event", 0xeeee}} {
					if mode == row.mode {
						if err := f.indicate(conn, request.ClientID, row.message); err != nil {
							return err
						}
					}
				}
				if mode == "aba-event" {
					// The final queried cell is unchanged, but two observed
					// system transitions must not be erased by value equality.
					for i := 0; i < 2; i++ {
						if err := f.indicate(conn, request.ClientID, 0x4e); err != nil {
							return err
						}
					}
				}
				value := nasLTEGoldenValue()
				if mode == "bad-cell" {
					value[1] = 0xff
				}
				if mode == "changed-cell" {
					binary.LittleEndian.PutUint32(value[6:10], 0x7654321)
				}
				output = append(output, qmi.TLV{Type: 0x13, Value: value})
			default:
				return errors.New("fixture: RF/attach/unknown NAS command forbidden")
			}
		} else if request.ServiceType == qmi.ServiceWDS {
			if !owned[key{qmi.ServiceWDS, request.ClientID}] {
				return errors.New("fixture: borrowed WDS CID")
			}
			cid, family := request.ClientID, families[request.ClientID]
			switch request.MessageID {
			case 0xa2:
				if mode == "single" || len(input) != 2 || !bytes.Equal(input[0x10], []byte{2, 0, 0, 0, 4, 0, 0, 0}) || !bytes.Equal(input[0x11], []byte{11}) {
					return errors.New("fixture: wrong PDN endpoint/mux")
				}
				bound[cid] = true
			case 0x4d:
				if !bound[cid] || len(input[1]) != 1 || input[1][0] != 4 && input[1][0] != 6 {
					return errors.New("fixture: wrong WDS family")
				}
				families[cid] = input[1][0]
			case 0x22:
				if family == 0 {
					return errors.New("fixture: status before WDS binding")
				}
				output = append(output, qmi.TLV{Type: 1, Value: []byte{2}})
			case qmi.WDSGetRuntimeSettings:
				localReads++
				f.wdsReads.Add(1)
				for _, address := range f.configuration.Addresses {
					if address.Address.Addr().Is4() == (family == 4) {
						row := boundRuntimeRow(address)
						if mode == "changed-pdn" && localReads > 2 {
							row.APN = "internet"
						}
						output = boundRuntimeTLVs(row)
					}
				}
			default:
				return errors.New("fixture: Start/Stop/profile WDS operation forbidden")
			}
		} else {
			return errors.New("fixture: forbidden non-WDS/NAS service")
		}
		frame := proxyResponse(request, output)
		if request.ServiceType == qmi.ServiceNAS && request.MessageID == nasGetCellLocationInfo && mode == "wrong-transaction" {
			frame[7]++
		}
		if _, err := conn.Write(frame); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

func TestNASSourceOwnedSolicitedQueryAndRevalidation(t *testing.T) {
	for _, mode := range []string{"mux", "single"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := nasSourceProductionFixture(t, mode)
			if probe.pins.Load() != 0 {
				t.Fatal("constructor performed I/O")
			}
			proof, err := source.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if proof.Sample() != (vowifi.LTECell{MCC: "310", MNC: "260", Duplex: "FDD", TAC: 0xabcd, CellID: 0x1234567}) {
				t.Fatal("wrong single-response sample")
			}
			start, end := proof.QueryWindow()
			if start.IsZero() || end.Before(start) || end.Sub(start) > 5*time.Second || proof.ContinuousCellCoverage() {
				t.Fatal("fabricated freshness guarantee")
			}
			if f.nasAlloc.Load() != 1 || f.wdsAlloc.Load() != 2 || f.nasFree.Load() != 1 || f.wdsFree.Load() != 2 || f.wdsReads.Load() != 4 || f.registrations.Load() != 1 || probe.held.Load() != 0 {
				t.Fatal("owned/bracketed query accounting")
			}
			if journal.record(source.lease.selection.key).RuntimePending || journal.record(source.lease.selection.key).RuntimeUncertain {
				t.Fatal("clean query left fence")
			}
			fresh, err := proof.Revalidate(context.Background())
			if err != nil || fresh == proof || !fresh.start.After(proof.start) {
				t.Fatal("revalidation reused an old sample", err)
			}
			text := fmt.Sprintf("%+v %#v %+v", proof, *proof, source)
			for _, secret := range []string{"310", "260", "1234567", source.lease.selection.SubscriberIMSI(), "198.51.100.20"} {
				if strings.Contains(text, secret) {
					t.Fatal("private graph escaped formatter")
				}
			}
		})
	}
}

func TestNASSourceRuntimeProofUsesSameQueryAndFreshContext(t *testing.T) {
	for _, mode := range []string{"mux", "single"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := nasSourceProductionFixture(t, mode)
			original, cancel := context.WithCancel(context.Background())
			defer cancel()
			cell, err := source.Observe(original)
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := cell.RuntimeProof()
			if err != nil || runtime == nil || runtime.fingerprint != cell.fingerprint {
				t.Fatal("lost same-query runtime binding", err)
			}
			start, end := cell.QueryWindow()
			if f.queries.Load() != 1 || f.wdsReads.Load() != 4 || probe.dials.Load() != 1 {
				t.Fatal("runtime extraction repeated the owned query")
			}
			cancel()
			// The extracted proof must NOT retain Observe's canceled caller or
			// NAS-aware opener. A later Verify is one ordinary owned WDS read.
			if err := runtime.Verify(context.Background()); err != nil {
				t.Fatal("runtime verification retained original query context", err)
			}
			ordinary, err := newLinuxRuntimeSource(source.lease, source.deps)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := ordinary.Read(context.Background())
			if err != nil || fresh.fingerprint != runtime.fingerprint || !reflect.DeepEqual(fresh.Runtime(), runtime.Runtime()) {
				t.Fatal("NAS and ordinary WDS binding keys disagree", err)
			}
			if f.queries.Load() != 1 || f.registrations.Load() != 1 || f.nasAlloc.Load() != 1 || f.nasFree.Load() != 1 ||
				f.wdsReads.Load() != 8 || f.wdsAlloc.Load() != 6 || f.wdsFree.Load() != 6 || probe.held.Load() != 0 {
				t.Fatal("runtime verification queried NAS or lost CID ownership")
			}
			if gotStart, gotEnd := cell.QueryWindow(); gotStart != start || gotEnd != end || cell.ContinuousCellCoverage() {
				t.Fatal("WDS verification refreshed the cell's age or guarantees")
			}
			if record := journal.record(source.lease.selection.key); record.RuntimePending || record.RuntimeUncertain {
				t.Fatal("ordinary verification left query fence")
			}
		})
	}
}

func TestNASSourceRuntimeProofIsDetachedAndRejectsMissingEvidence(t *testing.T) {
	source, _, _, _ := nasSourceProductionFixture(t, "mux")
	cell, err := source.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := cell.RuntimeProof()
	if err != nil {
		t.Fatal(err)
	}
	second, err := cell.RuntimeProof()
	if err != nil || first == second || first.source == second.source {
		t.Fatal("runtime extraction retained a shared mutable graph", err)
	}
	want := second.Runtime()
	if len(first.runtime.PCSCF) == 0 {
		t.Fatal("fixture has no P-CSCF")
	}
	first.runtime.PCSCF[0] = first.runtime.IPv4.Addr()
	first.runtime.PCSCFDomains = append(first.runtime.PCSCFDomains, "untrusted.example")
	first.fingerprint = "mutated"
	third, err := cell.RuntimeProof()
	if err != nil || !reflect.DeepEqual(second.Runtime(), want) || !reflect.DeepEqual(third.Runtime(), want) || third.fingerprint != cell.fingerprint {
		t.Fatal("extracted proof aliases NAS or sibling runtime evidence", err)
	}
	for _, invalid := range []*NASCellQueryProof{nil, {}, {source: source}, {source: source, fingerprint: "invalid"}} {
		if proof, err := invalid.RuntimeProof(); proof != nil || !errors.Is(err, ErrRuntime) {
			t.Fatal("missing owned runtime evidence accepted", err)
		}
	}
}

func TestNASSourceRejectsObservedEpochChangesAndCleansKnownCIDs(t *testing.T) {
	for _, mode := range []string{"registration-event", "system-event", "rf-event", "unknown-event", "aba-event", "event-on-release", "event-on-wds-release", "changed-pdn"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := nasSourceProductionFixture(t, mode)
			proof, err := source.Observe(context.Background())
			if proof != nil || !errors.Is(err, ErrChanged) {
				t.Fatal("changed sample accepted", err)
			}
			if f.nasFree.Load() != 1 || f.wdsFree.Load() != 2 || probe.held.Load() != 0 || journal.record(source.lease.selection.key).RuntimeUncertain {
				t.Fatal("observed change prevented known cleanup")
			}
		})
	}
}

func TestNASSourceKnownUnavailableDoesNotLeakCIDs(t *testing.T) {
	for _, mode := range []string{"unregistered", "detached", "mixed-rat", "bad-cell", "register-rejected", "deregister-after-cell"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := nasSourceProductionFixture(t, mode)
			proof, err := source.Observe(context.Background())
			if proof != nil || err == nil || errors.Is(err, ErrUncertain) {
				t.Fatal("invalid/unavailable sample accepted", err)
			}
			if f.nasFree.Load() != 1 || f.wdsFree.Load() != 2 || probe.held.Load() != 0 || journal.record(source.lease.selection.key).RuntimeUncertain {
				t.Fatal("known failure leaked owned CID")
			}
		})
	}
}

func TestNASSourceUnknownResourceRetainsApplicationFence(t *testing.T) {
	for _, mode := range []string{"allocation-lost", "foreign-allocation", "release-lost", "release-negative", "wrong-transaction"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := nasSourceProductionFixture(t, mode)
			proof, err := source.Observe(context.Background())
			if proof != nil || !errors.Is(err, ErrUncertain) {
				t.Fatal("unknown ownership accepted", err)
			}
			if !journal.record(source.lease.selection.key).RuntimeUncertain || len(source.lease.claim.retained) != 1 || probe.held.Load() != 1 || probe.unpins.Load() != 0 || f.wdsFree.Load() != 0 {
				t.Fatal("unknown session has no rooted ownership")
			}
			if err := source.lease.Close(context.Background()); !errors.Is(err, ErrUncertain) {
				t.Fatal("PDN closed with unknown NAS resources", err)
			}
			if _, err := source.Observe(context.Background()); !errors.Is(err, ErrUncertain) {
				t.Fatal("unknown query replayed", err)
			}
		})
	}
}

func TestNASSourceCancellationKeepsLateOwnerUntilACKCleanup(t *testing.T) {
	source, f, probe, journal := nasSourceProductionFixture(t, "late-cell")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := source.Observe(ctx); done <- err }()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("cell query not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller blocked")
	}
	if !journal.record(source.lease.selection.key).RuntimePending {
		t.Fatal("late query lost fence")
	}
	close(f.release)
	awaitClaim(t, journal, source.lease.selection.key, func(r claimRecord) bool { return !r.RuntimePending })
	if f.nasFree.Load() != 1 || f.wdsFree.Load() != 2 || probe.held.Load() != 0 || journal.record(source.lease.selection.key).RuntimeUncertain {
		t.Fatal("late acknowledged CIDs not cleaned")
	}
}

func TestNASSourceHardwareChangePreventsFurtherMutation(t *testing.T) {
	source, f, probe, journal := nasSourceProductionFixture(t, "hardware-change")
	f.mu.Lock()
	f.onCell = func() { probe.invalid.Store(true) }
	f.mu.Unlock()
	if proof, err := source.Observe(context.Background()); proof != nil || !errors.Is(err, ErrUncertain) {
		t.Fatal("changed hardware accepted", err)
	}
	if f.nasFree.Load() != 0 || f.wdsFree.Load() != 0 || !journal.record(source.lease.selection.key).RuntimeUncertain || len(source.lease.claim.retained) != 1 {
		t.Fatal("mutated after physical guard failed")
	}
}

func TestNASSourceLeaseIdentityOrBearerChangeRetainsKnownResources(t *testing.T) {
	for _, mode := range []string{"owner", "SIM", "slot", "physical-generation", "address", "suspended", "disconnected"} {
		t.Run(mode, func(t *testing.T) {
			source, f, _, journal := nasSourceProductionFixture(t, "mux")
			backend := source.lease.pool.backend.(*fakeBearerBackend)
			f.mu.Lock()
			f.onCell = func() {
				backend.mu.Lock()
				defer backend.mu.Unlock()
				path := source.lease.selection.snapshot.Path
				modem := backend.modems[path]
				source.lease.claim.mu.Lock()
				bearerPath := source.lease.claim.record.Path
				source.lease.claim.mu.Unlock()
				bearer := backend.bearers[bearerPath]
				switch mode {
				case "owner":
					modem.Owner = ":1.999"
				case "SIM":
					modem.Binding.ICCID = "8901000000000000099"
				case "slot":
					modem.Binding.Slot++
				case "physical-generation":
					modem.Binding.PhysicalToken = "replacement-hardware"
				case "address":
					bearer.Addresses = nil
				case "suspended":
					bearer.Suspended = true
				case "disconnected":
					bearer.Connected = false
				}
				backend.modems[path] = modem
				backend.bearers[bearerPath] = bearer
			}
			f.mu.Unlock()
			proof, err := source.Observe(context.Background())
			if proof != nil || !errors.Is(err, ErrUncertain) {
				t.Fatal("changed lease/bearer accepted", err)
			}
			if f.nasFree.Load() != 0 || f.wdsFree.Load() != 0 || !journal.record(source.lease.selection.key).RuntimeUncertain || len(source.lease.claim.retained) != 1 {
				t.Fatal("cleanup mutated a replacement lease/bearer")
			}
		})
	}
}

func TestNASSourceRevocationAndFinalJournalFailureWithholdProof(t *testing.T) {
	for _, mode := range []string{"revoked", "journal"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := nasSourceProductionFixture(t, "mux")
			var denied atomic.Bool
			source.lease.pool.authorize = func(context.Context, Selection) error {
				if denied.Load() {
					return ErrDenied
				}
				return nil
			}
			f.mu.Lock()
			f.onCell = func() {
				if mode == "revoked" {
					denied.Store(true)
				} else {
					journal.mu.Lock()
					journal.fail = true
					journal.mu.Unlock()
				}
			}
			f.mu.Unlock()
			want := error(ErrDenied)
			if mode == "journal" {
				want = ErrJournal
			}
			if proof, err := source.Observe(context.Background()); proof != nil || !errors.Is(err, want) {
				t.Fatal("revoked/uncommitted query exposed proof", err)
			}
			if f.nasFree.Load() != 1 || f.wdsFree.Load() != 2 || probe.held.Load() != 0 {
				t.Fatal("revocation/journal failure prevented safe cleanup")
			}
		})
	}
}

func TestNASSourceDurableIntentAndSharedControlFence(t *testing.T) {
	source, f, probe, journal := nasSourceProductionFixture(t, "mux")
	journal.mu.Lock()
	journal.fail = true
	journal.mu.Unlock()
	if proof, err := source.Observe(context.Background()); proof != nil || !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	if probe.pins.Load() != 0 || f.nasAlloc.Load() != 0 {
		t.Fatal("query before durable intent")
	}

	source, f, _, _ = nasSourceProductionFixture(t, "mux")
	f.mu.Lock()
	f.onCell = func() {
		if _, err := source.Observe(context.Background()); !errors.Is(err, ErrBusy) {
			t.Error("parallel NAS not fenced", err)
		}
		provider := &SIMProvider{lease: source.lease}
		if err := provider.begin(context.Background()); !errors.Is(err, ErrBusy) {
			t.Error("SIM not fenced", err)
		}
	}
	f.mu.Unlock()
	if _, err := source.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNASSourceNilAndRevalidationChangedCell(t *testing.T) {
	if _, err := NewCellularNASServingCellSource(nil); !errors.Is(err, ErrSelection) {
		t.Fatal(err)
	}
	var source *NASServingCellSource
	if _, err := source.Observe(context.Background()); !errors.Is(err, ErrSelection) {
		t.Fatal(err)
	}
	var proof *NASCellQueryProof
	if proof.Sample() != (vowifi.LTECell{}) {
		t.Fatal("nil proof supplied sample")
	}
	if _, err := proof.Revalidate(context.Background()); !errors.Is(err, ErrServingCellUnavailable) {
		t.Fatal(err)
	}

	source, f, _, _ := nasSourceProductionFixture(t, "mux")
	proof, err := source.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.mode = "changed-cell" // Next connection takes a detached mode snapshot.
	f.mu.Unlock()
	if next, err := proof.Revalidate(context.Background()); next != nil || !errors.Is(err, ErrChanged) {
		t.Fatal("changed cell accepted by revalidation", err)
	}
}
