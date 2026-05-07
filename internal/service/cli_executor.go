package service

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

type CLIExecutor func(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error

var cliExecutor CLIExecutor

func SetCLIExecutor(executor CLIExecutor) {
	cliExecutor = executor
}

func executeCLICommand(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error {
	if cliExecutor != nil {
		return cliExecutor(ctx, app, commandLogger, rawArgs)
	}

	execArgs := NormalizeArgs(rawArgs)
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	command, symbols := InferCommandMetadata(execArgs)

	if commandLogger != nil {
		commandLogger.LogExecution(runID, rawArgs, command, symbols, "started", nil, 0, nil)
	}
	if app != nil {
		app.ResetExecution()
	}

	root := newServiceRootCommand(app, commandLogger)
	root.SetArgs(execArgs)
	if err := root.ExecuteContext(ctx); err != nil {
		if app != nil {
			finalCommand, finalSymbols, result := app.ExecutionSnapshot()
			if finalCommand != "" {
				command = finalCommand
			}
			if len(finalSymbols) > 0 {
				symbols = finalSymbols
			}
			if commandLogger != nil {
				commandLogger.LogExecution(runID, rawArgs, command, symbols, "failed", result, 0, err)
			}
		}
		return err
	}

	if app != nil && commandLogger != nil {
		finalCommand, finalSymbols, result := app.ExecutionSnapshot()
		if finalCommand != "" {
			command = finalCommand
		}
		if len(finalSymbols) > 0 {
			symbols = finalSymbols
		}
		commandLogger.LogExecution(runID, rawArgs, command, symbols, "success", result, 0, nil)
	}
	return nil
}

func newServiceRootCommand(app *AppContext, commandLogger *CommandFileLogger) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "longtradego",
		Short:         "Longbridge CLI demo",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.AddCommand(newQuoteCommand(app))
	rootCmd.AddCommand(newLongbridgeCommand(app))
	rootCmd.AddCommand(newSkillCommand(app))
	rootCmd.AddCommand(newEmailCommand(app))
	rootCmd.AddCommand(newSystemCommand(app))
	rootCmd.AddCommand(newAdminCommand(app))
	rootCmd.AddCommand(newDaemonCommand(app, commandLogger))
	rootCmd.AddCommand(newTaskCommand())
	rootCmd.AddCommand(newWebhookCommand(app))
	rootCmd.AddCommand(newBookingCommand(app))
	rootCmd.AddCommand(newVersionCommand(app))
	rootCmd.AddCommand(newUpgradeCommand(app))
	return rootCmd
}
