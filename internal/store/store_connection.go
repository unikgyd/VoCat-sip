package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
)

// openStoreDatabase keeps the registered SQLite driver (and its registered
// functions) and the caller's DSN unchanged. Safety settings belong to each
// physical connection, not just the first connection opened by a sql.DB pool.
func openStoreDatabase(path string) (*sql.DB, error) {
	registered, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	sqliteDriver := registered.Driver()
	if err := registered.Close(); err != nil {
		return nil, err
	}
	var base driver.Connector = storeDSNConnector{path: path, driver: sqliteDriver}
	if contextual, ok := sqliteDriver.(driver.DriverContext); ok {
		base, err = contextual.OpenConnector(path)
		if err != nil {
			return nil, err
		}
	}
	return sql.OpenDB(storeConnector{base: base}), nil
}

type storeDSNConnector struct {
	path   string
	driver driver.Driver
}

func (c storeDSNConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.driver.Open(c.path)
}

func (c storeDSNConnector) Driver() driver.Driver { return c.driver }

type storeConnector struct{ base driver.Connector }

func (c storeConnector) Driver() driver.Driver { return c.base.Driver() }

func (c storeConnector) Close() error {
	if closer, ok := c.base.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (c storeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, err := c.base.Connect(ctx)
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, err
	}
	if connection == nil {
		return nil, errors.New("SQLite connector returned no connection")
	}
	if err := initializeStoreConnection(ctx, connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

func initializeStoreConnection(ctx context.Context, connection driver.Conn) error {
	execer, ok := connection.(driver.ExecerContext)
	if !ok {
		return errors.New("SQLite connection cannot initialize safety settings with context")
	}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := execer.ExecContext(ctx, pragma, nil); err != nil {
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}
	for _, setting := range []struct {
		query string
		want  int64
	}{
		{"PRAGMA foreign_keys", 1},
		{"PRAGMA busy_timeout", 5000},
	} {
		value, err := storeConnectionValue(ctx, connection, setting.query)
		if err != nil {
			return err
		}
		if actual, ok := value.(int64); !ok || actual != setting.want {
			return fmt.Errorf("%s was not applied", setting.query)
		}
	}
	value, err := storeConnectionValue(ctx, connection, "PRAGMA synchronous")
	if err != nil {
		return err
	}
	if level, ok := value.(int64); !ok || level < 2 {
		return errors.New("SQLite FULL synchronous setting was not applied")
	}
	value, err = storeConnectionValue(ctx, connection, "PRAGMA journal_mode")
	if err != nil {
		return err
	}
	journal, ok := value.(string)
	if !ok {
		return errors.New("cannot verify SQLite journal mode")
	}
	if strings.ToLower(journal) != "wal" {
		// Memory and anonymous temporary stores cannot use WAL. Preserve their
		// existing support without allowing a disk-backed connection to silently
		// enter the pool with a different journal mode. Durable CAS rejects them.
		filename, err := storeConnectionValue(ctx, connection, "SELECT file FROM pragma_database_list WHERE name='main'")
		if err != nil {
			return err
		}
		if name, ok := filename.(string); !ok || name != "" || (strings.ToLower(journal) != "memory" && strings.ToLower(journal) != "delete") {
			return errors.New("SQLite WAL journal setting was not applied")
		}
	}
	return ctx.Err()
}

func storeConnectionValue(ctx context.Context, connection driver.Conn, query string) (driver.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queryer, ok := connection.(driver.QueryerContext)
	if !ok {
		return nil, errors.New("SQLite connection cannot verify safety settings with context")
	}
	rows, err := queryer.QueryContext(ctx, query, nil)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", query, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = rows.Close()
		}
	}()
	if len(rows.Columns()) != 1 {
		return nil, fmt.Errorf("verify %s: unexpected columns", query)
	}
	values := make([]driver.Value, 1)
	if err := rows.Next(values); err != nil {
		return nil, fmt.Errorf("verify %s: %w", query, err)
	}
	value := values[0]
	if err := rows.Next(values); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("verify %s: expected exactly one row", query)
	}
	closed = true
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("finish verifying %s: %w", query, err)
	}
	return value, nil
}
