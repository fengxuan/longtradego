package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const (
	webhookTokenStateVersion   = 1
	webhookRuntimeStateVersion = 1
	webhookTokenStateFile      = "webhook_tokens.json"
	webhookRuntimeStateFile    = "webhook_runtime.json"
	webhookEventLogFile        = "webhook_events.json"

	defaultWebhookServeAddr      = ":8080"
	defaultWebhookServePath      = "/webhook/events"
	defaultWebhookTimestampSkew  = 5 * time.Minute
	defaultWebhookMaxBodyBytes   = int64(1024 * 1024)
	defaultWebhookStopTimeout    = 5 * time.Second
	defaultWebhookSendTimeout    = 10 * time.Second
	defaultWebhookStartTimeout   = 3 * time.Second
	defaultWebhookStartPoll      = 100 * time.Millisecond
	webhookServeInternalEnv      = "LONGTRADEGO_WEBHOOK_INTERNAL_SERVE"
	webhookServeRuntimePathEnv   = "LONGTRADEGO_WEBHOOK_RUNTIME_PATH"
	webhookHeaderThirdPartyID    = "X-Third-Party-ID"
	webhookHeaderTimestamp       = "X-Webhook-Timestamp"
	webhookHeaderSignature       = "X-Webhook-Token"
	webhookSignMethod            = "POST"
	defaultWebhookTokenByteSize  = 32
	defaultWebhookResponseIndent = "  "
)

type webhookTokenState struct {
	Version int                  `json:"version"`
	Tokens  []webhookTokenRecord `json:"tokens"`
}

type webhookRuntimeState struct {
	Version int                `json:"version"`
	Runtime webhookRuntimeInfo `json:"runtime"`
}

type webhookTokenRecord struct {
	ThirdPartyID string `json:"third_party_id"`
	Token        string `json:"token"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type webhookRuntimeInfo struct {
	PID                int    `json:"pid"`
	Address            string `json:"address"`
	Path               string `json:"path"`
	TokenStore         string `json:"token_store"`
	EventLog           string `json:"event_log"`
	RoutesFile         string `json:"routes_file,omitempty"`
	AuditLog           string `json:"audit_log,omitempty"`
	DispatchQueue      string `json:"dispatch_queue,omitempty"`
	DispatchHistory    string `json:"dispatch_history,omitempty"`
	DeadLetter         string `json:"dead_letter,omitempty"`
	AllowSysDownstream bool   `json:"allow_sys_downstream,omitempty"`
	DispatchWorkers    int    `json:"dispatch_workers,omitempty"`
	TimestampSkew      string `json:"timestamp_skew,omitempty"`
	MaxBodyBytes       int64  `json:"max_body_bytes,omitempty"`
	StartedAt          string `json:"started_at"`
	UpdatedAt          string `json:"updated_at,omitempty"`
	OwnerSessionID     string `json:"owner_session_id,omitempty"`
	OwnerDaemonPID     int    `json:"owner_daemon_pid,omitempty"`
	OwnerClaimedAt     string `json:"owner_claimed_at,omitempty"`
	OwnerStartToken    string `json:"owner_start_token,omitempty"`
}

type webhookTokenMutationResult struct {
	Action       string `json:"action"`
	ThirdPartyID string `json:"third_party_id"`
	Token        string `json:"token"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type webhookTokenQueryResult struct {
	ThirdPartyID string `json:"third_party_id"`
	TokenMasked  string `json:"token_masked"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type webhookSignResult struct {
	Method    string            `json:"method"`
	Path      string            `json:"path,omitempty"`
	URL       string            `json:"url,omitempty"`
	Timestamp string            `json:"timestamp"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	Signature string            `json:"signature"`
}

type webhookSendResult struct {
	TargetURL              string            `json:"target_url"`
	RoutePath              string            `json:"route_path"`
	RequestHeaders         map[string]string `json:"request_headers"`
	HTTPStatus             int               `json:"http_status,omitempty"`
	ResponseBody           string            `json:"response_body,omitempty"`
	ResponseJSON           any               `json:"response_json,omitempty"`
	ResponseEventID        string            `json:"response_event_id,omitempty"`
	ResponseThirdPartyID   string            `json:"response_third_party_id,omitempty"`
	ResponseTokenValid     *bool             `json:"response_token_valid,omitempty"`
	ResponseTimestampValid *bool             `json:"response_timestamp_valid,omitempty"`
	ResponseJSONValid      *bool             `json:"response_json_valid,omitempty"`
	ResponseMetaError      string            `json:"response_meta_error,omitempty"`
	DurationMs             int64             `json:"duration_ms"`
}

type webhookDebugPipelineResult struct {
	Mode           string                    `json:"mode"`
	EventID        string                    `json:"event_id,omitempty"`
	RouteID        string                    `json:"route_id,omitempty"`
	Method         string                    `json:"method"`
	Path           string                    `json:"path"`
	ResponseStatus int                       `json:"response_status"`
	PipelineSeed   map[string]any            `json:"pipeline_seed"`
	DataParseError string                    `json:"data_parse_error,omitempty"`
	Dispatch       *webhookDispatchAuditInfo `json:"dispatch,omitempty"`
}

type webhookServeResult struct {
	Mode          string                `json:"mode"`
	Address       string                `json:"address"`
	Path          string                `json:"path"`
	TokenStore    string                `json:"token_store"`
	EventLog      string                `json:"event_log"`
	RoutesFile    string                `json:"routes_file,omitempty"`
	AuditLog      string                `json:"audit_log,omitempty"`
	RouteCount    int                   `json:"route_count,omitempty"`
	Routes        []webhookRouteSummary `json:"routes,omitempty"`
	TimestampSkew string                `json:"timestamp_skew"`
}

type webhookStartResult struct {
	Mode      string              `json:"mode"`
	Status    string              `json:"status"`
	Message   string              `json:"message,omitempty"`
	Runtime   *webhookRuntimeInfo `json:"runtime,omitempty"`
	LogPath   string              `json:"log_path,omitempty"`
	StartArgs []string            `json:"start_args,omitempty"`
}

type webhookStatusResult struct {
	Mode       string                  `json:"mode"`
	Status     string                  `json:"status"`
	Running    bool                    `json:"running"`
	Runtime    *webhookRuntimeInfo     `json:"runtime,omitempty"`
	RouteCount int                     `json:"route_count,omitempty"`
	Routes     []webhookRouteSummary   `json:"routes,omitempty"`
	Dispatch   webhookDispatchStats    `json:"dispatch"`
	Metrics    *webhookMetricsSnapshot `json:"metrics,omitempty"`
	LogQueue   *webhookLogQueueDepth   `json:"log_queue_depth,omitempty"`
}

type webhookStopResult struct {
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

type webhookKillPortFailure struct {
	PID   int    `json:"pid"`
	Error string `json:"error"`
}

type webhookKillPortResult struct {
	Mode        string                   `json:"mode"`
	Status      string                   `json:"status"`
	Address     string                   `json:"address"`
	Port        int                      `json:"port"`
	MatchedPIDs []int                    `json:"matched_pids,omitempty"`
	StoppedPIDs []int                    `json:"stopped_pids,omitempty"`
	Failed      []webhookKillPortFailure `json:"failed,omitempty"`
	Message     string                   `json:"message,omitempty"`
}

type webhookValidationStatus struct {
	TokenValid     bool `json:"token_valid"`
	TimestampValid bool `json:"timestamp_valid"`
	JSONValid      bool `json:"json_valid"`
}

type webhookEventMeta struct {
	EventID      string                  `json:"event_id"`
	ThirdPartyID string                  `json:"third_party_id,omitempty"`
	ReceivedAt   string                  `json:"received_at"`
	Validation   webhookValidationStatus `json:"validation"`
	Error        string                  `json:"error,omitempty"`
}

type webhookEventEnvelope struct {
	Meta    webhookEventMeta `json:"meta"`
	Data    any              `json:"data"`
	RawBody []byte           `json:"-"`
}

type webhookServeOptions struct {
	Path               string
	RouteID            string
	RouteMode          string
	RoutePipeline      string
	TokenStorePath     string
	EventLogPath       string
	AuditLogPath       string
	TimestampSkew      time.Duration
	MaxBodyBytes       int64
	DownstreamTimeout  time.Duration
	AllowSysDownstream bool
	Dispatcher         *webhookDispatchManager
	ResolvedRoute      *webhookResolvedRoute
	TokenFinder        webhookTokenFinder
	EventLogAppender   func(webhookEventEnvelope) error
	AuditLogAppender   func(webhookAuditLogEntry) error
	Metrics            *webhookServerMetrics
	Now                func() time.Time
}

type webhookServeConfig struct {
	Addr                     string
	Path                     string
	RuntimePath              string
	RoutesPath               string
	TokenStore               string
	EventLogPath             string
	AuditLogPath             string
	DispatchQueuePath        string
	DispatchHistoryPath      string
	DeadLetterPath           string
	TimeSkew                 time.Duration
	MaxBodyBytes             int64
	AllowSysDownstream       bool
	DispatchWorkers          int
	DispatchPollInterval     time.Duration
	DefaultAsyncMaxAttempts  int
	DefaultAsyncRetryBackoff time.Duration
	DefaultDownstreamTimeout time.Duration
}

type webhookSpawnFunc func(cfg *webhookServeConfig, logPath string) (pid int, commandArgs []string, err error)

var webhookSpawnBackgroundProcess webhookSpawnFunc = spawnWebhookBackgroundProcess
var webhookKillPortListListeningPIDs = listListeningPIDsByPort
var webhookKillPortTerminateProcess = terminateProcess

type webhookStartOwnerClaim struct {
	SessionID  string
	DaemonPID  int
	ClaimedAt  time.Time
	StartToken string
}

type webhookStartOwnerClaimContextKey struct{}

var webhookVerifyBackgroundStart = verifyWebhookBackgroundStart

func withWebhookStartOwnerClaim(ctx context.Context, claim webhookStartOwnerClaim) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, webhookStartOwnerClaimContextKey{}, claim)
}

func webhookStartOwnerClaimFromContext(ctx context.Context) (webhookStartOwnerClaim, bool) {
	if ctx == nil {
		return webhookStartOwnerClaim{}, false
	}
	value := ctx.Value(webhookStartOwnerClaimContextKey{})
	claim, ok := value.(webhookStartOwnerClaim)
	if !ok {
		return webhookStartOwnerClaim{}, false
	}
	claim.SessionID = strings.TrimSpace(claim.SessionID)
	claim.StartToken = strings.TrimSpace(claim.StartToken)
	if claim.SessionID == "" || claim.StartToken == "" {
		return webhookStartOwnerClaim{}, false
	}
	if claim.ClaimedAt.IsZero() {
		claim.ClaimedAt = time.Now()
	}
	return claim, true
}

func newWebhookCommand(app *appContext) *cobra.Command {
	webhookCmd := &cobra.Command{
		Use:   "webhook",
		Short: "Run webhook server and manage webhook tokens/signatures",
	}
	webhookCmd.AddCommand(newWebhookStartCommand(app))
	webhookCmd.AddCommand(newWebhookStatusCommand(app))
	webhookCmd.AddCommand(newWebhookStopCommand(app))
	webhookCmd.AddCommand(newWebhookKillPortCommand(app))
	webhookCmd.AddCommand(newWebhookServeCommand(app))
	webhookCmd.AddCommand(newWebhookRouteCommand(app))
	webhookCmd.AddCommand(newWebhookTokenCommand(app))
	webhookCmd.AddCommand(newWebhookSignCommand(app))
	webhookCmd.AddCommand(newWebhookSendCommand(app))
	webhookCmd.AddCommand(newWebhookDebugPipelineCommand(app))
	return webhookCmd
}

func newWebhookServeCommand(app *appContext) *cobra.Command {
	cfg := newWebhookServeConfig()

	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Start webhook HTTP service (foreground)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(os.Getenv(webhookServeInternalEnv)) != "1" {
				return fmt.Errorf("webhook serve is internal; use `webhook start`")
			}
			if err := cfg.validate(); err != nil {
				return err
			}

			routeRecords, err := ensureWebhookLegacyRoute(cfg.RoutesPath, cfg.Path)
			if err != nil {
				return err
			}
			routes, err := resolveWebhookRoutes(routeRecords, cfg)
			if err != nil {
				return err
			}
			if err := validateWebhookManagementPathConflicts(routes); err != nil {
				return err
			}
			dispatcher, err := newWebhookDispatchManager(webhookDispatcherOptions{
				QueuePath:          cfg.DispatchQueuePath,
				HistoryPath:        cfg.DispatchHistoryPath,
				DeadLetterPath:     cfg.DeadLetterPath,
				PollInterval:       cfg.DispatchPollInterval,
				Workers:            cfg.DispatchWorkers,
				AllowSysDownstream: cfg.AllowSysDownstream,
			})
			if err != nil {
				return err
			}
			dispatcher.Start()
			defer dispatcher.Stop()

			tokenCache, err := newWebhookTokenCache(cfg.TokenStore, defaultWebhookTokenRefreshInterval)
			if err != nil {
				return err
			}
			tokenCache.Start()
			defer tokenCache.Stop()

			eventLogWriter, err := newWebhookEventLogWriter(cfg.EventLogPath)
			if err != nil {
				return err
			}
			eventLogWriter.Start()
			defer eventLogWriter.Stop()

			auditLogWriter, err := newWebhookAuditLogWriter(cfg.AuditLogPath)
			if err != nil {
				return err
			}
			auditLogWriter.Start()
			defer auditLogWriter.Stop()

			metrics := newWebhookServerMetrics(defaultWebhookMetricsWindowSize)
			startedAt := time.Now()
			runtimePath := strings.TrimSpace(os.Getenv(webhookServeRuntimePathEnv))

			app.SetExecution("webhook", []string{"serve", cfg.Addr, cfg.Path})
			app.SetResult(webhookServeResult{
				Mode:          "serve",
				Address:       cfg.Addr,
				Path:          cfg.Path,
				TokenStore:    cfg.TokenStore,
				EventLog:      cfg.EventLogPath,
				RoutesFile:    cfg.RoutesPath,
				AuditLog:      cfg.AuditLogPath,
				RouteCount:    len(routes),
				Routes:        summarizeWebhookRoutes(routes),
				TimestampSkew: cfg.TimeSkew.String(),
			})

			mux := http.NewServeMux()
			var (
				server       *http.Server
				shutdownOnce sync.Once
			)
			requestShutdown := func(source string) {
				shutdownOnce.Do(func() {
					if removed, err := cleanupWebhookRuntimeStateForCurrentProcess(runtimePath); err != nil {
						fmt.Printf("webhook runtime cleanup skipped: %v\n", err)
					} else if removed {
						fmt.Printf("webhook runtime state removed on shutdown source=%s\n", source)
					}
					if server == nil {
						return
					}
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = server.Shutdown(shutdownCtx)
				})
			}
			registerWebhookManagementHandlers(mux, cfg, routes, metrics, tokenCache, eventLogWriter, auditLogWriter, startedAt, requestShutdown)

			tokenFinder := func(thirdPartyID string) (webhookTokenRecord, bool, error) {
				return tokenCache.Find(thirdPartyID)
			}
			eventAppender := func(event webhookEventEnvelope) error {
				return appendWebhookEventLogAsync(eventLogWriter, event)
			}
			auditAppender := func(entry webhookAuditLogEntry) error {
				return appendWebhookAuditLogAsync(auditLogWriter, entry)
			}

			for _, route := range routes {
				resolvedRoute := route
				handler := newWebhookEventHandler(webhookServeOptions{
					Path:               route.Record.Path,
					RouteID:            route.Record.ID,
					RouteMode:          route.Mode,
					RoutePipeline:      route.Record.Pipeline,
					TokenStorePath:     cfg.TokenStore,
					EventLogPath:       cfg.EventLogPath,
					AuditLogPath:       cfg.AuditLogPath,
					TimestampSkew:      cfg.TimeSkew,
					MaxBodyBytes:       cfg.MaxBodyBytes,
					DownstreamTimeout:  route.Timeout,
					AllowSysDownstream: cfg.AllowSysDownstream,
					Dispatcher:         dispatcher,
					ResolvedRoute:      &resolvedRoute,
					TokenFinder:        tokenFinder,
					EventLogAppender:   eventAppender,
					AuditLogAppender:   auditAppender,
					Metrics:            metrics,
				})
				mux.Handle(route.Record.Path, handler)
			}

			server = &http.Server{
				Addr:              cfg.Addr,
				Handler:           mux,
				ReadHeaderTimeout: 5 * time.Second,
			}

			fmt.Printf("Webhook server listening on %s (routes=%d)\n", cfg.Addr, len(routes))
			for _, route := range routes {
				fmt.Printf("  - %s %s mode=%s\n", route.Record.ID, route.Record.Path, route.Mode)
			}

			go func() {
				<-cmd.Context().Done()
				requestShutdown("context_done")
			}()

			err = server.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	bindWebhookServeFlags(cmd, cfg)
	return cmd
}

func newWebhookStartCommand(app *appContext) *cobra.Command {
	cfg := newWebhookServeConfig()
	var (
		runtimePath string
		logPath     string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start webhook service in background (non-blocking)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.validate(); err != nil {
				return err
			}
			runtimePath = strings.TrimSpace(runtimePath)
			logPath = strings.TrimSpace(logPath)
			if runtimePath == "" {
				return fmt.Errorf("runtime is required")
			}
			if logPath == "" {
				return fmt.Errorf("log-file is required")
			}

			var ownerClaim *webhookStartOwnerClaim
			if claim, ok := webhookStartOwnerClaimFromContext(cmd.Context()); ok {
				ownerClaim = &claim
			}

			result, err := startWebhookInBackground(runtimePath, logPath, cfg, ownerClaim)
			if err != nil {
				return err
			}
			app.SetExecution("webhook", []string{"start", cfg.Addr, cfg.Path})
			app.SetResult(result)
			if result.Runtime != nil {
				fmt.Printf("webhook %s: pid=%d addr=%s path=%s\n", result.Status, result.Runtime.PID, result.Runtime.Address, result.Runtime.Path)
			} else {
				fmt.Printf("webhook %s\n", result.Status)
			}
			if strings.TrimSpace(result.Message) != "" {
				fmt.Println(result.Message)
			}
			return nil
		},
	}

	bindWebhookServeFlags(cmd, cfg)
	cmd.Flags().StringVar(&runtimePath, "runtime", defaultWebhookRuntimeStatePath(), "Path to webhook runtime state JSON")
	cmd.Flags().StringVar(&logPath, "log-file", defaultWebhookServerLogPath(), "Path to webhook server log file")
	return cmd
}

func newWebhookStatusCommand(app *appContext) *cobra.Command {
	var runtimePath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show webhook background runtime status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := webhookStatus(runtimePath)
			if err != nil {
				return err
			}
			app.SetExecution("webhook", []string{"status"})
			app.SetResult(result)
			if result.Runtime != nil {
				base := fmt.Sprintf(
					"webhook status=%s running=%t pid=%d addr=%s path=%s routes=%d queue(pending=%d retrying=%d dead=%d)",
					result.Status,
					result.Running,
					result.Runtime.PID,
					result.Runtime.Address,
					result.Runtime.Path,
					result.RouteCount,
					result.Dispatch.Pending,
					result.Dispatch.Retrying,
					result.Dispatch.DeadLetter,
				)
				if result.Metrics != nil && result.LogQueue != nil {
					base += fmt.Sprintf(
						" metrics(req=%d auth=%d avg=%.2fms p95=%dms) logq(event=%d audit=%d)",
						result.Metrics.RequestTotal,
						result.Metrics.AuthFailureTotal,
						result.Metrics.AverageLatencyMs,
						result.Metrics.P95LatencyMs,
						result.LogQueue.Event,
						result.LogQueue.Audit,
					)
				}
				fmt.Println(base)
			} else {
				fmt.Printf("webhook status=%s running=%t\n", result.Status, result.Running)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runtimePath, "runtime", defaultWebhookRuntimeStatePath(), "Path to webhook runtime state JSON")
	return cmd
}

func newWebhookStopCommand(app *appContext) *cobra.Command {
	var (
		runtimePath string
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop background webhook service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return fmt.Errorf("timeout must be > 0")
			}
			result, err := stopWebhook(runtimePath, timeout)
			if err != nil {
				return err
			}
			app.SetExecution("webhook", []string{"stop"})
			app.SetResult(result)
			if result.PID > 0 {
				fmt.Printf("webhook stop status=%s pid=%d\n", result.Status, result.PID)
			} else {
				fmt.Printf("webhook stop status=%s\n", result.Status)
			}
			if strings.TrimSpace(result.Message) != "" {
				fmt.Println(result.Message)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runtimePath, "runtime", defaultWebhookRuntimeStatePath(), "Path to webhook runtime state JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultWebhookStopTimeout, "Graceful stop timeout, e.g. 5s")
	return cmd
}

func newWebhookKillPortCommand(app *appContext) *cobra.Command {
	var (
		addr        string
		timeout     time.Duration
		runtimePath string
		dryRun      bool
	)
	cmd := &cobra.Command{
		Use:   "kill-port",
		Short: "Stop processes listening on webhook port",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return fmt.Errorf("timeout must be > 0")
			}
			result, err := executeWebhookKillPort(addr, timeout, runtimePath, dryRun)
			app.SetExecution("webhook", []string{"kill-port", result.Address})
			app.SetResult(result)
			fmt.Printf(
				"webhook kill-port status=%s addr=%s port=%d matched=%d stopped=%d failed=%d\n",
				result.Status,
				result.Address,
				result.Port,
				len(result.MatchedPIDs),
				len(result.StoppedPIDs),
				len(result.Failed),
			)
			if strings.TrimSpace(result.Message) != "" {
				fmt.Println(result.Message)
			}
			if err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultWebhookServeAddr, "Listen address, e.g. :8080")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultWebhookStopTimeout, "Graceful stop timeout per process, e.g. 5s")
	cmd.Flags().StringVar(&runtimePath, "runtime", defaultWebhookRuntimeStatePath(), "Path to webhook runtime state JSON")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List matched listening pids without stopping them")
	return cmd
}

func newWebhookTokenCommand(app *appContext) *cobra.Command {
	var tokenStore string

	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage webhook raw tokens by third-party ID",
	}

	generateCmd := &cobra.Command{
		Use:   "generate <third-party-id>",
		Short: "Generate a new raw token for third-party ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			thirdPartyID := normalizeThirdPartyID(args[0])
			if thirdPartyID == "" {
				return fmt.Errorf("third-party-id is required")
			}

			record, err := createWebhookToken(tokenStore, thirdPartyID, time.Now())
			if err != nil {
				return err
			}

			result := webhookTokenMutationResult{
				Action:       "generate",
				ThirdPartyID: record.ThirdPartyID,
				Token:        record.Token,
				CreatedAt:    record.CreatedAt,
				UpdatedAt:    record.UpdatedAt,
			}
			app.SetExecution("webhook", []string{"token", "generate", thirdPartyID})
			app.SetResult(result)
			fmt.Printf("token generated: third_party_id=%s token=%s\n", result.ThirdPartyID, result.Token)
			return nil
		},
	}

	queryCmd := &cobra.Command{
		Use:   "query <third-party-id>",
		Short: "Query token metadata for third-party ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			thirdPartyID := normalizeThirdPartyID(args[0])
			if thirdPartyID == "" {
				return fmt.Errorf("third-party-id is required")
			}

			record, ok, err := findWebhookToken(tokenStore, thirdPartyID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("token not found for third-party-id %q", thirdPartyID)
			}

			result := webhookTokenQueryResult{
				ThirdPartyID: record.ThirdPartyID,
				TokenMasked:  maskWebhookToken(record.Token),
				CreatedAt:    record.CreatedAt,
				UpdatedAt:    record.UpdatedAt,
			}
			app.SetExecution("webhook", []string{"token", "query", thirdPartyID})
			app.SetResult(result)
			fmt.Printf("token found: third_party_id=%s token=%s created_at=%s updated_at=%s\n", result.ThirdPartyID, result.TokenMasked, result.CreatedAt, result.UpdatedAt)
			return nil
		},
	}

	resetCmd := &cobra.Command{
		Use:   "reset <third-party-id>",
		Short: "Reset token for third-party ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			thirdPartyID := normalizeThirdPartyID(args[0])
			if thirdPartyID == "" {
				return fmt.Errorf("third-party-id is required")
			}

			record, err := resetWebhookToken(tokenStore, thirdPartyID, time.Now())
			if err != nil {
				return err
			}

			result := webhookTokenMutationResult{
				Action:       "reset",
				ThirdPartyID: record.ThirdPartyID,
				Token:        record.Token,
				CreatedAt:    record.CreatedAt,
				UpdatedAt:    record.UpdatedAt,
			}
			app.SetExecution("webhook", []string{"token", "reset", thirdPartyID})
			app.SetResult(result)
			fmt.Printf("token reset: third_party_id=%s token=%s\n", result.ThirdPartyID, result.Token)
			return nil
		},
	}

	tokenCmd.PersistentFlags().StringVar(&tokenStore, "token-store", defaultWebhookTokenStatePath(), "Path to webhook token store JSON")
	tokenCmd.AddCommand(generateCmd, queryCmd, resetCmd)
	return tokenCmd
}

func newWebhookSignCommand(app *appContext) *cobra.Command {
	var (
		thirdPartyID  string
		rawToken      string
		dataText      string
		dataFile      string
		timestampText string
		path          string
		targetURL     string
	)

	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Generate signed webhook request package",
		RunE: func(cmd *cobra.Command, _ []string) error {
			thirdPartyID = normalizeThirdPartyID(thirdPartyID)
			rawToken = strings.TrimSpace(rawToken)
			if thirdPartyID == "" {
				return fmt.Errorf("third-party-id is required")
			}
			if rawToken == "" {
				return fmt.Errorf("token is required")
			}

			body, err := resolveWebhookSignBody(dataText, dataFile)
			if err != nil {
				return err
			}
			if _, err := parseWebhookJSONObject(body); err != nil {
				return err
			}

			resolvedTimestamp, err := resolveWebhookTimestamp(timestampText, time.Now())
			if err != nil {
				return err
			}
			resolvedPath := ensureWebhookPath(path)

			result := buildWebhookSignResult(thirdPartyID, rawToken, resolvedTimestamp, body, resolvedPath, targetURL)
			app.SetExecution("webhook", []string{"sign", thirdPartyID})
			app.SetResult(result)

			rendered, err := marshalJSONIndentNoHTMLEscape(result, "", defaultWebhookResponseIndent)
			if err != nil {
				return err
			}
			fmt.Println(string(rendered))
			return nil
		},
	}

	cmd.Flags().StringVar(&thirdPartyID, "third-party-id", "", "Third-party ID")
	cmd.Flags().StringVar(&rawToken, "token", "", "Raw token used as signing secret")
	cmd.Flags().StringVar(&dataText, "data", "", "Raw JSON body text")
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Path to file containing raw JSON body")
	cmd.Flags().StringVar(&timestampText, "timestamp", "", "Unix timestamp seconds (optional; defaults to now)")
	cmd.Flags().StringVar(&path, "path", defaultWebhookServePath, "Webhook endpoint path")
	cmd.Flags().StringVar(&targetURL, "url", "", "Optional full webhook URL")
	return cmd
}

func newWebhookSendCommand(app *appContext) *cobra.Command {
	var (
		thirdPartyID  string
		rawToken      string
		dataText      string
		dataFile      string
		timestampText string
		targetURL     string
		timeout       time.Duration
	)

	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a signed webhook test request",
		RunE: func(cmd *cobra.Command, _ []string) error {
			thirdPartyID = normalizeThirdPartyID(thirdPartyID)
			rawToken = strings.TrimSpace(rawToken)
			targetURL = strings.TrimSpace(targetURL)
			if thirdPartyID == "" {
				return fmt.Errorf("third-party-id is required")
			}
			if rawToken == "" {
				return fmt.Errorf("token is required")
			}
			if targetURL == "" {
				return fmt.Errorf("url is required")
			}
			if timeout <= 0 {
				return fmt.Errorf("timeout must be > 0")
			}

			body, err := resolveWebhookSignBody(dataText, dataFile)
			if err != nil {
				return err
			}
			if _, err := parseWebhookJSONObject(body); err != nil {
				return err
			}

			resolvedTimestamp, err := resolveWebhookTimestamp(timestampText, time.Now())
			if err != nil {
				return err
			}

			signResult, err := buildWebhookSendSignResult(thirdPartyID, rawToken, resolvedTimestamp, body, targetURL)
			if err != nil {
				return err
			}
			sendResult, sendErr := executeWebhookSendRequest(signResult, timeout)

			app.SetExecution("webhook", []string{"send", thirdPartyID})
			app.SetResult(sendResult)

			if sendErr != nil {
				return sendErr
			}
			display := buildWebhookSendDisplayResult(sendResult)
			rendered, err := marshalJSONIndentNoHTMLEscape(display, "", defaultWebhookResponseIndent)
			if err != nil {
				return err
			}
			fmt.Println(string(rendered))
			return nil
		},
	}

	cmd.Flags().StringVar(&thirdPartyID, "third-party-id", "", "Third-party ID")
	cmd.Flags().StringVar(&rawToken, "token", "", "Raw token used as signing secret")
	cmd.Flags().StringVar(&dataText, "data", "", "Raw JSON body text")
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Path to file containing raw JSON body")
	cmd.Flags().StringVar(&timestampText, "timestamp", "", "Unix timestamp seconds (optional; defaults to now)")
	cmd.Flags().StringVar(&targetURL, "url", "", "Target webhook URL")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultWebhookSendTimeout, "HTTP request timeout, e.g. 10s")
	return cmd
}

func newWebhookDebugPipelineCommand(app *appContext) *cobra.Command {
	var (
		auditLogPath string
		eventID      string
		routeID      string
	)

	cmd := &cobra.Command{
		Use:   "debug-pipeline",
		Short: "Show webhook downstream pipeline input snapshot from audit log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			entry, err := findWebhookAuditEntryForPipelineDebug(auditLogPath, eventID, routeID)
			if err != nil {
				return err
			}
			result := buildWebhookPipelineDebugResult(entry)
			app.SetExecution("webhook", []string{"debug-pipeline", result.EventID})
			app.SetResult(result)
			rendered, err := marshalJSONIndentNoHTMLEscape(result, "", defaultWebhookResponseIndent)
			if err != nil {
				return err
			}
			fmt.Println(string(rendered))
			return nil
		},
	}

	cmd.Flags().StringVar(&auditLogPath, "audit-log", defaultWebhookAuditLogPath(), "Path to webhook audit JSONL log")
	cmd.Flags().StringVar(&eventID, "event-id", "", "Match one specific webhook event ID")
	cmd.Flags().StringVar(&routeID, "route-id", "", "Match one specific route ID")
	return cmd
}

func bindWebhookServeFlags(cmd *cobra.Command, cfg *webhookServeConfig) {
	cmd.Flags().StringVar(&cfg.Addr, "addr", defaultWebhookServeAddr, "Listen address, e.g. :8080")
	cmd.Flags().StringVar(&cfg.Path, "path", defaultWebhookServePath, "Webhook endpoint path")
	cmd.Flags().StringVar(&cfg.RoutesPath, "routes-file", defaultWebhookRouteStatePath(), "Path to webhook route definitions JSON")
	cmd.Flags().StringVar(&cfg.TokenStore, "token-store", defaultWebhookTokenStatePath(), "Path to webhook token store JSON")
	cmd.Flags().StringVar(&cfg.EventLogPath, "event-log", defaultWebhookEventLogPath(), "Path to webhook event log JSON")
	cmd.Flags().StringVar(&cfg.AuditLogPath, "audit-log", defaultWebhookAuditLogPath(), "Path to webhook audit JSONL log")
	cmd.Flags().StringVar(&cfg.DispatchQueuePath, "dispatch-queue", defaultWebhookDispatchQueuePath(), "Path to webhook dispatch queue JSON")
	cmd.Flags().StringVar(&cfg.DispatchHistoryPath, "dispatch-history", defaultWebhookDispatchHistoryPath(), "Path to webhook dispatch history JSONL")
	cmd.Flags().StringVar(&cfg.DeadLetterPath, "dead-letter", defaultWebhookDeadLetterPath(), "Path to webhook dead-letter JSONL")
	cmd.Flags().DurationVar(&cfg.TimeSkew, "timestamp-skew", defaultWebhookTimestampSkew, "Allowed timestamp skew window, e.g. 5m")
	cmd.Flags().Int64Var(&cfg.MaxBodyBytes, "max-body-bytes", defaultWebhookMaxBodyBytes, "Max request body bytes")
	cmd.Flags().BoolVar(&cfg.AllowSysDownstream, "allow-sys-downstream", false, "Allow downstream pipeline to execute sys/shell commands")
	cmd.Flags().IntVar(&cfg.DispatchWorkers, "dispatch-workers", defaultWebhookDispatchWorkers, "Number of async dispatch workers")
	cmd.Flags().DurationVar(&cfg.DispatchPollInterval, "dispatch-poll-interval", defaultWebhookDispatchPollInterval, "Async dispatch poll interval")
	cmd.Flags().IntVar(&cfg.DefaultAsyncMaxAttempts, "async-max-attempts", defaultWebhookAsyncMaxAttempts, "Default async max attempts for routes")
	cmd.Flags().DurationVar(&cfg.DefaultAsyncRetryBackoff, "async-retry-backoff", defaultWebhookAsyncRetryBackoff, "Default async retry backoff")
	cmd.Flags().DurationVar(&cfg.DefaultDownstreamTimeout, "downstream-timeout", defaultWebhookDownstreamTimeout, "Default downstream pipeline timeout")
}

func newWebhookServeConfig() *webhookServeConfig {
	return &webhookServeConfig{
		Addr:                     defaultWebhookServeAddr,
		Path:                     defaultWebhookServePath,
		RoutesPath:               defaultWebhookRouteStatePath(),
		TokenStore:               defaultWebhookTokenStatePath(),
		EventLogPath:             defaultWebhookEventLogPath(),
		AuditLogPath:             defaultWebhookAuditLogPath(),
		DispatchQueuePath:        defaultWebhookDispatchQueuePath(),
		DispatchHistoryPath:      defaultWebhookDispatchHistoryPath(),
		DeadLetterPath:           defaultWebhookDeadLetterPath(),
		TimeSkew:                 defaultWebhookTimestampSkew,
		MaxBodyBytes:             defaultWebhookMaxBodyBytes,
		AllowSysDownstream:       false,
		DispatchWorkers:          defaultWebhookDispatchWorkers,
		DispatchPollInterval:     defaultWebhookDispatchPollInterval,
		DefaultAsyncMaxAttempts:  defaultWebhookAsyncMaxAttempts,
		DefaultAsyncRetryBackoff: defaultWebhookAsyncRetryBackoff,
		DefaultDownstreamTimeout: defaultWebhookDownstreamTimeout,
	}
}

func (c *webhookServeConfig) validate() error {
	c.Path = ensureWebhookPath(c.Path)
	c.Addr = strings.TrimSpace(c.Addr)
	c.RuntimePath = strings.TrimSpace(c.RuntimePath)
	c.RoutesPath = strings.TrimSpace(c.RoutesPath)
	c.TokenStore = strings.TrimSpace(c.TokenStore)
	c.EventLogPath = strings.TrimSpace(c.EventLogPath)
	c.AuditLogPath = strings.TrimSpace(c.AuditLogPath)
	c.DispatchQueuePath = strings.TrimSpace(c.DispatchQueuePath)
	c.DispatchHistoryPath = strings.TrimSpace(c.DispatchHistoryPath)
	c.DeadLetterPath = strings.TrimSpace(c.DeadLetterPath)
	if c.Addr == "" {
		return fmt.Errorf("addr is required")
	}
	if c.TimeSkew <= 0 {
		return fmt.Errorf("timestamp-skew must be > 0")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("max-body-bytes must be > 0")
	}
	if c.TokenStore == "" {
		return fmt.Errorf("token-store is required")
	}
	if c.RoutesPath == "" {
		return fmt.Errorf("routes-file is required")
	}
	if c.EventLogPath == "" {
		return fmt.Errorf("event-log is required")
	}
	if c.AuditLogPath == "" {
		return fmt.Errorf("audit-log is required")
	}
	if c.DispatchQueuePath == "" {
		return fmt.Errorf("dispatch-queue is required")
	}
	if c.DispatchHistoryPath == "" {
		return fmt.Errorf("dispatch-history is required")
	}
	if c.DeadLetterPath == "" {
		return fmt.Errorf("dead-letter is required")
	}
	if c.DispatchWorkers <= 0 {
		return fmt.Errorf("dispatch-workers must be > 0")
	}
	if c.DispatchPollInterval <= 0 {
		return fmt.Errorf("dispatch-poll-interval must be > 0")
	}
	if c.DefaultAsyncMaxAttempts <= 0 {
		return fmt.Errorf("async-max-attempts must be > 0")
	}
	if c.DefaultAsyncRetryBackoff <= 0 {
		return fmt.Errorf("async-retry-backoff must be > 0")
	}
	if c.DefaultDownstreamTimeout <= 0 {
		return fmt.Errorf("downstream-timeout must be > 0")
	}
	return nil
}

func startWebhookInBackground(runtimePath string, logPath string, cfg *webhookServeConfig, ownerClaim *webhookStartOwnerClaim) (webhookStartResult, error) {
	runtimePath = strings.TrimSpace(runtimePath)
	if runtimePath == "" {
		return webhookStartResult{}, fmt.Errorf("runtime path is empty")
	}
	logPath = strings.TrimSpace(logPath)
	if logPath == "" {
		return webhookStartResult{}, fmt.Errorf("log path is empty")
	}

	status, _, err := webhookRuntimeStatus(runtimePath)
	if err != nil {
		return webhookStartResult{}, err
	}
	if status == "running" {
		runtime, _, err := readWebhookRuntimeState(runtimePath)
		if err != nil {
			return webhookStartResult{}, err
		}
		return webhookStartResult{
			Mode:    "start",
			Status:  "already_running",
			Message: "webhook server is already running",
			Runtime: runtime,
			LogPath: logPath,
		}, nil
	}
	if status == "stale" {
		_ = removeWebhookRuntimeState(runtimePath)
	}
	if err := ensureWebhookAddrAvailable(cfg.Addr); err != nil {
		if errors.Is(err, errWebhookAddrAlreadyInUse) {
			return webhookStartResult{
				Mode:    "start",
				Status:  "already_running",
				Message: fmt.Sprintf("webhook address %s is already in use; another service may be running on this port", cfg.Addr),
				LogPath: logPath,
			}, nil
		}
		return webhookStartResult{}, err
	}

	spawnCfg := *cfg
	spawnCfg.RuntimePath = runtimePath
	pid, cmdArgs, err := webhookSpawnBackgroundProcess(&spawnCfg, logPath)
	if err != nil {
		return webhookStartResult{}, err
	}

	now := time.Now().Format(time.RFC3339Nano)
	runtime := webhookRuntimeInfo{
		PID:                pid,
		Address:            cfg.Addr,
		Path:               cfg.Path,
		TokenStore:         cfg.TokenStore,
		EventLog:           cfg.EventLogPath,
		RoutesFile:         cfg.RoutesPath,
		AuditLog:           cfg.AuditLogPath,
		DispatchQueue:      cfg.DispatchQueuePath,
		DispatchHistory:    cfg.DispatchHistoryPath,
		DeadLetter:         cfg.DeadLetterPath,
		AllowSysDownstream: cfg.AllowSysDownstream,
		DispatchWorkers:    cfg.DispatchWorkers,
		TimestampSkew:      cfg.TimeSkew.String(),
		MaxBodyBytes:       cfg.MaxBodyBytes,
		StartedAt:          now,
		UpdatedAt:          now,
	}
	if ownerClaim != nil {
		runtime.OwnerSessionID = strings.TrimSpace(ownerClaim.SessionID)
		runtime.OwnerDaemonPID = ownerClaim.DaemonPID
		runtime.OwnerClaimedAt = ownerClaim.ClaimedAt.Format(time.RFC3339Nano)
		runtime.OwnerStartToken = strings.TrimSpace(ownerClaim.StartToken)
	}
	if err := webhookVerifyBackgroundStart(cfg, pid); err != nil {
		_ = killProcessByPID(pid)
		return webhookStartResult{}, err
	}
	if err := writeWebhookRuntimeState(runtimePath, runtime); err != nil {
		_ = killProcessByPID(pid)
		return webhookStartResult{}, err
	}

	return webhookStartResult{
		Mode:      "start",
		Status:    "started",
		Runtime:   &runtime,
		LogPath:   logPath,
		StartArgs: cmdArgs,
	}, nil
}

var errWebhookAddrAlreadyInUse = errors.New("webhook address already in use")

func ensureWebhookAddrAvailable(addr string) error {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return fmt.Errorf("webhook addr is empty")
	}
	listener, err := net.Listen("tcp", trimmed)
	if err != nil {
		if isWebhookAddrAlreadyInUseError(err) {
			return errWebhookAddrAlreadyInUse
		}
		return fmt.Errorf("check webhook addr %q failed: %w", trimmed, err)
	}
	_ = listener.Close()
	return nil
}

func isWebhookAddrAlreadyInUseError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

func verifyWebhookBackgroundStart(cfg *webhookServeConfig, pid int) error {
	deadline := time.Now().Add(defaultWebhookStartTimeout)
	var lastAdminErr error
	for {
		running, err := isProcessRunning(pid)
		if err != nil {
			return fmt.Errorf("check webhook process status failed: %w", err)
		}
		if !running {
			return fmt.Errorf("webhook process exited before becoming ready")
		}

		if _, err := fetchWebhookAdminStatus(cfg.Addr, defaultWebhookManagementHTTPTimeout); err == nil {
			return nil
		} else {
			lastAdminErr = err
		}

		if time.Now().After(deadline) {
			break
		}
		time.Sleep(defaultWebhookStartPoll)
	}
	if lastAdminErr != nil {
		return fmt.Errorf("webhook management endpoint not ready: %w", lastAdminErr)
	}
	return fmt.Errorf("webhook management endpoint not ready")
}

func spawnWebhookBackgroundProcess(cfg *webhookServeConfig, logPath string) (int, []string, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, nil, err
	}

	execPath, err := os.Executable()
	if err != nil {
		_ = logFile.Close()
		return 0, nil, err
	}
	serveArgs := buildWebhookServeArgs(cfg)
	cmdArgs := append([]string{"webhook", "serve"}, serveArgs...)
	child := exec.Command(execPath, cmdArgs...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Stdin = nil
	childEnv := append(os.Environ(), webhookServeInternalEnv+"=1")
	if cfg != nil && strings.TrimSpace(cfg.RuntimePath) != "" {
		childEnv = append(childEnv, webhookServeRuntimePathEnv+"="+strings.TrimSpace(cfg.RuntimePath))
	}
	child.Env = childEnv
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return 0, nil, fmt.Errorf("start webhook background process failed: %w", err)
	}
	_ = logFile.Close()
	return child.Process.Pid, cmdArgs, nil
}

func killProcessByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil && !isProcessNotFoundError(err) && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func buildWebhookServeArgs(cfg *webhookServeConfig) []string {
	args := make([]string, 0, 32)
	args = append(args, "--addr", cfg.Addr)
	args = append(args, "--path", cfg.Path)
	args = append(args, "--routes-file", cfg.RoutesPath)
	args = append(args, "--token-store", cfg.TokenStore)
	args = append(args, "--event-log", cfg.EventLogPath)
	args = append(args, "--audit-log", cfg.AuditLogPath)
	args = append(args, "--dispatch-queue", cfg.DispatchQueuePath)
	args = append(args, "--dispatch-history", cfg.DispatchHistoryPath)
	args = append(args, "--dead-letter", cfg.DeadLetterPath)
	args = append(args, "--timestamp-skew", cfg.TimeSkew.String())
	args = append(args, "--max-body-bytes", strconv.FormatInt(cfg.MaxBodyBytes, 10))
	args = append(args, "--dispatch-workers", strconv.Itoa(cfg.DispatchWorkers))
	args = append(args, "--dispatch-poll-interval", cfg.DispatchPollInterval.String())
	args = append(args, "--async-max-attempts", strconv.Itoa(cfg.DefaultAsyncMaxAttempts))
	args = append(args, "--async-retry-backoff", cfg.DefaultAsyncRetryBackoff.String())
	args = append(args, "--downstream-timeout", cfg.DefaultDownstreamTimeout.String())
	if cfg.AllowSysDownstream {
		args = append(args, "--allow-sys-downstream")
	}
	return args
}

func webhookStatus(runtimePath string) (webhookStatusResult, error) {
	status, runtime, err := webhookRuntimeStatus(runtimePath)
	if err != nil {
		return webhookStatusResult{}, err
	}
	result := webhookStatusResult{
		Mode:     "status",
		Status:   status,
		Running:  status == "running",
		Runtime:  runtime,
		Dispatch: webhookDispatchStats{},
	}
	if runtime == nil {
		return result, nil
	}

	routesPath := strings.TrimSpace(runtime.RoutesFile)
	if routesPath == "" {
		routesPath = defaultWebhookRouteStatePath()
	}
	records, routeErr := loadWebhookRouteRecords(routesPath)
	if routeErr != nil {
		return result, routeErr
	}
	routeRecords := make([]webhookRouteRecord, 0, len(records))
	for _, record := range records {
		routeRecords = append(routeRecords, record)
	}
	if len(routeRecords) == 0 {
		routeRecords = append(routeRecords, webhookRouteRecord{
			ID:           defaultWebhookLegacyRouteID,
			Path:         ensureWebhookPath(runtime.Path),
			Mode:         webhookRouteModeSync,
			Pipeline:     "",
			Enabled:      true,
			MaxAttempts:  defaultWebhookAsyncMaxAttempts,
			RetryBackoff: defaultWebhookAsyncRetryBackoff.String(),
			Timeout:      defaultWebhookDownstreamTimeout.String(),
		})
	}
	routes, routeResolveErr := resolveWebhookRoutes(routeRecords, newWebhookServeConfig())
	if routeResolveErr != nil {
		return result, routeResolveErr
	}
	result.RouteCount = len(routes)
	result.Routes = summarizeWebhookRoutes(routes)

	queuePath := strings.TrimSpace(runtime.DispatchQueue)
	if queuePath == "" {
		queuePath = defaultWebhookDispatchQueuePath()
	}
	deadLetterPath := strings.TrimSpace(runtime.DeadLetter)
	if deadLetterPath == "" {
		deadLetterPath = defaultWebhookDeadLetterPath()
	}
	stats, statErr := readWebhookDispatchStats(queuePath, deadLetterPath)
	if statErr != nil {
		return result, statErr
	}
	result.Dispatch = stats

	if status == "running" && runtime != nil {
		adminStatus, adminErr := fetchWebhookAdminStatus(runtime.Address, defaultWebhookManagementHTTPTimeout)
		if adminErr == nil {
			metrics := adminStatus.Metrics
			queueDepth := adminStatus.LogQueueDepth
			result.Metrics = &metrics
			result.LogQueue = &queueDepth
			result.Dispatch = adminStatus.Dispatch
			if adminStatus.RouteCount > 0 {
				result.RouteCount = adminStatus.RouteCount
			}
			if len(adminStatus.Routes) > 0 {
				result.Routes = adminStatus.Routes
			}
		}
	}
	return result, nil
}

func webhookRuntimeStatus(runtimePath string) (string, *webhookRuntimeInfo, error) {
	runtime, exists, err := readWebhookRuntimeState(runtimePath)
	if err != nil {
		return "", nil, err
	}
	if !exists || runtime == nil {
		return "stopped", nil, nil
	}
	if runtime.PID <= 0 {
		return "stale", runtime, nil
	}
	running, err := isProcessRunning(runtime.PID)
	if err != nil {
		return "", nil, err
	}
	if running {
		return "running", runtime, nil
	}
	return "stale", runtime, nil
}

func stopWebhook(runtimePath string, timeout time.Duration) (webhookStopResult, error) {
	runtime, exists, err := readWebhookRuntimeState(runtimePath)
	if err != nil {
		return webhookStopResult{}, err
	}
	if !exists || runtime == nil {
		return webhookStopResult{Mode: "stop", Status: "not_running", Message: "runtime state not found"}, nil
	}

	if runtime.PID <= 0 {
		_ = removeWebhookRuntimeState(runtimePath)
		return webhookStopResult{Mode: "stop", Status: "stale_removed", Message: "invalid runtime pid removed"}, nil
	}

	running, err := isProcessRunning(runtime.PID)
	if err != nil {
		return webhookStopResult{}, err
	}
	if !running {
		_ = removeWebhookRuntimeState(runtimePath)
		return webhookStopResult{Mode: "stop", Status: "stale_removed", PID: runtime.PID, Message: "stale runtime removed"}, nil
	}

	if err := terminateProcess(runtime.PID, timeout); err != nil {
		return webhookStopResult{}, err
	}
	if err := removeWebhookRuntimeState(runtimePath); err != nil {
		return webhookStopResult{}, err
	}
	return webhookStopResult{Mode: "stop", Status: "stopped", PID: runtime.PID}, nil
}

func executeWebhookKillPort(address string, timeout time.Duration, runtimePath string, dryRun bool) (webhookKillPortResult, error) {
	trimmedAddr := strings.TrimSpace(address)
	port, err := parseWebhookListenPort(trimmedAddr)
	result := webhookKillPortResult{
		Mode:    "kill-port",
		Address: trimmedAddr,
		Port:    port,
	}
	if err != nil {
		result.Status = "partial_failed"
		result.Message = err.Error()
		return result, err
	}

	pids, err := webhookKillPortListListeningPIDs(port)
	if err != nil {
		result.Status = "partial_failed"
		result.Message = err.Error()
		return result, err
	}
	result.MatchedPIDs = append(result.MatchedPIDs, pids...)
	if len(pids) == 0 {
		result.Status = "not_running"
		result.Message = fmt.Sprintf("no listening process found on port %d", port)
		return result, nil
	}

	if dryRun {
		result.Status = "dry_run"
		result.Message = fmt.Sprintf("dry-run matched pids on port %d: %v", port, pids)
		return result, nil
	}

	failed := make([]webhookKillPortFailure, 0)
	for _, pid := range pids {
		if err := webhookKillPortTerminateProcess(pid, timeout); err != nil {
			failed = append(failed, webhookKillPortFailure{PID: pid, Error: err.Error()})
			continue
		}
		result.StoppedPIDs = append(result.StoppedPIDs, pid)
	}
	result.Failed = failed

	runtimeRemoved, runtimeErr := cleanupWebhookRuntimeStateByStoppedPIDs(runtimePath, result.StoppedPIDs)
	if runtimeErr != nil {
		result.Message = fmt.Sprintf("stopped=%d failed=%d; runtime cleanup skipped: %v", len(result.StoppedPIDs), len(result.Failed), runtimeErr)
	} else if runtimeRemoved {
		result.Message = fmt.Sprintf("stopped=%d failed=%d; runtime state cleaned", len(result.StoppedPIDs), len(result.Failed))
	} else {
		result.Message = fmt.Sprintf("stopped=%d failed=%d", len(result.StoppedPIDs), len(result.Failed))
	}

	if len(result.Failed) > 0 {
		result.Status = "partial_failed"
		return result, fmt.Errorf("port %d stop completed with %d failure(s)", port, len(result.Failed))
	}
	result.Status = "stopped"
	return result, nil
}

func terminateProcess(pid int, timeout time.Duration) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if !isProcessNotFoundError(err) {
			return err
		}
	}

	waitDuration := timeout
	if waitDuration <= 0 {
		waitDuration = 500 * time.Millisecond
	}
	if waitDuration > 2*time.Second {
		waitDuration = 2 * time.Second
	}
	time.Sleep(waitDuration)

	running, runErr := isProcessRunning(pid)
	if runErr != nil {
		return runErr
	}
	if !running {
		return nil
	}

	if err := process.Signal(syscall.SIGKILL); err != nil && !isProcessNotFoundError(err) && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func isProcessRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if isProcessNotFoundError(err) {
		return false, nil
	}
	if isPermissionError(err) {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) {
		return false, nil
	}
	return false, err
}

func parseWebhookListenPort(addr string) (int, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return 0, fmt.Errorf("addr is required")
	}

	portText := trimmed
	if strings.HasPrefix(trimmed, ":") {
		portText = strings.TrimPrefix(trimmed, ":")
	} else {
		if strings.Contains(trimmed, ":") {
			_, splitPort, err := net.SplitHostPort(trimmed)
			if err != nil {
				return 0, fmt.Errorf("invalid addr %q: %w", trimmed, err)
			}
			portText = splitPort
		}
	}

	port, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil {
		return 0, fmt.Errorf("invalid addr %q: parse port failed", trimmed)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("invalid addr %q: port must be between 1 and 65535", trimmed)
	}
	return port, nil
}

func listListeningPIDsByPort(port int) ([]int, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	query := fmt.Sprintf("-tiTCP:%d", port)
	cmd := exec.Command("lsof", "-nP", query, "-sTCP:LISTEN")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("lsof command not found; please install lsof")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return []int{}, nil
		}
		errText := strings.TrimSpace(string(raw))
		if errText == "" {
			return nil, fmt.Errorf("lsof failed: %w", err)
		}
		return nil, fmt.Errorf("lsof failed: %s", errText)
	}

	seen := make(map[int]struct{})
	pids := make([]int, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		pid, parseErr := strconv.Atoi(trimmed)
		if parseErr != nil || pid <= 0 {
			continue
		}
		if _, exists := seen[pid]; exists {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func cleanupWebhookRuntimeStateByStoppedPIDs(runtimePath string, stoppedPIDs []int) (bool, error) {
	trimmedPath := strings.TrimSpace(runtimePath)
	if trimmedPath == "" || len(stoppedPIDs) == 0 {
		return false, nil
	}
	runtime, exists, err := readWebhookRuntimeState(trimmedPath)
	if err != nil {
		return false, err
	}
	if !exists || runtime == nil || runtime.PID <= 0 {
		return false, nil
	}
	for _, pid := range stoppedPIDs {
		if pid == runtime.PID {
			if err := removeWebhookRuntimeState(trimmedPath); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func isProcessNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.ESRCH
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such process")
}

func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EPERM
	}
	return strings.Contains(strings.ToLower(err.Error()), "operation not permitted")
}

func defaultWebhookTokenStatePath() string {
	return filepath.Join(daemonConfigDir, webhookTokenStateFile)
}

func defaultWebhookRuntimeStatePath() string {
	return filepath.Join(daemonDataDir, webhookRuntimeStateFile)
}

func defaultWebhookLegacyRuntimeStatePath() string {
	return filepath.Join(daemonConfigDir, webhookRuntimeStateFile)
}

func defaultWebhookEventLogPath() string {
	return filepath.Join(daemonDataDir, webhookEventLogFile)
}

func defaultWebhookServerLogPath() string {
	return filepath.Join(commandLogDir, "webhook_server.log")
}

func normalizeThirdPartyID(value string) string {
	return strings.TrimSpace(value)
}

func ensureWebhookPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return defaultWebhookServePath
	}
	if strings.HasPrefix(trimmed, "/") {
		return trimmed
	}
	return "/" + trimmed
}

func resolveWebhookTimestamp(raw string, now time.Time) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return strconv.FormatInt(now.Unix(), 10), nil
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp %q: %w", trimmed, err)
	}
	return strconv.FormatInt(parsed, 10), nil
}

func resolveWebhookSignBody(dataText string, dataFile string) ([]byte, error) {
	trimmedText := strings.TrimSpace(dataText)
	trimmedFile := strings.TrimSpace(dataFile)
	if trimmedText == "" && trimmedFile == "" {
		return nil, fmt.Errorf("either --data or --data-file is required")
	}
	if trimmedText != "" && trimmedFile != "" {
		return nil, fmt.Errorf("--data and --data-file cannot be used together")
	}
	if trimmedFile != "" {
		data, err := os.ReadFile(trimmedFile)
		if err != nil {
			return nil, fmt.Errorf("read data-file %s: %w", trimmedFile, err)
		}
		return data, nil
	}
	return []byte(dataText), nil
}

func buildWebhookSendSignResult(thirdPartyID string, rawToken string, timestampText string, body []byte, targetURL string) (webhookSignResult, error) {
	trimmedURL := strings.TrimSpace(targetURL)
	if trimmedURL == "" {
		return webhookSignResult{}, fmt.Errorf("url is required")
	}
	request, err := http.NewRequest(http.MethodPost, trimmedURL, nil)
	if err != nil {
		return webhookSignResult{}, fmt.Errorf("invalid url %q: %w", trimmedURL, err)
	}
	if strings.TrimSpace(request.URL.Scheme) == "" || strings.TrimSpace(request.URL.Host) == "" {
		return webhookSignResult{}, fmt.Errorf("url must include scheme and host")
	}
	routePath := request.URL.Path
	if strings.TrimSpace(routePath) == "" {
		routePath = "/"
	}
	return buildWebhookSignResult(thirdPartyID, rawToken, timestampText, body, ensureWebhookPath(routePath), trimmedURL), nil
}

func buildWebhookSignResult(thirdPartyID string, rawToken string, timestampText string, body []byte, path string, targetURL string) webhookSignResult {
	signature := computeWebhookSignature(thirdPartyID, timestampText, rawToken, body)
	return webhookSignResult{
		Method:    webhookSignMethod,
		Path:      path,
		URL:       strings.TrimSpace(targetURL),
		Timestamp: timestampText,
		Headers: map[string]string{
			webhookHeaderThirdPartyID: thirdPartyID,
			webhookHeaderTimestamp:    timestampText,
			webhookHeaderSignature:    signature,
		},
		Body:      string(body),
		Signature: signature,
	}
}

func executeWebhookSendRequest(signResult webhookSignResult, timeout time.Duration) (webhookSendResult, error) {
	result := webhookSendResult{
		TargetURL: signResult.URL,
		RoutePath: ensureWebhookPath(signResult.Path),
		RequestHeaders: map[string]string{
			webhookHeaderThirdPartyID: signResult.Headers[webhookHeaderThirdPartyID],
			webhookHeaderTimestamp:    signResult.Headers[webhookHeaderTimestamp],
			webhookHeaderSignature:    signResult.Headers[webhookHeaderSignature],
			"Content-Type":            "application/json",
		},
	}
	if timeout <= 0 {
		return result, fmt.Errorf("timeout must be > 0")
	}
	if strings.TrimSpace(signResult.URL) == "" {
		return result, fmt.Errorf("target url is required")
	}

	startedAt := time.Now()
	request, err := http.NewRequest(http.MethodPost, signResult.URL, strings.NewReader(signResult.Body))
	if err != nil {
		return result, fmt.Errorf("build webhook request failed: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range signResult.Headers {
		request.Header.Set(key, value)
	}

	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	result.DurationMs = time.Since(startedAt).Milliseconds()
	if err != nil {
		return result, fmt.Errorf("send webhook request failed: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return result, fmt.Errorf("read webhook response failed: %w", err)
	}
	result.HTTPStatus = response.StatusCode
	result.ResponseBody = string(responseBody)
	applyWebhookResponseInsightsToSendResult(&result)
	return result, nil
}

type webhookResponseInsights struct {
	ResponseJSON           any
	ResponseEventID        string
	ResponseThirdPartyID   string
	ResponseTokenValid     *bool
	ResponseTimestampValid *bool
	ResponseJSONValid      *bool
	ResponseMetaError      string
}

type webhookRequestInsights struct {
	RequestJSON      any
	RequestJSONValid *bool
}

func buildWebhookSendDisplayResult(result webhookSendResult) map[string]any {
	display := map[string]any{
		"target_url":      result.TargetURL,
		"route_path":      result.RoutePath,
		"request_headers": result.RequestHeaders,
		"http_status":     result.HTTPStatus,
		"duration_ms":     result.DurationMs,
	}

	if result.ResponseJSON != nil {
		display["response_json"] = result.ResponseJSON
	} else if strings.TrimSpace(result.ResponseBody) != "" {
		display["response_body"] = result.ResponseBody
	}
	if strings.TrimSpace(result.ResponseEventID) != "" {
		display["response_event_id"] = result.ResponseEventID
	}
	if strings.TrimSpace(result.ResponseThirdPartyID) != "" {
		display["response_third_party_id"] = result.ResponseThirdPartyID
	}
	if result.ResponseTokenValid != nil {
		display["response_token_valid"] = *result.ResponseTokenValid
	}
	if result.ResponseTimestampValid != nil {
		display["response_timestamp_valid"] = *result.ResponseTimestampValid
	}
	if result.ResponseJSONValid != nil {
		display["response_json_valid"] = *result.ResponseJSONValid
	}
	if strings.TrimSpace(result.ResponseMetaError) != "" {
		display["response_meta_error"] = result.ResponseMetaError
	}
	return display
}

func applyWebhookResponseInsightsToSendResult(result *webhookSendResult) {
	if result == nil {
		return
	}
	insights := parseWebhookResponseInsights(result.ResponseBody)
	result.ResponseJSON = insights.ResponseJSON
	result.ResponseEventID = insights.ResponseEventID
	result.ResponseThirdPartyID = insights.ResponseThirdPartyID
	result.ResponseTokenValid = insights.ResponseTokenValid
	result.ResponseTimestampValid = insights.ResponseTimestampValid
	result.ResponseJSONValid = insights.ResponseJSONValid
	result.ResponseMetaError = insights.ResponseMetaError
}

func applyWebhookResponseInsightsToAuditEntry(entry *webhookAuditLogEntry) {
	if entry == nil {
		return
	}
	insights := parseWebhookResponseInsights(entry.ResponseBody)
	entry.ResponseJSON = insights.ResponseJSON
	entry.ResponseEventID = insights.ResponseEventID
	entry.ResponseThirdPartyID = insights.ResponseThirdPartyID
	entry.ResponseTokenValid = insights.ResponseTokenValid
	entry.ResponseTimestampValid = insights.ResponseTimestampValid
	entry.ResponseJSONValid = insights.ResponseJSONValid
	entry.ResponseMetaError = insights.ResponseMetaError
}

func applyWebhookRequestInsightsToAuditEntry(entry *webhookAuditLogEntry) {
	if entry == nil {
		return
	}
	insights := parseWebhookRequestInsights(entry.RequestBody)
	entry.RequestJSON = insights.RequestJSON
	entry.RequestJSONValid = insights.RequestJSONValid
}

func parseWebhookResponseInsights(raw string) webhookResponseInsights {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return webhookResponseInsights{}
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return webhookResponseInsights{}
	}

	insights := webhookResponseInsights{
		ResponseJSON: decoded,
	}

	root, ok := decoded.(map[string]any)
	if !ok {
		return insights
	}
	meta, ok := root["meta"].(map[string]any)
	if !ok {
		return insights
	}

	if eventID, ok := meta["event_id"].(string); ok {
		insights.ResponseEventID = strings.TrimSpace(eventID)
	}
	if thirdPartyID, ok := meta["third_party_id"].(string); ok {
		insights.ResponseThirdPartyID = strings.TrimSpace(thirdPartyID)
	}
	if metaError, ok := meta["error"].(string); ok {
		insights.ResponseMetaError = strings.TrimSpace(metaError)
	}
	if validation, ok := meta["validation"].(map[string]any); ok {
		if value, ok := validation["token_valid"].(bool); ok {
			insights.ResponseTokenValid = webhookBoolPointer(value)
		}
		if value, ok := validation["timestamp_valid"].(bool); ok {
			insights.ResponseTimestampValid = webhookBoolPointer(value)
		}
		if value, ok := validation["json_valid"].(bool); ok {
			insights.ResponseJSONValid = webhookBoolPointer(value)
		}
	}

	return insights
}

func parseWebhookRequestInsights(raw string) webhookRequestInsights {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return webhookRequestInsights{}
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return webhookRequestInsights{
			RequestJSONValid: webhookBoolPointer(false),
		}
	}

	return webhookRequestInsights{
		RequestJSON:      decoded,
		RequestJSONValid: webhookBoolPointer(true),
	}
}

func webhookBoolPointer(value bool) *bool {
	next := value
	return &next
}

func computeWebhookSignature(thirdPartyID string, timestampText string, rawToken string, body []byte) string {
	var payload bytes.Buffer
	payload.WriteString(thirdPartyID)
	payload.WriteByte('\n')
	payload.WriteString(timestampText)
	payload.WriteByte('\n')
	payload.WriteString(rawToken)
	payload.WriteByte('\n')
	payload.Write(body)

	sum := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(sum[:])
}

func newWebhookEventHandler(opts webhookServeOptions) http.Handler {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	timestampSkew := opts.TimestampSkew
	if timestampSkew <= 0 {
		timestampSkew = defaultWebhookTimestampSkew
	}
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultWebhookMaxBodyBytes
	}
	path := ensureWebhookPath(opts.Path)
	route := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       strings.TrimSpace(opts.RouteID),
			Path:     path,
			Mode:     normalizeWebhookRouteMode(opts.RouteMode),
			Pipeline: strings.TrimSpace(opts.RoutePipeline),
			Enabled:  true,
		},
		Mode:         normalizeWebhookRouteMode(opts.RouteMode),
		MaxAttempts:  defaultWebhookAsyncMaxAttempts,
		RetryBackoff: defaultWebhookAsyncRetryBackoff,
		Timeout:      defaultWebhookDownstreamTimeout,
	}
	if route.Record.ID == "" {
		route.Record.ID = defaultWebhookLegacyRouteID
	}
	if route.Mode == "" {
		route.Mode = webhookRouteModeSync
	}
	if opts.ResolvedRoute != nil {
		route = *opts.ResolvedRoute
	}
	if route.Timeout <= 0 {
		if opts.DownstreamTimeout > 0 {
			route.Timeout = opts.DownstreamTimeout
		} else {
			route.Timeout = defaultWebhookDownstreamTimeout
		}
	}
	var routeParseErr error
	if len(route.Commands) == 0 && strings.TrimSpace(route.Record.Pipeline) != "" {
		route.Commands, routeParseErr = parsePipelineCommandLine(route.Record.Pipeline)
	}
	tokenFinder := opts.TokenFinder
	if tokenFinder == nil {
		tokenStorePath := strings.TrimSpace(opts.TokenStorePath)
		tokenFinder = func(thirdPartyID string) (webhookTokenRecord, bool, error) {
			return findWebhookToken(tokenStorePath, thirdPartyID)
		}
	}
	eventLogAppender := opts.EventLogAppender
	if eventLogAppender == nil {
		eventPath := strings.TrimSpace(opts.EventLogPath)
		eventLogAppender = func(event webhookEventEnvelope) error {
			return appendWebhookEventLog(eventPath, event)
		}
	}
	auditLogAppender := opts.AuditLogAppender
	if auditLogAppender == nil {
		auditPath := strings.TrimSpace(opts.AuditLogPath)
		auditLogAppender = func(entry webhookAuditLogEntry) error {
			return appendWebhookAuditLog(auditPath, entry)
		}
	}
	metrics := opts.Metrics

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAt := nowFn()
		writer := &webhookResponseCapture{ResponseWriter: w}
		audit := webhookAuditLogEntry{
			Timestamp:      receivedAt.Format(time.RFC3339Nano),
			RouteID:        route.Record.ID,
			Method:         r.Method,
			Path:           r.URL.Path,
			RemoteAddr:     r.RemoteAddr,
			RequestHeaders: copyWebhookHeadersForAudit(r.Header),
			RequestBody:    "",
			ReceivedAt:     receivedAt.Format(time.RFC3339Nano),
		}
		defer func() {
			if writer.status == 0 {
				writer.status = http.StatusOK
			}
			audit.ResponseStatus = writer.status
			audit.ResponseBody = writer.body.String()
			applyWebhookResponseInsightsToAuditEntry(&audit)
			respondedAt := nowFn()
			duration := respondedAt.Sub(receivedAt)
			if duration < 0 {
				duration = 0
			}
			audit.RespondedAt = respondedAt.Format(time.RFC3339Nano)
			audit.DurationMs = duration.Milliseconds()
			if metrics != nil {
				metrics.Observe(writer.status, duration)
			}
			_ = auditLogAppender(audit)
		}()

		event := webhookEventEnvelope{
			Meta: webhookEventMeta{
				EventID:    newWebhookEventID(receivedAt),
				ReceivedAt: receivedAt.Format(time.RFC3339Nano),
			},
			Data: nil,
		}
		audit.EventID = event.Meta.EventID
		audit.Meta = &event.Meta
		dispatch := &webhookDispatchAuditInfo{Mode: route.Mode, Status: "skipped"}

		if r.Method != http.MethodPost {
			event.Meta.Error = "method not allowed"
			dispatch.Status = "method_not_allowed"
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusMethodNotAllowed, event)
			return
		}
		if routeParseErr != nil {
			event.Meta.Error = fmt.Sprintf("route pipeline parse failed: %v", routeParseErr)
			dispatch.Status = "route_error"
			dispatch.Message = routeParseErr.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusInternalServerError, event)
			return
		}

		body, err := readWebhookBody(r.Body, maxBodyBytes)
		if err != nil {
			event.Meta.Error = err.Error()
			status := http.StatusBadRequest
			if strings.Contains(strings.ToLower(err.Error()), "too large") {
				status = http.StatusRequestEntityTooLarge
			}
			dispatch.Status = "read_body_failed"
			dispatch.Message = err.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, status, event)
			return
		}
		audit.RequestBody = string(body)
		applyWebhookRequestInsightsToAuditEntry(&audit)

		thirdPartyID := normalizeThirdPartyID(r.Header.Get(webhookHeaderThirdPartyID))
		timestampText := strings.TrimSpace(r.Header.Get(webhookHeaderTimestamp))
		signatureText := strings.ToLower(strings.TrimSpace(r.Header.Get(webhookHeaderSignature)))
		event.Meta.ThirdPartyID = thirdPartyID

		if thirdPartyID == "" || timestampText == "" || signatureText == "" {
			event.Meta.Error = "missing required headers"
			dispatch.Status = "header_validation_failed"
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusUnauthorized, event)
			return
		}

		timestamp, err := parseWebhookTimestamp(timestampText)
		if err != nil {
			event.Meta.Error = err.Error()
			dispatch.Status = "timestamp_parse_failed"
			dispatch.Message = err.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusUnauthorized, event)
			return
		}
		event.Meta.Validation.TimestampValid = webhookTimestampWithinWindow(receivedAt, timestamp, timestampSkew)
		if !event.Meta.Validation.TimestampValid {
			event.Meta.Error = "timestamp is outside allowed window"
			dispatch.Status = "timestamp_outside_window"
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusUnauthorized, event)
			return
		}

		record, found, err := tokenFinder(thirdPartyID)
		if err != nil {
			event.Meta.Error = err.Error()
			dispatch.Status = "token_lookup_failed"
			dispatch.Message = err.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusInternalServerError, event)
			return
		}
		if !found {
			event.Meta.Error = "token not found for third-party-id"
			dispatch.Status = "token_not_found"
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusUnauthorized, event)
			return
		}

		expectedSignature := computeWebhookSignature(thirdPartyID, timestampText, record.Token, body)
		event.Meta.Validation.TokenValid = hmac.Equal([]byte(signatureText), []byte(expectedSignature))
		if !event.Meta.Validation.TokenValid {
			event.Meta.Error = "signature verification failed"
			dispatch.Status = "signature_failed"
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusUnauthorized, event)
			return
		}

		data, err := parseWebhookJSONObject(body)
		if err != nil {
			event.Meta.Validation.JSONValid = false
			event.Meta.Error = err.Error()
			dispatch.Status = "json_invalid"
			dispatch.Message = err.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusBadRequest, event)
			return
		}
		event.Meta.Validation.JSONValid = true
		event.Data = data
		event.RawBody = append([]byte(nil), body...)

		if err := eventLogAppender(event); err != nil {
			event.Meta.Error = fmt.Sprintf("append event log failed: %v", err)
			dispatch.Status = "event_log_failed"
			dispatch.Message = err.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusInternalServerError, event)
			return
		}

		if len(route.Commands) == 0 {
			dispatch.Status = "success"
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusOK, event)
			return
		}

		if route.Mode == webhookRouteModeAsync {
			if opts.Dispatcher == nil {
				event.Meta.Error = "dispatch manager is not initialized"
				dispatch.Status = "enqueue_failed"
				dispatch.Message = event.Meta.Error
				audit.Dispatch = dispatch
				writeWebhookResponse(writer, http.StatusInternalServerError, event)
				return
			}
			job, enqueueErr := opts.Dispatcher.Enqueue(route, event)
			if enqueueErr != nil {
				event.Meta.Error = fmt.Sprintf("enqueue webhook dispatch failed: %v", enqueueErr)
				dispatch.Status = "enqueue_failed"
				dispatch.Message = enqueueErr.Error()
				audit.Dispatch = dispatch
				writeWebhookResponse(writer, http.StatusInternalServerError, event)
				return
			}
			dispatch.Status = "enqueued"
			dispatch.JobID = job.JobID
			dispatch.Attempt = job.Attempt
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusAccepted, event)
			return
		}

		downstreamCtx := r.Context()
		var cancel context.CancelFunc
		if route.Timeout > 0 {
			downstreamCtx, cancel = context.WithTimeout(downstreamCtx, route.Timeout)
		}
		if cancel != nil {
			defer cancel()
		}
		downstreamResult, runErr := runWebhookDownstreamPipeline(downstreamCtx, route, event, opts.AllowSysDownstream)
		if runErr != nil {
			event.Meta.Error = fmt.Sprintf("downstream pipeline failed: %v", runErr)
			dispatch.Status = "downstream_failed"
			dispatch.Message = runErr.Error()
			audit.Dispatch = dispatch
			writeWebhookResponse(writer, http.StatusInternalServerError, event)
			return
		}
		if downstreamResult != nil {
			dispatch.Result = downstreamResult
		}

		dispatch.Status = "success"
		audit.Dispatch = dispatch
		writeWebhookResponse(writer, http.StatusOK, event)
	})
}

type webhookResponseCapture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *webhookResponseCapture) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *webhookResponseCapture) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_, _ = w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func parseWebhookTimestamp(timestampText string) (time.Time, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(timestampText), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp header %q", timestampText)
	}
	return time.Unix(parsed, 0).UTC(), nil
}

func webhookTimestampWithinWindow(now time.Time, timestamp time.Time, window time.Duration) bool {
	if window < 0 {
		return false
	}
	delta := now.Sub(timestamp)
	if delta < 0 {
		delta = -delta
	}
	return delta <= window
}

func readWebhookBody(reader io.Reader, maxBodyBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("request body is empty")
	}
	limited := io.LimitReader(reader, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read request body failed: %w", err)
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, fmt.Errorf("request body too large")
	}
	return body, nil
}

func parseWebhookJSONObject(body []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("request body is empty")
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("request body is not valid json object: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("request body must be a json object")
	}
	return data, nil
}

func writeWebhookResponse(w http.ResponseWriter, status int, payload webhookEventEnvelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, err := marshalJSONIndentNoHTMLEscape(payload, "", defaultWebhookResponseIndent)
	if err != nil {
		_, _ = io.WriteString(w, `{"meta":{"error":"encode response failed"},"data":null}`)
		return
	}
	_, _ = w.Write(append(data, '\n'))
}

func appendWebhookEventLog(path string, event webhookEventEnvelope) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("event log path is empty")
	}
	encoded, err := marshalJSONNoHTMLEscape(event)
	if err != nil {
		return err
	}
	return appendWebhookEventLogLines(path, [][]byte{append(encoded, '\n')})
}

func findWebhookAuditEntryForPipelineDebug(auditPath string, eventID string, routeID string) (webhookAuditLogEntry, error) {
	trimmedPath := strings.TrimSpace(auditPath)
	if trimmedPath == "" {
		return webhookAuditLogEntry{}, fmt.Errorf("audit-log is required")
	}
	targetEventID := strings.TrimSpace(eventID)
	targetRouteID := strings.TrimSpace(routeID)

	file, err := os.Open(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return webhookAuditLogEntry{}, fmt.Errorf("audit log not found: %s", trimmedPath)
		}
		return webhookAuditLogEntry{}, fmt.Errorf("open audit log failed: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	const maxAuditLineBytes = 10 * 1024 * 1024
	buffer := make([]byte, 0, 256*1024)
	scanner.Buffer(buffer, maxAuditLineBytes)

	found := false
	var latest webhookAuditLogEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var entry webhookAuditLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return webhookAuditLogEntry{}, fmt.Errorf("decode audit log line failed: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(entry.Method), http.MethodPost) {
			continue
		}
		if targetEventID != "" && strings.TrimSpace(entry.EventID) != targetEventID {
			continue
		}
		if targetRouteID != "" && strings.TrimSpace(entry.RouteID) != targetRouteID {
			continue
		}
		found = true
		latest = entry
	}
	if err := scanner.Err(); err != nil {
		return webhookAuditLogEntry{}, fmt.Errorf("scan audit log failed: %w", err)
	}
	if !found {
		if targetEventID != "" {
			return webhookAuditLogEntry{}, fmt.Errorf("no matching webhook POST entry found for event-id %q", targetEventID)
		}
		if targetRouteID != "" {
			return webhookAuditLogEntry{}, fmt.Errorf("no matching webhook POST entry found for route-id %q", targetRouteID)
		}
		return webhookAuditLogEntry{}, fmt.Errorf("no webhook POST entry found in audit log")
	}
	return latest, nil
}

func buildWebhookPipelineDebugResult(entry webhookAuditLogEntry) webhookDebugPipelineResult {
	routeMode := webhookRouteModeSync
	if entry.Dispatch != nil && strings.TrimSpace(entry.Dispatch.Mode) != "" {
		routeMode = entry.Dispatch.Mode
	}

	meta := webhookEventMeta{
		EventID:    strings.TrimSpace(entry.EventID),
		ReceivedAt: strings.TrimSpace(entry.ReceivedAt),
	}
	if entry.Meta != nil {
		meta = *entry.Meta
	}

	var (
		data           any
		dataParseError string
	)
	requestBody := entry.RequestBody
	if entry.RequestJSON != nil {
		data = entry.RequestJSON
	} else {
		if parsed, err := parseWebhookJSONObject([]byte(requestBody)); err == nil {
			data = parsed
		} else {
			dataParseError = err.Error()
			data = nil
		}
	}

	seed := map[string]any{
		"meta":     meta,
		"data":     data,
		"raw_body": requestBody,
		"route": map[string]any{
			"id":   entry.RouteID,
			"path": entry.Path,
			"mode": routeMode,
		},
	}

	return webhookDebugPipelineResult{
		Mode:           "debug-pipeline",
		EventID:        strings.TrimSpace(entry.EventID),
		RouteID:        strings.TrimSpace(entry.RouteID),
		Method:         strings.TrimSpace(entry.Method),
		Path:           strings.TrimSpace(entry.Path),
		ResponseStatus: entry.ResponseStatus,
		PipelineSeed:   seed,
		DataParseError: dataParseError,
		Dispatch:       entry.Dispatch,
	}
}

func newWebhookEventID(now time.Time) string {
	randomPart := make([]byte, 4)
	if _, err := rand.Read(randomPart); err != nil {
		return fmt.Sprintf("evt-%d", now.UnixNano())
	}
	return fmt.Sprintf("evt-%d-%s", now.UnixNano(), hex.EncodeToString(randomPart))
}

func createWebhookToken(path string, thirdPartyID string, now time.Time) (webhookTokenRecord, error) {
	thirdPartyID = normalizeThirdPartyID(thirdPartyID)
	if thirdPartyID == "" {
		return webhookTokenRecord{}, fmt.Errorf("third-party-id is required")
	}

	records, err := loadWebhookTokenRecords(path)
	if err != nil {
		return webhookTokenRecord{}, err
	}
	if _, exists := records[thirdPartyID]; exists {
		return webhookTokenRecord{}, fmt.Errorf("token already exists for third-party-id %q, use reset", thirdPartyID)
	}

	token, err := generateWebhookRawToken(defaultWebhookTokenByteSize)
	if err != nil {
		return webhookTokenRecord{}, err
	}
	nowText := now.Format(time.RFC3339Nano)
	record := webhookTokenRecord{
		ThirdPartyID: thirdPartyID,
		Token:        token,
		CreatedAt:    nowText,
		UpdatedAt:    nowText,
	}
	records[thirdPartyID] = record

	if err := writeWebhookTokenRecords(path, records); err != nil {
		return webhookTokenRecord{}, err
	}
	return record, nil
}

func findWebhookToken(path string, thirdPartyID string) (webhookTokenRecord, bool, error) {
	thirdPartyID = normalizeThirdPartyID(thirdPartyID)
	if thirdPartyID == "" {
		return webhookTokenRecord{}, false, nil
	}

	records, err := loadWebhookTokenRecords(path)
	if err != nil {
		return webhookTokenRecord{}, false, err
	}
	record, exists := records[thirdPartyID]
	if !exists {
		return webhookTokenRecord{}, false, nil
	}
	return record, true, nil
}

func resetWebhookToken(path string, thirdPartyID string, now time.Time) (webhookTokenRecord, error) {
	thirdPartyID = normalizeThirdPartyID(thirdPartyID)
	if thirdPartyID == "" {
		return webhookTokenRecord{}, fmt.Errorf("third-party-id is required")
	}

	records, err := loadWebhookTokenRecords(path)
	if err != nil {
		return webhookTokenRecord{}, err
	}
	record, exists := records[thirdPartyID]
	if !exists {
		return webhookTokenRecord{}, fmt.Errorf("token not found for third-party-id %q", thirdPartyID)
	}

	token, err := generateWebhookRawToken(defaultWebhookTokenByteSize)
	if err != nil {
		return webhookTokenRecord{}, err
	}
	record.Token = token
	if strings.TrimSpace(record.CreatedAt) == "" {
		record.CreatedAt = now.Format(time.RFC3339Nano)
	}
	record.UpdatedAt = now.Format(time.RFC3339Nano)
	records[thirdPartyID] = record

	if err := writeWebhookTokenRecords(path, records); err != nil {
		return webhookTokenRecord{}, err
	}
	return record, nil
}

func loadWebhookTokenRecords(path string) (map[string]webhookTokenRecord, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("token store path is empty")
	}

	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]webhookTokenRecord{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]webhookTokenRecord{}, nil
	}

	var state webhookTokenState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}

	records := make(map[string]webhookTokenRecord, len(state.Tokens))
	for _, record := range state.Tokens {
		thirdPartyID := normalizeThirdPartyID(record.ThirdPartyID)
		token := strings.TrimSpace(record.Token)
		if thirdPartyID == "" || token == "" {
			continue
		}
		record.ThirdPartyID = thirdPartyID
		record.Token = token
		records[thirdPartyID] = record
	}
	return records, nil
}

func writeWebhookTokenRecords(path string, records map[string]webhookTokenRecord) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("token store path is empty")
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	state := webhookTokenState{
		Version: webhookTokenStateVersion,
		Tokens:  make([]webhookTokenRecord, 0, len(keys)),
	}
	for _, key := range keys {
		record := records[key]
		record.ThirdPartyID = normalizeThirdPartyID(key)
		record.Token = strings.TrimSpace(record.Token)
		if record.ThirdPartyID == "" || record.Token == "" {
			continue
		}
		state.Tokens = append(state.Tokens, record)
	}

	data, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(data, '\n'), 0o644)
}

func readWebhookRuntimeState(path string) (*webhookRuntimeInfo, bool, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, false, fmt.Errorf("runtime path is empty")
	}
	if err := migrateWebhookRuntimeStateIfNeeded(trimmedPath); err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, false, nil
	}
	var state webhookRuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, false, err
	}
	if state.Runtime.PID == 0 && strings.TrimSpace(state.Runtime.Address) == "" {
		return nil, false, nil
	}
	state.Runtime.Path = ensureWebhookPath(state.Runtime.Path)
	return &state.Runtime, true, nil
}

func writeWebhookRuntimeState(path string, runtime webhookRuntimeInfo) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("runtime path is empty")
	}
	runtime.Path = ensureWebhookPath(runtime.Path)
	if strings.TrimSpace(runtime.UpdatedAt) == "" {
		runtime.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	}
	state := webhookRuntimeState{
		Version: webhookRuntimeStateVersion,
		Runtime: runtime,
	}
	data, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(data, '\n'), 0o644)
}

func migrateWebhookRuntimeStateIfNeeded(path string) error {
	trimmedPath := strings.TrimSpace(path)
	if !isDefaultWebhookRuntimeStatePath(trimmedPath) {
		return nil
	}
	legacyPath := strings.TrimSpace(defaultWebhookLegacyRuntimeStatePath())
	if filepath.Clean(trimmedPath) == filepath.Clean(legacyPath) {
		return nil
	}
	if _, err := os.Stat(trimmedPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	legacyInfo, err := os.Stat(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if legacyInfo.IsDir() {
		return fmt.Errorf("legacy runtime path is a directory: %s", legacyPath)
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(legacyPath, trimmedPath); err == nil {
		return nil
	}

	raw, readErr := os.ReadFile(legacyPath)
	if readErr != nil {
		return readErr
	}
	if writeErr := os.WriteFile(trimmedPath, raw, 0o644); writeErr != nil {
		return writeErr
	}
	if removeErr := os.Remove(legacyPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func isDefaultWebhookRuntimeStatePath(path string) bool {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return false
	}
	return filepath.Clean(trimmedPath) == filepath.Clean(defaultWebhookRuntimeStatePath())
}

func removeWebhookRuntimeState(path string) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil
	}
	err := os.Remove(trimmedPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func cleanupWebhookRuntimeStateForCurrentProcess(runtimePath string) (bool, error) {
	trimmedPath := strings.TrimSpace(runtimePath)
	if trimmedPath == "" {
		return false, nil
	}
	runtime, exists, err := readWebhookRuntimeState(trimmedPath)
	if err != nil {
		return false, err
	}
	if !exists || runtime == nil {
		return false, nil
	}
	if runtime.PID != os.Getpid() {
		return false, nil
	}
	if err := removeWebhookRuntimeState(trimmedPath); err != nil {
		return false, err
	}
	return true, nil
}

func generateWebhookRawToken(byteSize int) (string, error) {
	if byteSize <= 0 {
		return "", fmt.Errorf("token byte size must be > 0")
	}
	raw := make([]byte, byteSize)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random token failed: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func maskWebhookToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return "****"
	}
	return trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
}
