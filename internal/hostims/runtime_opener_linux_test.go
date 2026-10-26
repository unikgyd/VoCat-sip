//go:build linux

package hostims

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

type fakeWDSTopology struct {
	invalid *atomic.Bool
	mux     byte
}

func (p *fakeWDSTopology) Verify(context.Context) error {
	if p.invalid.Load() {
		return ErrChanged
	}
	return nil
}
func (p *fakeWDSTopology) Binding() wdsBinding {
	return wdsBinding{Endpoint: 2, Interface: 4, Mux: p.mux}
}
func (p *fakeWDSTopology) Key() string  { return hashText("synthetic pinned USB/mux") }
func (p *fakeWDSTopology) Close() error { return nil }

type wdsProxyFixture struct {
	mode                           string
	configuration                  BearerConfiguration
	mu                             sync.Mutex
	connections                    []net.Conn
	wg                             sync.WaitGroup
	errors                         chan error
	allocations, releases, queries atomic.Int32
	entered, release               chan struct{}
	mux                            byte
}

func startRuntimeProxy(t *testing.T, lease *BearerLease, mode string) (*wdsProxyFixture, func(context.Context) (proxyChannel, error)) {
	t.Helper()
	path := "\x00vocat-wds-" + hashText(t.TempDir())[:32]
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	f := &wdsProxyFixture{mode: mode, configuration: lease.Configuration(), errors: make(chan error, 8), entered: make(chan struct{}, 8), mux: 11}
	if mode == "late-allocation" {
		f.release = make(chan struct{})
	}
	if mode == "single" {
		f.mux = 0
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
					select {
					case f.errors <- err:
					default:
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		f.mu.Lock()
		for _, conn := range f.connections {
			conn.Close()
		}
		f.mu.Unlock()
		if f.release != nil {
			select {
			case <-f.release:
			default:
				close(f.release)
			}
		}
		done := make(chan struct{})
		go func() { f.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("WDS fixture did not close")
		}
		select {
		case err := <-f.errors:
			t.Error(err)
		default:
		}
	})
	return f, func(ctx context.Context) (proxyChannel, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}
		if err := trustedProxyPeer(conn.(*net.UnixConn)); err != nil {
			conn.Close()
			return nil, err
		}
		return newProxyQMI(conn), nil
	}
}
func (f *wdsProxyFixture) serve(conn net.Conn) error {
	opened := false
	next := byte(7)
	families := map[byte]byte{}
	bound := map[byte]bool{}
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
				if opened || string(input[1]) != "/dev/cdc-wdm-test" {
					return errors.New("fixture: wrong proxy device")
				}
				opened = true
			case 0x22:
				if !opened || !bytes.Equal(input[1], []byte{qmi.ServiceWDS}) {
					return errors.New("fixture: foreign CID allocation")
				}
				f.allocations.Add(1)
				families[next] = 0
				bound[next] = f.mux == 0
				select {
				case f.entered <- struct{}{}:
				default:
				}
				if f.mode == "late-allocation" {
					<-f.release
				}
				if f.mode == "allocation-lost" {
					return nil
				}
				info := []byte{qmi.ServiceWDS, next}
				if f.mode == "foreign-allocation" {
					info[0] = qmi.ServiceUIM
				}
				output = append(output, qmi.TLV{Type: 1, Value: info})
				next++
			case 0x23:
				if len(input[1]) != 2 || input[1][0] != qmi.ServiceWDS {
					return errors.New("fixture: bad release")
				}
				cid := input[1][1]
				if _, exists := families[cid]; !exists {
					return errors.New("fixture: release foreign CID")
				}
				f.releases.Add(1)
				if f.mode == "release-lost" {
					return nil
				}
				if f.mode == "release-negative" {
					if _, err := conn.Write(proxyResponse(request, []qmi.TLV{{Type: 2, Value: []byte{1, 0, 5, 0}}})); err != nil {
						return err
					}
					continue
				}
				info := []byte{qmi.ServiceWDS, cid}
				if f.mode == "release-foreign" {
					info[1]++
				}
				output = append(output, qmi.TLV{Type: 1, Value: info})
				if f.mode == "wds-change-on-release" {
					frame := proxyResponse(&qmi.Packet{ServiceType: qmi.ServiceWDS, ClientID: cid, MessageID: 0x22}, []qmi.TLV{{Type: 1, Value: []byte{1, 1}}})
					frame[6] = 4
					if _, err := conn.Write(frame); err != nil {
						return err
					}
				}
				delete(families, cid)
			default:
				return errors.New("fixture: forbidden CTL mutation")
			}
		} else {
			if request.ServiceType != qmi.ServiceWDS {
				return errors.New("fixture: non-WDS service")
			}
			family, owned := families[request.ClientID]
			if !opened || !owned {
				return errors.New("fixture: borrowed CID")
			}
			switch request.MessageID {
			case 0xa2:
				if f.mux == 0 || !bytes.Equal(input[0x10], []byte{2, 0, 0, 0, 4, 0, 0, 0}) || !bytes.Equal(input[0x11], []byte{f.mux}) || len(input) != 2 {
					return errors.New("fixture: bad endpoint or mux")
				}
				bound[request.ClientID] = true
			case 0x4d:
				if !bound[request.ClientID] || len(input[1]) != 1 || input[1][0] != 4 && input[1][0] != 6 {
					return errors.New("fixture: family before binding")
				}
				families[request.ClientID] = input[1][0]
			case 0x22:
				if family == 0 {
					return errors.New("fixture: status before family")
				}
				status := byte(2)
				if f.mode == "unconnected" {
					status = 1
				}
				output = append(output, qmi.TLV{Type: 1, Value: []byte{status}})
			case qmi.WDSGetRuntimeSettings:
				f.queries.Add(1)
				found := false
				for _, address := range f.configuration.Addresses {
					if address.Address.Addr().Is4() == (family == 4) {
						row := boundRuntimeRow(address)
						if f.mode == "wrong-apn" {
							row.APN = "internet"
						}
						output = boundRuntimeTLVs(row)
						found = true
					}
				}
				if !found {
					return errors.New("fixture: unexpected family")
				}
				if f.mode == "wds-change" {
					frame := proxyResponse(request, []qmi.TLV{{Type: 1, Value: []byte{1, 1}}})
					frame[6] = 4
					if _, err := conn.Write(frame); err != nil {
						return err
					}
				}
			default:
				return errors.New("fixture: Start/Stop/reset/profile command is forbidden")
			}
		}
		if _, err := conn.Write(proxyResponse(request, output)); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

func runtimeProductionFixture(t *testing.T, mode string) (*RuntimeSource, *wdsProxyFixture, *openerProbe, *memoryClaims) {
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
	fixture, dial := startRuntimeProxy(t, lease, mode)
	probe := &openerProbe{}
	source, err := newLinuxRuntimeSource(lease, runtimeOpenerDependencies{
		qmi: simOpenerDependencies{pin: func(Selection) (endpointGuard, error) { probe.pins.Add(1); return &fakeEndpointPin{probe: probe}, nil }, acquire: func(context.Context, string) (sequenceGate, error) {
			probe.held.Add(1)
			return &fakeSequence{probe: probe}, nil
		}, dial: func(ctx context.Context) (proxyChannel, error) { probe.dials.Add(1); return dial(ctx) }},
		topology: func(*BearerLease, endpointGuard) (wdsTopology, error) {
			return &fakeWDSTopology{invalid: &probe.invalid, mux: fixture.mux}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, fixture, probe, journal
}
func TestRuntimeProductionUnixProxyOwnedDualStackCIDs(t *testing.T) {
	for _, mode := range []string{"mux", "single"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := runtimeProductionFixture(t, mode)
			if probe.pins.Load() != 0 || f.allocations.Load() != 0 {
				t.Fatal("constructor activated endpoint")
			}
			proof, err := source.Read(context.Background())
			if err != nil || len(proof.Endpoints()) != 2 {
				t.Fatal(err)
			}
			if f.allocations.Load() != 2 || f.releases.Load() != 2 || f.queries.Load() != 2 || probe.held.Load() != 0 || probe.unpins.Load() != 1 {
				t.Fatal("CID/gate ownership mismatch")
			}
			if journal.record(source.lease.selection.key).RuntimeUncertain {
				t.Fatal("clean query left uncertainty")
			}
		})
	}
}
func TestRuntimeProductionUnknownAllocationOrReleaseRetainsFence(t *testing.T) {
	for _, mode := range []string{"allocation-lost", "foreign-allocation", "release-lost", "release-foreign", "release-negative"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := runtimeProductionFixture(t, mode)
			proof, err := source.Read(context.Background())
			if proof != nil || !errors.Is(err, ErrUncertain) {
				t.Fatal("unknown CID outcome accepted", err)
			}
			record := journal.record(source.lease.selection.key)
			if !record.RuntimeUncertain || probe.held.Load() != 1 || probe.unpins.Load() != 0 || len(source.lease.claim.retained) == 0 {
				t.Fatal("unknown resources were discarded")
			}
			if f.releases.Load() > 1 {
				t.Fatal("release retried or continued after uncertainty")
			}
		})
	}
}
func TestRuntimeProductionKnownFailureAndIndicationCleanOwnedCIDs(t *testing.T) {
	for _, mode := range []string{"unconnected", "wrong-apn", "wds-change"} {
		t.Run(mode, func(t *testing.T) {
			source, f, probe, journal := runtimeProductionFixture(t, mode)
			proof, err := source.Read(context.Background())
			if proof != nil || err == nil {
				t.Fatal("invalid runtime accepted")
			}
			if journal.record(source.lease.selection.key).RuntimeUncertain || probe.held.Load() != 0 || f.allocations.Load() != 1 || f.releases.Load() != 1 {
				t.Fatal("known owned CID was not released", err)
			}
			if mode == "unconnected" && f.queries.Load() != 0 {
				t.Fatal("unconnected runtime queried")
			}
		})
	}
}
func TestRuntimeProductionCancellationDuringAllocationIsObserved(t *testing.T) {
	source, f, probe, journal := runtimeProductionFixture(t, "late-allocation")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := source.Read(ctx); done <- err }()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("allocation not reached")
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
		t.Fatal("lost pending fence")
	}
	close(f.release)
	awaitClaim(t, journal, source.lease.selection.key, func(r claimRecord) bool { return !r.RuntimePending })
	if f.allocations.Load() != 1 || f.releases.Load() != 1 || probe.held.Load() != 0 {
		t.Fatal("late owned CID not cleaned or next family allocated")
	}
}

func TestRuntimeProductionObservedChangeDuringReleaseWithholdsProof(t *testing.T) {
	source, f, probe, journal := runtimeProductionFixture(t, "wds-change-on-release")
	proof, err := source.Read(context.Background())
	if proof != nil || !errors.Is(err, ErrChanged) {
		t.Fatal("observed late change produced a proof", err)
	}
	if f.releases.Load() != 2 || probe.held.Load() != 0 || journal.record(source.lease.selection.key).RuntimeUncertain {
		t.Fatal("late change prevented known CID cleanup")
	}
}
