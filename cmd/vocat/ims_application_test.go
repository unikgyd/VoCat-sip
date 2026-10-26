package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/linelock"
	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/vowifi/ims"
)

func TestIMSBackgroundGateHoldsModeFenceUntilLegacyWorkCompletes(t *testing.T) {
	manager := newRegionTestManager(t, &fakeModemClient{})
	database := newRegionTestStore(t)
	config := store.Device{ID: "gate-line", Name: "Gate", ATPort: "/dev/ttyUSB2"}
	if err := database.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	current, entry, unlock, ok := lockLegacyConfiguredRead(context.Background(), database, manager, config.ID)
	if !ok || current.ID != config.ID || entry.ID != regionTestDeviceID {
		t.Fatal("legacy line could not enter gate")
	}
	if linelock.For(config.ID).TryLock() {
		linelock.For(config.ID).Unlock()
		unlock()
		t.Fatal("mode writer entered unfinished legacy I/O")
	}
	if !linelock.For("independent-line").TryLock() {
		unlock()
		t.Fatal("legacy I/O blocked another device")
	}
	linelock.For("independent-line").Unlock()
	unlock()
	gate := linelock.For(config.ID)
	gate.Lock()
	config.IMSMode, config.IMSEnabled = store.IMSModeHost, true
	err := database.UpsertDevice(context.Background(), config)
	gate.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, release, ok := lockLegacyConfiguredRead(context.Background(), database, manager, config.ID); ok {
		release()
		t.Fatal("Host line inherited legacy work")
	}
}

func TestIMSBackgroundGateRereadsAfterModeWriterWins(t *testing.T) {
	manager := newRegionTestManager(t, &fakeModemClient{})
	database := newRegionTestStore(t)
	config := store.Device{ID: "stale-line", Name: "Stale", ATPort: "/dev/ttyUSB2"}
	if err := database.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	// Simulate a caller holding a stale pre-switch ListDevices row.
	listed, err := database.ListDevices(context.Background())
	if err != nil || len(listed) != 1 {
		t.Fatal(err)
	}
	gate := linelock.For(config.ID)
	gate.Lock()
	started, result := make(chan struct{}), make(chan bool, 1)
	go func() {
		close(started)
		_, _, release, ok := lockLegacyConfiguredRead(context.Background(), database, manager, listed[0].ID)
		if ok {
			release()
		}
		result <- ok
	}()
	<-started
	config.IMSMode, config.IMSEnabled = store.IMSModeHost, true
	err = database.UpsertDevice(context.Background(), config)
	gate.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-result:
		if ok {
			t.Fatal("stale legacy row survived lock-internal reload")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legacy gate did not finish")
	}
	got, err := database.Device(context.Background(), config.ID)
	if err != nil || got.IMSMode != store.IMSModeHost {
		t.Fatal("mode was overwritten", err)
	}
}

func TestIMSBackgroundPhysicalGateLocksExistingAliasesAndRejectsHost(t *testing.T) {
	manager := newRegionTestManager(t, &fakeModemClient{})
	database := newRegionTestStore(t)
	for _, id := range []string{"physical-a", "physical-alias"} {
		if err := database.UpsertDevice(context.Background(), store.Device{ID: id, Name: id, ATPort: "/dev/ttyUSB2"}); err != nil {
			t.Fatal(err)
		}
	}
	unlock, ok := lockLegacyPhysicalRead(context.Background(), database, manager, regionTestDeviceID)
	if !ok {
		t.Fatal("legacy physical gate did not resolve aliases")
	}
	for _, id := range []string{"physical-a", "physical-alias"} {
		if linelock.For(id).TryLock() {
			linelock.For(id).Unlock()
			unlock()
			t.Fatal("alias writer bypassed physical work", id)
		}
	}
	unlock()
	config, err := database.Device(context.Background(), "physical-alias")
	if err != nil {
		t.Fatal(err)
	}
	config.IMSMode, config.IMSEnabled = store.IMSModeHost, true
	if err := database.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if release, ok := lockLegacyPhysicalRead(context.Background(), database, manager, regionTestDeviceID); ok {
		release()
		t.Fatal("Host/unknown physical owner was probed through legacy alias")
	}
}

func TestIMSBackgroundPhysicalGateKeepsPCSCReaderAlias(t *testing.T) {
	config := store.Device{ID: "reader-line", DeviceType: store.DeviceTypeUSBSIMReader, ControlDevice: "synthetic-reader"}
	entry := device.Device{ID: "reader-discovery", Discovered: true, Candidate: modem.Candidate{ReaderName: "synthetic-reader"}}
	ids, ok := legacyPhysicalGateIDs([]store.Device{config}, []device.Device{entry}, entry.ID)
	if !ok || len(ids) != 1 || ids[0] != config.ID {
		t.Fatal("PCSC reader was not included in its line gate")
	}
}

type backgroundCountingClient struct{ calls atomic.Int32 }

func (c *backgroundCountingClient) Execute(context.Context, string) (modem.Response, error) {
	c.calls.Add(1)
	return modem.Response{}, context.Canceled
}
func (*backgroundCountingClient) WaitURC(context.Context, func(string) bool) (string, error) {
	return "", context.Canceled
}
func (*backgroundCountingClient) Close() error { return nil }

func TestIMSBackgroundHostPreventsStaleDefaultAndRecoveryIO(t *testing.T) {
	client := &backgroundCountingClient{}
	manager := newRegionTestManager(t, client)
	database := newRegionTestStore(t)
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "host-line", Name: "Host", ATPort: "/dev/ttyUSB2", IMSMode: store.IMSModeHost, IMSEnabled: true}); err != nil {
		t.Fatal(err)
	}
	snapshot := &device.Snapshot{SIMReady: true, ICCID: "8944100000000000001", IMSI: "234150000000001"}
	enforceDefaultSafeCardPolicy(context.Background(), regionTestLogger(), database, manager, regionTestDeviceID, snapshot)
	enforceCardRegion(context.Background(), regionTestLogger(), database, manager, regionTestDeviceID, &device.Snapshot{SIMReady: true, IMSI: "460001234567890"})
	if release, ok := lockLegacyPhysicalRead(context.Background(), database, manager, regionTestDeviceID); ok {
		release()
		t.Fatal("recovery could enter Host gate")
	}
	if client.calls.Load() != 0 {
		t.Fatal("stale background action contacted Host modem")
	}
}

func TestExplicitIMSModeDoesNotApplyLegacyRFOffCardDefaults(t *testing.T) {
	for _, mode := range []string{store.IMSModeHost, store.IMSModeModule, store.IMSModeVoWiFi} {
		t.Run(mode, func(t *testing.T) {
			client := &fakeModemClient{}
			devices := newRegionTestManager(t, client)
			database := newRegionTestStore(t)
			if err := database.UpsertDevice(context.Background(), store.Device{ID: "configured", Name: "Configured", ATPort: "/dev/ttyUSB2", IMSMode: mode, IMSEnabled: true}); err != nil {
				t.Fatal(err)
			}
			if !explicitIMSOwnsPhysical(context.Background(), database, devices, regionTestDeviceID) {
				t.Fatal("explicit mode not recognized")
			}
			enforceDefaultSafeCardPolicy(context.Background(), regionTestLogger(), database, devices, regionTestDeviceID, &device.Snapshot{SIMReady: true, ICCID: "8944100000000000001", IMSI: "234150000000001"})
			client.assertExhausted(t)
			if _, err := database.CardPolicy(context.Background(), "8944100000000000001"); err != store.ErrNotFound {
				t.Fatalf("created legacy policy: %v", err)
			}
		})
	}
}

func TestApplicationIMSConfigSharesIncomingSMSAndCallDelivery(t *testing.T) {
	database := newRegionTestStore(t)
	config := store.Device{ID: "host-a", Name: "Host A", IMSMode: store.IMSModeHost, ModemIMEI: "359762080000001"}
	if err := database.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	called := false
	settings, err := applicationIMSConfig(context.Background(), config, database, regionTestLogger(), func(context.Context, ims.ReceivedCall) error { called = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = settings.OnIncomingCall(context.Background(), ims.ReceivedCall{DeviceID: config.ID}); err != nil || !called {
		t.Fatal("call callback not shared")
	}
	if err = settings.OnSMS(context.Background(), ims.ReceivedSMS{DeviceID: config.ID, MessageID: "synthetic-only", ICCID: "8944100000000000001", IMSI: "234150000000001", Text: "Synthetic callback", From: "test", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	items, err := database.ListSMSMessages(context.Background(), store.SMSFilter{DeviceID: config.ID})
	if err != nil || len(items) != 1 || items[0].Body != "Synthetic callback" || items[0].Source != "ims" {
		t.Fatal("Host callback did not persist message")
	}
}
