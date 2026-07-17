package eventstore

import (
	"errors"
	"fmt"
)

const (
	CodePathInvalid          = "STORE_PATH_INVALID"
	CodeOpenFailed           = "STORE_OPEN_FAILED"
	CodePragmaFailed         = "STORE_PRAGMA_FAILED"
	CodeMigrationFailed      = "STORE_MIGRATION_FAILED"
	CodeMigrationUnsupported = "STORE_MIGRATION_UNSUPPORTED"
	CodeBusy                 = "STORE_BUSY"
	CodeReadOnly             = "STORE_READ_ONLY"
	CodeWriteFailed          = "STORE_WRITE_FAILED"
	CodeQueryFailed          = "STORE_QUERY_FAILED"
	CodeCanceled             = "STORE_CANCELED"
)

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func ErrorCode(err error) string {
	var storeError *Error
	if errors.As(err, &storeError) {
		return storeError.Code
	}
	return CodeOpenFailed
}

func wrap(code, operation string, err error) error {
	return &Error{Code: code, Err: fmt.Errorf("%s: %w", operation, err)}
}
