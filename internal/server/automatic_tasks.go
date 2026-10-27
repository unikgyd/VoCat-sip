package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

const (
	automaticTaskPollInterval = 5 * time.Second
	automaticTaskMaxRuntime   = 8 * time.Minute
)

type automaticTaskPayload struct {
	Phone           string `json:"phone,omitempty"`
	Message         string `json:"message,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
}

type automaticTaskExecutionError struct {
	err       error
	retryable bool
}

func (value automaticTaskExecutionError) Error() string { return value.err.Error() }
func (value automaticTaskExecutionError) Unwrap() error { return value.err }

type automaticTaskProgress func(string)

type automaticTaskEnvironmentSnapshot struct {
	config store.Device
	policy store.CardPolicy
}

type automaticTaskScheduler struct {
	server *Server
	ctx    context.Context
	mu     sync.Mutex
	queues map[string]chan store.AutomaticTaskRun
	queued map[int64]bool
}

func (s *Server) StartAutomaticTasks(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	scheduler := &automaticTaskScheduler{server: s, ctx: ctx, queues: make(map[string]chan store.AutomaticTaskRun), queued: make(map[int64]bool)}
	s.automaticTasks = scheduler
	queued, err := s.store.RecoverAutomaticTaskRuns(ctx, time.Now().UTC())
	if err != nil {
		s.logger.Warn("recover automatic tasks", "error", err)
	} else {
		for _, run := range queued {
			scheduler.enqueue(run)
		}
	}
	go scheduler.run()
	go s.recoverAutomaticTaskEnvironments(ctx)
}

func (scheduler *automaticTaskScheduler) run() {
	ticker := time.NewTicker(automaticTaskPollInterval)
	defer ticker.Stop()
	scheduler.claim()
	for {
		select {
		case <-scheduler.ctx.Done():
			return
		case <-ticker.C:
			scheduler.claim()
		}
	}
}

func (scheduler *automaticTaskScheduler) claim() {
	var runs []store.AutomaticTaskRun
	var err error
	if scheduler.server.developerActive(scheduler.ctx) {
		runs, err = scheduler.server.store.ClaimDueAutomaticTasks(scheduler.ctx, time.Now().UTC(), 50)
	} else {
		runs, err = scheduler.server.store.ClaimDueAvailableAutomaticTasks(scheduler.ctx, time.Now().UTC(), 50)
	}
	if err != nil {
		scheduler.server.logger.Warn("claim automatic tasks", "error", err)
		return
	}
	for _, run := range runs {
		scheduler.enqueue(run)
	}
	// A full in-memory queue never loses its durable queued row. Refill on
	// every poll, deduplicating rows that are queued or executing locally.
	queued, err := scheduler.server.store.ListQueuedAutomaticTaskRuns(scheduler.ctx)
	if err == nil {
		for _, run := range queued {
			scheduler.enqueue(run)
		}
	}
}

func (scheduler *automaticTaskScheduler) enqueue(run store.AutomaticTaskRun) {
	deviceID := strings.TrimSpace(run.DeviceID)
	scheduler.mu.Lock()
	if scheduler.queued == nil {
		scheduler.queued = make(map[int64]bool)
	}
	if scheduler.queued[run.ID] {
		scheduler.mu.Unlock()
		return
	}
	queue := scheduler.queues[deviceID]
	if queue == nil {
		queue = make(chan store.AutomaticTaskRun, 100)
		scheduler.queues[deviceID] = queue
		go scheduler.worker(deviceID, queue)
	}
	scheduler.queued[run.ID] = true
	scheduler.mu.Unlock()
	select {
	case queue <- run:
	default:
		scheduler.mu.Lock()
		delete(scheduler.queued, run.ID)
		scheduler.mu.Unlock()
	}
}

func (scheduler *automaticTaskScheduler) worker(deviceID string, queue <-chan store.AutomaticTaskRun) {
	for {
		select {
		case <-scheduler.ctx.Done():
			return
		case run := <-queue:
			// Keep queued status until restoration is positively acknowledged.
			for scheduler.server.store.RequireAutomaticTaskRecovered(scheduler.ctx, deviceID) != nil {
				select {
				case <-scheduler.ctx.Done():
					return
				case <-time.After(automaticTaskPollInterval):
				}
			}
			scheduler.execute(run)
			scheduler.mu.Lock()
			delete(scheduler.queued, run.ID)
			scheduler.mu.Unlock()
		}
	}
}

func (scheduler *automaticTaskScheduler) execute(run store.AutomaticTaskRun) {
	task, err := scheduler.server.store.AutomaticTask(scheduler.ctx, run.TaskID)
	if err != nil {
		run.Status, run.Error, run.FinishedAt = "failed", err.Error(), time.Now().UTC()
		_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
		return
	}
	if err := validateAutomaticTaskAvailability(scheduler.server.developerActive(scheduler.ctx), task.TaskType, task.Environment); err != nil {
		run.Status, run.Error, run.FinishedAt = "failed", err.Error(), time.Now().UTC()
		_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
		return
	}
	run.Status, run.StartedAt = "running", time.Now().UTC()
	_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
	var output string
attempts:
	for attempt := 1; attempt <= task.RetryCount+1; attempt++ {
		if scheduler.ctx.Err() != nil {
			err = scheduler.ctx.Err()
			break
		}
		run.Attempts = attempt
		run.Output = fmt.Sprintf("第 %d 次尝试：正在检查设备和 eSIM Profile", attempt)
		_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
		progress := func(message string) {
			run.Output = fmt.Sprintf("第 %d 次尝试：%s", attempt, message)
			_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
		}
		runtimeLimit := automaticTaskMaxRuntime
		if task.TaskType == "call" {
			var payload automaticTaskPayload
			if json.Unmarshal(task.Payload, &payload) == nil && payload.DurationSeconds > 0 && payload.DurationSeconds <= 600 {
				runtimeLimit += time.Duration(payload.DurationSeconds) * time.Second
			}
		}
		operationContext, cancel := context.WithTimeout(scheduler.ctx, runtimeLimit)
		output, err = scheduler.server.executeAutomaticTaskWhenAvailable(operationContext, task, progress)
		cancel()
		if err == nil {
			break
		}
		var executionError automaticTaskExecutionError
		if errors.As(err, &executionError) && !executionError.retryable {
			break
		}
		if attempt <= task.RetryCount {
			// A device error may contain the full AT command, including APN
			// credentials. The persisted run retains a user-facing outcome; logs
			// contain only non-sensitive execution metadata.
			scheduler.server.logger.Warn("automatic task attempt failed", "task_id", task.ID, "device_id", task.DeviceID, "attempt", attempt)
			select {
			case <-scheduler.ctx.Done():
				err = scheduler.ctx.Err()
				break attempts
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
		}
	}
	run.FinishedAt = time.Now().UTC()
	if err == nil {
		run.Status, run.Output, run.Error = "success", output, ""
	} else {
		run.Status, run.Error = "failed", err.Error()
	}
	if updateErr := scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run); updateErr != nil {
		scheduler.server.logger.Warn("finish automatic task run", "run_id", run.ID, "error", updateErr)
	}
	if task.Notify {
		go scheduler.server.notifyAutomaticTask(context.Background(), task, run)
	}
}

func (s *Server) executeAutomaticTask(ctx context.Context, task store.AutomaticTask, progress automaticTaskProgress) (output string, err error) {
	ctx, release, err := s.beginAutomaticTask(ctx, task.DeviceID)
	if err != nil {
		return "", automaticTaskAdmissionError{err}
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			output = ""
			err = automaticTaskExecutionError{err: errors.Join(err, releaseErr), retryable: false}
		}
	}()
	if progress == nil {
		progress = func(string) {}
	}
	if err := validateAutomaticTaskAvailability(s.developerActive(ctx), task.TaskType, task.Environment); err != nil {
		return "", automaticTaskExecutionError{err: err, retryable: false}
	}
	current, err := s.store.Device(ctx, task.DeviceID)
	if err != nil {
		return "", err
	}
	if err := validateAutomaticTaskDeviceCapabilities(current, task.TaskType, task.Environment); err != nil {
		return "", err
	}
	if err := s.requireAutomaticTaskCallsIdle(ctx, current); err != nil {
		return "", err
	}
	journal, err := s.createAutomaticTaskJournal(ctx, task, current)
	if err != nil {
		return "", err
	}
	ctx = context.WithValue(ctx, automaticTaskJournalKey{}, journal)
	// This finalizer exists before the first stop, RF, data or LPA mutation.
	// Its durable row remains until restoration and all task children finish.
	defer func() {
		progress("正在恢复任务执行前保存的卡策略与接入环境")
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
		defer cancel()
		if restoreErr := journal.restore(cleanup); restoreErr != nil {
			output = ""
			err = automaticTaskExecutionError{err: errors.Join(err, fmt.Errorf("automatic task restoration is pending: %w", restoreErr)), retryable: false}
		}
	}()
	progress("正在释放原接入并核对设备和 SIM")
	config := current
	var entry device.Device
	var physicalID string
	if journal.usesHostTaskMetadata(ctx, current) {
		if err := s.stopTelephonyAccess(ctx, current); err != nil {
			return "", err
		}
		if err := s.quiesceOwnedData(ctx, current); err != nil {
			return "", err
		}
		if _, err := s.hostTaskSubscription(ctx, current.ID, task.ProfileICCID); err != nil {
			return "", err
		}
		entry, physicalID, err = journal.verifyHardware(current)
		if err != nil {
			return "", err
		}
	} else {
		if err := s.enterAutomaticTaskMaintenance(ctx, current); err != nil {
			return "", err
		}
		_, physical, err := journal.verifyHardware(temporaryAutomaticTaskDevice(current))
		if err != nil {
			return "", err
		}
		live, err := s.devices.Refresh(ctx, physical)
		if err != nil || strings.TrimSpace(live.ICCID) != journal.payload.Original.Card.ICCID {
			return "", errors.Join(err, device.ErrSIMObservation)
		}
		if err := s.requireModuleCallsIdle(ctx, temporaryAutomaticTaskDevice(current)); err != nil {
			return "", err
		}
		// Only this task capability may start new work after the old data
		// owner has acknowledged cleanup and the disabled projection is saved.
		s.cellularDataRuntime().resume(current.ID)
		config, entry, physicalID, _, err = s.ensureAutomaticTaskProfile(ctx, task, progress)
		if err != nil {
			return "", err
		}
	}
	s.cellularDataRuntime().resume(current.ID)
	if err := s.prepareAutomaticTaskEnvironment(ctx, &config, entry, physicalID, task, progress); err != nil {
		return "", err
	}
	var payload automaticTaskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode task payload: %w", err)
	}
	// Record an ambiguous submission before dialing/sending, never replay it
	// during restart recovery even if the process dies before the response.
	if err := journal.save(ctx, "executing"); err != nil {
		return "", err
	}
	switch task.TaskType {
	case "sms":
		progress("正在发送短信")
		output, err = s.executeAutomaticSMS(ctx, task, payload)
	case "call":
		progress("正在发起通话")
		output, err = s.executeAutomaticCall(ctx, task, payload)
	case "cellular_attach":
		output = "已注册蜂窝网络，未启用数据连接"
	case "public_ip":
		progress("蜂窝数据已连接，正在查询漫游公网 IP")
		output, err = s.executeAutomaticPublicIP(ctx, config, task.ProfileICCID)
	default:
		return "", fmt.Errorf("unsupported automatic task type %q", task.TaskType)
	}
	return output, err
}

type automaticTaskRevertState struct {
	needed          bool
	modified        bool
	switchAttempted bool
	physicalID      string
	initialICCID    string
	initialAID      string
	snapshot        automaticTaskEnvironmentSnapshot
}

func (s *Server) ensureAutomaticTaskProfile(ctx context.Context, task store.AutomaticTask, progress automaticTaskProgress) (config store.Device, entry device.Device, physicalID string, revertState automaticTaskRevertState, err error) {
	var recovery automaticTaskRevertState
	defer func() {
		if err != nil && recovery.modified {
			config, physicalID, revertState = recovery.snapshot.config, recovery.physicalID, recovery
		}
	}()
	config, err = s.store.Device(ctx, task.DeviceID)
	if err != nil {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("read device: %w", err)
	}
	if store.HostIMSConfigured(config) {
		// These legacy tasks explicitly switch profiles/radio/data policies.
		// They cannot silently replace a selected Host IMS owner's transport.
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, errors.New("switch to module or VoWiFi mode before using legacy automatic radio/profile tasks")
	}
	if err := validateAutomaticTaskDeviceCapabilities(config, task.TaskType, task.Environment); err != nil {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, err
	}
	entry, physicalID, present := s.physicalForConfig(config)
	if !present || entry.Snapshot == nil {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, errors.New("configured device is offline")
	}
	initialICCID := strings.TrimSpace(entry.Snapshot.ICCID)
	if strings.EqualFold(initialICCID, strings.TrimSpace(task.ProfileICCID)) {
		return config, entry, physicalID, automaticTaskRevertState{}, nil
	}

	initialPolicy, policyErr := s.store.CardPolicy(ctx, initialICCID)
	if errors.Is(policyErr, store.ErrNotFound) {
		initialPolicy = defaultCardPolicy(initialICCID)
	} else if policyErr != nil {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("read initial card policy: %w", policyErr)
	}
	initialSnapshot := automaticTaskEnvironmentSnapshot{config: config, policy: initialPolicy}
	recovery = automaticTaskRevertState{physicalID: physicalID, initialICCID: initialICCID, snapshot: initialSnapshot}
	if config.Telephony != nil {
		recovery.modified = true
		if err := s.stopTelephonyAccess(ctx, config); err != nil {
			return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, err
		}
		if err := s.quiesceOwnedData(ctx, config); err != nil {
			return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("stop original data before task profile switch: %w", err)
		}
	}

	var initialAID, initialEID string
	if info, infoErr := s.devices.ESIMListProfiles(ctx, physicalID); infoErr == nil {
		initialEID = strings.TrimSpace(info.EID)
		for _, p := range info.Profiles {
			if strings.EqualFold(strings.TrimSpace(p.ICCID), initialICCID) {
				initialAID = strings.TrimSpace(p.AID)
				break
			}
		}
		if initialAID == "" {
			initialAID = strings.TrimSpace(info.AID)
		}
	}
	if initialEID == "" {
		if chip, chipErr := s.devices.ESIMChipInfo(ctx, physicalID); chipErr == nil && chip != nil {
			initialEID = strings.TrimSpace(chip.EID)
		}
	}
	if err := recordAutomaticTaskProfileSwitch(ctx, initialAID, initialEID); err != nil {
		return store.Device{}, device.Device{}, "", recovery, err
	}

	recovery.initialAID = initialAID
	revertState = recovery
	originalNetworkEnabled := config.NetworkEnabled
	recovery.modified = true
	if task.TaskType == "cellular_attach" {
		// Persist the disabled intent before switching: device reappearance can
		// otherwise make the lifecycle reconciler restore the previous data state.
		config.NetworkEnabled = false
		if err := s.store.UpsertDevice(ctx, config); err != nil {
			return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, err
		}
		request := s.cellularNetworkRequest(ctx, config, entry.Snapshot)
		request.Enabled = false
		if _, journaled := ctx.Value(automaticTaskJournalKey{}).(*automaticTaskJournal); !journaled {
			if _, err := s.applyCellularData(ctx, config.ID, physicalID, request); err != nil {
				return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("stop cellular data before profile switch: %w", err)
			}
		}
	}
	progress("正在切换到任务指定的 eSIM Profile")
	desiredData := config.NetworkEnabled && !(config.VoWiFiEnabled && store.WiFiExclusive(config))
	dataRuntime := s.cellularDataRuntime()
	s.clearPublicIP(config.ID)
	dataRuntime.invalidateWithMaintenancePhase(config.ID, desiredData, "recovering", "", "profile_switching")
	switchCompleted := false
	switchDeviceID := config.ID
	defer func() {
		if !switchCompleted {
			dataRuntime.invalidate(switchDeviceID, desiredData, "failed", "automatic eSIM profile switch did not complete")
		}
	}()
	if _, err := s.devices.SetFlight(ctx, physicalID, true); err != nil {
		// Data stopped successfully, and ESIMSwitchProfile has not been called.
		// Restore intent only here; after a switch attempt the active SIM is uncertain.
		if task.TaskType == "cellular_attach" {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			config.NetworkEnabled = originalNetworkEnabled
			if restoreErr := s.store.UpsertDevice(cleanupContext, config); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore device intent before profile switch: %w", restoreErr))
			} else {
				desiredData = config.NetworkEnabled && !(config.VoWiFiEnabled && store.WiFiExclusive(config))
			}
		}
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("enter airplane mode before profile switch: %w", err)
	}
	recovery.needed, recovery.switchAttempted = true, true
	revertState = recovery
	if err := s.devices.ESIMSwitchProfile(ctx, physicalID, task.ProfileICCID, task.ProfileAID); err != nil {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("switch eSIM profile: %w", err)
	}
	entry, physicalID, present = s.physicalForConfig(config)
	if !present {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, errors.New("device did not recover after profile switch")
	}
	snapshot, err := s.devices.Refresh(ctx, physicalID)
	if err != nil {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("verify switched profile: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(snapshot.ICCID), strings.TrimSpace(task.ProfileICCID)) {
		return store.Device{}, device.Device{}, "", automaticTaskRevertState{}, fmt.Errorf("profile verification failed: current ICCID is %s", firstNonEmpty(snapshot.ICCID, "unavailable"))
	}
	dataRuntime.invalidate(config.ID, false, "disabled", "")
	switchCompleted = true
	entry.Snapshot = &snapshot
	if config.Telephony != nil {
		card, policyErr := s.store.CardPolicy(ctx, snapshot.ICCID)
		if errors.Is(policyErr, store.ErrNotFound) {
			card = defaultCardPolicy(snapshot.ICCID)
		} else if policyErr != nil {
			return store.Device{}, device.Device{}, "", revertState, policyErr
		}
		policy, policyErr := s.store.TelephonyForCard(ctx, config, card)
		if policyErr != nil {
			return store.Device{}, device.Device{}, "", revertState, policyErr
		}
		config.Telephony = &policy
		config = temporaryAutomaticTaskDevice(config)
		config.APN = card.APN
		// Keep the target SIM's durable preferences intact. The task changes
		// only the active projection while its line writer is held.
		if err := s.store.UpsertDevice(ctx, config); err != nil {
			return store.Device{}, device.Device{}, "", revertState, err
		}
		s.cellularDataRuntime().resume(config.ID)
	}
	return config, entry, physicalID, revertState, nil
}

func (s *Server) prepareAutomaticTaskEnvironment(ctx context.Context, config *store.Device, entry device.Device, physicalID string, task store.AutomaticTask, progress automaticTaskProgress) error {
	if task.Environment == store.IMSModeHost && config.Telephony == nil {
		card, err := s.store.CardPolicy(ctx, task.ProfileICCID)
		if errors.Is(err, store.ErrNotFound) {
			card, err = defaultCardPolicy(task.ProfileICCID), nil
		}
		if err != nil {
			return err
		}
		policy, err := s.store.TelephonyForCard(ctx, *config, card)
		if err != nil {
			return err
		}
		// This is only the active task projection. The journal restores a
		// legacy device without persisting new per-SIM connection preferences.
		config.Telephony = &policy
	}
	if config.Telephony != nil {
		return s.prepareIndependentTaskEnvironment(ctx, config, physicalID, task, progress)
	}
	iccid := strings.TrimSpace(task.ProfileICCID)
	if task.Environment == "vowifi" {
		progress("正在准备 VoWiFi 执行环境")
		if task.TaskType == "public_ip" {
			return errors.New("public IP tasks cannot run over VoWiFi")
		}
		desiredData := config.NetworkEnabled && !(config.VoWiFiEnabled && store.WiFiExclusive(*config))
		dataRuntime := s.cellularDataRuntime()
		s.clearPublicIP(config.ID)
		dataRuntime.invalidateWithMaintenancePhase(config.ID, desiredData, "recovering", "", "vowifi")
		if _, err := s.devices.SetFlight(ctx, physicalID, true); err != nil {
			return fmt.Errorf("enable airplane mode for VoWiFi: %w", err)
		}
		config.VoWiFiEnabled, config.NetworkEnabled = true, false
		config.IMSMode, config.IMSEnabled = store.IMSModeVoWiFi, true
		if err := s.store.UpsertDevice(ctx, *config); err != nil {
			return err
		}
		dataRuntime.invalidate(config.ID, false, "disabled", "")
		policy, policyErr := s.store.CardPolicy(ctx, iccid)
		if errors.Is(policyErr, store.ErrNotFound) {
			policy = defaultCardPolicy(iccid)
			policyErr = nil
		}
		if policyErr != nil {
			return policyErr
		}
		policy.NetworkEnabled = false
		policy.VoWiFiEnabled = true
		policy.AirplaneEnabled = true
		policy.Source = "automatic_task"
		if err := s.store.UpsertCardPolicy(ctx, policy); err != nil {
			return err
		}
		if s.vowifi == nil {
			return errors.New("VoWiFi runtime is unavailable")
		}
		state, stateErr := s.vowifi.State(config.ID)
		stateMatchesCard := state.ICCID == "" || strings.EqualFold(strings.TrimSpace(state.ICCID), iccid)
		if stateErr == nil && stateMatchesCard && state.IMSReady && (task.TaskType != "sms" || state.SMSReady) {
			return nil
		}
		if stateErr == nil && state.Enabled {
			_, stateErr = s.vowifi.RequestReconnect(config.ID)
		} else {
			_, stateErr = s.vowifi.RequestEnabled(config.ID, true)
		}
		if stateErr != nil {
			return fmt.Errorf("start VoWiFi: %w", stateErr)
		}
		return s.waitAutomaticVoWiFi(ctx, config.ID, iccid, task.TaskType == "sms")
	}
	if s.vowifi != nil {
		if state, stateErr := s.vowifi.State(config.ID); stateErr == nil && (state.Enabled || state.Active) {
			if _, stateErr = s.vowifi.RequestEnabled(config.ID, false); stateErr != nil {
				return fmt.Errorf("stop VoWiFi: %w", stateErr)
			}
			if err := s.waitAutomaticVoWiFiStopped(ctx, config.ID); err != nil {
				return err
			}
		}
	}
	progress("正在开启蜂窝无线并启用自动选网")
	config.VoWiFiEnabled = false
	config.IMSMode, config.IMSEnabled = store.IMSModeModule, false
	config.NetworkEnabled = task.TaskType == "public_ip"
	if err := s.store.UpsertDevice(ctx, *config); err != nil {
		return err
	}
	policy, policyErr := s.store.CardPolicy(ctx, iccid)
	if errors.Is(policyErr, store.ErrNotFound) {
		policy = defaultCardPolicy(iccid)
		policy.APN = config.APN
	} else if policyErr != nil {
		return policyErr
	}
	policy.NetworkEnabled = config.NetworkEnabled
	policy.VoWiFiEnabled = false
	policy.AirplaneEnabled = false
	policy.Source = "automatic_task"
	if err := s.store.UpsertCardPolicy(ctx, policy); err != nil {
		return err
	}
	if task.TaskType != "public_ip" {
		if _, err := s.applyCellularData(ctx, config.ID, physicalID, s.cardNetworkRequest(ctx, physicalID, *config, policy, false)); err != nil {
			if task.TaskType == "cellular_attach" {
				return fmt.Errorf("stop cellular data before registration: %w", err)
			}
			s.logger.Warn("automatic task could not stop unused cellular data", "device_id", config.ID)
		}
	}
	if _, err := s.devices.SetFlight(ctx, physicalID, false); err != nil {
		return fmt.Errorf("enable cellular radio: %w", err)
	}
	if _, err := s.devices.SetOperatorSelection(ctx, physicalID, true, "", nil); err != nil {
		return fmt.Errorf("enable automatic network selection: %w", err)
	}
	if _, err := s.devices.ReRegisterOperator(ctx, physicalID); err != nil {
		return fmt.Errorf("re-register cellular network: %w", err)
	}
	progress("正在搜索并注册蜂窝网络（漫游注册可能需要数分钟）")
	if err := s.waitAutomaticCellular(ctx, physicalID, task.TaskType == "public_ip"); err != nil {
		return err
	}
	if task.TaskType == "public_ip" {
		progress("已注册蜂窝网络，正在建立数据连接")
		if _, err := s.applyCellularData(ctx, config.ID, physicalID, s.cardNetworkRequest(ctx, physicalID, *config, policy, true)); err != nil {
			return fmt.Errorf("start roaming data: %w", err)
		}
	}
	return nil
}

func (s *Server) waitAutomaticVoWiFi(ctx context.Context, deviceID, iccid string, requireSMS bool) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		state, err := s.vowifi.State(deviceID)
		if err == nil && state.IMSReady && (!requireSMS || state.SMSReady) && (state.ICCID == "" || strings.EqualFold(state.ICCID, iccid)) {
			return nil
		}
		if err == nil && state.LastError != "" && !state.Active && !state.Enabled {
			return errors.New(state.LastError)
		}
		select {
		case <-ctx.Done():
			if err == nil && state.LastError != "" {
				return fmt.Errorf("wait for VoWiFi readiness: %s", state.LastError)
			}
			return fmt.Errorf("wait for VoWiFi readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) waitAutomaticVoWiFiStopped(ctx context.Context, deviceID string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, err := s.vowifi.State(deviceID)
		if err != nil || (!state.Active && !state.Enabled) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for VoWiFi shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) waitAutomaticCellular(ctx context.Context, physicalID string, requirePacketAttach bool) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	stableSamples := 0
	for {
		snapshot, err := s.devices.Refresh(ctx, physicalID)
		registered := err == nil && (snapshot.RegistrationStatus == 1 || snapshot.RegistrationStatus == 5)
		if registered && (!requirePacketAttach || snapshot.PSAttached) {
			stableSamples++
			if stableSamples >= 2 {
				return nil
			}
		} else {
			stableSamples = 0
		}
		if err == nil && snapshot.RegistrationStatus == 3 {
			return errors.New("cellular network registration was denied")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for cellular registration: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) executeAutomaticSMS(ctx context.Context, task store.AutomaticTask, payload automaticTaskPayload) (string, error) {
	body, _ := json.Marshal(map[string]any{"device_id": task.DeviceID, "phone": payload.Phone, "message": payload.Message})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/sms/send", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleSMSSend(recorder, request)
	if recorder.Code < 200 || recorder.Code >= 300 {
		failure := fmt.Errorf("send SMS failed (HTTP %d): %s", recorder.Code, compactAutomaticResponse(recorder.Body.Bytes()))
		// Once any part reached the modem/IMS transaction, retrying the whole
		// message could deliver a duplicate. Preparation failures remain safe to
		// retry according to the configured count.
		return "", automaticTaskExecutionError{err: failure, retryable: automaticSMSRetrySafe(recorder.Body.Bytes())}
	}
	return "短信已提交到 " + payload.Phone, nil
}

func automaticSMSRetrySafe(body []byte) bool {
	var payload struct {
		Data struct {
			PartsAttempted int   `json:"parts_attempted"`
			PartsAccepted  int   `json:"parts_accepted"`
			RetrySafe      *bool `json:"retry_safe"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if payload.Data.RetrySafe != nil {
		return *payload.Data.RetrySafe
	}
	return payload.Data.PartsAttempted == 0 && payload.Data.PartsAccepted == 0
}

func (s *Server) executeAutomaticCall(ctx context.Context, task store.AutomaticTask, payload automaticTaskPayload) (string, error) {
	config, err := s.store.Device(ctx, task.DeviceID)
	if err != nil {
		return "", err
	}
	_, physicalID, present := s.physicalForConfig(config)
	if !present {
		return "", errors.New("configured device is offline")
	}
	return s.executeOwnedAutomaticCall(ctx, config, physicalID, task, payload)
}

func (s *Server) executeAutomaticPublicIP(ctx context.Context, config store.Device, iccid string) (string, error) {
	if strings.TrimSpace(config.Interface) == "" {
		return "", errors.New("device has no cellular network interface")
	}
	info, generation, err := s.lookupCellularPublicIP(ctx, config.ID, config.Interface)
	if err != nil {
		return "", fmt.Errorf("detect roaming public IP: %w", err)
	}
	s.savePublicIP(config.ID, iccid, info, generation)
	return strings.TrimSpace(fmt.Sprintf("公网 IP %s · %s %s", info.IP, info.CountryCode, info.Region)), nil
}

func (s *Server) restoreAutomaticTaskEnvironment(physicalID string, snapshot automaticTaskEnvironmentSnapshot, origins ...context.Context) error {
	origin := context.Background()
	if len(origins) != 0 && origins[0] != nil {
		origin = context.WithoutCancel(origins[0])
	}
	cleanupContext, cancel := context.WithTimeout(origin, 60*time.Second)
	defer cancel()
	config, policy := snapshot.config, snapshot.policy
	if config.Telephony != nil {
		return s.restoreIndependentTaskEnvironment(cleanupContext, physicalID, snapshot)
	}
	current, readErr := s.store.Device(cleanupContext, config.ID)
	if readErr != nil {
		return readErr
	}
	if err := s.requireAutomaticTaskCallsIdle(cleanupContext, current); err != nil {
		return err
	}
	live, liveErr := s.devices.Refresh(cleanupContext, physicalID)
	if liveErr != nil || strings.TrimSpace(live.ICCID) != strings.TrimSpace(policy.ICCID) {
		return errors.Join(liveErr, errors.New("the saved task environment does not match the current SIM"))
	}
	identity := vowifi.SIMIdentity{ICCID: strings.TrimSpace(live.ICCID), IMSI: strings.TrimSpace(live.IMSI)}
	if store.HostIMSConfigured(config) {
		// Host IMS owns its cellular bearer; never restore a legacy packet
		// connection or issue module RF commands after queuing its runtime.
		config.NetworkEnabled, config.VoWiFiEnabled = false, false
		if err := s.store.UpsertCardPolicy(cleanupContext, policy); err != nil {
			return err
		}
		if err := s.store.UpsertDevice(cleanupContext, config); err != nil {
			return err
		}
		s.cellularDataRuntime().resume(config.ID)
		if !store.AccessEnabled(config, store.IMSModeHost) {
			return nil
		}
		controller := s.controllerForMode(store.IMSModeHost)
		if controller == nil {
			return errIMSUnavailable
		}
		if ensure, ok := controller.(interface {
			Ensure(context.Context, string) error
		}); ok {
			if err := ensure.Ensure(cleanupContext, config.ID); err != nil {
				return err
			}
		}
		endIMSMaintenance(controller, config.ID)
		_, err := controller.RequestEnabled(config.ID, true)
		return err
	}
	desiredNetwork := policy.NetworkEnabled && !policy.VoWiFiEnabled && !policy.AirplaneEnabled
	config.APN = policy.APN
	config.NetworkEnabled = desiredNetwork
	config.VoWiFiEnabled = policy.VoWiFiEnabled
	var restoreErrors []error
	if err := s.store.UpsertCardPolicy(cleanupContext, policy); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("persist card policy: %w", err))
	}
	if err := s.store.UpsertDevice(cleanupContext, config); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("persist device policy: %w", err))
	}
	if err := errors.Join(restoreErrors...); err != nil {
		return err
	}
	s.cellularDataRuntime().resume(config.ID)
	if config.DeviceType == store.DeviceTypeUSBSIMReader {
		if s.vowifi == nil {
			return errors.Join(append(restoreErrors, errors.New("VoWiFi runtime is unavailable"))...)
		}
		state, stateErr := s.vowifi.State(config.ID)
		if policy.VoWiFiEnabled {
			if stateErr == nil && state.Enabled {
				_, stateErr = s.vowifi.RequestReconnect(config.ID)
			} else {
				_, stateErr = s.vowifi.RequestEnabled(config.ID, true)
			}
		} else if stateErr == nil && (state.Enabled || state.Active) {
			_, stateErr = s.vowifi.RequestEnabled(config.ID, false)
		}
		if stateErr != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore reader VoWiFi: %w", stateErr))
		}
		return errors.Join(restoreErrors...)
	}

	if policy.VoWiFiEnabled {
		if _, err := s.applyCellularData(cleanupContext, config.ID, physicalID, s.cardNetworkRequest(cleanupContext, physicalID, config, policy, false)); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("stop cellular data: %w", err))
		}
		if _, err := s.setSubscriptionFlight(cleanupContext, physicalID, identity, true); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore airplane mode: %w", err))
		}
		if s.vowifi == nil {
			restoreErrors = append(restoreErrors, errors.New("VoWiFi runtime is unavailable"))
		} else if state, stateErr := s.vowifi.State(config.ID); stateErr == nil && state.Enabled {
			if _, err := s.vowifi.RequestReconnect(config.ID); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore VoWiFi: %w", err))
			}
		} else if _, err := s.vowifi.RequestEnabled(config.ID, true); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore VoWiFi: %w", err))
		}
		return errors.Join(restoreErrors...)
	}
	if s.vowifi != nil {
		if state, stateErr := s.vowifi.State(config.ID); stateErr == nil && (state.Enabled || state.Active) {
			if _, err := s.vowifi.RequestEnabled(config.ID, false); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("stop VoWiFi: %w", err))
			}
		}
	}
	if policy.AirplaneEnabled {
		if _, err := s.applyCellularData(cleanupContext, config.ID, physicalID, s.cardNetworkRequest(cleanupContext, physicalID, config, policy, false)); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("stop cellular data: %w", err))
		}
		if _, err := s.setSubscriptionFlight(cleanupContext, physicalID, identity, true); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore airplane mode: %w", err))
		}
		return errors.Join(restoreErrors...)
	}
	if !desiredNetwork {
		if _, err := s.applyCellularData(cleanupContext, config.ID, physicalID, s.cardNetworkRequest(cleanupContext, physicalID, config, policy, false)); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("stop cellular data: %w", err))
		}
	}
	if _, err := s.setSubscriptionFlight(cleanupContext, physicalID, identity, false); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("restore cellular radio: %w", err))
	}
	if desiredNetwork {
		if _, err := s.applyCellularData(cleanupContext, config.ID, physicalID, s.cardNetworkRequest(cleanupContext, physicalID, config, policy, true)); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore cellular data: %w", err))
		}
	}
	return errors.Join(restoreErrors...)
}

func (s *Server) cardNetworkRequest(
	ctx context.Context,
	physicalID string,
	config store.Device,
	policy store.CardPolicy,
	enabled bool,
) device.NetworkRequest {
	request := device.NetworkRequest{
		Enabled: enabled, APN: policy.APN, IPVersion: policy.IPVersion, Backend: config.DeviceBackend,
	}
	if request.IPVersion == "" {
		request.IPVersion = "IPV4V6"
	}
	profile, err := s.store.CardAPNProfileByAPN(ctx, policy.ICCID, policy.APN, policy.IPVersion)
	if err != nil {
		return request
	}
	request.Username = profile.Username
	request.Password = profile.Password
	request.Authentication = profile.AuthType
	if entry, getErr := s.devices.Get(physicalID); getErr == nil && entry.Snapshot != nil &&
		entry.Snapshot.RegistrationStatus == 5 && profile.RoamingIPVersion != "" {
		request.IPVersion = profile.RoamingIPVersion
	}
	return request
}

func compactAutomaticResponse(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if apiErr, ok := payload["error"].(map[string]any); ok {
			return firstNonEmpty(fmt.Sprint(apiErr["message"]), fmt.Sprint(apiErr["code"]), "request failed")
		}
	}
	return strings.TrimSpace(string(body))
}

func (s *Server) routeAutomaticTasksAPI(w http.ResponseWriter, r *http.Request, cleanPath string) bool {
	segments := splitAPIPath(cleanPath)
	if len(segments) == 0 || segments[0] != "automatic-tasks" {
		return false
	}
	if len(segments) == 1 {
		s.handleAutomaticTasks(w, r)
		return true
	}
	if len(segments) == 2 && segments[1] == "runs" {
		s.handleAutomaticTaskRuns(w, r)
		return true
	}
	id, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_task_id", "automatic task ID is invalid")
		return true
	}
	if len(segments) == 2 {
		s.handleAutomaticTask(w, r, id)
		return true
	}
	if len(segments) == 3 && segments[2] == "run" {
		s.handleAutomaticTaskRunNow(w, r, id)
		return true
	}
	writeError(w, http.StatusNotFound, "not_found", "automatic task endpoint not found")
	return true
}

func (s *Server) handleAutomaticTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := s.store.ListAutomaticTasks(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		if !s.developerActive(r.Context()) {
			visible := tasks[:0]
			for _, task := range tasks {
				if validateAutomaticTaskAvailability(false, task.TaskType, task.Environment) == nil {
					visible = append(visible, task)
				}
			}
			tasks = visible
		}
		recoveries, err := s.store.ListAutomaticTaskRecoveries(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		type recoveryStatus struct {
			store.AutomaticTaskRecovery
			Active bool `json:"active"`
		}
		statuses := make([]recoveryStatus, 0, len(recoveries))
		for _, recovery := range recoveries {
			_, active := s.automaticTaskOwners.Load(recovery.DeviceID)
			statuses = append(statuses, recoveryStatus{recovery, active})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"tasks": tasks, "recoveries": statuses}})
	case http.MethodPost:
		task, err := s.decodeAutomaticTask(r, 0)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_automatic_task", err.Error())
			return
		}
		saved, err := s.store.SaveAutomaticTask(r.Context(), task)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"data": saved})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleAutomaticTask(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodPut:
		task, err := s.decodeAutomaticTask(r, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_automatic_task", err.Error())
			return
		}
		saved, err := s.store.SaveAutomaticTask(r.Context(), task)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": saved})
	case http.MethodDelete:
		if err := s.store.DeleteAutomaticTask(r.Context(), id); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true}})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleAutomaticTaskRuns(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))
	var runs []store.AutomaticTaskRun
	var total int
	var err error
	if s.developerActive(r.Context()) {
		runs, total, err = s.store.ListAutomaticTaskRunsPaginated(r.Context(), limit, offset)
	} else {
		runs, total, err = s.store.ListAvailableAutomaticTaskRunsPaginated(r.Context(), limit, offset)
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"runs": runs, "total": total}})
}

func (s *Server) handleAutomaticTaskRunNow(w http.ResponseWriter, r *http.Request, id int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if s.automaticTasks == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "automatic task scheduler is unavailable")
		return
	}
	task, err := s.store.AutomaticTask(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	config, err := s.store.Device(r.Context(), task.DeviceID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := validateAutomaticTaskDeviceCapabilities(config, task.TaskType, task.Environment); err != nil {
		writeError(w, http.StatusConflict, "wifi_calling_only_device", err.Error())
		return
	}
	if err := validateAutomaticTaskAvailability(s.developerActive(r.Context()), task.TaskType, task.Environment); err != nil {
		writeError(w, http.StatusNotFound, "task_unavailable", err.Error())
		return
	}
	run, err := s.store.QueueAutomaticTaskNow(r.Context(), task)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.automaticTasks.enqueue(run)
	writeJSON(w, http.StatusAccepted, map[string]any{"data": run})
}

func (s *Server) decodeAutomaticTask(r *http.Request, id int64) (store.AutomaticTask, error) {
	var request struct {
		Name               string               `json:"name"`
		Enabled            bool                 `json:"enabled"`
		DeviceID           string               `json:"device_id"`
		ProfileICCID       string               `json:"profile_iccid"`
		ProfileAID         string               `json:"profile_aid"`
		TaskType           string               `json:"task_type"`
		Environment        string               `json:"environment"`
		IntervalDays       int                  `json:"interval_days"`
		StartDate          string               `json:"start_date"`
		RunTime            string               `json:"run_time"`
		Timezone           string               `json:"timezone"`
		RetryCount         int                  `json:"retry_count"`
		Notify             bool                 `json:"notify"`
		RevertProfile      *bool                `json:"revert_profile"`
		RevertProfileCamel *bool                `json:"revertProfile"`
		Payload            automaticTaskPayload `json:"payload"`
	}
	if err := s.decodeJSON(nilResponseWriter{}, r, &request); err != nil {
		return store.AutomaticTask{}, err
	}
	request.Name, request.DeviceID = strings.TrimSpace(request.Name), strings.TrimSpace(request.DeviceID)
	request.ProfileICCID, request.ProfileAID = strings.TrimSpace(request.ProfileICCID), strings.TrimSpace(request.ProfileAID)
	request.TaskType, request.Environment = strings.ToLower(strings.TrimSpace(request.TaskType)), strings.ToLower(strings.TrimSpace(request.Environment))
	if request.Name == "" || request.DeviceID == "" || request.ProfileICCID == "" {
		return store.AutomaticTask{}, errors.New("name, device, and eSIM profile are required")
	}
	selectedDevice, err := s.store.Device(r.Context(), request.DeviceID)
	if err != nil {
		return store.AutomaticTask{}, errors.New("selected device does not exist")
	}
	if request.Environment != "vowifi" && request.Environment != "cellular" && request.Environment != store.IMSModeHost {
		return store.AutomaticTask{}, errors.New("environment must be vowifi, cellular or host_ims")
	}
	if request.TaskType != "sms" && request.TaskType != "call" && request.TaskType != "public_ip" && request.TaskType != "cellular_attach" {
		return store.AutomaticTask{}, errors.New("unsupported task type")
	}
	if request.TaskType == "public_ip" && request.Environment != "cellular" {
		return store.AutomaticTask{}, errors.New("public IP tasks must use cellular direct mode")
	}
	if err := validateAutomaticTaskAvailability(s.developerActive(r.Context()), request.TaskType, request.Environment); err != nil {
		return store.AutomaticTask{}, err
	}
	if err := validateAutomaticTaskDeviceCapabilities(selectedDevice, request.TaskType, request.Environment); err != nil {
		return store.AutomaticTask{}, err
	}
	if request.IntervalDays < 1 || request.IntervalDays > 365 || request.RetryCount < 0 || request.RetryCount > 10 {
		return store.AutomaticTask{}, errors.New("interval_days must be 1-365 and retry_count must be 0-10")
	}
	if request.TaskType == "sms" {
		if !validDialNumber(request.Payload.Phone) || strings.TrimSpace(request.Payload.Message) == "" {
			return store.AutomaticTask{}, errors.New("SMS phone and message are required")
		}
		if blocked, reason := blockedSMSDestination(request.Payload.Phone); blocked {
			return store.AutomaticTask{}, errors.New(reason)
		}
	}
	if request.TaskType == "call" && (!validDialNumber(request.Payload.Phone) || request.Payload.DurationSeconds < 1 || request.Payload.DurationSeconds > 600) {
		return store.AutomaticTask{}, errors.New("call phone is required and automatic hang-up must be 1-600 seconds")
	}
	request.Timezone = strings.TrimSpace(request.Timezone)
	if request.Timezone == "" {
		request.Timezone = time.Local.String()
	}
	location, err := time.LoadLocation(request.Timezone)
	if err != nil {
		return store.AutomaticTask{}, errors.New("timezone must be a valid IANA time zone")
	}
	nextRun, err := nextAutomaticRun(request.StartDate, request.RunTime, request.IntervalDays, time.Now().In(location))
	if err != nil {
		return store.AutomaticTask{}, err
	}
	revertProfile := true
	if request.RevertProfile != nil {
		revertProfile = *request.RevertProfile
	} else if request.RevertProfileCamel != nil {
		revertProfile = *request.RevertProfileCamel
	}
	payload, _ := json.Marshal(request.Payload)
	task := store.AutomaticTask{ID: id, Name: request.Name, Enabled: request.Enabled, DeviceID: request.DeviceID,
		ProfileICCID: request.ProfileICCID, ProfileAID: request.ProfileAID, TaskType: request.TaskType,
		Environment: request.Environment, IntervalDays: request.IntervalDays, StartDate: request.StartDate,
		RunTime: request.RunTime, Timezone: request.Timezone, Payload: payload, RetryCount: request.RetryCount,
		Notify: request.Notify, RevertProfile: revertProfile, NextRunAt: nextRun.UTC()}
	if id != 0 {
		if previous, previousErr := s.store.AutomaticTask(r.Context(), id); previousErr == nil {
			task.CreatedAt, task.LastRunAt, task.LastStatus, task.LastError = previous.CreatedAt, previous.LastRunAt, previous.LastStatus, previous.LastError
		}
	}
	return task, nil
}

func validateAutomaticTaskAvailability(available bool, taskType, environment string) error {
	if environment != "vowifi" && environment != "cellular" && environment != store.IMSModeHost {
		return errors.New("unsupported task environment")
	}
	if taskType != "sms" && taskType != "call" && taskType != "public_ip" && taskType != "cellular_attach" {
		return errors.New("unsupported task type")
	}
	if taskType == "public_ip" && environment != "cellular" {
		return errors.New("public IP tasks must use cellular direct mode")
	}
	if taskType == "cellular_attach" && environment != "cellular" {
		return errors.New("cellular registration tasks must use cellular direct mode")
	}
	if !available && taskType == "public_ip" {
		return errors.New("unsupported task type or environment")
	}
	return nil
}

func validateAutomaticTaskDeviceCapabilities(config store.Device, taskType, environment string) error {
	if config.DeviceType != store.DeviceTypeUSBSIMReader {
		return nil
	}
	if environment != "vowifi" {
		return errors.New("USB SIM reader tasks must use the VoWiFi environment")
	}
	if taskType != "sms" && taskType != "call" {
		return errors.New("USB SIM readers support only VoWiFi SMS and call tasks")
	}
	return nil
}

func nextAutomaticRun(date, clock string, intervalDays int, now time.Time) (time.Time, error) {
	location := now.Location()
	start, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(date)+" "+strings.TrimSpace(clock), location)
	if err != nil {
		return time.Time{}, errors.New("start_date and run_time must use YYYY-MM-DD and HH:MM")
	}
	for start.Before(now) {
		start = start.AddDate(0, 0, intervalDays)
	}
	return start, nil
}

// nilResponseWriter is used only because decodeJSON's size/error contract is
// shared with HTTP handlers; decode errors are returned to the real handler.
type nilResponseWriter struct{}

func (nilResponseWriter) Header() http.Header        { return make(http.Header) }
func (nilResponseWriter) Write([]byte) (int, error)  { return 0, nil }
func (nilResponseWriter) WriteHeader(statusCode int) {}
