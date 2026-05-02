package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	bookingRuntimeStateVersion = 1
	bookingRuntimeStateFile    = "booking_runtime.json"
	bookingServiceLogFile      = "booking_service.log"
	bookingAPIKeysConfigFile   = "booking_api_keys.json"
	bookingLLMConfigFile       = "booking_llm.json"
	bookingIntakeDraftsFile    = "booking_intake_drafts.json"

	defaultBookingServiceAddr         = ":18081"
	defaultBookingServicePortFallback = 20
	defaultBookingServiceStopTimeout  = 5 * time.Second
	defaultBookingServiceStartTimeout = 3 * time.Second
	defaultBookingServiceStartPoll    = 100 * time.Millisecond

	bookingServiceInternalEnv = "LONGTRADEGO_BOOKING_INTERNAL_SERVE"

	bookingPublicCatalogPath       = "/booking/catalog"
	bookingPublicReservationsPath  = "/booking/reservations"
	bookingPublicIntentParsePath   = "/booking/intents/parse"
	bookingPublicIntentConfirmPath = "/booking/intents/confirm"

	bookingAdminStatusPath             = "/admin/booking/status"
	bookingAdminProductUpsertPath      = "/admin/booking/product/upsert"
	bookingAdminProductRemovePath      = "/admin/booking/product/remove"
	bookingAdminSlotUpsertPath         = "/admin/booking/slot/upsert"
	bookingAdminSlotRemovePath         = "/admin/booking/slot/remove"
	bookingAdminReservationConfirmPath = "/admin/booking/reservation/confirm"
	bookingAdminReservationRejectPath  = "/admin/booking/reservation/reject"
	bookingAdminReservationCancelPath  = "/admin/booking/reservation/cancel"

	bookingHeaderAPIKey = "X-Booking-API-Key"

	bookingDraftStateVersion    = 1
	bookingDraftStatusDraft     = "draft"
	bookingDraftStatusConfirmed = "confirmed"
	bookingDraftStatusExpired   = "expired"

	bookingDraftContinueWindow          = 24 * time.Hour
	bookingDraftOverrideConfidenceMin   = 0.80
	bookingDraftOverrideConfidenceDelta = 0.10
	bookingIntentParseActionCreated     = "created"
	bookingIntentParseActionContinued   = "continued"
	bookingIntentParseActionNoChange    = "continued_no_change"
)

type bookingServiceRuntimeState struct {
	Version int                       `json:"version"`
	Runtime bookingServiceRuntimeInfo `json:"runtime"`
}

type bookingServiceRuntimeInfo struct {
	PID       int    `json:"pid"`
	Address   string `json:"address"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type bookingServiceStartResult struct {
	Mode      string                     `json:"mode"`
	Status    string                     `json:"status"`
	Message   string                     `json:"message,omitempty"`
	Runtime   *bookingServiceRuntimeInfo `json:"runtime,omitempty"`
	LogPath   string                     `json:"log_path,omitempty"`
	StartArgs []string                   `json:"start_args,omitempty"`
}

type bookingServiceStatusResult struct {
	Mode    string                     `json:"mode"`
	Status  string                     `json:"status"`
	Running bool                       `json:"running"`
	Runtime *bookingServiceRuntimeInfo `json:"runtime,omitempty"`
	URL     string                     `json:"url,omitempty"`
	Message string                     `json:"message,omitempty"`
}

type bookingServiceStopResult struct {
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	PID     int    `json:"pid,omitempty"`
	Message string `json:"message,omitempty"`
}

type bookingAPIKeysConfig struct {
	Keys    []string `json:"keys"`
	APIKeys []string `json:"api_keys"`
}

type bookingLLMConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model,omitempty"`
}

type bookingServiceServeConfig struct {
	Addr             string
	RuntimePath      string
	CatalogPath      string
	ReservationsPath string
	DraftsPath       string
	APIKeysPath      string
	LLMConfigPath    string
	AdminAuthPath    string
	MaxPortFallback  int
}

type bookingServiceHandle struct {
	server      *http.Server
	listener    net.Listener
	runtimePath string
	out         io.Writer
	stopOnce    sync.Once
	errCh       chan error
}

type bookingServiceController struct {
	service       *bookingService
	apiKeys       map[string]struct{}
	authConfig    daemonAdminAuthConfig
	draftsPath    string
	llmConfigPath string
	nowFn         func() time.Time
	parseWithLLM  bookingIntentParseFn
}

type bookingIntentParseFn func(context.Context, bookingIntentParseRequest, bookingIntentParseContext) (bookingIntentParseExtracted, error)

var (
	bookingSpawnBackgroundProcess = spawnBookingBackgroundProcess
	bookingVerifyBackgroundStart  = verifyBookingBackgroundStart
	bookingStateDraftMu           sync.Mutex
	bookingParseIntentWithLLM     bookingIntentParseFn = parseBookingIntentWithOpenAI
)

type bookingIntentParseRequest struct {
	UserID  string `json:"user_id"`
	Content string `json:"content"`
	Channel string `json:"channel,omitempty"`
}

type bookingIntentConfirmRequest struct {
	DraftID             string                       `json:"draft_id"`
	ProductID           string                       `json:"product_id,omitempty"`
	SlotID              string                       `json:"slot_id,omitempty"`
	PartySize           int                          `json:"party_size,omitempty"`
	Personnel           *bookingReservationPersonnel `json:"personnel,omitempty"`
	SpecialRequirements string                       `json:"special_requirements,omitempty"`
}

type bookingIntentParseExtracted struct {
	ProductID           string                      `json:"product_id,omitempty"`
	ProductName         string                      `json:"product_name,omitempty"`
	SlotID              string                      `json:"slot_id,omitempty"`
	SlotStartAt         string                      `json:"slot_start_at,omitempty"`
	PartySize           int                         `json:"party_size,omitempty"`
	Personnel           bookingReservationPersonnel `json:"personnel,omitempty"`
	SpecialRequirements string                      `json:"special_requirements,omitempty"`
	Confidence          float64                     `json:"confidence,omitempty"`
}

type bookingIntentParseContext struct {
	Products []bookingProduct
	Slots    []bookingSlotView
	LLM      bookingLLMConfig
}

type bookingIntakeDraftState struct {
	Version   int                  `json:"version"`
	NextID    int64                `json:"next_id,omitempty"`
	Drafts    []bookingIntakeDraft `json:"drafts"`
	UpdatedAt string               `json:"updated_at,omitempty"`
}

type bookingIntakeDraft struct {
	ID                   string                      `json:"id"`
	UserID               string                      `json:"user_id"`
	Channel              string                      `json:"channel,omitempty"`
	Content              string                      `json:"content"`
	Status               string                      `json:"status"`
	Extracted            bookingIntentParseExtracted `json:"extracted"`
	MissingFields        []string                    `json:"missing_fields,omitempty"`
	ProductCandidates    []bookingProduct            `json:"product_candidates,omitempty"`
	SlotCandidates       []bookingSlotView           `json:"slot_candidates,omitempty"`
	ConfirmedReservation string                      `json:"confirmed_reservation_id,omitempty"`
	CreatedAt            string                      `json:"created_at,omitempty"`
	UpdatedAt            string                      `json:"updated_at,omitempty"`
}

type bookingIntentParseResult struct {
	Draft           bookingIntakeDraft
	Action          string
	DraftID         string
	UpdatedFields   []string
	OverrideApplied bool
	MatchReason     string
}

var errBookingLLMNotConfigured = errors.New("booking llm not configured")

func defaultBookingRuntimeStatePath() string {
	return filepath.Join(daemonDataDir, bookingRuntimeStateFile)
}

func defaultBookingServiceLogPath() string {
	return filepath.Join(commandLogDir, bookingServiceLogFile)
}

func defaultBookingAPIKeysConfigPath() string {
	return filepath.Join(daemonConfigDir, bookingAPIKeysConfigFile)
}

func defaultBookingLLMConfigPath() string {
	return filepath.Join(daemonConfigDir, bookingLLMConfigFile)
}

func defaultBookingIntakeDraftsPath() string {
	return filepath.Join(daemonDataDir, bookingIntakeDraftsFile)
}

func newBookingServiceServeConfig() *bookingServiceServeConfig {
	return &bookingServiceServeConfig{
		Addr:             defaultBookingServiceAddr,
		RuntimePath:      defaultBookingRuntimeStatePath(),
		CatalogPath:      defaultBookingCatalogStatePath(),
		ReservationsPath: defaultBookingReservationsStatePath(),
		DraftsPath:       defaultBookingIntakeDraftsPath(),
		APIKeysPath:      defaultBookingAPIKeysConfigPath(),
		LLMConfigPath:    defaultBookingLLMConfigPath(),
		AdminAuthPath:    defaultDaemonAdminAuthConfigPath(),
		MaxPortFallback:  defaultBookingServicePortFallback,
	}
}

func (cfg *bookingServiceServeConfig) validate() error {
	if cfg == nil {
		return fmt.Errorf("booking service config is required")
	}
	cfg.Addr = strings.TrimSpace(cfg.Addr)
	cfg.RuntimePath = strings.TrimSpace(cfg.RuntimePath)
	cfg.CatalogPath = strings.TrimSpace(cfg.CatalogPath)
	cfg.ReservationsPath = strings.TrimSpace(cfg.ReservationsPath)
	cfg.DraftsPath = strings.TrimSpace(cfg.DraftsPath)
	cfg.APIKeysPath = strings.TrimSpace(cfg.APIKeysPath)
	cfg.LLMConfigPath = strings.TrimSpace(cfg.LLMConfigPath)
	cfg.AdminAuthPath = strings.TrimSpace(cfg.AdminAuthPath)
	if cfg.Addr == "" {
		return fmt.Errorf("addr is required")
	}
	if cfg.RuntimePath == "" {
		return fmt.Errorf("runtime is required")
	}
	if cfg.CatalogPath == "" {
		return fmt.Errorf("catalog is required")
	}
	if cfg.ReservationsPath == "" {
		return fmt.Errorf("reservations is required")
	}
	if cfg.DraftsPath == "" {
		return fmt.Errorf("drafts is required")
	}
	if cfg.APIKeysPath == "" {
		return fmt.Errorf("api-keys is required")
	}
	if cfg.LLMConfigPath == "" {
		cfg.LLMConfigPath = defaultBookingLLMConfigPath()
	}
	if cfg.AdminAuthPath == "" {
		return fmt.Errorf("admin-auth is required")
	}
	if cfg.MaxPortFallback < 0 {
		return fmt.Errorf("max-port-fallback must be >= 0")
	}
	return nil
}

func loadBookingAPIKeys(path string) ([]string, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("booking api keys config path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("booking api keys config %s not found; copy conf-example/booking_api_keys.json to %s and set at least one key", trimmedPath, trimmedPath)
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("booking api keys config %s is empty", trimmedPath)
	}
	var cfg bookingAPIKeysConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse booking api keys config %s: %w", trimmedPath, err)
	}
	keys := make([]string, 0, len(cfg.Keys)+len(cfg.APIKeys))
	keys = append(keys, cfg.Keys...)
	keys = append(keys, cfg.APIKeys...)
	seen := make(map[string]struct{}, len(keys))
	clean := make([]string, 0, len(keys))
	for _, item := range keys {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		clean = append(clean, trimmed)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("booking api keys config %s has no valid keys", trimmedPath)
	}
	sort.Strings(clean)
	return clean, nil
}

func appendBookingServiceLogLine(logPath string, line string) {
	trimmedPath := strings.TrimSpace(logPath)
	trimmedLine := strings.TrimSpace(line)
	if trimmedPath == "" || trimmedLine == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(trimmedPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintf(file, "%s %s\n", time.Now().Format(time.RFC3339Nano), trimmedLine)
}

func appendBookingServiceStartFailureLog(logPath string, err error) {
	if err == nil {
		return
	}
	appendBookingServiceLogLine(logPath, fmt.Sprintf("booking service start failed: %v", err))
}

func loadBookingLLMConfig(path string) (bookingLLMConfig, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return bookingLLMConfig{}, fmt.Errorf("booking llm config path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		return bookingLLMConfig{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return bookingLLMConfig{}, fmt.Errorf("booking llm config %s is empty", trimmedPath)
	}
	var cfg bookingLLMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return bookingLLMConfig{}, fmt.Errorf("parse booking llm config %s: %w", trimmedPath, err)
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.APIKey == "" {
		return bookingLLMConfig{}, fmt.Errorf("booking llm config %s has empty api_key", trimmedPath)
	}
	if cfg.BaseURL == "" {
		return bookingLLMConfig{}, fmt.Errorf("booking llm config %s has empty base_url", trimmedPath)
	}
	if _, err := normalizeBookingOpenAIChatCompletionsURL(cfg.BaseURL); err != nil {
		return bookingLLMConfig{}, fmt.Errorf("booking llm config %s has invalid base_url: %w", trimmedPath, err)
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4.1-mini"
	}
	return cfg, nil
}

func normalizeBookingOpenAIChatCompletionsURL(baseURL string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", fmt.Errorf("base_url is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.Scheme) == "" || strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("base_url must include scheme and host")
	}
	path := strings.TrimRight(parsed.Path, "/")
	pathLower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(pathLower, "/chat/completions"):
		// Full endpoint already provided; keep as-is.
	case strings.HasSuffix(pathLower, "/v1"):
		path = path + "/chat/completions"
	default:
		path = path + "/v1/chat/completions"
	}
	if path == "" {
		path = "/v1/chat/completions"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func bookingRuntimeStatus(runtimePath string) (string, *bookingServiceRuntimeInfo, error) {
	runtime, exists, err := readBookingRuntimeState(runtimePath)
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

func readBookingRuntimeState(path string) (*bookingServiceRuntimeInfo, bool, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, false, fmt.Errorf("booking runtime path is empty")
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
	var state bookingServiceRuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, false, err
	}
	if state.Runtime.PID == 0 && strings.TrimSpace(state.Runtime.Address) == "" {
		return nil, false, nil
	}
	return &state.Runtime, true, nil
}

func writeBookingRuntimeState(path string, runtime bookingServiceRuntimeInfo) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("booking runtime path is empty")
	}
	if strings.TrimSpace(runtime.UpdatedAt) == "" {
		runtime.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	}
	state := bookingServiceRuntimeState{
		Version: bookingRuntimeStateVersion,
		Runtime: runtime,
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

func removeBookingRuntimeState(path string) error {
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

func cleanupBookingRuntimeStateForCurrentProcess(runtimePath string) (bool, error) {
	trimmedPath := strings.TrimSpace(runtimePath)
	if trimmedPath == "" {
		return false, nil
	}
	runtime, exists, err := readBookingRuntimeState(trimmedPath)
	if err != nil {
		return false, err
	}
	if !exists || runtime == nil {
		return false, nil
	}
	if runtime.PID != os.Getpid() {
		return false, nil
	}
	if err := removeBookingRuntimeState(trimmedPath); err != nil {
		return false, err
	}
	return true, nil
}

func bookingServiceStatus(runtimePath string) (bookingServiceStatusResult, error) {
	status, runtime, err := bookingRuntimeStatus(runtimePath)
	if err != nil {
		return bookingServiceStatusResult{}, err
	}
	result := bookingServiceStatusResult{
		Mode:    "service_status",
		Status:  status,
		Running: status == "running",
		Runtime: runtime,
	}
	if runtime != nil {
		result.URL = buildWebhookManagementURL(runtime.Address, bookingAdminStatusPath)
	}
	if status == "stale" {
		result.Message = "booking runtime exists but process is not running"
	}
	if status == "stopped" {
		result.Message = "booking runtime state not found"
	}
	return result, nil
}

func stopBookingService(runtimePath string, timeout time.Duration) (bookingServiceStopResult, error) {
	runtime, exists, err := readBookingRuntimeState(runtimePath)
	if err != nil {
		return bookingServiceStopResult{}, err
	}
	if !exists || runtime == nil {
		return bookingServiceStopResult{Mode: "service_stop", Status: "not_running", Message: "runtime state not found"}, nil
	}
	if runtime.PID <= 0 {
		_ = removeBookingRuntimeState(runtimePath)
		return bookingServiceStopResult{Mode: "service_stop", Status: "stale_removed", Message: "invalid runtime pid removed"}, nil
	}
	running, err := isProcessRunning(runtime.PID)
	if err != nil {
		return bookingServiceStopResult{}, err
	}
	if !running {
		_ = removeBookingRuntimeState(runtimePath)
		return bookingServiceStopResult{Mode: "service_stop", Status: "stale_removed", PID: runtime.PID, Message: "stale runtime removed"}, nil
	}
	if err := terminateProcess(runtime.PID, timeout); err != nil {
		return bookingServiceStopResult{}, err
	}
	if err := removeBookingRuntimeState(runtimePath); err != nil {
		return bookingServiceStopResult{}, err
	}
	return bookingServiceStopResult{Mode: "service_stop", Status: "stopped", PID: runtime.PID}, nil
}

func startBookingServiceInBackground(runtimePath string, logPath string, cfg *bookingServiceServeConfig) (bookingServiceStartResult, error) {
	runtimePath = strings.TrimSpace(runtimePath)
	logPath = strings.TrimSpace(logPath)
	if runtimePath == "" {
		return bookingServiceStartResult{}, fmt.Errorf("runtime path is empty")
	}
	if logPath == "" {
		return bookingServiceStartResult{}, fmt.Errorf("log path is empty")
	}
	if err := cfg.validate(); err != nil {
		appendBookingServiceStartFailureLog(logPath, err)
		return bookingServiceStartResult{}, err
	}
	if _, err := loadBookingAPIKeys(cfg.APIKeysPath); err != nil {
		appendBookingServiceStartFailureLog(logPath, err)
		return bookingServiceStartResult{}, err
	}
	authCfg, err := loadDaemonAdminAuthConfig(cfg.AdminAuthPath)
	if err != nil {
		appendBookingServiceStartFailureLog(logPath, err)
		return bookingServiceStartResult{}, err
	}

	status, runtime, err := bookingRuntimeStatus(runtimePath)
	if err != nil {
		appendBookingServiceStartFailureLog(logPath, err)
		return bookingServiceStartResult{}, err
	}
	if status == "running" {
		return bookingServiceStartResult{
			Mode:    "service_start",
			Status:  "already_running",
			Message: "booking service is already running",
			Runtime: runtime,
			LogPath: logPath,
		}, nil
	}
	if status == "stale" {
		_ = removeBookingRuntimeState(runtimePath)
	}

	maxFallback := cfg.MaxPortFallback
	if maxFallback < 0 {
		maxFallback = defaultBookingServicePortFallback
	}
	var lastErr error
	for offset := 0; offset <= maxFallback; offset++ {
		candidateAddr, addrErr := daemonAdminAddrWithPortOffset(cfg.Addr, offset)
		if addrErr != nil {
			appendBookingServiceStartFailureLog(logPath, addrErr)
			return bookingServiceStartResult{}, addrErr
		}
		if err := ensureBookingAddrAvailable(candidateAddr); err != nil {
			lastErr = err
			continue
		}

		spawnCfg := *cfg
		spawnCfg.Addr = candidateAddr
		spawnCfg.RuntimePath = runtimePath
		pid, cmdArgs, spawnErr := bookingSpawnBackgroundProcess(&spawnCfg, logPath)
		if spawnErr != nil {
			lastErr = spawnErr
			continue
		}
		if verifyErr := bookingVerifyBackgroundStart(&spawnCfg, pid, authCfg); verifyErr != nil {
			_ = killProcessByPID(pid)
			lastErr = verifyErr
			continue
		}
		nowText := time.Now().Format(time.RFC3339Nano)
		runtimeInfo := bookingServiceRuntimeInfo{
			PID:       pid,
			Address:   candidateAddr,
			StartedAt: nowText,
			UpdatedAt: nowText,
		}
		if err := writeBookingRuntimeState(runtimePath, runtimeInfo); err != nil {
			_ = killProcessByPID(pid)
			appendBookingServiceStartFailureLog(logPath, err)
			return bookingServiceStartResult{}, err
		}

		result := bookingServiceStartResult{
			Mode:      "service_start",
			Status:    "started",
			Runtime:   &runtimeInfo,
			LogPath:   logPath,
			StartArgs: cmdArgs,
		}
		if offset > 0 {
			result.Message = fmt.Sprintf("preferred addr unavailable, fallback to %s", candidateAddr)
		}
		return result, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("all booking service ports unavailable")
	}
	startErr := fmt.Errorf("booking service start failed: %w", lastErr)
	appendBookingServiceStartFailureLog(logPath, startErr)
	return bookingServiceStartResult{}, startErr
}

func ensureBookingAddrAvailable(addr string) error {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return fmt.Errorf("booking addr is empty")
	}
	listener, err := net.Listen("tcp", trimmed)
	if err != nil {
		if isWebhookAddrAlreadyInUseError(err) {
			return err
		}
		return fmt.Errorf("check booking addr %q failed: %w", trimmed, err)
	}
	_ = listener.Close()
	return nil
}

func verifyBookingBackgroundStart(cfg *bookingServiceServeConfig, pid int, authCfg daemonAdminAuthConfig) error {
	deadline := time.Now().Add(defaultBookingServiceStartTimeout)
	var lastErr error
	for {
		running, err := isProcessRunning(pid)
		if err != nil {
			return fmt.Errorf("check booking process status failed: %w", err)
		}
		if !running {
			return fmt.Errorf("booking process exited before becoming ready")
		}

		if _, err := fetchBookingAdminStatus(cfg.Addr, authCfg, defaultWebhookManagementHTTPTimeout); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			break
		}
		time.Sleep(defaultBookingServiceStartPoll)
	}
	if lastErr != nil {
		return fmt.Errorf("booking admin endpoint not ready: %w", lastErr)
	}
	return fmt.Errorf("booking admin endpoint not ready")
}

func fetchBookingAdminStatus(address string, authCfg daemonAdminAuthConfig, timeout time.Duration) (bookingAdminStatus, error) {
	url := buildWebhookManagementURL(address, bookingAdminStatusPath)
	if strings.TrimSpace(url) == "" {
		return bookingAdminStatus{}, fmt.Errorf("booking address is empty")
	}
	clientTimeout := timeout
	if clientTimeout <= 0 {
		clientTimeout = 2 * time.Second
	}
	client := &http.Client{Timeout: clientTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return bookingAdminStatus{}, err
	}
	req.SetBasicAuth(authCfg.Username, authCfg.Password)
	resp, err := client.Do(req)
	if err != nil {
		return bookingAdminStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return bookingAdminStatus{}, fmt.Errorf("booking admin status http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload bookingAdminStatus
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return bookingAdminStatus{}, err
	}
	return payload, nil
}

func spawnBookingBackgroundProcess(cfg *bookingServiceServeConfig, logPath string) (int, []string, error) {
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
	serveArgs := buildBookingServiceServeArgs(cfg)
	cmdArgs := append([]string{"booking", "service", "serve"}, serveArgs...)
	child := exec.Command(execPath, cmdArgs...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Stdin = nil
	child.Env = append(os.Environ(), bookingServiceInternalEnv+"=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return 0, nil, fmt.Errorf("start booking background process failed: %w", err)
	}
	_ = logFile.Close()
	return child.Process.Pid, cmdArgs, nil
}

func buildBookingServiceServeArgs(cfg *bookingServiceServeConfig) []string {
	args := make([]string, 0, 22)
	args = append(args, "--addr", cfg.Addr)
	args = append(args, "--runtime", cfg.RuntimePath)
	args = append(args, "--catalog", cfg.CatalogPath)
	args = append(args, "--reservations", cfg.ReservationsPath)
	args = append(args, "--drafts", cfg.DraftsPath)
	args = append(args, "--api-keys", cfg.APIKeysPath)
	args = append(args, "--llm-config", cfg.LLMConfigPath)
	args = append(args, "--admin-auth", cfg.AdminAuthPath)
	return args
}

func startBookingHTTPService(ctx context.Context, cfg *bookingServiceServeConfig, out io.Writer) (*bookingServiceHandle, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	apiKeys, err := loadBookingAPIKeys(cfg.APIKeysPath)
	if err != nil {
		return nil, err
	}
	authCfg, err := loadDaemonAdminAuthConfig(cfg.AdminAuthPath)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	actualAddr := listener.Addr().String()
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		host := "127.0.0.1"
		if tcpAddr.IP != nil && !tcpAddr.IP.IsUnspecified() {
			host = tcpAddr.IP.String()
		}
		actualAddr = net.JoinHostPort(host, strconv.Itoa(tcpAddr.Port))
	}

	controller := &bookingServiceController{
		service:       newBookingService(cfg.CatalogPath, cfg.ReservationsPath),
		apiKeys:       bookingAPIKeySet(apiKeys),
		authConfig:    authCfg,
		draftsPath:    cfg.DraftsPath,
		llmConfigPath: cfg.LLMConfigPath,
		nowFn:         time.Now,
		parseWithLLM:  bookingParseIntentWithLLM,
	}

	mux := http.NewServeMux()
	controller.registerHandlers(mux)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	nowText := time.Now().Format(time.RFC3339Nano)
	runtimeInfo := bookingServiceRuntimeInfo{
		PID:       os.Getpid(),
		Address:   actualAddr,
		StartedAt: nowText,
		UpdatedAt: nowText,
	}
	if err := writeBookingRuntimeState(cfg.RuntimePath, runtimeInfo); err != nil {
		_ = listener.Close()
		return nil, err
	}

	handle := &bookingServiceHandle{
		server:      server,
		listener:    listener,
		runtimePath: cfg.RuntimePath,
		out:         out,
		errCh:       make(chan error, 1),
	}
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			if out != nil {
				_, _ = fmt.Fprintf(out, "booking service stopped with error: %v\n", err)
			}
			handle.errCh <- err
			close(handle.errCh)
			return
		}
		handle.errCh <- nil
		close(handle.errCh)
	}()

	go func() {
		if ctx == nil {
			return
		}
		<-ctx.Done()
		_ = handle.Close()
	}()

	return handle, nil
}

func (s *bookingServiceHandle) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.stopOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if s.server != nil {
			if err := s.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				closeErr = err
			}
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if removed, err := cleanupBookingRuntimeStateForCurrentProcess(s.runtimePath); err != nil {
			if s.out != nil {
				_, _ = fmt.Fprintf(s.out, "booking runtime cleanup skipped: %v\n", err)
			}
		} else if removed && s.out != nil {
			_, _ = fmt.Fprintln(s.out, "booking runtime state removed on shutdown")
		}
	})
	return closeErr
}

func (s *bookingServiceHandle) Wait() error {
	if s == nil || s.errCh == nil {
		return nil
	}
	return <-s.errCh
}

func bookingAPIKeySet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		set[trimmed] = struct{}{}
	}
	return set
}

func (c *bookingServiceController) registerHandlers(mux *http.ServeMux) {
	if mux == nil {
		return
	}

	requireAPIKey := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !c.isAPIKeyAuthorized(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	requireAdmin := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !c.isAdminAuthorized(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Booking Admin"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc(bookingPublicCatalogPath, requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query, err := parseDaemonBookingCatalogQuery(r)
		if err != nil {
			writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
			return
		}
		result, err := c.service.QueryCatalog(query)
		if err != nil {
			writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
			return
		}
		writeBookingJSON(w, http.StatusOK, result)
	}))

	mux.HandleFunc(bookingPublicReservationsPath, requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			filter := bookingReservationListFilter{
				UserID: strings.TrimSpace(r.URL.Query().Get("user_id")),
				Status: strings.TrimSpace(r.URL.Query().Get("status")),
			}
			reservations, err := c.service.ListReservations(filter)
			if err != nil {
				writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
				return
			}
			writeBookingJSON(w, http.StatusOK, map[string]any{"reservations": reservations})
		case http.MethodPost:
			var payload daemonBookingCreateReservationRequest
			if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
				writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
				return
			}
			reservation, err := c.service.CreateReservation(bookingReservationCreateInput{
				ProductID: strings.TrimSpace(payload.ProductID),
				SlotID:    strings.TrimSpace(payload.SlotID),
				UserID:    strings.TrimSpace(payload.UserID),
				PartySize: payload.PartySize,
				Personnel: bookingReservationPersonnel{
					ContactName:  strings.TrimSpace(payload.Personnel.ContactName),
					ContactPhone: strings.TrimSpace(payload.Personnel.ContactPhone),
					Members:      append([]string(nil), payload.Personnel.Members...),
				},
				SpecialRequirements: strings.TrimSpace(payload.SpecialRequirements),
			})
			if err != nil {
				writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
				return
			}
			writeBookingJSON(w, http.StatusCreated, map[string]any{
				"status":         "created",
				"reservation_id": reservation.ID,
				"reservation":    reservation,
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc(bookingPublicIntentParsePath, requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload bookingIntentParseRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			writeBookingJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "message": err.Error()})
			return
		}
		payload.UserID = strings.TrimSpace(payload.UserID)
		payload.Content = strings.TrimSpace(payload.Content)
		payload.Channel = strings.TrimSpace(payload.Channel)
		if payload.UserID == "" || payload.Content == "" {
			writeBookingJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "message": "user_id and content are required"})
			return
		}
		result, err := c.parseIntentDraft(r.Context(), payload)
		if err != nil {
			if errors.Is(err, errBookingLLMNotConfigured) {
				writeBookingJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "message": err.Error()})
				return
			}
			writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
			return
		}
		writeBookingJSON(w, http.StatusOK, map[string]any{
			"status":           "ok",
			"action":           result.Action,
			"draft_id":         result.DraftID,
			"updated_fields":   result.UpdatedFields,
			"override_applied": result.OverrideApplied,
			"draft":            result.Draft,
		})
	}))

	mux.HandleFunc(bookingPublicIntentConfirmPath, requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload bookingIntentConfirmRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			writeBookingJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "message": err.Error()})
			return
		}
		result, err := c.confirmIntentDraft(payload)
		if err != nil {
			writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
			return
		}
		writeBookingJSON(w, http.StatusCreated, map[string]any{"status": "created", "result": result})
	}))

	registerAdmin := func(path string, handler func(*http.Request) (any, error)) {
		mux.HandleFunc(path, requireAdmin(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			result, err := handler(r)
			if err != nil {
				writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error(), "result": result})
				return
			}
			writeBookingJSON(w, http.StatusOK, map[string]any{"status": "ok", "result": result})
		}))
	}

	mux.HandleFunc(bookingAdminStatusPath, requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := c.service.AdminStatus()
		if err != nil {
			writeBookingJSON(w, bookingHTTPStatus(err), map[string]any{"status": "error", "message": err.Error()})
			return
		}
		writeBookingJSON(w, http.StatusOK, result)
	}))

	registerAdmin(bookingAdminProductUpsertPath, func(r *http.Request) (any, error) {
		var payload daemonBookingProductUpsertRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			return nil, err
		}
		product, err := c.service.UpsertProduct(bookingProductUpsertInput{
			ID:          strings.TrimSpace(payload.ID),
			Name:        strings.TrimSpace(payload.Name),
			Description: strings.TrimSpace(payload.Description),
			Enabled:     payload.Enabled,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "product": product}, nil
	})

	registerAdmin(bookingAdminProductRemovePath, func(r *http.Request) (any, error) {
		id := daemonAdminRequestID(r)
		if id == "" {
			var payload struct {
				ID string `json:"id"`
			}
			if err := decodeDaemonAdminJSONBody(r, &payload); err == nil {
				id = strings.TrimSpace(payload.ID)
			}
		}
		if strings.TrimSpace(id) == "" {
			return nil, bookingValidationf("product id is required")
		}
		removed, err := c.service.RemoveProduct(id)
		if err != nil {
			return nil, err
		}
		status := "removed"
		if !removed {
			status = "not_found"
		}
		return map[string]any{"status": status, "product_id": id}, nil
	})

	registerAdmin(bookingAdminSlotUpsertPath, func(r *http.Request) (any, error) {
		var payload daemonBookingSlotUpsertRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			return nil, err
		}
		slot, err := c.service.UpsertSlot(bookingSlotUpsertInput{
			ID:        strings.TrimSpace(payload.ID),
			ProductID: strings.TrimSpace(payload.ProductID),
			StartAt:   strings.TrimSpace(payload.StartAt),
			EndAt:     strings.TrimSpace(payload.EndAt),
			Capacity:  payload.Capacity,
			Enabled:   payload.Enabled,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "slot": slot}, nil
	})

	registerAdmin(bookingAdminSlotRemovePath, func(r *http.Request) (any, error) {
		id := daemonAdminRequestID(r)
		if id == "" {
			var payload struct {
				ID string `json:"id"`
			}
			if err := decodeDaemonAdminJSONBody(r, &payload); err == nil {
				id = strings.TrimSpace(payload.ID)
			}
		}
		if strings.TrimSpace(id) == "" {
			return nil, bookingValidationf("slot id is required")
		}
		removed, err := c.service.RemoveSlot(id)
		if err != nil {
			return nil, err
		}
		status := "removed"
		if !removed {
			status = "not_found"
		}
		return map[string]any{"status": status, "slot_id": id}, nil
	})

	registerAdmin(bookingAdminReservationConfirmPath, func(r *http.Request) (any, error) {
		var payload daemonBookingReservationActionRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload.ID) == "" {
			payload.ID = daemonAdminRequestID(r)
		}
		reservation, err := c.service.ConfirmReservation(strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Note))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "confirmed", "reservation": reservation}, nil
	})

	registerAdmin(bookingAdminReservationRejectPath, func(r *http.Request) (any, error) {
		var payload daemonBookingReservationActionRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload.ID) == "" {
			payload.ID = daemonAdminRequestID(r)
		}
		reservation, err := c.service.RejectReservation(strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Note))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "rejected", "reservation": reservation}, nil
	})

	registerAdmin(bookingAdminReservationCancelPath, func(r *http.Request) (any, error) {
		var payload daemonBookingReservationActionRequest
		if err := decodeDaemonAdminJSONBody(r, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload.ID) == "" {
			payload.ID = daemonAdminRequestID(r)
		}
		reservation, err := c.service.CancelReservation(strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Note))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "cancelled", "reservation": reservation}, nil
	})
}

func (c *bookingServiceController) isAPIKeyAuthorized(r *http.Request) bool {
	if r == nil {
		return false
	}
	if len(c.apiKeys) == 0 {
		return false
	}
	key := strings.TrimSpace(r.Header.Get(bookingHeaderAPIKey))
	if key == "" {
		return false
	}
	_, ok := c.apiKeys[key]
	return ok
}

func (c *bookingServiceController) isAdminAuthorized(r *http.Request) bool {
	if r == nil {
		return false
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(username), []byte(c.authConfig.Username)) == 1 &&
		subtle.ConstantTimeCompare([]byte(password), []byte(c.authConfig.Password)) == 1
}

func writeBookingJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, err := marshalJSONIndentNoHTMLEscape(payload, "", defaultWebhookResponseIndent)
	if err != nil {
		_, _ = io.WriteString(w, `{"error":"encode failed"}`)
		return
	}
	_, _ = w.Write(append(encoded, '\n'))
}

func bookingHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if isBookingConflictError(err) {
		return http.StatusConflict
	}
	if isBookingValidationError(err) {
		return http.StatusBadRequest
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "required") || strings.Contains(message, "invalid") || strings.Contains(message, "not found") || strings.Contains(message, "missing") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func readBookingDraftState(path string) (bookingIntakeDraftState, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return bookingIntakeDraftState{}, fmt.Errorf("booking draft path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bookingIntakeDraftState{Version: bookingDraftStateVersion, NextID: 1, Drafts: []bookingIntakeDraft{}}, nil
		}
		return bookingIntakeDraftState{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return bookingIntakeDraftState{Version: bookingDraftStateVersion, NextID: 1, Drafts: []bookingIntakeDraft{}}, nil
	}
	var state bookingIntakeDraftState
	if err := json.Unmarshal(raw, &state); err != nil {
		return bookingIntakeDraftState{}, err
	}
	if state.Version == 0 {
		state.Version = bookingDraftStateVersion
	}
	if state.NextID <= 0 {
		state.NextID = 1
	}
	if state.Drafts == nil {
		state.Drafts = []bookingIntakeDraft{}
	}
	return state, nil
}

func writeBookingDraftState(path string, state bookingIntakeDraftState, nowText string) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("booking draft path is empty")
	}
	state.Version = bookingDraftStateVersion
	state.UpdatedAt = strings.TrimSpace(nowText)
	if state.NextID <= 0 {
		state.NextID = 1
	}
	if state.Drafts == nil {
		state.Drafts = []bookingIntakeDraft{}
	}
	sort.Slice(state.Drafts, func(i int, j int) bool {
		left := parseBookingTimeOrZero(state.Drafts[i].CreatedAt)
		right := parseBookingTimeOrZero(state.Drafts[j].CreatedAt)
		if left.Equal(right) {
			return strings.ToLower(state.Drafts[i].ID) < strings.ToLower(state.Drafts[j].ID)
		}
		return left.After(right)
	})
	encoded, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(encoded, '\n'), 0o644)
}

func (c *bookingServiceController) parseIntentDraft(ctx context.Context, req bookingIntentParseRequest) (bookingIntentParseResult, error) {
	catalog, err := c.service.QueryCatalog(bookingCatalogQuery{IncludeFull: true})
	if err != nil {
		return bookingIntentParseResult{}, err
	}
	llmConfigPath := strings.TrimSpace(c.llmConfigPath)
	if llmConfigPath == "" {
		llmConfigPath = defaultBookingLLMConfigPath()
	}
	llmConfig, err := loadBookingLLMConfig(llmConfigPath)
	if err != nil {
		return bookingIntentParseResult{}, fmt.Errorf("%w: %v", errBookingLLMNotConfigured, err)
	}
	parseContext := bookingIntentParseContext{
		Products: catalog.Products,
		Slots:    catalog.Slots,
		LLM:      llmConfig,
	}
	parser := c.parseWithLLM
	if parser == nil {
		parser = bookingParseIntentWithLLM
	}
	bookingLogIntentParse("request", map[string]any{
		"user_id":      strings.TrimSpace(req.UserID),
		"channel":      strings.TrimSpace(req.Channel),
		"content":      bookingTruncateForLog(strings.TrimSpace(req.Content), 800),
		"llm_base_url": strings.TrimSpace(llmConfig.BaseURL),
		"llm_model":    strings.TrimSpace(llmConfig.Model),
	})
	extracted, err := parser(ctx, req, parseContext)
	if err != nil {
		bookingLogIntentParse("parse_error", map[string]any{
			"user_id": strings.TrimSpace(req.UserID),
			"error":   err.Error(),
		})
		return bookingIntentParseResult{}, err
	}
	extracted = normalizeBookingIntentExtracted(extracted)
	extracted, productCandidates, slotCandidates := enrichBookingIntentCandidates(extracted, parseContext)
	missing := bookingIntentMissingFields(extracted)
	bookingLogIntentParse("parse_result", map[string]any{
		"user_id":              strings.TrimSpace(req.UserID),
		"extracted":            extracted,
		"missing_fields":       missing,
		"product_candidates_n": len(productCandidates),
		"slot_candidates_n":    len(slotCandidates),
	})

	bookingStateDraftMu.Lock()
	defer bookingStateDraftMu.Unlock()
	state, err := readBookingDraftState(c.draftsPath)
	if err != nil {
		return bookingIntentParseResult{}, err
	}
	nowText := c.now().Format(time.RFC3339Nano)
	now := c.now()
	continuationIndex, matchReason := findContinuableDraftIndex(state, req.UserID, req.Channel, now)
	if continuationIndex >= 0 {
		original := state.Drafts[continuationIndex]
		merged, overrideApplied := mergeBookingIntentForContinuation(original.Extracted, extracted)
		merged, mergedProductCandidates, mergedSlotCandidates := enrichBookingIntentCandidates(merged, parseContext)
		merged = normalizeBookingIntentExtracted(merged)
		updatedFields := bookingIntentTrackedUpdatedFields(original.Extracted, merged)
		finalMissing := bookingIntentMissingFields(merged)

		updated := original
		updated.Extracted = merged
		updated.MissingFields = finalMissing
		updated.ProductCandidates = mergedProductCandidates
		updated.SlotCandidates = mergedSlotCandidates
		updated.Content = req.Content
		if strings.TrimSpace(updated.Channel) == "" {
			updated.Channel = req.Channel
		}
		updated.UpdatedAt = nowText
		state.Drafts[continuationIndex] = updated
		if err := writeBookingDraftState(c.draftsPath, state, nowText); err != nil {
			return bookingIntentParseResult{}, err
		}

		action := bookingIntentParseActionNoChange
		if len(updatedFields) > 0 {
			action = bookingIntentParseActionContinued
		}
		bookingLogIntentParse("draft_continued", map[string]any{
			"user_id":          strings.TrimSpace(req.UserID),
			"channel":          strings.TrimSpace(req.Channel),
			"draft_id":         updated.ID,
			"action":           action,
			"match_reason":     matchReason,
			"updated_fields":   updatedFields,
			"override_applied": overrideApplied,
			"missing_fields":   finalMissing,
		})
		return bookingIntentParseResult{
			Draft:           updated,
			Action:          action,
			DraftID:         updated.ID,
			UpdatedFields:   updatedFields,
			OverrideApplied: overrideApplied,
			MatchReason:     matchReason,
		}, nil
	}

	draftID := fmt.Sprintf("d-%d", state.NextID)
	state.NextID++
	draft := bookingIntakeDraft{
		ID:                draftID,
		UserID:            req.UserID,
		Channel:           req.Channel,
		Content:           req.Content,
		Status:            bookingDraftStatusDraft,
		Extracted:         extracted,
		MissingFields:     missing,
		ProductCandidates: productCandidates,
		SlotCandidates:    slotCandidates,
		CreatedAt:         nowText,
		UpdatedAt:         nowText,
	}
	state.Drafts = append(state.Drafts, draft)
	if err := writeBookingDraftState(c.draftsPath, state, nowText); err != nil {
		return bookingIntentParseResult{}, err
	}
	bookingLogIntentParse("draft_created", map[string]any{
		"user_id":        strings.TrimSpace(req.UserID),
		"channel":        strings.TrimSpace(req.Channel),
		"draft_id":       draft.ID,
		"match_reason":   matchReason,
		"missing_fields": missing,
	})
	return bookingIntentParseResult{
		Draft:           draft,
		Action:          bookingIntentParseActionCreated,
		DraftID:         draft.ID,
		UpdatedFields:   []string{},
		OverrideApplied: false,
		MatchReason:     matchReason,
	}, nil
}

func findContinuableDraftIndex(state bookingIntakeDraftState, userID string, channel string, now time.Time) (int, string) {
	trimmedUserID := strings.TrimSpace(userID)
	if trimmedUserID == "" {
		return -1, "empty_user"
	}
	trimmedChannel := strings.TrimSpace(channel)
	if trimmedChannel != "" {
		if index := findLatestDraftIndex(state, trimmedUserID, trimmedChannel, true, now); index >= 0 {
			return index, "matched_user_channel"
		}
	}
	if index := findLatestDraftIndex(state, trimmedUserID, trimmedChannel, false, now); index >= 0 {
		return index, "matched_user_fallback"
	}
	return -1, "not_found"
}

func findLatestDraftIndex(state bookingIntakeDraftState, userID string, channel string, requireChannel bool, now time.Time) int {
	targetUser := strings.TrimSpace(userID)
	targetChannel := strings.TrimSpace(channel)
	bestIndex := -1
	var bestTime time.Time

	for index := range state.Drafts {
		draft := state.Drafts[index]
		if !strings.EqualFold(strings.TrimSpace(draft.Status), bookingDraftStatusDraft) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(draft.UserID), targetUser) {
			continue
		}
		draftChannel := strings.TrimSpace(draft.Channel)
		if requireChannel && !strings.EqualFold(draftChannel, targetChannel) {
			continue
		}
		if !requireChannel && targetChannel == "" {
			// no-op: user-only match
		}
		timestamp, ok := bookingDraftContinuationTimestamp(draft)
		if !ok {
			continue
		}
		if now.Sub(timestamp) > bookingDraftContinueWindow {
			continue
		}
		if bestIndex < 0 || timestamp.After(bestTime) {
			bestIndex = index
			bestTime = timestamp
		}
	}
	return bestIndex
}

func bookingDraftContinuationTimestamp(draft bookingIntakeDraft) (time.Time, bool) {
	if parsed := parseBookingTimeOrZero(strings.TrimSpace(draft.UpdatedAt)); !parsed.IsZero() {
		return parsed, true
	}
	if parsed := parseBookingTimeOrZero(strings.TrimSpace(draft.CreatedAt)); !parsed.IsZero() {
		return parsed, true
	}
	return time.Time{}, false
}

func mergeBookingIntentForContinuation(base bookingIntentParseExtracted, incoming bookingIntentParseExtracted) (bookingIntentParseExtracted, bool) {
	merged := normalizeBookingIntentExtracted(base)
	next := normalizeBookingIntentExtracted(incoming)
	allowOverride := next.Confidence >= bookingDraftOverrideConfidenceMin &&
		(next.Confidence-merged.Confidence) >= bookingDraftOverrideConfidenceDelta
	overrideApplied := false

	mergeString := func(current *string, candidate string) {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			return
		}
		if strings.TrimSpace(*current) == "" {
			*current = trimmed
			return
		}
		if allowOverride && strings.TrimSpace(*current) != trimmed {
			*current = trimmed
			overrideApplied = true
		}
	}
	mergeInt := func(current *int, candidate int) {
		if candidate <= 0 {
			return
		}
		if *current <= 0 {
			*current = candidate
			return
		}
		if allowOverride && *current != candidate {
			*current = candidate
			overrideApplied = true
		}
	}
	mergeMembers := func(current *[]string, candidate []string) {
		if len(candidate) == 0 {
			return
		}
		if len(*current) == 0 {
			*current = append([]string(nil), candidate...)
			return
		}
		if allowOverride && !bookingIntentStringSliceEqual(*current, candidate) {
			*current = append([]string(nil), candidate...)
			overrideApplied = true
		}
	}

	mergeString(&merged.ProductID, next.ProductID)
	mergeString(&merged.SlotID, next.SlotID)
	mergeInt(&merged.PartySize, next.PartySize)
	mergeString(&merged.Personnel.ContactName, next.Personnel.ContactName)
	mergeString(&merged.Personnel.ContactPhone, next.Personnel.ContactPhone)
	mergeMembers(&merged.Personnel.Members, next.Personnel.Members)
	mergeString(&merged.SpecialRequirements, next.SpecialRequirements)
	if merged.Confidence <= 0 && next.Confidence > 0 {
		merged.Confidence = next.Confidence
	} else if allowOverride && next.Confidence > merged.Confidence {
		merged.Confidence = next.Confidence
	}

	if strings.TrimSpace(merged.ProductName) == "" && strings.TrimSpace(next.ProductName) != "" {
		merged.ProductName = next.ProductName
	}
	if strings.TrimSpace(merged.SlotStartAt) == "" && strings.TrimSpace(next.SlotStartAt) != "" {
		merged.SlotStartAt = next.SlotStartAt
	}
	return normalizeBookingIntentExtracted(merged), overrideApplied
}

func bookingIntentTrackedUpdatedFields(before bookingIntentParseExtracted, after bookingIntentParseExtracted) []string {
	left := normalizeBookingIntentExtracted(before)
	right := normalizeBookingIntentExtracted(after)
	fields := make([]string, 0, 8)
	if left.ProductID != right.ProductID {
		fields = append(fields, "product_id")
	}
	if left.SlotID != right.SlotID {
		fields = append(fields, "slot_id")
	}
	if left.PartySize != right.PartySize {
		fields = append(fields, "party_size")
	}
	if left.Personnel.ContactName != right.Personnel.ContactName {
		fields = append(fields, "personnel.contact_name")
	}
	if left.Personnel.ContactPhone != right.Personnel.ContactPhone {
		fields = append(fields, "personnel.contact_phone")
	}
	if !bookingIntentStringSliceEqual(left.Personnel.Members, right.Personnel.Members) {
		fields = append(fields, "personnel.members")
	}
	if left.SpecialRequirements != right.SpecialRequirements {
		fields = append(fields, "special_requirements")
	}
	return fields
}

func bookingIntentStringSliceEqual(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if strings.TrimSpace(left[i]) != strings.TrimSpace(right[i]) {
			return false
		}
	}
	return true
}

func (c *bookingServiceController) confirmIntentDraft(req bookingIntentConfirmRequest) (map[string]any, error) {
	draftID := strings.TrimSpace(req.DraftID)
	if draftID == "" {
		return nil, bookingValidationf("draft_id is required")
	}

	bookingStateDraftMu.Lock()
	state, err := readBookingDraftState(c.draftsPath)
	if err != nil {
		bookingStateDraftMu.Unlock()
		return nil, err
	}
	index := -1
	for i := range state.Drafts {
		if strings.EqualFold(strings.TrimSpace(state.Drafts[i].ID), draftID) {
			index = i
			break
		}
	}
	if index < 0 {
		bookingStateDraftMu.Unlock()
		return nil, bookingValidationf("draft not found: %s", draftID)
	}
	draft := state.Drafts[index]
	if strings.TrimSpace(strings.ToLower(draft.Status)) != bookingDraftStatusDraft {
		bookingStateDraftMu.Unlock()
		return nil, bookingValidationf("draft %s is not confirmable (status=%s)", draft.ID, draft.Status)
	}
	merged := mergeBookingDraftForConfirm(draft.Extracted, req)
	missing := bookingIntentMissingFields(merged)
	if len(missing) > 0 {
		bookingStateDraftMu.Unlock()
		return nil, bookingValidationf("missing fields: %s", strings.Join(missing, ", "))
	}
	bookingStateDraftMu.Unlock()

	reservation, err := c.service.CreateReservation(bookingReservationCreateInput{
		ProductID: strings.TrimSpace(merged.ProductID),
		SlotID:    strings.TrimSpace(merged.SlotID),
		UserID:    strings.TrimSpace(draft.UserID),
		PartySize: merged.PartySize,
		Personnel: bookingReservationPersonnel{
			ContactName:  strings.TrimSpace(merged.Personnel.ContactName),
			ContactPhone: strings.TrimSpace(merged.Personnel.ContactPhone),
			Members:      append([]string(nil), merged.Personnel.Members...),
		},
		SpecialRequirements: strings.TrimSpace(merged.SpecialRequirements),
	})
	if err != nil {
		return nil, err
	}

	bookingStateDraftMu.Lock()
	defer bookingStateDraftMu.Unlock()
	state, err = readBookingDraftState(c.draftsPath)
	if err != nil {
		return nil, err
	}
	index = -1
	for i := range state.Drafts {
		if strings.EqualFold(strings.TrimSpace(state.Drafts[i].ID), draftID) {
			index = i
			break
		}
	}
	if index >= 0 {
		nowText := c.now().Format(time.RFC3339Nano)
		state.Drafts[index].Extracted = merged
		state.Drafts[index].MissingFields = []string{}
		state.Drafts[index].Status = bookingDraftStatusConfirmed
		state.Drafts[index].ConfirmedReservation = reservation.ID
		state.Drafts[index].UpdatedAt = nowText
		_ = writeBookingDraftState(c.draftsPath, state, nowText)
	}

	return map[string]any{
		"draft_id":       draft.ID,
		"reservation_id": reservation.ID,
		"reservation":    reservation,
	}, nil
}

func (c *bookingServiceController) now() time.Time {
	if c == nil || c.nowFn == nil {
		return time.Now()
	}
	return c.nowFn()
}

func mergeBookingDraftForConfirm(base bookingIntentParseExtracted, req bookingIntentConfirmRequest) bookingIntentParseExtracted {
	merged := normalizeBookingIntentExtracted(base)
	if value := strings.TrimSpace(req.ProductID); value != "" {
		merged.ProductID = value
	}
	if value := strings.TrimSpace(req.SlotID); value != "" {
		merged.SlotID = value
	}
	if req.PartySize > 0 {
		merged.PartySize = req.PartySize
	}
	if req.Personnel != nil {
		if value := strings.TrimSpace(req.Personnel.ContactName); value != "" {
			merged.Personnel.ContactName = value
		}
		if value := strings.TrimSpace(req.Personnel.ContactPhone); value != "" {
			merged.Personnel.ContactPhone = value
		}
		if req.Personnel.Members != nil {
			merged.Personnel.Members = append([]string(nil), req.Personnel.Members...)
		}
	}
	if value := strings.TrimSpace(req.SpecialRequirements); value != "" {
		merged.SpecialRequirements = value
	}
	return normalizeBookingIntentExtracted(merged)
}

func normalizeBookingIntentExtracted(value bookingIntentParseExtracted) bookingIntentParseExtracted {
	value.ProductID = strings.TrimSpace(value.ProductID)
	value.ProductName = strings.TrimSpace(value.ProductName)
	value.SlotID = strings.TrimSpace(value.SlotID)
	value.SlotStartAt = strings.TrimSpace(value.SlotStartAt)
	value.Personnel.ContactName = strings.TrimSpace(value.Personnel.ContactName)
	value.Personnel.ContactPhone = strings.TrimSpace(value.Personnel.ContactPhone)
	cleanMembers := make([]string, 0, len(value.Personnel.Members))
	for _, item := range value.Personnel.Members {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		cleanMembers = append(cleanMembers, trimmed)
	}
	value.Personnel.Members = cleanMembers
	value.SpecialRequirements = strings.TrimSpace(value.SpecialRequirements)
	if value.Confidence < 0 {
		value.Confidence = 0
	}
	if value.Confidence > 1 {
		value.Confidence = 1
	}
	return value
}

func bookingIntentMissingFields(extracted bookingIntentParseExtracted) []string {
	missing := make([]string, 0, 6)
	if strings.TrimSpace(extracted.ProductID) == "" {
		missing = append(missing, "product_id")
	}
	if strings.TrimSpace(extracted.SlotID) == "" {
		missing = append(missing, "slot_id")
	}
	if extracted.PartySize <= 0 {
		missing = append(missing, "party_size")
	}
	if strings.TrimSpace(extracted.Personnel.ContactName) == "" {
		missing = append(missing, "personnel.contact_name")
	}
	if strings.TrimSpace(extracted.Personnel.ContactPhone) == "" {
		missing = append(missing, "personnel.contact_phone")
	}
	return missing
}

func enrichBookingIntentCandidates(extracted bookingIntentParseExtracted, ctx bookingIntentParseContext) (bookingIntentParseExtracted, []bookingProduct, []bookingSlotView) {
	productCandidates := make([]bookingProduct, 0)
	slotCandidates := make([]bookingSlotView, 0)
	seenProduct := make(map[string]struct{})
	seenSlot := make(map[string]struct{})
	productID := strings.TrimSpace(extracted.ProductID)
	productName := strings.ToLower(strings.TrimSpace(extracted.ProductName))

	for _, product := range ctx.Products {
		pid := strings.TrimSpace(product.ID)
		if pid == "" {
			continue
		}
		match := false
		if productID != "" && strings.EqualFold(pid, productID) {
			match = true
		}
		if !match && productName != "" {
			name := strings.ToLower(strings.TrimSpace(product.Name))
			if strings.Contains(name, productName) || strings.Contains(productName, name) || strings.Contains(strings.ToLower(pid), productName) {
				match = true
			}
		}
		if !match {
			continue
		}
		key := strings.ToLower(pid)
		if _, exists := seenProduct[key]; exists {
			continue
		}
		seenProduct[key] = struct{}{}
		productCandidates = append(productCandidates, product)
	}
	if productID == "" && len(productCandidates) == 1 {
		extracted.ProductID = strings.TrimSpace(productCandidates[0].ID)
	}

	targetTime := strings.TrimSpace(extracted.SlotStartAt)
	parsedTargetTime := parseBookingTimeOrZero(targetTime)
	slotID := strings.TrimSpace(extracted.SlotID)
	resolvedProductID := strings.TrimSpace(extracted.ProductID)
	for _, slot := range ctx.Slots {
		sid := strings.TrimSpace(slot.ID)
		if sid == "" {
			continue
		}
		match := false
		if slotID != "" && strings.EqualFold(slotID, sid) {
			match = true
		}
		if !match && !parsedTargetTime.IsZero() {
			slotStart := parseBookingTimeOrZero(slot.StartAt)
			if !slotStart.IsZero() && slotStart.Equal(parsedTargetTime) {
				match = true
			}
		}
		if !match {
			continue
		}
		if resolvedProductID != "" && !strings.EqualFold(strings.TrimSpace(slot.ProductID), resolvedProductID) {
			continue
		}
		key := strings.ToLower(sid)
		if _, exists := seenSlot[key]; exists {
			continue
		}
		seenSlot[key] = struct{}{}
		slotCandidates = append(slotCandidates, slot)
	}
	if slotID == "" && len(slotCandidates) == 1 {
		extracted.SlotID = strings.TrimSpace(slotCandidates[0].ID)
	}
	return extracted, productCandidates, slotCandidates
}

func parseBookingIntentWithOpenAI(ctx context.Context, req bookingIntentParseRequest, parseContext bookingIntentParseContext) (bookingIntentParseExtracted, error) {
	apiKey := strings.TrimSpace(parseContext.LLM.APIKey)
	baseURL := strings.TrimSpace(parseContext.LLM.BaseURL)
	model := strings.TrimSpace(parseContext.LLM.Model)
	if apiKey == "" || baseURL == "" {
		return bookingIntentParseExtracted{}, fmt.Errorf("%w: missing api_key or base_url", errBookingLLMNotConfigured)
	}
	if model == "" {
		model = "gpt-4.1-mini"
	}
	apiURL, err := normalizeBookingOpenAIChatCompletionsURL(baseURL)
	if err != nil {
		return bookingIntentParseExtracted{}, fmt.Errorf("%w: invalid base_url: %v", errBookingLLMNotConfigured, err)
	}

	systemPrompt := "You extract booking intent from user text. Return strict JSON with fields: product_id, product_name, slot_id, slot_start_at, party_size, personnel{contact_name,contact_phone,members}, special_requirements, confidence. Use RFC3339 for slot_start_at when possible. Keep unknown fields empty and confidence between 0 and 1."
	catalogPayload := map[string]any{
		"products": parseContext.Products,
		"slots":    parseContext.Slots,
	}
	catalogJSON, _ := marshalJSONNoHTMLEscape(catalogPayload)
	userPrompt := map[string]any{
		"user_id": req.UserID,
		"channel": req.Channel,
		"content": req.Content,
		"catalog": json.RawMessage(catalogJSON),
	}
	userPromptJSON, _ := marshalJSONNoHTMLEscape(userPrompt)

	requestBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": string(userPromptJSON)},
		},
		"temperature":     0,
		"response_format": map[string]any{"type": "json_object"},
	}
	encodedBody, err := marshalJSONNoHTMLEscape(requestBody)
	if err != nil {
		return bookingIntentParseExtracted{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(encodedBody))
	if err != nil {
		return bookingIntentParseExtracted{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return bookingIntentParseExtracted{}, err
	}
	defer httpResp.Body.Close()
	respRaw, err := io.ReadAll(io.LimitReader(httpResp.Body, 2*1024*1024))
	if err != nil {
		return bookingIntentParseExtracted{}, err
	}
	if httpResp.StatusCode != http.StatusOK {
		bookingLogIntentParse("llm_http_error", map[string]any{
			"user_id":   strings.TrimSpace(req.UserID),
			"url":       apiURL,
			"http_code": httpResp.StatusCode,
			"body":      bookingTruncateForLog(strings.TrimSpace(string(respRaw)), 1200),
		})
		return bookingIntentParseExtracted{}, fmt.Errorf("openai parse failed: url=%s http %d: %s", apiURL, httpResp.StatusCode, strings.TrimSpace(string(respRaw)))
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respRaw, &completion); err != nil {
		return bookingIntentParseExtracted{}, err
	}
	if len(completion.Choices) == 0 {
		return bookingIntentParseExtracted{}, fmt.Errorf("openai parse failed: empty choices")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if content == "" {
		return bookingIntentParseExtracted{}, fmt.Errorf("openai parse failed: empty content")
	}
	bookingLogIntentParse("llm_response_raw", map[string]any{
		"user_id": strings.TrimSpace(req.UserID),
		"url":     apiURL,
		"raw":     bookingTruncateForLog(content, 4000),
	})

	parsed, err := parseBookingIntentExtractedFromModelJSON(content)
	if err != nil {
		bookingLogIntentParse("llm_response_parse_error", map[string]any{
			"user_id": strings.TrimSpace(req.UserID),
			"error":   err.Error(),
			"raw":     bookingTruncateForLog(content, 1200),
		})
		return bookingIntentParseExtracted{}, fmt.Errorf("openai parse response is not valid json: %w", err)
	}
	return normalizeBookingIntentExtracted(parsed), nil
}

func parseBookingIntentExtractedFromModelJSON(content string) (bookingIntentParseExtracted, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return bookingIntentParseExtracted{}, err
	}
	parsed := bookingIntentParseExtracted{
		ProductID:           bookingIntentAnyToString(payload["product_id"]),
		ProductName:         bookingIntentAnyToString(payload["product_name"]),
		SlotID:              bookingIntentAnyToString(payload["slot_id"]),
		SlotStartAt:         bookingIntentAnyToString(payload["slot_start_at"]),
		PartySize:           bookingIntentAnyToInt(payload["party_size"]),
		SpecialRequirements: bookingIntentAnyToString(payload["special_requirements"]),
		Confidence:          bookingIntentAnyToFloat(payload["confidence"]),
	}
	if personnelObj, ok := payload["personnel"].(map[string]any); ok {
		parsed.Personnel.ContactName = bookingIntentAnyToString(personnelObj["contact_name"])
		parsed.Personnel.ContactPhone = bookingIntentAnyToString(personnelObj["contact_phone"])
		parsed.Personnel.Members = bookingIntentAnyToStrings(personnelObj["members"])
	}
	return parsed, nil
}

func bookingIntentAnyToString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		if float64(int64(typed)) == typed {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		asFloat := float64(typed)
		if float64(int64(asFloat)) == asFloat {
			return strconv.FormatInt(int64(asFloat), 10)
		}
		return strconv.FormatFloat(asFloat, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint16:
		return strconv.FormatUint(uint64(typed), 10)
	case uint8:
		return strconv.FormatUint(uint64(typed), 10)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func bookingIntentAnyToInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return int(i)
		}
		if f, err := typed.Float64(); err == nil {
			return int(f)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		if i, err := strconv.Atoi(trimmed); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return int(f)
		}
	}
	return 0
}

func bookingIntentAnyToFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
	}
	return 0
}

func bookingIntentAnyToStrings(value any) []string {
	switch typed := value.(type) {
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			text := bookingIntentAnyToString(item)
			if strings.TrimSpace(text) == "" {
				continue
			}
			items = append(items, text)
		}
		return items
	case []string:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			text := strings.TrimSpace(item)
			if text == "" {
				continue
			}
			items = append(items, text)
		}
		return items
	default:
		text := bookingIntentAnyToString(value)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}
}

func bookingLogIntentParse(event string, fields map[string]any) {
	payload := map[string]any{
		"component": "booking_intent_parse",
		"event":     strings.TrimSpace(event),
	}
	for key, value := range fields {
		payload[key] = value
	}
	encoded, err := marshalJSONNoHTMLEscape(payload)
	if err != nil {
		log.Printf(`{"component":"booking_intent_parse","event":"%s","log_error":"%s"}`, strings.TrimSpace(event), bookingTruncateForLog(err.Error(), 200))
		return
	}
	log.Printf("%s", string(encoded))
}

func bookingTruncateForLog(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "...(truncated)"
}
