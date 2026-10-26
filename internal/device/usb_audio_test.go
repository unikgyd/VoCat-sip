package device

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/modem"
	"vocat/internal/modemaudio"
)

type audioInspectorFunc func(context.Context, modem.Candidate) (modemaudio.Info, error)

func (f audioInspectorFunc) Inspect(ctx context.Context, c modem.Candidate) (modemaudio.Info, error) {
	return f(ctx, c)
}

func TestUSBAudioDeviceInspectionIsReadOnlyAndFencesReplacement(t *testing.T) {
	for _, change := range []string{"none", "generation", "removed", "stopped", "at-port"} {
		t.Run(change, func(t *testing.T) {
			state := &managedDevice{discovered: true, candidate: modem.Candidate{ID: "selected", USBPath: "/sys/bus/usb/devices/1-2", USBGeneration: "1:5", VendorID: "2c7c", ProductID: "0125", ATPort: modem.Port{Path: "/synthetic/at"}}}
			manager := &Manager{started: true, devices: map[string]*managedDevice{"selected": state}}
			manager.audioInspector = audioInspectorFunc(func(_ context.Context, c modem.Candidate) (modemaudio.Info, error) {
				if c.USBPath != state.candidate.USBPath || c.USBGeneration != "1:5" {
					t.Fatal("wrong physical candidate")
				}
				manager.mu.Lock()
				switch change {
				case "generation":
					state.candidate.USBGeneration = "1:6"
				case "removed":
					delete(manager.devices, "selected")
				case "stopped":
					manager.started = false
				case "at-port":
					state.candidate.ATPort.Path = "/synthetic/replacement"
				}
				manager.mu.Unlock()
				return modemaudio.Info{Pairing: "bound", VoiceBridgeReady: true}, nil
			})
			info, e := manager.USBAudioBinding(context.Background(), "selected")
			if change == "none" {
				if e != nil || info.Pairing != "bound" || info.VoiceBridgeReady {
					t.Fatal(info, e)
				}
			} else if !errors.Is(e, modemaudio.ErrChanged) {
				t.Fatal(change, e)
			}
		})
	}
}
