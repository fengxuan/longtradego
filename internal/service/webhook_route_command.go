package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type webhookRouteMutationResult struct {
	Action string             `json:"action"`
	Route  webhookRouteRecord `json:"route"`
}

type webhookRouteListResult struct {
	Routes []webhookRouteRecord `json:"routes"`
}

func newWebhookRouteCommand(app *AppContext) *cobra.Command {
	var routesPath string
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Manage webhook routes",
	}

	addCmd := &cobra.Command{
		Use:   "add <route-id>",
		Short: "Add one webhook route",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			routeID := strings.TrimSpace(args[0])
			if routeID == "" {
				return fmt.Errorf("route id is required")
			}
			record, err := parseWebhookRouteFlags(cmd, true)
			if err != nil {
				return err
			}
			record.ID = routeID
			nowText := time.Now().Format(time.RFC3339Nano)
			record.CreatedAt = nowText
			record.UpdatedAt = nowText

			records, err := loadWebhookRouteRecords(routesPath)
			if err != nil {
				return err
			}
			if _, exists := records[routeID]; exists {
				return fmt.Errorf("route %q already exists", routeID)
			}
			records[routeID] = record
			items := make([]webhookRouteRecord, 0, len(records))
			for _, item := range records {
				items = append(items, item)
			}
			if _, err := resolveWebhookRoutes(items, newWebhookServeConfig()); err != nil {
				return err
			}
			if err := writeWebhookRouteRecords(routesPath, records); err != nil {
				return err
			}
			result := webhookRouteMutationResult{Action: "add", Route: record}
			app.SetExecution("webhook", []string{"route", "add", routeID})
			app.SetResult(result)
			fmt.Printf("route added: id=%s path=%s mode=%s\n", record.ID, record.Path, normalizeWebhookRouteMode(record.Mode))
			return nil
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List webhook routes",
		RunE: func(cmd *cobra.Command, args []string) error {
			records, err := loadWebhookRouteRecords(routesPath)
			if err != nil {
				return err
			}
			items := make([]webhookRouteRecord, 0, len(records))
			for _, record := range records {
				items = append(items, normalizeWebhookRouteRecord(record))
			}
			sort.Slice(items, func(i, j int) bool {
				return items[i].ID < items[j].ID
			})
			result := webhookRouteListResult{Routes: items}
			app.SetExecution("webhook", []string{"route", "list"})
			app.SetResult(result)
			if len(items) == 0 {
				fmt.Println("no route")
				return nil
			}
			for _, item := range items {
				fmt.Printf("id=%s path=%s mode=%s enabled=%t pipeline=%q\n", item.ID, item.Path, normalizeWebhookRouteMode(item.Mode), item.Enabled, item.Pipeline)
			}
			return nil
		},
	}

	updateCmd := &cobra.Command{
		Use:   "update <route-id>",
		Short: "Update one webhook route",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			routeID := strings.TrimSpace(args[0])
			if routeID == "" {
				return fmt.Errorf("route id is required")
			}
			records, err := loadWebhookRouteRecords(routesPath)
			if err != nil {
				return err
			}
			existing, exists := records[routeID]
			if !exists {
				return fmt.Errorf("route %q not found", routeID)
			}
			changed, err := applyWebhookRouteFlagUpdates(cmd, &existing)
			if err != nil {
				return err
			}
			if !changed {
				return fmt.Errorf("no route update flags provided")
			}
			existing.ID = routeID
			existing.UpdatedAt = time.Now().Format(time.RFC3339Nano)
			records[routeID] = existing

			items := make([]webhookRouteRecord, 0, len(records))
			for _, item := range records {
				items = append(items, item)
			}
			if _, err := resolveWebhookRoutes(items, newWebhookServeConfig()); err != nil {
				return err
			}
			if err := writeWebhookRouteRecords(routesPath, records); err != nil {
				return err
			}

			result := webhookRouteMutationResult{Action: "update", Route: existing}
			app.SetExecution("webhook", []string{"route", "update", routeID})
			app.SetResult(result)
			fmt.Printf("route updated: id=%s path=%s mode=%s\n", existing.ID, existing.Path, normalizeWebhookRouteMode(existing.Mode))
			return nil
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <route-id> <confirm-route-id>",
		Short: "Remove one webhook route",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			routeID := strings.TrimSpace(args[0])
			confirmID := strings.TrimSpace(args[1])
			if routeID == "" {
				return fmt.Errorf("route id is required")
			}
			if routeID != confirmID {
				return fmt.Errorf("route id confirmation mismatch: expected %q, got %q", routeID, confirmID)
			}
			records, err := loadWebhookRouteRecords(routesPath)
			if err != nil {
				return err
			}
			record, exists := records[routeID]
			if !exists {
				return fmt.Errorf("route %q not found", routeID)
			}
			delete(records, routeID)
			if err := writeWebhookRouteRecords(routesPath, records); err != nil {
				return err
			}
			result := webhookRouteMutationResult{Action: "remove", Route: record}
			app.SetExecution("webhook", []string{"route", "remove", routeID})
			app.SetResult(result)
			fmt.Printf("route removed: id=%s\n", routeID)
			return nil
		},
	}

	bindWebhookRouteFlags(addCmd)
	bindWebhookRouteFlags(updateCmd)

	cmd.PersistentFlags().StringVar(&routesPath, "routes-file", defaultWebhookRouteStatePath(), "Path to webhook route definitions JSON")
	cmd.AddCommand(addCmd, listCmd, updateCmd, removeCmd)
	return cmd
}

func bindWebhookRouteFlags(cmd *cobra.Command) {
	cmd.Flags().String("path", "", "Webhook route path")
	cmd.Flags().String("mode", webhookRouteModeSync, "Route mode: sync or async")
	cmd.Flags().String("pipeline", "", "Downstream command pipeline")
	cmd.Flags().Bool("enabled", true, "Whether route is enabled")
	cmd.Flags().Int("max-attempts", defaultWebhookAsyncMaxAttempts, "Async max attempts")
	cmd.Flags().String("retry-backoff", defaultWebhookAsyncRetryBackoff.String(), "Async retry backoff duration")
	cmd.Flags().String("timeout", defaultWebhookDownstreamTimeout.String(), "Downstream timeout duration")
}

func parseWebhookRouteFlags(cmd *cobra.Command, requirePath bool) (webhookRouteRecord, error) {
	path, _ := cmd.Flags().GetString("path")
	mode, _ := cmd.Flags().GetString("mode")
	pipeline, _ := cmd.Flags().GetString("pipeline")
	enabled, _ := cmd.Flags().GetBool("enabled")
	maxAttempts, _ := cmd.Flags().GetInt("max-attempts")
	retryBackoff, _ := cmd.Flags().GetString("retry-backoff")
	timeout, _ := cmd.Flags().GetString("timeout")

	if requirePath && strings.TrimSpace(path) == "" {
		return webhookRouteRecord{}, fmt.Errorf("--path is required")
	}
	record := webhookRouteRecord{
		Path:         path,
		Mode:         mode,
		Pipeline:     pipeline,
		Enabled:      enabled,
		MaxAttempts:  maxAttempts,
		RetryBackoff: retryBackoff,
		Timeout:      timeout,
	}
	parsedMode, modeErr := parseWebhookRouteMode(record.Mode)
	if modeErr != nil {
		return webhookRouteRecord{}, modeErr
	}
	record.Mode = parsedMode
	record = normalizeWebhookRouteRecord(record)
	if requirePath && record.Path == "" {
		return webhookRouteRecord{}, fmt.Errorf("route path is required")
	}
	return record, nil
}

func applyWebhookRouteFlagUpdates(cmd *cobra.Command, record *webhookRouteRecord) (bool, error) {
	if record == nil {
		return false, fmt.Errorf("route record is nil")
	}
	changed := false
	if cmd.Flags().Changed("path") {
		value, _ := cmd.Flags().GetString("path")
		if strings.TrimSpace(value) == "" {
			return false, fmt.Errorf("--path cannot be empty")
		}
		record.Path = value
		changed = true
	}
	if cmd.Flags().Changed("mode") {
		value, _ := cmd.Flags().GetString("mode")
		parsedMode, modeErr := parseWebhookRouteMode(value)
		if modeErr != nil {
			return false, modeErr
		}
		record.Mode = parsedMode
		changed = true
	}
	if cmd.Flags().Changed("pipeline") {
		value, _ := cmd.Flags().GetString("pipeline")
		record.Pipeline = value
		changed = true
	}
	if cmd.Flags().Changed("enabled") {
		value, _ := cmd.Flags().GetBool("enabled")
		record.Enabled = value
		changed = true
	}
	if cmd.Flags().Changed("max-attempts") {
		value, _ := cmd.Flags().GetInt("max-attempts")
		record.MaxAttempts = value
		changed = true
	}
	if cmd.Flags().Changed("retry-backoff") {
		value, _ := cmd.Flags().GetString("retry-backoff")
		record.RetryBackoff = value
		changed = true
	}
	if cmd.Flags().Changed("timeout") {
		value, _ := cmd.Flags().GetString("timeout")
		record.Timeout = value
		changed = true
	}
	*record = normalizeWebhookRouteRecord(*record)
	return changed, nil
}
