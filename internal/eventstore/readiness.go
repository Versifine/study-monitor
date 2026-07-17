package eventstore

import (
	"context"
)

const (
	ReadinessWritable        = "writable"
	ReadinessBusy            = "busy"
	ReadinessReadOnly        = "read_only"
	ReadinessUnavailable     = "unavailable"
	ReadinessMigrationFailed = "migration_failed"
)

type Readiness struct {
	Status        string
	SchemaVersion int
	ErrorCode     string
}

func (store *Store) Readiness(ctx context.Context) Readiness {
	connection, err := store.db.Conn(ctx)
	if err != nil {
		return readinessFromError(classifySQLiteError(CodeQueryFailed, "acquire readiness connection", err))
	}
	defer connection.Close()

	var queryOnly int
	if err := connection.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return readinessFromError(classifySQLiteError(CodeQueryFailed, "read query-only mode", err))
	}
	if queryOnly != 0 {
		return Readiness{Status: ReadinessReadOnly, SchemaVersion: CurrentSchemaVersion, ErrorCode: CodeReadOnly}
	}
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return readinessFromError(classifySQLiteError(CodeWriteFailed, "probe writable database", err))
	}
	rollbackRequired := true
	defer func() {
		if rollbackRequired {
			_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var schemaVersion int
	if err := connection.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return readinessFromError(classifySQLiteError(CodeQueryFailed, "read readiness schema version", err))
	}
	if _, err := connection.ExecContext(ctx, "ROLLBACK"); err != nil {
		return readinessFromError(classifySQLiteError(CodeWriteFailed, "finish writable database probe", err))
	}
	rollbackRequired = false
	if schemaVersion != CurrentSchemaVersion {
		return Readiness{Status: ReadinessMigrationFailed, SchemaVersion: schemaVersion, ErrorCode: CodeMigrationFailed}
	}
	return Readiness{Status: ReadinessWritable, SchemaVersion: schemaVersion}
}

func readinessFromError(err error) Readiness {
	switch ErrorCode(err) {
	case CodeBusy:
		return Readiness{Status: ReadinessBusy, SchemaVersion: CurrentSchemaVersion, ErrorCode: CodeBusy}
	case CodeReadOnly:
		return Readiness{Status: ReadinessReadOnly, SchemaVersion: CurrentSchemaVersion, ErrorCode: CodeReadOnly}
	default:
		return Readiness{Status: ReadinessUnavailable, SchemaVersion: CurrentSchemaVersion, ErrorCode: ErrorCode(err)}
	}
}
