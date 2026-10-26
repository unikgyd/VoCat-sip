package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"vocat/internal/callprompt"
)

func TestCallAnsweringRevisionSIMIsolationAndPrivateAudio(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t, filepath.Join(t.TempDir(), "answering.db"))
	settings := DefaultCallAnswering("8901000000000000000")
	settings.Enabled = true
	pcm := make([]byte, 16000)
	pcm[0] = 42
	saved, err := db.SaveCallAnswering(ctx, settings, "", &callprompt.Recording{Name: "message.wav", Format: "wav", PCM: pcm})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision == "" || saved.Prompt == nil || saved.Prompt.DurationMS != 1000 {
		t.Fatal("missing revision or computed duration")
	}
	metadata, err := db.CallAnswering(ctx, settings.ICCID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil || metadata.Prompt.PCM != nil || strings.Contains(string(encoded), "PCM") {
		t.Fatal("audio leaked into metadata response")
	}
	_, audio, err := db.CallAnsweringAudio(ctx, settings.ICCID, saved.Revision)
	if err != nil || len(audio) != 16000 || audio[0] != 42 {
		t.Fatal("original audio unavailable", err)
	}
	other, err := db.CallAnswering(ctx, "8901000000000000001")
	if err != nil || other.Enabled || other.Prompt != nil {
		t.Fatal("settings crossed subscriptions", err)
	}
	if _, err := db.SaveCallAnswering(ctx, saved, "stale", nil); !errors.Is(err, ErrAnsweringConflict) {
		t.Fatal("stale editor overwrote settings", err)
	}
	updated, err := db.SaveCallAnswering(ctx, saved, saved.Revision, &callprompt.Recording{})
	if err != nil || updated.Enabled || updated.Prompt != nil {
		t.Fatal("clearing prompt did not disable automatic answer", err)
	}
	if _, _, err := db.CallAnsweringAudio(ctx, settings.ICCID, saved.Revision); !errors.Is(err, ErrAnsweringConflict) {
		t.Fatal("old playback borrowed replacement prompt", err)
	}
}

func TestAutomaticTaskRecoverySurvivesReopenAndRequiresOriginalRestoredToken(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	record := AutomaticTaskRecovery{DeviceID: "line-a", Token: "original-token", TaskID: 7, Phase: "executing", Payload: json.RawMessage(`{"original":"private environment"}`)}
	if err := db.CreateAutomaticTaskRecovery(ctx, record); err != nil {
		t.Fatal(err)
	}
	other := record
	other.Token = "successor-token"
	if err := db.CreateAutomaticTaskRecovery(ctx, other); !errors.Is(err, ErrAutomaticTaskRecoveryPending) {
		t.Fatal("pending recovery overwritten", err)
	}
	if err := db.RequireAutomaticTaskRecovered(ctx, "line-b"); err != nil {
		t.Fatal("other modem blocked", err)
	}
	if err := db.DeleteAutomaticTaskRecovery(ctx, record.DeviceID, record.Token); !errors.Is(err, ErrAutomaticTaskRecoveryPending) {
		t.Fatal("unfinished task fence removed", err)
	}
	if _, err := db.db.ExecContext(ctx, "PRAGMA user_version=29"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal("repaired migration erased/blocked new schema", err)
	}
	got, err := db.AutomaticTaskRecovery(ctx, record.DeviceID)
	if err != nil || got.Token != record.Token || string(got.Payload) != string(record.Payload) {
		t.Fatal("reopen lost restoration state", err)
	}
	public, _ := json.Marshal(got)
	if strings.Contains(string(public), record.Token) || strings.Contains(string(public), "private environment") {
		t.Fatal("private restoration payload leaked")
	}
	other.Phase = "restored"
	if err := db.UpdateAutomaticTaskRecovery(ctx, other); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign token changed recovery", err)
	}
	record.Phase = "restored"
	if err := db.UpdateAutomaticTaskRecovery(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAutomaticTaskRecovery(ctx, record.DeviceID, other.Token); !errors.Is(err, ErrAutomaticTaskRecoveryPending) {
		t.Fatal("old closer cleared successor", err)
	}
	if err := db.DeleteAutomaticTaskRecovery(ctx, record.DeviceID, record.Token); err != nil {
		t.Fatal(err)
	}
	if err := db.RequireAutomaticTaskRecovered(ctx, record.DeviceID); err != nil {
		t.Fatal("restored original token did not release line", err)
	}
}
