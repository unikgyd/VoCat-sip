//go:build !linux

package main

import (
	"context"
	"log/slog"
	"os"
	"vocat/internal/device"
)

func configureDataOwner(context.Context, string, *os.File) (device.DataNetworkController, error) {
	return nil, nil
}
func runDataOwner(*slog.Logger, []string) error { return device.ErrDataBackendUnavailable }
