//go:build !linux

package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/store"
	"vocat/internal/vowifi/ims"
)

type unsupportedHostIMSInventory struct{}

func (unsupportedHostIMSInventory) List() []device.Device {
	panic("unsupported factory must not discover/touch hardware")
}

func TestHostIMSFactoryUnsupportedIsOptionalAndInert(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "line", Name: "Line"}); err != nil {
		t.Fatal(err)
	}
	authorizations := 0
	factory, err := buildHostIMSFactory(context.Background(), hostIMSFactoryOptions{Store: database, Devices: unsupportedHostIMSInventory{}, IMSConfig: func(context.Context, store.Device) (ims.Config, error) { return ims.Config{}, nil }, Authorize: func(context.Context, hostims.Selection) error { authorizations++; return hostims.ErrDenied }})
	if err != nil || factory == nil {
		t.Fatal("optional unsupported platform prevented app construction", err)
	}
	line, err := factory.Factory(context.Background(), "line")
	if err != nil {
		t.Fatal(err)
	}
	if err := line.Start(context.Background()); err == nil {
		t.Fatal("unsupported platform pretended to register")
	}
	if authorizations != 0 {
		t.Fatal("unsupported platform attempted a bearer operation")
	}
	if err := line.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHostIMSFactoryUnsupportedInventoryGuardFailsClosed(t *testing.T) {
	config, entry, _ := hostIMSMappingFixture()
	if id, err := hostIMSInventoryPhysicalID(config, []device.Device{entry}); id != "" || !errors.Is(err, errHostIMSFactory) {
		t.Fatal("unsupported platform guessed a physical guard", id, err)
	}
}
