package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"

	"vocat/internal/callrecord"
	"vocat/internal/config"
	"vocat/internal/store"
	"vocat/internal/vowifi/ims"
)

func configureCallRecordings(ctx context.Context, cfg config.Config, database *store.Store, logger *slog.Logger) (*callrecord.Service, error) {
	policy := callrecord.DefaultConfig()
	setting, err := database.AppSetting(ctx, callrecord.SettingKey)
	if err == nil {
		if err = json.Unmarshal(setting.Value, &policy); err != nil {
			return nil, err
		}
		if err = policy.Validate(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	directory := strings.TrimSpace(cfg.RecordingsDirectory)
	if directory == "" {
		directory = filepath.Join(filepath.Dir(cfg.DatabasePath), "recordings")
	}
	return callrecord.New(directory, policy, logger)
}
func attachCallRecordings(config *ims.Config, services []*callrecord.Service) {
	if len(services) == 1 && services[0] != nil {
		config.RecordCall = services[0].Start
	}
}
