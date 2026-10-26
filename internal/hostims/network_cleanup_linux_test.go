//go:build linux

package hostims

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestKernelCleanupIdentitySurvivesDownRenameAndMTUChange(t *testing.T) {
	requireSecurityTCNamespace(t)
	_, driver, _, _ := networkFixture(t, false)
	if alive, err := driver.cleanupIdentity(context.Background()); err != nil || !alive {
		t.Fatal("fresh identity", alive, err)
	}
	link, err := netlink.LinkByIndex(driver.index)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetDown(link); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMTU(link, 1280); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetName(link, "ren"+strings.TrimPrefix(driver.name, "ims")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(driver.anchor.path); !os.IsNotExist(err) {
		t.Fatal("fixture did not invalidate the original name path", err)
	}
	if alive, err := driver.cleanupIdentity(context.Background()); err != nil || !alive {
		t.Fatal("cleanup identity incorrectly depends on link UP/MTU/original name", alive, err)
	}
	if err := driver.verify(context.Background()); err == nil {
		t.Fatal("cleanup identity accidentally authorized new bearer I/O")
	}
}

func TestKernelCleanupIdentityProvesDeletionWithoutTouchingReusedIndex(t *testing.T) {
	requireSecurityTCNamespace(t)
	_, driver, _, _ := networkFixture(t, false)
	link, err := netlink.LinkByIndex(driver.index)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(link); err != nil {
		t.Fatal(err)
	}
	var held unix.Stat_t
	if unix.Fstat(int(driver.anchor.file.Fd()), &held) != nil {
		t.Fatal("lost original O_PATH inode")
	}
	index, missing, indexErr := cleanupSysfsIndex(int(driver.anchor.file.Fd()))
	fdTarget, targetErr := os.Readlink("/proc/thread-self/fd/" + strconv.FormatUint(uint64(driver.anchor.file.Fd()), 10))
	t.Logf("original netdev tombstone nlink=%d index=%d missing=%v err=%v fd=%q fdErr=%v", held.Nlink, index, missing, indexErr, fdTarget, targetErr)
	if alive, err := driver.cleanupIdentity(context.Background()); err != nil || alive {
		t.Fatal("original deletion was not provable", alive, err)
	}
	replacement := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: driver.name, Index: driver.index}}
	if err := netlink.LinkAdd(replacement); err != nil {
		t.Fatal("explicit reused-index fixture", err)
	}
	t.Cleanup(func() { netlink.LinkDel(replacement) })
	current, err := netlink.LinkByName(driver.name)
	if err != nil || current.Attrs().Index != driver.index {
		t.Fatal("kernel did not honor explicit reused-index fixture", err)
	}
	foreign := &netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: driver.index,
		Handle: securityTCClsact, Parent: securityTCParent}}
	if err := netlink.QdiscAdd(foreign); err != nil {
		t.Fatal(err)
	}
	if alive, err := driver.cleanupIdentity(context.Background()); err != nil || alive {
		t.Fatal("reused ifindex became an authorized old interface", alive, err)
	}
	qdiscs, err := netlink.QdiscList(current)
	if err != nil || len(qdiscs) != 1 || qdiscs[0].Type() != "clsact" {
		t.Fatal("cleanup identity changed replacement TC objects", err)
	}
}

func TestKernelCleanupIdentityDoesNotGuessDeathFromBadAnchorOrNamespace(t *testing.T) {
	requireSecurityTCNamespace(t)
	_, driver, _, _ := networkFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := driver.cleanupIdentity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled cleanup did I/O", err)
	}
	for _, corrupt := range []string{"cookie", "namespace-key", "non-sysfs-anchor", "index", "closed-anchor"} {
		t.Run(corrupt, func(t *testing.T) {
			oldCookie, oldKey, oldAnchor, oldIndex := driver.namespaceCookie, driver.namespaceKey, driver.anchor, driver.index
			defer func() {
				driver.namespaceCookie, driver.namespaceKey, driver.anchor, driver.index = oldCookie, oldKey, oldAnchor, oldIndex
			}()
			switch corrupt {
			case "cookie":
				driver.namespaceCookie++
			case "namespace-key":
				driver.namespaceKey = strings.Repeat("0", 64)
			case "non-sysfs-anchor":
				fake, err := pinEndpointNode(t.TempDir(), true)
				if err != nil {
					t.Fatal(err)
				}
				defer fake.file.Close()
				driver.anchor = fake
			case "index":
				driver.index++
			case "closed-anchor":
				fd, err := unix.FcntlInt(driver.anchor.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
				if err != nil {
					t.Fatal(err)
				}
				closed := os.NewFile(uintptr(fd), "closed-original-anchor")
				closed.Close()
				driver.anchor.file = closed
			}
			if alive, err := driver.cleanupIdentity(context.Background()); err == nil || alive {
				t.Fatal("unproven identity was mistaken for removed netdev", alive, err)
			}
		})
	}
	if alive, err := driver.cleanupIdentity(context.Background()); err != nil || !alive {
		t.Fatal("read-only failure checks damaged the live identity", alive, err)
	}
}

func TestKernelCleanupIdentityRejectsClosedDriver(t *testing.T) {
	requireSecurityTCNamespace(t)
	_, driver, _, _ := networkFixture(t, false)
	driver.close()
	if alive, err := driver.cleanupIdentity(context.Background()); err == nil || alive {
		t.Fatal("closed cleanup driver guessed missing or reopened a netlink handle", alive, err)
	}
}

func TestKernelCleanupIdentityDoesNotTreatNamespaceMoveAsDeletion(t *testing.T) {
	requireSecurityTCNamespace(t)
	_, driver, _, _ := networkFixture(t, false)
	other := cleanupTestNetworkNamespace(t)
	defer other.Close()
	link, err := netlink.LinkByIndex(driver.index)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(link, int(other.Fd())); err != nil {
		t.Fatal(err)
	}
	index, missing, err := cleanupSysfsIndex(int(driver.anchor.file.Fd()))
	t.Logf("moved original inode ifindex=%d missing=%v err=%v", index, missing, err)
	if alive, err := driver.cleanupIdentity(context.Background()); err == nil || alive {
		t.Fatal("a moved but still existing original inode was guessed dead", alive, err)
	}
}

func cleanupTestNetworkNamespace(t *testing.T) *os.File {
	t.Helper()
	type result struct {
		file *os.File
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		original, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			ready <- result{err: err}
			return
		}
		defer original.Close()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			ready <- result{err: err}
			return
		}
		other, err := os.Open("/proc/thread-self/ns/net")
		if restoreErr := unix.Setns(int(original.Fd()), unix.CLONE_NEWNET); restoreErr != nil {
			if other != nil {
				other.Close()
			}
			ready <- result{err: restoreErr}
			return // Never return an unrestored OS thread to Go's pool.
		}
		runtime.UnlockOSThread()
		ready <- result{file: other, err: err}
	}()
	value := <-ready
	if value.err != nil || value.file == nil {
		t.Fatal("private target namespace", value.err)
	}
	return value.file
}
