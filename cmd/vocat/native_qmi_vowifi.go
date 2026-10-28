package main

import (
	"context"
	"errors"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/integration"
)

// nativeQMIControllerMapper keeps the configured Web/API device ID stable
// while Linux exposes the physical MHI modem under its discovery ID.
type nativeQMIControllerMapper struct {
	Mapper  integration.ATMapper
	Devices *device.Manager
}

func (mapper nativeQMIControllerMapper) coexist(ctx context.Context, id string) (bool, error) {
	if mapper.Mapper.Store == nil {
		return false, nil
	}
	config, err := mapper.Mapper.Store.Device(ctx, id)
	if err != nil {
		return false, err
	}
	return config.Telephony != nil && !store.WiFiExclusive(config), nil
}

func (mapper nativeQMIControllerMapper) physical(configuredID string) (string, error) {
	entry, err := mapper.Mapper.Get(configuredID)
	if err != nil {
		return "", err
	}
	return entry.ID, nil
}

func (mapper nativeQMIControllerMapper) ReadNativeQMIIdentity(ctx context.Context, id string) (string, string, string, string, string, error) {
	physical, err := mapper.physical(id)
	if err != nil {
		return "", "", "", "", "", err
	}
	shared, err := mapper.coexist(ctx, id)
	if err != nil {
		return "", "", "", "", "", err
	}
	if shared {
		return mapper.Devices.ReadNativeQMIIdentityCoexist(ctx, physical)
	}
	return mapper.Devices.ReadNativeQMIIdentity(ctx, physical)
}

func (mapper nativeQMIControllerMapper) ReadSIMMetadata(ctx context.Context, id string) (vowifi.SIMMetadata, error) {
	if mapper.Mapper.Store != nil {
		if config, err := mapper.Mapper.Store.Device(ctx, id); err == nil && store.HostIMSConfigured(config) {
			return vowifi.SIMMetadata{}, errors.New("AT SIM metadata is unavailable while Host IMS owns the modem")
		}
	}
	return mapper.Mapper.ReadSIMMetadata(ctx, id)
}

func (mapper nativeQMIControllerMapper) ReadSMSCenter(ctx context.Context, id string) (string, error) {
	if mapper.Mapper.Store != nil {
		if config, err := mapper.Mapper.Store.Device(ctx, id); err == nil && store.HostIMSConfigured(config) {
			return "", errors.New("AT SMSC query is unavailable while Host IMS owns the modem")
		}
	}
	return mapper.Mapper.ReadSMSCenter(ctx, id)
}

func (mapper nativeQMIControllerMapper) ProbeNativeQMIApplication(ctx context.Context, id, preference string) ([]byte, string, error) {
	physical, err := mapper.physical(id)
	if err != nil {
		return nil, "", err
	}
	shared, err := mapper.coexist(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if shared {
		return mapper.Devices.ProbeNativeQMIApplicationCoexist(ctx, physical, preference)
	}
	return mapper.Devices.ProbeNativeQMIApplication(ctx, physical, preference)
}

func (mapper nativeQMIControllerMapper) AuthenticateNativeQMI(ctx context.Context, id string, aid, apdu []byte) ([]byte, error) {
	physical, err := mapper.physical(id)
	if err != nil {
		return nil, err
	}
	shared, err := mapper.coexist(ctx, id)
	if err != nil {
		return nil, err
	}
	if shared {
		return mapper.Devices.AuthenticateNativeQMICoexist(ctx, physical, aid, apdu)
	}
	return mapper.Devices.AuthenticateNativeQMI(ctx, physical, aid, apdu)
}

func (mapper nativeQMIControllerMapper) NativeQMIRadioSnapshot(ctx context.Context, id string) (int, bool, error) {
	physical, err := mapper.physical(id)
	if err != nil {
		return 0, false, err
	}
	return mapper.Devices.NativeQMIRadioSnapshot(ctx, physical)
}

func (mapper nativeQMIControllerMapper) StopNativeQMICellularData(ctx context.Context, id string) error {
	physical, err := mapper.physical(id)
	if err != nil {
		return err
	}
	return mapper.Devices.StopNativeQMICellularData(ctx, physical)
}

func (mapper nativeQMIControllerMapper) SetNativeQMIRadioOff(ctx context.Context, id string, off bool) error {
	physical, err := mapper.physical(id)
	if err != nil {
		return err
	}
	return mapper.Devices.SetNativeQMIRadioOff(ctx, physical, off)
}

type coexistAwareVoWiFiAdapter struct {
	database *store.Store
	deviceID string
	at       vowifiDeviceAdapter
	qmi      vowifiDeviceAdapter
}

func (a coexistAwareVoWiFiAdapter) selectAdapter(ctx context.Context) vowifiDeviceAdapter {
	if a.database != nil && a.qmi != nil {
		if config, err := a.database.Device(ctx, a.deviceID); err == nil && store.HostIMSConfigured(config) && !store.WiFiExclusive(config) {
			return a.qmi
		}
	}
	return a.at
}

func (a coexistAwareVoWiFiAdapter) ReadIdentity(ctx context.Context, deviceID string) (vowifi.SIMIdentity, error) {
	return a.selectAdapter(ctx).ReadIdentity(ctx, deviceID)
}

func (a coexistAwareVoWiFiAdapter) ReadSMSCenter(ctx context.Context, deviceID string) (string, error) {
	if reader, ok := a.selectAdapter(ctx).(vowifi.SMSCenterReader); ok {
		return reader.ReadSMSCenter(ctx, deviceID)
	}
	return "", errors.New("SMS center reader unavailable")
}

func (a coexistAwareVoWiFiAdapter) CheckReady(ctx context.Context, identity vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	return a.selectAdapter(ctx).CheckReady(ctx, identity)
}

func (a coexistAwareVoWiFiAdapter) Authenticate(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	return a.selectAdapter(ctx).Authenticate(ctx, identity, challenge)
}

func (a coexistAwareVoWiFiAdapter) AuthenticateWithPreference(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge, preference string) (vowifi.AKAResult, error) {
	return a.selectAdapter(ctx).AuthenticateWithPreference(ctx, identity, challenge, preference)
}

func (a coexistAwareVoWiFiAdapter) Snapshot(ctx context.Context, deviceID string) (vowifi.RadioSnapshot, error) {
	return a.selectAdapter(ctx).Snapshot(ctx, deviceID)
}

func (a coexistAwareVoWiFiAdapter) StopCellularData(ctx context.Context, deviceID string) error {
	return a.selectAdapter(ctx).StopCellularData(ctx, deviceID)
}

func (a coexistAwareVoWiFiAdapter) EnterVoWiFiRFOff(ctx context.Context, deviceID string) error {
	return a.selectAdapter(ctx).EnterVoWiFiRFOff(ctx, deviceID)
}

func (a coexistAwareVoWiFiAdapter) Restore(ctx context.Context, deviceID string, snapshot vowifi.RadioSnapshot) error {
	return a.selectAdapter(ctx).Restore(ctx, deviceID, snapshot)
}

