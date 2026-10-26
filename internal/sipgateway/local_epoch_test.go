package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vocat/internal/store"
)

func localEpochConfig(t *testing.T) Config {
	t.Helper()
	config := DefaultConfig()
	config.Listen = unusedAddress(t)
	config.Accounts = []Account{{Username: "1001", Password: testPassword}, {Username: "1002", Password: testPassword + "-two"}}
	return config
}

func localEpochFileStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "recipient-epochs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readLocalEpochSettings(t *testing.T, db repository) stored {
	t.Helper()
	value, err := db.AppSetting(context.Background(), SettingKey)
	if err != nil || !value.Sensitive {
		t.Fatalf("private settings were not committed: %v", err)
	}
	var config stored
	if err := json.Unmarshal(value.Value, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func saveLocalEpochSettings(t *testing.T, db repository, value stored) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAppSetting(context.Background(), store.AppSetting{Key: SettingKey, Value: raw, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLocalCredentialEpochRetainsOnOrdinarySaveAndRestart(t *testing.T) {
	db := localEpochFileStore(t)
	manager := New(context.Background(), db)
	t.Cleanup(manager.Close)
	config := localEpochConfig(t)
	config.Enabled = true
	if _, err := manager.Apply(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	first := localRecipientIdentity(manager.current, "1001")
	if first == "" || !validLocalCredentialEpoch(manager.current.LocalAccountEpochs["1001"]) {
		t.Fatal("missing private recipient identity")
	}
	view := manager.Snapshot()
	view.Config.Accounts[0].DisplayName = "Updated without credential reset"
	if _, err := manager.Apply(context.Background(), view.Config); err != nil {
		t.Fatal(err)
	}
	if localRecipientIdentity(manager.current, "1001") != first || localRecipientIdentity(readLocalEpochSettings(t, db), "1001") != first {
		t.Fatal("ordinary save replaced the persisted recipient identity")
	}
	public, _ := json.Marshal(manager.Snapshot())
	if strings.Contains(string(public), "local_account_epochs") || strings.Contains(string(public), "config_revision") || strings.Contains(string(public), manager.current.LocalAccountEpochs["1001"]) || strings.Contains(string(public), testPassword) {
		t.Fatal("public configuration exposed private recipient credentials")
	}
	manager.Close()
	manager = New(context.Background(), db)
	defer manager.Close()
	if !manager.Snapshot().Status.Running || localRecipientIdentity(manager.current, "1001") != first {
		t.Fatal("reopening did not retain the durable recipient identity")
	}
	revision, listener := manager.current.ConfigRevision, manager.runtime
	if _, err := manager.Apply(context.Background(), manager.Snapshot().Config); err != nil {
		t.Fatal(err)
	}
	if manager.current.ConfigRevision == revision || !validLocalCredentialEpoch(manager.current.ConfigRevision) || manager.runtime != listener || localRecipientIdentity(manager.current, "1001") != first {
		t.Fatal("ordinary save did not renew the settings fence independently of the listener and recipient epoch")
	}
}

func TestLocalCredentialEpochRotatesOnCredentialLifecycle(t *testing.T) {
	for _, kind := range []string{"changed-password", "same-password", "explicit-reset", "changed-realm", "delete-recreate"} {
		t.Run(kind, func(t *testing.T) {
			initial, err := prepare(localEpochConfig(t), stored{}, false)
			if err != nil {
				t.Fatal(err)
			}
			request := cloneConfig(initial.Config)
			previous := initial
			switch kind {
			case "changed-password":
				request.Accounts[0].Password = testPassword + "-changed"
			case "same-password":
				request.Accounts[0].Password = testPassword
			case "explicit-reset":
				request.Accounts[0].ResetCredentialIdentity = true
			case "changed-realm":
				request.Realm = "replacement-realm"
				for i := range request.Accounts {
					request.Accounts[i].Password = testPassword
				}
			case "delete-recreate":
				removed := cloneConfig(initial.Config)
				removed.Accounts = removed.Accounts[1:]
				previous, err = prepare(removed, initial, false)
				if err != nil {
					t.Fatal(err)
				}
				if _, exists := previous.LocalAccountEpochs["1001"]; exists {
					t.Fatal("deleted account retained its epoch")
				}
				request.Accounts[0].Password = testPassword
			}
			next, err := prepare(request, previous, false)
			if err != nil {
				t.Fatal(err)
			}
			if localRecipientIdentity(next, "1001") == localRecipientIdentity(initial, "1001") || !validLocalCredentialEpoch(next.LocalAccountEpochs["1001"]) {
				t.Fatal("credential lifecycle reused the previous recipient identity")
			}
			if next.Config.Accounts[0].ResetCredentialIdentity || next.Config.Accounts[0].Password != "" {
				t.Fatal("one-shot reset or plaintext password survived preparation")
			}
			if kind != "changed-realm" && next.LocalAccountEpochs["1002"] != initial.LocalAccountEpochs["1002"] {
				t.Fatal("reset changed an unrelated recipient")
			}
		})
	}
}

func TestLocalRecipientIdentityRejectsMissingOrMalformedEpoch(t *testing.T) {
	initial, err := prepare(localEpochConfig(t), stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, epoch := range []string{"", "short", strings.Repeat("A", 32), strings.Repeat("z", 32), strings.Repeat("0", 33)} {
		copy := initial
		copy.LocalAccountEpochs = map[string]string{"1001": epoch}
		if localRecipientIdentity(copy, "1001") != "" {
			t.Fatal("invalid private epoch acquired a password-only fallback")
		}
	}
	copy := initial
	copy.Config = cloneConfig(initial.Config)
	copy.Config.Accounts = copy.Config.Accounts[1:]
	if localRecipientIdentity(copy, "1001") != "" || localRecipientIdentity(initial, "9999") != "" {
		t.Fatal("removed or unknown account retained recipient authority")
	}
	copy = initial
	copy.Secrets = map[string]secret{"1001": {SHA256: initial.Secrets["1001"].SHA256}}
	if localRecipientIdentity(copy, "1001") != "" {
		t.Fatal("incomplete authentication context acquired recipient authority")
	}
}

type localEpochCASRepository struct {
	repository
	compare func(context.Context, json.RawMessage, json.RawMessage) (bool, error)
}

func (db *localEpochCASRepository) CompareSIPSettings(ctx context.Context, before, after json.RawMessage) (bool, error) {
	return db.compare(ctx, before, after)
}

type localEpochNoCASRepository struct{ repository }

func TestManagerLegacyLocalEpochMigrationIsCommittedBeforeAdmission(t *testing.T) {
	db := localEpochFileStore(t)
	config := localEpochConfig(t)
	config.Enabled = true
	legacy, err := prepare(config, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	legacy.LocalAccountEpochs = nil
	before := saveLocalEpochSettings(t, db, legacy)
	called := false
	wrapped := &localEpochCASRepository{repository: db}
	wrapped.compare = func(ctx context.Context, expected, after json.RawMessage) (bool, error) {
		called = true
		if !bytes.Equal(expected, before) {
			t.Fatal("migration did not use the exact originally read bytes")
		}
		listener, err := net.ListenPacket("udp", config.Listen)
		if err != nil {
			t.Fatal("listener admitted before migration commit")
		}
		listener.Close()
		committed, err := db.CompareSIPSettings(ctx, expected, after)
		if committed && localRecipientIdentity(readLocalEpochSettings(t, db), "1001") == "" {
			t.Fatal("migration committed without a recipient epoch")
		}
		return committed, err
	}
	manager := New(context.Background(), wrapped)
	defer manager.Close()
	if !called || !manager.Snapshot().Status.Running || localRecipientIdentity(manager.current, "1001") != localRecipientIdentity(readLocalEpochSettings(t, db), "1001") {
		t.Fatal("legacy migration was not durable before listener startup")
	}
}

func TestManagerLegacyEpochMigrationFailureLeavesListenerClosed(t *testing.T) {
	for _, failure := range []string{"conflict", "storage-error", "missing-capability"} {
		t.Run(failure, func(t *testing.T) {
			db := localEpochFileStore(t)
			config := localEpochConfig(t)
			config.Enabled = true
			legacy, err := prepare(config, stored{}, false)
			if err != nil {
				t.Fatal(err)
			}
			legacy.LocalAccountEpochs = nil
			before := saveLocalEpochSettings(t, db, legacy)
			var wrapped repository = &localEpochNoCASRepository{repository: db}
			if failure != "missing-capability" {
				wrapped = &localEpochCASRepository{repository: db, compare: func(context.Context, json.RawMessage, json.RawMessage) (bool, error) {
					if failure == "storage-error" {
						return false, errors.New("synthetic commit failure")
					}
					return false, nil
				}}
			}
			manager := New(context.Background(), wrapped)
			defer manager.Close()
			if manager.Snapshot().Status.Running || manager.Snapshot().Status.Error == "" {
				t.Fatal("uncommitted migration admitted a listener")
			}
			value, err := db.AppSetting(context.Background(), SettingKey)
			if err != nil || !bytes.Equal(value.Value, before) {
				t.Fatal("failed migration changed the legacy document")
			}
			listener, err := net.ListenPacket("udp", config.Listen)
			if err != nil {
				t.Fatal("failed migration retained its listener")
			}
			listener.Close()
		})
	}
}

func TestManagerMissingSettingsCASCannotSaveOrAdmit(t *testing.T) {
	db := localEpochFileStore(t)
	manager := New(context.Background(), &localEpochNoCASRepository{repository: db})
	defer manager.Close()
	config := localEpochConfig(t)
	config.Enabled = true
	if _, err := manager.Apply(context.Background(), config); err == nil || manager.Snapshot().Status.Running {
		t.Fatal("non-CAS repository pretended to persist settings")
	}
	if _, err := db.AppSetting(context.Background(), SettingKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("non-CAS repository used an unsafe Upsert fallback")
	}
}

func TestManagerConcurrentLegacyMigrationHasOnePersistedEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-epochs.db")
	first, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	config := localEpochConfig(t)
	config.Enabled = true
	legacy, err := prepare(config, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	legacy.LocalAccountEpochs = nil
	saveLocalEpochSettings(t, first, legacy)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	wrap := func(db *store.Store) repository {
		return &localEpochCASRepository{repository: db, compare: func(ctx context.Context, before, after json.RawMessage) (bool, error) {
			entered <- struct{}{}
			<-release
			return db.CompareSIPSettings(ctx, before, after)
		}}
	}
	var managers [2]*Manager
	var group sync.WaitGroup
	group.Add(2)
	go func() { defer group.Done(); managers[0] = New(context.Background(), wrap(first)) }()
	go func() { defer group.Done(); managers[1] = New(context.Background(), wrap(second)) }()
	<-entered
	<-entered
	close(release)
	group.Wait()
	winners := 0
	for _, manager := range managers {
		defer manager.Close()
		if manager.Snapshot().Status.Running {
			winners++
			if localRecipientIdentity(manager.current, "1001") != localRecipientIdentity(readLocalEpochSettings(t, first), "1001") {
				t.Fatal("admitted manager used an uncommitted private epoch")
			}
		} else if manager.Snapshot().Status.Error == "" {
			t.Fatal("losing migration did not fail closed")
		}
	}
	if winners != 1 {
		t.Fatal("migration did not select exactly one durable recipient identity")
	}
}

func TestManagerStaleSaveDoesNotOverwriteNewerConfiguration(t *testing.T) {
	db := localEpochFileStore(t)
	first, second := New(context.Background(), db), New(context.Background(), db)
	defer first.Close()
	defer second.Close()
	config := localEpochConfig(t)
	if _, err := first.Apply(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	before, _ := db.AppSetting(context.Background(), SettingKey)
	config.Accounts[0].DisplayName = "stale manager overwrite"
	if _, err := second.Apply(context.Background(), config); err == nil {
		t.Fatal("stale manager overwrote another manager's settings")
	}
	after, _ := db.AppSetting(context.Background(), SettingKey)
	if !bytes.Equal(before.Value, after.Value) || second.Snapshot().Status.Running {
		t.Fatal("CAS conflict altered authoritative settings or admitted a stale listener")
	}
}

func TestManagerBindRollbackCannotOverwriteAnotherManagerSave(t *testing.T) {
	db := localEpochFileStore(t)
	wrapped := &localEpochCASRepository{repository: db}
	interfere := false
	var concurrent json.RawMessage
	wrapped.compare = func(ctx context.Context, before, after json.RawMessage) (bool, error) {
		committed, err := db.CompareSIPSettings(ctx, before, after)
		if committed && interfere {
			interfere = false
			var newer stored
			if json.Unmarshal(after, &newer) != nil {
				t.Fatal("invalid candidate document")
			}
			newer.Config.Enabled = false
			newer.Config.Accounts[0].DisplayName = "authoritative concurrent save"
			concurrent, _ = json.Marshal(newer)
			if changed, err := db.CompareSIPSettings(ctx, after, concurrent); err != nil || !changed {
				t.Fatalf("concurrent CAS failed: %v", err)
			}
		}
		return committed, err
	}
	manager := New(context.Background(), wrapped)
	defer manager.Close()
	config := localEpochConfig(t)
	config.Enabled = true
	if _, err := manager.Apply(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	request := manager.Snapshot().Config
	request.Listen = busy.LocalAddr().String()
	interfere = true
	if _, err := manager.Apply(context.Background(), request); err == nil {
		t.Fatal("failed bind and rollback conflict were hidden")
	}
	after, _ := db.AppSetting(context.Background(), SettingKey)
	if concurrent == nil || !bytes.Equal(after.Value, concurrent) || manager.Snapshot().Status.Running {
		t.Fatal("rollback overwrote concurrent settings or restored a stale listener")
	}
	listener, err := net.ListenPacket("udp", config.Listen)
	if err != nil {
		t.Fatal("rollback conflict retained the revoked listener")
	}
	listener.Close()
}

func TestManagerFirstSaveBindFailureDeletesOnlyOwnCandidate(t *testing.T) {
	db := localEpochFileStore(t)
	manager := New(context.Background(), db)
	defer manager.Close()
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	config := localEpochConfig(t)
	config.Enabled, config.Listen = true, busy.LocalAddr().String()
	if _, err := manager.Apply(context.Background(), config); err == nil {
		t.Fatal("busy first candidate was admitted")
	}
	if _, err := db.AppSetting(context.Background(), SettingKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("first-save rollback did not delete precisely its own candidate")
	}
}

func TestCurrentConfigAuthorityReadsDurablePolicyNotStaleRuntime(t *testing.T) {
	db := localEpochFileStore(t)
	manager := New(context.Background(), db)
	defer manager.Close()
	if _, err := manager.Apply(context.Background(), localEpochConfig(t)); err != nil {
		t.Fatal(err)
	}
	admitted := manager.current
	request := cloneConfig(admitted.Config)
	request.Accounts[0].ResetCredentialIdentity = true
	request.SMSEnabled = false
	newer, err := prepare(request, admitted, false)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(newer)
	if changed, err := db.CompareSIPSettings(context.Background(), manager.savedRaw, after); err != nil || !changed {
		t.Fatalf("concurrent durable reset failed: %v", err)
	}
	current, err := readCurrentConfigAuthority(context.Background(), db, admitted)
	if err != nil || !current.requireDurableAuthority || current.Config.SMSEnabled || localRecipientIdentity(current, "1001") == localRecipientIdentity(admitted, "1001") {
		t.Fatal("stale runtime retained authority after another manager's durable reset")
	}
	if localRecipientIdentity(manager.current, "1001") != localRecipientIdentity(admitted, "1001") {
		t.Fatal("authority lookup mutated the admitted runtime instead of returning fresh policy")
	}
}

func TestCurrentConfigAuthorityRejectsUnknownEpochWithoutRepairing(t *testing.T) {
	for _, failure := range []string{"missing-local", "malformed-local", "missing-peer", "missing-revision", "malformed-revision", "reset-request", "plaintext-password", "invalid-ha1", "not-sensitive"} {
		t.Run(failure, func(t *testing.T) {
			db := localEpochFileStore(t)
			config := localEpochConfig(t)
			config.Peer = &PeerConfig{Endpoint: unusedAddress(t)}
			prepared, err := prepare(config, stored{}, false)
			if err != nil {
				t.Fatal(err)
			}
			prepared.ConfigRevision = strings.Repeat("a", 32)
			saveLocalEpochSettings(t, db, prepared)
			admitted := prepared
			admitted.requireDurableAuthority = true
			if _, err := readCurrentConfigAuthority(context.Background(), db, admitted); err != nil {
				t.Fatal("valid authority fixture was rejected before corruption")
			}
			sensitive := true
			switch failure {
			case "missing-local":
				prepared.LocalAccountEpochs = nil
			case "malformed-local":
				prepared.LocalAccountEpochs = map[string]string{"1001": strings.Repeat("Z", 32), "1002": prepared.LocalAccountEpochs["1002"]}
			case "missing-peer":
				peer := *prepared.PeerSecrets
				peer.CredentialEpoch = ""
				prepared.PeerSecrets = &peer
			case "missing-revision":
				prepared.ConfigRevision = ""
			case "malformed-revision":
				prepared.ConfigRevision = strings.Repeat("G", 32)
			case "reset-request":
				prepared.Config = cloneConfig(prepared.Config)
				prepared.Config.Accounts[0].ResetCredentialIdentity = true
			case "plaintext-password":
				prepared.Config = cloneConfig(prepared.Config)
				prepared.Config.Accounts[0].Password = testPassword
			case "invalid-ha1":
				prepared.Secrets = map[string]secret{"1001": {MD5: "not-a-digest", SHA256: "not-a-digest"}, "1002": prepared.Secrets["1002"]}
			case "not-sensitive":
				sensitive = false
			}
			raw, _ := json.Marshal(prepared)
			if err := db.UpsertAppSetting(context.Background(), store.AppSetting{Key: SettingKey, Value: raw, Sensitive: sensitive}); err != nil {
				t.Fatal(err)
			}
			if _, err := readCurrentConfigAuthority(context.Background(), db, admitted); err == nil {
				t.Fatal("incomplete durable recipient authority was accepted")
			}
			unchanged, err := db.AppSetting(context.Background(), SettingKey)
			if err != nil || !bytes.Equal(unchanged.Value, raw) {
				t.Fatal("authority check silently repaired or replaced unknown epochs")
			}
		})
	}
}

func TestCurrentConfigAuthoritySyntheticRuntimeDoesNotPretendDurable(t *testing.T) {
	config, err := prepare(localEpochConfig(t), stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	current, err := readCurrentConfigAuthority(context.Background(), nil, config)
	if err != nil || current.requireDurableAuthority || localRecipientIdentity(current, "1001") != localRecipientIdentity(config, "1001") {
		t.Fatal("direct synthetic runtime acquired durable-manager semantics")
	}
}

func TestManagerBindRollbackRejectsSameContentRevisionABA(t *testing.T) {
	db := localEpochFileStore(t)
	wrapped := &localEpochCASRepository{repository: db}
	interfere := false
	var concurrent json.RawMessage
	wrapped.compare = func(ctx context.Context, before, after json.RawMessage) (bool, error) {
		committed, err := db.CompareSIPSettings(ctx, before, after)
		if committed && interfere {
			interfere = false
			var sameContent stored
			if err := json.Unmarshal(after, &sameContent); err != nil {
				t.Fatal(err)
			}
			// Another manager committed the exact same logical configuration;
			// its independent revision must fence this attempt's rollback.
			sameContent.ConfigRevision, err = newConfigRevision()
			if err != nil {
				t.Fatal(err)
			}
			concurrent, _ = json.Marshal(sameContent)
			if changed, err := db.CompareSIPSettings(ctx, after, concurrent); err != nil || !changed {
				t.Fatalf("same-content concurrent CAS failed: %v", err)
			}
		}
		return committed, err
	}
	manager := New(context.Background(), wrapped)
	defer manager.Close()
	config := localEpochConfig(t)
	config.Enabled = true
	if _, err := manager.Apply(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	request := manager.Snapshot().Config
	request.Listen = busy.LocalAddr().String()
	interfere = true
	if _, err := manager.Apply(context.Background(), request); err == nil {
		t.Fatal("same-content revision conflict did not prevent stale rollback")
	}
	after, _ := db.AppSetting(context.Background(), SettingKey)
	if concurrent == nil || !bytes.Equal(after.Value, concurrent) || manager.Snapshot().Status.Running {
		t.Fatal("rollback ignored the newer settings revision")
	}
}

func TestManagerMalformedSavedRevisionFailsClosedWithoutReplacement(t *testing.T) {
	db := localEpochFileStore(t)
	config := localEpochConfig(t)
	config.Enabled = true
	prepared, err := prepare(config, stored{}, false)
	if err != nil {
		t.Fatal(err)
	}
	prepared.ConfigRevision = strings.Repeat("X", 32)
	before := saveLocalEpochSettings(t, db, prepared)
	manager := New(context.Background(), db)
	defer manager.Close()
	if manager.Snapshot().Status.Running || manager.Snapshot().Status.Error == "" {
		t.Fatal("malformed nonempty settings revision admitted a listener")
	}
	after, _ := db.AppSetting(context.Background(), SettingKey)
	if !bytes.Equal(before, after.Value) {
		t.Fatal("startup repaired an invalid revision without authority")
	}
}
