package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

const taskStorageAID = "A0000005591010FFFFFFFF8900000100"
const taskStorageEID = "89010000000000000000000000000001"

type taskStorageController struct {
	*revertTestController
	entries          []device.EsimInventoryEntry
	err              error
	reads            int
	cold             bool
	probeIMEI        string
	probes           int
	changeGeneration bool
}

func (c *taskStorageController) ESIMInventory(context.Context, string) ([]device.EsimInventoryEntry, error) {
	c.reads++
	return c.entries, c.err
}

func (c *taskStorageController) ExecuteAT(ctx context.Context, id, command string) (modem.Response, error) {
	if command == "AT+CGSN" {
		c.probes++
		if c.changeGeneration {
			c.entry.Candidate.USBGeneration = "replacement-generation"
		}
		return modem.Response{Lines: []string{c.probeIMEI}, Final: "OK"}, nil
	}
	return c.revertTestController.ExecuteAT(ctx, id, command)
}

func (c *taskStorageController) Refresh(ctx context.Context, id string) (device.Snapshot, error) {
	snapshot, err := c.revertTestController.Refresh(ctx, id)
	if c.cold && err == nil {
		c.entry.Snapshot = &snapshot // The real manager publishes a refreshed snapshot.
	}
	return snapshot, err
}

func taskStorageEntry(iccid, eid, aid string) device.EsimInventoryEntry {
	return device.EsimInventoryEntry{
		Info: device.EsimInfo{EID: eid, AID: aid, Profiles: []device.EsimProfile{{ICCID: iccid, AID: "A0000005591010FFFFFFFF8900000A01"}}},
		Chip: device.EsimChipInfo{EID: eid, AID: aid},
	}
}

func TestAutomaticTaskProfileStorageRequiresOriginalMembershipAndEID(t *testing.T) {
	for _, scenario := range []string{"management aid", "other storage first", "changed eid", "missing profile", "duplicate storage", "missing eid", "missing aid", "conflicting eid", "conflicting aid", "inventory error"} {
		t.Run(scenario, func(t *testing.T) {
			entry := taskStorageEntry("8901000000000000000", taskStorageEID, taskStorageAID)
			c := &taskStorageController{entries: []device.EsimInventoryEntry{entry}}
			wantOK := scenario == "management aid" || scenario == "other storage first"
			switch scenario {
			case "other storage first":
				c.entries = append([]device.EsimInventoryEntry{taskStorageEntry("8901000000000000002", "89010000000000000000000000000002", "A0000005591010FFFFFFFF8900000177")}, entry)
			case "changed eid":
				c.entries[0] = taskStorageEntry("8901000000000000000", "89010000000000000000000000000002", taskStorageAID)
			case "missing profile":
				c.entries[0].Info.Profiles = nil
			case "duplicate storage":
				c.entries = append(c.entries, entry)
			case "missing eid":
				c.entries[0].Info.EID, c.entries[0].Chip.EID = "", ""
			case "missing aid":
				c.entries[0].Info.AID, c.entries[0].Chip.AID = "", ""
			case "conflicting eid":
				c.entries[0].Chip.EID = "89010000000000000000000000000002"
			case "conflicting aid":
				c.entries[0].Chip.AID = "A0000005591010FFFFFFFF8900000177"
			case "inventory error":
				c.err = errors.New("inventory unavailable")
			}
			s := &Server{devices: c}
			got, err := s.taskProfileStorage(context.Background(), "physical", "8901000000000000000", taskStorageEID)
			if (err == nil) != wantOK || wantOK && (got.AID != taskStorageAID || got.EID != taskStorageEID) {
				t.Fatalf("storage=%+v error=%v", got, err)
			}
		})
	}
}

func TestAutomaticTaskOldProfileAIDJournalUsesVerifiedManagementStorage(t *testing.T) {
	for _, scenario := range []string{"old profile aid", "changed eid", "missing original profile", "ambiguous storage", "cold bootstrap", "cold wrong imei", "cold changed generation", "cold missing binding"} {
		t.Run(scenario, func(t *testing.T) {
			s := blockedRegionServer(t, "310260123456789")
			s.vowifi = nil
			c := &taskStorageController{revertTestController: &revertTestController{
				registration: 1, currentICCID: "8901000000000000000",
				fakeDeviceController: fakeDeviceController{entry: device.Device{ID: "dev1", Discovered: true, Snapshot: automaticTaskTestSnapshot("8901000000000000000")}},
			}, entries: []device.EsimInventoryEntry{taskStorageEntry("8901000000000000000", taskStorageEID, taskStorageAID)}}
			s.devices = c
			c.entry.Candidate.USBPath = "/sys/synthetic-original-modem"
			c.entry.Candidate.USBGeneration = "original-generation"
			ctx := context.Background()
			config := store.Device{ID: "dev1", Name: "Modem"}
			if err := s.store.UpsertDevice(ctx, config); err != nil {
				t.Fatal(err)
			}
			config, err := s.store.Device(ctx, config.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, iccid := range []string{"8901000000000000000", "8901000000000000001"} {
				if err := s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: iccid, Source: "user"}); err != nil {
					t.Fatal(err)
				}
			}
			journal, err := s.createAutomaticTaskJournal(ctx, store.AutomaticTask{DeviceID: "dev1", ProfileICCID: "8901000000000000001", Environment: "cellular", TaskType: "cellular_attach", RevertProfile: true}, config)
			if err != nil {
				t.Fatal(err)
			}
			// Reproduce the surviving version-1 journal: the stored AID is an
			// ISD-P profile identifier, while its original EID is still valid.
			journal.payload.InitialAID = "A0000005591010FFFFFFFF8900000A01"
			journal.payload.EID, journal.payload.SwitchAttempted = taskStorageEID, true
			if err := journal.save(ctx, "switching"); err != nil {
				t.Fatal(err)
			}
			c.currentICCID = "8901000000000000001"
			c.entry.Snapshot = automaticTaskTestSnapshot(c.currentICCID)
			if strings.HasPrefix(scenario, "cold ") {
				c.cold, c.probeIMEI = true, c.entry.Snapshot.IMEI
				c.entry.Snapshot = nil
				switch scenario {
				case "cold wrong imei":
					c.probeIMEI = "123456789012341"
				case "cold changed generation":
					c.changeGeneration = true
				case "cold missing binding":
					journal.payload.USBPath = ""
					if err := journal.save(ctx, "switching"); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch scenario {
			case "changed eid":
				c.entries[0] = taskStorageEntry("8901000000000000000", "89010000000000000000000000000002", taskStorageAID)
			case "missing original profile":
				c.entries[0].Info.Profiles = nil
			case "ambiguous storage":
				c.entries = append(c.entries, c.entries[0])
			}
			s.recoverAutomaticTaskEnvironment(ctx, journal.record)
			wantIdentityRefusal := scenario == "cold wrong imei" || scenario == "cold changed generation" || scenario == "cold missing binding"
			if !wantIdentityRefusal && c.reads == 0 {
				t.Fatal("recovery did not reach the live storage identity check")
			}
			if wantIdentityRefusal && c.reads != 0 {
				t.Fatal("unverified hardware reached eUICC inventory")
			}
			if strings.HasPrefix(scenario, "cold ") {
				wantProbes := 1
				if scenario == "cold missing binding" {
					wantProbes = 0
				}
				if c.probes != wantProbes {
					t.Fatalf("identity probes=%d want %d", c.probes, wantProbes)
				}
			}
			if scenario != "old profile aid" && scenario != "cold bootstrap" {
				if err := s.store.RequireAutomaticTaskRecovered(ctx, config.ID); err == nil || len(c.switches) != 0 {
					t.Fatal("unverified storage erased the fence or switched a profile", err, c.switches)
				}
				for _, action := range c.actions {
					if strings.HasPrefix(action, "flight:") {
						t.Fatal("unverified storage changed RF", c.actions)
					}
				}
				return
			}
			if len(c.switches) != 1 || c.switches[0] != "8901000000000000000:"+taskStorageAID || c.currentICCID != "8901000000000000000" {
				t.Fatal("old journal did not restore through the original management application", c.switches)
			}
			if err := s.store.RequireAutomaticTaskRecovered(ctx, config.ID); err != nil {
				t.Fatal("verified restoration retained the fence", err)
			}
		})
	}
}
