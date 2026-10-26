//go:build linux

package hostims

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	mmService = "org.freedesktop.ModemManager1"
	mmRoot    = "/org/freedesktop/ModemManager1"
	mmModem   = mmService + ".Modem"
	mmSIM     = mmService + ".Sim"
	mmBearer  = mmService + ".Bearer"
)

type modemManagerBackend struct{ connection *dbus.Conn }

// NewModemManagerBackend uses an existing private system-bus connection owned
// by the caller. No radio or bearer operation runs during construction/select.
func NewModemManagerBackend(connection *dbus.Conn) (*modemManagerBackend, error) {
	if connection == nil || !connection.Connected() {
		return nil, ErrTransport
	}
	return &modemManagerBackend{connection: connection}, nil
}

func (b *modemManagerBackend) owner(ctx context.Context) (string, error) {
	var owner string
	err := b.connection.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, mmService).Store(&owner)
	if err != nil || !validOwner(owner) {
		return "", ErrTransport
	}
	return owner, nil
}
func (b *modemManagerBackend) properties(ctx context.Context, owner, path, iface string) (map[string]dbus.Variant, error) {
	call := b.connection.Object(owner, dbus.ObjectPath(path)).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, iface)
	if call.Err != nil || len(call.Body) != 1 {
		return nil, ErrTransport
	}
	// Store recursively unwraps/re-wraps Variant values and can turn a(su)
	// into aav. Preserve the decoder's original signatures for schema checks.
	values, valid := call.Body[0].(map[string]dbus.Variant)
	if !valid || len(values) > 128 {
		return nil, ErrTransport
	}
	return values, nil
}
func (b *modemManagerBackend) Modems(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	owner, err := b.owner(ctx)
	if err != nil {
		return nil, err
	}
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if b.connection.Object(owner, dbus.ObjectPath(mmRoot)).CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects) != nil || len(objects) > 256 {
		return nil, ErrTransport
	}
	result := make(map[string]string)
	for path, interfaces := range objects {
		if props, ok := interfaces[mmModem]; ok {
			if !objectPath(string(path), "Modem") {
				return nil, ErrSelection
			}
			equipment, err := mmText(props, "EquipmentIdentifier")
			if err != nil {
				return nil, err
			}
			result[string(path)] = equipment
		}
	}
	return result, nil
}
func (b *modemManagerBackend) Modem(ctx context.Context, path string) (modemSnapshot, error) {
	if !objectPath(path, "Modem") {
		return modemSnapshot{}, ErrSelection
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	owner, err := b.owner(ctx)
	if err != nil {
		return modemSnapshot{}, err
	}
	props, err := b.properties(ctx, owner, path, mmModem)
	if err != nil {
		return modemSnapshot{}, err
	}
	read := func(name string) string { value, _ := mmText(props, name); return value }
	physical := read("Physdev")
	if physical == "" {
		physical = read("Device")
	}
	physicalToken, err := mmPhysicalToken(physical)
	if err != nil {
		return modemSnapshot{}, err
	}
	var busID string
	if b.connection.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetId", 0).Store(&busID) != nil {
		return modemSnapshot{}, ErrTransport
	}
	busBytes, err := hex.DecodeString(busID)
	if err != nil || len(busBytes) != 16 {
		return modemSnapshot{}, ErrTransport
	}
	simPath, ok := props["Sim"].Value().(dbus.ObjectPath)
	if !ok || simPath != "/" && !objectPath(string(simPath), "SIM") {
		return modemSnapshot{}, ErrSelection
	}
	var iccid, imsi string
	if simPath != "/" {
		sim, err := b.properties(ctx, owner, string(simPath), mmSIM)
		if err != nil {
			return modemSnapshot{}, err
		}
		iccid, _ = mmText(sim, "SimIdentifier")
		imsi, _ = mmText(sim, "Imsi")
		if active, exists := sim["Active"]; exists {
			value, ok := active.Value().(bool)
			if !ok || !value {
				return modemSnapshot{}, ErrSelection
			}
		}
	}
	state, ok := props["State"].Value().(int32)
	if !ok {
		return modemSnapshot{}, ErrSelection
	}
	families, err := mmUint(props, "SupportedIpFamilies")
	if err != nil {
		return modemSnapshot{}, err
	}
	slot, err := mmUint(props, "PrimarySimSlot")
	if err != nil {
		return modemSnapshot{}, err
	}
	bearers, ok := props["Bearers"].Value().([]dbus.ObjectPath)
	if !ok || len(bearers) > 64 {
		return modemSnapshot{}, ErrSelection
	}
	result := modemSnapshot{Owner: owner, Path: path, State: state, Families: families, SIMMissing: simPath == "/",
		Binding: cardBinding{Equipment: read("EquipmentIdentifier"), Device: read("DeviceIdentifier"), SIMPath: string(simPath), ICCID: iccid, IMSI: imsi,
			Physical: physical, PhysicalToken: hashText(physicalToken + "\x00" + busID), Slot: slot}}
	result.QMIControl, _ = mmQMIControl(props)
	for _, bearer := range bearers {
		if !objectPath(string(bearer), "Bearer") {
			return modemSnapshot{}, ErrSelection
		}
		result.Bearers = append(result.Bearers, string(bearer))
	}
	// Do not accept an identity assembled across a slot switch/service restart.
	again, err := b.properties(ctx, owner, path, mmModem)
	if err != nil {
		return modemSnapshot{}, err
	}
	againSIM, validSIM := again["Sim"].Value().(dbus.ObjectPath)
	againSlot, slotErr := mmUint(again, "PrimarySimSlot")
	againControl, _ := mmQMIControl(again)
	if !validSIM || againSIM != simPath || slotErr != nil || againSlot != slot || againControl != result.QMIControl {
		return modemSnapshot{}, ErrChanged
	}
	current, err := b.owner(ctx)
	if err != nil || current != owner {
		return modemSnapshot{}, ErrChanged
	}
	return result, nil
}

type mmPort struct {
	Name string
	Kind uint32
}

func mmPortName(value string) (string, bool) {
	if strings.HasPrefix(value, "/dev/") {
		value = strings.TrimPrefix(value, "/dev/")
	}
	if len(value) < 1 || len(value) > 128 || value == "." || value == ".." {
		return "", false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "", false
		}
	}
	return value, true
}

// Ports is a(su), and MM_MODEM_PORT_TYPE_QMI is 6. PrimaryPort disambiguates
// multiple QMI ports only if it itself names a listed QMI port.
func mmQMIControl(props map[string]dbus.Variant) (string, error) {
	value, exists := props["Ports"]
	if !exists {
		return "", ErrSelection
	}
	raw := reflect.ValueOf(value.Value())
	if value.Signature().String() != "a(su)" || !raw.IsValid() || raw.Kind() != reflect.Slice || raw.Len() < 1 || raw.Len() > 64 {
		return "", ErrSelection
	}
	var ports []mmPort
	if dbus.Store([]any{value.Value()}, &ports) != nil || len(ports) < 1 || len(ports) > 64 {
		return "", ErrSelection
	}
	primary := ""
	if raw, ok := props["PrimaryPort"]; ok {
		text, valid := raw.Value().(string)
		if !valid {
			return "", ErrSelection
		}
		if text != "" {
			var named bool
			primary, named = mmPortName(text)
			if !named {
				return "", ErrSelection
			}
		}
	}
	seen := map[string]bool{}
	var qmiPorts []string
	for _, port := range ports {
		name, valid := mmPortName(port.Name)
		if !valid || seen[name] {
			return "", ErrSelection
		}
		seen[name] = true
		if port.Kind == 6 {
			qmiPorts = append(qmiPorts, name)
		}
	}
	if len(qmiPorts) == 1 {
		return "/dev/" + qmiPorts[0], nil
	}
	for _, port := range qmiPorts {
		if port == primary {
			return "/dev/" + port, nil
		}
	}
	return "", ErrSelection
}
func mmPhysicalToken(path string) (string, error) {
	if !strings.HasPrefix(path, "/sys/devices/") || filepath.Clean(path) != path {
		return "", ErrSelection
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", ErrSelection
	}
	var stat unix.Stat_t
	if unix.Stat(path, &stat) != nil {
		return "", ErrSelection
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(boot) > 64 {
		return "", ErrSelection
	}
	return hashText(fmt.Sprintf("%s\x00%d:%d:%d:%d\x00%s", path, stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec, boot)), nil
}

func (b *modemManagerBackend) Bearer(ctx context.Context, owner, path string) (bearerSnapshot, error) {
	if !validOwner(owner) || !objectPath(path, "Bearer") {
		return bearerSnapshot{}, ErrSelection
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	props, err := b.properties(ctx, owner, path, mmBearer)
	if err != nil {
		return bearerSnapshot{}, err
	}
	return parseMMBearer(props)
}
func parseMMBearer(props map[string]dbus.Variant) (bearerSnapshot, error) {
	var result bearerSnapshot
	config, ok := props["Properties"].Value().(map[string]dbus.Variant)
	if !ok {
		return result, ErrParameters
	}
	apn, err := mmText(config, "apn")
	if err != nil {
		return result, err
	}
	apnType, err := mmUint(config, "apn-type")
	if err != nil {
		return result, err
	}
	family, err := mmUint(config, "ip-type")
	if err != nil {
		return result, err
	}
	mux, err := mmUint(config, "multiplex")
	if err != nil {
		return result, err
	}
	roaming, err := mmBool(config, "allow-roaming")
	if err != nil {
		return result, err
	}
	result.Properties = bearerProperties{APN: apn, APNType: apnType, IPFamily: family, Multiplex: mux, AllowRoaming: roaming}
	result.Connected, err = mmBool(props, "Connected")
	if err != nil {
		return result, err
	}
	result.Suspended, err = mmBool(props, "Suspended")
	if err != nil {
		return result, err
	}
	result.Multiplexed, err = mmBool(props, "Multiplexed")
	if err != nil {
		return result, err
	}
	result.Interface, err = mmText(props, "Interface")
	if err != nil {
		return result, err
	}
	result.Addresses, err = parseMMAddresses(props)
	result.ConfigurationError = err != nil
	if result.Connected && !validConfiguration(result) {
		result.ConfigurationError = true
	}
	// Ownership metadata remains usable for exact cleanup even when DHCP,
	// PPP, SLAAC or malformed addressing cannot authorize IMS networking.
	return result, nil
}

func parseMMAddresses(props map[string]dbus.Variant) ([]BearerAddress, error) {
	var result []BearerAddress
	for _, key := range []string{"Ip4Config", "Ip6Config"} {
		config, ok := props[key].Value().(map[string]dbus.Variant)
		if !ok {
			return result, ErrParameters
		}
		if len(config) == 0 {
			continue
		}
		method, err := mmUint(config, "method")
		if err != nil {
			return result, err
		}
		if method == 0 {
			continue
		}
		if method != 2 {
			return result, ErrParameters
		}
		address, err := mmText(config, "address")
		if err != nil {
			return result, err
		}
		ip, err := netip.ParseAddr(address)
		if err != nil || ip.Is4() != (key == "Ip4Config") {
			return result, ErrParameters
		}
		prefix, err := mmUint(config, "prefix")
		if err != nil || prefix > uint32(ip.BitLen()) {
			return result, ErrParameters
		}
		value := BearerAddress{Address: netip.PrefixFrom(ip, int(prefix))}
		if raw, ok := config["gateway"]; ok {
			gateway, ok := raw.Value().(string)
			if !ok {
				return result, ErrParameters
			}
			if gateway != "" {
				value.Gateway, err = netip.ParseAddr(gateway)
				if err != nil {
					return result, ErrParameters
				}
			}
		}
		if _, ok := config["mtu"]; ok {
			value.MTU, err = mmUint(config, "mtu")
			if err != nil {
				return result, err
			}
		}
		result = append(result, value)
	}
	return result, nil
}

func (b *modemManagerBackend) Create(ctx context.Context, owner, modem string, props bearerProperties) (string, error) {
	if !validOwner(owner) || !objectPath(modem, "Modem") {
		return "", ErrSelection
	}
	args := map[string]dbus.Variant{"apn": dbus.MakeVariant(props.APN), "apn-type": dbus.MakeVariant(props.APNType), "ip-type": dbus.MakeVariant(props.IPFamily), "multiplex": dbus.MakeVariant(props.Multiplex), "allow-roaming": dbus.MakeVariant(props.AllowRoaming)}
	var path dbus.ObjectPath
	if b.connection.Object(owner, dbus.ObjectPath(modem)).CallWithContext(ctx, mmModem+".CreateBearer", 0, args).Store(&path) != nil {
		return "", ErrTransport
	}
	return string(path), nil
}
func (b *modemManagerBackend) Connect(ctx context.Context, owner, path string) error {
	return b.bearerMethod(ctx, owner, path, "Connect")
}

func (b *modemManagerBackend) Enable(ctx context.Context, owner, path string) error {
	if !validOwner(owner) || !objectPath(path, "Modem") {
		return ErrSelection
	}
	if b.connection.Object(owner, dbus.ObjectPath(path)).CallWithContext(ctx, mmModem+".Enable", 0, true).Err != nil {
		return ErrTransport
	}
	return nil
}
func (b *modemManagerBackend) Disconnect(ctx context.Context, owner, path string) error {
	return b.bearerMethod(ctx, owner, path, "Disconnect")
}
func (b *modemManagerBackend) bearerMethod(ctx context.Context, owner, path, method string) error {
	if !validOwner(owner) || !objectPath(path, "Bearer") {
		return ErrSelection
	}
	if b.connection.Object(owner, dbus.ObjectPath(path)).CallWithContext(ctx, mmBearer+"."+method, 0).Err != nil {
		return ErrTransport
	}
	return nil
}
func (b *modemManagerBackend) Delete(ctx context.Context, owner, modem, path string) error {
	if !validOwner(owner) || !objectPath(modem, "Modem") || !objectPath(path, "Bearer") {
		return ErrSelection
	}
	if b.connection.Object(owner, dbus.ObjectPath(modem)).CallWithContext(ctx, mmModem+".DeleteBearer", 0, dbus.ObjectPath(path)).Err != nil {
		return ErrTransport
	}
	return nil
}
func mmText(props map[string]dbus.Variant, key string) (string, error) {
	v, ok := props[key].Value().(string)
	if !ok || len(v) > 4096 {
		return "", ErrParameters
	}
	return v, nil
}
func mmUint(props map[string]dbus.Variant, key string) (uint32, error) {
	v, ok := props[key].Value().(uint32)
	if !ok {
		return 0, ErrParameters
	}
	return v, nil
}
func mmBool(props map[string]dbus.Variant, key string) (bool, error) {
	v, ok := props[key].Value().(bool)
	if !ok {
		return false, ErrParameters
	}
	return v, nil
}
