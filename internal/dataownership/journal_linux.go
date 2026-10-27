//go:build linux

// Package dataownership persists a fence for an original live QMI capability.
// Records survive process death and never authorize CID/PDH adoption or expiry.
package dataownership

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/modem"
	"vocat/internal/store"
)

type Journal struct {
	Database *store.Store
	lock     func(string) (io.Closer, error) // isolated tests only
}

func digest(value string) string {
	raw := sha256.Sum256([]byte(value))
	return hex.EncodeToString(raw[:])
}
func PhysicalKey(candidate modem.Candidate) string { return digest("physical:" + candidate.ID) }

type record struct {
	Version                                                         int
	EquipmentKey, PhysicalKey, Token, SIMKey, Generation, Interface string
	PID                                                             int
	BootID                                                          string
	RecoveryAttempted                                               bool `json:",omitempty"`
}

func digits(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (j Journal) Acquire(ctx context.Context, candidate modem.Candidate, equipment, iccid, imsi string) (device.DataClaim, error) {
	if j.Database == nil || candidate.ID == "" || candidate.USBGeneration == "" || candidate.NetworkInterface == "" ||
		!digits(equipment, 15, 15) || equipment == "000000000000000" || !digits(iccid, 10, 22) || !digits(imsi, 10, 18) {
		return nil, device.ErrDataCleanupPending
	}
	key := digest("imei:" + equipment)
	lock := j.lock
	if lock == nil {
		lock = (hostims.SQLiteClaimJournal{Database: j.Database}).Lock
	}
	guard, err := lock(key)
	if err != nil {
		return nil, device.ErrDataCleanupPending
	}
	keep := false
	defer func() {
		if !keep {
			_ = guard.Close()
		}
	}()
	pending, err := j.Database.HasHostIMSClaim(ctx, "host.ims.claim.private.v1."+key)
	if err != nil || pending {
		return nil, device.ErrDataCleanupPending
	}
	pending, err = j.Pending(ctx, candidate)
	if err != nil || pending {
		return nil, device.ErrDataCleanupPending
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, device.ErrDataCleanupPending
	}
	raw, err := json.Marshal(record{Version: 1, EquipmentKey: key, PhysicalKey: PhysicalKey(candidate), Token: hex.EncodeToString(nonce[:]), SIMKey: digest(iccid + "\x00" + imsi), Generation: candidate.USBGeneration, Interface: candidate.NetworkInterface, PID: os.Getpid(), BootID: string(bytes.TrimSpace(boot))})
	if err != nil {
		return nil, err
	}
	accepted, err := j.Database.CompareDataBearerClaim(ctx, store.DataBearerClaimPrefix+key, nil, raw)
	if err != nil || !accepted {
		return nil, device.ErrDataCleanupPending
	}
	keep = true
	return &claim{database: j.Database, key: store.DataBearerClaimPrefix + key, raw: raw, guard: guard}, nil
}

func (j Journal) Pending(ctx context.Context, candidate modem.Candidate) (bool, error) {
	if j.Database == nil || candidate.ID == "" {
		return true, device.ErrDataCleanupPending
	}
	return j.Database.HasDataBearerClaim(ctx, "", PhysicalKey(candidate))
}

type claim struct {
	mu       sync.Mutex
	database *store.Store
	key      string
	raw      []byte
	guard    io.Closer
	released bool
}

func (c *claim) BeginRecovery(ctx context.Context, candidate modem.Candidate, equipment, iccid, imsi string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var original record
	if c.released || json.Unmarshal(c.raw, &original) != nil || original.Version != 1 ||
		original.EquipmentKey != digest("imei:"+equipment) || original.PhysicalKey != PhysicalKey(candidate) ||
		original.SIMKey != digest(iccid+"\x00"+imsi) || original.Generation != candidate.USBGeneration ||
		original.Interface != candidate.NetworkInterface || original.PID != os.Getpid() {
		return false, device.ErrDataCleanupPending
	}
	if original.RecoveryAttempted {
		return false, nil
	}
	if pending, err := c.database.HasHostIMSClaim(ctx, "host.ims.claim.private.v1."+original.EquipmentKey); err != nil || pending {
		return false, device.ErrDataCleanupPending
	}
	original.RecoveryAttempted = true
	next, err := json.Marshal(original)
	if err != nil {
		return false, device.ErrDataCleanupPending
	}
	ok, err := c.database.CompareDataBearerClaim(ctx, c.key, c.raw, next)
	if err != nil || !ok {
		// An uncertain commit consumes the intent, but never authorizes a reset.
		// Reconcile only these exact original-owner bytes so later physical
		// disappearance can still release the original claim safely.
		if stored, readErr := c.database.AppSetting(ctx, c.key); readErr == nil && bytes.Equal(stored.Value, next) {
			c.raw = next
		}
		return false, device.ErrDataCleanupPending
	}
	c.raw = next
	return true, nil
}

func (c *claim) Release(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return nil
	}
	ok, err := c.database.CompareDataBearerClaim(ctx, c.key, c.raw, nil)
	if err != nil {
		return device.ErrDataCleanupPending
	}
	if !ok {
		// A previous delete may have committed before the response was lost.
		// Holding the original equipment lock excludes a cooperating successor.
		_, readErr := c.database.AppSetting(ctx, c.key)
		if !errors.Is(readErr, store.ErrNotFound) {
			return device.ErrDataCleanupPending
		}
	}
	if err := c.guard.Close(); err != nil {
		return device.ErrDataCleanupPending
	}
	c.released = true
	return nil
}
