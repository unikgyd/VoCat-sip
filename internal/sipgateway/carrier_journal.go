package sipgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"vocat/internal/store"
)

const CallOperationSettingKey = "sip.gateway.call-operations.private.v1"

// ReserveWebCall uses the same durable, fail-closed journal as SIP dialing,
// including when the optional SIP listener is disabled. Never retry Dial after
// an ambiguous HTTP result with a new operation ID automatically.
func (m *Manager) ReserveWebCall(ctx context.Context, operation, deviceID, number string) error {
	if m == nil {
		return errors.New("carrier operation store unavailable")
	}
	_, err := m.journal.reserve(ctx, "web-phone-v1", "authenticated", operation, deviceID, number)
	return err
}

type callAttempt struct {
	Fingerprint string    `json:"fingerprint"`
	Until       time.Time `json:"until"`
}
type callAttempts struct {
	Version  int                    `json:"version"`
	Attempts map[string]callAttempt `json:"attempts"`
}
type callJournal struct {
	mu sync.Mutex
	db repository
}

func hashCallOperation(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Reserve durably BEFORE any carrier Dial. Same Call-ID/CSeq/branch retries do
// not charge again after a listener restart. No raw identity/phone is stored.
// An uncertain attempt remains reserved for 24 hours; users start a NEW call
// explicitly rather than having the gateway silently replay a paid operation.
func (j *callJournal) reserve(ctx context.Context, realm, user, callID, line, number string) (int, error) {
	if j == nil || j.db == nil {
		return 503, errors.New("carrier operation store unavailable")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	value, err := j.db.AppSetting(ctx, CallOperationSettingKey)
	state := callAttempts{Version: 1, Attempts: map[string]callAttempt{}}
	if err == nil {
		if len(value.Value) > 512*1024 || json.Unmarshal(value.Value, &state) != nil || state.Version != 1 || state.Attempts == nil {
			return 503, errors.New("carrier operation store is invalid")
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return 503, err
	}
	if len(state.Attempts) > 2048 {
		return 503, errors.New("carrier operation store exceeds limit")
	}
	now := time.Now().UTC()
	for key, attempt := range state.Attempts {
		keyBytes, keyErr := hex.DecodeString(key)
		fingerprintBytes, fingerprintErr := hex.DecodeString(attempt.Fingerprint)
		if keyErr != nil || fingerprintErr != nil || len(keyBytes) != 32 || len(fingerprintBytes) != 32 || attempt.Until.IsZero() {
			return 503, errors.New("carrier operation store contains invalid entries")
		}
		if !attempt.Until.After(now) {
			delete(state.Attempts, key)
		}
	}
	key := hashCallOperation(realm + "\n" + user + "\n" + callID)
	fingerprint := hashCallOperation(line + "\n" + number)
	if attempt, exists := state.Attempts[key]; exists {
		if attempt.Fingerprint != fingerprint {
			return 400, errors.New("call retry changed its destination")
		}
		return 482, errors.New("call already attempted; outcome may be unknown")
	}
	if len(state.Attempts) >= 2048 {
		return 503, errors.New("carrier operation store capacity reached")
	}
	state.Attempts[key] = callAttempt{Fingerprint: fingerprint, Until: now.Add(24 * time.Hour)}
	encoded, err := json.Marshal(state)
	if err == nil {
		err = j.db.UpsertAppSetting(ctx, store.AppSetting{Key: CallOperationSettingKey, Value: encoded, Sensitive: true})
	}
	if err != nil {
		return 503, err
	}
	return 0, nil
}
