package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type finalizationRefreshController struct {
	fakeDeviceController
	refreshes int
}

func (c *finalizationRefreshController) Refresh(context.Context, string) (device.Snapshot, error) {
	c.refreshes++
	return device.Snapshot{}, nil
}

func TestESIMFinalizationWaitsForCompleteTargetSubscriptionWithoutReplayingSwitch(t *testing.T) {
	controller := &finalizationRefreshController{fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "physical", Discovered: true}}}
	s := &Server{devices: controller}
	reads := 0
	expected := vowifi.SIMIdentity{ICCID: "8900000000000000001", IMSI: "515030000000001"}
	s.readSubscription = func(context.Context, string) (vowifi.SIMIdentity, error) {
		reads++
		if controller.refreshes == 0 {
			return vowifi.SIMIdentity{}, device.ErrSIMObservation
		}
		return expected, nil
	}
	actual, err := s.awaitSwitchedSubscription(context.Background(), store.Device{ID: "physical"}, "physical", expected.ICCID)
	if err != nil || actual.ICCID != expected.ICCID || actual.IMSI != expected.IMSI || reads != 2 || controller.refreshes != 1 {
		t.Fatalf("final identity=%+v error=%v reads=%d", actual, err, reads)
	}
}

func TestESIMFinalizationRejectsUnknownIdentityAndChangedUSBGeneration(t *testing.T) {
	for _, changed := range []bool{false, true} {
		controller := &fakeDeviceController{entry: device.Device{ID: "physical", Discovered: true, Candidate: modem.Candidate{USBGeneration: "1:2"}}}
		s := &Server{devices: controller}
		s.readSubscription = func(context.Context, string) (vowifi.SIMIdentity, error) {
			if changed {
				controller.entry.Candidate.USBGeneration = "1:3"
				return vowifi.SIMIdentity{ICCID: "8900000000000000001", IMSI: "515030000000001"}, nil
			}
			return vowifi.SIMIdentity{ICCID: "8900000000000000001"}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := s.awaitSwitchedSubscription(ctx, store.Device{ID: "physical"}, "physical", "8900000000000000001")
		cancel()
		if !errors.Is(err, device.ErrSIMObservation) {
			t.Fatalf("changed=%v unknown/replaced identity accepted: %v", changed, err)
		}
	}
}
