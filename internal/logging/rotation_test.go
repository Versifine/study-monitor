package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileBoundsSizeAndCount(t *testing.T) {
	directory := t.TempDir()
	writer, err := NewRotatingFile(directory, 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"12345\n", "67890\n", "abcde\n", "fghij\n"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("rotated files=%d want=3", len(entries))
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 8 || !strings.HasSuffix(string(raw), "\n") {
			t.Fatalf("invalid rotated file %s: %q", entry.Name(), raw)
		}
	}
}
