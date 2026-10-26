package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"vocat/internal/store"
)

const (
	callDeduplicationWindow     = 60 * time.Second
	cellularCallMonitorInterval = 3 * time.Second
)

type callNotificationKey struct{ device, transport, dialog, caller string }
type callNotificationHistory struct {
	mu   sync.Mutex
	seen map[callNotificationKey]time.Time
}

type IncomingCallNotification struct {
	DeviceID    string
	DeviceName  string
	DeviceLabel string
	Caller      string
	Called      string
	Time        time.Time
	Environment string
	CallID      string // Exact IMS dialog; never inferred from a telephone number.
}

func (value IncomingCallNotification) Title() string {
	return "收到来电"
}

func (value IncomingCallNotification) Text() string {
	envText := "VoWiFi"
	if value.Environment == "cellular" || value.Environment == "module" {
		envText = "基站直连"
	} else if value.Environment == "host_ims" {
		envText = "蜂窝 Host IMS"
	}
	return strings.Join([]string{
		"📞 收到来电",
		"设备  " + value.DeviceLabel,
		"来电号码  " + value.Caller,
		"被呼号码  " + value.Called,
		"时间  " + value.Time.Local().Format("2006-01-02 15:04:05"),
		"网络  " + envText,
	}, "\n")
}

func (value IncomingCallNotification) DetailText() string {
	lines := strings.Split(value.Text(), "\n")
	return strings.Join(lines[1:], "\n")
}

func (history *callNotificationHistory) shouldSuppressDuplicateCall(key callNotificationKey, now time.Time, window time.Duration) bool {
	history.mu.Lock()
	defer history.mu.Unlock()
	if history.seen == nil {
		history.seen = make(map[callNotificationKey]time.Time)
	}
	for k, t := range history.seen {
		if now.Sub(t) > window*2 {
			delete(history.seen, k)
		}
	}
	if lastTime, exists := history.seen[key]; exists {
		if now.Sub(lastTime) < window {
			return true
		}
	}
	if len(history.seen) >= 4096 {
		var oldestKey callNotificationKey
		var oldest time.Time
		for k, at := range history.seen {
			if oldest.IsZero() || at.Before(oldest) {
				oldestKey, oldest = k, at
			}
		}
		delete(history.seen, oldestKey)
	}
	history.seen[key] = now
	return false
}

// NotifyIncomingCall delivers an incoming call alert to all configured notification channels.
func (s *Server) NotifyIncomingCall(ctx context.Context, notification IncomingCallNotification) {
	if s.autoAnswer != nil {
		s.autoAnswer.offer(notification)
	}
	s.notifyIncomingCall(ctx, notification, sendCallNotification)
}

func (s *Server) notifyIncomingCall(ctx context.Context, notification IncomingCallNotification, send func(context.Context, string, map[string]any, IncomingCallNotification) error) {
	if ctx == nil {
		ctx = context.Background()
	}
	caller := strings.TrimSpace(notification.Caller)
	if caller == "" {
		caller = "未知号码"
	}
	notification.Caller = caller

	called := strings.TrimSpace(notification.Called)
	if called == "" {
		called = "--"
	}
	notification.Called = called

	if notification.Time.IsZero() {
		notification.Time = time.Now().UTC()
	}
	if s.logger != nil {
		s.logger.Info("incoming call detected",
			"category", "call",
			"event", "call.incoming",
			"device_id", notification.DeviceID,
			"caller", notification.Caller,
			"called", notification.Called,
			"transport", notification.Environment,
		)
	}

	dedupKey := callNotificationKey{device: notification.DeviceID, transport: notification.Environment, dialog: notification.CallID}
	if dedupKey.dialog == "" {
		dedupKey.caller = notification.Caller
	}
	if s.callNotifications.shouldSuppressDuplicateCall(dedupKey, time.Now(), callDeduplicationWindow) {
		if s.logger != nil {
			s.logger.Debug("suppressed duplicate incoming call notification", "category", "call", "device_id", notification.DeviceID, "caller", notification.Caller)
		}
		return
	}

	if notification.DeviceLabel == "" || notification.DeviceLabel == "--" {
		if configured, err := s.store.Device(ctx, notification.DeviceID); err == nil {
			notification.DeviceName = strings.TrimSpace(configured.Name)
			notification.DeviceLabel = firstNonEmpty(configured.Name, configured.ID, "--")
		} else {
			notification.DeviceLabel = firstNonEmpty(notification.DeviceID, "--")
		}
	}

	destCtx := s.notificationDestinationContext(ctx)
	var deliveries sync.WaitGroup
	for _, channel := range notificationChannels {
		setting, err := s.store.NotificationSetting(destCtx, channel)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !setting.Enabled) {
			continue
		}
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("read incoming call notification setting", "channel", channel, "error", err)
			}
			continue
		}
		var config map[string]any
		if err := json.Unmarshal(setting.Config, &config); err != nil {
			if s.logger != nil {
				s.logger.Warn("decode incoming call notification setting", "channel", channel, "error", err)
			}
			continue
		}
		deliveries.Add(1)
		go func() {
			defer deliveries.Done()
			channelCtx, cancel := context.WithTimeout(destCtx, 10*time.Second)
			defer cancel()
			if err := send(channelCtx, channel, config, notification); err != nil && s.logger != nil {
				s.logger.Warn("send incoming call notification", "category", "call", "channel", channel, "device_id", notification.DeviceID, "raw_error", err)
			}
		}()
	}
	deliveries.Wait()
}

// sendCallNotification 按渠道发送来电信息，MeoW 与其他独立标题渠道复用 DetailText。
func sendCallNotification(ctx context.Context, channel string, config map[string]any, message IncomingCallNotification) error {
	if channel == "meow" {
		return sendMeowNotification(ctx, config, &message.DeviceID, message.Title(), message.DetailText())
	}
	switch channel {
	case "telegram":
		return sendTelegramTextNotification(ctx, config, message.DeviceID, message.Text())
	case "bark":
		return sendBarkTextNotification(ctx, config, message.DeviceID, message.Title(), message.DetailText())
	case "email":
		return sendEmailTextNotification(ctx, config, message.DeviceID, message.Title()+" - "+message.DeviceLabel, message.Text())
	case "pushplus":
		return sendPushplusTextNotification(ctx, config, message.DeviceID, message.Title(), message.DetailText())
	case "webhook":
		return sendCallWebhookNotification(ctx, config, message)
	case "wecom":
		return sendWecomNotification(ctx, config, &message.DeviceID, wecomCallValues(message))
	case "lark":
		return sendLarkNotification(ctx, config, &message.DeviceID, larkCallValues(message))
	default:
		return fmt.Errorf("unsupported notification channel %q", channel)
	}
}

func renderCallWebhookTemplate(template string, message IncomingCallNotification) string {
	rendered := message.Text()
	if strings.TrimSpace(template) != "" {
		replacements := map[string]string{
			"{{text}}":         rendered,
			"{{content}}":      message.DetailText(),
			"{{event}}":        "call.received",
			"{{timestamp}}":    message.Time.UTC().Format(time.RFC3339),
			"{{time}}":         message.Time.Local().Format("2006-01-02 15:04:05"),
			"{{number}}":       message.Caller,
			"{{caller}}":       message.Caller,
			"{{called}}":       message.Called,
			"{{device_id}}":    message.DeviceID,
			"{{device_name}}":  message.DeviceName,
			"{{device_label}}": message.DeviceLabel,
			"{{environment}}":  message.Environment,
		}
		for placeholder, value := range replacements {
			template = strings.ReplaceAll(template, placeholder, value)
		}
		return template
	}
	return rendered
}

func sendCallWebhookNotification(ctx context.Context, config map[string]any, message IncomingCallNotification) error {
	if !notificationMatchesDevice(configStrings(config, "device_ids"), message.DeviceID) {
		return nil
	}
	template := configString(config, "text_template")
	rendered := renderCallWebhookTemplate(template, message)
	payload, _ := json.Marshal(map[string]any{
		"event":        "call.received",
		"message":      rendered,
		"timestamp":    message.Time.UTC().Format(time.RFC3339),
		"device_id":    message.DeviceID,
		"device_name":  message.DeviceName,
		"device_label": message.DeviceLabel,
		"caller":       message.Caller,
		"called":       message.Called,
		"environment":  message.Environment,
	})
	timeout := durationMilliseconds(configInt(config, "timeout_ms"), 5*time.Second)
	client, err := restrictedHTTPClient(ctx, timeout, "")
	if err != nil {
		return err
	}
	retries := configInt(config, "retry_max")
	for _, destination := range configStrings(config, "urls") {
		parsed, err := validateOutboundURL(ctx, destination, false)
		if err != nil {
			return err
		}
		var sendErr error
		for attempt := 0; attempt <= retries; attempt++ {
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(payload))
			if requestErr != nil {
				return fmt.Errorf("create call webhook notification request: %w", requestErr)
			}
			for name, value := range configStringMap(config, "headers") {
				request.Header.Set(name, value)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "vocat-call-notification/1")
			if secret := configString(config, "secret"); secret != "" {
				signature := hmac.New(sha256.New, []byte(secret))
				_, _ = signature.Write(payload)
				request.Header.Set("X-vocat-Signature", "sha256="+hex.EncodeToString(signature.Sum(nil)))
			}
			sendErr = performNotificationRequest(client, request, false)
			if sendErr == nil {
				break
			}
		}
		if sendErr != nil {
			return sendErr
		}
	}
	return nil
}

func wecomCallValues(message IncomingCallNotification) wecomTemplateValues {
	return wecomTemplateValues{
		"event":        "call.received",
		"title":        message.Title(),
		"message":      message.Text(),
		"timestamp":    message.Time.UTC().Format(time.RFC3339),
		"content":      message.DetailText(),
		"number":       message.Caller,
		"device_id":    message.DeviceID,
		"device_name":  message.DeviceName,
		"device_label": message.DeviceLabel,
		"time":         message.Time.Local().Format("2006-01-02 15:04:05"),
	}
}

func larkCallValues(message IncomingCallNotification) larkTemplateValues {
	return larkTemplateValues{
		"event":        "call.received",
		"title":        message.Title(),
		"message":      message.Text(),
		"timestamp":    message.Time.UTC().Format(time.RFC3339),
		"content":      message.DetailText(),
		"number":       message.Caller,
		"device_id":    message.DeviceID,
		"device_name":  message.DeviceName,
		"device_label": message.DeviceLabel,
		"time":         message.Time.Local().Format("2006-01-02 15:04:05"),
	}
}

// StartCellularCallMonitor scans physical modems for incoming calls in cellular mode.
func (s *Server) StartCellularCallMonitor(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(cellularCallMonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollCellularCalls(ctx)
		}
	}
}

func (s *Server) pollCellularCalls(ctx context.Context) {
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	// A timeout on one physical module must not postpone every other module's
	// call observation. Keep concurrency bounded even for a large device list.
	jobs := make(chan string)
	var workers sync.WaitGroup
	for i := 0; i < min(4, len(devices)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				s.pollCellularCallsForDevice(ctx, id)
			}
		}()
	}
	for _, config := range devices {
		select {
		case jobs <- config.ID:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}
func (s *Server) pollCellularCallsForDevice(ctx context.Context, id string) {
	for _, notification := range s.collectCellularCalls(ctx, id) {
		s.NotifyIncomingCall(ctx, notification)
	}
}
func (s *Server) collectCellularCalls(ctx context.Context, id string) (notifications []IncomingCallNotification) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	lock := s.imsModeLock(id)
	for {
		if ctx.Err() != nil {
			return
		}
		if lock.TryRLock() {
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	defer lock.RUnlock()
	config, err := s.store.Device(ctx, id)
	if err != nil {
		return
	}
	// Native 410 firmware has no supported cellular call interface. Its
	// VoWiFi calls arrive through SIP; polling CLCC while SIM/IMS is starting
	// only produces ERROR and incorrectly poisons the modem health state.
	if store.NormalizeDeviceType(config.DeviceType) == store.DeviceTypeWiFi410 {
		return
	}
	// If VoWiFi is active, incoming calls are handled directly by SIP INVITE in real time.
	if config.Telephony != nil && !store.AccessEnabled(config, store.IMSModeModule) || config.Telephony == nil && s.callTransportForConfig(config) != "cellular" {
		return
	}
	entry, physicalID, present := s.physicalForConfig(config)
	if !present {
		return
	}
	if s.moduleCalls != nil && s.moduleVoiceEnabled(config) && s.moduleCalls.Available(ctx, id) == nil {
		calls, err := s.moduleCalls.Poll(ctx, id)
		if err != nil {
			return
		}
		for _, call := range calls {
			if call.Direction != "incoming" || call.State != "ringing" || call.EndedAt != nil {
				continue
			}
			s.ForwardIMSIncomingCall(id, call.ID)
			notifications = append(notifications, IncomingCallNotification{DeviceID: id, DeviceName: config.Name, DeviceLabel: firstNonEmpty(config.Name, id), Caller: call.Number, CallID: call.ID, Time: call.StartedAt, Environment: "module"})
		}
		return
	}
	response, err := s.devices.ExecuteAT(ctx, physicalID, "AT+CLCC")
	if err != nil || !response.OK() {
		return
	}
	calls := parseCLCC(response)
	if len(calls) == 0 {
		s.moduleCallScopes.Delete(id)
	}
	for _, call := range calls {
		if isIncomingVoiceCLCC(call) {
			caller, _ := call["number"].(string)
			if caller == "" {
				caller = "未知号码"
			}
			called := ""
			if entry.Snapshot != nil {
				called = entry.Snapshot.Phone.Number
			}
			notifications = append(notifications, IncomingCallNotification{
				DeviceID:    config.ID,
				DeviceName:  strings.TrimSpace(config.Name),
				DeviceLabel: firstNonEmpty(config.Name, config.ID, "--"),
				Caller:      caller,
				Called:      firstNonEmpty(called, "--"),
				Time:        time.Now().UTC(),
				Environment: "cellular",
			})
		}
	}
	return
}

func isIncomingVoiceCLCC(call map[string]any) bool {
	direction, _ := call["direction"].(int)
	state, _ := call["state"].(int)
	mode, _ := call["mode"].(int)
	// direction 1 = incoming (Mobile Terminated)
	// mode 0 = voice; some modems also expose packet-data sessions as CLCC mode 1
	// state 4 = incoming/ringing, 5 = waiting, 0 = active, 3 = alerting
	return direction == 1 && mode == 0 && (state == 4 || state == 5 || state == 0 || state == 3)
}
