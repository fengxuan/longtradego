package service

import "github.com/spf13/cobra"

func NewQuoteCommand(app *AppContext) *cobra.Command {
	return newQuoteCommand(app)
}

func NewEmailCommand(app *AppContext) *cobra.Command {
	return newEmailCommand(app)
}

func NewSystemCommand(app *AppContext) *cobra.Command {
	return newSystemCommand(app)
}

func NewAdminCommand(app *AppContext) *cobra.Command {
	return newAdminCommand(app)
}

func NewDaemonCommand(app *AppContext, commandLogger *CommandFileLogger) *cobra.Command {
	return newDaemonCommand(app, commandLogger)
}

func NewTaskCommand() *cobra.Command {
	return newTaskCommand()
}

func NewWebhookCommand(app *AppContext) *cobra.Command {
	return newWebhookCommand(app)
}

func NewBookingCommand(app *AppContext) *cobra.Command {
	return newBookingCommand(app)
}

func NewVersionCommand(app *AppContext) *cobra.Command {
	return newVersionCommand(app)
}

func NewUpgradeCommand(app *AppContext) *cobra.Command {
	return newUpgradeCommand(app)
}
