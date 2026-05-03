package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("path is empty")
	}
	dir := filepath.Dir(trimmedPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(trimmedPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, trimmedPath); err != nil {
		cleanup()
		return err
	}
	return nil
}

func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm)
}
