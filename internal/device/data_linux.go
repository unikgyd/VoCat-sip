//go:build linux

package device

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"

	"vocat/internal/hostims"
	"vocat/internal/modem"
)

const (
	qmiDataOpenTimeout   = 15 * time.Second
	qmiDataStatusTimeout = 12 * time.Second
	qmiDataStopTimeout   = 20 * time.Second
	qmiDataStartTimeout  = 60 * time.Second
	qmiDataEventTimeout  = 5 * time.Second
)

type productionQMIDataSession struct {
	client                *qmi.Client
	wds                   *qmi.WDSService
	uim                   *qmi.UIMService
	packetStatusEvents    chan struct{}
	packetStatusEventOnce sync.Once
}

func (s *productionQMIDataSession) GetIMEI(ctx context.Context) (string, error) {
	dms, err := qmi.NewDMSServiceWithContext(ctx, s.client)
	if err != nil {
		return "", err
	}
	info, err := dms.GetDeviceSerialNumbers(ctx)
	if err != nil {
		return "", err
	}
	return info.IMEI, nil
}

func (s *productionQMIDataSession) GetICCID(ctx context.Context) (string, error) {
	if s.uim == nil {
		var err error
		s.uim, err = qmi.NewUIMServiceWithContext(ctx, s.client)
		if err != nil {
			return "", err
		}
	}
	return s.uim.GetICCID(ctx)
}

func (s *productionQMIDataSession) GetIMSI(ctx context.Context) (string, error) {
	if s.uim == nil {
		if _, err := s.GetICCID(ctx); err != nil {
			return "", err
		}
	}
	return s.uim.GetIMSI(ctx)
}

func openQMIDataSession(ctx context.Context, controlDevice string) (qmiDataSession, error) {
	openContext, cancel := context.WithTimeout(ctx, qmiDataOpenTimeout)
	defer cancel()
	opts := sharedSIMClientOptions()
	opts.DefaultRequestTimeout = qmiDataStartTimeout
	opts.ProxyExecutable = qmiProxyExecutable()
	opts.Logf = func(qmi.ClientLogLevel, string, ...any) {}
	client, err := qmi.NewClientWithOptions(openContext, controlDevice, opts)
	if err != nil {
		return nil, fmt.Errorf("open QMI WDS transport: %w", err)
	}
	wds, err := qmi.NewWDSServiceWithContext(openContext, client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("allocate QMI WDS client: %w", err)
	}
	return &productionQMIDataSession{
		client:             client,
		wds:                wds,
		packetStatusEvents: make(chan struct{}, 1),
	}, nil
}

func qmiProxyExecutable() string {
	if path, err := exec.LookPath("qmi-proxy"); err == nil {
		return path
	}
	for _, path := range []string{"/usr/libexec/qmi-proxy", "/usr/lib/qmi-proxy", "/usr/lib/libqmi-glib/qmi-proxy"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	// Keep the library's conventional path in the resulting diagnostic.
	return "/usr/libexec/qmi-proxy"
}

// enableQMIDataEvents is best-effort. Some modem firmware accepts WDS event
// registration but never emits indications, while other firmware rejects the
// request entirely. The periodic NetworkStatus monitor remains authoritative
// in both cases.
func (manager *Manager) enableQMIDataEvents(ctx context.Context, state *managedDevice, deviceID string, session qmiDataSession) {
	source, ok := session.(qmiDataEventSource)
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	registrationContext, cancelRegistration := context.WithTimeout(ctx, qmiDataEventTimeout)
	err := source.RegisterPacketStatusEvents(registrationContext)
	cancelRegistration()
	if err != nil {
		if manager.logger != nil {
			manager.logger.Warn("QMI packet status event registration failed; periodic monitor remains active", "device_id", deviceID, "error", err)
		}
		return
	}
	events := source.PacketStatusEvents()
	if events == nil {
		return
	}
	if manager.logger != nil {
		manager.logger.Info("registered QMI packet status events", "device_id", deviceID)
	}
	if state.dataEventCancel != nil {
		state.dataEventCancel()
	}
	watchContext, cancelWatch := context.WithCancel(context.Background())
	state.dataEventCancel = cancelWatch
	go func() {
		for {
			select {
			case <-watchContext.Done():
				return
			case _, ok := <-events:
				if !ok {
					// A closed event stream means the QMI client disappeared;
					// wake the reconciler so it can confirm the live state now.
					manager.publishNetworkStatusEvent(deviceID)
					return
				}
				manager.publishNetworkStatusEvent(deviceID)
			}
		}
	}()
}

func disableQMIDataEvents(state *managedDevice) {
	if state == nil || state.dataEventCancel == nil {
		return
	}
	state.dataEventCancel()
	state.dataEventCancel = nil
}

func (session *productionQMIDataSession) Start(ctx context.Context, apn, username, password string, authType, ipFamily uint8) (uint32, error) {
	return session.wds.StartNetworkInterface(ctx, apn, username, password, authType, ipFamily)
}

func (session *productionQMIDataSession) PrepareFamily(ctx context.Context, family uint8) error {
	return session.wds.SetIPFamilyPreference(ctx, family)
}

func (session *productionQMIDataSession) Stop(ctx context.Context, handle uint32) error {
	return session.wds.StopNetworkInterface(ctx, handle)
}

func (session *productionQMIDataSession) StopAny(ctx context.Context, disableAutoconnect bool) error {
	return session.wds.StopAnyNetworkInterface(ctx, disableAutoconnect)
}

func (session *productionQMIDataSession) RegisterPacketStatusEvents(ctx context.Context) error {
	if session == nil || session.wds == nil || session.client == nil {
		return errors.New("QMI data session is closed")
	}
	if err := session.wds.RegisterEventReport(ctx); err != nil {
		return err
	}
	session.packetStatusEventOnce.Do(func() {
		go func() {
			defer close(session.packetStatusEvents)
			for event := range session.client.Events() {
				if event.Type != qmi.EventPacketServiceStatusChanged {
					continue
				}
				select {
				case session.packetStatusEvents <- struct{}{}:
				default:
					// Coalesce bursts; the consumer always performs a fresh
					// NetworkStatus query before deciding whether to recover.
				}
			}
		}()
	})
	return nil
}

func (session *productionQMIDataSession) PacketStatusEvents() <-chan struct{} {
	if session == nil {
		return nil
	}
	return session.packetStatusEvents
}

func (session *productionQMIDataSession) Connected(ctx context.Context) (bool, error) {
	status, err := session.wds.GetPacketServiceStatus(ctx)
	return status == qmi.StatusConnected || status == qmi.StatusSuspended, err
}

func (session *productionQMIDataSession) RawIP(ctx context.Context) (bool, error) {
	wda, err := qmi.NewWDAServiceWithContext(ctx, session.client)
	if err != nil {
		return false, err
	}
	defer wda.Close()
	format, err := wda.GetDataFormat(ctx)
	if err != nil {
		return false, err
	}
	return format.LinkProtocol == qmi.LinkProtocolIP, nil
}

func (session *productionQMIDataSession) SetRawIP(ctx context.Context) error {
	wda, err := qmi.NewWDAServiceWithContext(ctx, session.client)
	if err != nil {
		return err
	}
	defer wda.Close()
	return wda.SetDataFormat(ctx, qmi.DataFormat{
		LinkProtocol:      qmi.LinkProtocolIP,
		UlDataAggregation: uint32(qmi.DataFormatUlDataAggDisabled),
		DlDataAggregation: uint32(qmi.DataFormatDlDataAggDisabled),
	})
}

func (session *productionQMIDataSession) RuntimeIPv4(ctx context.Context) (qmiIPv4Settings, error) {
	settings, err := session.wds.GetRuntimeSettings(ctx, 4)
	if err != nil {
		return qmiIPv4Settings{}, err
	}
	address := settings.IPv4Address.To4()
	ones, bits := settings.IPv4Subnet.Size()
	if address == nil || address.IsUnspecified() || bits != 32 || ones < 0 {
		return qmiIPv4Settings{}, errors.New("QMI WDS returned no valid IPv4 address/subnet")
	}
	result := qmiIPv4Settings{Address: address.String(), Prefix: ones, MTU: settings.MTU}
	if gateway := settings.IPv4Gateway.To4(); gateway != nil && !gateway.IsUnspecified() {
		result.Gateway = gateway.String()
	}
	for _, server := range []net.IP{settings.IPv4DNS1, settings.IPv4DNS2} {
		if server = server.To4(); server != nil && !server.IsUnspecified() {
			result.DNS = append(result.DNS, server.String())
		}
	}
	return result, nil
}

func (session *productionQMIDataSession) RuntimeIPv6(ctx context.Context) (qmiIPv6Settings, error) {
	settings, err := session.wds.GetRuntimeSettings(ctx, 6)
	if err != nil {
		return qmiIPv6Settings{}, err
	}
	address := settings.IPv6Address
	if address == nil || address.To4() != nil || !address.IsGlobalUnicast() || address.IsLinkLocalUnicast() || settings.IPv6Prefix < 1 || settings.IPv6Prefix > 128 {
		return qmiIPv6Settings{}, errors.New("QMI WDS returned no valid global IPv6 address/prefix")
	}
	result := qmiIPv6Settings{Address: address.String(), Prefix: settings.IPv6Prefix, MTU: settings.MTU}
	if gateway := settings.IPv6Gateway; gateway != nil && !gateway.IsUnspecified() {
		if gateway.To4() != nil || gateway.IsMulticast() {
			return qmiIPv6Settings{}, errors.New("QMI WDS returned an invalid IPv6 gateway")
		}
		result.Gateway = gateway.String()
	}
	for _, server := range []net.IP{settings.IPv6DNS1, settings.IPv6DNS2} {
		if server != nil && server.To4() == nil && !server.IsUnspecified() && !server.IsMulticast() {
			result.DNS = append(result.DNS, server.String())
		}
	}
	return result, nil
}

// IMSRuntime is a read-only capability for the forthcoming Host IMS owner.
// It must be called under the same lifecycle gate as this session's WDS CID.
// A decoded address is not proof that host routes, SIM or bearer are ready.
func (session *productionQMIDataSession) IMSRuntime(ctx context.Context) (hostims.Runtime, error) {
	if session == nil || session.client == nil || session.wds == nil {
		return hostims.Runtime{}, hostims.ErrRuntime
	}
	return hostims.ReadRuntime(ctx, session.client, session.wds.ClientID())
}

func (session *productionQMIDataSession) Close() error {
	if session == nil {
		return nil
	}
	var closeErrors []error
	if session.uim != nil {
		closeErrors = append(closeErrors, session.uim.Close())
		session.uim = nil
	}
	if session.wds != nil {
		closeErrors = append(closeErrors, session.wds.Close())
		session.wds = nil
	}
	if session.client != nil {
		closeErrors = append(closeErrors, session.client.Close())
		session.client = nil
	}
	return errors.Join(closeErrors...)
}

// NetworkStatus observes the current QMI call instead of inferring it from the
// last SetNetwork result. TryLock keeps an overview refresh from waiting behind
// a long start/stop transaction; callers can retain the in-progress phase in
// that case.
func (manager *Manager) NetworkStatus(ctx context.Context, id string) (NetworkStatus, error) {
	state, err := manager.lookup(id)
	if err != nil {
		return NetworkStatus{}, err
	}
	if !state.dataMu.TryLock() {
		return NetworkStatus{}, ErrDataOperationInProgress
	}
	defer state.dataMu.Unlock()
	if err := manager.validateActive(id, state); err != nil {
		return NetworkStatus{}, err
	}
	candidate := manager.candidateFor(state)
	status := NetworkStatus{Backend: "qmi", Interface: candidate.NetworkInterface}
	if manager.dataNetwork != nil && state.dataOwner == nil && (candidate.QMIControl != "" || manager.dataNetwork.Admitted(id)) {
		if err := manager.requireNoPendingDataClaim(ctx, candidate); err != nil && !manager.dataNetwork.Owns(candidate) {
			return status, err
		}
		remote, err := manager.dataNetwork.Status(ctx, id)
		if remote.Interface == "" {
			remote.Interface = candidate.NetworkInterface
		}
		return remote, err
	}
	if candidate.QMIControl == "" || candidate.NetworkInterface == "" {
		return status, ErrDataBackendUnavailable
	}
	if state.dataSession == nil || state.dataSessionControl != candidate.QMIControl {
		status.Detail = "no process-owned QMI data session"
		return status, nil
	}
	statusContext, cancel := context.WithTimeout(ctx, qmiDataStatusTimeout)
	connected, err := state.dataSession.Connected(statusContext)
	cancel()
	if err != nil {
		return status, fmt.Errorf("query QMI packet service status: %w", err)
	}
	if !connected {
		status.Detail = "QMI packet data session is disconnected"
		return status, nil
	}
	if complete, ok := state.dataSession.(interface{ AllConnected(context.Context) error }); ok {
		check, cancel := context.WithTimeout(ctx, qmiDataStatusTimeout)
		err := complete.AllConnected(check)
		cancel()
		if err != nil {
			return status, fmt.Errorf("dual-stack packet service is incomplete: %w", err)
		}
	}
	rawContext, cancelRaw := context.WithTimeout(ctx, qmiDataStatusTimeout)
	modemRaw, rawErr := state.dataSession.RawIP(rawContext)
	cancelRaw()
	if rawErr != nil {
		return status, fmt.Errorf("query QMI data format: %w", rawErr)
	}
	if err := validateQMIDataFormat(candidate.NetworkInterface, modemRaw); err != nil {
		status.Detail = err.Error()
		return status, nil
	}
	family := uint8(4)
	if state.dataOwner != nil && state.dataOwner.ipFamily != 0 {
		family = state.dataOwner.ipFamily
	}
	if err := qmiDataHostReadyForFamily(ctx, candidate.NetworkInterface, family); err != nil {
		status.Detail = err.Error()
		return status, nil
	}
	status.Connected = true
	status.Detail = "QMI packet data session and host route are ready"
	return status, nil
}

func qmiRawIPSysfsPath(networkInterface string) (string, error) {
	clean := strings.TrimSpace(networkInterface)
	if clean == "" || filepath.Base(clean) != clean || clean == "." {
		return "", fmt.Errorf("invalid cellular interface %q", networkInterface)
	}
	return filepath.Join("/sys/class/net", clean, "qmi", "raw_ip"), nil
}

func readKernelRawIP(path string) (supported, enabled bool, err error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	value := strings.TrimSpace(string(content))
	return true, strings.EqualFold(value, "Y") || value == "1", nil
}

func validateQMIDataFormat(networkInterface string, modemRaw bool) error {
	path, err := qmiRawIPSysfsPath(networkInterface)
	if err != nil {
		return err
	}
	supported, kernelRaw, err := readKernelRawIP(path)
	if err != nil {
		return fmt.Errorf("read kernel QMI data format: %w", err)
	}
	if !supported {
		if modemRaw {
			return errors.New("modem uses Raw-IP but the kernel interface cannot enable Raw-IP")
		}
		return nil
	}
	if modemRaw != kernelRaw {
		return fmt.Errorf("QMI data format mismatch: modem raw_ip=%t, kernel raw_ip=%t", modemRaw, kernelRaw)
	}
	return nil
}

// prepareQMIDataFormat restores the link-layer setup formerly performed by
// qmi-network. qmi_wwan exposes a raw_ip switch; when present, both WDA and the
// kernel must use Raw-IP or the host emits ARP frames which never enter WDS.
func prepareQMIDataFormat(
	ctx context.Context,
	session qmiDataSession,
	ipCommand string,
	networkInterface string,
	rawIPPath string,
) error {
	supported, kernelRaw, err := readKernelRawIP(rawIPPath)
	if err != nil {
		return fmt.Errorf("read kernel QMI data format: %w", err)
	}
	formatContext, cancelFormat := context.WithTimeout(ctx, qmiDataStatusTimeout)
	modemRaw, err := session.RawIP(formatContext)
	cancelFormat()
	if err != nil {
		return fmt.Errorf("query modem QMI data format: %w", err)
	}
	if !supported {
		if modemRaw {
			return errors.New("modem uses Raw-IP but the kernel interface cannot enable Raw-IP")
		}
		return nil
	}
	if kernelRaw && modemRaw {
		return nil
	}

	// Changing the framing of a live interface is unsafe. Remove every host
	// path first so a partial failure remains fail-closed.
	clearExportProxyRoute(ctx, networkInterface)
	_, _ = exec.CommandContext(ctx, ipCommand, "-4", "addr", "flush", "dev", networkInterface, "scope", "global").CombinedOutput()
	downOutput, downErr := exec.CommandContext(ctx, ipCommand, "link", "set", "dev", networkInterface, "down").CombinedOutput()
	if downErr != nil {
		return fmt.Errorf("set %s down before QMI data-format change: %w: %s", networkInterface, downErr, strings.TrimSpace(string(downOutput)))
	}
	if !modemRaw {
		setContext, cancelSet := context.WithTimeout(ctx, qmiDataStatusTimeout)
		err = session.SetRawIP(setContext)
		cancelSet()
		if err != nil {
			return fmt.Errorf("set modem QMI data format to Raw-IP: %w", err)
		}
	}
	if !kernelRaw {
		if err := os.WriteFile(rawIPPath, []byte("Y\n"), 0o644); err != nil {
			return fmt.Errorf("set kernel QMI data format to Raw-IP: %w", err)
		}
	}
	supported, kernelRaw, err = readKernelRawIP(rawIPPath)
	if err != nil {
		return fmt.Errorf("kernel QMI Raw-IP verification failed: %w", err)
	}
	if !supported || !kernelRaw {
		return fmt.Errorf("kernel QMI Raw-IP verification failed: supported=%t enabled=%t", supported, kernelRaw)
	}
	verifyContext, cancelVerify := context.WithTimeout(ctx, qmiDataStatusTimeout)
	modemRaw, err = session.RawIP(verifyContext)
	cancelVerify()
	if err != nil {
		return fmt.Errorf("modem QMI Raw-IP verification failed: enabled=%t: %w", modemRaw, err)
	}
	if !modemRaw {
		return fmt.Errorf("modem QMI Raw-IP verification failed: enabled=%t", modemRaw)
	}
	return nil
}

func qmiDataHostReady(ctx context.Context, networkInterface string) error {
	return qmiDataHostReadyForFamily(ctx, networkInterface, 4)
}

func qmiDataHostReadyForFamily(ctx context.Context, networkInterface string, family uint8) error {
	interfaceInfo, err := net.InterfaceByName(networkInterface)
	if err != nil {
		return fmt.Errorf("cellular interface is unavailable: %w", err)
	}
	addresses, err := interfaceInfo.Addrs()
	if err != nil {
		return fmt.Errorf("read cellular interface addresses: %w", err)
	}
	hasIPv4, hasIPv6 := false, false
	for _, value := range addresses {
		address, _, parseErr := net.ParseCIDR(value.String())
		if parseErr == nil && address.To4() != nil && !address.IsUnspecified() {
			hasIPv4 = true
		}
		if parseErr == nil && address.To4() == nil && address.IsGlobalUnicast() && !address.IsLinkLocalUnicast() {
			hasIPv6 = true
		}
	}
	if family != 6 && !hasIPv4 || family != 4 && !hasIPv6 {
		return errors.New("cellular interface lacks a requested IP-family address")
	}
	ipCommand, err := exec.LookPath("ip")
	if err != nil {
		return fmt.Errorf("check cellular policy route: %w", err)
	}
	_, table, _ := exportProxyRouteIdentity(networkInterface)
	for _, flag := range dataFamilyFlags(family) {
		output, err := exec.CommandContext(ctx, ipCommand, flag, "route", "show", "table", strconv.Itoa(table), "default", "dev", networkInterface).CombinedOutput()
		if err != nil {
			return fmt.Errorf("check cellular policy route: %w: %s", err, strings.TrimSpace(string(output)))
		}
		if strings.TrimSpace(string(output)) == "" {
			return errors.New("cellular policy route is missing")
		}
	}
	return nil
}

func legacyQMINetworkStatePath(controlDevice string) string {
	name := filepath.Base(filepath.Clean(controlDevice))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return ""
	}
	return filepath.Join(os.TempDir(), "qmi-network-state-"+name)
}

func removeLegacyQMINetworkState(candidate modem.Candidate) {
	if path := legacyQMINetworkStatePath(candidate.QMIControl); path != "" {
		_ = os.Remove(path)
	}
}

// invalidateQMINetworkSession is called immediately before a radio/SIM reset.
// Closing the transport forgets the old WDS client and handle without waiting
// for a modem transaction which the reset is about to invalidate anyway.
func invalidateQMINetworkSession(state *managedDevice, candidate modem.Candidate) {
	removeLegacyQMINetworkState(candidate)
	if state == nil {
		return
	}
	disableQMIDataEvents(state)
	if state.dataSession == nil {
		return
	}
	_ = state.dataSession.Close()
	state.dataSession = nil
	state.dataSessionHandle = 0
	state.dataSessionControl = ""
}

func stoppedQMIDataError(err error) bool {
	if err == nil {
		return true
	}
	var outOfCall *qmi.OutOfCallError
	if errors.As(err, &outOfCall) {
		return true
	}
	protocolError := qmi.GetQMIError(err)
	if protocolError == nil {
		return false
	}
	switch protocolError.ErrorCode {
	case qmi.QMIErrInvalidID, qmi.QMIErrNoEffect, qmi.QMIErrOutOfCall:
		return true
	default:
		return false
	}
}

func qmiDataIPFamily(ipVersion string) uint8 {
	if ipVersion == "IPV6" {
		return 6
	}
	if ipVersion == "IPV4V6" {
		return qmiDataDualFamily
	}
	return 4
}

func qmiDataRequestKey(apn, ipVersion, user, password, auth string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%q|%q|%q|%q|%q", apn, ipVersion, user, password, auth))))
}

func closeQMIDataSession(state *managedDevice) {
	disableQMIDataEvents(state)
	if state.dataSession != nil {
		_ = state.dataSession.Close()
	}
	state.dataSession = nil
	state.dataSessionHandle = 0
	state.dataSessionControl = ""
}

func rollbackQMIDataHost(state *managedDevice, candidate modem.Candidate, ipCommand string) {
	rollbackContext, cancel := context.WithTimeout(context.Background(), managerCommandCleanupTimeout)
	defer cancel()
	if owner := state.dataOwner; owner != nil && (owner.guard != nil && owner.guard.binding.iccid != "" || owner.ipFamily != 0 && owner.ipFamily != 4) {
		// Keep an uncertain original WDS capability for coordinated cleanup.
		current := owner.guard.manager.ownedNetworkPhysicalCurrent(state, owner)
		cleanupErr := clearOwnedQMIHost(rollbackContext, owner, current)
		if !current && cleanupErr == nil {
			closeQMIDataSession(state)
			owner.packetStopped = true
			_ = owner.guard.manager.releaseDataOwner(rollbackContext, state)
			return
		}
		if state.dataSession != nil {
			stop := func() error {
				if owned, ok := state.dataSession.(qmiOwnedDataStopper); ok {
					return owned.StopOwned(rollbackContext)
				}
				if owner.handleKnown {
					return state.dataSession.Stop(rollbackContext, state.dataSessionHandle)
				}
				return ErrDataCleanupPending
			}
			if err := stop(); stoppedQMIDataError(err) && cleanupErr == nil {
				closeQMIDataSession(state)
				owner.packetStopped = true
				_ = owner.guard.manager.releaseDataOwner(rollbackContext, state)
			}
		}
		return
	}
	clearExportProxyRoute(rollbackContext, candidate.NetworkInterface)
	if state.dataSession != nil {
		_ = state.dataSession.Stop(rollbackContext, state.dataSessionHandle)
	}
	closeQMIDataSession(state)
	_, _ = exec.CommandContext(rollbackContext, ipCommand, "-4", "addr", "flush", "dev", candidate.NetworkInterface, "scope", "global").CombinedOutput()
	_, _ = exec.CommandContext(rollbackContext, ipCommand, "link", "set", "dev", candidate.NetworkInterface, "down").CombinedOutput()
}

func (manager *Manager) stopQMIDataSession(ctx context.Context, state *managedDevice, candidate modem.Candidate) error {
	removeLegacyQMINetworkState(candidate)
	if state.dataSession != nil && state.dataSessionControl == candidate.QMIControl {
		stopContext, cancel := context.WithTimeout(ctx, qmiDataStopTimeout)
		err := state.dataSession.Stop(stopContext, state.dataSessionHandle)
		cancel()
		closeQMIDataSession(state)
		if stoppedQMIDataError(err) {
			return nil
		}
		return err
	}
	closeQMIDataSession(state)
	openContext, cancelOpen := context.WithTimeout(ctx, qmiDataOpenTimeout)
	session, err := manager.qmiDataOpener(openContext, candidate.QMIControl)
	cancelOpen()
	if err != nil {
		return err
	}
	defer session.Close()
	stopContext, cancelStop := context.WithTimeout(ctx, qmiDataStopTimeout)
	err = session.StopAny(stopContext, true)
	cancelStop()
	if stoppedQMIDataError(err) {
		return nil
	}
	return err
}

func (manager *Manager) setQMINetwork(
	ctx context.Context,
	state *managedDevice,
	candidate modem.Candidate,
	enabled bool,
	apn string,
	ipVersion string,
	username string,
	password string,
	authentication string,
) (NetworkResult, error) {
	guard, err := manager.newSIMWriteGuard(ctx, candidate)
	if err != nil {
		return NetworkResult{}, err
	}
	scoped := guard != nil && guard.binding.iccid != ""
	if state.dataOwner == nil {
		if err := manager.requireNoPendingDataClaim(ctx, candidate); err != nil {
			return NetworkResult{}, err
		}
	}
	if !enabled && (scoped || state.dataOwner != nil && state.dataOwner.ipFamily != 0 && state.dataOwner.ipFamily != 4) {
		if state.dataOwner == nil {
			if state.dataSession != nil {
				return NetworkResult{}, ErrDataCleanupPending
			}
			if err := manager.requireNoPendingDataClaim(ctx, candidate); err != nil {
				return NetworkResult{}, err
			}
			return NetworkResult{Backend: "qmi", Interface: candidate.NetworkInterface, Detail: "no process-owned packet data session"}, nil
		}
		if state.dataOwner.backend != "qmi" {
			return NetworkResult{}, ErrDataCleanupPending
		}
		err := manager.stopOwnedQMINetwork(ctx, state, state.dataOwner)
		return NetworkResult{Backend: "qmi", Interface: candidate.NetworkInterface, Detail: "process-owned packet data session released"}, err
	}
	ipCommand, err := exec.LookPath("ip")
	if err != nil {
		return NetworkResult{}, fmt.Errorf("%w: install iproute2 to control %s", ErrDataBackendUnavailable, candidate.NetworkInterface)
	}

	// Disabling is fail-closed: remove every host path before asking the modem
	// to release WDS. Even if the baseband session is stale or unresponsive, no
	// application can continue routing traffic through the cellular interface.
	if !enabled {
		cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		clearExportProxyRoute(cleanupContext, candidate.NetworkInterface)
		_, _ = exec.CommandContext(cleanupContext, ipCommand, "-4", "addr", "flush", "dev", candidate.NetworkInterface, "scope", "global").CombinedOutput()
		_, _ = exec.CommandContext(cleanupContext, ipCommand, "link", "set", "dev", candidate.NetworkInterface, "down").CombinedOutput()
		cancelCleanup()
		err := manager.stopQMIDataSession(ctx, state, candidate)
		if err != nil {
			return NetworkResult{}, fmt.Errorf("stop QMI data session: %w", err)
		}
		state.dataOwner = nil
		return NetworkResult{
			Enabled: false, Backend: "qmi", Interface: candidate.NetworkInterface,
			ControlDevice: candidate.QMIControl, APN: apn, IPVersion: ipVersion,
			Detail: "packet data session stopped",
		}, nil
	}
	if owner := state.dataOwner; owner != nil && owner.radioStopped && manager.ownedNetworkPhysicalCurrent(state, owner) {
		if err := manager.stopOwnedQMINetwork(ctx, state, owner); err != nil {
			return NetworkResult{}, err
		}
	}
	requestKey := qmiDataRequestKey(apn, ipVersion, username, password, authentication)
	if owner := state.dataOwner; owner != nil && owner.requestKey != "" && owner.requestKey != requestKey {
		if err := manager.stopOwnedQMINetwork(ctx, state, owner); err != nil {
			return NetworkResult{}, err
		}
	}
	if owner := state.dataOwner; owner != nil && owner.packetStopped {
		if err := manager.stopOwnedQMINetwork(ctx, state, owner); err != nil {
			return NetworkResult{}, err
		}
	}
	if state.dataOwner != nil && state.dataSession == nil {
		return NetworkResult{}, ErrDataCleanupPending
	}
	if scoped && state.dataSession != nil && state.dataOwner == nil {
		return NetworkResult{}, ErrDataCleanupPending
	}
	if owner := state.dataOwner; owner != nil {
		if owner.backend != "qmi" || !sameSIMWritePhysical(owner.candidate, candidate) || scoped && (owner.guard == nil || owner.guard.binding.iccid != guard.binding.iccid) {
			return NetworkResult{}, ErrDataCleanupPending
		}
	}

	removeLegacyQMINetworkState(candidate)
	if state.dataSession != nil && state.dataSessionControl == candidate.QMIControl {
		if err := guard.verify(ctx, state.dataSession, true); err != nil {
			return NetworkResult{}, err
		}
		rawIPPath, pathErr := qmiRawIPSysfsPath(candidate.NetworkInterface)
		if pathErr != nil {
			return NetworkResult{}, pathErr
		}
		if formatErr := prepareQMIDataFormat(ctx, state.dataSession, ipCommand, candidate.NetworkInterface, rawIPPath); formatErr != nil {
			rollbackQMIDataHost(state, candidate, ipCommand)
			return NetworkResult{}, fmt.Errorf("prepare QMI data format: %w", formatErr)
		}
		statusContext, cancel := context.WithTimeout(ctx, qmiDataStatusTimeout)
		connected, statusErr := state.dataSession.Connected(statusContext)
		cancel()
		if statusErr != nil && (scoped || state.dataOwner != nil && state.dataOwner.ipFamily != 0 && state.dataOwner.ipFamily != 4) {
			return NetworkResult{}, errors.Join(ErrDataCleanupPending, statusErr)
		}
		if statusErr == nil && connected {
			return configureQMIDataHost(ctx, state, candidate, ipCommand, apn, ipVersion, "packet data session already connected")
		}
		if owner := state.dataOwner; owner != nil && (scoped || owner.ipFamily != 0 && owner.ipFamily != 4) {
			if err := manager.stopOwnedQMINetwork(ctx, state, owner); err != nil {
				return NetworkResult{}, err
			}
		} else {
			closeQMIDataSession(state)
			state.dataOwner = nil
		}
	}

	// A fresh transport cannot recover an earlier process's WDS capability.
	// SIM-bound and multi-family requests below only observe packet status and
	// reject an existing call; they never reclaim it through StopAny or a cached
	// CID/PDH. Seeing another client's runtime settings does not grant ownership.
	openContext, cancelOpen := context.WithTimeout(ctx, qmiDataOpenTimeout)
	session, err := manager.qmiDataOpener(openContext, candidate.QMIControl)
	cancelOpen()
	if err != nil {
		return NetworkResult{}, fmt.Errorf("open QMI data session: %w", err)
	}
	if err := guard.verify(ctx, session, true); err != nil {
		_ = session.Close()
		return NetworkResult{}, err
	}
	var equipment string
	if scoped && manager.dataClaimJournal != nil {
		reader, ok := session.(interface {
			GetIMEI(context.Context) (string, error)
		})
		if !ok {
			_ = session.Close()
			return NetworkResult{}, ErrDataCleanupPending
		}
		equipment, err = reader.GetIMEI(ctx)
		if err != nil {
			_ = session.Close()
			return NetworkResult{}, ErrDataCleanupPending
		}
	}
	family := qmiDataIPFamily(ipVersion)
	retained := scoped || family != 4
	if family != 4 {
		if err := validateIPv6DataHostClaim(candidate.NetworkInterface, nil); err != nil {
			_ = session.Close()
			return NetworkResult{}, err
		}
	}
	if prepare, ok := session.(qmiDataFamilyPreparer); ok {
		firstFamily := family
		if firstFamily == qmiDataDualFamily {
			firstFamily = 4
		}
		if err := prepare.PrepareFamily(ctx, firstFamily); err != nil {
			_ = session.Close()
			return NetworkResult{}, err
		}
	}
	if family == qmiDataDualFamily {
		second, err := manager.qmiDataOpener(ctx, candidate.QMIControl)
		if err != nil {
			_ = session.Close()
			return NetworkResult{}, err
		}
		if prepare, ok := second.(qmiDataFamilyPreparer); ok {
			err = prepare.PrepareFamily(ctx, 6)
		}
		if err == nil {
			err = guard.verify(ctx, second, true)
		}
		if err != nil {
			_ = second.Close()
			_ = session.Close()
			return NetworkResult{}, err
		}
		session = &dualQMIDataSession{parts: [2]qmiDataSession{session, second}, verify: func(ctx context.Context, part qmiDataSession) error { return guard.verify(ctx, part, true) }}
	}
	manager.enableQMIDataEvents(ctx, state, candidate.ID, session)
	var stopErr error
	if retained {
		// A fresh CID does not prove ownership of a modem-wide packet call.
		connected, observeErr := session.Connected(ctx)
		if observeErr != nil || connected {
			stopErr = ErrDataCleanupPending
		}
	} else {
		stopContext, cancelStop := context.WithTimeout(ctx, qmiDataStopTimeout)
		stopErr = session.StopAny(stopContext, true)
		cancelStop()
	}
	if !stoppedQMIDataError(stopErr) {
		disableQMIDataEvents(state)
		_ = session.Close()
		return NetworkResult{}, fmt.Errorf("reclaim stale QMI data session: %w", stopErr)
	}
	if retained {
		state.dataOwner = newOwnedQMINetwork(&ownedNetwork{candidate: copyCandidate(candidate), backend: "qmi", guard: guard, hostTouched: true, ipFamily: family, requestKey: requestKey})
		if scoped && manager.dataClaimJournal != nil {
			claim, err := manager.dataClaimJournal.Acquire(ctx, candidate, equipment, guard.binding.iccid, guard.binding.imsi)
			if err != nil {
				state.dataOwner = nil
				disableQMIDataEvents(state)
				_ = session.Close()
				return NetworkResult{}, ErrDataCleanupPending
			}
			state.dataOwner.claim = claim
			manager.retainDataClaim(candidate, claim)
		}
		state.dataSession, state.dataSessionControl = session, candidate.QMIControl
		state.dataSessionHandle = 0
	}
	rawIPPath, pathErr := qmiRawIPSysfsPath(candidate.NetworkInterface)
	if pathErr != nil {
		disableQMIDataEvents(state)
		if !retained {
			_ = session.Close()
		}
		return NetworkResult{}, pathErr
	}
	if formatErr := prepareQMIDataFormat(ctx, session, ipCommand, candidate.NetworkInterface, rawIPPath); formatErr != nil {
		disableQMIDataEvents(state)
		if !retained {
			_ = session.Close()
		}
		return NetworkResult{}, fmt.Errorf("prepare QMI data format: %w", formatErr)
	}
	if err := guard.verify(ctx, session, true); err != nil {
		disableQMIDataEvents(state)
		if !retained {
			_ = session.Close()
		}
		return NetworkResult{}, err
	}
	if !retained {
		state.dataOwner = newOwnedQMINetwork(&ownedNetwork{candidate: copyCandidate(candidate), backend: "qmi", guard: guard, hostTouched: true, ipFamily: family, requestKey: requestKey})
	}
	state.dataSession, state.dataSessionControl = session, candidate.QMIControl
	state.dataSessionHandle = 0
	authType := map[string]uint8{"NONE": 0, "PAP": 1, "CHAP": 2, "PAP_OR_CHAP": 3}[authentication]
	startContext, cancelStart := context.WithTimeout(ctx, qmiDataStartTimeout)
	handle, err := session.Start(startContext, apn, username, password, authType, qmiDataIPFamily(ipVersion))
	cancelStart()
	if err != nil {
		disableQMIDataEvents(state)
		if !retained {
			closeQMIDataSession(state)
			state.dataOwner = nil
		}
		return NetworkResult{}, fmt.Errorf("start QMI data session: %w", err)
	}
	state.dataSession = session
	state.dataSessionHandle = handle
	state.dataSessionControl = candidate.QMIControl
	state.dataOwner.handleKnown = handle != 0 && handle != ^uint32(0)
	if retained {
		if !state.dataOwner.handleKnown {
			return NetworkResult{}, ErrDataCleanupPending
		}
		if err := guard.verify(ctx, session, true); err != nil {
			return NetworkResult{}, err
		}
	}
	return configureQMIDataHost(ctx, state, candidate, ipCommand, apn, ipVersion, fmt.Sprintf("packet data handle %d", handle))
}

func configureQMIDataHost(ctx context.Context, state *managedDevice, candidate modem.Candidate, ipCommand, apn, ipVersion, detail string) (NetworkResult, error) {
	if ipVersion == "IPV6" || ipVersion == "IPV4V6" {
		return configureQMIIPv6Host(ctx, state, candidate, ipCommand, apn, ipVersion, detail)
	}
	// The modem-side call and the host-side lease have independent lifetimes.
	// A daemon/modem restart can leave an address from the previous packet call
	// on the interface, so DHCP must always begin from a clean host generation.
	clearExportProxyRoute(ctx, candidate.NetworkInterface)
	flushOutput, flushErr := exec.CommandContext(ctx, ipCommand, "-4", "addr", "flush", "dev", candidate.NetworkInterface, "scope", "global").CombinedOutput()
	if flushErr != nil {
		rollbackQMIDataHost(state, candidate, ipCommand)
		return NetworkResult{}, fmt.Errorf("clear stale addresses on %s: %w: %s", candidate.NetworkInterface, flushErr, strings.TrimSpace(string(flushOutput)))
	}
	linkOutput, linkErr := exec.CommandContext(ctx, ipCommand, "link", "set", "dev", candidate.NetworkInterface, "up").CombinedOutput()
	if linkErr != nil {
		rollbackQMIDataHost(state, candidate, ipCommand)
		return NetworkResult{}, fmt.Errorf("set %s up: %w: %s", candidate.NetworkInterface, linkErr, strings.TrimSpace(string(linkOutput)))
	}
	runtimeContext, cancelRuntime := context.WithTimeout(ctx, qmiDataStatusTimeout)
	runtimeSettings, runtimeErr := state.dataSession.RuntimeIPv4(runtimeContext)
	cancelRuntime()
	if runtimeErr == nil {
		if runtimeSettings.MTU >= 576 && runtimeSettings.MTU <= 65535 {
			mtuOutput, mtuErr := exec.CommandContext(ctx, ipCommand, "link", "set", "dev", candidate.NetworkInterface, "mtu", strconv.Itoa(runtimeSettings.MTU)).CombinedOutput()
			if mtuErr != nil {
				rollbackQMIDataHost(state, candidate, ipCommand)
				return NetworkResult{}, fmt.Errorf("set %s MTU: %w: %s", candidate.NetworkInterface, mtuErr, strings.TrimSpace(string(mtuOutput)))
			}
		}
		runtimeDetail, configureErr := configureExportProxyIPv4(
			ctx, ipCommand, candidate.NetworkInterface,
			runtimeSettings.Address, runtimeSettings.Prefix,
			runtimeSettings.Gateway, runtimeSettings.DNS,
		)
		if configureErr != nil {
			rollbackQMIDataHost(state, candidate, ipCommand)
			return NetworkResult{}, fmt.Errorf("QMI session started but WDS runtime configuration failed: %w", configureErr)
		}
		return NetworkResult{
			Enabled: true, Backend: "qmi", Interface: candidate.NetworkInterface,
			ControlDevice: candidate.QMIControl, APN: apn, IPVersion: ipVersion,
			Detail: strings.TrimSpace(detail + "\n" + runtimeDetail),
		}, nil
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		rollbackQMIDataHost(state, candidate, ipCommand)
		return NetworkResult{}, fmt.Errorf("%w: WDS runtime settings unavailable (%v) and busybox udhcpc is required for %s", ErrDataBackendUnavailable, runtimeErr, candidate.NetworkInterface)
	}
	dhcpDetail, err := configureExportProxyDHCP(ctx, busybox, ipCommand, candidate.NetworkInterface)
	if err != nil {
		rollbackQMIDataHost(state, candidate, ipCommand)
		return NetworkResult{}, fmt.Errorf("QMI session started but WDS runtime settings were unavailable (%v) and protected DHCP failed: %w", runtimeErr, err)
	}
	return NetworkResult{
		Enabled:       true,
		Backend:       "qmi",
		Interface:     candidate.NetworkInterface,
		ControlDevice: candidate.QMIControl,
		APN:           apn,
		IPVersion:     ipVersion,
		Detail:        strings.TrimSpace(detail + "\n" + dhcpDetail),
	}, nil
}

// exportProxyRouteIdentity must stay in sync with the Export Proxy plugin's
// Linux socket mark. Unmarked host traffic never sees the cellular default
// route; only plugin sockets carrying this mark are policy-routed to it.
func exportProxyRouteIdentity(networkInterface string) (mark uint32, table, priority int) {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(networkInterface))
	value := hash.Sum32()
	mark = 0x56000000 | (value & 0x00ffffff)
	table = 20000 + int(value%10000)
	priority = 20000 + int(value%10000)
	return
}

func configureExportProxyDHCP(ctx context.Context, busybox, ipCommand, networkInterface string) (string, error) {
	lease, err := os.CreateTemp("", "vocat-dhcp-lease-*.env")
	if err != nil {
		return "", err
	}
	leasePath := lease.Name()
	_ = lease.Close()
	_ = os.Remove(leasePath)
	defer os.Remove(leasePath)
	script, err := os.CreateTemp("", "vocat-udhcpc-*.sh")
	if err != nil {
		return "", err
	}
	scriptPath := script.Name()
	defer os.Remove(scriptPath)
	scriptText := fmt.Sprintf(`#!/bin/sh
case "$1" in
  bound|renew)
    (umask 077; printf 'ip=%%s\nsubnet=%%s\nrouter=%%s\ndns=%%s\n' "$ip" "$subnet" "$router" "$dns" > %q)
    ;;
esac
exit 0
`, leasePath)
	if _, err := script.WriteString(scriptText); err != nil {
		_ = script.Close()
		return "", err
	}
	if err := script.Chmod(0o700); err != nil {
		_ = script.Close()
		return "", err
	}
	if err := script.Close(); err != nil {
		return "", err
	}
	output, err := exec.CommandContext(ctx, busybox, "udhcpc", "-q", "-n", "-t", "5", "-T", "3", "-i", networkInterface, "-s", scriptPath).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(output)), "address family not supported") {
			return "", fmt.Errorf("udhcpc cannot open its link-layer socket: allow AF_PACKET in the vocat systemd service RestrictAddressFamilies setting: %w", err)
		}
		return "", fmt.Errorf("udhcpc: %w: %s", err, strings.TrimSpace(string(output)))
	}
	raw, err := os.ReadFile(leasePath)
	if err != nil {
		return "", fmt.Errorf("read DHCP lease: %w", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	address := net.ParseIP(values["ip"]).To4()
	maskIP := net.ParseIP(values["subnet"]).To4()
	if address == nil || maskIP == nil {
		return "", errors.New("DHCP returned no valid IPv4 address/subnet")
	}
	mask := net.IPMask(maskIP)
	ones, bits := mask.Size()
	if bits != 32 || ones < 0 {
		return "", errors.New("DHCP returned an invalid IPv4 subnet")
	}
	routers := strings.Fields(values["router"])
	if len(routers) > 0 && net.ParseIP(routers[0]).To4() == nil {
		return "", errors.New("DHCP returned an invalid IPv4 gateway")
	}
	gateway := ""
	if len(routers) > 0 {
		gateway = routers[0]
	}
	return configureExportProxyIPv4(ctx, ipCommand, networkInterface, address.String(), ones, gateway, strings.Fields(values["dns"]))
}

func configureExportProxyIPv4(ctx context.Context, ipCommand, networkInterface, addressText string, prefix int, gatewayText string, dns []string) (string, error) {
	address := net.ParseIP(addressText).To4()
	if address == nil || prefix < 0 || prefix > 32 {
		return "", errors.New("no valid IPv4 address/subnet was provided")
	}
	mask := net.CIDRMask(prefix, 32)
	network := address.Mask(mask)
	gateway := net.ParseIP(gatewayText).To4()
	if gatewayText != "" && gateway == nil {
		return "", errors.New("an invalid IPv4 gateway was provided")
	}
	if result, addrErr := exec.CommandContext(ctx, ipCommand, "-4", "addr", "replace", fmt.Sprintf("%s/%d", address.String(), prefix), "dev", networkInterface).CombinedOutput(); addrErr != nil {
		return "", fmt.Errorf("configure cellular address: %w: %s", addrErr, strings.TrimSpace(string(result)))
	}
	mark, table, priority := exportProxyRouteIdentity(networkInterface)
	clearExportProxyRoute(ctx, networkInterface)
	connectedCIDR := fmt.Sprintf("%s/%d", network.String(), prefix)
	if result, routeErr := exec.CommandContext(ctx, ipCommand, "-4", "route", "replace", "table", strconv.Itoa(table), connectedCIDR, "dev", networkInterface, "scope", "link", "src", address.String()).CombinedOutput(); routeErr != nil {
		clearExportProxyRoute(ctx, networkInterface)
		return "", fmt.Errorf("install protected connected route: %w: %s", routeErr, strings.TrimSpace(string(result)))
	}
	defaultArgs := []string{"-4", "route", "replace", "table", strconv.Itoa(table), "default"}
	if gateway != nil {
		defaultArgs = append(defaultArgs, "via", gateway.String())
	}
	defaultArgs = append(defaultArgs, "dev", networkInterface, "onlink")
	if result, routeErr := exec.CommandContext(ctx, ipCommand, defaultArgs...).CombinedOutput(); routeErr != nil {
		clearExportProxyRoute(ctx, networkInterface)
		return "", fmt.Errorf("install protected default route: %w: %s", routeErr, strings.TrimSpace(string(result)))
	}
	markText := fmt.Sprintf("0x%x", mark)
	result, err := exec.CommandContext(ctx, ipCommand, "rule", "add", "priority", strconv.Itoa(priority), "fwmark", markText, "lookup", strconv.Itoa(table)).CombinedOutput()
	if err != nil {
		clearExportProxyRoute(ctx, networkInterface)
		return "", fmt.Errorf("install protected routing rule: %w: %s", err, strings.TrimSpace(string(result)))
	}
	if err := writeExportProxyDNS(networkInterface, dns); err != nil {
		clearExportProxyRoute(ctx, networkInterface)
		return "", fmt.Errorf("publish protected DNS configuration: %w", err)
	}
	return fmt.Sprintf("protected IPv4 configuration %s/%d", address.String(), prefix), nil
}

func exportProxyDNSPath(networkInterface string) string {
	safeName := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			return character
		}
		return '_'
	}, networkInterface)
	return "/run/vocat/cellular-" + safeName + ".dns"
}

func writeExportProxyDNS(networkInterface string, servers []string) error {
	valid := make([]string, 0, len(servers))
	for _, server := range servers {
		if address := net.ParseIP(server); address != nil {
			valid = append(valid, address.String())
		}
	}
	if len(valid) == 0 {
		// This is used only by marked Export Proxy sockets. It never changes the
		// host resolver and is merely a fallback for carriers omitting DHCP DNS.
		valid = []string{"1.1.1.1", "8.8.8.8"}
	}
	if err := os.MkdirAll("/run/vocat", 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp("/run/vocat", ".cellular-dns-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.WriteString(strings.Join(valid, "\n") + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, exportProxyDNSPath(networkInterface))
}

func clearExportProxyRoute(ctx context.Context, networkInterface string) {
	_ = os.Remove(exportProxyDNSPath(networkInterface))
	ipCommand, err := exec.LookPath("ip")
	if err != nil {
		return
	}
	mark, table, priority := exportProxyRouteIdentity(networkInterface)
	_, _ = exec.CommandContext(ctx, ipCommand, "rule", "del", "priority", strconv.Itoa(priority), "fwmark", fmt.Sprintf("0x%x", mark), "lookup", strconv.Itoa(table)).CombinedOutput()
	_, _ = exec.CommandContext(ctx, ipCommand, "-4", "route", "flush", "table", strconv.Itoa(table), "dev", networkInterface).CombinedOutput()
}

const managerCommandCleanupTimeout = 15 * time.Second
