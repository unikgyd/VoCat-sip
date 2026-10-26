package device

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetadataReadsCancelWhileOriginalATOwnerIsBusy(t *testing.T) {
	for _, action := range []string{"usbnet", "cellular_ims", "refresh"} {
		t.Run(action, func(t *testing.T) {
			client := &transcriptClient{}
			manager, id := newStartedTestManager(t, client)
			state := manager.devices[id]
			state.opMu.Lock()
			locked := true
			defer func() {
				if locked {
					state.opMu.Unlock()
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if action == "usbnet" {
					_, err = manager.USBNetMode(ctx, id)
				} else if action == "cellular_ims" {
					_, err = manager.CellularIMS(ctx, id)
				} else {
					_, err = manager.Refresh(ctx, id)
				}
				done <- err
			}()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("metadata request remained queued behind another AT owner")
			}
			state.opMu.Unlock()
			locked = false
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.unexpected != nil {
				t.Fatal("canceled metadata issued AT I/O", client.unexpected)
			}
		})
	}
}
