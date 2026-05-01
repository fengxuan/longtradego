package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewAdminCommandHasStatusSubcommand(t *testing.T) {
	app := newAppContext()
	cmd := newAdminCommand(app)
	found := false
	for _, sub := range cmd.Commands() {
		if sub.Name() == "status" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected admin command includes status subcommand")
	}
}

func TestRunAdminStatusSetsExecutionResult(t *testing.T) {
	app := newAppContext()
	runtimePath := filepath.Join(t.TempDir(), "admin_runtime.json")

	if err := runAdminStatus(app, runtimePath); err != nil {
		t.Fatalf("runAdminStatus stopped failed: %v", err)
	}
	command, symbols, result := app.ExecutionSnapshot()
	if command != "admin" {
		t.Fatalf("expected admin command, got %q", command)
	}
	if len(symbols) != 1 || symbols[0] != "status" {
		t.Fatalf("expected admin status symbols, got %v", symbols)
	}
	statusResult, ok := result.(adminStatusResult)
	if !ok {
		t.Fatalf("expected adminStatusResult, got %T", result)
	}
	if statusResult.Status != "stopped" {
		t.Fatalf("expected stopped status, got %+v", statusResult)
	}

	now := time.Now().Format(time.RFC3339Nano)
	if err := writeDaemonAdminRuntimeState(runtimePath, daemonAdminRuntimeInfo{
		PID:       os.Getpid(),
		Address:   ":18080",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write runtime failed: %v", err)
	}
	if err := runAdminStatus(app, runtimePath); err != nil {
		t.Fatalf("runAdminStatus running failed: %v", err)
	}
	_, _, result = app.ExecutionSnapshot()
	statusResult, ok = result.(adminStatusResult)
	if !ok {
		t.Fatalf("expected adminStatusResult after running, got %T", result)
	}
	if statusResult.Status != "running" || statusResult.Runtime == nil || statusResult.Runtime.PID != os.Getpid() {
		t.Fatalf("expected running result with current pid, got %+v", statusResult)
	}
}
