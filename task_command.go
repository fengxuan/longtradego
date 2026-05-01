package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newTaskCommand() *cobra.Command {
	taskCmd := &cobra.Command{
		Use:   "task",
		Short: "Manage daemon scheduled tasks",
		Long:  "Manage scheduled tasks in daemon mode: add/list/pause/resume/global-pause/global-resume/remove.",
		RunE:  runTaskOutsideDaemon,
	}

	taskCmd.AddCommand(&cobra.Command{
		Use:   "add",
		Short: "Add a scheduled task in daemon mode",
		RunE:  runTaskOutsideDaemon,
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List scheduled tasks in daemon mode",
		RunE:    runTaskOutsideDaemon,
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "pause <task-id>",
		Short: "Pause a scheduled task in daemon mode",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runTaskOutsideDaemon,
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "global-pause",
		Short: "Pause all scheduled tasks globally (master switch)",
		RunE:  runTaskOutsideDaemon,
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "resume <task-id>",
		Short: "Resume a scheduled task in daemon mode",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runTaskOutsideDaemon,
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "global-resume",
		Short: "Resume all scheduled tasks globally (master switch)",
		RunE:  runTaskOutsideDaemon,
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:     "remove <task-id> <confirm-task-id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Remove a scheduled task in daemon mode (requires task id confirmation)",
		Args:    cobra.MinimumNArgs(2),
		RunE:    runTaskOutsideDaemon,
	})

	return taskCmd
}

func runTaskOutsideDaemon(cmd *cobra.Command, _ []string) error {
	return fmt.Errorf("task command is available in daemon mode only, start with: longtradego daemon")
}
