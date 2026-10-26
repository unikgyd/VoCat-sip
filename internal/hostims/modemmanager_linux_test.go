//go:build linux

package hostims

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"vocat/internal/store"
)

func privateDBus(t *testing.T) string {
	t.Helper()
	command, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Fatal("dbus-daemon required for isolated integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command, "--session", "--nofork", "--print-address=1")
	cmd.Stderr = io.Discard
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(output).ReadString('\n'); ready <- line }()
	select {
	case address := <-ready:
		if len(address) == 0 {
			t.Fatal("private bus did not start")
		}
		return address[:len(address)-1]
	case <-time.After(5 * time.Second):
		t.Fatal("private bus startup timed out")
	}
	return ""
}

type wireProperties struct {
	read func(string) (map[string]dbus.Variant, *dbus.Error)
}

func (w *wireProperties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	return w.read(iface)
}

type wireObjects struct{ backend *fakeBearerBackend }

func (w *wireObjects) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	paths, _ := w.backend.Modems(context.Background())
	result := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for path, equipment := range paths {
		result[dbus.ObjectPath(path)] = map[string]map[string]dbus.Variant{mmModem: {"EquipmentIdentifier": dbus.MakeVariant(equipment)}}
	}
	return result, nil
}

type wireModem struct {
	backend     *fakeBearerBackend
	connection  *dbus.Conn
	owner, path string
}

func (w *wireModem) CreateBearer(values map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	apn, _ := mmText(values, "apn")
	apnType, _ := mmUint(values, "apn-type")
	family, _ := mmUint(values, "ip-type")
	mux, _ := mmUint(values, "multiplex")
	roaming, _ := mmBool(values, "allow-roaming")
	path, err := w.backend.Create(context.Background(), w.owner, w.path, bearerProperties{APN: apn, APNType: apnType, IPFamily: family, Multiplex: mux, AllowRoaming: roaming})
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if err := w.connection.Export(&wireBearer{backend: w.backend, owner: w.owner, path: path}, dbus.ObjectPath(path), mmBearer); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if err := w.connection.Export(&wireProperties{read: func(string) (map[string]dbus.Variant, *dbus.Error) {
		b, err := w.backend.Bearer(context.Background(), w.owner, path)
		if err != nil {
			return nil, dbus.MakeFailedError(err)
		}
		return wireBearerProps(b), nil
	}}, dbus.ObjectPath(path), "org.freedesktop.DBus.Properties"); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return dbus.ObjectPath(path), nil
}
func (w *wireModem) DeleteBearer(path dbus.ObjectPath) *dbus.Error {
	if err := w.backend.Delete(context.Background(), w.owner, w.path, string(path)); err != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}

type wireBearer struct {
	backend     *fakeBearerBackend
	owner, path string
}

func (w *wireBearer) Connect() *dbus.Error {
	if err := w.backend.Connect(context.Background(), w.owner, w.path); err != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}
func (w *wireBearer) Disconnect() *dbus.Error {
	if err := w.backend.Disconnect(context.Background(), w.owner, w.path); err != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}
func wireBearerProps(b bearerSnapshot) map[string]dbus.Variant {
	v4, v6 := map[string]dbus.Variant{}, map[string]dbus.Variant{}
	for _, a := range b.Addresses {
		value := map[string]dbus.Variant{"method": dbus.MakeVariant(uint32(2)), "address": dbus.MakeVariant(a.Address.Addr().String()), "prefix": dbus.MakeVariant(uint32(a.Address.Bits())), "mtu": dbus.MakeVariant(a.MTU)}
		if a.Address.Addr().Is4() {
			v4 = value
		} else {
			v6 = value
		}
	}
	if b.ConfigurationError {
		v6 = map[string]dbus.Variant{"method": dbus.MakeVariant(uint32(3))}
	}
	return map[string]dbus.Variant{"Properties": dbus.MakeVariant(map[string]dbus.Variant{"apn": dbus.MakeVariant(b.Properties.APN), "apn-type": dbus.MakeVariant(b.Properties.APNType), "ip-type": dbus.MakeVariant(b.Properties.IPFamily), "multiplex": dbus.MakeVariant(b.Properties.Multiplex), "allow-roaming": dbus.MakeVariant(b.Properties.AllowRoaming)}),
		"Connected": dbus.MakeVariant(b.Connected), "Suspended": dbus.MakeVariant(b.Suspended), "Multiplexed": dbus.MakeVariant(b.Multiplexed), "Interface": dbus.MakeVariant(b.Interface), "Ip4Config": dbus.MakeVariant(v4), "Ip6Config": dbus.MakeVariant(v6)}
}
func wireModemProps(m modemSnapshot) map[string]dbus.Variant {
	paths := make([]dbus.ObjectPath, 0, len(m.Bearers))
	for _, path := range m.Bearers {
		paths = append(paths, dbus.ObjectPath(path))
	}
	result := map[string]dbus.Variant{"EquipmentIdentifier": dbus.MakeVariant(m.Binding.Equipment), "DeviceIdentifier": dbus.MakeVariant(m.Binding.Device), "Device": dbus.MakeVariant("/sys/devices/system/cpu"), "Sim": dbus.MakeVariant(dbus.ObjectPath(m.Binding.SIMPath)), "PrimarySimSlot": dbus.MakeVariant(m.Binding.Slot), "State": dbus.MakeVariant(m.State), "SupportedIpFamilies": dbus.MakeVariant(m.Families), "Bearers": dbus.MakeVariant(paths)}
	if m.QMIControl != "" {
		result["Ports"] = dbus.MakeVariant([]mmPort{{Name: m.QMIControl, Kind: 6}})
		result["PrimaryPort"] = dbus.MakeVariant(m.QMIControl)
	}
	return result
}
func mmWireFixture(t *testing.T) (*BearerPool, *fakeBearerBackend, SQLiteClaimJournal, Selection, *dbus.Conn) {
	t.Helper()
	address := privateDBus(t)
	server, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if reply, err := server.RequestName(mmService, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal("mock service name not owned")
	}
	b := newFakeBearerBackend()
	owner := server.Names()[0]
	for path, m := range b.modems {
		m.Owner = owner
		b.modems[path] = m
		pathCopy := path
		if err := server.Export(&wireModem{backend: b, connection: server, owner: owner, path: path}, dbus.ObjectPath(path), mmModem); err != nil {
			t.Fatal(err)
		}
		if err := server.Export(&wireProperties{read: func(string) (map[string]dbus.Variant, *dbus.Error) {
			m, err := b.Modem(context.Background(), pathCopy)
			if err != nil {
				return nil, dbus.MakeFailedError(err)
			}
			return wireModemProps(m), nil
		}}, dbus.ObjectPath(path), "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
		simPath := m.Binding.SIMPath
		if err := server.Export(&wireProperties{read: func(string) (map[string]dbus.Variant, *dbus.Error) {
			m, err := b.Modem(context.Background(), pathCopy)
			if err != nil {
				return nil, dbus.MakeFailedError(err)
			}
			return map[string]dbus.Variant{"SimIdentifier": dbus.MakeVariant(m.Binding.ICCID), "Imsi": dbus.MakeVariant(m.Binding.IMSI), "Active": dbus.MakeVariant(true)}, nil
		}}, dbus.ObjectPath(simPath), "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.Export(&wireObjects{backend: b}, dbus.ObjectPath(mmRoot), "org.freedesktop.DBus.ObjectManager"); err != nil {
		t.Fatal(err)
	}
	backend, err := NewModemManagerBackend(client)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	j := SQLiteClaimJournal{Database: database, lockDirectory: filepath.Join(t.TempDir(), "private-locks")}
	p, err := NewBearerPool(backend, j, func(context.Context, Selection) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Select(context.Background(), "/org/freedesktop/ModemManager1/Modem/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.claims {
			c.mu.Lock()
			if c.lock != nil {
				_ = c.lock.Close()
			}
			c.mu.Unlock()
		}
	})
	return p, b, j, s, server
}

func TestModemManagerRealDBusOwnedLifecycle(t *testing.T) {
	p, b, j, s, _ := mmWireFixture(t)
	lease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if raw, err := j.Read(context.Background(), s.key); err != nil || raw != nil {
		t.Fatal("persistent fence not released")
	}
	if b.count("create") != 1 || b.count("delete") != 1 {
		t.Fatal("wire lifecycle repeated mutations")
	}
}

func TestModemManagerLiveLockAndExplicitRestartRecovery(t *testing.T) {
	p, b, j, s, _ := mmWireFixture(t)
	lease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewBearerPool(p.backend, j, p.authorize)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := other.Select(context.Background(), s.snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Recover(context.Background(), selected); !errors.Is(err, ErrBusy) {
		t.Fatal("another process's live lock did not fence recovery")
	}
	// Simulate process death: the OS closes its FD, but its durable claim and
	// daemon-owned PDN survive. No production force-unlock API exists.
	lease.claim.mu.Lock()
	_ = lease.claim.lock.Close()
	lease.claim.lock = nil
	lease.claim.mu.Unlock()
	if err := other.Recover(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	if b.count("disconnect") != 1 || b.count("delete") != 1 {
		t.Fatal("restart recovery did not clean exact confirmed path")
	}
	newLease, err := other.Acquire(context.Background(), selected, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Recover(context.Background(), selected); !errors.Is(err, ErrBusy) {
		t.Fatal("adopted flag leaked to a new live generation")
	}
	if err := newLease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestModemManagerServiceLossDoesNotDeleteOtherResources(t *testing.T) {
	p, b, _, s, server := mmWireFixture(t)
	lease, err := p.Acquire(context.Background(), s, BearerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = server.ReleaseName(mmService)
	if err := lease.Verify(context.Background()); err == nil {
		t.Fatal("lost service still verified")
	}
	if err := lease.Close(context.Background()); err == nil {
		t.Fatal("lost service authorized cleanup")
	}
	if b.count("disconnect") != 0 || b.count("delete") != 0 {
		t.Fatal("service loss touched resources")
	}
}

func TestModemManagerRejectsPPPAndSLAACWithoutHostConfiguration(t *testing.T) {
	for _, method := range []uint32{1, 3} {
		props := wireBearerProps(bearerSnapshot{Properties: bearerProperties{APN: "ims"}})
		props["Ip6Config"] = dbus.MakeVariant(map[string]dbus.Variant{"method": dbus.MakeVariant(method)})
		if bearer, err := parseMMBearer(props); err != nil || !bearer.ConfigurationError || bearer.Properties.APN != "ims" {
			t.Fatal("unusable addressing either enabled data or destroyed valid bearer ownership proof")
		}
	}
}

func TestModemManagerUnsupportedAddressingReleasesProvenOwnedPath(t *testing.T) {
	p, b, _, s, _ := mmWireFixture(t)
	b.badConfiguration = true
	if _, err := p.Acquire(context.Background(), s, BearerOptions{}); !errors.Is(err, ErrParameters) {
		t.Fatalf("address rejection=%v", err)
	}
	if b.count("create") != 1 || b.count("connect") != 1 || b.count("disconnect") != 1 || b.count("delete") != 1 {
		t.Fatal("D-Bus cleanup lost ownership because of unsupported addressing")
	}
}

func TestHostIMSProcessLockCannotFollowSymlinkOrReplaceOtherOwner(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private-locks")
	key := hashText("test")
	one, err := lockClaimAt(directory, key)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	if _, err := lockClaimAt(directory, key); !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate live lock admitted")
	}
	if err := one.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, key+".lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "unrelated"), filepath.Join(directory, key+".lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := lockClaimAt(directory, key); !errors.Is(err, ErrJournal) {
		t.Fatal("symlink lock followed")
	}
}

func TestHostIMSProcessLockCrossProcess(t *testing.T) {
	key := hashText("cross-process-lock-test")
	if directory := os.Getenv("VOCAT_HOSTIMS_LOCK_HELPER"); directory != "" {
		_, err := lockClaimAt(directory, key)
		if !errors.Is(err, ErrBusy) {
			t.Fatal("another process acquired the held claim lock")
		}
		return
	}
	directory := filepath.Join(t.TempDir(), "private-locks")
	lock, err := lockClaimAt(directory, key)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostIMSProcessLockCrossProcess$", "-test.timeout=4s")
	child.Env = append(os.Environ(), "VOCAT_HOSTIMS_LOCK_HELPER="+directory)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("independent-process lock proof failed: %v (%s)", err, output)
	}
}

func TestReadOnlySystemModemManagerProbe(t *testing.T) {
	if os.Getenv("VOCAT_MM_SYSTEM_PROBE") != "1" {
		t.Skip("explicit read-only system ModemManager probe")
	}
	connection, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	backend, err := NewModemManagerBackend(connection)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewBearerPool(backend, newMemoryClaims(), func(context.Context, Selection) error { return ErrDenied })
	if err != nil {
		t.Fatal(err)
	}
	paths, err := backend.Modems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for path := range paths {
		if _, err := p.Select(context.Background(), path); err == nil {
			selected++
		}
	}
	if len(paths) == 0 || selected == 0 {
		t.Fatal("no verified modem/SIM selected")
	}
	t.Logf("read-only ModemManager probe: modems=%d, selected=%d; no bearer/radio mutations", len(paths), selected)
}
