//go:build linux

package hostims

import (
	"context"
	"strings"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"vocat/internal/qmiport"
)

type endpointGuard interface {
	Path() string
	Verify(context.Context) error
	Close() error
}
type sequenceGate interface{ Release() }
type proxyChannel interface {
	RequestSender
	healthy() bool
	wdsRevision() uint64
	Close() error
}
type simOpenerDependencies struct {
	pin     func(Selection) (endpointGuard, error)
	acquire func(context.Context, string) (sequenceGate, error)
	dial    func(context.Context) (proxyChannel, error)
}

// NewCellularSIMProvider constructs the cellular-only, lease-bound provider.
// No endpoint or SIM is opened until ReadCard/CheckReady/Authenticate. A running
// trusted local qmi-proxy is required; no raw fallback or implicit CTL Sync.
func NewCellularSIMProvider(lease *BearerLease) (*SIMProvider, error) {
	return newLinuxSIMProvider(lease, linuxOpenerDependencies())
}
func linuxOpenerDependencies() simOpenerDependencies {
	return simOpenerDependencies{
		pin: func(s Selection) (endpointGuard, error) {
			return pinQMIEndpoint(s, endpointRoots{dev: "/dev", sys: "/sys"})
		},
		acquire: func(ctx context.Context, path string) (sequenceGate, error) {
			if strings.HasPrefix(path, "/dev/cdc-wdm") {
				return qmiport.AcquireSequence(ctx, path)
			}
			// Native WWAN drivers may remove the control node when their last
			// descriptor closes. This keeper never sends raw QMI packets.
			return qmiport.Acquire(ctx, path)
		},
		dial: func(ctx context.Context) (proxyChannel, error) { return dialQMIProxy(ctx) },
	}
}
func newLinuxSIMProvider(lease *BearerLease, deps simOpenerDependencies) (*SIMProvider, error) {
	if lease == nil || lease.claim == nil || !hashString(lease.token) || deps.pin == nil || deps.acquire == nil || deps.dial == nil {
		return nil, ErrSelection
	}
	if lease.selection.snapshot.QMIControl == "" {
		return nil, ErrQMIEndpoint
	}
	var provider *SIMProvider
	provider, err := newSIMProvider(lease, func(ctx context.Context, s Selection) (uimSession, error) {
		return openCellularUIM(ctx, provider, s, deps)
	})
	return provider, err
}

type ownedUIMSession struct {
	wire      *qmiUIMSession
	provider  *SIMProvider
	pin       endpointGuard
	gate      sequenceGate
	proxy     proxyChannel
	cid       byte
	uncertain bool
}

func (s *ownedUIMSession) verify(ctx context.Context) error {
	if s.uncertain || s.pin.Verify(ctx) != nil || s.proxy == nil || !s.proxy.healthy() || s.provider.verifyLease(ctx) != nil {
		return ErrSIM
	}
	snapshot, err := s.provider.lease.pool.verify(ctx, s.provider.lease.selection)
	if err != nil || snapshot.QMIControl != s.provider.lease.selection.snapshot.QMIControl {
		return ErrChanged
	}
	return s.pin.Verify(ctx)
}
func (s *ownedUIMSession) release() error {
	if s.uncertain {
		return ErrUncertain
	}
	if s.cid != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if s.verify(ctx) != nil {
			return ErrUncertain
		}
		if _, err := ownedUIMCID(ctx, s.proxy, s.cid); err != nil {
			s.uncertain = true
			return ErrUncertain
		}
		s.cid = 0
	}
	if s.proxy != nil {
		if s.proxy.Close() != nil {
			s.uncertain = true
			return ErrUncertain
		}
		s.proxy = nil
	}
	if s.pin != nil {
		if s.pin.Close() != nil {
			s.uncertain = true
			return ErrUncertain
		}
		s.pin = nil
	}
	if s.gate != nil {
		s.gate.Release()
		s.gate = nil
	}
	return nil
}
func (s *ownedUIMSession) Uncertain() bool { return s.uncertain || s.wire != nil && s.wire.Uncertain() }
func (s *ownedUIMSession) Close() error {
	if s.wire != nil {
		return s.wire.Close()
	}
	return s.release()
}
func (s *ownedUIMSession) Card(ctx context.Context, binding cardBinding) (uimCard, string, error) {
	if s.wire == nil {
		return uimCard{}, "", ErrSIM
	}
	return s.wire.Card(ctx, binding)
}
func (s *ownedUIMSession) Open(ctx context.Context, slot byte, aid []byte) (byte, error) {
	if s.wire == nil {
		return 0, ErrSIM
	}
	return s.wire.Open(ctx, slot, aid)
}
func (s *ownedUIMSession) APDU(ctx context.Context, slot, channel byte, command []byte) ([]byte, error) {
	if s.wire == nil {
		return nil, ErrSIM
	}
	return s.wire.APDU(ctx, slot, channel, command)
}
func (s *ownedUIMSession) CloseChannel(ctx context.Context, slot, channel byte) error {
	if s.wire == nil {
		return ErrSIM
	}
	return s.wire.CloseChannel(ctx, slot, channel)
}

func openCellularUIM(ctx context.Context, provider *SIMProvider, selection Selection, deps simOpenerDependencies) (uimSession, error) {
	if err := provider.verifyLease(ctx); err != nil {
		return nil, err
	}
	snapshot, err := selection.pool.verify(ctx, selection)
	if err != nil || snapshot.QMIControl != selection.snapshot.QMIControl {
		return nil, ErrChanged
	}
	pin, err := deps.pin(selection)
	if err != nil {
		return nil, ErrQMIEndpoint
	}
	resource := &ownedUIMSession{provider: provider, pin: pin}
	cleanFailure := func(cause error) (uimSession, error) {
		if resource.release() != nil {
			resource.uncertain = true
			return resource, ErrUncertain
		}
		return nil, safeSIMError(cause)
	}
	gate, err := deps.acquire(ctx, pin.Path())
	if err != nil {
		return cleanFailure(err)
	}
	resource.gate = gate
	if pin.Verify(ctx) != nil || provider.verifyLease(ctx) != nil {
		return cleanFailure(ErrChanged)
	}
	proxy, err := deps.dial(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return cleanFailure(ctx.Err())
		}
		return cleanFailure(ErrQMIProxy)
	}
	resource.proxy = proxy
	if resource.verify(ctx) != nil {
		return cleanFailure(ErrChanged)
	}
	// Proxy device open is NOT a CID allocation or a modem reset. Closing this
	// connection after a failed handshake leaves no owned UICC channel.
	response, err := resource.proxy.SendRequest(ctx, qmi.ServiceControl, 0, 0xff00, []qmi.TLV{{Type: 1, Value: []byte(pin.Path())}})
	if err != nil {
		return cleanFailure(ErrTransport)
	}
	_, err = controlResult(response, 0xff00, false)
	clearProxyPacket(response)
	if err != nil {
		return cleanFailure(err)
	}
	if resource.verify(ctx) != nil {
		return cleanFailure(ErrChanged)
	}
	resource.cid, err = ownedUIMCID(ctx, resource.proxy, 0)
	if err != nil {
		if err == ErrUncertain {
			resource.uncertain = true
			return resource, ErrUncertain
		}
		return cleanFailure(err)
	}
	if resource.verify(ctx) != nil {
		resource.uncertain = true
		return resource, ErrUncertain
	}
	resource.wire, err = newQMIUIMSession(resource.proxy, resource.cid, resource.verify, resource.release)
	if err != nil {
		resource.uncertain = true
		return resource, ErrUncertain
	}
	return resource, nil
}
