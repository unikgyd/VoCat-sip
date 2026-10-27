//go:build linux

package device

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type receiptClaim struct {
	saved     []DataHostReceipt
	failReady bool
}

func (*receiptClaim) Release(context.Context) error { return nil }
func (c *receiptClaim) SaveHost(_ context.Context, h DataHostReceipt) error {
	if h.Ready && c.failReady {
		return errors.New("durable receipt unavailable")
	}
	c.saved = append(c.saved, cloneDataHostReceipt(h))
	return nil
}

func TestDataHostReceiptKernelActiveGoneAndReplacement(t *testing.T) {
	for _, family := range []uint8{4, 6, qmiDataDualFamily} {
		t.Run(fmt.Sprint(family), func(t *testing.T) {
			ip, state, candidate := dataKernelFixture(t, family)
			claim := &receiptClaim{}
			state.dataOwner.claim, state.dataOwner.handleKnown = claim, true
			version := "IP"
			if family == 6 {
				version = "IPV6"
			} else if family == qmiDataDualFamily {
				version = "IPV4V6"
			}
			if err := beginDataHostReceipt(context.Background(), state.dataOwner, version); err != nil {
				t.Fatal("empty slot refused", err)
			}
			if result, err := configureQMIDataHost(context.Background(), state, candidate, ip, "internet", version, "receipt fixture"); err != nil || !result.Enabled {
				t.Fatal(result, err)
			}
			if len(claim.saved) != 2 || claim.saved[0].Ready || !claim.saved[1].Ready {
				t.Fatal("acknowledged configuration lacks pending-to-ready receipt")
			}
			receipt := claim.saved[1]
			if err := validateCurrentDataHost(receipt); err != nil {
				t.Fatal("active host differs from committed receipt", err)
			}
			if err := CleanupOrphanDataHost(context.Background(), receipt); err == nil {
				t.Fatal("still-present original interface was cleaned")
			}
			// An unchanged reconfiguration must also become unready before any
			// writes, then produce a fresh inode receipt only after success.
			if err := beginDataHostReceipt(context.Background(), state.dataOwner, version); err != nil {
				t.Fatal(err)
			}
			if _, err := configureQMIDataHost(context.Background(), state, candidate, ip, "internet", version, "repeat"); err != nil {
				t.Fatal(err)
			}
			receipt = *state.dataOwner.hostReceipt
			dataKernelCommand(t, ip, "link", "del", candidate.NetworkInterface)
			dataKernelCommand(t, ip, "link", "add", candidate.NetworkInterface, "type", "dummy")
			dataKernelCommand(t, ip, "link", "set", "dev", candidate.NetworkInterface, "up")
			dataKernelCommand(t, ip, "-4", "addr", "add", "198.51.100.2/24", "dev", candidate.NetworkInterface)
			before := dataKernelCommand(t, ip, "-j", "addr", "show", "dev", candidate.NetworkInterface)
			if err := CleanupOrphanDataHost(context.Background(), receipt); err != nil {
				t.Fatal("gone original was not cleaned", err)
			}
			if before != dataKernelCommand(t, ip, "-j", "addr", "show", "dev", candidate.NetworkInterface) {
				t.Fatal("replacement netdev was changed")
			}
			if err := CleanupOrphanDataHost(context.Background(), receipt); err != nil {
				t.Fatal("ack retry was not idempotent", err)
			}
		})
	}
}

func TestDataHostReceiptKernelRefusesForeignObjectsBeforeDeletion(t *testing.T) {
	for _, scenario := range []string{"rule", "rule action", "route", "DNS inode", "DNS bytes", "DNS symlink", "DNS directory"} {
		t.Run(scenario, func(t *testing.T) {
			ip, state, candidate := dataKernelFixture(t, 4)
			state.dataOwner.claim, state.dataOwner.handleKnown = &receiptClaim{}, true
			if err := beginDataHostReceipt(context.Background(), state.dataOwner, "IP"); err != nil {
				t.Fatal(err)
			}
			if _, err := configureQMIDataHost(context.Background(), state, candidate, ip, "internet", "IP", "fixture"); err != nil {
				t.Fatal(err)
			}
			receipt := *state.dataOwner.hostReceipt
			path := exportProxyDNSPath(candidate.NetworkInterface)
			t.Cleanup(func() { _ = os.Remove(path) })
			dataKernelCommand(t, ip, "link", "del", candidate.NetworkInterface)
			mark, table, priority := exportProxyRouteIdentity(candidate.NetworkInterface)
			switch scenario {
			case "rule":
				dataKernelCommand(t, ip, "rule", "add", "priority", fmt.Sprint(priority), "from", "192.0.2.0/24", "lookup", fmt.Sprint(table))
			case "rule action":
				dataKernelCommand(t, ip, "rule", "del", "priority", fmt.Sprint(priority))
				dataKernelCommand(t, ip, "rule", "add", "priority", fmt.Sprint(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", fmt.Sprint(table), "blackhole")
			case "route":
				dataKernelCommand(t, ip, "link", "add", "vcforeign", "type", "dummy")
				dataKernelCommand(t, ip, "link", "set", "dev", "vcforeign", "up")
				t.Cleanup(func() { dataKernelCommand(t, ip, "link", "del", "vcforeign") })
				dataKernelCommand(t, ip, "route", "add", "table", fmt.Sprint(table), "203.0.113.0/24", "dev", "vcforeign")
			case "DNS inode":
				raw, _ := os.ReadFile(path)
				if err := os.WriteFile(path+".new", raw, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			case "DNS bytes":
				if err := os.WriteFile(path, []byte("203.0.113.53\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "DNS symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/resolv.conf", path); err != nil {
					t.Fatal(err)
				}
			case "DNS directory":
				directory := filepath.Dir(path)
				old := directory + ".receipt-test"
				if err := os.Rename(directory, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(old, filepath.Base(path)), path); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = os.Rename(path, filepath.Join(old, filepath.Base(path)))
					_ = os.Remove(directory)
					_ = os.Rename(old, directory)
				})
			}
			rules := dataKernelCommand(t, ip, "-j", "rule", "show")
			dnsBefore, dnsErr := os.Lstat(path)
			if err := CleanupOrphanDataHost(context.Background(), receipt); err == nil {
				t.Fatal("foreign object accepted")
			}
			if rules != dataKernelCommand(t, ip, "-j", "rule", "show") {
				t.Fatal("rule deleted before all ownership checks")
			}
			dnsAfter, afterErr := os.Lstat(path)
			if dnsErr != nil || afterErr != nil || !os.SameFile(dnsBefore, dnsAfter) {
				t.Fatal("foreign DNS removed")
			}
			// Restore only fixture-created rules so subsequent table aliases do
			// not obscure the remaining independent negative cases.
			dataKernelCommand(t, ip, "rule", "del", "priority", fmt.Sprint(priority))
			if scenario == "rule" {
				dataKernelCommand(t, ip, "rule", "del", "priority", fmt.Sprint(priority))
			}
		})
	}
}

func TestDataHostReceiptKernelReservationAndCommitFailures(t *testing.T) {
	ip, state, candidate := dataKernelFixture(t, 4)
	claim := &receiptClaim{failReady: true}
	state.dataOwner.claim, state.dataOwner.handleKnown = claim, true
	dataKernelCommand(t, ip, "-4", "addr", "add", "198.51.100.9/24", "dev", candidate.NetworkInterface)
	before := dataKernelCommand(t, ip, "-j", "addr", "show", "dev", candidate.NetworkInterface)
	if err := beginDataHostReceipt(context.Background(), state.dataOwner, "IP"); err == nil || len(claim.saved) != 0 {
		t.Fatal("foreign global address reserved")
	}
	if before != dataKernelCommand(t, ip, "-j", "addr", "show", "dev", candidate.NetworkInterface) {
		t.Fatal("failed reserve changed host")
	}
	dataKernelCommand(t, ip, "-4", "addr", "del", "198.51.100.9/24", "dev", candidate.NetworkInterface)
	if err := beginDataHostReceipt(context.Background(), state.dataOwner, "IP"); err != nil {
		t.Fatal(err)
	}
	if result, err := configureQMIDataHost(context.Background(), state, candidate, ip, "internet", "IP", "failed commit"); err == nil || result.Enabled {
		t.Fatal("configuration acknowledged without durable receipt", err)
	}
	if state.dataOwner.hostReceipt.Ready || len(claim.saved) != 1 {
		t.Fatal("uncertain receipt became recovery permission")
	}
	if err := clearOwnedQMIHost(context.Background(), state.dataOwner, true); err != nil {
		t.Fatal(err)
	}
	if addresses, err := dataReceiptAddresses(candidate.NetworkInterface); err != nil || len(addresses) != 0 {
		t.Fatal("original live owner lost cleanup capability", addresses, err)
	}
	info, _ := net.InterfaceByName(candidate.NetworkInterface)
	if info == nil {
		t.Fatal("test interface unexpectedly removed")
	}
	if !reflect.DeepEqual(claim.saved[0].Families, []int{4}) {
		t.Fatal("wrong reserved family")
	}
}
