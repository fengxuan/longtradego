package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunSystemCommandExecSuccess(t *testing.T) {
	result, err := runSystemCommand(context.Background(), []string{"echo", "hello"}, "")
	if err != nil {
		t.Fatalf("runSystemCommand exec failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success result")
	}
	if result.Mode != "exec" {
		t.Fatalf("expected exec mode, got %q", result.Mode)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("unexpected stdout: %q", result.Stdout)
	}
	if result.StdoutJSON != nil || result.StdoutJSONValid != nil {
		t.Fatalf("expected plain text stdout without structured json fields, got json=%#v valid=%#v", result.StdoutJSON, result.StdoutJSONValid)
	}
}

func TestRunSystemCommandShellSuccess(t *testing.T) {
	result, err := runSystemCommand(context.Background(), nil, `printf "ok"`)
	if err != nil {
		t.Fatalf("runSystemCommand shell failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success result")
	}
	if result.Mode != "shell" {
		t.Fatalf("expected shell mode, got %q", result.Mode)
	}
	if result.Stdout != "ok" {
		t.Fatalf("unexpected stdout: %q", result.Stdout)
	}
}

func TestRunSystemCommandParsesStructuredStdoutJSON(t *testing.T) {
	result, err := runSystemCommand(context.Background(), []string{"/bin/sh", "-c", `printf '%s\n' '{"ok":true,"n":1}'`}, "")
	if err != nil {
		t.Fatalf("runSystemCommand json stdout failed: %v", err)
	}
	if result.StdoutJSONValid == nil || !*result.StdoutJSONValid {
		t.Fatalf("expected stdout_json_valid=true, got %#v", result.StdoutJSONValid)
	}
	stdoutJSON, ok := result.StdoutJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected stdout_json object, got %#v", result.StdoutJSON)
	}
	if stdoutJSON["ok"] != true {
		t.Fatalf("expected stdout_json.ok=true, got %#v", stdoutJSON["ok"])
	}
	if stdoutJSON["n"].(float64) != 1 {
		t.Fatalf("expected stdout_json.n=1, got %#v", stdoutJSON["n"])
	}
}

func TestRunSystemCommandParsesStructuredStderrJSON(t *testing.T) {
	result, err := runSystemCommand(context.Background(), []string{"/bin/sh", "-c", `printf '%s\n' '{"err":"bad"}' >&2; exit 3`}, "")
	if err == nil {
		t.Fatalf("expected command failure with exit 3")
	}
	if result.StderrJSONValid == nil || !*result.StderrJSONValid {
		t.Fatalf("expected stderr_json_valid=true, got %#v stderr=%q", result.StderrJSONValid, result.Stderr)
	}
	stderrJSON, ok := result.StderrJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected stderr_json object, got %#v", result.StderrJSON)
	}
	if stderrJSON["err"] != "bad" {
		t.Fatalf("expected stderr_json.err=bad, got %#v", stderrJSON["err"])
	}
}

func TestRunSystemCommandMixedModeRejected(t *testing.T) {
	if _, err := runSystemCommand(context.Background(), []string{"echo", "x"}, "echo y"); err == nil {
		t.Fatalf("expected mixed mode to fail")
	}
}

func TestRunSystemCommandMissingCommandRejected(t *testing.T) {
	if _, err := runSystemCommand(context.Background(), nil, ""); err == nil {
		t.Fatalf("expected missing command to fail")
	}
}

func TestRunSystemCommandFailureExitCode(t *testing.T) {
	result, err := runSystemCommand(context.Background(), nil, "exit 7")
	if err == nil {
		t.Fatalf("expected failure for exit 7")
	}
	if result.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", result.ExitCode)
	}
}

func TestRunSystemCommandTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := runSystemCommand(ctx, nil, "sleep 1")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !result.TimedOut {
		t.Fatalf("expected timed_out=true, got false")
	}
}

func TestRunSystemCommandPipelineEnvAndInput(t *testing.T) {
	ctx := withDaemonPipelineInput(context.Background(), daemonPipelineInput{
		SourceCommand: "mail monitor --once",
		ResultJSON:    `{"matched_count":1}`,
		InputJSON:     `{"source_command":"mail monitor --once","result":{"matched_count":1}}`,
	})

	result, err := runSystemCommand(ctx, nil, `printf "%s\n%s\n" "$LONGTRADE_PIPELINE_SOURCE_COMMAND" "$LONGTRADE_PIPELINE_RESULT_JSON"; cat`)
	if err != nil {
		t.Fatalf("runSystemCommand shell with pipeline context failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "mail monitor --once") {
		t.Fatalf("expected source command env in stdout, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, `{"matched_count":1}`) {
		t.Fatalf("expected result json env in stdout, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, `"source_command":"mail monitor --once"`) {
		t.Fatalf("expected input json in stdin/stdout, got %q", result.Stdout)
	}
}

func TestInjectPipelineEnvUpsert(t *testing.T) {
	env := []string{
		pipelineEnvSourceCommand + "=old",
		"PATH=/bin",
	}
	injected := injectPipelineEnv(env, daemonPipelineInput{
		SourceCommand: "quote AAPL.US",
		ResultJSON:    `{"price":123}`,
		InputJSON:     `{"source_command":"quote AAPL.US","result":{"price":123}}`,
	})

	joined := strings.Join(injected, "\n")
	if !strings.Contains(joined, pipelineEnvSourceCommand+"=quote AAPL.US") {
		t.Fatalf("expected source command env updated, got %v", injected)
	}
	if !strings.Contains(joined, pipelineEnvResultJSON+`={"price":123}`) {
		t.Fatalf("expected result json env added, got %v", injected)
	}
	if !strings.Contains(joined, pipelineEnvInputJSON+`={"source_command":"quote AAPL.US","result":{"price":123}}`) {
		t.Fatalf("expected input json env added, got %v", injected)
	}
}

func TestAppendSystemCommandLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "system.log")
	valid := true
	result := systemCommandResult{
		Mode:            "shell",
		CommandLine:     "echo hi",
		ExitCode:        0,
		Success:         true,
		Stdout:          "hi\n",
		StdoutJSON:      map[string]any{"ok": true},
		StdoutJSONValid: &valid,
	}
	if err := appendSystemCommandLog(logPath, result, nil); err != nil {
		t.Fatalf("appendSystemCommandLog failed: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file failed: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("decode system log failed: %v raw=%s", err, string(raw))
	}
	resultObj, ok := entry["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object in log, got %#v", entry["result"])
	}
	if resultObj["command_line"] != "echo hi" {
		t.Fatalf("expected command line in log, got %#v", resultObj["command_line"])
	}
	if resultObj["success"] != true {
		t.Fatalf("expected success flag in log, got %#v", resultObj["success"])
	}
	if resultObj["stdout_json_valid"] != true {
		t.Fatalf("expected stdout_json_valid=true in log, got %#v", resultObj["stdout_json_valid"])
	}
}
