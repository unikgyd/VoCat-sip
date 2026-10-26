package exportproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/store"
)

const (
	SettingKey   = "developer.export_proxy.configs"
	PasswordMask = "••••••••"
	ReservedID   = "export-proxy"
)

var (
	ErrNotFound = errors.New("export proxy configuration not found")
	ErrDisabled = errors.New("export proxy is disabled")
)

type Config struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DeviceID    string `json:"device_id"`
	Interface   string `json:"interface"`
	Mode        string `json:"mode"`
	ListenHost  string `json:"listen_host"`
	ListenPort  int    `json:"listen_port"`
	Enabled     bool   `json:"enabled"`
	AuthEnabled bool   `json:"auth_enabled"`
	Username    string `json:"username"`
	Password    string `json:"password"`
}

type Status struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	Enabled   bool      `json:"enabled"`
	Running   bool      `json:"running"`
	Ready     bool      `json:"ready"`
	Listen    string    `json:"listen"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type Manager struct {
	mu          sync.Mutex
	store       *store.Store
	logger      *slog.Logger
	configs     []Config
	listeners   map[string]net.Listener
	started     map[string]time.Time
	lastError   map[string]string
	disabled    bool
	admission   Admission
	connections map[*proxyConnection]struct{}
}

// Admission grants a revocable lease on one confirmed cellular data session.
// Until the application installs it, listeners cannot send user-plane traffic.
type Admission func(context.Context, Config) (context.Context, func(), error)

type proxyConnection struct {
	proxyID string
	client  net.Conn
	cancel  context.CancelFunc
}

func (manager *Manager) SetAdmission(admission Admission) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.admission = admission
	for connection := range manager.connections {
		connection.cancel()
		_ = connection.client.Close()
	}
}

func New(ctx context.Context, database *store.Store, logger *slog.Logger, legacyConfigPath string) (*Manager, error) {
	if database == nil {
		return nil, errors.New("export proxy store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{
		store: database, logger: logger,
		listeners: make(map[string]net.Listener),
		started:   make(map[string]time.Time),
		lastError: make(map[string]string),
	}
	migrated, err := manager.load(ctx, legacyConfigPath)
	if err != nil {
		return nil, err
	}
	if migrated {
		if err := manager.saveLocked(ctx); err != nil {
			return nil, fmt.Errorf("migrate legacy export proxy configurations: %w", err)
		}
		_ = RemoveLegacyConfig(legacyConfigPath)
	}

	for _, config := range manager.configs {
		if config.Enabled {
			if err := manager.start(ctx, config.ID); err != nil {
				manager.logger.Warn("start built-in export proxy", "id", config.ID, "error", err)
			}
		}
	}
	return manager, nil
}

func RemoveLegacyConfig(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (manager *Manager) load(ctx context.Context, legacyConfigPath string) (bool, error) {
	setting, err := manager.store.AppSetting(ctx, SettingKey)
	if err == nil {
		if err := json.Unmarshal(setting.Value, &manager.configs); err != nil {
			return false, fmt.Errorf("decode export proxy configurations: %w", err)
		}
		return false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	legacy, err := os.ReadFile(strings.TrimSpace(legacyConfigPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.TrimSpace(legacyConfigPath) == "" {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(legacy, &manager.configs); err != nil {
		return false, fmt.Errorf("decode legacy export proxy configurations: %w", err)
	}
	return true, nil
}

func (manager *Manager) saveLocked(ctx context.Context) error {
	raw, err := json.Marshal(manager.configs)
	if err != nil {
		return err
	}
	return manager.store.UpsertAppSetting(ctx, store.AppSetting{Key: SettingKey, Value: raw, Sensitive: true})
}

func (manager *Manager) Configs() ([]Config, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.disabled {
		return nil, ErrDisabled
	}
	result := make([]Config, len(manager.configs))
	for index, config := range manager.configs {
		result[index] = redact(config)
	}
	return result, nil
}

// EnabledConfigForDevice returns the first enabled configuration bound to the
// given device, reporting whether one exists. It is used to block turning off a
// device's roaming data while one of its export proxies is still running.
func (manager *Manager) EnabledConfigForDevice(deviceID string) (Config, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.disabled {
		return Config{}, false
	}
	for _, config := range manager.configs {
		if config.DeviceID == deviceID && config.Enabled {
			return redact(config), true
		}
	}
	return Config{}, false
}

func (manager *Manager) Status() ([]Status, error) {
	manager.mu.Lock()
	if manager.disabled {
		manager.mu.Unlock()
		return nil, ErrDisabled
	}
	configs := append([]Config(nil), manager.configs...)
	admission := manager.admission
	result := make([]Status, 0, len(manager.configs))
	for _, config := range manager.configs {
		status := Status{ID: config.ID, Name: config.Name, Mode: config.Mode, Enabled: config.Enabled, Error: manager.lastError[config.ID]}
		if listener := manager.listeners[config.ID]; listener != nil {
			status.Running = true
			status.Listen = listener.Addr().String()
			status.StartedAt = manager.started[config.ID]
		}
		result = append(result, status)
	}
	manager.mu.Unlock()
	for i := range result {
		if !result[i].Running || admission == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, release, err := admission(ctx, configs[i])
		if err == nil {
			result[i].Ready = true
			release()
		}
		cancel()
	}
	return result, nil
}

func (manager *Manager) Create(ctx context.Context, config Config) (Config, error) {
	config.ID = generateID()
	if err := manager.prepareConfig(ctx, &config); err != nil {
		return Config{}, err
	}
	manager.mu.Lock()
	if manager.disabled {
		manager.mu.Unlock()
		return Config{}, ErrDisabled
	}
	if err := manager.checkPortLocked(config, ""); err != nil {
		manager.mu.Unlock()
		return Config{}, err
	}
	manager.configs = append(manager.configs, config)
	if err := manager.saveLocked(ctx); err != nil {
		manager.configs = manager.configs[:len(manager.configs)-1]
		manager.mu.Unlock()
		return Config{}, err
	}
	manager.mu.Unlock()
	if config.Enabled {
		if err := manager.start(ctx, config.ID); err != nil {
			_ = manager.Delete(context.Background(), config.ID)
			return Config{}, err
		}
	}
	return redact(config), nil
}

func (manager *Manager) Update(ctx context.Context, id string, incoming Config) (Config, error) {
	incoming.ID = strings.TrimSpace(id)
	manager.mu.Lock()
	if manager.disabled {
		manager.mu.Unlock()
		return Config{}, ErrDisabled
	}
	existing, index := manager.configByIDLocked(incoming.ID)
	manager.mu.Unlock()
	if index < 0 {
		return Config{}, ErrNotFound
	}
	if incoming.Password == "" || incoming.Password == PasswordMask {
		incoming.Password = existing.Password
	}
	if err := manager.prepareConfig(ctx, &incoming); err != nil {
		return Config{}, err
	}

	manager.mu.Lock()
	if manager.disabled {
		manager.mu.Unlock()
		return Config{}, ErrDisabled
	}
	existing, index = manager.configByIDLocked(incoming.ID)
	if index < 0 {
		manager.mu.Unlock()
		return Config{}, ErrNotFound
	}
	if err := manager.checkPortLocked(incoming, incoming.ID); err != nil {
		manager.mu.Unlock()
		return Config{}, err
	}
	wasRunning := manager.listeners[incoming.ID] != nil
	runtimeChanged := existing.DeviceID != incoming.DeviceID || existing.Mode != incoming.Mode || existing.Interface != incoming.Interface ||
		existing.ListenHost != incoming.ListenHost || existing.ListenPort != incoming.ListenPort ||
		existing.AuthEnabled != incoming.AuthEnabled || existing.Username != incoming.Username || existing.Password != incoming.Password
	manager.configs[index] = incoming
	if err := manager.saveLocked(ctx); err != nil {
		manager.configs[index] = existing
		manager.mu.Unlock()
		return Config{}, err
	}
	manager.mu.Unlock()

	switch {
	case !incoming.Enabled:
		manager.stop(incoming.ID)
	case !wasRunning || runtimeChanged || !existing.Enabled:
		if err := manager.start(ctx, incoming.ID); err != nil {
			return redact(incoming), err
		}
	}
	return redact(incoming), nil
}

func (manager *Manager) Delete(ctx context.Context, id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.disabled {
		return ErrDisabled
	}
	_, index := manager.configByIDLocked(strings.TrimSpace(id))
	if index < 0 {
		return ErrNotFound
	}
	manager.stopLocked(id)
	manager.configs = append(manager.configs[:index], manager.configs[index+1:]...)
	return manager.saveLocked(ctx)
}

// DeleteAllAndDisable is irreversible for the active developer-mode session:
// it closes every listener, removes every saved proxy, and rejects new work.
func (manager *Manager) DeleteAllAndDisable(ctx context.Context) error {
	manager.mu.Lock()
	for id := range manager.listeners {
		manager.stopLocked(id)
	}
	manager.configs = nil
	manager.disabled = true
	manager.mu.Unlock()
	err := manager.store.DeleteAppSetting(ctx, SettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (manager *Manager) Close() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.disabled = true
	for id := range manager.listeners {
		manager.stopLocked(id)
	}
	return nil
}

func (manager *Manager) prepareConfig(ctx context.Context, config *Config) error {
	config.Name = strings.TrimSpace(config.Name)
	config.DeviceID = strings.TrimSpace(config.DeviceID)
	config.Interface = strings.TrimSpace(config.Interface)
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	config.ListenHost = strings.TrimSpace(config.ListenHost)
	config.Username = strings.TrimSpace(config.Username)
	if config.Name == "" {
		config.Name = "proxy-" + config.ID[:4]
	}
	if config.DeviceID == "" {
		return errors.New("device is required")
	}
	device, err := manager.store.Device(ctx, config.DeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("configured device was not found")
		}
		return err
	}
	if strings.TrimSpace(device.Interface) == "" {
		return errors.New("the selected device has no cellular interface")
	}
	if config.Interface != "" && config.Interface != device.Interface {
		return errors.New("proxy interface does not match the selected device")
	}
	config.Interface = device.Interface
	if config.Enabled && !store.CellularDataEnabled(device) {
		return errors.New("enable roaming data on the selected device before starting its export proxy")
	}
	if config.Enabled {
		if err := manager.store.RequireAutomaticTaskRecovered(ctx, config.DeviceID); err != nil {
			return err
		}
	}
	if config.Mode != "http" && config.Mode != "socks5" {
		return errors.New("mode must be http or socks5")
	}
	if config.ListenHost == "" {
		config.ListenHost = "0.0.0.0"
	}
	if net.ParseIP(config.ListenHost) == nil && config.ListenHost != "localhost" {
		return errors.New("listen host must be an IP address")
	}
	if config.ListenPort < 0 || config.ListenPort > 65535 {
		return errors.New("listen port must be between 0 and 65535")
	}
	if config.AuthEnabled {
		if config.Username == "" {
			return errors.New("username is required when authentication is enabled")
		}
		if len(config.Username) > 128 || len(config.Password) > 128 {
			return errors.New("proxy credentials are too long")
		}
	}
	return nil
}

func (manager *Manager) checkPortLocked(config Config, excludeID string) error {
	if config.ListenPort == 0 {
		return nil
	}
	for _, current := range manager.configs {
		if current.ID != excludeID && current.ListenPort == config.ListenPort && current.ListenHost == config.ListenHost {
			return fmt.Errorf("port %d is already used by another export proxy", config.ListenPort)
		}
	}
	if existing, _ := manager.configByIDLocked(excludeID); excludeID != "" &&
		existing.ListenHost == config.ListenHost && existing.ListenPort == config.ListenPort {
		return nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(config.ListenHost, strconv.Itoa(config.ListenPort)))
	if err != nil {
		return fmt.Errorf("port %d is already in use", config.ListenPort)
	}
	_ = listener.Close()
	return nil
}

func (manager *Manager) start(ctx context.Context, id string) error {
	manager.mu.Lock()
	if manager.disabled {
		manager.mu.Unlock()
		return ErrDisabled
	}
	config, index := manager.configByIDLocked(id)
	if index < 0 || !config.Enabled {
		manager.mu.Unlock()
		return ErrNotFound
	}
	if err := platformSupported(); err != nil {
		manager.lastError[id] = err.Error()
		manager.mu.Unlock()
		return err
	}
	manager.stopLocked(id)
	listener, err := net.Listen("tcp", net.JoinHostPort(config.ListenHost, strconv.Itoa(config.ListenPort)))
	if err != nil {
		manager.lastError[id] = err.Error()
		manager.mu.Unlock()
		return err
	}
	if config.ListenPort == 0 {
		config.ListenPort = listener.Addr().(*net.TCPAddr).Port
		manager.configs[index] = config
		if err := manager.saveLocked(ctx); err != nil {
			_ = listener.Close()
			manager.mu.Unlock()
			return err
		}
	}
	delete(manager.lastError, id)
	manager.listeners[id] = listener
	manager.started[id] = time.Now().UTC()
	manager.mu.Unlock()
	go manager.serve(listener, config)
	return nil
}

func (manager *Manager) stop(id string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked(id)
}

func (manager *Manager) stopLocked(id string) {
	if listener := manager.listeners[id]; listener != nil {
		_ = listener.Close()
		delete(manager.listeners, id)
	}
	delete(manager.started, id)
	for connection := range manager.connections {
		if connection.proxyID == id {
			connection.cancel()
			_ = connection.client.Close()
		}
	}
}

func (manager *Manager) serve(listener net.Listener, config Config) {
	dialer := boundDialer(config.Interface)
	resolver := boundResolver(config.Interface)
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func(client net.Conn) {
			defer client.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tracked := &proxyConnection{proxyID: config.ID, client: client, cancel: cancel}
			manager.mu.Lock()
			admission := manager.admission
			if manager.disabled || manager.listeners[config.ID] != listener || admission == nil {
				manager.mu.Unlock()
				return
			}
			if manager.connections == nil {
				manager.connections = make(map[*proxyConnection]struct{})
			}
			manager.connections[tracked] = struct{}{}
			manager.mu.Unlock()
			defer func() { manager.mu.Lock(); delete(manager.connections, tracked); manager.mu.Unlock() }()
			ctx, release, err := admission(ctx, config)
			if err != nil {
				return
			}
			defer release()
			if config.Mode == "http" {
				err = serveHTTPContext(ctx, client, config, &dialer, resolver)
			} else {
				err = serveSOCKSContext(ctx, client, config, &dialer, resolver)
			}
			if err != nil {
				manager.logger.Debug("export proxy connection closed", "id", config.ID, "error", err)
			}
		}(connection)
	}
}

func (manager *Manager) configByIDLocked(id string) (Config, int) {
	for index, config := range manager.configs {
		if config.ID == id {
			return config, index
		}
	}
	return Config{}, -1
}

func redact(config Config) Config {
	if config.Password != "" {
		config.Password = PasswordMask
	}
	return config
}

func generateID() string {
	value := make([]byte, 4)
	_, _ = rand.Read(value)
	return hex.EncodeToString(value)
}
