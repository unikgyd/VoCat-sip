package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreConnectionsReapplySafetyAfterPhysicalDiscard(t *testing.T) {
	for _, useURI := range []bool{false, true} {
		t.Run(fmt.Sprintf("uri=%t", useURI), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reconnected.db")
			if useURI {
				// A caller-supplied URI remains unchanged. Its unsafe connection
				// pragmas must be overridden before every connection enters the pool.
				uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
				query := url.Values{}
				query.Add("_pragma", "foreign_keys(0)")
				query.Add("_pragma", "busy_timeout(1)")
				query.Add("_pragma", "synchronous(OFF)")
				uri.RawQuery = query.Encode()
				path = uri.String()
			}
			database := openTestStore(t, path)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := database.db.ExecContext(ctx, `CREATE TABLE store_connection_parent(id INTEGER PRIMARY KEY);
				CREATE TABLE store_connection_child(parent INTEGER REFERENCES store_connection_parent(id));
				INSERT INTO store_connection_parent(id) VALUES(1)`)
			if err != nil {
				t.Fatal(err)
			}
			for iteration := 0; iteration < 3; iteration++ {
				assertStoreConnectionSafety(t, database)
				if _, err := database.db.ExecContext(ctx, "INSERT INTO store_connection_child(parent) VALUES(99)"); err == nil {
					t.Fatal("a reconnected Store accepted an invalid foreign key")
				}
				if _, err := database.db.ExecContext(ctx, "PRAGMA foreign_keys=OFF; PRAGMA busy_timeout=1; PRAGMA synchronous=OFF; PRAGMA journal_mode=DELETE"); err != nil {
					t.Fatal(err)
				}
				discardStorePhysicalConnection(t, database)
			}
			assertStoreConnectionSafety(t, database)
			var count int
			if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM store_connection_parent").Scan(&count); err != nil || count != 1 {
				t.Fatalf("reconnection changed the original database: count=%d err=%v", count, err)
			}
		})
	}
}

func TestStoreConnectionsKeepSafetyAfterCancelledTransaction(t *testing.T) {
	database := openTestStore(t, filepath.Join(t.TempDir(), "cancelled.db"))
	ctx, cancel := context.WithCancel(context.Background())
	connection, err := database.db.Conn(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		_ = connection.Close()
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO app_settings(key,value_json,sensitive,updated_at) VALUES('connection.cancel.test','{}',1,0)`)
	cancel()
	if err != nil {
		_ = tx.Rollback()
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		_ = connection.Close()
		t.Fatal("cancelled transaction unexpectedly committed")
	}
	_ = tx.Rollback() // awaitDone may already own the actual rollback.
	_ = connection.Close()
	if _, err := database.AppSetting(context.Background(), "connection.cancel.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled transaction left a setting: %v", err)
	}
	assertStoreConnectionSafety(t, database)
	discardStorePhysicalConnection(t, database)
	assertStoreConnectionSafety(t, database)
}

func TestStoreConnectionSafetySurvivesCASFailureAndReconnection(t *testing.T) {
	for _, fixture := range sipPrivateCASFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			database := openTestStore(t, filepath.Join(t.TempDir(), "private.db"))
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if ok, err := fixture.compare(database, cancelled, nil, json.RawMessage(`{"generation":1}`)); ok || err == nil {
				t.Fatalf("cancelled CAS authorized a write: ok=%t err=%v", ok, err)
			}
			assertStoreConnectionSafety(t, database)
			ctx := context.Background()
			_, err := database.db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE store_connection_deferred_parent(id INTEGER PRIMARY KEY);
				CREATE TABLE store_connection_deferred_child(parent INTEGER REFERENCES store_connection_deferred_parent(id) DEFERRABLE INITIALLY DEFERRED);
				CREATE TRIGGER store_connection_commit_failure AFTER INSERT ON app_settings
				WHEN NEW.key='%s' BEGIN INSERT INTO store_connection_deferred_child(parent) VALUES(99); END;`, fixture.key))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{"generation":1}`)); ok || err == nil || !strings.Contains(err.Error(), "commit SIP private CAS; result unknown") {
				t.Fatalf("failed COMMIT authorized CAS: ok=%t err=%v", ok, err)
			}
			if _, err := database.AppSetting(ctx, fixture.key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed COMMIT exposed its setting: %v", err)
			}
			assertStoreConnectionSafety(t, database)
			discardStorePhysicalConnection(t, database)
			assertStoreConnectionSafety(t, database)
			if _, err := database.db.ExecContext(ctx, "DROP TRIGGER store_connection_commit_failure"); err != nil {
				t.Fatal(err)
			}
			if ok, err := fixture.compare(database, ctx, nil, json.RawMessage(`{"generation":2}`)); !ok || err != nil {
				t.Fatalf("configured replacement connection cannot commit CAS: ok=%t err=%v", ok, err)
			}
		})
	}
}

func assertStoreConnectionSafety(t *testing.T, database *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, pragma := range []struct {
		query string
		want  int
	}{{"PRAGMA foreign_keys", 1}, {"PRAGMA busy_timeout", 5000}, {"PRAGMA synchronous", 2}} {
		var actual int
		if err := database.db.QueryRowContext(ctx, pragma.query).Scan(&actual); err != nil || actual != pragma.want {
			t.Fatalf("%s=%d, want %d: %v", pragma.query, actual, pragma.want, err)
		}
	}
	var journal string
	if err := database.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal=%q, want wal: %v", journal, err)
	}
}

func discardStorePhysicalConnection(t *testing.T, database *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		_ = connection.Close()
		t.Fatalf("physical connection was not discarded: %v", err)
	}
	_ = connection.Close()
}

func TestStoreConnectorFailsClosedBeforeGivingConnectionsToPool(t *testing.T) {
	for _, failure := range []string{
		"PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys", "PRAGMA busy_timeout", "PRAGMA synchronous", "PRAGMA journal_mode",
	} {
		t.Run(failure, func(t *testing.T) {
			connection := &storeConnectorTestConn{failure: failure}
			base := &storeConnectorTestBase{connection: connection, original: &storeConnectorTestDriver{}}
			connector := storeConnector{base: base}
			if connector.Driver() != base.original {
				t.Fatal("wrapper replaced the registered driver")
			}
			if actual, err := connector.Connect(context.Background()); actual != nil || err == nil || !connection.closed {
				t.Fatalf("uninitialized physical connection escaped: connection=%v error=%v closed=%t", actual, err, connection.closed)
			}
		})
	}
	for _, query := range []string{"PRAGMA foreign_keys", "PRAGMA busy_timeout", "PRAGMA synchronous", "PRAGMA journal_mode"} {
		t.Run("ignored "+query, func(t *testing.T) {
			connection := &storeConnectorTestConn{ignored: query}
			base := &storeConnectorTestBase{connection: connection, original: &storeConnectorTestDriver{}}
			if actual, err := (storeConnector{base: base}).Connect(context.Background()); actual != nil || err == nil || !connection.closed {
				t.Fatalf("ignored safety setting escaped: connection=%v error=%v closed=%t", actual, err, connection.closed)
			}
		})
	}
}

func TestStoreConnectorForwardsContextAndClosesOnCancellation(t *testing.T) {
	type contextKey struct{}
	for _, stage := range []string{"before connect", "during connect", "during initialize", "success"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "same context"))
			defer cancel()
			connection := &storeConnectorTestConn{expected: ctx}
			base := &storeConnectorTestBase{connection: connection, original: &storeConnectorTestDriver{}, expected: ctx}
			switch stage {
			case "before connect":
				cancel()
			case "during connect":
				base.cancel = cancel
			case "during initialize":
				connection.cancel = cancel
			}
			actual, err := (storeConnector{base: base}).Connect(ctx)
			if stage == "success" {
				if actual != connection || err != nil || connection.closed {
					t.Fatalf("configured connection rejected: %v", err)
				}
				_ = actual.Close()
				return
			}
			if actual != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled connection escaped: connection=%v error=%v", actual, err)
			}
			if stage == "before connect" {
				if base.calls != 0 || connection.closed {
					t.Fatal("already-cancelled Connect opened a physical connection")
				}
			} else if !connection.closed {
				t.Fatal("cancellation leaked its physical connection")
			}
		})
	}
}

func TestStoreDSNConnectorPreservesTheRegisteredDriverAndExactDSN(t *testing.T) {
	path := "file:relative%20name.db?mode=rwc&cache=private&_pragma=busy_timeout(1)"
	connection := &storeConnectorTestConn{}
	original := &storeConnectorTestDriver{connection: connection}
	connector := storeDSNConnector{path: path, driver: original}
	actual, err := connector.Connect(context.Background())
	if err != nil || actual != connection || original.openedPath != path || connector.Driver() != original {
		t.Fatalf("DSN or registered driver changed: path=%q error=%v", original.openedPath, err)
	}
	_ = actual.Close()
	original.openedPath = ""
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if actual, err := connector.Connect(ctx); actual != nil || !errors.Is(err, context.Canceled) || original.openedPath != "" {
		t.Fatal("already-cancelled fallback connector invoked driver.Open")
	}
}

type storeConnectorTestDriver struct {
	connection driver.Conn
	openedPath string
}

func (d *storeConnectorTestDriver) Open(path string) (driver.Conn, error) {
	d.openedPath = path
	if d.connection == nil {
		return nil, errors.New("unexpected direct driver Open")
	}
	return d.connection, nil
}

type storeConnectorTestBase struct {
	connection *storeConnectorTestConn
	original   driver.Driver
	expected   context.Context
	cancel     context.CancelFunc
	calls      int
}

func (c *storeConnectorTestBase) Driver() driver.Driver { return c.original }

func (c *storeConnectorTestBase) Connect(ctx context.Context) (driver.Conn, error) {
	c.calls++
	if c.expected != nil && ctx != c.expected {
		return nil, errors.New("connector lost caller context")
	}
	if c.cancel != nil {
		c.cancel()
	}
	return c.connection, nil
}

type storeConnectorTestConn struct {
	closed   bool
	failure  string
	ignored  string
	expected context.Context
	cancel   context.CancelFunc
}

func (*storeConnectorTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (*storeConnectorTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected Begin")
}
func (c *storeConnectorTestConn) Close() error { c.closed = true; return nil }

func (c *storeConnectorTestConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if c.expected != nil && ctx != c.expected {
		return nil, errors.New("initializer lost caller context")
	}
	if query == c.failure {
		return nil, errors.New("synthetic safety pragma failure")
	}
	if c.cancel != nil {
		c.cancel()
	}
	return driver.RowsAffected(0), nil
}

func (c *storeConnectorTestConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.expected != nil && ctx != c.expected {
		return nil, errors.New("verification lost caller context")
	}
	if query == c.failure {
		return nil, errors.New("synthetic safety verification failure")
	}
	values := map[string]driver.Value{
		"PRAGMA foreign_keys": int64(1), "PRAGMA busy_timeout": int64(5000), "PRAGMA synchronous": int64(2),
		"PRAGMA journal_mode": "wal", "SELECT file FROM pragma_database_list WHERE name='main'": "/synthetic/private.db",
	}
	value, ok := values[query]
	if !ok {
		return nil, errors.New("unexpected verification query")
	}
	if c.ignored == query {
		if query == "PRAGMA journal_mode" {
			value = "memory"
		} else {
			value = int64(0)
		}
	}
	return &storeConnectorTestRows{value: value}, nil
}

type storeConnectorTestRows struct {
	value driver.Value
	done  bool
}

func (*storeConnectorTestRows) Columns() []string { return []string{"value"} }
func (*storeConnectorTestRows) Close() error      { return nil }
func (r *storeConnectorTestRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	values[0] = r.value
	return nil
}

var _ driver.Connector = storeConnector{}
var _ driver.ExecerContext = (*storeConnectorTestConn)(nil)
var _ driver.QueryerContext = (*storeConnectorTestConn)(nil)
