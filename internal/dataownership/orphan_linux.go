//go:build linux

package dataownership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func processStart(pid int) (string, bool, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false, err
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return "", false, device.ErrDataCleanupPending
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return "", false, device.ErrDataCleanupPending
	}
	if ticks, err := strconv.ParseUint(fields[19], 10, 64); err != nil || ticks == 0 {
		return "", false, device.ErrDataCleanupPending
	}
	return fields[19], fields[0] == "Z" || fields[0] == "X", nil
}

func captureProcessIdentity(r *record) error {
	var err error
	r.StartTicks, _, err = processStart(os.Getpid())
	if err != nil {
		return err
	}
	for name, target := range map[string]*string{"pid": &r.PIDNamespace, "user": &r.UserNamespace, "net": &r.NetNamespace} {
		*target, err = os.Readlink("/proc/self/ns/" + name)
		if err != nil {
			return err
		}
	}
	return nil
}

// Death is proved in the original boot and namespaces. A PID from another
// container, a permission error, or a reboot never counts as evidence of death.
func originalProcessDead(r record) error {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	ticks, tickErr := strconv.ParseUint(r.StartTicks, 10, 64)
	if err != nil || string(bytes.TrimSpace(boot)) != r.BootID || r.PID <= 0 || r.UID != os.Geteuid() || tickErr != nil || ticks == 0 {
		return device.ErrDataCleanupPending
	}
	for name, expected := range map[string]string{"pid": r.PIDNamespace, "user": r.UserNamespace, "net": r.NetNamespace} {
		actual, err := os.Readlink("/proc/self/ns/" + name)
		if err != nil || expected == "" || actual != expected {
			return device.ErrDataCleanupPending
		}
	}
	start, zombie, err := processStart(r.PID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || start == r.StartTicks && !zombie {
		return device.ErrDataCleanupPending
	}
	return nil // same-namespace PID reuse or a reaped/zombie original
}

type Orphan struct {
	claim    *claim
	original record
	closed   atomic.Bool
}

// OpenOrphan takes the same equipment flock as the original owner. Only a
// committed active receipt is recoverable; legacy/unknown-start records remain
// fenced. Configured physical IDs are supplied by the independent owner only.
func (j Journal) OpenOrphan(ctx context.Context, physicalID string) (*Orphan, error) {
	if j.Database == nil || physicalID == "" {
		return nil, device.ErrDataCleanupPending
	}
	settings, err := j.Database.DataBearerClaims(ctx)
	if err != nil {
		return nil, err
	}
	var selected *record
	var key string
	var raw []byte
	for _, setting := range settings {
		var header struct {
			Version                   int
			EquipmentKey, PhysicalKey string
		}
		if json.Unmarshal(setting.Value, &header) != nil || (header.Version != 1 && header.Version != 2) || setting.Key != store.DataBearerClaimPrefix+header.EquipmentKey || !dataHash(header.EquipmentKey) || !dataHash(header.PhysicalKey) {
			return nil, device.ErrDataCleanupPending
		}
		if header.PhysicalKey != PhysicalKey(modem.Candidate{ID: physicalID}) {
			continue
		}
		if selected != nil {
			return nil, device.ErrDataCleanupPending
		}
		var r record
		decoder := json.NewDecoder(bytes.NewReader(setting.Value))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&r) != nil || r.Version != 2 || r.Host == nil || !r.Host.Ready || device.ValidateDataHostReceipt(*r.Host) != nil || r.Host.Interface != r.Interface || r.Host.DNS.UID != uint32(r.UID) || !dataHash(r.Token) || !dataHash(r.SIMKey) || !validUSBGeneration(r.Generation) || !filepath.IsAbs(r.USBPath) || filepath.Clean(r.USBPath) != r.USBPath || !strings.HasPrefix(r.USBPath, "/sys/") {
			return nil, device.ErrDataCleanupPending
		}
		selected, key, raw = &r, setting.Key, append([]byte(nil), setting.Value...)
	}
	if selected == nil {
		return nil, nil
	}
	if err := originalProcessDead(*selected); err != nil {
		return nil, err
	}
	lock := j.lock
	if lock == nil {
		lock = (hostims.SQLiteClaimJournal{Database: j.Database}).Lock
	}
	guard, err := lock(selected.EquipmentKey)
	if err != nil {
		return nil, device.ErrDataCleanupPending
	}
	keep := false
	defer func() {
		if !keep {
			_ = guard.Close()
		}
	}()
	current, err := j.Database.AppSetting(ctx, key)
	if err != nil || !bytes.Equal(current.Value, raw) || originalProcessDead(*selected) != nil {
		return nil, device.ErrDataCleanupPending
	}
	if pending, err := j.Database.HasHostIMSClaim(ctx, "host.ims.claim.private.v1."+selected.EquipmentKey); err != nil || pending {
		return nil, device.ErrDataCleanupPending
	}
	keep = true
	return &Orphan{claim: &claim{database: j.Database, key: key, raw: raw, guard: guard}, original: *selected}, nil
}

func validUSBGeneration(value string) bool {
	bus, dev, ok := strings.Cut(value, ":")
	return ok && digits(bus, 1, 8) && digits(dev, 1, 8)
}

func dataHash(value string) bool { return len(value) == 64 && digitsHex(value) }
func digitsHex(value string) bool {
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (o *Orphan) RecoveryAttempted() bool { return o.original.RecoveryAttempted }

func (o *Orphan) Matches(c modem.Candidate) bool {
	r := o.original
	return !o.closed.Load() && PhysicalKey(c) == r.PhysicalKey && c.USBPath == r.USBPath && c.USBGeneration == r.Generation && c.NetworkInterface == r.Interface
}

func (o *Orphan) VerifyIdentity(c modem.Candidate, equipment, iccid, imsi string) error {
	if !o.Matches(c) || !digits(equipment, 15, 15) || !digits(iccid, 10, 22) || !digits(imsi, 10, 18) || digest("imei:"+equipment) != o.original.EquipmentKey || digest(iccid+"\x00"+imsi) != o.original.SIMKey || originalProcessDead(o.original) != nil {
		return device.ErrDataCleanupPending
	}
	return nil
}

// BeginRecovery consumes the one durable reset intent before any AT write.
// A lost commit reply is never permission to issue or replay the command.
func (o *Orphan) BeginRecovery(ctx context.Context, c modem.Candidate, equipment, iccid, imsi string) (bool, error) {
	o.claim.mu.Lock()
	defer o.claim.mu.Unlock()
	if o.claim.released || o.VerifyIdentity(c, equipment, iccid, imsi) != nil {
		return false, device.ErrDataCleanupPending
	}
	if o.original.RecoveryAttempted {
		return false, nil
	}
	if pending, err := o.claim.database.HasHostIMSClaim(ctx, "host.ims.claim.private.v1."+o.original.EquipmentKey); err != nil || pending {
		return false, device.ErrDataCleanupPending
	}
	nextRecord := o.original
	nextRecord.RecoveryAttempted = true
	next, err := json.Marshal(nextRecord)
	if err != nil {
		return false, err
	}
	ok, err := o.claim.database.CompareDataBearerClaim(ctx, o.claim.key, o.claim.raw, next)
	if err != nil || !ok {
		if stored, readErr := o.claim.database.AppSetting(ctx, o.claim.key); readErr == nil && bytes.Equal(stored.Value, next) {
			o.claim.raw, o.original = next, nextRecord
		}
		return false, device.ErrDataCleanupPending
	}
	o.claim.raw, o.original = next, nextRecord
	return true, nil
}

// OriginalGone observes sysfs directly, not the result of an AT discovery
// probe that may temporarily omit a busy or inhibited original device.
func (o *Orphan) OriginalGone() (bool, error) {
	if o.closed.Load() || originalProcessDead(o.original) != nil {
		return false, device.ErrDataCleanupPending
	}
	if _, err := os.Stat(o.original.USBPath); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	bus, err := os.ReadFile(filepath.Join(o.original.USBPath, "busnum"))
	if err != nil {
		return false, err
	}
	dev, err := os.ReadFile(filepath.Join(o.original.USBPath, "devnum"))
	if err != nil {
		return false, err
	}
	current := strings.TrimSpace(string(bus)) + ":" + strings.TrimSpace(string(dev))
	if !digits(strings.TrimSpace(string(bus)), 1, 8) || !digits(strings.TrimSpace(string(dev)), 1, 8) {
		return false, device.ErrDataCleanupPending
	}
	return current != o.original.Generation, nil
}

func (o *Orphan) Complete(ctx context.Context) error {
	if gone, err := o.OriginalGone(); err != nil || !gone {
		return device.ErrDataCleanupPending
	}
	if err := device.CleanupOrphanDataHost(ctx, *o.original.Host); err != nil {
		return err
	}
	// Recheck the irreversible physical proof before the exact original CAS.
	if gone, err := o.OriginalGone(); err != nil || !gone {
		return device.ErrDataCleanupPending
	}
	if err := o.claim.Release(ctx); err != nil {
		return err
	}
	o.closed.Store(true)
	return nil
}

// Close relinquishes only this recovery process's flock. The durable fence
// survives cancellation, another crash, and uncertain reset submission.
func (o *Orphan) Close() error {
	o.claim.mu.Lock()
	defer o.claim.mu.Unlock()
	if o.closed.Swap(true) {
		return nil
	}
	return o.claim.guard.Close()
}

var _ io.Closer = (*Orphan)(nil)
