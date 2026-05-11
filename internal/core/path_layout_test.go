package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

func TestLooksLikeGoRunExecutable(t *testing.T) {
	if !LooksLikeGoRunExecutable("/var/folders/x/abcd/T/go-build/b001/exe/main") {
		t.Fatalf("expected go run executable to be detected")
	}
	if LooksLikeGoRunExecutable("/usr/local/bin/longtradego") {
		t.Fatalf("expected installed binary path to not match go run heuristic")
	}
}

func TestDefaultDirsUseRepoLayoutForWorkspace(t *testing.T) {
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
			return fakeFileInfo{}, nil
		default:
			return nil, errors.New("missing")
		}
	}

	if got := DefaultConfigDir(); got != "conf" {
		t.Fatalf("unexpected config dir: %q", got)
	}
	if got := DefaultDataDir(); got != "data" {
		t.Fatalf("unexpected data dir: %q", got)
	}
	if got := DefaultLogDir(); got != "logs" {
		t.Fatalf("unexpected log dir: %q", got)
	}
}

func TestDefaultDirsUseAppHomeOutsideWorkspace(t *testing.T) {
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
	pathStat = func(path string) (os.FileInfo, error) { return nil, errors.New("missing") }

	if got := DefaultConfigDir(); got != filepath.Join("/Users/demo", ".config", "longtradego", "conf") {
		t.Fatalf("unexpected config dir: %q", got)
	}
	if got := DefaultDataDir(); got != filepath.Join("/Users/demo", ".config", "longtradego", "data") {
		t.Fatalf("unexpected data dir: %q", got)
	}
	if got := DefaultLogDir(); got != filepath.Join("/Users/demo", ".config", "longtradego", "logs") {
		t.Fatalf("unexpected log dir: %q", got)
	}
}

func TestDefaultDirsUseRepoLayoutForGoRun(t *testing.T) {
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

	pathExecutable = func() (string, error) { return "/var/folders/x/abcd/T/go-build/b001/exe/main", nil }
	pathGetwd = func() (string, error) { return "/tmp", nil }
	pathUserHome = func() (string, error) { return "/Users/demo", nil }
	pathStat = func(path string) (os.FileInfo, error) { return nil, errors.New("missing") }

	if got := DefaultConfigDir(); got != "conf" {
		t.Fatalf("unexpected config dir: %q", got)
	}
	if got := DefaultDataDir(); got != "data" {
		t.Fatalf("unexpected data dir: %q", got)
	}
	if got := DefaultLogDir(); got != "logs" {
		t.Fatalf("unexpected log dir: %q", got)
	}
}
