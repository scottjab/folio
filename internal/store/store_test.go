package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scottjab/tsnotes/internal/store"
)

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAppliesMigrations(t *testing.T) {
	db := newDB(t)
	v, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Errorf("SchemaVersion = %d, want at least 1", v)
	}

	// Every table the rest of tsnotes depends on must exist after Open.
	names, err := db.All[string](context.Background(),
		`SELECT name FROM sqlite_master WHERE type IN ('table','view') ORDER BY name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	for _, want := range []string{"users", "vaults", "shares", "notes", "notes_fts", "tags", "links"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing table %q; have %v", want, names)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	db1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	v1, _ := db1.SchemaVersion(context.Background())
	if _, err := db1.Exec(context.Background(), `INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
		1, "a@github", "A", "a-github", time.Now().Unix()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db1.Close()

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	v2, _ := db2.SchemaVersion(context.Background())
	if v1 != v2 {
		t.Errorf("schema version moved on reopen: %d -> %d", v1, v2)
	}
	n, err := db2.One[int](context.Background(), `SELECT count(*) FROM users`)
	if err != nil || n != 1 {
		t.Errorf("row count = %d, %v; want the row to survive reopen", n, err)
	}
}

func TestPragmas(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	mode, err := db.One[string](ctx, `PRAGMA journal_mode`)
	if err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	fk, err := db.One[int](ctx, `PRAGMA foreign_keys`)
	if err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Error("foreign_keys is off; the schema relies on cascade deletes")
	}
}

// rowUser mirrors a users row for the struct-scanning tests.
type rowUser struct {
	ID              int64  `db:"id"`
	TailscaleUserID int64  `db:"tailscale_user_id"`
	Login           string `db:"login"`
	DisplayName     string `db:"display_name"`
	VaultDir        string `db:"vault_dir"`
	CreatedAt       int64  `db:"created_at"`
}

func seedUsers(t *testing.T, db *store.DB, logins ...string) {
	t.Helper()
	for i, l := range logins {
		_, err := db.Exec(context.Background(),
			`INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
			int64(i+1), l, strings.ToUpper(l), l, int64(1000+i))
		if err != nil {
			t.Fatalf("seed %q: %v", l, err)
		}
	}
}

func TestAllScansStructs(t *testing.T) {
	db := newDB(t)
	seedUsers(t, db, "alice", "bob")

	users, err := db.All[rowUser](context.Background(), `SELECT * FROM users ORDER BY login`)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
	if users[0].Login != "alice" || users[0].DisplayName != "ALICE" || users[0].TailscaleUserID != 1 {
		t.Errorf("users[0] = %+v", users[0])
	}
	if users[1].Login != "bob" {
		t.Errorf("users[1] = %+v", users[1])
	}
}

func TestAllScansScalars(t *testing.T) {
	db := newDB(t)
	seedUsers(t, db, "alice", "bob", "carol")

	logins, err := db.All[string](context.Background(), `SELECT login FROM users ORDER BY login`)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if !slices.Equal(logins, []string{"alice", "bob", "carol"}) {
		t.Errorf("logins = %v", logins)
	}
}

func TestAllOnEmptyResultReturnsEmptyNotNilError(t *testing.T) {
	db := newDB(t)
	got, err := db.All[rowUser](context.Background(), `SELECT * FROM users`)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0", len(got))
	}
}

func TestOne(t *testing.T) {
	db := newDB(t)
	seedUsers(t, db, "alice")

	u, err := db.One[rowUser](context.Background(), `SELECT * FROM users WHERE login = ?`, "alice")
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if u.Login != "alice" {
		t.Errorf("Login = %q", u.Login)
	}

	n, err := db.One[int64](context.Background(), `SELECT count(*) FROM users`)
	if err != nil || n != 1 {
		t.Errorf("count = %d, %v", n, err)
	}
}

func TestOneMissingReturnsErrNoRows(t *testing.T) {
	db := newDB(t)
	_, err := db.One[rowUser](context.Background(), `SELECT * FROM users WHERE login = ?`, "nobody")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("One on empty = %v, want sql.ErrNoRows", err)
	}
}

func TestScanHandlesNulls(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE nullable(a TEXT, b INTEGER, c TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO nullable VALUES (NULL, NULL, 'set')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	type row struct {
		A string         `db:"a"`
		B int64          `db:"b"`
		C sql.NullString `db:"c"`
	}
	got, err := db.One[row](ctx, `SELECT * FROM nullable`)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.A != "" || got.B != 0 {
		t.Errorf("NULLs should scan to zero values, got %+v", got)
	}
	if !got.C.Valid || got.C.String != "set" {
		t.Errorf("C = %+v", got.C)
	}
}

func TestScanIgnoresUnmappedColumns(t *testing.T) {
	// Selecting more columns than the struct declares is normal (SELECT *
	// against a table that grew a column). It must not be an error.
	db := newDB(t)
	seedUsers(t, db, "alice")

	type partial struct {
		Login string `db:"login"`
	}
	got, err := db.One[partial](context.Background(), `SELECT * FROM users`)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Login != "alice" {
		t.Errorf("Login = %q", got.Login)
	}
}

func TestScanErrorsOnUnmappableStructField(t *testing.T) {
	// The reverse is a bug: asking for a column the query never returned means
	// the caller and the SQL have drifted apart. Say so loudly.
	db := newDB(t)
	seedUsers(t, db, "alice")

	type wrong struct {
		Nope string `db:"does_not_exist"`
	}
	_, err := db.One[wrong](context.Background(), `SELECT login FROM users`)
	if err == nil {
		t.Fatal("expected an error for a struct field with no matching column")
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Errorf("error should name the offending field: %v", err)
	}
}

func TestScanMatchesSnakeCaseWithoutTags(t *testing.T) {
	db := newDB(t)
	seedUsers(t, db, "alice")

	type untagged struct {
		Login           string
		TailscaleUserID int64
	}
	got, err := db.One[untagged](context.Background(), `SELECT login, tailscale_user_id FROM users`)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Login != "alice" || got.TailscaleUserID != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestTxCommits(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
			1, "alice", "A", "alice", 0)
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 1 {
		t.Errorf("count = %d, want the committed row", n)
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
			1, "alice", "A", "alice", 0); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v, want the callback error", err)
	}
	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 0 {
		t.Errorf("count = %d, want the insert rolled back", n)
	}
}

func TestTxRollsBackOnPanic(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic should propagate out of Tx")
			}
		}()
		db.Tx(ctx, func(tx *store.Tx) error {
			tx.Exec(ctx, `INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
				1, "alice", "A", "alice", 0)
			panic("boom")
		})
	}()

	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 0 {
		t.Errorf("count = %d, want the insert rolled back after the panic", n)
	}
}

func TestTxQueriesSeeUncommittedWrites(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	err := db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
			1, "alice", "A", "alice", 0); err != nil {
			return err
		}
		n, err := tx.One[int](ctx, `SELECT count(*) FROM users`)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("inside the tx, count = %d, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
}

func TestUniqueConstraintsHold(t *testing.T) {
	db := newDB(t)
	seedUsers(t, db, "alice")
	_, err := db.Exec(context.Background(),
		`INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
		1, "different", "D", "different", 0)
	if err == nil {
		t.Error("duplicate tailscale_user_id was accepted")
	}
}

func TestForeignKeysCascade(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedUsers(t, db, "alice")

	uid, _ := db.One[int64](ctx, `SELECT id FROM users WHERE login = 'alice'`)
	if _, err := db.Exec(ctx, `INSERT INTO vaults(user_id, dir, created_at) VALUES (?,?,?)`, uid, "alice", 0); err != nil {
		t.Fatalf("insert vault: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = ?`, uid); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	n, _ := db.One[int](ctx, `SELECT count(*) FROM vaults`)
	if n != 0 {
		t.Errorf("vaults left behind after the user was deleted: %d", n)
	}
}

func TestFTS5IsAvailable(t *testing.T) {
	// The whole search feature rides on FTS5 being compiled into the pure-Go
	// driver. If this ever regresses, fail here rather than in the indexer.
	db := newDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `INSERT INTO notes_fts(rowid, title, body, tags, path) VALUES (1, 'Hello', 'world of go', 'go', 'a.md')`); err != nil {
		t.Fatalf("insert into fts: %v", err)
	}
	hits, err := db.All[int64](ctx, `SELECT rowid FROM notes_fts WHERE notes_fts MATCH ?`, "world")
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	if !slices.Equal(hits, []int64{1}) {
		t.Errorf("hits = %v, want [1]", hits)
	}

	snip, err := db.One[string](ctx,
		`SELECT snippet(notes_fts, 1, '<b>', '</b>', '…', 8) FROM notes_fts WHERE notes_fts MATCH ?`, "world")
	if err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if !strings.Contains(snip, "<b>world</b>") {
		t.Errorf("snippet = %q, want the match highlighted", snip)
	}

	if _, err := db.One[float64](ctx,
		`SELECT bm25(notes_fts, 10.0, 1.0, 5.0, 2.0) FROM notes_fts WHERE notes_fts MATCH ?`, "world"); err != nil {
		t.Fatalf("bm25 with column weights: %v", err)
	}
}

func TestConcurrentWritersDoNotFailWithBusy(t *testing.T) {
	// WAL plus a busy timeout is what keeps the API, the indexer, and the file
	// watcher from tripping over each other.
	db := newDB(t)
	ctx := context.Background()

	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, err := db.Exec(ctx,
				`INSERT INTO users(tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,?)`,
				int64(i), "u", "U", "u", 0)
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil && !strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("concurrent insert failed with something other than the expected UNIQUE violation: %v", err)
		}
	}
}

func TestScanEmbeddedStruct(t *testing.T) {
	// Composing a row type by embedding another is how the index builds a
	// search hit on top of a note row. The embedded type's own name is
	// unexported, which must not stop the scanner from reaching its fields.
	db := newDB(t)
	seedUsers(t, db, "alice")

	type base struct {
		Login string `db:"login"`
	}
	type derived struct {
		base
		DisplayName string `db:"display_name"`
	}
	got, err := db.One[derived](context.Background(), `SELECT login, display_name FROM users`)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Login != "alice" {
		t.Errorf("promoted field Login = %q, want alice", got.Login)
	}
	if got.DisplayName != "ALICE" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
}
