package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

const sqliteDriverName = "sqlite"

type Options struct {
	BusyTimeout        time.Duration
	MaxOpenConnections int
	MaxBatchEvents     int
	MaxEventBytes      int
	MaxPayloadDepth    int
	MaxPageSize        int
	CollectorPolicies  map[string]CollectorPolicy
	Now                func() time.Time
}

type CollectorPolicy struct {
	Kind            string
	HeartbeatPeriod time.Duration
}

type Store struct {
	db                         *sql.DB
	maxBatchEvents             int
	maxEventBytes              int
	maxPayloadDepth            int
	maxPageSize                int
	maxTimelineProjectionBytes int
	timelineSync               chan struct{}
	collectorPolicies          map[string]CollectorPolicy
	now                        func() time.Time
}

func Open(ctx context.Context, databasePath string, options Options) (*Store, error) {
	if !filepath.IsAbs(databasePath) {
		return nil, &Error{Code: CodePathInvalid, Err: errors.New("database path must be absolute")}
	}
	if options.BusyTimeout < 100*time.Millisecond || options.BusyTimeout > 30*time.Second ||
		options.MaxOpenConnections < 1 || options.MaxBatchEvents < 1 || options.MaxEventBytes < 1 || options.MaxPayloadDepth < 1 || options.MaxPageSize < 1 {
		return nil, &Error{Code: CodeOpenFailed, Err: errors.New("event store options are invalid")}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return nil, wrap(CodeOpenFailed, "create database directory", err)
	}

	dsn := sqliteDSN(databasePath, options.BusyTimeout)
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, wrap(CodeOpenFailed, "open database", err)
	}
	db.SetMaxOpenConns(options.MaxOpenConnections)
	db.SetMaxIdleConns(options.MaxOpenConnections)
	db.SetConnMaxIdleTime(5 * time.Minute)
	closeOnError := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}

	if err := db.PingContext(ctx); err != nil {
		return closeOnError(classifySQLiteError(CodeOpenFailed, "ping database", err))
	}
	if err := verifyPragmas(ctx, db, options.BusyTimeout); err != nil {
		return closeOnError(err)
	}
	if err := migrate(ctx, db, options.Now); err != nil {
		return closeOnError(err)
	}

	policies := make(map[string]CollectorPolicy, len(options.CollectorPolicies))
	for id, policy := range options.CollectorPolicies {
		policies[id] = policy
	}
	return &Store{
		db:                         db,
		maxBatchEvents:             options.MaxBatchEvents,
		maxEventBytes:              options.MaxEventBytes,
		maxPayloadDepth:            options.MaxPayloadDepth,
		maxPageSize:                options.MaxPageSize,
		maxTimelineProjectionBytes: 32 << 20,
		timelineSync:               make(chan struct{}, 1),
		collectorPolicies:          policies,
		now:                        options.Now,
	}, nil
}

func (store *Store) Close() error { return store.db.Close() }

func sqliteDSN(databasePath string, busyTimeout time.Duration) string {
	uriPath := filepath.ToSlash(databasePath)
	if filepath.VolumeName(databasePath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	databaseURL := &url.URL{Scheme: "file", Path: uriPath}
	query := databaseURL.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	query.Add("_pragma", "wal_autocheckpoint(1000)")
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String()
}

func verifyPragmas(ctx context.Context, db *sql.DB, busyTimeout time.Duration) error {
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return classifySQLiteError(CodePragmaFailed, "read journal mode", err)
	}
	if journalMode != "wal" {
		return &Error{Code: CodePragmaFailed, Err: fmt.Errorf("journal mode is %q, want wal", journalMode)}
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return classifySQLiteError(CodePragmaFailed, "read foreign key mode", err)
	}
	if foreignKeys != 1 {
		return &Error{Code: CodePragmaFailed, Err: errors.New("foreign keys are disabled")}
	}
	var configuredBusyTimeout int64
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&configuredBusyTimeout); err != nil {
		return classifySQLiteError(CodePragmaFailed, "read busy timeout", err)
	}
	if configuredBusyTimeout != busyTimeout.Milliseconds() {
		return &Error{Code: CodePragmaFailed, Err: fmt.Errorf("busy timeout is %dms, want %dms", configuredBusyTimeout, busyTimeout.Milliseconds())}
	}
	return nil
}

func classifySQLiteError(defaultCode, operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return wrap(CodeCanceled, operation, err)
	}
	var sqliteError *sqlite.Error
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() & 0xff {
		case 5, 6:
			return wrap(CodeBusy, operation, err)
		case 8:
			return wrap(CodeReadOnly, operation, err)
		}
	}
	return wrap(defaultCode, operation, err)
}
