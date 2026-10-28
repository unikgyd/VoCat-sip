//go:build linux

package hostims

import (
	"context"
	"errors"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

type runtimeOpenerDependencies struct {
	qmi      simOpenerDependencies
	topology func(*BearerLease, endpointGuard) (wdsTopology, error)
}

// NewCellularRuntimeSource does not activate an endpoint or create a PDN.
// Read explicitly opens a trusted local proxy and owns every allocated CID.
func NewCellularRuntimeSource(lease *BearerLease) (*RuntimeSource, error) {
	return newLinuxRuntimeSource(lease, runtimeOpenerDependencies{
		qmi: linuxOpenerDependencies(),
		topology: func(lease *BearerLease, pin endpointGuard) (wdsTopology, error) {
			control, ok := pin.(*qmiEndpointPin)
			if !ok {
				return nil, ErrQMIEndpoint
			}
			return pinWDSTopology(lease, control)
		},
	})
}
func newLinuxRuntimeSource(lease *BearerLease, deps runtimeOpenerDependencies) (*RuntimeSource, error) {
	if lease == nil || lease.selection.snapshot.QMIControl == "" {
		return nil, ErrQMIEndpoint
	}
	if deps.qmi.pin == nil || deps.qmi.acquire == nil || deps.qmi.dial == nil || deps.topology == nil {
		return nil, ErrSelection
	}
	return newRuntimeSource(lease, func(ctx context.Context, verify func(context.Context) error) (runtimeSession, error) {
		return openCellularRuntime(ctx, lease, verify, deps)
	})
}

type ownedWDSSession struct {
	lease             *BearerLease
	guard             func(context.Context) error
	pin               endpointGuard
	topology          wdsTopology
	gate              sequenceGate
	proxy             proxyChannel
	cids              []byte
	uncertain, closed bool
	bindingKey        string
	revision          uint64
	watched, changed  bool
}

func (s *ownedWDSSession) Uncertain() bool { return s.uncertain }
func (s *ownedWDSSession) BindingKey() string {
	if s.changed {
		return ""
	}
	return s.bindingKey
}
func (s *ownedWDSSession) verify(ctx context.Context) error {
	if s.closed || s.uncertain || s.guard(ctx) != nil || s.pin == nil || s.pin.Verify(ctx) != nil || s.topology == nil || s.topology.Verify(ctx) != nil || s.proxy == nil || !s.proxy.healthy() {
		return ErrChanged
	}
	return nil
}
func (s *ownedWDSSession) Query(ctx context.Context, family byte) (Runtime, error) {
	if family != 4 && family != 6 || len(s.cids) >= 2 {
		return Runtime{}, ErrParameters
	}
	if err := s.verify(ctx); err != nil {
		return Runtime{}, err
	}
	if s.watched && s.proxy.wdsRevision() != s.revision {
		s.changed = true
		return Runtime{}, ErrChanged
	}
	cid, err := ownedServiceCID(ctx, s.proxy, qmi.ServiceWDS, 0)
	if err != nil {
		if errors.Is(err, ErrUncertain) {
			s.uncertain = true
			return Runtime{}, ErrUncertain
		}
		if errors.Is(err, ErrSIMApplication) {
			return Runtime{}, ErrWDSUnavailable
		}
		return Runtime{}, err
	}
	for _, previous := range s.cids {
		if cid == previous {
			s.uncertain = true
			return Runtime{}, ErrUncertain
		}
	}
	s.cids = append(s.cids, cid)
	if err := s.verify(ctx); err != nil {
		return Runtime{}, err
	}
	if !s.watched {
		s.revision = s.proxy.wdsRevision()
		s.watched = true
	}
	runtime, err := s.readOriginalRuntime(ctx, cid, family)
	if err != nil {
		return Runtime{}, err
	}
	if s.proxy.wdsRevision() != s.revision {
		s.changed = true
		return Runtime{}, ErrChanged
	}
	return runtime, nil
}

func (s *ownedWDSSession) readOriginalRuntime(ctx context.Context, cid, family byte) (Runtime, error) {
	if s.lease == nil {
		return Runtime{}, ErrSelection
	}
	runtime, err := readBoundRuntime(ctx, s.proxy, cid, s.topology.Binding(), family, s.verify)
	if err != nil {
		return Runtime{}, err
	}
	for _, address := range s.lease.configuration.Addresses {
		if address.Address.Addr().Is4() == (family == 4) {
			return normalizeBoundRuntime(runtime, address, s.lease.claim.record.Options.APN)
		}
	}
	return Runtime{}, ErrRuntime
}
func (s *ownedWDSSession) Close(ctx context.Context) error {
	if s.closed {
		return nil
	}
	if s.uncertain {
		return ErrUncertain
	}
	for len(s.cids) > 0 {
		if err := s.verify(ctx); err != nil {
			s.uncertain = true
			return ErrUncertain
		}
		index := len(s.cids) - 1
		if _, err := ownedServiceCID(ctx, s.proxy, qmi.ServiceWDS, s.cids[index]); err != nil {
			s.uncertain = true
			return ErrUncertain
		}
		s.cids = s.cids[:index]
	}
	if s.proxy != nil {
		if s.proxy.Close() != nil {
			s.uncertain = true
			return ErrUncertain
		}
		if s.watched && s.proxy.wdsRevision() != s.revision {
			s.changed = true
		}
		s.proxy = nil
	}
	if s.topology != nil {
		if s.topology.Close() != nil {
			s.uncertain = true
			return ErrUncertain
		}
		s.topology = nil
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
	s.closed = true
	return nil
}
func openCellularRuntime(ctx context.Context, lease *BearerLease, verify func(context.Context) error, deps runtimeOpenerDependencies) (runtimeSession, error) {
	if err := verify(ctx); err != nil {
		return nil, err
	}
	pin, err := deps.qmi.pin(lease.selection)
	if err != nil {
		return nil, ErrQMIEndpoint
	}
	s := &ownedWDSSession{lease: lease, pin: pin, guard: verify}
	cleanFailure := func(cause error) (runtimeSession, error) {
		cleanup, finish := context.WithTimeout(context.Background(), 4*time.Second)
		defer finish()
		if s.Close(cleanup) != nil {
			return s, ErrUncertain
		}
		return nil, safeRuntimeError(cause)
	}
	topology, err := deps.topology(lease, pin)
	if err != nil {
		return cleanFailure(ErrQMIEndpoint)
	}
	s.topology = topology
	if !s.topology.Binding().valid() || !hashString(s.topology.Key()) {
		return cleanFailure(ErrQMIEndpoint)
	}
	s.bindingKey = s.topology.Key()
	gate, err := deps.qmi.acquire(ctx, pin.Path())
	if err != nil {
		return cleanFailure(err)
	}
	s.gate = gate
	if pin.Verify(ctx) != nil || s.topology.Verify(ctx) != nil || verify(ctx) != nil {
		return cleanFailure(ErrChanged)
	}
	proxy, err := deps.qmi.dial(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return cleanFailure(ctx.Err())
		}
		return cleanFailure(ErrQMIProxy)
	}
	s.proxy = proxy
	if err := s.verify(ctx); err != nil {
		return cleanFailure(err)
	}
	packet, err := s.proxy.SendRequest(ctx, qmi.ServiceControl, 0, 0xff00, []qmi.TLV{{Type: 1, Value: []byte(pin.Path())}})
	if err != nil {
		return cleanFailure(ErrTransport)
	}
	_, err = controlResult(packet, 0xff00, false)
	clearProxyPacket(packet)
	if err != nil {
		return cleanFailure(err)
	}
	if err := s.verify(ctx); err != nil {
		return cleanFailure(err)
	}
	return s, nil
}
