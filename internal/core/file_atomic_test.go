package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicWriteAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := writeFileAtomic(path, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic first write failed: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first write failed: %v", err)
	}
	if string(raw) != "first\n" {
		t.Fatalf("unexpected first write content: %q", string(raw))
	}

	if err := writeFileAtomic(path, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic overwrite failed: %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read overwrite failed: %v", err)
	}
	if string(raw) != "second\n" {
		t.Fatalf("unexpected overwrite content: %q", string(raw))
	}
}

func TestWriteFileAtomicEmptyPath(t *testing.T) {
	if err := writeFileAtomic("", []byte("x"), 0o644); err == nil {
		t.Fatalf("expected empty path error")
	}
}

func TestWriteFileAtomicCleanupTempOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "destdir")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir dest dir failed: %v", err)
	}

	err := writeFileAtomic(destDir, []byte("content"), 0o644)
	if err == nil {
		t.Fatalf("expected rename failure when destination path is an existing directory")
	}

	pattern := filepath.Join(dir, ".destdir.tmp-*")
	leftovers, globErr := filepath.Glob(pattern)
	if globErr != nil {
		t.Fatalf("glob tmp files failed: %v", globErr)
	}
	if len(leftovers) > 0 {
		t.Fatalf("expected no temp leftovers, got %v", leftovers)
	}
	errText := strings.ToLower(err.Error())
	if !strings.Contains(errText, "directory") && !strings.Contains(errText, "exists") {
		t.Fatalf("expected directory/existing-target rename error, got %v", err)
	}
}
