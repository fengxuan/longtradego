package service

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunLongbridgeCommandPassThroughAndJSONOutput(t *testing.T) {
	oldLookPath := longbridgeLookPath
	oldCommandContext := longbridgeCommandContext
	oldStdout := longbridgeCommandStdout
	oldStderr := longbridgeCommandStderr
	t.Cleanup(func() {
		longbridgeLookPath = oldLookPath
		longbridgeCommandContext = oldCommandContext
		longbridgeCommandStdout = oldStdout
		longbridgeCommandStderr = oldStderr
	})

	var gotProgram string
	var gotArgs []string
	longbridgeLookPath = func(file string) (string, error) {
		if file != longbridgeCLIProgram {
			t.Fatalf("unexpected lookup file: %q", file)
		}
		return "/fake/longbridge", nil
	}
	longbridgeCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotProgram = name
		gotArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, "/bin/sh", "-lc", `printf '%s\n' '{"ok":true,"n":1}'`)
	}

	var visibleStdout bytes.Buffer
	var visibleStderr bytes.Buffer
	longbridgeCommandStdout = &visibleStdout
	longbridgeCommandStderr = &visibleStderr

	result, err := runLongbridgeCommand(context.Background(), []string{"quote", "AAPL.US", "--format", "json"})
	if err != nil {
		t.Fatalf("runLongbridgeCommand failed: %v", err)
	}
	if gotProgram != "/fake/longbridge" {
		t.Fatalf("expected resolved binary path to be used, got %q", gotProgram)
	}
	if !reflect.DeepEqual(gotArgs, []string{"quote", "AAPL.US", "--format", "json"}) {
		t.Fatalf("unexpected passthrough args: %v", gotArgs)
	}
	if !result.Success || result.ExitCode != 0 {
		t.Fatalf("expected success result, got %+v", result)
	}
	if !strings.Contains(visibleStdout.String(), `{"ok":true,"n":1}`) {
		t.Fatalf("expected command stdout to stay visible, got %q", visibleStdout.String())
	}
	if strings.TrimSpace(visibleStderr.String()) != "" {
		t.Fatalf("expected empty visible stderr, got %q", visibleStderr.String())
	}
	if result.StdoutJSONValid == nil || !*result.StdoutJSONValid {
		t.Fatalf("expected stdout_json_valid=true, got %#v", result.StdoutJSONValid)
	}
	stdoutJSON, ok := result.StdoutJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected stdout json object, got %#v", result.StdoutJSON)
	}
	if stdoutJSON["ok"] != true || stdoutJSON["n"].(float64) != 1 {
		t.Fatalf("unexpected stdout json payload: %#v", stdoutJSON)
	}
}

func TestNewLongbridgeCommandDefaultsToHelpWhenNoArgs(t *testing.T) {
	oldLookPath := longbridgeLookPath
	oldCommandContext := longbridgeCommandContext
	oldStdout := longbridgeCommandStdout
	oldStderr := longbridgeCommandStderr
	t.Cleanup(func() {
		longbridgeLookPath = oldLookPath
		longbridgeCommandContext = oldCommandContext
		longbridgeCommandStdout = oldStdout
		longbridgeCommandStderr = oldStderr
	})

	var gotArgs []string
	longbridgeLookPath = func(file string) (string, error) { return "/fake/longbridge", nil }
	longbridgeCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, "/bin/sh", "-lc", `printf "help"`)
	}

	longbridgeCommandStdout = &bytes.Buffer{}
	longbridgeCommandStderr = &bytes.Buffer{}

	app := NewAppContext()
	cmd := newLongbridgeCommand(app)
	cmd.SetArgs([]string{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute longbridge command failed: %v", err)
	}
	if !reflect.DeepEqual(gotArgs, []string{"--help"}) {
		t.Fatalf("expected default --help passthrough args, got %v", gotArgs)
	}

	command, symbols, resultAny := app.ExecutionSnapshot()
	if command != "longbridge" {
		t.Fatalf("expected execution command longbridge, got %q", command)
	}
	if !reflect.DeepEqual(symbols, []string{"--help"}) {
		t.Fatalf("expected execution symbols [--help], got %v", symbols)
	}
	result, ok := resultAny.(longbridgeCommandResult)
	if !ok {
		t.Fatalf("expected longbridgeCommandResult, got %T", resultAny)
	}
	if !result.Success {
		t.Fatalf("expected help execution success, got %+v", result)
	}
}

func TestRunLongbridgeCommandReturnsClearErrorWhenBinaryMissing(t *testing.T) {
	oldLookPath := longbridgeLookPath
	oldCommandContext := longbridgeCommandContext
	t.Cleanup(func() {
		longbridgeLookPath = oldLookPath
		longbridgeCommandContext = oldCommandContext
	})

	longbridgeLookPath = func(file string) (string, error) {
		return "", exec.ErrNotFound
	}
	longbridgeCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("command context should not be called when binary lookup fails")
		return nil
	}

	result, err := runLongbridgeCommand(context.Background(), []string{"quote", "AAPL.US"})
	if err == nil {
		t.Fatalf("expected missing binary error")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("expected PATH lookup error, got %v", err)
	}
	if result.ExitCode != -1 {
		t.Fatalf("expected unresolved command exit_code=-1, got %+v", result)
	}
}

func TestRunLongbridgeCommandPropagatesExitCodeAndStderr(t *testing.T) {
	oldLookPath := longbridgeLookPath
	oldCommandContext := longbridgeCommandContext
	oldStdout := longbridgeCommandStdout
	oldStderr := longbridgeCommandStderr
	t.Cleanup(func() {
		longbridgeLookPath = oldLookPath
		longbridgeCommandContext = oldCommandContext
		longbridgeCommandStdout = oldStdout
		longbridgeCommandStderr = oldStderr
	})

	longbridgeLookPath = func(file string) (string, error) { return "/fake/longbridge", nil }
	longbridgeCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-lc", `echo 'boom' >&2; exit 7`)
	}

	longbridgeCommandStdout = &bytes.Buffer{}
	longbridgeCommandStderr = &bytes.Buffer{}

	result, err := runLongbridgeCommand(context.Background(), []string{"order", "sell", "AAPL.US"})
	if err == nil {
		t.Fatalf("expected non-zero exit error")
	}
	if result.Success {
		t.Fatalf("expected success=false on non-zero exit, got %+v", result)
	}
	if result.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %+v", result)
	}
	if !strings.Contains(err.Error(), "exit code 7") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error to include exit code and stderr, got %v", err)
	}
}

func TestRunLongbridgeCommandTimeout(t *testing.T) {
	oldLookPath := longbridgeLookPath
	oldCommandContext := longbridgeCommandContext
	oldStdout := longbridgeCommandStdout
	oldStderr := longbridgeCommandStderr
	t.Cleanup(func() {
		longbridgeLookPath = oldLookPath
		longbridgeCommandContext = oldCommandContext
		longbridgeCommandStdout = oldStdout
		longbridgeCommandStderr = oldStderr
	})

	longbridgeLookPath = func(file string) (string, error) { return "/fake/longbridge", nil }
	longbridgeCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-lc", "sleep 1")
	}
	longbridgeCommandStdout = &bytes.Buffer{}
	longbridgeCommandStderr = &bytes.Buffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := runLongbridgeCommand(ctx, []string{"quote", "AAPL.US"})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !result.TimedOut {
		t.Fatalf("expected timed_out=true, got %+v", result)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", ctx.Err())
	}
}
