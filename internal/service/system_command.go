package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const systemCommandLogFileName = "system_command.log"

type systemCommandResult struct {
	Mode            string   `json:"mode"`
	CommandLine     string   `json:"command_line"`
	Command         []string `json:"command,omitempty"`
	ShellCommand    string   `json:"shell_command,omitempty"`
	ExitCode        int      `json:"exit_code"`
	DurationMs      int64    `json:"duration_ms"`
	Stdout          string   `json:"stdout,omitempty"`
	StdoutJSON      any      `json:"stdout_json,omitempty"`
	StdoutJSONValid *bool    `json:"stdout_json_valid,omitempty"`
	Stderr          string   `json:"stderr,omitempty"`
	StderrJSON      any      `json:"stderr_json,omitempty"`
	StderrJSONValid *bool    `json:"stderr_json_valid,omitempty"`
	Success         bool     `json:"success"`
	TimedOut        bool     `json:"timed_out,omitempty"`
}

type systemCommandLogEntry struct {
	Timestamp string              `json:"timestamp"`
	Result    systemCommandResult `json:"result"`
	Error     string              `json:"error,omitempty"`
}

func newSystemCommand(app *AppContext) *cobra.Command {
	var (
		shellCommand string
		timeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:     "sys [--shell <command>] [--timeout <duration>] [-- <program> [arg...]]",
		Aliases: []string{"shell"},
		Short:   "Execute Linux system commands for automation",
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout < 0 {
				return fmt.Errorf("timeout must be >= 0")
			}

			trimmedShell := strings.TrimSpace(shellCommand)
			if trimmedShell != "" && len(args) > 0 {
				return fmt.Errorf("cannot use command args with --shell at the same time")
			}
			if trimmedShell == "" && len(args) == 0 {
				return fmt.Errorf("missing command, usage: sys [--shell <command>] [-- <program> [arg...]]")
			}

			symbols := make([]string, 0, 1)
			if trimmedShell != "" {
				symbols = append(symbols, "shell")
			} else {
				symbols = append(symbols, args[0])
			}
			app.SetExecution("sys", symbols)

			runCtx := cmd.Context()
			var cancel context.CancelFunc
			if timeout > 0 {
				runCtx, cancel = context.WithTimeout(runCtx, timeout)
				defer cancel()
			}

			result, runErr := runSystemCommand(runCtx, args, trimmedShell)
			app.SetResult(result)
			_ = appendSystemCommandLog(defaultSystemCommandLogPath(), result, runErr)

			return runErr
		},
	}

	cmd.Flags().StringVar(&shellCommand, "shell", "", "Run command with /bin/sh -lc")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Timeout for command execution, e.g. 5s, 1m")
	return cmd
}

func defaultSystemCommandLogPath() string {
	return filepath.Join(commandLogDir, systemCommandLogFileName)
}

func runSystemCommand(ctx context.Context, args []string, shellCommand string) (systemCommandResult, error) {
	result := systemCommandResult{ExitCode: -1}
	startedAt := time.Now()

	var cmd *exec.Cmd
	if shellCommand != "" {
		if len(args) > 0 {
			return result, fmt.Errorf("cannot use command args with shell command")
		}
		shellProgram := resolveShellProgram()
		result.Mode = "shell"
		result.ShellCommand = shellCommand
		result.CommandLine = shellCommand
		cmd = exec.CommandContext(ctx, shellProgram, "-lc", shellCommand)
	} else {
		if len(args) == 0 {
			return result, fmt.Errorf("missing command args")
		}
		result.Mode = "exec"
		result.Command = append([]string(nil), args...)
		result.CommandLine = FormatCommandLine(args)
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	}
	if pipelineInput, ok := daemonPipelineInputFromContext(ctx); ok {
		cmd.Env = injectPipelineEnv(os.Environ(), pipelineInput)
		if strings.TrimSpace(pipelineInput.InputJSON) != "" {
			cmd.Stdin = strings.NewReader(pipelineInput.InputJSON)
		}
	}

	var stdoutBuffer bytes.Buffer
	var stderrBuffer bytes.Buffer
	cmd.Stdout = &stdoutBuffer
	cmd.Stderr = &stderrBuffer

	runErr := cmd.Run()
	result.DurationMs = time.Since(startedAt).Milliseconds()
	result.Stdout = stdoutBuffer.String()
	result.Stderr = stderrBuffer.String()
	applySystemCommandOutputInsights(&result)
	result.Success = runErr == nil

	if runErr == nil {
		result.ExitCode = 0
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
	}
	if result.TimedOut {
		return result, fmt.Errorf("system command timed out: %w", runErr)
	}
	if result.ExitCode >= 0 {
		errText := strings.TrimSpace(result.Stderr)
		if errText == "" {
			return result, fmt.Errorf("system command failed with exit code %d: %w", result.ExitCode, runErr)
		}
		return result, fmt.Errorf("system command failed with exit code %d: %s", result.ExitCode, errText)
	}
	return result, fmt.Errorf("system command failed: %w", runErr)
}

func applySystemCommandOutputInsights(result *systemCommandResult) {
	if result == nil {
		return
	}
	result.StdoutJSON, result.StdoutJSONValid = parseSystemCommandOutputJSON(result.Stdout)
	result.StderrJSON, result.StderrJSONValid = parseSystemCommandOutputJSON(result.Stderr)
}

func parseSystemCommandOutputJSON(raw string) (any, *bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, nil
	}

	valid := true
	return decoded, &valid
}

func injectPipelineEnv(env []string, input daemonPipelineInput) []string {
	next := append([]string(nil), env...)
	if strings.TrimSpace(input.SourceCommand) != "" {
		next = upsertEnvValue(next, pipelineEnvSourceCommand, input.SourceCommand)
	}
	if strings.TrimSpace(input.ResultJSON) != "" {
		next = upsertEnvValue(next, pipelineEnvResultJSON, input.ResultJSON)
	}
	if strings.TrimSpace(input.InputJSON) != "" {
		next = upsertEnvValue(next, pipelineEnvInputJSON, input.InputJSON)
	}
	return next
}

func upsertEnvValue(env []string, key string, value string) []string {
	prefix := key + "="
	for index, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[index] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func resolveShellProgram() string {
	candidates := make([]string, 0, 4)
	if envShell := strings.TrimSpace(os.Getenv("SHELL")); envShell != "" {
		candidates = append(candidates, envShell)
	}
	candidates = append(candidates, "/bin/zsh", "zsh", "/bin/sh")

	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil && strings.TrimSpace(path) != "" {
			return path
		}
	}
	return "/bin/sh"
}

func appendSystemCommandLog(path string, result systemCommandResult, runErr error) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}

	entry := systemCommandLogEntry{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Result:    result,
	}
	if runErr != nil {
		entry.Error = runErr.Error()
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line := append(data, '\n')
	return appendLogLineWithRotation(path, line, commandLogMaxSize, "system_command")
}
