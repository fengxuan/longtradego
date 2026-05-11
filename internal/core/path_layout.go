package core

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	repoConfigDirName = "conf"
	repoDataDirName   = "data"
	repoLogDirName    = "logs"
	appHomeEnvVar     = "LONGTRADEGO_HOME"
	dotenvFileName    = "longtradego.env"
)

var (
	pathExecutable = os.Executable
	pathGetwd      = os.Getwd
	pathUserHome   = os.UserHomeDir
	pathStat       = os.Stat
)

func LooksLikeGoRunExecutable(executablePath string) bool {
	trimmed := strings.TrimSpace(executablePath)
	if trimmed == "" {
		return false
	}
	normalized := filepath.ToSlash(trimmed)
	return strings.Contains(normalized, "/go-build/") && strings.Contains(normalized, "/exe/")
}

func UseRepoRelativeLayout() bool {
	executablePath, err := pathExecutable()
	if err == nil && LooksLikeGoRunExecutable(executablePath) {
		return true
	}
	wd, err := pathGetwd()
	if err == nil && looksLikeProjectWorkspace(wd) {
		return true
	}
	return false
}

func DefaultAppHomeDir() string {
	if override := strings.TrimSpace(os.Getenv(appHomeEnvVar)); override != "" {
		return override
	}
	home, err := pathUserHome()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(repoConfigDirName, "longtradego")
	}
	return filepath.Join(home, ".config", "longtradego")
}

func DefaultConfigDir() string {
	if UseRepoRelativeLayout() {
		return repoConfigDirName
	}
	return filepath.Join(DefaultAppHomeDir(), repoConfigDirName)
}

func DefaultDataDir() string {
	if UseRepoRelativeLayout() {
		return repoDataDirName
	}
	return filepath.Join(DefaultAppHomeDir(), repoDataDirName)
}

func DefaultLogDir() string {
	if UseRepoRelativeLayout() {
		return repoLogDirName
	}
	return filepath.Join(DefaultAppHomeDir(), repoLogDirName)
}

func ResolveConfigPath(name string) string {
	return filepath.Join(DefaultConfigDir(), strings.TrimSpace(name))
}

func ResolveDataPath(name string) string {
	return filepath.Join(DefaultDataDir(), strings.TrimSpace(name))
}

func ResolveLogPath(name string) string {
	return filepath.Join(DefaultLogDir(), strings.TrimSpace(name))
}

func DefaultEnvFilePath() string {
	return filepath.Join(DefaultConfigDir(), dotenvFileName)
}

func LegacyRepoEnvFilePath() string {
	if !UseRepoRelativeLayout() {
		return ""
	}
	return ".env"
}

func looksLikeProjectWorkspace(start string) bool {
	dir := strings.TrimSpace(start)
	if dir == "" {
		return false
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) && fileExists(filepath.Join(dir, "internal", "service", "version_command.go")) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := pathStat(path)
	return err == nil
}
