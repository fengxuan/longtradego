package cli

import (
	"context"
	"time"

	"longtradego/internal/core"
)

func executeCLICommand(ctx context.Context, app *core.AppContext, commandLogger *core.CommandFileLogger, rawArgs []string) error {
	execArgs := core.NormalizeArgs(rawArgs)
	startedAt := time.Now()
	runID := newRunID()

	command, symbols := core.InferCommandMetadata(execArgs)
	if commandLogger != nil {
		commandLogger.LogExecution(runID, rawArgs, command, symbols, "started", nil, 0, nil)
	}

	app.ResetExecution()

	rootCmd := newRootCommand(app, commandLogger)
	rootCmd.SetArgs(execArgs)
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		finalCommand, finalSymbols, result := app.ExecutionSnapshot()
		if finalCommand != "" {
			command = finalCommand
		}
		if len(finalSymbols) > 0 {
			symbols = finalSymbols
		}

		if commandLogger != nil {
			commandLogger.LogExecution(runID, rawArgs, command, symbols, "failed", result, time.Since(startedAt), err)
		}
		return err
	}

	finalCommand, finalSymbols, result := app.ExecutionSnapshot()
	if finalCommand != "" {
		command = finalCommand
	}
	if len(finalSymbols) > 0 {
		symbols = finalSymbols
	}
	if commandLogger != nil {
		commandLogger.LogExecution(runID, rawArgs, command, symbols, "success", result, time.Since(startedAt), nil)
	}
	return nil
}
