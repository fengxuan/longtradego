package main

import "github.com/spf13/cobra"

func newRootCommand(app *appContext, commandLogger *commandFileLogger) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "longtradego",
		Short:         "Longbridge CLI demo",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.AddCommand(newQuoteCommand(app))
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
