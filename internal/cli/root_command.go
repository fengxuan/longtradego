package cli

import (
	"github.com/spf13/cobra"

	"longtradego/internal/core"
	"longtradego/internal/service"
)

func newRootCommand(app *core.AppContext, commandLogger *core.CommandFileLogger) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "longtradego",
		Short:         "Longbridge CLI demo",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.AddCommand(service.NewQuoteCommand(app))
	rootCmd.AddCommand(service.NewLongbridgeCommand(app))
	rootCmd.AddCommand(service.NewEmailCommand(app))
	rootCmd.AddCommand(service.NewSystemCommand(app))
	rootCmd.AddCommand(service.NewAdminCommand(app))
	rootCmd.AddCommand(service.NewDaemonCommand(app, commandLogger))
	rootCmd.AddCommand(service.NewTaskCommand())
	rootCmd.AddCommand(service.NewWebhookCommand(app))
	rootCmd.AddCommand(service.NewBookingCommand(app))
	rootCmd.AddCommand(service.NewVersionCommand(app))
	rootCmd.AddCommand(service.NewUpgradeCommand(app))
	return rootCmd
}
