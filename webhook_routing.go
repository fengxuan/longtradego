package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	webhookRouteStateVersion    = 1
	webhookDispatchStateVersion = 1

	webhookRouteStateFile      = "webhook_routes.json"
	webhookDispatchQueueFile   = "webhook_dispatch_queue.json"
	webhookDispatchHistoryFile = "webhook_dispatch_history.jsonl"
	webhookDeadLetterFile      = "webhook_dead_letters.jsonl"
	webhookAuditLogFile        = "webhook_audit.log"

	webhookRouteModeSync  = "sync"
	webhookRouteModeAsync = "async"

	defaultWebhookDispatchWorkers      = 4
	defaultWebhookDispatchPollInterval = 1 * time.Second
	defaultWebhookAsyncMaxAttempts     = 5
	defaultWebhookAsyncRetryBackoff    = 1 * time.Second
	defaultWebhookDownstreamTimeout    = 30 * time.Second
	defaultWebhookLegacyRouteID        = "legacy-default"
)

type webhookRouteState struct {
	Version int                  `json:"version"`
	Routes  []webhookRouteRecord `json:"routes"`
}

type webhookRouteRecord struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Mode         string `json:"mode"`
	Pipeline     string `json:"pipeline,omitempty"`
	Enabled      bool   `json:"enabled"`
	MaxAttempts  int    `json:"max_attempts,omitempty"`
	RetryBackoff string `json:"retry_backoff,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type webhookResolvedRoute struct {
	Record       webhookRouteRecord
	Mode         string
	Commands     [][]string
	MaxAttempts  int
	RetryBackoff time.Duration
	Timeout      time.Duration
}

type webhookRouteSummary struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
	Enabled  bool   `json:"enabled"`
	Pipeline string `json:"pipeline,omitempty"`
}

type webhookDispatchQueueState struct {
	Version int                  `json:"version"`
	Jobs    []webhookDispatchJob `json:"jobs"`
}

type webhookDispatchJob struct {
	JobID              string               `json:"job_id"`
	RouteID            string               `json:"route_id"`
	RoutePath          string               `json:"route_path"`
	Pipeline           string               `json:"pipeline"`
	Event              webhookEventEnvelope `json:"event"`
	Attempt            int                  `json:"attempt"`
	MaxAttempts        int                  `json:"max_attempts"`
	RetryBackoff       string               `json:"retry_backoff"`
	Timeout            string               `json:"timeout"`
	AllowSysDownstream bool                 `json:"allow_sys_downstream,omitempty"`
	NextAttemptAt      string               `json:"next_attempt_at"`
	CreatedAt          string               `json:"created_at"`
	UpdatedAt          string               `json:"updated_at"`
	LastError          string               `json:"last_error,omitempty"`
}

type webhookDispatchHistoryEntry struct {
	Timestamp string             `json:"timestamp"`
	Status    string             `json:"status"`
	Job       webhookDispatchJob `json:"job"`
	Error     string             `json:"error,omitempty"`
}

type webhookDispatchStats struct {
	Pending    int `json:"pending"`
	Retrying   int `json:"retrying"`
	DeadLetter int `json:"dead_letter"`
}

type webhookDispatchAuditInfo struct {
	Mode    string `json:"mode,omitempty"`
	Status  string `json:"status,omitempty"`
	JobID   string `json:"job_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
	Result  any    `json:"result,omitempty"`
}

type webhookAuditLogEntry struct {
	Timestamp              string                    `json:"timestamp"`
	EventID                string                    `json:"event_id,omitempty"`
	RouteID                string                    `json:"route_id,omitempty"`
	Method                 string                    `json:"method"`
	Path                   string                    `json:"path"`
	RemoteAddr             string                    `json:"remote_addr,omitempty"`
	RequestHeaders         map[string][]string       `json:"request_headers"`
	RequestBody            string                    `json:"request_body"`
	RequestJSON            any                       `json:"request_json,omitempty"`
	RequestJSONValid       *bool                     `json:"request_json_valid,omitempty"`
	ResponseStatus         int                       `json:"response_status"`
	ResponseBody           string                    `json:"response_body"`
	ResponseJSON           any                       `json:"response_json,omitempty"`
	ResponseEventID        string                    `json:"response_event_id,omitempty"`
	ResponseThirdPartyID   string                    `json:"response_third_party_id,omitempty"`
	ResponseTokenValid     *bool                     `json:"response_token_valid,omitempty"`
	ResponseTimestampValid *bool                     `json:"response_timestamp_valid,omitempty"`
	ResponseJSONValid      *bool                     `json:"response_json_valid,omitempty"`
	ResponseMetaError      string                    `json:"response_meta_error,omitempty"`
	ReceivedAt             string                    `json:"received_at"`
	RespondedAt            string                    `json:"responded_at"`
	DurationMs             int64                     `json:"duration_ms"`
	Meta                   *webhookEventMeta         `json:"meta,omitempty"`
	Dispatch               *webhookDispatchAuditInfo `json:"dispatch,omitempty"`
}

type webhookDispatcherOptions struct {
	QueuePath          string
	HistoryPath        string
	DeadLetterPath     string
	PollInterval       time.Duration
	Workers            int
	AllowSysDownstream bool
	Now                func() time.Time
}

type webhookDispatchManager struct {
	queuePath          string
	historyPath        string
	deadLetterPath     string
	pollInterval       time.Duration
	workers            int
	allowSysDownstream bool
	nowFn              func() time.Time

	mu     sync.Mutex
	jobs   map[string]webhookDispatchJob
	wakeCh chan struct{}
	stopCh chan struct{}
	doneCh chan struct{}
}

func defaultWebhookRouteStatePath() string {
	return filepath.Join(daemonConfigDir, webhookRouteStateFile)
}

func defaultWebhookDispatchQueuePath() string {
	return filepath.Join(daemonDataDir, webhookDispatchQueueFile)
}

func defaultWebhookDispatchHistoryPath() string {
	return filepath.Join(daemonDataDir, webhookDispatchHistoryFile)
}

func defaultWebhookDeadLetterPath() string {
	return filepath.Join(daemonDataDir, webhookDeadLetterFile)
}

func defaultWebhookAuditLogPath() string {
	return filepath.Join(commandLogDir, webhookAuditLogFile)
}

func normalizeWebhookRouteMode(mode string) string {
	trimmed := strings.ToLower(strings.TrimSpace(mode))
	switch trimmed {
	case webhookRouteModeAsync:
		return webhookRouteModeAsync
	default:
		return webhookRouteModeSync
	}
}

func parseWebhookRouteMode(mode string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(mode))
	if trimmed == "" {
		return webhookRouteModeSync, nil
	}
	if trimmed == webhookRouteModeSync || trimmed == webhookRouteModeAsync {
		return trimmed, nil
	}
	return "", fmt.Errorf("invalid route mode %q, allowed: sync|async", mode)
}

func normalizeWebhookRouteRecord(record webhookRouteRecord) webhookRouteRecord {
	record.ID = strings.TrimSpace(record.ID)
	record.Path = ensureWebhookPath(record.Path)
	record.Mode = normalizeWebhookRouteMode(record.Mode)
	record.Pipeline = strings.TrimSpace(record.Pipeline)
	if record.MaxAttempts <= 0 {
		record.MaxAttempts = defaultWebhookAsyncMaxAttempts
	}
	if strings.TrimSpace(record.RetryBackoff) == "" {
		record.RetryBackoff = defaultWebhookAsyncRetryBackoff.String()
	}
	if strings.TrimSpace(record.Timeout) == "" {
		record.Timeout = defaultWebhookDownstreamTimeout.String()
	}
	if strings.TrimSpace(record.CreatedAt) == "" {
		record.CreatedAt = time.Now().Format(time.RFC3339Nano)
	}
	record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	return record
}

func parseWebhookRouteDuration(raw string, fallback time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if fallback <= 0 {
			return 0, fmt.Errorf("duration fallback must be > 0")
		}
		return fallback, nil
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", trimmed, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("duration %q must be > 0", trimmed)
	}
	return parsed, nil
}

func loadWebhookRouteRecords(path string) (map[string]webhookRouteRecord, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("routes path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]webhookRouteRecord{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]webhookRouteRecord{}, nil
	}
	var state webhookRouteState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	records := make(map[string]webhookRouteRecord, len(state.Routes))
	for _, record := range state.Routes {
		record = normalizeWebhookRouteRecord(record)
		if strings.TrimSpace(record.ID) == "" {
			continue
		}
		records[record.ID] = record
	}
	return records, nil
}

func writeWebhookRouteRecords(path string, records map[string]webhookRouteRecord) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("routes path is empty")
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	state := webhookRouteState{
		Version: webhookRouteStateVersion,
		Routes:  make([]webhookRouteRecord, 0, len(keys)),
	}
	for _, key := range keys {
		record := normalizeWebhookRouteRecord(records[key])
		if strings.TrimSpace(record.ID) == "" {
			record.ID = key
		}
		if record.ID == "" {
			continue
		}
		state.Routes = append(state.Routes, record)
	}
	encoded, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(encoded, '\n'), 0o644)
}

func ensureWebhookLegacyRoute(routesPath string, legacyPath string) ([]webhookRouteRecord, error) {
	records, err := loadWebhookRouteRecords(routesPath)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		nowText := time.Now().Format(time.RFC3339Nano)
		legacy := webhookRouteRecord{
			ID:           defaultWebhookLegacyRouteID,
			Path:         ensureWebhookPath(legacyPath),
			Mode:         webhookRouteModeSync,
			Pipeline:     "",
			Enabled:      true,
			MaxAttempts:  defaultWebhookAsyncMaxAttempts,
			RetryBackoff: defaultWebhookAsyncRetryBackoff.String(),
			Timeout:      defaultWebhookDownstreamTimeout.String(),
			CreatedAt:    nowText,
			UpdatedAt:    nowText,
		}
		records[legacy.ID] = legacy
		if err := writeWebhookRouteRecords(routesPath, records); err != nil {
			return nil, err
		}
	}
	items := make([]webhookRouteRecord, 0, len(records))
	for _, record := range records {
		items = append(items, normalizeWebhookRouteRecord(record))
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func resolveWebhookRoutes(records []webhookRouteRecord, cfg *webhookServeConfig) ([]webhookResolvedRoute, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("no webhook routes configured")
	}
	resolved := make([]webhookResolvedRoute, 0, len(records))
	usedPaths := make(map[string]string)
	for _, raw := range records {
		record := normalizeWebhookRouteRecord(raw)
		mode, modeErr := parseWebhookRouteMode(record.Mode)
		if modeErr != nil {
			return nil, fmt.Errorf("route %s mode invalid: %w", record.ID, modeErr)
		}
		record.Mode = mode
		if !record.Enabled {
			continue
		}
		if record.ID == "" {
			return nil, fmt.Errorf("route id is required")
		}
		if record.Path == "" {
			return nil, fmt.Errorf("route %s path is required", record.ID)
		}
		if previousID, exists := usedPaths[record.Path]; exists {
			return nil, fmt.Errorf("duplicate enabled route path %s used by %s and %s", record.Path, previousID, record.ID)
		}
		usedPaths[record.Path] = record.ID

		commands := make([][]string, 0)
		if strings.TrimSpace(record.Pipeline) != "" {
			parsed, err := parsePipelineCommandLine(record.Pipeline)
			if err != nil {
				return nil, fmt.Errorf("route %s pipeline parse failed: %w", record.ID, err)
			}
			commands = parsed
		}
		if mode == webhookRouteModeAsync && len(commands) == 0 {
			return nil, fmt.Errorf("route %s async mode requires pipeline", record.ID)
		}
		maxAttempts := record.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = cfg.DefaultAsyncMaxAttempts
		}
		if maxAttempts <= 0 {
			maxAttempts = defaultWebhookAsyncMaxAttempts
		}
		retryBackoff, err := parseWebhookRouteDuration(record.RetryBackoff, cfg.DefaultAsyncRetryBackoff)
		if err != nil {
			return nil, fmt.Errorf("route %s retry-backoff: %w", record.ID, err)
		}
		timeout, err := parseWebhookRouteDuration(record.Timeout, cfg.DefaultDownstreamTimeout)
		if err != nil {
			return nil, fmt.Errorf("route %s timeout: %w", record.ID, err)
		}
		resolved = append(resolved, webhookResolvedRoute{
			Record:       record,
			Mode:         mode,
			Commands:     commands,
			MaxAttempts:  maxAttempts,
			RetryBackoff: retryBackoff,
			Timeout:      timeout,
		})
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("no enabled webhook routes")
	}
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].Record.ID < resolved[j].Record.ID
	})
	return resolved, nil
}

func summarizeWebhookRoutes(routes []webhookResolvedRoute) []webhookRouteSummary {
	summaries := make([]webhookRouteSummary, 0, len(routes))
	for _, route := range routes {
		summaries = append(summaries, webhookRouteSummary{
			ID:       route.Record.ID,
			Path:     route.Record.Path,
			Mode:     route.Mode,
			Enabled:  route.Record.Enabled,
			Pipeline: route.Record.Pipeline,
		})
	}
	return summaries
}

func validateWebhookDispatchCommands(commands [][]string, allowSys bool) error {
	if len(commands) == 0 {
		return nil
	}
	for index, command := range commands {
		if len(command) == 0 {
			return fmt.Errorf("stage %d is empty", index+1)
		}
		first := strings.ToLower(strings.TrimSpace(command[0]))
		switch first {
		case "exit", "quit", "daemon", "d", "task", "monitor":
			return fmt.Errorf("stage %d command %q is not allowed for webhook downstream", index+1, first)
		case "webhook":
			if len(command) < 2 || !strings.EqualFold(strings.TrimSpace(command[1]), "sign") {
				return fmt.Errorf("stage %d webhook subcommand is not allowed", index+1)
			}
		case "sys", "shell":
			if !allowSys {
				return fmt.Errorf("stage %d system command is disabled, enable --allow-sys-downstream", index+1)
			}
		case "email", "mail":
			if len(command) >= 2 {
				sub := strings.ToLower(strings.TrimSpace(command[1]))
				if sub == "monitor" || sub == "watch" || sub == "receive" || sub == "recv" || sub == "inbox" {
					return fmt.Errorf("stage %d email subcommand %q is not allowed for webhook downstream", index+1, sub)
				}
			}
		case "quote", "q":
			// allowed
		default:
			return fmt.Errorf("stage %d command %q is not in webhook downstream allowlist", index+1, first)
		}
	}
	return nil
}

func buildWebhookPipelineSeed(route webhookResolvedRoute, event webhookEventEnvelope) *daemonPipelineStage {
	rawBody := string(event.RawBody)
	return &daemonPipelineStage{
		commandLine: fmt.Sprintf("webhook route %s", route.Record.ID),
		result: map[string]any{
			"meta":     event.Meta,
			"data":     event.Data,
			"raw_body": rawBody,
			"route":    map[string]any{"id": route.Record.ID, "path": route.Record.Path, "mode": route.Mode},
		},
	}
}

func runWebhookDownstreamPipeline(ctx context.Context, route webhookResolvedRoute, event webhookEventEnvelope, allowSys bool) (any, error) {
	if len(route.Commands) == 0 {
		return nil, nil
	}
	if err := validateWebhookDispatchCommands(route.Commands, allowSys); err != nil {
		return nil, err
	}
	app := newAppContext()
	defer func() {
		_ = app.Close()
	}()
	seed := buildWebhookPipelineSeed(route, event)
	commands := clonePipelineCommands(route.Commands)
	if err := executeDaemonPipelineWithPrevious(ctx, app, nil, nil, commands, seed); err != nil {
		return nil, err
	}
	_, _, result := app.ExecutionSnapshot()
	return result, nil
}

func parseWebhookDispatchTime(raw string, fallback time.Time) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseWebhookDispatchDuration(raw string, fallback time.Duration) time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if fallback > 0 {
			return fallback
		}
		return defaultWebhookAsyncRetryBackoff
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil || parsed <= 0 {
		if fallback > 0 {
			return fallback
		}
		return defaultWebhookAsyncRetryBackoff
	}
	return parsed
}

func parseWebhookDispatchInt(raw string, fallback int) int {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(trimmed)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func webhookDispatchBackoff(attempt int, base time.Duration) time.Duration {
	if attempt <= 1 {
		return base
	}
	if base <= 0 {
		base = defaultWebhookAsyncRetryBackoff
	}
	backoff := base
	for index := 1; index < attempt; index++ {
		if backoff > 12*time.Hour {
			return 12 * time.Hour
		}
		backoff = backoff * 2
	}
	if backoff > 12*time.Hour {
		return 12 * time.Hour
	}
	return backoff
}

func readWebhookDispatchQueue(path string) ([]webhookDispatchJob, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("dispatch queue path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []webhookDispatchJob{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return []webhookDispatchJob{}, nil
	}
	var state webhookDispatchQueueState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	jobs := make([]webhookDispatchJob, 0, len(state.Jobs))
	for _, job := range state.Jobs {
		if strings.TrimSpace(job.JobID) == "" {
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func writeWebhookDispatchQueue(path string, jobs []webhookDispatchJob) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("dispatch queue path is empty")
	}
	sorted := append([]webhookDispatchJob(nil), jobs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].NextAttemptAt == sorted[j].NextAttemptAt {
			return sorted[i].JobID < sorted[j].JobID
		}
		return sorted[i].NextAttemptAt < sorted[j].NextAttemptAt
	})
	state := webhookDispatchQueueState{
		Version: webhookDispatchStateVersion,
		Jobs:    sorted,
	}
	encoded, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(encoded, '\n'), 0o644)
}

func appendWebhookDispatchHistory(path string, entry webhookDispatchHistoryEntry) error {
	return appendWebhookJSONL(path, entry)
}

func appendWebhookDeadLetter(path string, entry webhookDispatchHistoryEntry) error {
	return appendWebhookJSONL(path, entry)
}

func appendWebhookJSONL(path string, value any) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("jsonl path is empty")
	}
	encoded, err := marshalJSONNoHTMLEscape(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(trimmedPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(encoded, '\n'))
	return err
}

func countWebhookJSONLLines(path string) (int, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return 0, nil
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if len(raw) == 0 {
		return 0, nil
	}
	count := 0
	for _, b := range raw {
		if b == '\n' {
			count++
		}
	}
	if raw[len(raw)-1] != '\n' {
		count++
	}
	return count, nil
}

func readWebhookDispatchStats(queuePath string, deadLetterPath string) (webhookDispatchStats, error) {
	jobs, err := readWebhookDispatchQueue(queuePath)
	if err != nil {
		return webhookDispatchStats{}, err
	}
	stats := webhookDispatchStats{}
	for _, job := range jobs {
		if job.Attempt > 0 {
			stats.Retrying++
			continue
		}
		stats.Pending++
	}
	deadCount, err := countWebhookJSONLLines(deadLetterPath)
	if err != nil {
		return webhookDispatchStats{}, err
	}
	stats.DeadLetter = deadCount
	return stats, nil
}

func appendWebhookAuditLog(path string, entry webhookAuditLogEntry) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil
	}
	encoded, err := marshalJSONNoHTMLEscape(entry)
	if err != nil {
		return err
	}
	return appendLogLineWithRotation(trimmedPath, append(encoded, '\n'), commandLogMaxSize, "webhook_audit")
}

func copyWebhookHeadersForAudit(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return map[string][]string{}
	}
	copied := make(map[string][]string, len(headers))
	for key, values := range headers {
		next := make([]string, 0, len(values))
		for _, value := range values {
			next = append(next, value)
		}
		copied[key] = next
	}
	return copied
}

func newWebhookDispatchManager(opts webhookDispatcherOptions) (*webhookDispatchManager, error) {
	queuePath := strings.TrimSpace(opts.QueuePath)
	if queuePath == "" {
		return nil, fmt.Errorf("dispatch queue path is empty")
	}
	jobs, err := readWebhookDispatchQueue(queuePath)
	if err != nil {
		return nil, err
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultWebhookDispatchPollInterval
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWebhookDispatchWorkers
	}
	manager := &webhookDispatchManager{
		queuePath:          queuePath,
		historyPath:        strings.TrimSpace(opts.HistoryPath),
		deadLetterPath:     strings.TrimSpace(opts.DeadLetterPath),
		pollInterval:       poll,
		workers:            workers,
		allowSysDownstream: opts.AllowSysDownstream,
		nowFn:              nowFn,
		jobs:               make(map[string]webhookDispatchJob, len(jobs)),
		wakeCh:             make(chan struct{}, 1),
		stopCh:             make(chan struct{}),
		doneCh:             make(chan struct{}),
	}
	for _, job := range jobs {
		manager.jobs[job.JobID] = job
	}
	return manager, nil
}

func (m *webhookDispatchManager) Start() {
	if m == nil {
		return
	}
	for index := 0; index < m.workers; index++ {
		go m.workerLoop()
	}
}

func (m *webhookDispatchManager) Stop() {
	if m == nil {
		return
	}
	select {
	case <-m.stopCh:
		return
	default:
		close(m.stopCh)
	}
}

func (m *webhookDispatchManager) Enqueue(route webhookResolvedRoute, event webhookEventEnvelope) (webhookDispatchJob, error) {
	if m == nil {
		return webhookDispatchJob{}, fmt.Errorf("dispatch manager is not initialized")
	}
	now := m.nowFn()
	job := webhookDispatchJob{
		JobID:              newWebhookEventID(now),
		RouteID:            route.Record.ID,
		RoutePath:          route.Record.Path,
		Pipeline:           route.Record.Pipeline,
		Event:              event,
		Attempt:            0,
		MaxAttempts:        route.MaxAttempts,
		RetryBackoff:       route.RetryBackoff.String(),
		Timeout:            route.Timeout.String(),
		AllowSysDownstream: m.allowSysDownstream,
		NextAttemptAt:      now.Format(time.RFC3339Nano),
		CreatedAt:          now.Format(time.RFC3339Nano),
		UpdatedAt:          now.Format(time.RFC3339Nano),
	}

	m.mu.Lock()
	m.jobs[job.JobID] = job
	jobs := m.snapshotJobsLocked()
	m.mu.Unlock()

	if err := writeWebhookDispatchQueue(m.queuePath, jobs); err != nil {
		m.mu.Lock()
		delete(m.jobs, job.JobID)
		m.mu.Unlock()
		return webhookDispatchJob{}, err
	}
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
	return job, nil
}

func (m *webhookDispatchManager) snapshotJobsLocked() []webhookDispatchJob {
	jobs := make([]webhookDispatchJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	return jobs
}

func (m *webhookDispatchManager) workerLoop() {
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

		now := m.nowFn()
		job, wait := m.nextDueJob(now)
		if strings.TrimSpace(job.JobID) == "" {
			if !m.waitForWake(wait) {
				return
			}
			continue
		}
		if wait > 0 {
			if !m.waitForWake(wait) {
				return
			}
			continue
		}
		m.processJob(job)
	}
}

func (m *webhookDispatchManager) waitForWake(wait time.Duration) bool {
	if wait <= 0 {
		wait = m.pollInterval
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-m.stopCh:
		return false
	case <-m.wakeCh:
		return true
	case <-timer.C:
		return true
	}
}

func (m *webhookDispatchManager) nextDueJob(now time.Time) (webhookDispatchJob, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.jobs) == 0 {
		return webhookDispatchJob{}, m.pollInterval
	}
	jobs := m.snapshotJobsLocked()
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].NextAttemptAt == jobs[j].NextAttemptAt {
			return jobs[i].JobID < jobs[j].JobID
		}
		return jobs[i].NextAttemptAt < jobs[j].NextAttemptAt
	})
	candidate := jobs[0]
	nextAt := parseWebhookDispatchTime(candidate.NextAttemptAt, now)
	if nextAt.After(now) {
		return candidate, nextAt.Sub(now)
	}
	return candidate, 0
}

func (m *webhookDispatchManager) processJob(job webhookDispatchJob) {
	route := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       job.RouteID,
			Path:     job.RoutePath,
			Pipeline: job.Pipeline,
			Mode:     webhookRouteModeAsync,
			Enabled:  true,
		},
		Mode: webhookRouteModeAsync,
	}
	if strings.TrimSpace(job.Pipeline) != "" {
		parsed, err := parsePipelineCommandLine(job.Pipeline)
		if err != nil {
			m.markJobFailed(job, fmt.Errorf("parse pipeline failed: %w", err), true)
			return
		}
		route.Commands = parsed
	}
	route.MaxAttempts = parseWebhookDispatchInt(strconv.Itoa(job.MaxAttempts), defaultWebhookAsyncMaxAttempts)
	route.RetryBackoff = parseWebhookDispatchDuration(job.RetryBackoff, defaultWebhookAsyncRetryBackoff)
	route.Timeout = parseWebhookDispatchDuration(job.Timeout, defaultWebhookDownstreamTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), route.Timeout)
	_, err := runWebhookDownstreamPipeline(ctx, route, job.Event, job.AllowSysDownstream)
	cancel()
	if err != nil {
		m.markJobFailed(job, err, false)
		return
	}
	m.markJobSucceeded(job)
}

func (m *webhookDispatchManager) markJobSucceeded(job webhookDispatchJob) {
	nowText := m.nowFn().Format(time.RFC3339Nano)
	m.mu.Lock()
	delete(m.jobs, job.JobID)
	jobs := m.snapshotJobsLocked()
	m.mu.Unlock()
	_ = writeWebhookDispatchQueue(m.queuePath, jobs)
	job.UpdatedAt = nowText
	_ = appendWebhookDispatchHistory(m.historyPath, webhookDispatchHistoryEntry{
		Timestamp: nowText,
		Status:    "success",
		Job:       job,
	})
}

func (m *webhookDispatchManager) markJobFailed(job webhookDispatchJob, err error, fatal bool) {
	now := m.nowFn()
	nowText := now.Format(time.RFC3339Nano)
	job.Attempt++
	job.UpdatedAt = nowText
	job.LastError = err.Error()

	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultWebhookAsyncMaxAttempts
	}
	if fatal || job.Attempt >= maxAttempts {
		m.mu.Lock()
		delete(m.jobs, job.JobID)
		jobs := m.snapshotJobsLocked()
		m.mu.Unlock()
		_ = writeWebhookDispatchQueue(m.queuePath, jobs)
		entry := webhookDispatchHistoryEntry{
			Timestamp: nowText,
			Status:    "dead_letter",
			Job:       job,
			Error:     err.Error(),
		}
		_ = appendWebhookDispatchHistory(m.historyPath, entry)
		_ = appendWebhookDeadLetter(m.deadLetterPath, entry)
		return
	}

	base := parseWebhookDispatchDuration(job.RetryBackoff, defaultWebhookAsyncRetryBackoff)
	nextAt := now.Add(webhookDispatchBackoff(job.Attempt, base))
	job.NextAttemptAt = nextAt.Format(time.RFC3339Nano)

	m.mu.Lock()
	m.jobs[job.JobID] = job
	jobs := m.snapshotJobsLocked()
	m.mu.Unlock()
	_ = writeWebhookDispatchQueue(m.queuePath, jobs)
	_ = appendWebhookDispatchHistory(m.historyPath, webhookDispatchHistoryEntry{
		Timestamp: nowText,
		Status:    "retry",
		Job:       job,
		Error:     err.Error(),
	})
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}
