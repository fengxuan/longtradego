package core

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

var (
	dotenvReadFile   = godotenv.Read
	dotenvSetenv     = os.Setenv
	dotenvLookupEnv  = os.LookupEnv
	dotenvEnvPath    = DefaultEnvFilePath
	dotenvLegacyPath = LegacyRepoEnvFilePath
	dotenvStat       = os.Stat
)

func LoadDefaultEnvFiles() error {
	primaryPath := strings.TrimSpace(dotenvEnvPath())
	if primaryPath == "" {
		return nil
	}

	primaryExists, err := loadDotenvFile(primaryPath)
	if err != nil {
		return err
	}
	if primaryExists {
		return nil
	}

	legacyPath := strings.TrimSpace(dotenvLegacyPath())
	if legacyPath == "" {
		return nil
	}
	_, err = loadDotenvFile(legacyPath)
	return err
}

func loadDotenvFile(path string) (bool, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false, nil
	}
	if _, err := dotenvStat(trimmed); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	values, err := dotenvReadFile(trimmed)
	if err != nil {
		return true, fmt.Errorf("load env file %s: %w", trimmed, err)
	}
	for key, value := range values {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if _, exists := dotenvLookupEnv(key); exists {
			continue
		}
		if err := dotenvSetenv(key, value); err != nil {
			return true, fmt.Errorf("set env %s from %s: %w", key, trimmed, err)
		}
	}
	return true, nil
}
