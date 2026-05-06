package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const longbridgeCLIProgram = "longbridge"

type longbridgeCommandResult struct {
	Command         []string `json:"command"`
	CommandLine     string   `json:"command_line"`
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

var (
	longbridgeLookPath                 = exec.LookPath
	longbridgeCommandContext           = exec.CommandContext
	longbridgeCommandStdout  io.Writer = os.Stdout
	longbridgeCommandStderr  io.Writer = os.Stderr
)

func newLongbridgeCommand(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:                "longbridge [ARGS...]",
		Aliases:            []string{"lb"},
		Short:              "Run Longbridge CLI commands directly",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			forwarded := append([]string(nil), args...)
			if len(forwarded) == 0 {
				forwarded = []string{"--help"}
			}

			app.SetExecution("longbridge", append([]string(nil), forwarded...))
			result, err := runLongbridgeCommand(cmd.Context(), forwarded)
			app.SetResult(result)
			return err
		},
	}
}

func runLongbridgeCommand(ctx context.Context, args []string) (longbridgeCommandResult, error) {
	forwarded := append([]string(nil), args...)
	if len(forwarded) == 0 {
		forwarded = []string{"--help"}
	}

	result := longbridgeCommandResult{
		Command:     append([]string{longbridgeCLIProgram}, forwarded...),
		CommandLine: FormatCommandLine(append([]string{longbridgeCLIProgram}, forwarded...)),
		ExitCode:    -1,
	}

	startedAt := time.Now()
	binaryPath, err := longbridgeLookPath(longbridgeCLIProgram)
	if err != nil {
		return result, fmt.Errorf("longbridge CLI not found in PATH: %w", err)
	}

	command := longbridgeCommandContext(ctx, binaryPath, forwarded...)
	if pipelineInput, ok := daemonPipelineInputFromContext(ctx); ok {
		command.Env = injectPipelineEnv(os.Environ(), pipelineInput)
		if strings.TrimSpace(pipelineInput.InputJSON) != "" {
			command.Stdin = strings.NewReader(pipelineInput.InputJSON)
		}
	}

	var stdoutBuffer bytes.Buffer
	var stderrBuffer bytes.Buffer
	command.Stdout = io.MultiWriter(&stdoutBuffer, writerOrDiscard(longbridgeCommandStdout))
	command.Stderr = io.MultiWriter(&stderrBuffer, writerOrDiscard(longbridgeCommandStderr))

	runErr := command.Run()
	result.DurationMs = time.Since(startedAt).Milliseconds()
	result.Stdout = stdoutBuffer.String()
	result.Stderr = stderrBuffer.String()
	result.StdoutJSON, result.StdoutJSONValid = parseSystemCommandOutputJSON(result.Stdout)
	result.StderrJSON, result.StderrJSONValid = parseSystemCommandOutputJSON(result.Stderr)
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
		return result, fmt.Errorf("longbridge command timed out: %w", runErr)
	}
	if result.ExitCode >= 0 {
		errText := strings.TrimSpace(result.Stderr)
		if errText == "" {
			return result, fmt.Errorf("longbridge command failed with exit code %d: %w", result.ExitCode, runErr)
		}
		return result, fmt.Errorf("longbridge command failed with exit code %d: %s", result.ExitCode, errText)
	}
	return result, fmt.Errorf("longbridge command failed: %w", runErr)
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
