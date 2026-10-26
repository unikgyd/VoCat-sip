package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"vocat/internal/store"
)

type memoryRepository struct {
	mu       sync.Mutex
	value    store.AppSetting
	exists   bool
	failSave bool
}

func (db *memoryRepository) AppSetting(context.Context, string) (store.AppSetting, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if !db.exists {
		return store.AppSetting{}, store.ErrNotFound
	}
	value := db.value
	value.Value = append(json.RawMessage(nil), value.Value...)
	return value, nil
}
func (db *memoryRepository) UpsertAppSetting(_ context.Context, v store.AppSetting) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.failSave {
		return errors.New("simulated storage failure")
	}
	v.Value = append(json.RawMessage(nil), v.Value...)
	db.value = v
	db.exists = true
	return nil
}

// Test double only. Actual durability is exercised with file-backed Store tests.
func (db *memoryRepository) CompareSIPSettings(ctx context.Context, before, after json.RawMessage) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if db.failSave {
		return false, errors.New("simulated storage failure")
	}
	if before == nil && after == nil {
		return false, errors.New("both settings operands are absent")
	}
	if before == nil {
		if db.exists {
			return false, nil
		}
	} else if !db.exists || !db.value.Sensitive || !bytes.Equal(db.value.Value, before) {
		return false, nil
	}
	if after == nil {
		db.exists = false
		db.value = store.AppSetting{}
	} else {
		db.exists = true
		db.value = store.AppSetting{Key: SettingKey, Value: append(json.RawMessage(nil), after...), Sensitive: true}
	}
	return true, nil
}
func unusedAddress(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := conn.LocalAddr().String()
	conn.Close()
	return address
}
func managerConfig(t *testing.T) Config {
	t.Helper()
	c := testConfig(t, "SHA-256").Config
	c.Listen = unusedAddress(t)
	for i := range c.Accounts {
		c.Accounts[i].Password = testPassword
	}
	return c
}

func TestManagerDefaultsAndPersistentRestart(t *testing.T) {
	ctx := context.Background()
	db := &memoryRepository{}
	m := New(ctx, db)
	defer m.Close()
	if view := m.Snapshot(); view.Config.Enabled || view.Status.Running || len(view.Config.Accounts) != 0 || db.exists {
		t.Fatal("defaults are not optional and empty")
	}
	view, err := m.Apply(ctx, managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if !view.Status.Running || !db.value.Sensitive || strings.Contains(string(db.value.Value), testPassword) {
		t.Fatal("runtime or secret persistence invalid")
	}
	public, _ := json.Marshal(view)
	if strings.Contains(string(public), "password_sha256") || strings.Contains(string(public), testPassword) {
		t.Fatal("public view leaked credentials")
	}
	view.Config.Accounts[0].DisplayName = "Updated display"
	if _, err := m.Apply(ctx, view.Config); err != nil {
		t.Fatalf("blank passwords not retained: %v", err)
	}
	m.Close()
	m = New(ctx, db)
	defer m.Close()
	if view = m.Snapshot(); !view.Status.Running || view.Config.Accounts[0].DisplayName != "Updated display" {
		t.Fatal("saved enabled state did not restart")
	}
	c := newWireClient(t, m.runtime)
	c.challengeRegister()
	requireStatus(t, c.register(2, "Contact: <sip:1001@10.0.0.11>\r\n"), 200)
	before := m.runtime
	if _, err := m.Apply(ctx, m.Snapshot().Config); err != nil || m.runtime != before {
		t.Fatal("unchanged save restarted the listener")
	}
	view = m.Snapshot()
	view.Config.Enabled = false
	if _, err := m.Apply(ctx, view.Config); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Status.Running {
		t.Fatal("disable retained listener")
	}
	conn, err := net.ListenPacket("udp", view.Config.Listen)
	if err != nil {
		t.Fatal("disabled service retained port")
	}
	conn.Close()
}
func TestManagerBindFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	db := &memoryRepository{}
	m := New(ctx, db)
	defer m.Close()
	view, err := m.Apply(ctx, managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	saved := string(db.value.Value)
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	view.Config.Listen = busy.LocalAddr().String()
	if _, err := m.Apply(ctx, view.Config); err == nil {
		t.Fatal("busy port accepted")
	}
	if !m.Snapshot().Status.Running || string(db.value.Value) != saved || m.Snapshot().Config.Listen == view.Config.Listen {
		t.Fatal("failed bind changed good settings")
	}
	c := newWireClient(t, m.runtime)
	requireStatus(t, c.exchange(c.packet("OPTIONS", "1001", 1, "", "")), 200)
}
func TestManagerStorageFailureRestoresPreviousService(t *testing.T) {
	ctx := context.Background()
	db := &memoryRepository{}
	m := New(ctx, db)
	defer m.Close()
	view, err := m.Apply(ctx, managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	saved := string(db.value.Value)
	db.failSave = true
	view.Config.Enabled = false
	if _, err := m.Apply(ctx, view.Config); err == nil {
		t.Fatal("storage failure hidden")
	}
	if view = m.Snapshot(); !view.Config.Enabled || !view.Status.Running || saved != string(db.value.Value) {
		t.Fatal("save failure did not restore service")
	}
}
func TestManagerInvalidSavedConfigurationKeepsManagementAvailable(t *testing.T) {
	for _, raw := range []string{`{"version":99}`, `{"version":1,"config":{"enabled":true}}`} {
		db := &memoryRepository{exists: true, value: store.AppSetting{Value: json.RawMessage(raw)}}
		m := New(context.Background(), db)
		if v := m.Snapshot(); v.Status.Running || v.Status.Error == "" || v.Config.Enabled {
			t.Fatal("bad persisted config not surfaced safely")
		}
		m.Close()
	}
}
func TestManagerSnapshotCannotMutateCredentialsOrAllowlist(t *testing.T) {
	m := New(context.Background(), &memoryRepository{})
	defer m.Close()
	view, err := m.Apply(context.Background(), managerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	view.Config.Accounts[0].Username = "9000"
	view.Config.AllowedCIDRs[0] = "0.0.0.0/0"
	if next := m.Snapshot(); next.Config.Accounts[0].Username != "1001" || next.Config.AllowedCIDRs[0] == "0.0.0.0/0" {
		t.Fatal("public view aliases runtime configuration")
	}
}

func TestManagerLegacyRegistrarConfigDoesNotEnableMessages(t *testing.T) {
	config := DefaultConfig()
	config.Accounts = []Account{{Username: "1001", Password: testPassword}}
	prepared, err := prepare(config, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(prepared)
	var document map[string]any
	json.Unmarshal(encoded, &document)
	legacy := document["config"].(map[string]any)
	delete(legacy, "messages_enabled")
	delete(legacy, "identity_domain")
	delete(legacy, "calls_enabled")
	for _, account := range legacy["accounts"].([]any) {
		delete(account.(map[string]any), "allow_messages")
		delete(account.(map[string]any), "allow_calls")
	}
	encoded, _ = json.Marshal(document)
	db := &memoryRepository{exists: true, value: store.AppSetting{Value: encoded, Sensitive: true}}
	m := New(context.Background(), db)
	defer m.Close()
	view := m.Snapshot()
	if view.Status.Error != "" || view.Config.MessagesEnabled || view.Config.Accounts[0].AllowMessages || view.Status.Capabilities.Messages {
		t.Fatal("legacy upgrade enabled messaging or lost configuration")
	}
	if view.Config.CallsEnabled || view.Config.Accounts[0].AllowCalls || view.Status.Capabilities.Calls {
		t.Fatal("legacy upgrade unexpectedly enabled calls")
	}
	if _, err := m.Apply(context.Background(), view.Config); err != nil {
		t.Fatal("legacy account password was not preserved")
	}
}
