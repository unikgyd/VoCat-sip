//go:build linux

package hostims

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"vocat/internal/vowifi"
)

type openerProbe struct {
	pins, unpins, held, dials atomic.Int32
	invalid                   atomic.Bool
}
type fakeEndpointPin struct {
	probe *openerProbe
	once  sync.Once
}

func (*fakeEndpointPin) Path() string { return "/dev/cdc-wdm-test" }
func (p *fakeEndpointPin) Verify(context.Context) error {
	if p.probe.invalid.Load() {
		return ErrChanged
	}
	return nil
}
func (p *fakeEndpointPin) Close() error { p.once.Do(func() { p.probe.unpins.Add(1) }); return nil }

type fakeSequence struct {
	probe *openerProbe
	once  sync.Once
}

func (p *fakeSequence) Release() { p.once.Do(func() { p.probe.held.Add(-1) }) }

type proxyUIMFixture struct {
	mu                           sync.Mutex
	listener                     *net.UnixListener
	connections                  []net.Conn
	wg                           sync.WaitGroup
	state                        *fakeUIM
	mode                         string
	allocations, releases, opens atomic.Int32
	allocationEntered            chan struct{}
	allocationContinue           chan struct{}
	errors                       chan error
}

func fixtureCardICCID(value string) []byte {
	raw := make([]byte, 10)
	for i := range raw {
		raw[i] = 0xff
	}
	for i, c := range []byte(value) {
		if i%2 == 0 {
			raw[i/2] = (raw[i/2] & 0xf0) | (c - '0')
		} else {
			raw[i/2] = (c-'0')<<4 | (raw[i/2] & 15)
		}
	}
	return raw
}
func startProxyUIM(t *testing.T, state *fakeUIM, mode string) (*proxyUIMFixture, func(context.Context) (proxyChannel, error)) {
	t.Helper()
	// Like the real qmi-proxy, use an abstract socket. A hashed unique name
	// stays below sockaddr_un's limit even for long Go subtest names.
	path := "\x00vocat-uim-" + hashText(t.TempDir())[:32]
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	f := &proxyUIMFixture{listener: listener, state: state, mode: mode, allocationEntered: make(chan struct{}, 16), errors: make(chan error, 16)}
	if mode == "late-allocation" {
		f.allocationContinue = make(chan struct{})
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.connections = append(f.connections, connection)
			f.mu.Unlock()
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer connection.Close()
				if err := f.serve(connection); err != nil {
					select {
					case f.errors <- err:
					default:
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		for _, connection := range f.connections {
			_ = connection.Close()
		}
		f.mu.Unlock()
		if f.allocationContinue != nil {
			select {
			case <-f.allocationContinue:
			default:
				close(f.allocationContinue)
			}
		}
		finished := make(chan struct{})
		go func() { f.wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("private proxy fixture did not stop")
		}
		select {
		case err := <-f.errors:
			t.Error(err)
		default:
		}
	})
	dial := func(ctx context.Context) (proxyChannel, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}
		if trustedProxyPeer(connection.(*net.UnixConn)) != nil {
			connection.Close()
			return nil, ErrDenied
		}
		return newProxyQMI(connection), nil
	}
	return f, dial
}
func (f *proxyUIMFixture) serve(connection net.Conn) error {
	card := &fakeUIMSession{state: f.state}
	opened, allocated := false, false
	for {
		request, err := readTestProxyRequest(connection)
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return nil
		}
		if err != nil {
			return err
		}
		input := map[byte][]byte{}
		for _, tlv := range request.TLVs {
			input[tlv.Type] = tlv.Value
		}
		output := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}
		if request.ServiceType == qmi.ServiceControl {
			if request.ClientID != 0 {
				return errors.New("fixture: nonzero CTL CID")
			}
			switch request.MessageID {
			case 0xff00:
				if opened || string(input[1]) != "/dev/cdc-wdm-test" {
					return errors.New("fixture: wrong proxy open")
				}
				opened = true
				f.opens.Add(1)
			case 0x0022:
				if !opened || allocated || !bytes.Equal(input[1], []byte{qmi.ServiceUIM}) {
					return errors.New("fixture: allocation not owned")
				}
				f.allocations.Add(1)
				f.allocationEntered <- struct{}{}
				if f.allocationContinue != nil {
					<-f.allocationContinue
				}
				if f.mode == "alloc-drop" {
					return nil
				}
				if f.mode == "alloc-negative" {
					output[0].Value = []byte{1, 0, 0x52, 0}
				} else {
					allocated = true
					service := byte(qmi.ServiceUIM)
					if f.mode == "alloc-malformed" {
						service = qmi.ServiceDMS
					}
					output = append(output, qmi.TLV{Type: 1, Value: []byte{service, 7}})
				}
			case 0x0023:
				if !allocated || !bytes.Equal(input[1], []byte{qmi.ServiceUIM, 7}) || card.channel != 0 {
					return errors.New("fixture: released unowned/live CID")
				}
				f.releases.Add(1)
				if f.mode == "release-drop" {
					return nil
				}
				allocated = false
				output = append(output, qmi.TLV{Type: 1, Value: []byte{qmi.ServiceUIM, 7}})
			default:
				return errors.New("fixture: unexpected CTL mutation or Sync")
			}
		} else {
			if request.ServiceType != qmi.ServiceUIM || request.ClientID != 7 || !allocated {
				return errors.New("fixture: unowned service/CID")
			}
			switch request.MessageID {
			case 0x002f:
				apps := []uimApplication{{Kind: 2, State: 7, AID: usimPrefix}}
				if f.state.hasISIM {
					apps = append(apps, uimApplication{Kind: 5, State: 7, AID: isimPrefix})
				}
				output = append(output, qmi.TLV{Type: 0x10, Value: statusEnvelope(0, statusCard(apps...))})
			case 0x0047:
				output = append(output, qmi.TLV{Type: 0x10, Value: append([]byte{1}, slotStatusRaw(1, fixtureCardICCID(f.state.iccid))...)})
			case 0x003f:
				if len(input[1]) != 1 {
					return errors.New("fixture: missing physical slot")
				}
				if aid, ok := input[0x10]; ok {
					if len(aid) < 1 || int(aid[0]) != len(aid)-1 {
						return errors.New("fixture: invalid AID length")
					}
					channel, err := card.Open(context.Background(), input[1][0], aid[1:])
					if err != nil {
						return err
					}
					output = append(output, qmi.TLV{Type: 0x10, Value: []byte{channel}}, qmi.TLV{Type: 0x11, Value: []byte{0x90, 0}})
				} else {
					if len(input[0x11]) != 1 || !bytes.Equal(input[0x13], []byte{1}) {
						return errors.New("fixture: invalid owned channel close")
					}
					if err := card.CloseChannel(context.Background(), input[1][0], input[0x11][0]); err != nil {
						return err
					}
					if f.mode == "close-drop" && f.state.auths > 0 {
						return nil
					}
				}
			case 0x003b:
				if len(input[1]) != 1 || len(input[0x10]) != 1 || len(input[2]) < 6 || int(binary.LittleEndian.Uint16(input[2])) != len(input[2])-2 {
					return errors.New("fixture: invalid APDU framing")
				}
				raw, err := card.APDU(context.Background(), input[1][0], input[0x10][0], input[2][2:])
				if err != nil {
					return err
				}
				if f.mode == "apdu-drop" && input[2][3] == 0x88 {
					clear(raw)
					return nil
				}
				response := make([]byte, 2+len(raw))
				binary.LittleEndian.PutUint16(response, uint16(len(raw)))
				copy(response[2:], raw)
				clear(raw)
				output = append(output, qmi.TLV{Type: 0x10, Value: response})
			default:
				return errors.New("fixture: unexpected UIM command")
			}
		}
		frame := proxyResponse(request, output)
		clearProxyPacket(request)
		_, err = connection.Write(frame)
		clear(frame)
		for _, tlv := range output {
			clear(tlv.Value)
		}
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return err
		}
	}
}
func productionOpenerFixture(t *testing.T, mode string) (*SIMProvider, *proxyUIMFixture, *openerProbe, *memoryClaims) {
	t.Helper()
	pool, b, j, _ := bearerFixture(t)
	path := "/org/freedesktop/ModemManager1/Modem/0"
	b.mu.Lock()
	m := b.modems[path]
	m.QMIControl = "/dev/cdc-wdm-test"
	b.modems[path] = m
	b.mu.Unlock()
	selection, err := pool.Select(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Acquire(context.Background(), selection, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f, dial := startProxyUIM(t, newFakeUIM(selection), mode)
	probe := &openerProbe{}
	provider, err := newLinuxSIMProvider(lease, simOpenerDependencies{
		pin: func(Selection) (endpointGuard, error) { probe.pins.Add(1); return &fakeEndpointPin{probe: probe}, nil },
		acquire: func(context.Context, string) (sequenceGate, error) {
			probe.held.Add(1)
			return &fakeSequence{probe: probe}, nil
		},
		dial: func(ctx context.Context) (proxyChannel, error) { probe.dials.Add(1); return dial(ctx) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, f, probe, j
}
func TestCellularProductionOpenerRoundTripOverRealUnixProxy(t *testing.T) {
	p, f, probe, j := productionOpenerFixture(t, "")
	card, err := p.ReadCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if card.Application() != "ISIM" || card.IMSDomain() != "ims.example.net" {
		t.Fatal("card identity mismatch")
	}
	result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CK) != 16 || len(result.RES) != 4 {
		t.Fatal("AKA mismatch")
	}
	clearCardAKA(&result)
	if f.allocations.Load() != 2 || f.releases.Load() != 2 || probe.pins.Load() != probe.unpins.Load() || probe.held.Load() != 0 || f.state.auths != 1 {
		t.Fatal("proxy ownership/cleanup mismatch")
	}
	if j.record(p.lease.selection.key).SIMPending || j.record(p.lease.selection.key).SIMUncertain {
		t.Fatal("known transaction left a fence")
	}
	if err := p.lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCellularProductionOpenerLostAllocationOrReleaseNeverRetries(t *testing.T) {
	for _, mode := range []string{"alloc-drop", "alloc-malformed", "release-drop"} {
		t.Run(mode, func(t *testing.T) {
			p, f, probe, j := productionOpenerFixture(t, mode)
			if _, err := p.ReadCard(context.Background()); !errors.Is(err, ErrUncertain) {
				t.Fatal("unknown control operation accepted", err)
			}
			if f.allocations.Load() != 1 || probe.held.Load() != 1 || probe.unpins.Load() != 0 || len(p.retained) != 1 || !j.record(p.lease.selection.key).SIMUncertain {
				t.Fatal("unknown control ownership lost")
			}
			if _, err := p.ReadCard(context.Background()); err == nil || f.allocations.Load() != 1 {
				t.Fatal("unknown allocation retried")
			}
		})
	}
}
func TestCellularProductionOpenerKnownAllocationRejectionCleansTransport(t *testing.T) {
	p, f, probe, j := productionOpenerFixture(t, "alloc-negative")
	if _, err := p.ReadCard(context.Background()); !errors.Is(err, ErrSIMApplication) {
		t.Fatal("rejection not preserved", err)
	}
	if f.allocations.Load() != 1 || f.releases.Load() != 0 || probe.held.Load() != 0 || probe.unpins.Load() != 1 || j.record(p.lease.selection.key).SIMUncertain {
		t.Fatal("known rejection mislabeled unknown")
	}
}
func TestCellularProductionOpenerLostAuthAndChannelCloseWithholdKeys(t *testing.T) {
	for _, mode := range []string{"apdu-drop", "close-drop"} {
		t.Run(mode, func(t *testing.T) {
			p, f, probe, j := productionOpenerFixture(t, mode)
			card, err := p.ReadCard(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.Authenticate(context.Background(), card.Subscriber(), vowifi.AKAChallenge{})
			if !errors.Is(err, ErrUncertain) {
				t.Fatal("lost reply accepted", err)
			}
			assertNoAKA(t, result)
			f.state.mu.Lock()
			auths := f.state.auths
			f.state.mu.Unlock()
			if auths != 1 || f.allocations.Load() != 2 || f.releases.Load() != 1 || probe.held.Load() != 1 || !j.record(p.lease.selection.key).SIMUncertain {
				t.Fatal("AUTH replayed or unknown channel released")
			}
		})
	}
}
func TestCellularProductionOpenerCallerCancelStillObservesLateCID(t *testing.T) {
	p, f, probe, j := productionOpenerFixture(t, "late-allocation")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.ReadCard(ctx); done <- err }()
	select {
	case <-f.allocationEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("allocation not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller blocked waiting for CID")
	}
	if !j.record(p.lease.selection.key).SIMPending || probe.held.Load() != 1 {
		t.Fatal("ownership lost before late result")
	}
	close(f.allocationContinue)
	awaitClaim(t, j, p.lease.selection.key, func(r claimRecord) bool { return !r.SIMPending })
	if f.allocations.Load() != 1 || f.releases.Load() != 1 || probe.held.Load() != 0 || probe.unpins.Load() != 1 || j.record(p.lease.selection.key).SIMUncertain {
		t.Fatal("known late CID was retried or leaked")
	}
}

func TestCellularProductionOpenerMissingProxyReportsSetupErrorWithoutSIMMutation(t *testing.T) {
	p, _, _, j := productionOpenerFixture(t, "")
	probe := &openerProbe{}
	provider, err := newLinuxSIMProvider(p.lease, simOpenerDependencies{
		pin: func(Selection) (endpointGuard, error) { probe.pins.Add(1); return &fakeEndpointPin{probe: probe}, nil },
		acquire: func(context.Context, string) (sequenceGate, error) {
			probe.held.Add(1)
			return &fakeSequence{probe: probe}, nil
		},
		dial: func(context.Context) (proxyChannel, error) {
			return nil, errors.New("private socket path must not escape")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ReadCard(context.Background()); !errors.Is(err, ErrQMIProxy) {
		t.Fatal("missing proxy misreported as SIM/card failure", err)
	}
	if probe.pins.Load() != 1 || probe.unpins.Load() != 1 || probe.held.Load() != 0 || j.record(p.lease.selection.key).SIMUncertain {
		t.Fatal("unissued proxy failure retained a SIM fence")
	}
}
func TestCellularProductionConstructorHasNoActivationSideEffects(t *testing.T) {
	if _, err := NewCellularSIMProvider(nil); !errors.Is(err, ErrSelection) {
		t.Fatal("invalid lease accepted")
	}
	p, _, _, j := simFixture(t)
	if _, err := NewCellularSIMProvider(p.lease); !errors.Is(err, ErrQMIEndpoint) {
		t.Fatal("missing QMI port guessed")
	}
	p.lease.selection.snapshot.QMIControl = "/dev/cdc-wdm-not-opened"
	if _, err := NewCellularSIMProvider(p.lease); err != nil {
		t.Fatal("constructor unexpectedly opened endpoint", err)
	}
	if j.record(p.lease.selection.key).SIMPending {
		t.Fatal("construction started UIM transaction")
	}
}
