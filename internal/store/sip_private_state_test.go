package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sipPrivateCASFixture struct {
	name    string
	key     string
	maximum int
	compare func(*Store, context.Context, json.RawMessage, json.RawMessage) (bool, error)
}

func sipPrivateCASFixtures() []sipPrivateCASFixture {
	return []sipPrivateCASFixture{
		{"ledger", "sip.gateway.sms-ledger.private.v1", MaxSMSLedgerBytes, (*Store).CompareSMSLedger},
		{"settings", "sip.gateway.private.v1", MaxSIPSettingsBytes, (*Store).CompareSIPSettings},
	}
}

func TestSIPPrivateCASRawImagesAndSensitiveFence(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
			ctx := context.Background()
			before := json.RawMessage(" \n{\"generation\" : 1, \"payload\" : [ true ]}\t")
			after := json.RawMessage("{\"generation\":2,\"payload\":[true]}\n")
			if _, err := database.db.ExecContext(ctx, `INSERT INTO app_settings(key,value_json,sensitive,updated_at) VALUES(?,?,1,0)`, fixture.key, string(before)); err != nil {
				t.Fatal(err)
			}
			canonicalBefore := json.RawMessage(`{"generation":1,"payload":[true]}`)
			if ok, err := fixture.compare(database, ctx, canonicalBefore, after); err != nil || ok {
				t.Fatalf("canonicalized legacy snapshot acquired CAS: ok=%v err=%v", ok, err)
			}
			if ok, err := fixture.compare(database, ctx, before, after); err != nil || !ok {
				t.Fatalf("exact legacy snapshot did not acquire CAS: ok=%v err=%v", ok, err)
			}
			stored, err := database.AppSetting(ctx, fixture.key)
			if err != nil || !stored.Sensitive || !bytes.Equal(stored.Value, after) {
				t.Fatal("CAS normalized or exposed the committed raw image")
			}
			if ok, err := fixture.compare(database, ctx, before, nil); err != nil || ok {
				t.Fatalf("stale snapshot deleted current state: ok=%v err=%v", ok, err)
			}
			if _, err := database.db.ExecContext(ctx, `UPDATE app_settings SET sensitive=0 WHERE key=?`, fixture.key); err != nil {
				t.Fatal(err)
			}
			for _, change := range [][2]json.RawMessage{{nil, before}, {after, before}, {after, nil}} {
				if ok, err := fixture.compare(database, ctx, change[0], change[1]); err != nil || ok {
					t.Fatalf("nonprivate record acquired CAS: ok=%v err=%v", ok, err)
				}
			}
			if _, err := database.db.ExecContext(ctx, `UPDATE app_settings SET sensitive=1 WHERE key=?`, fixture.key); err != nil {
				t.Fatal(err)
			}
			if ok, err := fixture.compare(database, ctx, after, nil); err != nil || !ok {
				t.Fatalf("exact private record was not deleted: ok=%v err=%v", ok, err)
			}
			if _, err := database.AppSetting(ctx, fixture.key); !errors.Is(err, ErrNotFound) {
				t.Fatal("deleted state remains visible")
			}
		})
	}
}

func TestSIPPrivateCASIndependentFileStoresHaveOneWinner(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private.db")
			one, two := openTestStore(t, path), openTestStore(t, path)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			start := make(chan struct{})
			var group sync.WaitGroup
			var winners atomic.Int32
			failures := make(chan error, 24)
			for index := 0; index < 24; index++ {
				group.Add(1)
				go func(index int) {
					defer group.Done()
					<-start
					database := one
					if index%2 != 0 {
						database = two
					}
					ok, err := fixture.compare(database, ctx, nil, json.RawMessage(fmt.Sprintf(`{"owner":%d}`, index)))
					if err != nil {
						failures <- err
					} else if ok {
						winners.Add(1)
					}
				}(index)
			}
			close(start)
			group.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if winners.Load() != 1 {
				t.Fatalf("insert winners=%d, want 1", winners.Load())
			}
			stored, err := one.AppSetting(ctx, fixture.key)
			if err != nil {
				t.Fatal(err)
			}
			newImage := json.RawMessage(`{"owner":"replacement"}`)
			if ok, err := fixture.compare(two, ctx, stored.Value, newImage); err != nil || !ok {
				t.Fatalf("current cross-store CAS failed: ok=%v err=%v", ok, err)
			}
			if ok, err := fixture.compare(one, ctx, stored.Value, nil); err != nil || ok {
				t.Fatalf("stale cross-store delete acquired CAS: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestSIPPrivateCASForcesFullDurabilityOnTheActualConnection(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
			ctx := context.Background()
			if _, err := database.db.ExecContext(ctx, "PRAGMA synchronous=OFF"); err != nil {
				t.Fatal(err)
			}
			if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{"generation":1}`)); err != nil || !ok {
				t.Fatalf("file CAS failed: ok=%v err=%v", ok, err)
			}
			var synchronous int
			if err := database.db.QueryRowContext(ctx, "PRAGMA main.synchronous").Scan(&synchronous); err != nil || synchronous < 2 {
				t.Fatalf("actual pooled connection is not FULL: level=%d err=%v", synchronous, err)
			}
		})
	}
}

func TestSIPPrivateCASRejectsEphemeralBackingAndUnsafeJournals(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			for _, path := range []string{":memory:", "file::memory:?cache=shared", "file:sip-private-test?mode=memory&cache=shared"} {
				database := openTestStore(t, path)
				if ok, err := fixture.compare(database, context.Background(), nil, json.RawMessage(`{}`)); err == nil || ok {
					t.Fatalf("ephemeral database claimed durable CAS: ok=%v err=%v", ok, err)
				}
				if _, err := database.AppSetting(context.Background(), fixture.key); !errors.Is(err, ErrNotFound) {
					t.Fatal("rejected ephemeral CAS changed state")
				}
			}
			for _, journal := range []string{"off", "memory"} {
				database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
				var actual string
				if err := database.db.QueryRowContext(context.Background(), "PRAGMA journal_mode="+journal).Scan(&actual); err != nil || actual != journal {
					t.Fatalf("cannot install synthetic unsafe journal: journal=%q err=%v", actual, err)
				}
				if ok, err := fixture.compare(database, context.Background(), nil, json.RawMessage(`{}`)); err == nil || ok {
					t.Fatalf("unsafe journal claimed durable CAS: ok=%v err=%v", ok, err)
				}
			}
		})
	}
}

func TestSIPPrivateCASLimitsInvalidImagesAndCancellationLeaveNoState(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
			invalid := []json.RawMessage{{}, []byte(`{`), {'"', 0xff, '"'}, []byte(`{"payload":"` + strings.Repeat("x", fixture.maximum) + `"}`)}
			if ok, err := fixture.compare(database, context.Background(), nil, nil); err == nil || ok {
				t.Fatal("two absent images were accepted")
			}
			for _, image := range invalid {
				for _, pair := range [][2]json.RawMessage{{nil, image}, {image, json.RawMessage(`{}`)}} {
					if ok, err := fixture.compare(database, context.Background(), pair[0], pair[1]); err == nil || ok {
						t.Fatalf("invalid image acquired CAS: ok=%v err=%v", ok, err)
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{}`)); err == nil || ok {
				t.Fatal("cancelled operation acquired CAS")
			}
			if _, err := database.AppSetting(context.Background(), fixture.key); !errors.Is(err, ErrNotFound) {
				t.Fatal("rejected operation changed state")
			}
			maximum := json.RawMessage(`"` + strings.Repeat("x", fixture.maximum-2) + `"`)
			if len(maximum) != fixture.maximum {
				t.Fatal("invalid exact-limit fixture")
			}
			if ok, err := fixture.compare(database, context.Background(), nil, maximum); err != nil || !ok {
				t.Fatalf("exact maximum image rejected: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestSIPPrivateCASCommitErrorDoesNotAuthorizeOrLeakATransaction(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
			ctx := context.Background()
			// The INSERT succeeds but COMMIT fails, exercising the real driver's
			// commit path rather than faking a pre-statement write error.
			_, err := database.db.ExecContext(ctx, fmt.Sprintf(`
				CREATE TABLE sip_cas_parent(id INTEGER PRIMARY KEY);
				CREATE TABLE sip_cas_deferred(parent INTEGER REFERENCES sip_cas_parent(id) DEFERRABLE INITIALLY DEFERRED);
				CREATE TRIGGER sip_cas_commit_failure AFTER INSERT ON app_settings
				WHEN NEW.key='%s' BEGIN INSERT INTO sip_cas_deferred(parent) VALUES(77); END;`, fixture.key))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{"generation":1}`)); err == nil || ok || !strings.Contains(err.Error(), "commit SIP private CAS; result unknown") {
				t.Fatalf("failed COMMIT authorized CAS or lacked unknown classification: ok=%v err=%v", ok, err)
			}
			if _, err := database.AppSetting(ctx, fixture.key); !errors.Is(err, ErrNotFound) {
				t.Fatal("failed COMMIT left its uncommitted key visible")
			}
			var count int
			if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sip_cas_deferred").Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed COMMIT leaked an active transaction: count=%d err=%v", count, err)
			}
			if _, err := database.db.ExecContext(ctx, "DROP TRIGGER sip_cas_commit_failure"); err != nil {
				t.Fatal(err)
			}
			if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{"generation":2}`)); err != nil || !ok {
				t.Fatalf("connection remained poisoned after failed COMMIT: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestSIPPrivateCASDoesNotMutateOtherSettings(t *testing.T) {
	database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
	ctx := context.Background()
	neighbor := "sip.gateway.sms-ledger.private.v1.extra"
	if _, err := database.db.ExecContext(ctx, `INSERT INTO app_settings(key,value_json,sensitive,updated_at) VALUES(?,?,1,0)`, neighbor, `{"unchanged":true}`); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range sipPrivateCASFixtures() {
		if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{"generation":1}`)); err != nil || !ok {
			t.Fatalf("initial private CAS failed: ok=%v err=%v", ok, err)
		}
	}
	if ok, err := database.CompareSMSLedger(ctx, json.RawMessage(`{"generation":1}`), nil); err != nil || !ok {
		t.Fatal("ledger deletion failed")
	}
	for _, key := range []string{neighbor, "sip.gateway.private.v1"} {
		if _, err := database.AppSetting(ctx, key); err != nil {
			t.Fatalf("exact-key CAS changed sibling setting %q: %v", key, err)
		}
	}
}

func TestSIPPrivateCASAcrossProcesses(t *testing.T) {
	if os.Getenv("SIP_PRIVATE_CAS_TEST_CHILD") == "v1" {
		runSIPPrivateCASChild(t)
		return
	}
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private.db")
			database := openTestStore(t, path)
			before := json.RawMessage(`{"generation":0}`)
			if ok, err := fixture.compare(database, context.Background(), nil, before); err != nil || !ok {
				t.Fatal("could not seed cross-process CAS")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			type child struct {
				command *exec.Cmd
				input   *os.File
				scanner *bufio.Scanner
				stderr  *bytes.Buffer
			}
			// Use pipes we own and close, not files, sleeps or shared-memory
			// pseudo-process fixtures. Each child opens its own actual Store.
			var children []*child
			defer func() {
				cancel()
				for _, process := range children {
					_ = process.input.Close()
					if process.command.ProcessState == nil {
						_ = process.command.Process.Kill()
						_ = process.command.Wait()
					}
				}
			}()
			for index := 0; index < 2; index++ {
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSIPPrivateCASAcrossProcesses$", "-test.timeout=20s")
				command.Env = append(os.Environ(), "SIP_PRIVATE_CAS_TEST_CHILD=v1", "SIP_PRIVATE_CAS_TEST_PATH="+path,
					"SIP_PRIVATE_CAS_TEST_API="+fixture.name, fmt.Sprintf("SIP_PRIVATE_CAS_TEST_OWNER=%d", index))
				read, input, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				command.Stdin = read
				output, err := command.StdoutPipe()
				if err != nil {
					_ = read.Close()
					_ = input.Close()
					t.Fatal(err)
				}
				stderr := &bytes.Buffer{}
				command.Stderr = stderr
				if err := command.Start(); err != nil {
					_ = read.Close()
					_ = input.Close()
					t.Fatal(err)
				}
				_ = read.Close()
				children = append(children, &child{command, input, bufio.NewScanner(output), stderr})
			}
			for _, process := range children {
				if !process.scanner.Scan() || process.scanner.Text() != "SIP-CAS-READY" {
					t.Fatal("CAS child did not reach the cross-process barrier")
				}
			}
			for _, process := range children {
				if _, err := fmt.Fprintln(process.input, "GO"); err != nil {
					t.Fatal(err)
				}
				_ = process.input.Close()
			}
			winners := 0
			for _, process := range children {
				if !process.scanner.Scan() {
					t.Fatal("CAS child did not report an outcome")
				}
				switch process.scanner.Text() {
				case "SIP-CAS-WON":
					winners++
				case "SIP-CAS-LOST":
				default:
					t.Fatalf("unexpected CAS child protocol: %q", process.scanner.Text())
				}
				if err := process.command.Wait(); err != nil {
					t.Fatalf("CAS child failed: %v %s", err, process.stderr.String())
				}
			}
			if winners != 1 {
				t.Fatalf("cross-process CAS winners=%d, want 1", winners)
			}
		})
	}
}

func runSIPPrivateCASChild(t *testing.T) {
	t.Helper()
	path := os.Getenv("SIP_PRIVATE_CAS_TEST_PATH")
	relative, err := filepath.Rel(os.TempDir(), path)
	if err != nil || !filepath.IsAbs(path) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.Base(path) != "private.db" || !strings.Contains(relative, "TestSIPPrivateCASAcrossProcesses") {
		t.Fatal("CAS child refuses a non-test database path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var fixture *sipPrivateCASFixture
	for _, candidate := range sipPrivateCASFixtures() {
		if candidate.name == os.Getenv("SIP_PRIVATE_CAS_TEST_API") {
			copy := candidate
			fixture = &copy
		}
	}
	if fixture == nil {
		t.Fatal("CAS child API is unknown")
	}
	fmt.Fprintln(os.Stdout, "SIP-CAS-READY")
	var directive string
	if _, err := fmt.Fscanln(os.Stdin, &directive); err != nil || directive != "GO" {
		t.Fatal("CAS child barrier was not released")
	}
	after, err := json.Marshal(map[string]string{"owner": os.Getenv("SIP_PRIVATE_CAS_TEST_OWNER")})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := fixture.compare(database, ctx, json.RawMessage(`{"generation":0}`), after)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		fmt.Fprintln(os.Stdout, "SIP-CAS-WON")
	} else {
		fmt.Fprintln(os.Stdout, "SIP-CAS-LOST")
	}
}
