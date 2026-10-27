package server

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

type revertTestController struct {
	fakeDeviceController
	actions      []string
	switches     []string
	registration int
	currentICCID string
}

func (c *revertTestController) SetNetwork(_ context.Context, _ string, request device.NetworkRequest) (device.NetworkResult, error) {
	c.actions = append(c.actions, fmt.Sprintf("data:%t", request.Enabled))
	return device.NetworkResult{Enabled: request.Enabled}, nil
}

func (c *revertTestController) StopOwnedNetwork(ctx context.Context, id string) error {
	_, err := c.SetNetwork(ctx, id, device.NetworkRequest{Enabled: false})
	return err
}

func (c *revertTestController) SetFlight(_ context.Context, _ string, enabled bool) (device.FlightResult, error) {
	c.actions = append(c.actions, fmt.Sprintf("flight:%t", enabled))
	return device.FlightResult{}, nil
}

func (c *revertTestController) SetOperatorSelection(_ context.Context, _ string, automatic bool, _ string, _ *int) (device.OperatorSelection, error) {
	c.actions = append(c.actions, fmt.Sprintf("automatic:%t", automatic))
	return device.OperatorSelection{}, nil
}

func (c *revertTestController) ReRegisterOperator(context.Context, string) (device.OperatorSelection, error) {
	c.actions = append(c.actions, "register")
	return device.OperatorSelection{}, nil
}

func (c *revertTestController) Refresh(context.Context, string) (device.Snapshot, error) {
	c.actions = append(c.actions, "refresh")
	snapshot := *automaticTaskTestSnapshot(c.currentICCID)
	snapshot.RegistrationStatus = c.registration
	return snapshot, nil
}

func (c *revertTestController) ExecuteAT(ctx context.Context, id, command string) (modem.Response, error) {
	if command == "AT+CLCC" {
		return modem.Response{Final: "OK"}, nil
	}
	return c.fakeDeviceController.ExecuteAT(ctx, id, command)
}

func (c *revertTestController) SetFlightForSIM(ctx context.Context, id, iccid, imsi string, enabled bool) (device.FlightResult, error) {
	if iccid != c.currentICCID || imsi != "310260123456789" {
		return device.FlightResult{}, device.ErrSIMObservation
	}
	return c.SetFlight(ctx, id, enabled)
}

func (c *revertTestController) ESIMChipInfo(context.Context, string) (*device.EsimChipInfo, error) {
	return &device.EsimChipInfo{EID: "89010000000000000000000000000001"}, nil
}

func (c *revertTestController) ESIMListProfiles(_ context.Context, _ string) (device.EsimInfo, error) {
	return device.EsimInfo{
		EID: "89010000000000000000000000000001",
		AID: "A0000005591010FFFFFFFF8900000100",
		Profiles: []device.EsimProfile{
			{ICCID: "8901000000000000000", AID: "A0000005591010FFFFFFFF8900000A01"},
			{ICCID: "8901000000000000001", AID: "A0000005591010FFFFFFFF8900000A02"},
		},
	}, nil
}

func (c *revertTestController) ESIMInventory(ctx context.Context, id string) ([]device.EsimInventoryEntry, error) {
	info, err := c.ESIMListProfiles(ctx, id)
	return []device.EsimInventoryEntry{{Info: info, Chip: device.EsimChipInfo{EID: info.EID, AID: info.AID}}}, err
}

func (c *revertTestController) ESIMSwitchProfile(_ context.Context, _ string, iccid, aid string) error {
	c.switches = append(c.switches, fmt.Sprintf("%s:%s", iccid, aid))
	c.currentICCID = iccid
	c.entry.Snapshot = automaticTaskTestSnapshot(iccid)
	return nil
}

func TestAutomaticTaskRevertToInitialProfile(t *testing.T) {
	s := blockedRegionServer(t, "310260123456789")
	s.vowifi = &automaticTaskTestWiFi{fakeVoWiFiController: &fakeVoWiFiController{}}

	ctrl := &revertTestController{
		registration: 1,
		currentICCID: "8901000000000000000",
		fakeDeviceController: fakeDeviceController{
			entry: device.Device{
				ID:         "dev1",
				Discovered: true,
				Snapshot:   automaticTaskTestSnapshot("8901000000000000000"),
			},
		},
	}
	s.devices = ctrl

	ctx := context.Background()
	// Setup initial device and policies
	if err := s.store.UpsertDevice(ctx, store.Device{
		ID:             "dev1",
		Name:           "Modem",
		NetworkEnabled: false,
		VoWiFiEnabled:  true,
	}); err != nil {
		t.Fatal(err)
	}

	initialPolicy := store.CardPolicy{
		ICCID:           "8901000000000000000",
		AirplaneEnabled: true,
		VoWiFiEnabled:   true,
		NetworkEnabled:  false,
		Source:          "user",
	}
	if err := s.store.UpsertCardPolicy(ctx, initialPolicy); err != nil {
		t.Fatal(err)
	}

	taskPolicy := store.CardPolicy{
		ICCID:           "8901000000000000001",
		AirplaneEnabled: false,
		NetworkEnabled:  true,
		Source:          "user",
	}
	if err := s.store.UpsertCardPolicy(ctx, taskPolicy); err != nil {
		t.Fatal(err)
	}

	task := store.AutomaticTask{
		DeviceID:      "dev1",
		ProfileICCID:  "8901000000000000001",
		ProfileAID:    "A0000005591010FFFFFFFF8900000200",
		TaskType:      "cellular_attach",
		Environment:   "cellular",
		RevertProfile: true,
		Payload:       []byte(`{}`),
	}

	output, err := s.executeAutomaticTask(ctx, task, func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask failed: %v", err)
	}
	if output != "已注册蜂窝网络，未启用数据连接" {
		t.Fatalf("unexpected output: %q", output)
	}

	// Verify profile switches: first to 8901000000000000001, then reverted back to 8901000000000000000
	wantSwitches := []string{
		"8901000000000000001:A0000005591010FFFFFFFF8900000200",
		"8901000000000000000:A0000005591010FFFFFFFF8900000100",
	}
	if !reflect.DeepEqual(ctrl.switches, wantSwitches) {
		t.Fatalf("switches=%v, want %v", ctrl.switches, wantSwitches)
	}
	if ctrl.currentICCID != "8901000000000000000" {
		t.Fatalf("final currentICCID=%s, want 8901000000000000000", ctrl.currentICCID)
	}

	// Verify initial policy was restored
	storedDevice, err := s.store.Device(ctx, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if storedDevice.VoWiFiEnabled != initialPolicy.VoWiFiEnabled || storedDevice.NetworkEnabled != false {
		t.Fatalf("device setting not restored: %+v", storedDevice)
	}

	storedInitialPolicy, err := s.store.CardPolicy(ctx, "8901000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if storedInitialPolicy.VoWiFiEnabled != initialPolicy.VoWiFiEnabled || storedInitialPolicy.AirplaneEnabled != initialPolicy.AirplaneEnabled {
		t.Fatalf("8901000000000000000 policy not restored: %+v", storedInitialPolicy)
	}

	// Verify 8901000000000000001's own saved policy was also preserved in store
	storedTaskPolicy, err := s.store.CardPolicy(ctx, "8901000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if storedTaskPolicy.NetworkEnabled != taskPolicy.NetworkEnabled {
		t.Fatalf("8901000000000000001 policy altered: %+v", storedTaskPolicy)
	}
}

func TestAutomaticTaskDoNotRevertWhenDisabled(t *testing.T) {
	s := blockedRegionServer(t, "310260123456789")
	s.vowifi = nil

	ctrl := &revertTestController{
		registration: 1,
		currentICCID: "8901000000000000000",
		fakeDeviceController: fakeDeviceController{
			entry: device.Device{
				ID:         "dev1",
				Discovered: true,
				Snapshot:   automaticTaskTestSnapshot("8901000000000000000"),
			},
		},
	}
	s.devices = ctrl

	ctx := context.Background()
	_ = s.store.UpsertDevice(ctx, store.Device{ID: "dev1", Name: "Modem"})
	_ = s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: "8901000000000000000", Source: "user"})
	_ = s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: "8901000000000000001", NetworkEnabled: true, Source: "user"})

	task := store.AutomaticTask{
		DeviceID:      "dev1",
		ProfileICCID:  "8901000000000000001",
		ProfileAID:    "A0000005591010FFFFFFFF8900000200",
		TaskType:      "cellular_attach",
		Environment:   "cellular",
		RevertProfile: false, // Do NOT revert
		Payload:       []byte(`{}`),
	}

	output, err := s.executeAutomaticTask(ctx, task, func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask failed: %v", err)
	}
	if output != "已注册蜂窝网络，未启用数据连接" {
		t.Fatalf("unexpected output: %q", output)
	}

	// Only switched once to 8901000000000000001, did not revert
	wantSwitches := []string{"8901000000000000001:A0000005591010FFFFFFFF8900000200"}
	if !reflect.DeepEqual(ctrl.switches, wantSwitches) {
		t.Fatalf("switches=%v, want %v", ctrl.switches, wantSwitches)
	}
	if ctrl.currentICCID != "8901000000000000001" {
		t.Fatalf("final currentICCID=%s, want 8901000000000000001", ctrl.currentICCID)
	}
}

func TestAutomaticTaskRevertsEvenOnTaskFailure(t *testing.T) {
	s := blockedRegionServer(t, "310260123456789")
	s.vowifi = nil

	ctrl := &revertTestController{
		registration: 3, // Registration denied -> task failure
		currentICCID: "8901000000000000000",
		fakeDeviceController: fakeDeviceController{
			entry: device.Device{
				ID:         "dev1",
				Discovered: true,
				Snapshot:   automaticTaskTestSnapshot("8901000000000000000"),
			},
		},
	}
	s.devices = ctrl

	ctx := context.Background()
	_ = s.store.UpsertDevice(ctx, store.Device{ID: "dev1", Name: "Modem"})
	_ = s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: "8901000000000000000", Source: "user"})
	_ = s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: "8901000000000000001", Source: "user"})

	task := store.AutomaticTask{
		DeviceID:      "dev1",
		ProfileICCID:  "8901000000000000001",
		ProfileAID:    "A0000005591010FFFFFFFF8900000200",
		TaskType:      "cellular_attach",
		Environment:   "cellular",
		RevertProfile: true,
		Payload:       []byte(`{}`),
	}

	_, err := s.executeAutomaticTask(ctx, task, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected registration denied error, got %v", err)
	}

	// Must have reverted back to 8901000000000000000 even though task failed
	wantSwitches := []string{
		"8901000000000000001:A0000005591010FFFFFFFF8900000200",
		"8901000000000000000:A0000005591010FFFFFFFF8900000100",
	}
	if !reflect.DeepEqual(ctrl.switches, wantSwitches) {
		t.Fatalf("switches=%v, want %v", ctrl.switches, wantSwitches)
	}
	if ctrl.currentICCID != "8901000000000000000" {
		t.Fatalf("final currentICCID=%s, want 8901000000000000000", ctrl.currentICCID)
	}
}

func TestAutomaticTaskSameProfileDoesNotSwitch(t *testing.T) {
	s := blockedRegionServer(t, "310260123456789")
	s.vowifi = nil

	ctrl := &revertTestController{
		registration: 1,
		currentICCID: "8901000000000000003",
		fakeDeviceController: fakeDeviceController{
			entry: device.Device{
				ID:         "dev1",
				Discovered: true,
				Snapshot:   automaticTaskTestSnapshot("8901000000000000003"),
			},
		},
	}
	s.devices = ctrl

	ctx := context.Background()
	_ = s.store.UpsertDevice(ctx, store.Device{ID: "dev1", Name: "Modem"})
	_ = s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: "8901000000000000003", Source: "user"})

	task := store.AutomaticTask{
		DeviceID:      "dev1",
		ProfileICCID:  "8901000000000000003",
		TaskType:      "cellular_attach",
		Environment:   "cellular",
		RevertProfile: true,
		Payload:       []byte(`{}`),
	}

	_, err := s.executeAutomaticTask(ctx, task, func(string) {})
	if err != nil {
		t.Fatalf("executeAutomaticTask failed: %v", err)
	}

	// No switches because profile was already active
	if len(ctrl.switches) != 0 {
		t.Fatalf("expected 0 switches, got %v", ctrl.switches)
	}
}
