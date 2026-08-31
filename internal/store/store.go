// Package store is the SQLite layer: connection setup, migrations, and a small
// typed query helper.
//
// The helper is where Go 1.27's generic methods earn their keep. Before 1.27 a
// method could not declare its own type parameters, so a typed row scanner had
// to be a package-level function taking the handle as an argument, and you
// needed a second name for the transaction flavour:
//
//	store.All[Note](db, ctx, q)     // and
//	store.AllTx[Note](tx, ctx, q)   // ...the same thing, twice
//
// Now both *DB and *Tx carry the same generic method, and call sites read the
// way they should:
//
//	db.All[Note](ctx, q)
//	tx.All[Note](ctx, q)
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go, FTS5 included, so CGO_ENABLED=0 still gives us search
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is a handle on the tsnotes database.
type DB struct {
	sql *sql.DB
}

// Tx is a transaction. It carries the same generic query methods as DB so a
// helper written against one reads identically against the other.
type Tx struct {
	tx *sql.Tx
}

// querier is what the scan helpers actually need. Both *sql.DB and *sql.Tx
// satisfy it.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Open connects to the database at path, applying any pending migrations.
//
// The pragmas are not optional decoration:
//   - WAL lets the HTTP handlers read while the indexer writes;
//   - busy_timeout turns a lock collision into a short wait instead of an error;
//   - foreign_keys makes the ON DELETE CASCADE rules in the schema real, since
//     SQLite ignores them by default;
//   - txlock=immediate takes the write lock at BEGIN, so two transactions can
//     never deadlock trying to upgrade from read to write.
func Open(path string) (*DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?" + strings.Join([]string{
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
		"_txlock=immediate",
	}, "&")

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite tolerates concurrent readers under WAL but only one writer. Leaving
	// the pool unbounded just converts contention into busy timeouts.
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{sql: sqlDB}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the connection pool.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the underlying handle for the rare caller that needs it, such as
// health checks. Prefer the typed methods.
func (d *DB) SQL() *sql.DB { return d.sql }

// SchemaVersion returns the applied migration number.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	return d.One[int](ctx, `PRAGMA user_version`)
}

// migrate applies every embedded migration whose number is above the database's
// current user_version, each in its own transaction.
func (d *DB) migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	current, err := d.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for _, name := range names {
		n, err := migrationNumber(name)
		if err != nil {
			return err
		}
		if n <= current {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := d.applyMigration(ctx, name, n, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) applyMigration(ctx context.Context, name string, n int, body string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	// PRAGMA does not take bind parameters, and n came from a filename we
	// validated as an integer, so this is safe.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
		return fmt.Errorf("stamp %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

// migrationNumber parses the leading integer of "0001_init.sql".
func migrationNumber(name string) (int, error) {
	num, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q must be named NNNN_description.sql", name)
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, fmt.Errorf("migration %q has a non-numeric prefix: %w", name, err)
	}
	return n, nil
}

// Exec runs a statement that returns no rows.
func (d *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.sql.ExecContext(ctx, query, args...)
}

// Exec runs a statement inside the transaction.
func (t *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// All runs query and scans every row into a T.
func (d *DB) All[T any](ctx context.Context, query string, args ...any) ([]T, error) {
	return queryAll[T](ctx, d.sql, query, args...)
}

// All runs query inside the transaction and scans every row into a T.
func (t *Tx) All[T any](ctx context.Context, query string, args ...any) ([]T, error) {
	return queryAll[T](ctx, t.tx, query, args...)
}

// One runs query and scans exactly one row, returning sql.ErrNoRows if there
// were none.
func (d *DB) One[T any](ctx context.Context, query string, args ...any) (T, error) {
	return queryOne[T](ctx, d.sql, query, args...)
}

// One runs query inside the transaction and scans exactly one row.
func (t *Tx) One[T any](ctx context.Context, query string, args ...any) (T, error) {
	return queryOne[T](ctx, t.tx, query, args...)
}

// Tx runs fn inside a transaction, committing if it returns nil and rolling
// back otherwise. A panic inside fn rolls back and then continues unwinding.
func (d *DB) Tx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func queryAll[T any](ctx context.Context, q querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	plan, err := planFor[T](cols)
	if err != nil {
		return nil, err
	}

	out := []T{}
	for rows.Next() {
		var v T
		if err := plan.scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}

func queryOne[T any](ctx context.Context, q querier, query string, args ...any) (T, error) {
	var zero T
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return zero, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return zero, fmt.Errorf("columns: %w", err)
	}
	plan, err := planFor[T](cols)
	if err != nil {
		return zero, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, fmt.Errorf("iterate: %w", err)
		}
		return zero, sql.ErrNoRows
	}
	var v T
	if err := plan.scan(rows, &v); err != nil {
		return zero, err
	}
	return v, rows.Err()
}
