package eventstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	repositorymigrations "github.com/Versifine/study-monitor/migrations"
)

const (
	CurrentSchemaVersion      = 1
	currentMediaSchemaVersion = 2
	currentM3SchemaVersion    = 1
)

type migration struct {
	version  int
	name     string
	contents string
	checksum string
}

func repositoryMigrations() ([]migration, error) {
	return loadMigrations([]string{"001_raw_events.sql"})
}

func repositoryMediaMigrations() ([]migration, error) {
	return loadMigrations([]string{"002_media_ingest.sql", "003_media_file_checks.sql"})
}

func repositoryM3Migrations() ([]migration, error) {
	return loadMigrations([]string{"004_collectors_timeline.sql"})
}

func loadMigrations(files []string) ([]migration, error) {
	migrations := make([]migration, 0, len(files))
	for index, name := range files {
		raw, err := repositorymigrations.Files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		migrations = append(migrations, migration{
			version:  index + 1,
			name:     name,
			contents: string(raw),
			checksum: hex.EncodeToString(digest[:]),
		})
	}
	return migrations, nil
}

func migrate(ctx context.Context, db *sql.DB, now func() time.Time) error {
	migrations, err := repositoryMigrations()
	if err != nil {
		return wrap(CodeMigrationFailed, "load embedded migrations", err)
	}
	if err := applyMigrations(ctx, db, migrations, now); err != nil {
		return err
	}
	mediaMigrations, err := repositoryMediaMigrations()
	if err != nil {
		return wrap(CodeMigrationFailed, "load embedded media migrations", err)
	}
	if err := applyMediaMigrations(ctx, db, mediaMigrations, now); err != nil {
		return err
	}
	m3Migrations, err := repositoryM3Migrations()
	if err != nil {
		return wrap(CodeMigrationFailed, "load embedded M3 migrations", err)
	}
	return applyM3Migrations(ctx, db, m3Migrations, now)
}

func applyMigrations(ctx context.Context, db *sql.DB, migrations []migration, now func() time.Time) error {
	return applyMigrationsThrough(ctx, db, migrations, now, CurrentSchemaVersion)
}

func applyMigrationsThrough(ctx context.Context, db *sql.DB, migrations []migration, now func() time.Time, targetVersion int) error {
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeMigrationFailed, "begin migration transaction", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL CHECK (length(checksum) = 64),
    applied_at_utc TEXT NOT NULL
) STRICT;
CREATE TRIGGER IF NOT EXISTS schema_migrations_reject_update
BEFORE UPDATE ON schema_migrations
BEGIN
    SELECT RAISE(ABORT, 'SCHEMA_MIGRATIONS_APPEND_ONLY');
END;
CREATE TRIGGER IF NOT EXISTS schema_migrations_reject_delete
BEFORE DELETE ON schema_migrations
BEGIN
    SELECT RAISE(ABORT, 'SCHEMA_MIGRATIONS_APPEND_ONLY');
END;`); err != nil {
		return classifySQLiteError(CodeMigrationFailed, "create migration ledger", err)
	}

	var userVersion int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return classifySQLiteError(CodeMigrationFailed, "read schema version", err)
	}
	if userVersion > targetVersion {
		return &Error{Code: CodeMigrationUnsupported, Err: fmt.Errorf("database schema version %d is newer than supported version %d", userVersion, targetVersion)}
	}

	recorded := make(map[int]string)
	rows, err := tx.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return classifySQLiteError(CodeMigrationFailed, "read migration ledger", err)
	}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return wrap(CodeMigrationFailed, "scan migration ledger", err)
		}
		recorded[version] = checksum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return classifySQLiteError(CodeMigrationFailed, "iterate migration ledger", err)
	}
	if err := rows.Close(); err != nil {
		return wrap(CodeMigrationFailed, "close migration ledger", err)
	}

	known := make(map[int]migration, len(migrations))
	for _, item := range migrations {
		known[item.version] = item
	}
	for version, checksum := range recorded {
		item, ok := known[version]
		if !ok || version > targetVersion {
			return &Error{Code: CodeMigrationUnsupported, Err: fmt.Errorf("database contains unknown migration version %d", version)}
		}
		if checksum != item.checksum {
			return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("migration checksum mismatch for version %d", version)}
		}
	}
	if len(recorded) != userVersion {
		return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("migration ledger count %d does not match schema version %d", len(recorded), userVersion)}
	}

	for _, item := range migrations {
		if item.version > targetVersion {
			break
		}
		if item.version <= userVersion {
			continue
		}
		if item.version != userVersion+1 {
			return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("migration sequence jumps from %d to %d", userVersion, item.version)}
		}
		if _, err := tx.ExecContext(ctx, item.contents); err != nil {
			return classifySQLiteError(CodeMigrationFailed, "apply "+item.name, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO schema_migrations(version, name, checksum, applied_at_utc) VALUES(?, ?, ?, ?)",
			item.version,
			item.name,
			item.checksum,
			now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return classifySQLiteError(CodeMigrationFailed, "record "+item.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", item.version)); err != nil {
			return classifySQLiteError(CodeMigrationFailed, "advance schema version", err)
		}
		userVersion = item.version
	}

	if userVersion != targetVersion {
		return &Error{Code: CodeMigrationFailed, Err: errors.New("not all required migrations were applied")}
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeMigrationFailed, "commit migrations", err)
	}
	return nil
}

func applyMediaMigrations(ctx context.Context, db *sql.DB, migrations []migration, now func() time.Time) error {
	return applyIndependentMigrations(ctx, db, migrations, now, "media_schema_migrations", "MEDIA_SCHEMA_MIGRATIONS", "media", currentMediaSchemaVersion)
}

func applyM3Migrations(ctx context.Context, db *sql.DB, migrations []migration, now func() time.Time) error {
	return applyIndependentMigrations(ctx, db, migrations, now, "m3_schema_migrations", "M3_SCHEMA_MIGRATIONS", "M3", currentM3SchemaVersion)
}

func applyIndependentMigrations(ctx context.Context, db *sql.DB, migrations []migration, now func() time.Time, ledger, triggerPrefix, label string, targetVersion int) error {
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeMigrationFailed, "begin "+label+" migration transaction", err)
	}
	defer tx.Rollback()

	ledgerSQL := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL CHECK (length(checksum) = 64),
    applied_at_utc TEXT NOT NULL
) STRICT;
CREATE TRIGGER IF NOT EXISTS %s_reject_update
BEFORE UPDATE ON %s
BEGIN
    SELECT RAISE(ABORT, '%s_APPEND_ONLY');
END;
CREATE TRIGGER IF NOT EXISTS %s_reject_delete
BEFORE DELETE ON %s
BEGIN
    SELECT RAISE(ABORT, '%s_APPEND_ONLY');
END;`, ledger, ledger, ledger, triggerPrefix, ledger, ledger, triggerPrefix)
	if _, err := tx.ExecContext(ctx, ledgerSQL); err != nil {
		return classifySQLiteError(CodeMigrationFailed, "create "+label+" migration ledger", err)
	}

	recorded := make(map[int]string)
	rows, err := tx.QueryContext(ctx, "SELECT version, checksum FROM "+ledger+" ORDER BY version")
	if err != nil {
		return classifySQLiteError(CodeMigrationFailed, "read "+label+" migration ledger", err)
	}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return wrap(CodeMigrationFailed, "scan "+label+" migration ledger", err)
		}
		recorded[version] = checksum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return classifySQLiteError(CodeMigrationFailed, "iterate "+label+" migration ledger", err)
	}
	if err := rows.Close(); err != nil {
		return wrap(CodeMigrationFailed, "close "+label+" migration ledger", err)
	}

	known := make(map[int]migration, len(migrations))
	for _, item := range migrations {
		known[item.version] = item
	}
	for version, checksum := range recorded {
		item, ok := known[version]
		if !ok || version > targetVersion {
			return &Error{Code: CodeMigrationUnsupported, Err: fmt.Errorf("database contains unknown %s migration version %d", label, version)}
		}
		if checksum != item.checksum {
			return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("%s migration checksum mismatch for version %d", label, version)}
		}
	}

	appliedVersion := 0
	for version := 1; version <= targetVersion; version++ {
		if _, ok := recorded[version]; !ok {
			break
		}
		appliedVersion = version
	}
	if len(recorded) != appliedVersion {
		return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("%s migration ledger is not contiguous", label)}
	}

	for _, item := range migrations {
		if item.version > targetVersion {
			break
		}
		if item.version <= appliedVersion {
			continue
		}
		if item.version != appliedVersion+1 {
			return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("%s migration sequence jumps from %d to %d", label, appliedVersion, item.version)}
		}
		if _, err := tx.ExecContext(ctx, item.contents); err != nil {
			return classifySQLiteError(CodeMigrationFailed, "apply "+item.name, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO "+ledger+"(version, name, checksum, applied_at_utc) VALUES(?, ?, ?, ?)",
			item.version,
			item.name,
			item.checksum,
			now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return classifySQLiteError(CodeMigrationFailed, "record "+item.name, err)
		}
		appliedVersion = item.version
	}

	if appliedVersion != targetVersion {
		return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("not all required %s migrations were applied", label)}
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeMigrationFailed, "commit "+label+" migrations", err)
	}
	return nil
}
