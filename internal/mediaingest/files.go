package mediaingest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type copiedFile struct {
	size   int64
	sha256 string
}

func (manager *Manager) copyToStaging(sourcePath, stagingPath string) (copiedFile, error) {
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, sourcePath, true); err != nil {
		return copiedFile{}, err
	}
	if err := ensureManagedFileTarget(manager.stagingRoot, stagingPath); err != nil {
		return copiedFile{}, err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("source media cannot be opened")}
	}
	defer source.Close()
	target, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("staging media cannot be opened")}
	}
	closeTarget := true
	defer func() {
		if closeTarget {
			_ = target.Close()
		}
	}()

	digest := sha256.New()
	reader := bufio.NewReaderSize(source, 64<<10)
	buffer := make([]byte, 64<<10)
	total := int64(0)
	for {
		read, readErr := reader.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > manager.config.MediaIngest.MaxSegmentBytes {
				return copiedFile{}, &Error{Code: CodeTooLarge, Err: errors.New("source media exceeds configured size")}
			}
			if _, err := target.Write(buffer[:read]); err != nil {
				return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("write staging media failed")}
			}
			_, _ = digest.Write(buffer[:read])
			if manager.afterCopyChunk != nil {
				if err := manager.afterCopyChunk(total); err != nil {
					return copiedFile{}, &Error{Code: CodeStorageFailed, Err: err}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("read source media failed")}
		}
	}
	if err := target.Sync(); err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("flush staging media failed")}
	}
	if err := target.Close(); err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("close staging media failed")}
	}
	closeTarget = false
	result := copiedFile{size: total, sha256: hex.EncodeToString(digest.Sum(nil))}
	if err := manager.verifyManagedFile(stagingPath, result.size, result.sha256); err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("persisted staging media failed verification")}
	}
	return result, nil
}

func (manager *Manager) installAccepted(stagingPath, acceptedPath string, size int64, expectedHash string) error {
	if err := ensureManagedFileTarget(manager.stagingRoot, stagingPath); err != nil {
		return err
	}
	if err := ensureManagedFileTarget(manager.acceptedRoot, acceptedPath); err != nil {
		return err
	}
	if _, err := os.Lstat(acceptedPath); err == nil {
		if err := manager.verifyManagedFile(acceptedPath, size, expectedHash); err != nil {
			return err
		}
		if err := os.Remove(stagingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &Error{Code: CodeStorageFailed, Err: errors.New("redundant staging media cannot be removed")}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return &Error{Code: CodeStorageFailed, Err: errors.New("accepted media path cannot be inspected")}
	}
	if err := os.Rename(stagingPath, acceptedPath); err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("staging media cannot be atomically renamed")}
	}
	file, err := os.OpenFile(acceptedPath, os.O_RDWR, 0)
	if err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("accepted media cannot be reopened")}
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("accepted media cannot be flushed")}
	}
	return nil
}

func (manager *Manager) verifyManagedFile(path string, size int64, expectedHash string) error {
	if err := ensureManagedPath(manager.storageRoot, path, true); err != nil {
		return err
	}
	result, err := hashFile(path, manager.config.MediaIngest.MaxSegmentBytes)
	if err != nil {
		return err
	}
	if result.size != size || result.sha256 != expectedHash {
		return &Error{Code: CodeStorageFailed, Err: errors.New("managed media does not match accepted metadata")}
	}
	return nil
}

func hashFile(path string, maximum int64) (copiedFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("media file cannot be opened for verification")}
	}
	defer file.Close()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maximum+1))
	if err != nil {
		return copiedFile{}, &Error{Code: CodeStorageFailed, Err: errors.New("media file cannot be verified")}
	}
	if written > maximum {
		return copiedFile{}, &Error{Code: CodeTooLarge, Err: errors.New("media file exceeds configured size")}
	}
	return copiedFile{size: written, sha256: hex.EncodeToString(digest.Sum(nil))}, nil
}

func ensureManagedFileTarget(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) || strings.Contains(relative, string(filepath.Separator)) {
		return &Error{Code: CodePathInvalid, Err: errors.New("managed media target is outside its fixed directory")}
	}
	if reparse, err := isReparsePoint(root); err != nil || reparse {
		return &Error{Code: CodeReparsePoint, Err: errors.New("managed media directory is a reparse point")}
	}
	if reparse, err := isReparsePoint(path); err == nil && reparse {
		return &Error{Code: CodeReparsePoint, Err: errors.New("managed media target is a reparse point")}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return &Error{Code: CodeStorageFailed, Err: errors.New("managed media target cannot be inspected")}
	}
	return nil
}

func ensureManagedPath(root, path string, requireRegular bool) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return &Error{Code: CodePathInvalid, Err: errors.New("managed media path is outside storage root")}
	}
	cursor := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		cursor = filepath.Join(cursor, component)
		reparse, err := isReparsePoint(cursor)
		if err != nil {
			return &Error{Code: CodeStorageFailed, Err: errors.New("managed media path cannot be inspected")}
		}
		if reparse {
			return &Error{Code: CodeReparsePoint, Err: errors.New("managed media path contains a reparse point")}
		}
	}
	if requireRegular {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return &Error{Code: CodeStorageFailed, Err: errors.New("managed media is not a regular file")}
		}
	}
	return nil
}

func (manager *Manager) quarantine(ctx context.Context, identity processIdentity, sourcePath string, sidecarRaw []byte, stagingPath, reasonCode string) error {
	if manager.beforeQuarantine != nil {
		if err := manager.beforeQuarantine(); err != nil {
			_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, manager.relativeManaged(stagingPath), 0)
			return &Error{Code: CodeQuarantineFailed, Err: err}
		}
	}
	base := identity.ingestKey + "-" + identity.sourceFingerprint
	mediaTarget := filepath.Join(manager.quarantineRoot, base+".media")
	if err := ensureManagedFileTarget(manager.quarantineRoot, mediaTarget); err != nil {
		_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, manager.relativeManaged(stagingPath), 0)
		return err
	}
	if _, err := os.Lstat(mediaTarget); errors.Is(err, os.ErrNotExist) {
		if stagingPath != "" {
			if err := os.Rename(stagingPath, mediaTarget); err != nil {
				_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, manager.relativeManaged(stagingPath), 0)
				return &Error{Code: CodeQuarantineFailed, Err: errors.New("staging media cannot enter quarantine")}
			}
		} else {
			temporary := filepath.Join(manager.quarantineRoot, base+".partial")
			result, err := manager.copyToQuarantine(sourcePath, temporary)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return manager.record(ctx, identity, "pending", CodeSourceMissing, "", 0)
				}
				_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, "", 0)
				return &Error{Code: CodeQuarantineFailed, Err: err}
			}
			if result.size > manager.config.MediaIngest.MaxSegmentBytes {
				_ = manager.record(ctx, identity, "failed", CodeTooLarge, "", 0)
				return &Error{Code: CodeTooLarge, Err: errors.New("media is too large to quarantine")}
			}
			if err := os.Rename(temporary, mediaTarget); err != nil {
				_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, "", 0)
				return &Error{Code: CodeQuarantineFailed, Err: errors.New("quarantine media cannot be committed")}
			}
		}
	} else if err != nil {
		_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, "", 0)
		return &Error{Code: CodeQuarantineFailed, Err: errors.New("quarantine target cannot be inspected")}
	} else if stagingPath != "" {
		if err := os.Remove(stagingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, manager.relativeManaged(stagingPath), 0)
			return &Error{Code: CodeQuarantineFailed, Err: errors.New("redundant quarantine staging file cannot be removed")}
		}
	}
	if len(sidecarRaw) > 0 {
		if err := writeAtomicFile(filepath.Join(manager.quarantineRoot, base+SidecarSuffix), sidecarRaw); err != nil {
			_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, "", 0)
			return &Error{Code: CodeQuarantineFailed, Err: err}
		}
	}
	reason, _ := json.Marshal(struct {
		SchemaVersion     int    `json:"schema_version"`
		ReasonCode        string `json:"reason_code"`
		SourceName        string `json:"source_name"`
		SourceFingerprint string `json:"source_fingerprint"`
		QuarantinedAtUTC  string `json:"quarantined_at_utc"`
	}{
		SchemaVersion:     StatusSchemaVersion,
		ReasonCode:        reasonCode,
		SourceName:        identity.sourceName,
		SourceFingerprint: identity.sourceFingerprint,
		QuarantinedAtUTC:  manager.now().UTC().Format(time.RFC3339Nano),
	})
	if err := writeAtomicFile(filepath.Join(manager.quarantineRoot, base+".reason.json"), reason); err != nil {
		_ = manager.record(ctx, identity, "failed", CodeQuarantineFailed, "", 0)
		return &Error{Code: CodeQuarantineFailed, Err: err}
	}
	return manager.record(ctx, identity, "quarantined", reasonCode, manager.relativeManaged(mediaTarget), 0)
}

func (manager *Manager) copyToQuarantine(sourcePath, targetPath string) (copiedFile, error) {
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, sourcePath, true); err != nil {
		return copiedFile{}, err
	}
	if err := ensureManagedFileTarget(manager.quarantineRoot, targetPath); err != nil {
		return copiedFile{}, err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return copiedFile{}, err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return copiedFile{}, err
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(target, digest), io.LimitReader(source, manager.config.MediaIngest.MaxSegmentBytes+1))
	if err != nil {
		_ = target.Close()
		return copiedFile{}, err
	}
	if written > manager.config.MediaIngest.MaxSegmentBytes {
		_ = target.Close()
		return copiedFile{}, &Error{Code: CodeTooLarge, Err: errors.New("media is too large to quarantine")}
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return copiedFile{}, err
	}
	if err := target.Close(); err != nil {
		return copiedFile{}, err
	}
	return copiedFile{size: written, sha256: hex.EncodeToString(digest.Sum(nil))}, nil
}

func writeAtomicFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("atomic destination is not a regular file")
		}
		if reparse, err := isReparsePoint(path); err != nil || reparse {
			return errors.New("atomic destination is a reparse point")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	committed = true
	return nil
}
