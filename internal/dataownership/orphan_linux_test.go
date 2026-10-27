//go:build linux

package dataownership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"

	"github.com/vishvananda/netlink"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func orphanFixtureHost() device.DataHostReceipt {
	// This metadata fixture is not a kernel cleanup permission. Positive
	// cleanup is covered using a real netdev in the opt-in kernel suite.
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	// FNV-1a, identical public mark/table contract as the Export Proxy.
	var hash uint32 = 2166136261
	for _, b := range []byte("fixture0") {
		hash ^= uint32(b)
		hash *= 16777619
	}
	rule.Mark, rule.Table, rule.Priority, rule.Type = 0x56000000|(hash&0xffffff), 20000+int(hash%10000), 20000+int(hash%10000), 1
	raw, _ := json.Marshal(rule)
	return device.DataHostReceipt{Version: 1, Interface: "fixture0", InterfaceIndex: 999999, Families: []int{4}, Rules: map[int]json.RawMessage{4: raw}, RouteHashes: map[int]string{4: digest("")}, Addresses: []string{"192.0.2.2/24"}, DNS: &device.DataHostFile{Device: 1, Inode: 1, DirectoryDevice: 1, DirectoryInode: 1, UID: uint32(os.Geteuid()), Mode: 0644, SHA256: digest("192.0.2.53\n")}, Ready: true}
}

func orphanFixtureChild(t *testing.T, directory string, ready bool) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestOrphanJournalRealProcessDeathAndOneShotIntent$")
	value := "pending"
	if ready {
		value = "ready"
	}
	child.Env = append(os.Environ(), "VOCAT_ORPHAN_CHILD="+directory, "VOCAT_ORPHAN_READY="+value)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("original owner: %v: %s", err, out)
	}
}

func TestOrphanJournalRealProcessDeathAndOneShotIntent(t *testing.T) {
	ctx := context.Background()
	if directory := os.Getenv("VOCAT_ORPHAN_CHILD"); directory != "" {
		j := fixtureJournal(t, directory)
		candidate := fixtureCandidate()
		candidate.USBPath = "/sys/devices/vocat-nonexistent-fixture"
		original, err := j.Acquire(ctx, candidate, fixtureEquipment, "1234567890123456789", "123456789012345")
		if err != nil {
			t.Fatal(err)
		}
		host := orphanFixtureHost()
		host.Ready = false
		if err := original.(device.DataHostClaim).SaveHost(ctx, host); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("VOCAT_ORPHAN_READY") == "ready" {
			host.Ready = true
			if err := original.(device.DataHostClaim).SaveHost(ctx, host); err != nil {
				t.Fatal(err)
			}
		}
		os.Exit(0) // kernel releases the flock; FULL-sync claim must survive
	}
	directory := t.TempDir()
	orphanFixtureChild(t, directory, true)
	j := fixtureJournal(t, directory)
	original, err := j.OpenOrphan(ctx, fixtureCandidate().ID)
	if err != nil || original == nil {
		t.Fatal("dead acknowledged owner not recoverable", err)
	}
	candidate := fixtureCandidate()
	candidate.USBPath = "/sys/devices/vocat-nonexistent-fixture"
	if _, err := j.OpenOrphan(ctx, candidate.ID); err == nil {
		t.Fatal("another recovery owner took the equipment lock")
	}
	if ok, err := original.BeginRecovery(ctx, candidate, fixtureEquipment, "1234567890123456789", "123456789012345"); err != nil || !ok {
		t.Fatal("original identity refused", err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if ok, err := original.BeginRecovery(ctx, candidate, fixtureEquipment, "1234567890123456789", "123456789012345"); ok || err == nil {
		t.Fatal("closed recovery guard remained a capability", err)
	}
	// Recovery-process cancellation/death does not permit a second reset.
	replacement, err := j.OpenOrphan(ctx, candidate.ID)
	if err != nil || replacement == nil || !replacement.RecoveryAttempted() {
		t.Fatal("restart intent lost", err)
	}
	defer replacement.Close()
	if ok, err := replacement.BeginRecovery(ctx, candidate, fixtureEquipment, "1234567890123456789", "123456789012345"); err != nil || ok {
		t.Fatal("reset replayed", err)
	}
	if gone, err := replacement.OriginalGone(); err != nil || !gone {
		t.Fatal("missing original sysfs path not observed", err)
	}
	if pending, _ := j.Pending(ctx, candidate); !pending {
		t.Fatal("death/reset implicitly cleared journal")
	}
	if replacement.VerifyIdentity(candidate, fixtureEquipment, "1234567890123456790", "123456789012345") == nil {
		t.Fatal("replacement SIM admitted")
	}
}

func TestOrphanJournalRejectsLiveUnknownAndChangedNamespace(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"live", "unready", "legacy", "boot", "PID namespace", "user namespace", "net namespace", "start", "host receipt", "unknown field", "foreign IMS"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			orphanFixtureChild(t, directory, scenario != "unready")
			j := fixtureJournal(t, directory)
			key := store.DataBearerClaimPrefix + digest("imei:"+fixtureEquipment)
			setting, err := j.Database.AppSetting(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			var r record
			if json.Unmarshal(setting.Value, &r) != nil {
				t.Fatal("bad fixture")
			}
			switch scenario {
			case "live":
				r.PID = os.Getpid()
				if err := captureProcessIdentity(&r); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				r.Version = 1
			case "boot":
				r.BootID = "different boot"
			case "PID namespace":
				r.PIDNamespace = "pid:[unknown]"
			case "user namespace":
				r.UserNamespace = "user:[unknown]"
			case "net namespace":
				r.NetNamespace = "net:[unknown]"
			case "start":
				r.StartTicks = "invalid"
			case "host receipt":
				r.Host.Rules = nil
			case "foreign IMS":
				if ok, err := j.Database.CompareHostIMSClaim(ctx, "host.ims.claim.private.v1."+r.EquipmentKey, nil, json.RawMessage(`{"pending":true}`)); !ok || err != nil {
					t.Fatal(err)
				}
			}
			next, _ := json.Marshal(r)
			if scenario == "unknown field" {
				var fields map[string]any
				_ = json.Unmarshal(next, &fields)
				fields["UnrecognisedCapability"] = true
				next, _ = json.Marshal(fields)
			}
			if ok, err := j.Database.CompareDataBearerClaim(ctx, key, setting.Value, next); !ok || err != nil {
				t.Fatal(err)
			}
			if orphan, err := j.OpenOrphan(ctx, fixtureCandidate().ID); orphan != nil || !errors.Is(err, device.ErrDataCleanupPending) {
				if orphan != nil {
					_ = orphan.Close()
				}
				t.Fatal("unsafe orphan admitted", err)
			}
			if pending, _ := j.Pending(ctx, modem.Candidate{ID: fixtureCandidate().ID}); !pending {
				t.Fatal("refusal cleared the fence")
			}
		})
	}
}

func TestProcessDeathProofRejectsReusedNamespaceAndAcceptsDifferentStart(t *testing.T) {
	r := record{PID: os.Getpid(), UID: os.Geteuid()}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	r.BootID = string(bytes.TrimSpace(boot))
	if err := captureProcessIdentity(&r); err != nil {
		t.Fatal(err)
	}
	if originalProcessDead(r) == nil {
		t.Fatal("live process treated as dead")
	}
	r.StartTicks = "1"
	if originalProcessDead(r) != nil {
		t.Fatal("same-namespace reused PID not recognised")
	}
	r.NetNamespace = "net:[0]"
	if originalProcessDead(r) == nil {
		t.Fatal("cross namespace PID used as death proof")
	}
}

func TestOrphanJournalKernelAcknowledgesOnlyAbsentOriginalInterface(t *testing.T) {
	if os.Getenv("VOCAT_DATA_KERNEL_TESTS") != "1" {
		t.Skip("opt-in private network/mount namespaces required")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil || self == init {
			t.Fatal("refusing original host namespace", kind, err)
		}
	}
	ctx := context.Background()
	for _, present := range []bool{true, false} {
		directory := t.TempDir()
		orphanFixtureChild(t, directory, true)
		j := fixtureJournal(t, directory)
		if present {
			key := store.DataBearerClaimPrefix + digest("imei:"+fixtureEquipment)
			setting, err := j.Database.AppSetting(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			var r record
			if err := json.Unmarshal(setting.Value, &r); err != nil {
				t.Fatal(err)
			}
			loopback, err := net.InterfaceByName("lo")
			if err != nil {
				t.Fatal(err)
			}
			r.Host.InterfaceIndex = loopback.Index
			next, _ := json.Marshal(r)
			if ok, err := j.Database.CompareDataBearerClaim(ctx, key, setting.Value, next); !ok || err != nil {
				t.Fatal(err)
			}
		}
		orphan, err := j.OpenOrphan(ctx, fixtureCandidate().ID)
		if err != nil || orphan == nil {
			t.Fatal(err)
		}
		err = orphan.Complete(ctx)
		if present {
			if err == nil {
				t.Fatal("existing kernel interface acknowledged as gone")
			}
			if pending, _ := j.Pending(ctx, fixtureCandidate()); !pending {
				t.Fatal("failed cleanup cleared the journal")
			}
			_ = orphan.Close()
		} else {
			if err != nil {
				t.Fatal("already-clean host receipt not acknowledged", err)
			}
			if pending, err := j.Pending(ctx, fixtureCandidate()); pending || err != nil {
				t.Fatal("exact original journal remained after confirmed cleanup", err)
			}
			successor, err := acquireFixture(j)
			if err != nil {
				t.Fatal("fresh successor not admitted", err)
			}
			if err := successor.Release(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
}
