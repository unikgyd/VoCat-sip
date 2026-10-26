package device

import (
	"context"

	"vocat/internal/modemaudio"
)

// USBAudioBinding is metadata only: no AT, SIM/APDU, audio open or route change.
// A late observation may not be rebound to a replacement physical device.
func (manager *Manager) USBAudioBinding(ctx context.Context, id string) (modemaudio.Info, error) {
	state, err := manager.lookup(id)
	if err != nil {
		return modemaudio.Info{}, err
	}
	if err := manager.validateActive(id, state); err != nil {
		return modemaudio.Info{}, err
	}
	candidate := manager.candidateFor(state)
	info, err := manager.audioInspector.Inspect(ctx, candidate)
	if err != nil {
		return modemaudio.Info{}, err
	}
	if err := ctx.Err(); err != nil {
		return modemaudio.Info{}, err
	}
	manager.mu.RLock()
	current := manager.devices[id]
	valid := manager.started && current == state && state.discovered &&
		state.candidate.USBPath == candidate.USBPath && state.candidate.USBGeneration == candidate.USBGeneration &&
		state.candidate.ATPort.OpenPath() == candidate.ATPort.OpenPath() &&
		state.candidate.VendorID == candidate.VendorID && state.candidate.ProductID == candidate.ProductID
	manager.mu.RUnlock()
	if !valid {
		return modemaudio.Info{}, modemaudio.ErrChanged
	}
	info.VoiceBridgeReady = false
	return info, nil
}
