package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
)

func TestHostIMSDurableControllerFencesDisabledColdOwnerWithoutPreparing(t *testing.T) {
	database := newRegionTestStore(t)
	config := store.Device{ID: "cold-host", Name: "Cold", IMSMode: store.IMSModeHost, IMSEnabled: false, ModemIMEI: "123456789012340"}
	if err := database.UpsertDevice(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	manager := hostimsruntime.New(hostimsruntime.Options{Factory: func(context.Context, string) (*hostimsruntime.Runtime, error) {
		t.Error("disabled cold line unexpectedly prepared")
		return nil, hostimsruntime.ErrPrepare
	}})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if err := manager.Ensure(context.Background(), config.ID); err != nil {
		t.Fatal(err)
	}
	controller := newDurableHostIMSController(manager, database)
	if err := controller.StopDevice(context.Background(), config.ID); err != nil {
		t.Fatal("clean disabled line could not stop", err)
	}
	sum := sha256.Sum256([]byte("imei:" + config.ModemIMEI))
	key := hex.EncodeToString(sum[:])
	journal := hostims.SQLiteClaimJournal{Database: database}
	if ok, err := journal.Compare(context.Background(), key, nil, []byte(`{"unknown_cold_record":true}`)); err != nil || !ok {
		t.Fatal(err)
	}
	if err := controller.StopDevice(context.Background(), config.ID); !errors.Is(err, hostims.ErrUncertain) {
		t.Fatal("cold durable claim allowed mode/delete release", err)
	}
	if ok, err := journal.Compare(context.Background(), key, []byte(`{"unknown_cold_record":true}`), nil); err != nil || !ok {
		t.Fatal(err)
	}
	if err := controller.StopDevice(context.Background(), config.ID); err != nil {
		t.Fatal("removed record stayed fenced", err)
	}
}

func TestHostIMSDurableControllerDoesNotGuessMissingSavedEquipment(t *testing.T) {
	database := newRegionTestStore(t)
	manager := hostimsruntime.New(hostimsruntime.Options{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	controller := newDurableHostIMSController(manager, database)
	for _, mode := range []string{store.IMSModeHost, store.IMSModeModule, store.IMSModeVoWiFi} {
		config := store.Device{ID: "missing-imei", Name: "No saved IMEI", IMSMode: mode}
		if err := database.UpsertDevice(context.Background(), config); err != nil {
			t.Fatal(err)
		}
		err := controller.StopDevice(context.Background(), config.ID)
		if err != nil {
			t.Fatal("empty durable namespace did not allow an unbound line to stop", err)
		}
	}
	journal := hostims.SQLiteClaimJournal{Database: database}
	sum := sha256.Sum256([]byte("imei:123456789012340"))
	key := hex.EncodeToString(sum[:])
	if ok, err := journal.Compare(context.Background(), key, nil, []byte(`{"unattributed":true}`)); err != nil || !ok {
		t.Fatal(err)
	}
	if err := controller.StopDevice(context.Background(), "missing-imei"); !errors.Is(err, hostims.ErrUncertain) {
		t.Fatal("unbound line guessed which modem owned a durable row", err)
	}
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "bound-other", Name: "Other", IMSMode: store.IMSModeHost, ModemIMEI: "123456789012341"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.StopDevice(context.Background(), "bound-other"); err != nil {
		t.Fatal("properly bound other equipment was blocked by foreign claim", err)
	}
	if err := controller.StopDevice(context.Background(), "missing-config"); !errors.Is(err, hostims.ErrUncertain) {
		t.Fatal("unreadable config was assumed clean", err)
	}
}
