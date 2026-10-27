package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigration33PreservesTaskHistoryRecoveryAndSequences(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v32.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 32; version++ {
		for _, statement := range migrationStatements(version) {
			if _, err := raw.ExecContext(ctx, statement); err != nil {
				t.Fatalf("old schema %d: %v", version, err)
			}
		}
	}
	for _, statement := range []string{
		`PRAGMA user_version = 32`,
		`INSERT INTO devices(id,name,created_at,updated_at) VALUES ('modem','Modem',1,1)`,
		`INSERT INTO automatic_tasks(id,name,enabled,device_id,profile_iccid,task_type,environment,interval_days,start_date,run_time,next_run_at,created_at,updated_at,revert_profile)
		 VALUES (1,'Existing',1,'modem','card','sms','vowifi',1,'2026-10-27','12:00',1,1,1,0)`,
		`INSERT INTO automatic_task_runs(id,task_id,device_id,scheduled_at,status,output,created_at,updated_at) VALUES (1,1,'modem',1,'success','preserved',1,1)`,
		`INSERT INTO automatic_task_recoveries(device_id,token,task_id,phase,payload_json,last_error,updated_at) VALUES ('modem','original-token',1,'executing','{"Version":1}','pending',1)`,
		`UPDATE sqlite_sequence SET seq=100 WHERE name IN ('automatic_tasks','automatic_task_runs')`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTestStore(t, path)
	task, err := db.AutomaticTask(ctx, 1)
	if err != nil || task.RevertProfile || task.Environment != "vowifi" {
		t.Fatal("old task or explicit false revert setting changed", err)
	}
	runs, err := db.ListAutomaticTaskRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Output != "preserved" {
		t.Fatal("run history changed", err)
	}
	recovery, err := db.AutomaticTaskRecovery(ctx, "modem")
	if err != nil || recovery.Token != "original-token" || recovery.Phase != "executing" || string(recovery.Payload) != `{"Version":1}` {
		t.Fatal("migration changed a private interrupted-task fence", err)
	}
	task.ID, task.Environment, task.NextRunAt = 0, "host_ims", time.Now().Add(-time.Minute)
	added, err := db.SaveAutomaticTask(ctx, task)
	if err != nil || added.ID <= 100 {
		t.Fatal("Host IMS task or autoincrement was lost", err)
	}
	claimed, err := db.ClaimDueAvailableAutomaticTasks(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 2 {
		t.Fatal("normal and Host IMS tasks did not remain schedulable", err)
	}
	for _, run := range claimed {
		if run.ID <= 100 {
			t.Fatal("history ID reused")
		}
	}
	rows, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign keys broken by migration")
	}
	rows.Close()
	if err := db.DeleteAutomaticTask(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AutomaticTaskRecovery(ctx, "modem"); err != nil {
		t.Fatal("task deletion erased its restoration fence", err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE automatic_tasks SET environment='unknown' WHERE id=?", added.ID); err == nil {
		t.Fatal("migration removed environment validation")
	}
}
