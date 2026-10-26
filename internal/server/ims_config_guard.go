package server

import (
	"context"
	"errors"
	"strings"

	"vocat/internal/store"
)

var errPhysicalDeviceAlias = errors.New("one physical modem can have only one configured device; edit the existing device instead")
var errDeviceAlreadyConfigured = errors.New("a device with this logical ID already exists")

// No hardware I/O occurs under imsConfigMu: physicalForConfig only reads the
// manager's cached inventory. Configuration persistence reserves the anchor
// before transport setters are invoked outside this short critical section.
func (s *Server) saveDeviceWithPhysicalGuard(ctx context.Context, config store.Device, creating bool) error {
	s.imsConfigMu.Lock()
	defer s.imsConfigMu.Unlock()
	if err := s.checkPhysicalDeviceUnique(ctx, config, creating); err != nil {
		return err
	}
	return s.store.UpsertDevice(ctx, config)
}
func (s *Server) checkPhysicalDeviceUnique(ctx context.Context, config store.Device, creating bool) error {
	configs, err := s.store.ListDevices(ctx)
	if err != nil {
		return err
	}
	_, targetID, targetPresent := s.physicalForConfig(config)
	for _, existing := range configs {
		if existing.ID == config.ID {
			if creating {
				return errDeviceAlreadyConfigured
			}
			continue
		}
		for _, pair := range [][2]string{{config.ModemIMEI, existing.ModemIMEI}, {config.USBPath, existing.USBPath}, {config.ControlDevice, existing.ControlDevice}, {config.ATPort, existing.ATPort}} {
			if strings.TrimSpace(pair[0]) != "" && strings.TrimSpace(pair[0]) == strings.TrimSpace(pair[1]) {
				return errPhysicalDeviceAlias
			}
		}
		if targetPresent {
			if _, otherID, present := s.physicalForConfig(existing); present && otherID == targetID {
				return errPhysicalDeviceAlias
			}
		}
	}
	return nil
}
