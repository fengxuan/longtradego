package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendLogLineWithRotation_NoRotateWhenBelowLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system_command.log")

	if err := os.WriteFile(path, []byte("1234"), 0o644); err != nil {
		t.Fatalf("write initial log failed: %v", err)
	}
	line := []byte("56\n")
	if err := appendLogLineWithRotation(path, line, 10, "system_command"); err != nil {
		t.Fatalf("appendLogLineWithRotation failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "system_command.log" {
		t.Fatalf("expected no rotated file, got entries: %v", entries)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active log failed: %v", err)
	}
	if string(raw) != "123456\n" {
		t.Fatalf("unexpected active log content: %q", string(raw))
	}
}

func TestAppendLogLineWithRotation_RotateWhenExceedLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mail_monitor.log")

	if err := os.WriteFile(path, []byte("123456789"), 0o644); err != nil {
		t.Fatalf("write initial log failed: %v", err)
	}
	line := []byte("X\n")
	if err := appendLogLineWithRotation(path, line, 10, "mail_monitor"); err != nil {
		t.Fatalf("appendLogLineWithRotation failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected active+rotated files, got entries: %v", entries)
	}

	var rotated string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "mail_monitor_") && strings.HasSuffix(name, ".log") {
			rotated = name
			break
		}
	}
	if rotated == "" {
		t.Fatalf("expected rotated mail_monitor file, got entries: %v", entries)
	}

	rotatedRaw, err := os.ReadFile(filepath.Join(dir, rotated))
	if err != nil {
		t.Fatalf("read rotated log failed: %v", err)
	}
	if string(rotatedRaw) != "123456789" {
		t.Fatalf("unexpected rotated log content: %q", string(rotatedRaw))
	}

	activeRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active log failed: %v", err)
	}
	if string(activeRaw) != "X\n" {
		t.Fatalf("unexpected active log content: %q", string(activeRaw))
	}
}
