package mediaingest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func secureMkdirAll(path string) error {
	cleaned, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return &Error{Code: CodePathInvalid, Err: errors.New("media directory cannot be normalized")}
	}
	missing := make([]string, 0, 4)
	cursor := cleaned
	for {
		info, err := os.Lstat(cursor)
		if err == nil {
			if reparse, reparseErr := isReparsePoint(cursor); reparseErr != nil || reparse {
				return &Error{Code: CodeReparsePoint, Err: errors.New("media directory is a reparse point")}
			}
			if !info.IsDir() {
				return &Error{Code: CodePathInvalid, Err: errors.New("media directory path contains a non-directory component")}
			}
			redirected, err := filepath.EvalSymlinks(cursor)
			if err != nil || !samePath(cursor, redirected) {
				return &Error{Code: CodeReparsePoint, Err: errors.New("media directory path traverses a link or reparse point")}
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return &Error{Code: CodeStorageFailed, Err: fmt.Errorf("inspect media directory: %w", err)}
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return &Error{Code: CodePathInvalid, Err: errors.New("media directory has no existing ancestor")}
		}
		missing = append(missing, cursor)
		cursor = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return &Error{Code: CodeStorageFailed, Err: fmt.Errorf("create media directory: %w", err)}
		}
		if reparse, err := isReparsePoint(missing[index]); err != nil || reparse {
			return &Error{Code: CodeReparsePoint, Err: errors.New("created media directory became a reparse point")}
		}
	}
	return nil
}

func ensureSafePath(root, path string, requireRegular bool) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return &Error{Code: CodePathInvalid, Err: errors.New("media path is outside the configured root")}
	}
	if strings.Contains(relative, string(filepath.Separator)) {
		return &Error{Code: CodePathInvalid, Err: errors.New("media inbox accepts only root-level files")}
	}
	if reparse, err := isReparsePoint(root); err != nil || reparse {
		return &Error{Code: CodeReparsePoint, Err: errors.New("media root is a reparse point")}
	}
	if reparse, err := isReparsePoint(path); errors.Is(err, os.ErrNotExist) {
		return err
	} else if err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("media path cannot be inspected")}
	} else if reparse {
		return &Error{Code: CodeReparsePoint, Err: errors.New("media file is a link or reparse point")}
	}
	if requireRegular {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return &Error{Code: CodePathInvalid, Err: errors.New("media inbox object is not a regular file")}
		}
	}
	return nil
}

func samePath(first, second string) bool {
	return strings.EqualFold(filepath.Clean(first), filepath.Clean(second))
}
