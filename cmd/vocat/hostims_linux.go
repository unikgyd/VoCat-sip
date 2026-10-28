//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
	"vocat/internal/vowifisettings"
)

type linuxHostIMSFactory struct {
	opts                          hostIMSFactoryOptions
	mu                            sync.Mutex
	lifetime                      context.Context
	cancel                        context.CancelFunc
	closing, closed, initializing bool
	initDone                      chan struct{}
	initErr                       error
	connection                    *dbus.Conn
	pool                          *hostims.BearerPool
	preparing                     int
	held                          map[*factoryHostIMSAccess]struct{}
	recoveries                    map[string]*factoryHostIMSAccess
}

// Construction is inert: neither system D-Bus nor QMI nor an IMS PDN is opened.
// The returned factory must outlive its Manager, including failed Stop retries.
func buildHostIMSFactory(ctx context.Context, opts hostIMSFactoryOptions) (hostIMSRuntimeFactory, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if opts.Store == nil || opts.Devices == nil || opts.IMSConfig == nil || opts.Authorize == nil {
		return nil, errHostIMSFactory
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &linuxHostIMSFactory{opts: opts, lifetime: lifetime, cancel: cancel, held: make(map[*factoryHostIMSAccess]struct{})}, nil
}

func (f *linuxHostIMSFactory) Factory(ctx context.Context, id string) (*hostimsruntime.Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f.mu.Lock()
	stopped := f.closing || f.closed
	f.mu.Unlock()
	if stopped {
		return nil, errHostIMSFactory
	}
	config, err := f.opts.Store.Device(ctx, strings.TrimSpace(id))
	if err != nil || config.ID != id {
		return nil, errHostIMSFactory
	}
	imsConfig, err := f.opts.IMSConfig(ctx, config)
	if err != nil {
		return nil, errHostIMSFactory
	}
	return hostimsruntime.NewRuntime(hostimsruntime.Config{DeviceID: id, IMS: imsConfig, Diagnostic: func(stage string, cause error) {
		if f.opts.Logger != nil {
			f.opts.Logger.Warn("Host IMS operation failed", "device_id", id, "stage", stage, "reason", hostims.PreparationFailureCode(cause))
		}
	}, Prepare: func(prepare context.Context) (hostimsruntime.Access, error) {
		return f.prepare(prepare, id)
	}})
}

func (f *linuxHostIMSFactory) initialize(done chan struct{}) {
	// Never attach the connection lifetime to a Start/request deadline: a late
	// QMI/MM cleanup may still need it after that request has returned.
	connection, err := dbus.ConnectSystemBus(dbus.WithContext(f.lifetime))
	var pool *hostims.BearerPool
	if err == nil {
		backend, backendErr := hostims.NewModemManagerBackend(connection)
		err = backendErr
		if err == nil {
			pool, err = hostims.NewBearerPool(backend, hostims.SQLiteClaimJournal{Database: f.opts.Store}, func(ctx context.Context, selection hostims.Selection) error {
				imsi := selection.SubscriberIMSI()
				if !hostIMSDigits(imsi, 14, 16) || device.IsPlaceholderIMSI(imsi) || device.RegionBlockReason(imsi) != "" {
					return hostims.ErrDenied
				}
				if f.opts.Authorize(ctx, selection) != nil {
					return hostims.ErrDenied
				}
				return nil
			})
		}
	}
	f.mu.Lock()
	if err != nil || f.closing || f.closed {
		f.initErr = hostims.ErrTransport
		f.mu.Unlock()
		if connection != nil {
			_ = connection.Close()
		}
		f.mu.Lock()
	} else {
		f.connection, f.pool, f.initErr = connection, pool, nil
	}
	f.initializing = false
	close(done)
	f.mu.Unlock()
}

func (f *linuxHostIMSFactory) sharedPool(ctx context.Context) (*hostims.BearerPool, error) {
	f.mu.Lock()
	if f.closing || f.closed {
		f.mu.Unlock()
		return nil, errHostIMSFactory
	}
	if f.pool != nil {
		pool := f.pool
		f.mu.Unlock()
		return pool, nil
	}
	if f.initDone == nil || !f.initializing && f.initErr != nil {
		f.initDone, f.initializing, f.initErr = make(chan struct{}), true, nil
		go f.initialize(f.initDone)
	}
	done := f.initDone
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing || f.closed || f.initErr != nil || f.pool == nil {
		return nil, hostims.ErrTransport
	}
	return f.pool, nil
}

func canonicalHostIMSPhysical(value string) (string, error) {
	if value == "" {
		return "", errHostIMSFactory
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil || !strings.HasPrefix(resolved, "/sys/devices/") || filepath.Clean(resolved) != resolved {
		return "", errHostIMSFactory
	}
	return resolved, nil
}

// A legacy refresh guard must use exactly the strong inventory mapping used
// by preparation, never ATMapper's advisory port/ID score.
func hostIMSInventoryPhysicalID(config store.Device, entries []device.Device) (string, error) {
	entry, _, err := resolveHostIMSInventory(config, entries, canonicalHostIMSPhysical)
	if err != nil {
		return "", err
	}
	return entry.ID, nil
}

func hostIMSPhysicalTarget(config store.Device, entries []device.Device) (hostims.ModemTarget, error) {
	entry, physical, err := resolveHostIMSInventory(config, entries, canonicalHostIMSPhysical)
	if err != nil {
		return hostims.ModemTarget{}, err
	}
	var stat unix.Stat_t
	if unix.Lstat(entry.Candidate.QMIControl, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		return hostims.ModemTarget{}, errHostIMSFactory
	}
	charTarget, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/char/%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev)))
	if err != nil || !strings.HasPrefix(charTarget, physical+"/") {
		return hostims.ModemTarget{}, errHostIMSFactory
	}
	if generation := entry.Candidate.USBGeneration; generation != "" {
		bus, busErr := os.ReadFile(filepath.Join(physical, "busnum"))
		number, numberErr := os.ReadFile(filepath.Join(physical, "devnum"))
		if busErr != nil || numberErr != nil || strings.TrimSpace(string(bus))+":"+strings.TrimSpace(string(number)) != generation {
			return hostims.ModemTarget{}, hostims.ErrChanged
		}
	}
	target := hostims.ModemTarget{Equipment: config.ModemIMEI, Physical: physical, QMIControl: entry.Candidate.QMIControl}
	if s := entry.Snapshot; s != nil {
		target.Equipment, target.ICCID, target.IMSI = s.IMEI, s.ICCID, s.IMSI
	}
	return target, nil
}

func (f *linuxHostIMSFactory) prepare(ctx context.Context, id string) (hostimsruntime.Access, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f.mu.Lock()
	if f.closing || f.closed {
		f.mu.Unlock()
		return nil, errHostIMSFactory
	}
	f.preparing++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.preparing--; f.mu.Unlock() }()
	config, err := f.opts.Store.Device(ctx, id)
	if err != nil {
		return nil, errHostIMSFactory
	}
	if !hostIMSModeEnabled(config) {
		return nil, hostims.ErrDenied
	}
	target, err := hostIMSPhysicalTarget(config, hostIMSMetadataInventory(config, f.opts.Devices.List()))
	if err != nil {
		return nil, err
	}
	// AT snapshots are intentionally not refreshed while MM owns this modem.
	// Pin fresh SIM identity below; only hardware anchors come from inventory.
	target.ICCID, target.IMSI = "", ""
	pool, err := f.sharedPool(ctx)
	if err != nil {
		return nil, err
	}
	_ = pool.SyncDeviceInhibition(ctx, map[string]bool{target.Physical: false})
	selection, err := pool.SelectTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	if config.Telephony != nil && config.Telephony.ICCID != selection.SubscriberICCID() {
		return nil, hostims.ErrChanged
	}
	if owner, err := f.recoverSelected(ctx, id, pool, selection); err != nil {
		return owner, errors.Join(err, hostimsruntime.ErrCleanup)
	}
	if _, err := hostims.ProbeQMIEndpoint(ctx, selection); err != nil {
		return nil, err
	}
	// Refuse a SIM/hardware swap between inventory resolution and the final MM
	// selection. Acquire independently revalidates its pinned daemon binding.
	currentConfig, configErr := f.opts.Store.Device(ctx, id)
	if configErr != nil || !sameHostIMSPrepareConfig(config, currentConfig) {
		return nil, hostims.ErrChanged
	}
	again, err := hostIMSPhysicalTarget(currentConfig, hostIMSMetadataInventory(currentConfig, f.opts.Devices.List()))
	again.ICCID, again.IMSI = "", ""
	if err != nil || again != target {
		return nil, hostims.ErrChanged
	}
	options := hostims.BearerOptions{APN: vowifisettings.IMSAPN(ctx, f.opts.Store), Timeout: time.Minute}
	if config.Telephony != nil {
		options.ProfileID = config.Telephony.HostIMSProfileID
	}
	if card, cardErr := f.opts.Store.CardPolicy(ctx, selection.SubscriberICCID()); cardErr == nil {
		options.IPFamily, err = hostIMSCardIPFamily(card.IPVersion)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(cardErr, store.ErrNotFound) {
		return nil, cardErr
	}
	if f.opts.BearerOptions != nil {
		options, err = f.opts.BearerOptions(ctx, config)
		if err != nil {
			return nil, hostims.ErrDenied
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	latestConfig, configErr := f.opts.Store.Device(ctx, id)
	if configErr != nil || !sameHostIMSPrepareConfig(config, latestConfig) {
		return nil, hostims.ErrChanged
	}
	lease, pendingOwner, err := f.acquireBearer(ctx, selection, options, pool.Acquire, func(s hostims.Selection) (hostIMSCleanup, error) {
		pending, err := pool.PendingCleanup(s)
		if pending == nil {
			return nil, err
		}
		return pending, err
	})
	if err != nil {
		return pendingOwner, err
	}
	owner := &factoryHostIMSAccess{lease: lease, factory: f}
	f.mu.Lock()
	f.held[owner] = struct{}{}
	f.mu.Unlock()
	access, prepareErr := hostims.PrepareAccess(ctx, lease)
	if prepareErr != nil && f.opts.Logger != nil {
		f.opts.Logger.Warn("Host IMS preparation failed", "device_id", id, "stage", "prepare_access", "reason", hostims.PreparationFailureCode(prepareErr))
	}
	owner.access = access
	if access != nil {
		return owner, prepareErr
	}
	// A failed pre-construction check still leaves our acquired PDN to release.
	// If that cleanup is uncertain, return a real non-nil cleanup owner instead
	// of abandoning it behind a nil Access/error result.
	cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if closeErr := owner.Close(cleanup); closeErr != nil {
		return owner, errors.Join(hostimsruntime.ErrPrepare, hostimsruntime.ErrCleanup)
	}
	if prepareErr == nil {
		prepareErr = hostimsruntime.ErrPrepare
	}
	return nil, prepareErr
}

func hostIMSCardIPFamily(version string) (uint32, error) {
	switch strings.ToUpper(strings.TrimSpace(version)) {
	case "":
		return 0, nil
	case "IP", "IPV4":
		return 1, nil
	case "IPV6":
		return 2, nil
	case "IPV4V6":
		return 4, nil
	default:
		return 0, hostims.ErrParameters
	}
}

// Subscription reads ModemManager metadata only. Cached SIM identifiers must
// not constrain selection when detecting a replacement card. Equipment, sysfs
// and QMI endpoint identity remain mandatory; no bearer or QMI client is opened.
func (f *linuxHostIMSFactory) Subscription(ctx context.Context, id string) (vowifi.SIMIdentity, error) {
	config, err := f.opts.Store.Device(ctx, id)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	entries := hostIMSMetadataInventory(config, f.opts.Devices.List())
	target, err := hostIMSPhysicalTarget(config, entries)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	target.ICCID, target.IMSI = "", ""
	pool, err := f.sharedPool(ctx)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	_ = pool.SyncDeviceInhibition(ctx, map[string]bool{target.Physical: false})
	iccid, imsi, err := pool.Subscription(ctx, target)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	return vowifi.SIMIdentity{ICCID: iccid, IMSI: imsi}, nil
}

func (f *linuxHostIMSFactory) ObserveRadio(ctx context.Context, id string) (device.Snapshot, error) {
	config, err := f.opts.Store.Device(ctx, id)
	if err != nil {
		return device.Snapshot{}, err
	}
	entries := hostIMSMetadataInventory(config, f.opts.Devices.List())
	target, err := hostIMSPhysicalTarget(config, entries)
	if err != nil {
		return device.Snapshot{}, err
	}
	target.ICCID, target.IMSI = "", ""
	pool, err := f.sharedPool(ctx)
	if err != nil {
		return device.Snapshot{}, err
	}
	_ = pool.SyncDeviceInhibition(ctx, map[string]bool{target.Physical: false})
	radio, err := pool.ObserveRadio(ctx, target)
	if err != nil {
		return device.Snapshot{}, err
	}
	operatorName := strings.TrimSpace(radio.OperatorName)
	if radio.OperatorCode != "" {
		if carrierName, _, found := device.CarrierForPLMN(radio.OperatorCode); found && strings.TrimSpace(carrierName) != "" {
			operatorName = strings.TrimSpace(carrierName)
		}
	}
	simStatus := "not_inserted"
	simReady := false
	if radio.ICCID != "" {
		simStatus = "ready"
		simReady = true
	}
	return device.Snapshot{
		DeviceID:           id,
		Responsive:         true,
		Manufacturer:       radio.Manufacturer,
		Model:              radio.Model,
		Firmware:           radio.Firmware,
		IMEI:               radio.IMEI,
		IMSI:               radio.IMSI,
		ICCID:              radio.ICCID,
		SIMStatus:          simStatus,
		SIMReady:           simReady,
		SignalPercent:      radio.SignalPercent,
		RSSIDBm:            radio.RSSIDBm,
		OperatorName:       operatorName,
		OperatorCode:       radio.OperatorCode,
		AccessTech:         radio.AccessTech,
		RegistrationStatus: radio.RegistrationStatus,
		RegistrationSource: "ModemManager",
		PSAttached:         radio.PSAttached,
		OperatingMode:      radio.OperatingMode,
		ModeKnown:          radio.ModeKnown,
		FlightMode:         radio.FlightMode,
		RadioOff:           radio.FlightMode,
		UpdatedAt:          time.Now().UTC(),
	}, nil
}

func (f *linuxHostIMSFactory) ReconcileDeviceInhibition(ctx context.Context) error {
	if f == nil || f.opts.Store == nil || f.opts.Devices == nil {
		return errHostIMSFactory
	}
	entries := f.opts.Devices.List()
	desired := make(map[string]bool)
	guard := defaultHostIMSPhysicalGuard()
	for _, entry := range entries {
		if !entry.Discovered || entry.Candidate.HardwareKind == "pcsc" {
			continue
		}
		physical, err := canonicalHostIMSPhysical(entry.Candidate.USBPath)
		if err != nil {
			continue
		}
		ownedByHostIMS := hostIMSOwnsPhysicalWith(ctx, f.opts.Store, entries, entry.ID, guard)
		desired[physical] = !ownedByHostIMS
	}
	if configs, err := f.opts.Store.ListDevices(ctx); err == nil {
		for _, config := range configs {
			physical, err := canonicalHostIMSPhysical(config.USBPath)
			if err != nil {
				continue
			}
			if store.HostIMSConfigured(config) {
				desired[physical] = false
			} else if _, exists := desired[physical]; !exists {
				desired[physical] = true
			}
		}
	}
	if len(desired) == 0 {
		return nil
	}
	pool, err := f.sharedPool(ctx)
	if err != nil {
		return err
	}
	return pool.SyncDeviceInhibition(ctx, desired)
}

type hostIMSCleanup interface{ Close(context.Context) error }
type failedHostIMSPendingQuery struct{}

func (failedHostIMSPendingQuery) Close(context.Context) error { return hostims.ErrUncertain }

// An Acquire error can still own a submitted mutation or a known object whose
// cleanup failed. Hand that exact line's cleanup capability to Runtime; a nil
// Access would let Stop falsely ACK and allow legacy control-plane reuse.
func (f *linuxHostIMSFactory) acquireBearer(ctx context.Context, selection hostims.Selection, options hostims.BearerOptions,
	acquire func(context.Context, hostims.Selection, hostims.BearerOptions) (*hostims.BearerLease, error),
	pending func(hostims.Selection) (hostIMSCleanup, error),
) (*hostims.BearerLease, hostimsruntime.Access, error) {
	lease, err := acquire(ctx, selection, options)
	if err == nil {
		return lease, nil, nil
	}
	cleanup, queryErr := pending(selection)
	if queryErr != nil && cleanup == nil {
		cleanup = failedHostIMSPendingQuery{}
	}
	if cleanup == nil {
		return nil, nil, err
	}
	owner := &factoryHostIMSAccess{lease: cleanup, factory: f}
	f.mu.Lock()
	f.held[owner] = struct{}{}
	f.mu.Unlock()
	return nil, owner, errors.Join(err, hostimsruntime.ErrCleanup)
}

func (f *linuxHostIMSFactory) Close(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closing = true
	if f.preparing != 0 || len(f.held) != 0 {
		return hostims.ErrBusy
	}
	if f.pool != nil && f.pool.HasPendingClaims() {
		return hostims.ErrUncertain
	}
	// With no lease/worker that could need MM, cancel an inert/late initializer
	// too. It checks closing and closes any connection it subsequently obtains.
	f.cancel()
	if f.connection != nil {
		if err := f.connection.Close(); err != nil {
			return hostims.ErrTransport
		}
	}
	f.closed = true
	return nil
}

type factoryHostIMSAccess struct {
	access     *hostims.PreparedAccess
	lease      interface{ Close(context.Context) error }
	factory    *linuxHostIMSFactory
	released   sync.Once
	recoveryID string
}

func (a *factoryHostIMSAccess) Close(ctx context.Context) error {
	var err error
	if a.access != nil {
		err = a.access.Close(ctx)
	} else {
		err = a.lease.Close(ctx)
	}
	if err == nil {
		a.released.Do(func() {
			a.factory.mu.Lock()
			delete(a.factory.held, a)
			if a.factory.recoveries[a.recoveryID] == a {
				delete(a.factory.recoveries, a.recoveryID)
			}
			a.factory.mu.Unlock()
		})
	}
	return err
}
func (a *factoryHostIMSAccess) CardIdentity() hostims.CardIdentity {
	if a.access == nil {
		return hostims.CardIdentity{}
	}
	return a.access.CardIdentity()
}
func (a *factoryHostIMSAccess) VerificationTimeout() time.Duration { return 20 * time.Second }
func (a *factoryHostIMSAccess) Evidence() vowifi.CellularIMSEvidence {
	if a.access == nil {
		return vowifi.CellularIMSEvidence{}
	}
	return a.access.Evidence()
}
func (a *factoryHostIMSAccess) Verify(ctx context.Context, id vowifi.SIMIdentity) error {
	if a.access == nil {
		return hostims.ErrHostAccessUnavailable
	}
	return a.access.Verify(ctx, id)
}
func (a *factoryHostIMSAccess) CheckReady(ctx context.Context, id vowifi.SIMIdentity) (vowifi.AKAEvidence, error) {
	if a.access == nil {
		return vowifi.AKAEvidence{}, hostims.ErrHostAccessUnavailable
	}
	return a.access.CheckReady(ctx, id)
}
func (a *factoryHostIMSAccess) Authenticate(ctx context.Context, id vowifi.SIMIdentity, c vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	return a.AuthenticateWithPreference(ctx, id, c, "")
}
func (a *factoryHostIMSAccess) AuthenticateWithPreference(ctx context.Context, id vowifi.SIMIdentity, c vowifi.AKAChallenge, preference string) (vowifi.AKAResult, error) {
	if a.access == nil {
		return vowifi.AKAResult{}, hostims.ErrHostAccessUnavailable
	}
	return a.access.AuthenticateWithPreference(ctx, id, c, preference)
}
func (a *factoryHostIMSAccess) BindSocket(ctx context.Context, network, address string, raw syscall.RawConn) error {
	if a.access == nil {
		return hostims.ErrHostAccessUnavailable
	}
	return a.access.BindSocket(ctx, network, address, raw)
}
func (a *factoryHostIMSAccess) AcquireMediaRoute(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
	if a.access == nil {
		return nil, hostims.ErrHostAccessUnavailable
	}
	return a.access.AcquireMediaRoute(ctx, local, remote)
}
func (a *factoryHostIMSAccess) BindProtectedSocket(ctx context.Context, network string, binding imssec.SocketBinding, raw syscall.RawConn) error {
	if a.access == nil {
		return hostims.ErrHostAccessUnavailable
	}
	return a.access.BindProtectedSocket(ctx, network, binding, raw)
}
func (a *factoryHostIMSAccess) InstallIPSec(ctx context.Context, config imssec.Config) (imssec.Handle, error) {
	if a.access == nil {
		return nil, hostims.ErrHostAccessUnavailable
	}
	return a.access.InstallIPSec(ctx, config)
}
func (a *factoryHostIMSAccess) RetainIPSec(handle imssec.Handle) {
	if a.access != nil {
		a.access.RetainIPSec(handle)
	}
}

var _ hostimsruntime.Access = (*factoryHostIMSAccess)(nil)
