package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type IKERetryStore struct{ Store *store.Store }

func (s IKERetryStore) LoadIKERetry(ctx context.Context) (json.RawMessage, error) {
	if s.Store == nil {
		return nil, vowifi.ErrIKERetryStore
	}
	value, err := s.Store.AppSetting(ctx, store.IKERetryJournalKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !value.Sensitive || len(value.Value) == 0 || len(value.Value) > vowifi.MaxIKERetryJournalBytes {
		return nil, vowifi.ErrIKERetryStore
	}
	return bytes.Clone(value.Value), nil
}

func (s IKERetryStore) CompareIKERetry(ctx context.Context, before, after json.RawMessage) (bool, error) {
	if s.Store == nil {
		return false, vowifi.ErrIKERetryStore
	}
	return s.Store.CompareIKERetryJournal(ctx, before, after)
}
