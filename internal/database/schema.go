// Package database owns SQLite connection setup and the schema shared at the fork.
package database

//go:generate go run ./cmd/generate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-jet/jet/v2/sqlite"
	sqliteDriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"sergeant/internal/database/jet/table"
	"sergeant/internal/database/schema"
)

// OpenBalances applies the historical balance migrations. The ledger engine
// supplies its additional journal migration when opening its own file.
func OpenBalances(path string, extraMigrations ...string) (*sql.DB, error) {
	db, err := openDatabase(path)
	if err != nil {
		return nil, err
	}
	steps := slices.Concat(schema.BalanceMigrations, extraMigrations)
	if err := migrate(db, steps); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func openDatabase(path string) (*sql.DB, error) {
	dsn := path
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: absolute}).String()
	}
	base, rawQuery, _ := strings.Cut(dsn, "?")
	options, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, err
	}
	options.Add("_pragma", "busy_timeout(5000)")
	options.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", base+"?"+options.Encode())
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if base == ":memory:" || base == "file::memory:" || options.Get("mode") == "memory" {
		db.SetMaxOpenConns(1)
	}
	if err := enableWAL(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// WAL persists in the file. Two processes switching a fresh database into WAL
// can get SQLITE_BUSY immediately even with busy_timeout set.
func enableWAL(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, err := sqlite.RawStatement("PRAGMA journal_mode=WAL").ExecContext(ctx, db)
		sqliteErr, ok := errors.AsType[*sqliteDriver.Error](err)
		if !ok || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("enable WAL: %w", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func migrate(db *sql.DB, migrations []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	ctx := context.Background()
	if err := ScanRow(ctx, tx, sqlite.RawStatement("PRAGMA user_version"), &version); err != nil {
		return err
	}
	if version < 0 || version > schema.JournalVersion {
		return fmt.Errorf("unsupported database schema version %d (latest %d)", version, schema.JournalVersion)
	}
	if err := dropComparisonColumns(ctx, tx); err != nil {
		return err
	}
	if version >= len(migrations) {
		return tx.Commit()
	}
	if _, err := sqlite.RawStatement(schema.Bootstrap).Exec(tx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	if err := backfillLegacyTimestamps(ctx, tx); err != nil {
		return fmt.Errorf("backfill legacy timestamps: %w", err)
	}

	if version == 0 {
		version, err = inferSchemaVersion(ctx, tx)
		if err != nil {
			return err
		}
	}
	for version < len(migrations) {
		if _, err := sqlite.RawStatement(migrations[version]).Exec(tx); err != nil {
			return fmt.Errorf("migration v%d: %w", version+1, err)
		}
		version++
	}
	if _, err := sqlite.RawStatement(fmt.Sprintf("PRAGMA user_version = %d", version)).Exec(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// A restored SQLite dump can contain migrated tables but omit user_version.
func inferSchemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	stmt := sqlite.RawStatement(`SELECT
		EXISTS (SELECT 1 FROM pragma_table_info('payment_methods') WHERE name='is_default'),
		EXISTS (SELECT 1 FROM sqlite_schema WHERE type='table' AND name='ledger_entries')`)
	var hasDefault, hasLedger bool
	if err := ScanRow(ctx, tx, stmt, &hasDefault, &hasLedger); err != nil {
		return 0, err
	}
	switch {
	case hasLedger:
		return schema.JournalVersion, nil
	case hasDefault:
		return 1, nil
	default:
		return 0, nil
	}
}

func dropComparisonColumns(ctx context.Context, tx *sql.Tx) error {
	for _, old := range []struct{ table, column string }{
		{"shadow_queue", "expected"},
		{"shadow_trial", "mismatches"},
	} {
		exists, err := hasColumn(ctx, tx, old.table, old.column)
		if err != nil {
			return err
		}
		if exists {
			drop := "ALTER TABLE " + old.table + " DROP COLUMN " + old.column
			if _, err := sqlite.RawStatement(drop).ExecContext(ctx, tx); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasColumn(ctx context.Context, tx *sql.Tx, tableName, column string) (bool, error) {
	check := sqlite.RawStatement(`SELECT EXISTS (
		SELECT 1 FROM pragma_table_info(:table) WHERE name=:column
	)`, sqlite.RawArgs{":table": tableName, ":column": column})
	var exists bool
	err := ScanRow(ctx, tx, check, &exists)
	return exists, err
}

func backfillLegacyTimestamps(ctx context.Context, tx *sql.Tx) error {
	// Early schemas only had updated_at. Preserve the known timestamp for
	// those rows before the journal starts recording new entries.
	d, p := table.Debts, table.PaymentMethods
	for _, target := range []struct {
		table             sqlite.Table
		inserted, updated sqlite.ColumnInteger
	}{
		{d, d.InsertedAt, d.UpdatedAt},
		{p, p.InsertedAt, p.UpdatedAt},
	} {
		hasTimestamp, err := hasColumn(ctx, tx, target.table.TableName(), "inserted_at")
		if err != nil {
			return err
		}
		if hasTimestamp {
			continue
		}
		addColumn := "ALTER TABLE " + target.table.TableName() +
			" ADD COLUMN inserted_at INTEGER NOT NULL DEFAULT 0"
		if _, err := sqlite.RawStatement(addColumn).Exec(tx); err != nil {
			return err
		}
		backfill := target.table.UPDATE(target.inserted).SET(target.updated).WHERE(sqlite.Bool(true))
		if _, err := backfill.Exec(tx); err != nil {
			return err
		}
	}
	return nil
}

// FileURI quotes a filesystem path for SQLite, including literal '?' and '#'.
func FileURI(path, mode string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=" + mode}).String()
}

func CheckGhost(ctx context.Context, path, id string) error {
	db, err := sql.Open("sqlite", FileURI(path, "ro"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var match bool
	t := table.ShadowTrial
	expected := t.ID.EQ(sqlite.String(id)).
		AND(t.Role.EQ(sqlite.String("ledger"))).
		AND(t.Seeded.EQ(sqlite.Int(1)))
	stmt := t.SELECT(expected).WHERE(t.Singleton.EQ(sqlite.Int(1)))
	err = ScanRow(ctx, db, stmt, &match)
	if err != nil {
		return err
	}
	if !match {
		return errors.New("database is not the ledger for this legacy trial")
	}
	return nil
}
