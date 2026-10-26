package server

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"vocat/internal/device"
	"vocat/internal/modulecall"
	"vocat/internal/store"
)

type moduleVoiceController interface {
	ModuleVoiceAvailable(context.Context, string) error
	AcquireModuleVoice(context.Context, string) (modulecall.Lease, error)
}

type moduleSignalingController interface {
	AcquireModuleSignaling(context.Context, string, string, string) (modulecall.Lease, error)
}
type moduleVoiceBackend struct{ s *Server }

func (b moduleVoiceBackend) config(ctx context.Context, id string) (store.Device, string, error) {
	config, err := b.s.store.Device(ctx, id)
	if err != nil {
		return store.Device{}, "", err
	}
	if config.Telephony != nil && !store.AccessEnabled(config, store.IMSModeModule) || config.Telephony == nil && config.IMSMode != store.IMSModeModule {
		return store.Device{}, "", modulecall.ErrUnavailable
	}
	entry, physical, present := b.s.physicalForConfig(config)
	if !present || entry.Snapshot == nil || entry.Snapshot.IMSI == "" || entry.Snapshot.ICCID == "" || device.IsPlaceholderIMSI(entry.Snapshot.IMSI) {
		return store.Device{}, "", modulecall.ErrUnavailable
	}
	if device.RegionBlockReason(entry.Snapshot.IMSI) != "" {
		return store.Device{}, "", modulecall.ErrUnavailable
	}
	if config.Telephony != nil && config.Telephony.ICCID != entry.Snapshot.ICCID {
		return store.Device{}, "", modulecall.ErrChanged
	}
	if config.ModemIMEI != "" && config.ModemIMEI != entry.Snapshot.IMEI {
		return store.Device{}, "", modulecall.ErrChanged
	}
	return config, physical, nil
}
func (b moduleVoiceBackend) Available(ctx context.Context, id string) error {
	_, physical, err := b.config(ctx, id)
	if err != nil {
		return err
	}
	controller, ok := b.s.devices.(moduleVoiceController)
	if !ok {
		return modulecall.ErrUnavailable
	}
	return controller.ModuleVoiceAvailable(ctx, physical)
}
func (b moduleVoiceBackend) Acquire(ctx context.Context, id string) (modulecall.Lease, error) {
	config, physical, err := b.config(ctx, id)
	if err != nil {
		return nil, err
	}
	controller, ok := b.s.devices.(moduleVoiceController)
	if !ok {
		return nil, modulecall.ErrUnavailable
	}
	line, err := controller.AcquireModuleVoice(ctx, physical)
	if err != nil {
		return nil, err
	}
	lease := &serverModuleLease{Lease: line, backend: b, config: config, physical: physical}
	lease.valid.Store(true)
	if err := lease.check(ctx); err != nil {
		line.Close()
		return nil, err
	}
	return lease, nil
}

func (b moduleVoiceBackend) AvailableSignaling(ctx context.Context, id string) error {
	if _, _, err := b.config(ctx, id); err != nil {
		return err
	}
	if _, ok := b.s.devices.(moduleSignalingController); !ok {
		return modulecall.ErrUnavailable
	}
	return nil
}

func (b moduleVoiceBackend) AcquireSignaling(ctx context.Context, id string) (modulecall.Lease, error) {
	config, physical, err := b.config(ctx, id)
	if err != nil {
		return nil, err
	}
	controller, ok := b.s.devices.(moduleSignalingController)
	if !ok {
		return nil, modulecall.ErrUnavailable
	}
	identity, err := b.s.observedTelephonySubscription(ctx, config)
	if err != nil {
		return nil, err
	}
	line, err := controller.AcquireModuleSignaling(ctx, physical, identity.ICCID, identity.IMSI)
	if err != nil {
		return nil, err
	}
	lease := &serverModuleLease{Lease: line, backend: b, config: config, physical: physical}
	lease.valid.Store(true)
	if err := lease.check(ctx); err != nil {
		line.Close()
		return nil, err
	}
	return lease, nil
}

type serverModuleLease struct {
	modulecall.Lease
	backend  moduleVoiceBackend
	config   store.Device
	physical string
	valid    atomic.Bool
}

func (l *serverModuleLease) check(ctx context.Context) error {
	config, physical, err := l.backend.config(ctx, l.config.ID)
	// A canceled read or temporary database failure is not evidence that the
	// original modem disappeared. Keep its reservation for confirmed cleanup.
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, modulecall.ErrUnavailable) && !errors.Is(err, modulecall.ErrChanged) {
		return err
	}
	if err != nil || physical != l.physical || !store.SameSubscription(config, l.config) || config.ControlDevice != l.config.ControlDevice || config.ATPort != l.config.ATPort || config.USBPath != l.config.USBPath || config.ModemIMEI != l.config.ModemIMEI {
		l.valid.Store(false)
		return modulecall.ErrChanged
	}
	if !l.Current() {
		return modulecall.ErrChanged
	}
	return nil
}
func (l *serverModuleLease) Current() bool { return l.valid.Load() && l.Lease.Current() }
func (l *serverModuleLease) RecordingICCID() string {
	if identity, ok := l.Lease.(interface{ RecordingICCID() string }); ok {
		return identity.RecordingICCID()
	}
	return ""
}
func (l *serverModuleLease) Read(ctx context.Context) ([]modulecall.Record, error) {
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	return l.Lease.Read(ctx)
}
func (l *serverModuleLease) Execute(ctx context.Context, action modulecall.Action) (bool, error) {
	if err := l.check(ctx); err != nil {
		return false, err
	}
	return l.Lease.Execute(ctx, action)
}
func (l *serverModuleLease) OpenAudio(ctx context.Context, current func() bool) (modulecall.Audio, error) {
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	return l.Lease.OpenAudio(ctx, func() bool { return l.Current() && current() })
}
func (s *Server) moduleVoiceSelected(ctx context.Context, id string) bool {
	if s.moduleCalls == nil || s.store == nil {
		return false
	}
	config, err := s.store.Device(ctx, id)
	return err == nil && (config.Telephony != nil && store.EffectiveIMSMode(config) == store.IMSModeModule && store.AccessEnabled(config, store.IMSModeModule) || config.Telephony == nil && config.IMSMode == store.IMSModeModule)
}

func (s *Server) moduleVoiceEnabled(config store.Device) bool {
	return config.Telephony != nil && store.AccessEnabled(config, store.IMSModeModule) || config.Telephony == nil && config.IMSMode == store.IMSModeModule
}
func (s *Server) moduleVoiceCall(id, cid string) bool {
	return s.moduleCalls != nil && (modulecall.ID(cid) || s.moduleCalls.Owns(id, cid))
}
