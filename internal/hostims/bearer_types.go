package hostims

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"
)

var (
	ErrSelection  = staticError("host IMS: invalid modem, SIM or bearer selection")
	ErrChanged    = staticError("host IMS: selected service, hardware, SIM or bearer changed")
	ErrBusy       = staticError("host IMS: selected modem already has an operation")
	ErrConflict   = staticError("host IMS: conflicting packet-data bearer")
	ErrUncertain  = staticError("host IMS: bearer outcome is uncertain; no automatic retry")
	ErrJournal    = staticError("host IMS: durable bearer ownership is unavailable")
	ErrDenied     = staticError("host IMS: cellular bearer authorization denied")
	ErrTransport  = staticError("host IMS: ModemManager operation failed")
	ErrParameters = staticError("host IMS: unsupported bearer configuration")
)

type staticError string

func (e staticError) Error() string { return string(e) }

type BearerOptions struct {
	APN string
	// SinglePDN explicitly opts out of required multiplexing. The owner then
	// rejects any other configured bearer, not just an active default bearer.
	SinglePDN    bool
	AllowRoaming bool
	IPFamily     uint32 // MM flags: 1=IPv4, 2=IPv6, 4=IPv4v6; 0=select supported.
	Timeout      time.Duration
}

func (o BearerOptions) normalized() (BearerOptions, error) {
	if o.APN == "" {
		o.APN = "ims"
	}
	o.APN = strings.ToLower(o.APN)
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	if !validAPN(o.APN) || o.Timeout < 100*time.Millisecond || o.Timeout > time.Minute || o.IPFamily != 0 && o.IPFamily != 1 && o.IPFamily != 2 && o.IPFamily != 4 {
		return o, ErrSelection
	}
	return o, nil
}

type cardBinding struct {
	Equipment, Device, SIMPath, ICCID, IMSI, Physical, PhysicalToken string
	Slot                                                             uint32
}

func (b cardBinding) valid() bool {
	return asciiDigits(b.Equipment, 15, 15) && strings.Trim(b.Equipment, "0") != "" &&
		b.Device != "" && len(b.Device) <= 256 && objectPath(b.SIMPath, "SIM") &&
		asciiDigits(b.ICCID, 18, 20) && asciiDigits(b.IMSI, 14, 16) &&
		strings.HasPrefix(b.Physical, "/sys/devices/") && len(b.Physical) <= 1024 && b.PhysicalToken != ""
}
func (b cardBinding) fingerprint(owner, path string) string {
	return hashText(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d", owner, path, b.Equipment, b.Device, b.SIMPath, b.ICCID, b.IMSI, b.Physical, b.PhysicalToken, b.Slot))
}
func (b cardBinding) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "<private cellular binding>")
}

type modemSnapshot struct {
	Owner, Path string
	Binding     cardBinding
	// Empty means no unambiguous QMI port, not permission to guess cdc-wdm0.
	QMIControl string
	State      int32
	Families   uint32
	Bearers    []string
}

func (s modemSnapshot) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private modem snapshot>")
}

type bearerProperties struct {
	APN                          string
	APNType, IPFamily, Multiplex uint32
	AllowRoaming                 bool
}
type bearerSnapshot struct {
	ConfigurationError                bool
	Properties                        bearerProperties
	Connected, Suspended, Multiplexed bool
	Interface                         string
	Addresses                         []BearerAddress
}
type BearerAddress struct {
	Address netip.Prefix
	Gateway netip.Addr
	MTU     uint32
}
type BearerConfiguration struct {
	Interface string
	Addresses []BearerAddress
}

// The backend is typed so the owner never forwards raw D-Bus errors/properties
// (which contain subscriber identifiers) into public diagnostics.
type bearerBackend interface {
	Modems(context.Context) (map[string]string, error)
	Modem(context.Context, string) (modemSnapshot, error)
	Bearer(context.Context, string, string) (bearerSnapshot, error)
	Create(context.Context, string, string, bearerProperties) (string, error)
	Connect(context.Context, string, string) error
	Disconnect(context.Context, string, string) error
	Delete(context.Context, string, string, string) error
}

type Selection struct {
	pool             *BearerPool
	snapshot         modemSnapshot
	key, fingerprint string
}

func (s Selection) Key() string           { return s.key }
func (s Selection) SubscriberKey() string { return hashText(s.snapshot.Binding.ICCID) }

// SubscriberIMSI is for the existing product policy gate only. Never log it.
func (s Selection) SubscriberIMSI() string  { return s.snapshot.Binding.IMSI }
func (s Selection) SubscriberICCID() string { return s.snapshot.Binding.ICCID }
func (s Selection) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<cellular modem "+s.key+">")
}

func hashText(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func asciiDigits(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func objectPath(path, kind string) bool {
	suffix, ok := strings.CutPrefix(path, "/org/freedesktop/ModemManager1/"+kind+"/")
	return ok && asciiDigits(suffix, 1, 10)
}
func validOwner(owner string) bool {
	left, right, ok := strings.Cut(strings.TrimPrefix(owner, ":"), ".")
	return strings.HasPrefix(owner, ":") && ok && asciiDigits(left, 1, 10) && asciiDigits(right, 1, 10)
}
