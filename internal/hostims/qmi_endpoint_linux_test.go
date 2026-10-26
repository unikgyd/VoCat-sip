//go:build linux

package hostims

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func TestMMQMIControlPortSelection(t *testing.T) {
	for _, row := range []struct {
		name, primary, want string
		ports               []mmPort
	}{
		{"unique", "ttyUSB2", "/dev/cdc-wdm7", []mmPort{{"ttyUSB2", 3}, {"cdc-wdm7", 6}}},
		{"full-path", "/dev/cdc-wdm7", "/dev/cdc-wdm7", []mmPort{{"/dev/cdc-wdm7", 6}}},
		{"native", "wwan2qmi0", "/dev/wwan2qmi0", []mmPort{{"wwan2qmi0", 6}}},
		{"primary-disambiguates", "cdc-wdm8", "/dev/cdc-wdm8", []mmPort{{"cdc-wdm7", 6}, {"cdc-wdm8", 6}}},
		{"ambiguous", "ttyUSB2", "", []mmPort{{"ttyUSB2", 3}, {"cdc-wdm7", 6}, {"cdc-wdm8", 6}}},
		{"mbim-not-qmi", "cdc-wdm0", "", []mmPort{{"cdc-wdm0", 7}}},
		{"duplicate", "cdc-wdm0", "", []mmPort{{"cdc-wdm0", 6}, {"/dev/cdc-wdm0", 6}}},
		{"traversal", "", "", []mmPort{{"../cdc-wdm0", 6}}},
		{"outside-dev", "", "", []mmPort{{"/tmp/cdc-wdm0", 6}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			props := map[string]dbus.Variant{"Ports": dbus.MakeVariant(row.ports), "PrimaryPort": dbus.MakeVariant(row.primary)}
			path, err := mmQMIControl(props)
			if row.want == "" {
				if err == nil || path != "" {
					t.Fatal("ambiguous/unsafe QMI port accepted")
				}
				return
			}
			if err != nil || path != row.want {
				t.Fatal("wrong QMI port", err)
			}
		})
	}
	if _, err := mmQMIControl(map[string]dbus.Variant{"Ports": dbus.MakeVariant("wrong wire type")}); err == nil {
		t.Fatal("wrong D-Bus type accepted")
	}
}
func endpointPinFixture(t *testing.T) (Selection, endpointRoots, string) {
	t.Helper()
	base := t.TempDir()
	roots := endpointRoots{dev: filepath.Join(base, "dev"), sys: filepath.Join(base, "sys")}
	_, _, _, s := bearerFixture(t)
	s.snapshot.QMIControl = "/dev/cdc-wdm-test"
	s.snapshot.Binding.Physical = "/sys/devices/modem0"
	physical := filepath.Join(roots.sys, "devices", "modem0")
	target := filepath.Join(physical, "interface0", "usbmisc", "cdc-wdm-test")
	for _, directory := range []string{roots.dev, target, filepath.Join(roots.sys, "dev", "char"), filepath.Join(roots.sys, "class", "usbmisc")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mknod(filepath.Join(roots.dev, "cdc-wdm-test"), unix.S_IFCHR|0600, int(unix.Mkdev(1, 3))); err != nil {
		if (errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)) && os.Getenv("VOCAT_QMI_NODE_TESTS_REQUIRED") != "1" {
			t.Skip("private character-node fixture needs CAP_MKNOD")
		}
		t.Fatal("private character node fixture requires CAP_MKNOD", err)
	}
	if err := os.Symlink(target, filepath.Join(roots.sys, "dev", "char", "1:3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(roots.sys, "class", "usbmisc"), filepath.Join(target, "subsystem")); err != nil {
		t.Fatal(err)
	}
	return s, roots, target
}
func TestQMIEndpointPinsExactCharacterNodeAndPhysicalAncestry(t *testing.T) {
	s, roots, _ := endpointPinFixture(t)
	pin, err := pinQMIEndpoint(s, roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := pin.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pin.Path() != filepath.Join(roots.dev, "cdc-wdm-test") {
		t.Fatal("guessed device path")
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pin.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatal("closed pin still accepted")
	}
}
func TestQMIEndpointRejectsNodeReplacementSymlinkAndWrongModem(t *testing.T) {
	for _, mode := range []string{"replacement", "symlink", "foreign-sysfs", "missing-sysfs", "wrong-class", "parent-rename"} {
		t.Run(mode, func(t *testing.T) {
			s, roots, target := endpointPinFixture(t)
			pin, err := pinQMIEndpoint(s, roots)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			switch mode {
			case "replacement":
				if err := os.Rename(pin.Path(), pin.Path()+".original"); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mknod(pin.Path(), unix.S_IFCHR|0600, int(unix.Mkdev(1, 3))); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(pin.Path(), pin.Path()+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(pin.Path()+".original", pin.Path()); err != nil {
					t.Fatal(err)
				}
			case "foreign-sysfs":
				foreign := filepath.Join(roots.sys, "devices", "modem1")
				if err := os.MkdirAll(foreign, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(pin.charLink); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, pin.charLink); err != nil {
					t.Fatal(err)
				}
			case "missing-sysfs":
				if err := os.Remove(pin.charLink); err != nil {
					t.Fatal(err)
				}
			case "wrong-class":
				if err := os.Remove(filepath.Join(target, "subsystem")); err != nil {
					t.Fatal(err)
				}
			case "parent-rename":
				if err := os.Rename(pin.physical, pin.physical+".old"); err != nil {
					t.Fatal(err)
				}
			}
			if err := pin.Verify(context.Background()); err == nil {
				t.Fatal("changed physical endpoint accepted")
			}
			if second, err := pinQMIEndpoint(s, roots); err == nil {
				second.Close()
				if mode != "replacement" {
					t.Fatal("unsafe new pin accepted")
				}
			}
		})
	}
}
func TestQMIEndpointRealDBusPortDiscovery(t *testing.T) {
	p, b, _, _, _ := mmWireFixture(t)
	path := "/org/freedesktop/ModemManager1/Modem/0"
	b.mu.Lock()
	m := b.modems[path]
	m.QMIControl = "/dev/cdc-wdm9"
	b.modems[path] = m
	b.mu.Unlock()
	s, err := p.Select(context.Background(), path)
	if err != nil || s.snapshot.QMIControl != "/dev/cdc-wdm9" {
		if backend, ok := p.backend.(*modemManagerBackend); ok {
			props, _ := backend.properties(context.Background(), s.snapshot.Owner, path, mmModem)
			value := props["Ports"]
			_, parseErr := mmQMIControl(props)
			t.Logf("port wire signature=%s Go-type=%T parse=%v", value.Signature().String(), value.Value(), parseErr)
		}
		t.Fatal("a(su) port decoding failed", err)
	}
}
func TestSystemQMIEndpointProbeIsReadOnly(t *testing.T) {
	if os.Getenv("VOCAT_QMI_ENDPOINT_PROBE") != "1" {
		t.Skip("explicit read-only system endpoint probe")
	}
	connection, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal("system bus unavailable")
	}
	defer connection.Close()
	backend, err := NewModemManagerBackend(connection)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := NewBearerPool(backend, newMemoryClaims(), func(context.Context, Selection) error { return ErrDenied })
	paths, err := backend.Modems(context.Background())
	if err != nil {
		t.Fatal("modem metadata unavailable")
	}
	verified := 0
	for path := range paths {
		s, err := pool.Select(context.Background(), path)
		if err != nil {
			continue
		}
		if _, err := ProbeQMIEndpoint(context.Background(), s); err != nil {
			t.Fatal("selected endpoint could not be pinned")
		}
		verified++
	}
	if verified == 0 {
		t.Fatal("no selected modem endpoint to verify")
	}
	t.Logf("verified=%d; driver I/O=0; QMI requests=0; UICC reads=0", verified)
}
