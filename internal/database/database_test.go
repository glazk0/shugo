package database

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// schemaVersion returns the database's PRAGMA user_version.
//
// Parameters:
//   - t (*testing.T): fails the test on query errors.
//   - db (*sql.DB): database to inspect.
func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()

	var v int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// openRaw opens a fresh database file without migrating it.
//
// Parameters:
//   - t (*testing.T): registers cleanup and fails the test on errors.
func openRaw(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAppliesEmbeddedMigrations(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "shugo.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	dir, err := fs.Sub(embedded, "migrations")
	if err != nil {
		t.Fatalf("fs.Sub() error = %v", err)
	}
	migrations, err := load(dir)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if got := schemaVersion(t, db); got != len(migrations) || got == 0 {
		t.Errorf("schema version = %d, want %d", got, len(migrations))
	}

	var mode string
	if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	_, err = db.ExecContext(t.Context(), "INSERT INTO guild_settings (guild_id) VALUES ('missing')")
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("insert without a guild error = %v, want a foreign key violation", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopening an up-to-date database must not reapply anything.
	db, err = Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer db.Close()
	if got := schemaVersion(t, db); got != len(migrations) {
		t.Errorf("schema version after reopen = %d, want %d", got, len(migrations))
	}
}

func TestMigrateAppliesOnlyPendingMigrations(t *testing.T) {
	t.Parallel()

	db := openRaw(t)
	v1 := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
	}
	if err := migrate(t.Context(), db, v1); err != nil {
		t.Fatalf("migrate(v1) error = %v", err)
	}

	v2 := fstest.MapFS{
		"0001_a.sql": v1["0001_a.sql"],
		"0002_b.sql": {Data: []byte("CREATE TABLE b (id INTEGER PRIMARY KEY);\nINSERT INTO a (id) VALUES (1);")},
	}
	if err := migrate(t.Context(), db, v2); err != nil {
		t.Fatalf("migrate(v2) error = %v", err)
	}
	if got := schemaVersion(t, db); got != 2 {
		t.Errorf("schema version = %d, want 2", got)
	}

	var rows int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM a").Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows in a = %d, want 1 from the second migration", rows)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	t.Parallel()

	db := openRaw(t)
	fsys := fstest.MapFS{
		"0001_ok.sql":     {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
		"0002_broken.sql": {Data: []byte("CREATE TABLE b (id INTEGER PRIMARY KEY);\nNOT VALID SQL;")},
	}

	err := migrate(t.Context(), db, fsys)
	if err == nil || !strings.Contains(err.Error(), "0002_broken.sql") {
		t.Fatalf("migrate() error = %v, want it to name the broken migration", err)
	}
	if got := schemaVersion(t, db); got != 1 {
		t.Errorf("schema version = %d, want 1", got)
	}
	var name string
	err = db.QueryRowContext(t.Context(), "SELECT name FROM sqlite_schema WHERE name = 'b'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("table b exists after a failed migration (err = %v)", err)
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	t.Parallel()

	db := openRaw(t)
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 5"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	fsys := fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 1;")}}

	if err := migrate(t.Context(), db, fsys); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("migrate() error = %v, want a newer-schema error", err)
	}
}

func TestLoadRejectsBadNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
	}{
		{"no version prefix", fstest.MapFS{"init.sql": {}}},
		{"non-numeric prefix", fstest.MapFS{"one_init.sql": {}}},
		{"gap", fstest.MapFS{"0001_a.sql": {}, "0003_c.sql": {}}},
		{"does not start at one", fstest.MapFS{"0002_b.sql": {}}},
		{"duplicate version", fstest.MapFS{"0001_a.sql": {}, "0001_b.sql": {}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := load(tt.fsys); err == nil {
				t.Error("load() error = nil, want an error")
			}
		})
	}
}

func TestOpenFailsOnCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if db, err := Open(ctx, filepath.Join(t.TempDir(), "shugo.db")); err == nil {
		_ = db.Close()
		t.Error("Open() error = nil, want the cancellation")
	}
}
