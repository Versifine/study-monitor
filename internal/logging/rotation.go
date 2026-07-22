package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// RotatingFile bounds structured log storage by both file size and retained count.
type RotatingFile struct {
	mu      sync.Mutex
	path    string
	maximum int64
	count   int
	file    *os.File
	size    int64
}

func NewRotatingFile(directory string, maximum int64, count int) (*RotatingFile, error) {
	if !filepath.IsAbs(directory) || maximum < 1 || count < 2 {
		return nil, errors.New("invalid rotating log settings")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "exam-monitor.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &RotatingFile{path: path, maximum: maximum, count: count, file: file, size: info.Size()}, nil
}

func (writer *RotatingFile) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.size > 0 && writer.size+int64(len(value)) > writer.maximum {
		if err := writer.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := writer.file.Write(value)
	writer.size += int64(written)
	return written, err
}

func (writer *RotatingFile) rotate() error {
	if err := writer.file.Sync(); err != nil {
		return err
	}
	if err := writer.file.Close(); err != nil {
		return err
	}
	for index := writer.count - 1; index >= 1; index-- {
		target := writer.path + "." + strconv.Itoa(index)
		_ = os.Remove(target)
		source := writer.path
		if index > 1 {
			source = writer.path + "." + strconv.Itoa(index-1)
		}
		if err := os.Rename(source, target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	file, err := os.OpenFile(writer.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer.file, writer.size = file, 0
	return nil
}

func (writer *RotatingFile) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.file == nil {
		return nil
	}
	err := writer.file.Sync()
	if closeErr := writer.file.Close(); err == nil {
		err = closeErr
	}
	writer.file = nil
	return err
}
