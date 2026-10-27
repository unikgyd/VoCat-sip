//go:build linux

package device

import (
	"context"
	"errors"
	"sync"
)

// 10 is an internal selector, never a QMI protocol IP-family value. Each
// protocol family retains its own original WDS client and packet-data handle.
const qmiDataDualFamily uint8 = 10

type qmiOwnedDataStopper interface {
	StopOwned(context.Context) error
}

type qmiDataFamilyPreparer interface {
	PrepareFamily(context.Context, uint8) error
}

type dualQMIDataSession struct {
	parts     [2]qmiDataSession
	handles   [2]uint32
	attempted [2]bool
	verify    func(context.Context, qmiDataSession) error
	events    chan struct{}
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (s *dualQMIDataSession) TransportLost() bool {
	lost := false
	for i, part := range s.parts {
		if !s.attempted[i] {
			continue
		}
		transport, ok := part.(qmiDataTransportState)
		if !ok || !transport.TransportLost() {
			return false
		}
		lost = true
	}
	return lost
}

func validDataHandle(handle uint32) bool { return handle != 0 && handle != ^uint32(0) }

func (s *dualQMIDataSession) GetICCID(ctx context.Context) (string, error) {
	reader, ok := s.parts[0].(simWriteIdentityReader)
	if !ok {
		return "", ErrSIMObservation
	}
	return reader.GetICCID(ctx)
}

func (s *dualQMIDataSession) GetIMSI(ctx context.Context) (string, error) {
	reader, ok := s.parts[0].(simWriteIdentityReader)
	if !ok {
		return "", ErrSIMObservation
	}
	return reader.GetIMSI(ctx)
}

func (s *dualQMIDataSession) Start(ctx context.Context, apn, user, password string, auth, family uint8) (uint32, error) {
	if family != qmiDataDualFamily || s.attempted[0] || s.attempted[1] {
		return 0, ErrDataCleanupPending
	}
	for i, part := range s.parts {
		if err := ctx.Err(); err != nil {
			return s.handles[0], err
		}
		if s.verify != nil {
			if err := s.verify(ctx, part); err != nil {
				return s.handles[0], err
			}
		}
		// The write may commit even when its response is missing. Keep the
		// exact client until StopOwned observes that family disconnected.
		s.attempted[i] = true
		handle, err := part.Start(ctx, apn, user, password, auth, uint8(4+2*i))
		s.handles[i] = handle
		if err != nil || !validDataHandle(handle) {
			return s.handles[0], errors.Join(ErrDataCleanupPending, err)
		}
		if s.verify != nil {
			if err := s.verify(ctx, part); err != nil {
				return s.handles[0], err
			}
		}
	}
	return s.handles[0], nil
}

func (s *dualQMIDataSession) StopOwned(ctx context.Context) error {
	var failures []error
	for i, part := range s.parts {
		if !s.attempted[i] {
			continue
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, errors.Join(ErrDataCleanupPending, err))
			continue
		}
		err := error(ErrDataCleanupPending)
		if validDataHandle(s.handles[i]) {
			err = part.Stop(ctx, s.handles[i])
			if stoppedQMIDataError(err) {
				err = nil
			}
		}
		if err != nil {
			connected, observeErr := part.Connected(ctx)
			if observeErr != nil || connected {
				failures = append(failures, errors.Join(ErrDataCleanupPending, err, observeErr))
				continue
			}
		}
		s.attempted[i], s.handles[i] = false, 0
	}
	return errors.Join(failures...)
}

func (s *dualQMIDataSession) Stop(ctx context.Context, _ uint32) error { return s.StopOwned(ctx) }
func (*dualQMIDataSession) StopAny(context.Context, bool) error        { return ErrDataCleanupPending }

// Any live part retains ownership. A partial disconnect must never cause the
// manager to discard the other family's original client without stopping it.
func (s *dualQMIDataSession) Connected(ctx context.Context) (bool, error) {
	connected := false
	var failures []error
	for _, part := range s.parts {
		active, err := part.Connected(ctx)
		connected = connected || active
		failures = append(failures, err)
	}
	return connected, errors.Join(failures...)
}

func (s *dualQMIDataSession) AllConnected(ctx context.Context) error {
	for _, part := range s.parts {
		active, err := part.Connected(ctx)
		if err != nil || !active {
			return errors.Join(ErrDataCleanupPending, err)
		}
	}
	return nil
}

func (s *dualQMIDataSession) RawIP(ctx context.Context) (bool, error) {
	return s.parts[0].RawIP(ctx)
}
func (s *dualQMIDataSession) SetRawIP(ctx context.Context) error { return s.parts[0].SetRawIP(ctx) }
func (s *dualQMIDataSession) RuntimeIPv4(ctx context.Context) (qmiIPv4Settings, error) {
	return s.parts[0].RuntimeIPv4(ctx)
}
func (s *dualQMIDataSession) RuntimeIPv6(ctx context.Context) (qmiIPv6Settings, error) {
	reader, ok := s.parts[1].(qmiIPv6RuntimeSource)
	if !ok {
		return qmiIPv6Settings{}, ErrDataBackendUnavailable
	}
	return reader.RuntimeIPv6(ctx)
}

func (s *dualQMIDataSession) RegisterPacketStatusEvents(ctx context.Context) error {
	watch, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.events = make(chan struct{}, 1)
	for _, part := range s.parts {
		source, ok := part.(qmiDataEventSource)
		if !ok {
			continue
		}
		if err := source.RegisterPacketStatusEvents(ctx); err != nil {
			cancel()
			return err
		}
		go func() {
			for {
				select {
				case <-watch.Done():
					return
				case _, open := <-source.PacketStatusEvents():
					select {
					case s.events <- struct{}{}:
					default:
					}
					if !open {
						return
					}
				}
			}
		}()
	}
	return nil
}
func (s *dualQMIDataSession) PacketStatusEvents() <-chan struct{} { return s.events }
func (s *dualQMIDataSession) Close() error {
	var failures []error
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		for _, part := range s.parts {
			failures = append(failures, part.Close())
		}
	})
	return errors.Join(failures...)
}
