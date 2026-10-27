//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vocat/internal/config"
	"vocat/internal/dataowner"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func configureDataOwner(ctx context.Context, databasePath string, instance *os.File) (device.DataNetworkController, error) {
	socket := strings.TrimSpace(os.Getenv("VOCAT_DATA_OWNER_SOCKET"))
	if socket == "" {
		return nil, nil
	}
	client, err := dataowner.NewClient(socket, databasePath, instance)
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		err = client.Ping(probe)
		cancel()
		if err == nil {
			return client, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, device.ErrDataBackendUnavailable
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type dataOwnerDriver struct {
	manager *device.Manager
	open    dataowner.Open
}

func (d *dataOwnerDriver) refresh(ctx context.Context) error {
	if _, err := d.manager.Discover(ctx); err != nil {
		return err
	}
	entry, err := d.manager.Get(d.open.Candidate.ID)
	if err != nil || !entry.Discovered || !device.SameDataPhysical(entry.Candidate, d.open.Candidate) {
		return device.ErrSIMObservation
	}
	return nil
}
func (d *dataOwnerDriver) Set(ctx context.Context, request device.NetworkRequest) (device.NetworkResult, error) {
	if err := d.refresh(ctx); err != nil {
		return device.NetworkResult{}, err
	}
	request.Backend = "qmi"
	return d.manager.SetNetworkForSIM(ctx, d.open.Candidate.ID, d.open.ICCID, d.open.IMSI, request)
}
func (d *dataOwnerDriver) Status(ctx context.Context) (device.NetworkStatus, error) {
	if err := d.refresh(ctx); err != nil {
		return device.NetworkStatus{}, err
	}
	return d.manager.NetworkStatus(ctx, d.open.Candidate.ID)
}
func (d *dataOwnerDriver) Stop(ctx context.Context) error {
	// Discovery can prove the old USB generation disappeared. Cleanup still
	// targets the original manager's capability, never the replacement modem.
	if _, err := d.manager.Discover(ctx); err != nil {
		return err
	}
	return d.manager.StopOwnedNetwork(ctx, d.open.Candidate.ID)
}
func (d *dataOwnerDriver) Close(ctx context.Context) error {
	if d.manager.HasOwnedNetwork(d.open.Candidate.ID) {
		if err := d.Stop(ctx); err != nil {
			return err
		}
	}
	return d.manager.Stop(ctx)
}

func runDataOwner(logger *slog.Logger, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: vocat data-owner (configured through the same environment as vocat serve)")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := device.LoadAuthorizedTestSIMs(cfg.AuthorizedTestSIMsFile); err != nil {
		return err
	}
	socket := strings.TrimSpace(os.Getenv("VOCAT_DATA_OWNER_SOCKET"))
	if socket == "" {
		return errors.New("VOCAT_DATA_OWNER_SOCKET is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	database, err := store.Open(ctx, cfg.DatabasePath)
	cancel()
	if err != nil {
		return err
	}
	defer database.Close()
	journal := newDataClaimJournal(database)
	server, err := dataowner.Listen(dataowner.ServerOptions{
		SocketPath: socket, InstanceLockPath: "/run/vocat/vocat-server.lock", DatabasePath: cfg.DatabasePath,
		Idle: func(ctx context.Context, id string) error {
			pending, err := journal.Pending(ctx, modem.Candidate{ID: id})
			if err != nil || pending {
				return device.ErrDataCleanupPending
			}
			return nil
		},
		Factory: func(ctx context.Context, open dataowner.Open) (dataowner.Driver, error) {
			open.Candidate.USBGeneration = open.Generation
			pending, err := journal.Pending(ctx, open.Candidate)
			if err != nil || pending {
				return nil, device.ErrDataCleanupPending
			}
			manager, err := device.NewManager(device.Options{DataClaims: journal, Logger: logger.With("category", "data_owner"), AllowAutomaticSIMRecovery: func(context.Context, modem.Candidate) bool { return false }, LegacyWriteICCID: func(ctx context.Context, c modem.Candidate) (string, error) {
				return legacyWriteICCID(ctx, database, c)
			}})
			if err != nil {
				return nil, err
			}
			driver := &dataOwnerDriver{manager: manager, open: open}
			if err = manager.Start(ctx); err == nil {
				err = driver.refresh(ctx)
			}
			if err != nil {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = manager.Stop(cleanup)
				return nil, err
			}
			return driver, nil
		},
	})
	if err != nil {
		return err
	}
	lifetime, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("independent QMI data owner listening", "category", "data_owner")
	serveErr := server.Serve(lifetime)
	cleanup, stopCleanup := context.WithTimeout(context.Background(), 90*time.Second)
	defer stopCleanup()
	return errors.Join(serveErr, server.Close(cleanup))
}
