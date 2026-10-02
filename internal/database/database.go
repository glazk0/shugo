// Package database opens the SQLite database and keeps its schema current by
// applying the SQL migrations embedded in the binary.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"strconv"
	"strings"

	// Registers the pure-Go "sqlite" driver, so the binary stays CGO-free.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedded embed.FS

// pragmas run on every connection the pool opens. WAL lets message handlers
// read while /settings writes, busy_timeout makes a writer wait for the lock
// instead of failing, foreign_keys enforces the REFERENCES clauses, which
// SQLite ignores by default, and temp_store keeps temporary tables off the
// container's read-only filesystem.
var pragmas = []string{
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"temp_store(MEMORY)",
}

// Open opens the SQLite database at path, creating the file if needed, and
// applies every pending migration.
//
// Parameters:
//   - ctx (context.Context): bounds the connection check and the migrations.
//   - path (string): database file path.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	// Every transaction here writes, so take the write lock at BEGIN: a
	// deferred transaction that upgrades later can fail with SQLITE_BUSY
	// without waiting for busy_timeout.
	query := url.Values{"_pragma": pragmas, "_txlock": {"immediate"}}
	db, err := sql.Open("sqlite", path+"?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("database: open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database: open %s: %w", path, err)
	}

	migrations, err := fs.Sub(embedded, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := migrate(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migration is one numbered schema change.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies the migrations in fsys that the database has not seen yet.
// The schema version lives in PRAGMA user_version, and each migration runs
// in its own transaction together with the version bump, so a failed
// migration leaves the database at the previous version.
//
// Parameters:
//   - ctx (context.Context): bounds the migrations.
//   - db (*sql.DB): database to migrate.
//   - fsys (fs.FS): directory holding NNNN_description.sql files.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	migrations, err := load(fsys)
	if err != nil {
		return err
	}

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("database: read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database: schema version %d is newer than this build supports (%d)", current, len(migrations))
	}

	for _, m := range migrations[current:] {
		if err := apply(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// load reads the migrations in fsys, ordered by version. Versions must run
// from 1 without gaps, so a misnamed file fails loudly instead of being
// skipped.
//
// Parameters:
//   - fsys (fs.FS): directory holding NNNN_description.sql files.
func load(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("database: list migrations: %w", err)
	}

	// Glob sorts names lexically, which is version order for zero-padded
	// prefixes; the gap check below catches any other naming.
	migrations := make([]migration, 0, len(names))
	for i, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return nil, fmt.Errorf("database: migration %q must be named NNNN_description.sql", name)
		}
		if version != i+1 {
			return nil, fmt.Errorf("database: migration %q has version %d, want %d", name, version, i+1)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("database: read migration %q: %w", name, err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(body)})
	}
	return migrations, nil
}

// apply runs m and records its version in one transaction.
//
// Parameters:
//   - ctx (context.Context): bounds the transaction.
//   - db (*sql.DB): database to migrate.
//   - m (migration): migration to apply.
func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: apply migration %s: %w", m.name, err)
	}
	// Rollback is a no-op once Commit has succeeded.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("database: apply migration %s: %w", m.name, err)
	}
	// PRAGMA statements do not accept bound parameters.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
		return fmt.Errorf("database: record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit migration %s: %w", m.name, err)
	}
	return nil
}
