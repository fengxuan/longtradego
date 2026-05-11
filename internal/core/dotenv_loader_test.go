package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeEnvFileInfo struct{}

func (fakeEnvFileInfo) Name() string       { return "" }
func (fakeEnvFileInfo) Size() int64        { return 0 }
func (fakeEnvFileInfo) Mode() os.FileMode  { return 0 }
func (fakeEnvFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeEnvFileInfo) IsDir() bool        { return false }
func (fakeEnvFileInfo) Sys() any           { return nil }

func TestLoadDefaultEnvFilesLoadsPrimaryEnvFile(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "longtradego.env")
	if err := os.WriteFile(primary, []byte("SMTP_HOST=primary.example.com\n"), 0o644); err != nil {
		t.Fatalf("write primary env failed: %v", err)
	}

	oldEnvPath := dotenvEnvPath
	oldLegacyPath := dotenvLegacyPath
	oldRead := dotenvReadFile
	oldLookup := dotenvLookupEnv
	oldSetenv := dotenvSetenv
	oldStat := dotenvStat
	defer func() {
		dotenvEnvPath = oldEnvPath
		dotenvLegacyPath = oldLegacyPath
		dotenvReadFile = oldRead
		dotenvLookupEnv = oldLookup
		dotenvSetenv = oldSetenv
		dotenvStat = oldStat
	}()

	loaded := map[string]string{}
	dotenvEnvPath = func() string { return primary }
	dotenvLegacyPath = func() string { return "" }
	dotenvReadFile = func(filenames ...string) (map[string]string, error) {
		return map[string]string{"SMTP_HOST": "primary.example.com"}, nil
	}
	dotenvLookupEnv = func(key string) (string, bool) { return "", false }
	dotenvSetenv = func(key string, value string) error {
		loaded[key] = value
		return nil
	}
	dotenvStat = os.Stat

	if err := LoadDefaultEnvFiles(); err != nil {
		t.Fatalf("LoadDefaultEnvFiles failed: %v", err)
	}
	if loaded["SMTP_HOST"] != "primary.example.com" {
		t.Fatalf("expected primary env value loaded, got %#v", loaded)
	}
}

func TestLoadDefaultEnvFilesFallsBackToLegacyRepoEnv(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".env")
	if err := os.WriteFile(legacy, []byte("SMTP_HOST=legacy.example.com\n"), 0o644); err != nil {
		t.Fatalf("write legacy env failed: %v", err)
	}

	oldEnvPath := dotenvEnvPath
	oldLegacyPath := dotenvLegacyPath
	oldRead := dotenvReadFile
	oldLookup := dotenvLookupEnv
	oldSetenv := dotenvSetenv
	oldStat := dotenvStat
	defer func() {
		dotenvEnvPath = oldEnvPath
		dotenvLegacyPath = oldLegacyPath
		dotenvReadFile = oldRead
		dotenvLookupEnv = oldLookup
		dotenvSetenv = oldSetenv
		dotenvStat = oldStat
	}()

	loaded := map[string]string{}
	dotenvEnvPath = func() string { return filepath.Join(dir, "conf", "longtradego.env") }
	dotenvLegacyPath = func() string { return legacy }
	dotenvReadFile = func(filenames ...string) (map[string]string, error) {
		if len(filenames) == 1 && filenames[0] == legacy {
			return map[string]string{"SMTP_HOST": "legacy.example.com"}, nil
		}
		return map[string]string{}, nil
	}
	dotenvLookupEnv = func(key string) (string, bool) { return "", false }
	dotenvSetenv = func(key string, value string) error {
		loaded[key] = value
		return nil
	}
	dotenvStat = os.Stat

	if err := LoadDefaultEnvFiles(); err != nil {
		t.Fatalf("LoadDefaultEnvFiles failed: %v", err)
	}
	if loaded["SMTP_HOST"] != "legacy.example.com" {
		t.Fatalf("expected legacy env fallback loaded, got %#v", loaded)
	}
}

func TestLoadDefaultEnvFilesDoesNotOverrideExistingEnv(t *testing.T) {
	oldEnvPath := dotenvEnvPath
	oldLegacyPath := dotenvLegacyPath
	oldRead := dotenvReadFile
	oldLookup := dotenvLookupEnv
	oldSetenv := dotenvSetenv
	oldStat := dotenvStat
	defer func() {
		dotenvEnvPath = oldEnvPath
		dotenvLegacyPath = oldLegacyPath
		dotenvReadFile = oldRead
		dotenvLookupEnv = oldLookup
		dotenvSetenv = oldSetenv
		dotenvStat = oldStat
	}()

	dotenvEnvPath = func() string { return filepath.Join(t.TempDir(), "longtradego.env") }
	dotenvLegacyPath = func() string { return "" }
	dotenvReadFile = func(filenames ...string) (map[string]string, error) {
		return map[string]string{"SMTP_HOST": "file.example.com"}, nil
	}
	dotenvLookupEnv = func(key string) (string, bool) {
		if key == "SMTP_HOST" {
			return "shell.example.com", true
		}
		return "", false
	}
	called := false
	dotenvSetenv = func(key string, value string) error {
		called = true
		return nil
	}
	dotenvStat = func(path string) (os.FileInfo, error) {
		return fakeEnvFileInfo{}, nil
	}

	if err := LoadDefaultEnvFiles(); err != nil {
		t.Fatalf("LoadDefaultEnvFiles failed: %v", err)
	}
	if called {
		t.Fatalf("expected existing env to prevent override")
	}
}

func TestLoadDefaultEnvFilesReturnsParseError(t *testing.T) {
	oldEnvPath := dotenvEnvPath
	oldLegacyPath := dotenvLegacyPath
	oldRead := dotenvReadFile
	oldLookup := dotenvLookupEnv
	oldSetenv := dotenvSetenv
	oldStat := dotenvStat
	defer func() {
		dotenvEnvPath = oldEnvPath
		dotenvLegacyPath = oldLegacyPath
		dotenvReadFile = oldRead
		dotenvLookupEnv = oldLookup
		dotenvSetenv = oldSetenv
		dotenvStat = oldStat
	}()

	dotenvEnvPath = func() string { return filepath.Join(t.TempDir(), "longtradego.env") }
	dotenvLegacyPath = func() string { return "" }
	dotenvReadFile = func(filenames ...string) (map[string]string, error) {
		return nil, os.ErrInvalid
	}
	dotenvLookupEnv = os.LookupEnv
	dotenvSetenv = os.Setenv
	dotenvStat = func(path string) (os.FileInfo, error) {
		return fakeEnvFileInfo{}, nil
	}

	if err := LoadDefaultEnvFiles(); err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestDefaultEnvFilePathUsesConfigDir(t *testing.T) {
	oldExecutable := pathExecutable
	oldGetwd := pathGetwd
	oldUserHome := pathUserHome
	oldStat := pathStat
	defer func() {
		pathExecutable = oldExecutable
		pathGetwd = oldGetwd
		pathUserHome = oldUserHome
		pathStat = oldStat
	}()

	pathExecutable = func() (string, error) { return "/usr/local/bin/longtradego", nil }
	pathGetwd = func() (string, error) { return "/tmp", nil }
	pathUserHome = func() (string, error) { return "/Users/demo", nil }
	pathStat = func(path string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	if got := DefaultEnvFilePath(); got != filepath.Join("/Users/demo", ".config", "longtradego", "conf", "longtradego.env") {
		t.Fatalf("unexpected env path: %q", got)
	}
}

func TestLegacyRepoEnvFilePathOnlyInRepoMode(t *testing.T) {
	oldExecutable := pathExecutable
	oldGetwd := pathGetwd
	oldUserHome := pathUserHome
	oldStat := pathStat
	defer func() {
		pathExecutable = oldExecutable
		pathGetwd = oldGetwd
		pathUserHome = oldUserHome
		pathStat = oldStat
	}()

	pathExecutable = func() (string, error) { return "/usr/local/bin/longtradego", nil }
	pathGetwd = func() (string, error) { return "/repo/subdir", nil }
	pathUserHome = func() (string, error) { return "/Users/demo", nil }
	pathStat = func(path string) (os.FileInfo, error) {
		switch filepath.Clean(path) {
		case filepath.Clean("/repo/go.mod"), filepath.Clean("/repo/internal/service/version_command.go"):
			return fakeEnvFileInfo{}, nil
		default:
			return nil, os.ErrNotExist
		}
	}

	if got := LegacyRepoEnvFilePath(); got != ".env" {
		t.Fatalf("expected repo legacy env path, got %q", got)
	}

	pathGetwd = func() (string, error) { return "/tmp", nil }
	pathStat = func(path string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	if got := LegacyRepoEnvFilePath(); got != "" {
		t.Fatalf("expected no legacy env path outside repo mode, got %q", got)
	}
}
