package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var logRotationMu sync.Mutex

func appendLogLineWithRotation(path string, line []byte, maxSize int64, rotatedBase string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("log path is empty")
	}
	if len(line) == 0 {
		return nil
	}
	if maxSize <= 0 {
		maxSize = commandLogMaxSize
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	logRotationMu.Lock()
	defer logRotationMu.Unlock()

	if err := rotateLogFileOnAppend(path, int64(len(line)), maxSize, rotatedBase); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(line)
	return err
}

func rotateLogFileOnAppend(path string, appendSize int64, maxSize int64, rotatedBase string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	if info.Size()+appendSize <= maxSize {
		return nil
	}

	return rotateLogFileWithBase(path, rotatedBase)
}

func rotateLogFileWithBase(path string, rotatedBase string) error {
	base := strings.TrimSpace(rotatedBase)
	if base == "" {
		base = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}

	stamp := time.Now().Format("20060102_150405")
	rotatedName := fmt.Sprintf("%s_%s.log", base, stamp)
	rotatedPath := filepath.Join(filepath.Dir(path), rotatedName)
	if _, err := os.Stat(rotatedPath); err == nil {
		rotatedPath = filepath.Join(filepath.Dir(path), fmt.Sprintf("%s_%s_%d.log", base, stamp, time.Now().UnixNano()))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return os.Rename(path, rotatedPath)
}
