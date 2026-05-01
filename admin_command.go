package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type adminStatusResult struct {
	Mode    string                  `json:"mode"`
	Status  string                  `json:"status"`
	Runtime *daemonAdminRuntimeInfo `json:"runtime,omitempty"`
	URL     string                  `json:"url,omitempty"`
	Message string                  `json:"message,omitempty"`
}

func newAdminCommand(app *appContext) *cobra.Command {
	var runtimePath string

	adminCmd := &cobra.Command{
		Use:   "admin",
		Short: "Inspect daemon admin service runtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAdminStatus(app, runtimePath)
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon admin runtime status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAdminStatus(app, runtimePath)
		},
	}

	adminCmd.Flags().StringVar(&runtimePath, "runtime", defaultDaemonAdminRuntimePath(), "Path to daemon admin runtime state JSON")
	statusCmd.Flags().StringVar(&runtimePath, "runtime", defaultDaemonAdminRuntimePath(), "Path to daemon admin runtime state JSON")
	adminCmd.AddCommand(statusCmd)
	return adminCmd
}

func runAdminStatus(app *appContext, runtimePath string) error {
	status, runtime, err := daemonAdminRuntimeStatus(runtimePath)
	if err != nil {
		return err
	}
	result := adminStatusResult{
		Mode:    "status",
		Status:  status,
		Runtime: runtime,
	}
	if runtime != nil {
		result.URL = defaultDaemonAdminURL(runtime.Address)
	}
	if status == "stale" {
		result.Message = "admin runtime exists but daemon process is not running"
	}
	if status == "stopped" {
		result.Message = "admin runtime state not found"
	}

	if app != nil {
		app.SetExecution("admin", []string{"status"})
		app.SetResult(result)
	}

	switch status {
	case "running":
		if runtime != nil {
			fmt.Printf("admin status=%s pid=%d addr=%s url=%s\n", status, runtime.PID, runtime.Address, strings.TrimSpace(result.URL))
		} else {
			fmt.Printf("admin status=%s\n", status)
		}
	case "stale":
		if runtime != nil {
			fmt.Printf("admin status=%s pid=%d addr=%s\n", status, runtime.PID, runtime.Address)
		} else {
			fmt.Printf("admin status=%s\n", status)
		}
	default:
		fmt.Printf("admin status=%s\n", status)
	}
	if strings.TrimSpace(result.Message) != "" {
		fmt.Println(result.Message)
	}
	return nil
}
