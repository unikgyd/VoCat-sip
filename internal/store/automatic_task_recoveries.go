package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrAutomaticTaskRecoveryPending = errors.New("automatic task environment restoration is pending")

// Payload contains private recovery state, never an API response. It contains
// no dial/SMS payload and cannot be used to replay the interrupted action.
type AutomaticTaskRecovery struct {
	DeviceID  string          `json:"deviceId"`
	Token     string          `json:"-"`
	TaskID    int64           `json:"taskId"`
	Phase     string          `json:"phase"`
	Payload   json.RawMessage `json:"-"`
	LastError string          `json:"lastError"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

const automaticTaskRecoverySelect = `SELECT device_id, token, task_id, phase, payload_json, last_error, updated_at FROM automatic_task_recoveries`

func (s *Store) AutomaticTaskRecovery(ctx context.Context, id string) (AutomaticTaskRecovery, error) {
	var value AutomaticTaskRecovery
	var payload string
	var updated int64
	err := s.db.QueryRowContext(ctx, automaticTaskRecoverySelect+` WHERE device_id = ?`, id).Scan(
		&value.DeviceID, &value.Token, &value.TaskID, &value.Phase, &payload, &value.LastError, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, err
	}
	value.Payload, value.UpdatedAt = json.RawMessage(payload), time.Unix(updated, 0).UTC()
	return value, nil
}

func (s *Store) RequireAutomaticTaskRecovered(ctx context.Context, id string) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM automatic_task_recoveries WHERE device_id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read task restoration fence: %w", err)
	}
	return ErrAutomaticTaskRecoveryPending
}

func (s *Store) ListAutomaticTaskRecoveries(ctx context.Context) ([]AutomaticTaskRecovery, error) {
	rows, err := s.db.QueryContext(ctx, automaticTaskRecoverySelect+` ORDER BY updated_at, device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AutomaticTaskRecovery, 0)
	for rows.Next() {
		var value AutomaticTaskRecovery
		var payload string
		var updated int64
		if err := rows.Scan(&value.DeviceID, &value.Token, &value.TaskID, &value.Phase, &payload, &value.LastError, &updated); err != nil {
			return nil, err
		}
		value.Payload, value.UpdatedAt = json.RawMessage(payload), time.Unix(updated, 0).UTC()
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) CreateAutomaticTaskRecovery(ctx context.Context, value AutomaticTaskRecovery) error {
	if value.DeviceID == "" || value.Token == "" || len(value.Payload) > 65536 || !json.Valid(value.Payload) {
		return errors.New("invalid task recovery record")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO automatic_task_recoveries (device_id, token, task_id, phase, payload_json, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(device_id) DO NOTHING`, value.DeviceID, value.Token, value.TaskID, value.Phase, string(value.Payload), value.LastError, time.Now().UTC().Unix())
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrAutomaticTaskRecoveryPending
	}
	return nil
}

func (s *Store) UpdateAutomaticTaskRecovery(ctx context.Context, value AutomaticTaskRecovery) error {
	if len(value.Payload) > 65536 || !json.Valid(value.Payload) {
		return errors.New("invalid task recovery payload")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE automatic_task_recoveries SET phase = ?, payload_json = ?, last_error = ?, updated_at = ? WHERE device_id = ? AND token = ?`,
		value.Phase, string(value.Payload), value.LastError, time.Now().UTC().Unix(), value.DeviceID, value.Token)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAutomaticTaskRecovery(ctx context.Context, id, token string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM automatic_task_recoveries WHERE device_id = ? AND token = ? AND phase = 'restored'`, id, token)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrAutomaticTaskRecoveryPending
	}
	return nil
}
