package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const (
	skillRouterRuntimeStateVersion = 1
	skillRouterRuntimeStateFile    = "skill_router_runtime.json"
	skillRouterHealthPath          = "/healthz"
)

type skillRouterRuntimeState struct {
	Version int                    `json:"version"`
	Runtime skillRouterRuntimeInfo `json:"runtime"`
}

type skillRouterRuntimeInfo struct {
	PID        int    `json:"pid"`
	Endpoint   string `json:"endpoint"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	ScriptPath string `json:"script_path,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
}

type skillRouterStartResult struct {
	Mode        string                  `json:"mode"`
	Status      string                  `json:"status"`
	Message     string                  `json:"message,omitempty"`
	RuntimePath string                  `json:"runtime_path,omitempty"`
	Runtime     *skillRouterRuntimeInfo `json:"runtime,omitempty"`
	StartArgs   []string                `json:"start_args,omitempty"`
}

type skillRouterStatusResult struct {
	Mode    string                  `json:"mode"`
	Status  string                  `json:"status"`
	Running bool                    `json:"running"`
	Runtime *skillRouterRuntimeInfo `json:"runtime,omitempty"`
	Message string                  `json:"message,omitempty"`
}

type skillRouterStopResult struct {
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	PID     int    `json:"pid,omitempty"`
	Message string `json:"message,omitempty"`
}

type skillSidecarRouteRequest struct {
	RequestID       string                       `json:"request_id"`
	Text            string                       `json:"text"`
	Symbols         []string                     `json:"symbols,omitempty"`
	InstalledSkills []skillSidecarInstalledSkill `json:"installed_skills"`
	AvailableSkills []skillSidecarAvailableSkill `json:"available_skills,omitempty"`
	RuntimeContext  map[string]any               `json:"runtime_context,omitempty"`
}

type skillSidecarInstalledSkill struct {
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	Executor       string   `json:"executor,omitempty"`
	CommandHints   []string `json:"command_hints,omitempty"`
	AllowedActions []string `json:"allowed_actions,omitempty"`
}

type skillSidecarAvailableSkill struct {
	ID                     string   `json:"id"`
	Capabilities           []string `json:"capabilities,omitempty"`
	Priority               int      `json:"priority,omitempty"`
	Source                 string   `json:"source,omitempty"`
	SourceURL              string   `json:"source_url,omitempty"`
	InstallCommandTemplate string   `json:"install_command_template,omitempty"`
}

type skillSidecarRouteResponse struct {
	SkillID         string                     `json:"skill_id"`
	Intent          string                     `json:"intent,omitempty"`
	Arguments       map[string]any             `json:"arguments,omitempty"`
	Confidence      float64                    `json:"confidence,omitempty"`
	Actions         []skillAction              `json:"actions,omitempty"`
	Steps           []skillRouteStep           `json:"steps,omitempty"`
	Recommendations []skillRouteRecommendation `json:"recommendations,omitempty"`
	Reason          string                     `json:"reason,omitempty"`
	Trace           map[string]any             `json:"trace,omitempty"`
	Errors          []string                   `json:"errors,omitempty"`
}

type skillCatalogFile struct {
	Skills []skillSidecarAvailableSkill `json:"skills"`
}

var startSkillRouterInBackgroundForRoute = startSkillRouterInBackground

func newSkillRouterCommand(app *AppContext) *cobra.Command {
	var (
		llmConfigPath string
		runtimePath   string
		logPath       string
		pythonBin     string
		timeout       time.Duration
		format        string
	)

	routerCmd := &cobra.Command{
		Use:   "router",
		Short: "Manage the local Skill Router sidecar process",
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the local Skill Router sidecar",
		RunE: func(cmd *cobra.Command, args []string) error {
			app.SetExecution("skill", []string{"router", "start"})
			result, err := startSkillRouterInBackground(resolveSkillsLLMConfigPath(llmConfigPath), runtimePath, logPath, pythonBin)
			app.SetResult(result)
			printSkillRouterStartResult(result, format)
			return err
		},
	}
	startCmd.Flags().StringVar(&llmConfigPath, "llm-config", defaultSkillsLLMConfigPath(), "Path to skills router/LLM config (V2)")
	startCmd.Flags().StringVar(&runtimePath, "runtime", "", "Optional path to router runtime state JSON")
	startCmd.Flags().StringVar(&logPath, "log-file", "", "Optional sidecar log file path")
	startCmd.Flags().StringVar(&pythonBin, "python", "", "Optional Python executable path")
	startCmd.Flags().StringVar(&format, "format", "table", "Output format: table|json")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show Skill Router sidecar status",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedRuntime := resolveSkillRouterRuntimePath(runtimePath, resolveSkillsLLMConfigPath(llmConfigPath))
			app.SetExecution("skill", []string{"router", "status"})
			result, err := skillRouterStatus(resolvedRuntime)
			app.SetResult(result)
			printSkillRouterStatusResult(result, format)
			return err
		},
	}
	statusCmd.Flags().StringVar(&llmConfigPath, "llm-config", defaultSkillsLLMConfigPath(), "Path to skills router/LLM config (V2)")
	statusCmd.Flags().StringVar(&runtimePath, "runtime", "", "Optional path to router runtime state JSON")
	statusCmd.Flags().StringVar(&format, "format", "table", "Output format: table|json")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop Skill Router sidecar",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedRuntime := resolveSkillRouterRuntimePath(runtimePath, resolveSkillsLLMConfigPath(llmConfigPath))
			app.SetExecution("skill", []string{"router", "stop"})
			result, err := stopSkillRouter(resolvedRuntime, timeout)
			app.SetResult(result)
			printSkillRouterStopResult(result, format)
			return err
		},
	}
	stopCmd.Flags().StringVar(&llmConfigPath, "llm-config", defaultSkillsLLMConfigPath(), "Path to skills router/LLM config (V2)")
	stopCmd.Flags().StringVar(&runtimePath, "runtime", "", "Optional path to router runtime state JSON")
	stopCmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "Process stop grace timeout before SIGKILL")
	stopCmd.Flags().StringVar(&format, "format", "table", "Output format: table|json")

	routerCmd.AddCommand(startCmd, statusCmd, stopCmd)
	return routerCmd
}

func routeSkillIntentWithSidecar(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return skillRouteDecision{}, fmt.Errorf("route text is empty")
	}
	cfg, err := loadSkillLLMConfig(llmConfigPath)
	if err != nil {
		return skillRouteDecision{}, err
	}
	ensureResult, ensureErr := ensureSkillRouterSidecarRunning(resolveSkillsLLMConfigPath(llmConfigPath), cfg)
	if ensureErr != nil {
		return skillRouteDecision{}, ensureErr
	}

	request := skillSidecarRouteRequest{
		RequestID:       newSkillTraceID(),
		Text:            trimmedText,
		InstalledSkills: make([]skillSidecarInstalledSkill, 0, len(skills)),
		RuntimeContext: map[string]any{
			"runtime": "longtradego",
			"mode":    "skill_v2",
		},
	}
	for _, skill := range skills {
		request.InstalledSkills = append(request.InstalledSkills, skillSidecarInstalledSkill{
			ID:             skill.ID,
			Description:    skill.Description,
			Executor:       skill.Executor,
			CommandHints:   append([]string(nil), skill.CommandHints...),
			AllowedActions: append([]string(nil), skill.AllowedActions...),
		})
	}
	catalogPath := resolveSkillsCatalogPath(cfg)
	availableSkills, catalogErr := loadSkillCatalogFile(catalogPath)
	if catalogErr == nil && len(availableSkills) > 0 {
		request.AvailableSkills = append([]skillSidecarAvailableSkill(nil), availableSkills...)
	} else if catalogErr != nil {
		request.RuntimeContext["skills_catalog_error"] = catalogErr.Error()
	}

	encoded, err := marshalJSONNoHTMLEscape(request)
	if err != nil {
		return skillRouteDecision{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Router.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return skillRouteDecision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(cfg.Router.AuthToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	timeout := time.Duration(cfg.Router.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultSkillRouterTimeoutSeconds) * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return skillRouteDecision{}, fmt.Errorf("skill router sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return skillRouteDecision{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return skillRouteDecision{}, fmt.Errorf("skill router sidecar http=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var payload skillSidecarRouteResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return skillRouteDecision{}, fmt.Errorf("skill router sidecar response is invalid json: %w", err)
	}
	if len(payload.Errors) > 0 && strings.TrimSpace(payload.SkillID) == "" {
		return skillRouteDecision{}, fmt.Errorf("skill router sidecar returned errors: %s", strings.Join(payload.Errors, "; "))
	}

	decision := skillRouteDecision{
		SkillID:         strings.TrimSpace(payload.SkillID),
		Intent:          strings.TrimSpace(payload.Intent),
		Arguments:       payload.Arguments,
		Confidence:      payload.Confidence,
		Actions:         payload.Actions,
		Steps:           payload.Steps,
		Recommendations: append([]skillRouteRecommendation(nil), payload.Recommendations...),
		Reason:          strings.TrimSpace(payload.Reason),
		Trace:           payload.Trace,
	}
	normalizeSkillRouteDecision(&decision)
	if decision.Trace == nil {
		decision.Trace = map[string]any{}
	}
	if tracePath := strings.TrimSpace(ensureResult.RuntimePath); tracePath != "" {
		decision.Trace["sidecar_runtime_path"] = tracePath
	}
	decision.Trace["sidecar_ensure_status"] = strings.TrimSpace(ensureResult.Status)
	decision.Trace["sidecar_started_by_route"] = strings.EqualFold(strings.TrimSpace(ensureResult.Status), "started")
	if decision.Reason == "" && len(payload.Errors) > 0 {
		decision.Reason = strings.Join(payload.Errors, "; ")
	}
	if strings.TrimSpace(decision.SkillID) == "" && len(decision.Steps) == 0 {
		return skillRouteDecision{}, fmt.Errorf("skill router sidecar returned empty skill_id")
	}
	return decision, nil
}

func ensureSkillRouterSidecarRunning(llmConfigPath string, cfg skillLLMConfig) (skillRouterStartResult, error) {
	if !shouldAutoStartSkillRouterSidecar(cfg.Router.Endpoint, cfg.Sidecar.Port) {
		return skillRouterStartResult{Mode: "start", Status: "skipped"}, nil
	}
	resolvedRuntime := resolveSkillRouterRuntimePath("", llmConfigPath)
	result, err := startSkillRouterInBackgroundForRoute(strings.TrimSpace(llmConfigPath), "", "", "")
	result.RuntimePath = resolvedRuntime
	if err != nil {
		return result, fmt.Errorf("ensure skill router sidecar running failed: %w", err)
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	switch status {
	case "started", "already_running":
		return result, nil
	default:
		return result, fmt.Errorf(
			"ensure skill router sidecar running failed: status=%s message=%s",
			strings.TrimSpace(result.Status),
			strings.TrimSpace(result.Message),
		)
	}
}

func shouldAutoStartSkillRouterSidecar(endpoint string, sidecarPort int) bool {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if !isLocalSkillRouterHost(host) {
		return false
	}
	if sidecarPort <= 0 {
		sidecarPort = defaultSkillRouterPort
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
		case "http":
			return sidecarPort == 80
		case "https":
			return sidecarPort == 443
		default:
			return false
		}
	}
	parsedPort, convErr := strconv.Atoi(port)
	if convErr != nil {
		return false
	}
	return parsedPort == sidecarPort
}

func isLocalSkillRouterHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func normalizeSkillRouterEndpoint(raw string, host string, port int) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.TrimSpace(host) == "" {
		host = defaultSkillRouterHost
	}
	if port <= 0 {
		port = defaultSkillRouterPort
	}
	if trimmed == "" {
		return fmt.Sprintf("http://%s:%d/v1/route", host, port), nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.Scheme) == "" || strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("router endpoint must include scheme and host")
	}
	lowerPath := strings.ToLower(strings.TrimSpace(parsed.Path))
	switch {
	case lowerPath == "", lowerPath == "/":
		parsed.Path = "/v1/route"
	case strings.HasSuffix(lowerPath, "/v1"):
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/route"
	case strings.HasSuffix(lowerPath, "/v1/route"):
		// keep as-is
	default:
		// allow custom paths explicitly configured by user
	}
	return parsed.String(), nil
}

func resolveSkillRouterRuntimePath(runtimePath string, llmConfigPath string) string {
	trimmed := strings.TrimSpace(runtimePath)
	if trimmed != "" {
		return trimmed
	}
	cfg, err := loadSkillLLMConfig(resolveSkillsLLMConfigPath(llmConfigPath))
	if err == nil && strings.TrimSpace(cfg.Sidecar.Runtime) != "" {
		return strings.TrimSpace(cfg.Sidecar.Runtime)
	}
	return defaultSkillRouterRuntimeStatePath()
}

func resolveSkillRouterLogPath(logPath string, cfg skillLLMConfig) string {
	trimmed := strings.TrimSpace(logPath)
	if trimmed != "" {
		return trimmed
	}
	if strings.TrimSpace(cfg.Sidecar.Log) != "" {
		return strings.TrimSpace(cfg.Sidecar.Log)
	}
	return defaultSkillRouterLogPath()
}

func resolveSkillsCatalogPath(cfg skillLLMConfig) string {
	if strings.TrimSpace(cfg.Sidecar.SkillsCatalog) != "" {
		return strings.TrimSpace(cfg.Sidecar.SkillsCatalog)
	}
	return defaultSkillsCatalogPath()
}

func loadSkillCatalogFile(path string) ([]skillSidecarAvailableSkill, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills catalog %s failed: %w", trimmed, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}

	var fromObject skillCatalogFile
	if err := json.Unmarshal(raw, &fromObject); err == nil {
		return normalizeAvailableSkillsCatalog(fromObject.Skills), nil
	}
	var fromArray []skillSidecarAvailableSkill
	if err := json.Unmarshal(raw, &fromArray); err == nil {
		return normalizeAvailableSkillsCatalog(fromArray), nil
	}
	return nil, fmt.Errorf("parse skills catalog %s failed: invalid json schema", trimmed)
}

func normalizeAvailableSkillsCatalog(raw []skillSidecarAvailableSkill) []skillSidecarAvailableSkill {
	if len(raw) == 0 {
		return nil
	}
	out := make([]skillSidecarAvailableSkill, 0, len(raw))
	seen := map[string]struct{}{}
	for _, one := range raw {
		id := strings.TrimSpace(one.ID)
		if id == "" {
			continue
		}
		lowerID := strings.ToLower(id)
		if _, exists := seen[lowerID]; exists {
			continue
		}
		seen[lowerID] = struct{}{}

		capabilities := make([]string, 0, len(one.Capabilities))
		capSeen := map[string]struct{}{}
		for _, capOne := range one.Capabilities {
			capability := strings.ToLower(strings.TrimSpace(capOne))
			if capability == "" {
				continue
			}
			if _, exists := capSeen[capability]; exists {
				continue
			}
			capSeen[capability] = struct{}{}
			capabilities = append(capabilities, capability)
		}
		sort.Strings(capabilities)

		out = append(out, skillSidecarAvailableSkill{
			ID:                     id,
			Capabilities:           capabilities,
			Priority:               one.Priority,
			Source:                 strings.TrimSpace(one.Source),
			SourceURL:              strings.TrimSpace(one.SourceURL),
			InstallCommandTemplate: strings.TrimSpace(one.InstallCommandTemplate),
		})
	}
	return out
}

func resolveSkillRouterScriptPath(cfg skillLLMConfig) string {
	if strings.TrimSpace(cfg.Sidecar.Script) != "" {
		return strings.TrimSpace(cfg.Sidecar.Script)
	}
	return filepath.Join("scripts", "skill_router_sidecar.py")
}

func resolveSkillRouterPythonBin(raw string, cfg skillLLMConfig) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		return trimmed
	}
	if strings.TrimSpace(cfg.Sidecar.PythonBin) != "" {
		return strings.TrimSpace(cfg.Sidecar.PythonBin)
	}
	return "python3"
}

func startSkillRouterInBackground(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
	cfg, err := loadSkillLLMConfig(resolveSkillsLLMConfigPath(llmConfigPath))
	if err != nil {
		return skillRouterStartResult{Mode: "start", Status: "invalid_config", RuntimePath: resolvedRuntimePathForStartResult(runtimePath, llmConfigPath), Message: err.Error()}, err
	}
	resolvedRuntime := resolveSkillRouterRuntimePath(runtimePath, llmConfigPath)
	resolvedLog := resolveSkillRouterLogPath(logPath, cfg)
	resolvedScript := resolveSkillRouterScriptPath(cfg)
	resolvedPython := resolveSkillRouterPythonBin(pythonBin, cfg)
	endpoint := strings.TrimSpace(cfg.Router.Endpoint)

	status, runtime, statusErr := skillRouterRuntimeStatus(resolvedRuntime)
	if statusErr != nil {
		return skillRouterStartResult{Mode: "start", Status: "error", RuntimePath: resolvedRuntime, Message: statusErr.Error()}, statusErr
	}
	if status == "running" && runtime != nil {
		return skillRouterStartResult{
			Mode:        "start",
			Status:      "already_running",
			Message:     "skill router sidecar is already running",
			RuntimePath: resolvedRuntime,
			Runtime:     runtime,
		}, nil
	}
	if status == "stale" {
		_ = removeSkillRouterRuntimeState(resolvedRuntime)
	}

	host := cfg.Sidecar.Host
	if strings.TrimSpace(host) == "" {
		host = defaultSkillRouterHost
	}
	port := cfg.Sidecar.Port
	if port <= 0 {
		port = defaultSkillRouterPort
	}
	configPathForSidecar := strings.TrimSpace(cfg.Sidecar.ConfigPath)
	if configPathForSidecar == "" {
		configPathForSidecar = resolveSkillsLLMConfigPath(llmConfigPath)
	}
	pid, args, spawnErr := spawnSkillRouterBackgroundProcess(resolvedPython, resolvedScript, host, port, configPathForSidecar, resolvedLog)
	if spawnErr != nil {
		return skillRouterStartResult{Mode: "start", Status: "start_failed", RuntimePath: resolvedRuntime, Message: spawnErr.Error()}, spawnErr
	}

	if verifyErr := verifySkillRouterBackgroundStart(endpoint, pid, 5*time.Second); verifyErr != nil {
		_ = killProcessByPID(pid)
		return skillRouterStartResult{Mode: "start", Status: "start_failed", RuntimePath: resolvedRuntime, Message: verifyErr.Error(), StartArgs: args}, verifyErr
	}

	nowText := time.Now().Format(time.RFC3339Nano)
	runtimeInfo := skillRouterRuntimeInfo{
		PID:        pid,
		Endpoint:   endpoint,
		Host:       host,
		Port:       port,
		StartedAt:  nowText,
		UpdatedAt:  nowText,
		ScriptPath: resolvedScript,
		ConfigPath: configPathForSidecar,
		LogPath:    resolvedLog,
	}
	if writeErr := writeSkillRouterRuntimeState(resolvedRuntime, runtimeInfo); writeErr != nil {
		_ = killProcessByPID(pid)
		return skillRouterStartResult{Mode: "start", Status: "start_failed", RuntimePath: resolvedRuntime, Message: writeErr.Error(), StartArgs: args}, writeErr
	}
	return skillRouterStartResult{
		Mode:        "start",
		Status:      "started",
		RuntimePath: resolvedRuntime,
		Runtime:     &runtimeInfo,
		StartArgs:   args,
	}, nil
}

func resolvedRuntimePathForStartResult(runtimePath string, llmConfigPath string) string {
	trimmedRuntime := strings.TrimSpace(runtimePath)
	if trimmedRuntime != "" {
		return trimmedRuntime
	}
	trimmedLLMPath := strings.TrimSpace(llmConfigPath)
	if trimmedLLMPath != "" {
		return resolveSkillRouterRuntimePath("", trimmedLLMPath)
	}
	return defaultSkillRouterRuntimeStatePath()
}

func spawnSkillRouterBackgroundProcess(pythonBin string, scriptPath string, host string, port int, llmConfigPath string, logPath string) (int, []string, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, nil, err
	}
	args := []string{scriptPath, "--host", host, "--port", strconv.Itoa(port), "--config", llmConfigPath}
	child := exec.Command(strings.TrimSpace(pythonBin), args...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Stdin = nil
	child.Env = append(os.Environ(), "LONGTRADEGO_SKILL_ROUTER_LOG="+logPath)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return 0, nil, fmt.Errorf("start skill router sidecar failed: %w", err)
	}
	_ = logFile.Close()
	commandLine := append([]string{strings.TrimSpace(pythonBin)}, args...)
	return child.Process.Pid, commandLine, nil
}

func verifySkillRouterBackgroundStart(routeEndpoint string, pid int, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	healthURL := skillRouterHealthEndpoint(routeEndpoint)
	client := &http.Client{Timeout: 900 * time.Millisecond}

	for time.Now().Before(deadline) {
		running, err := isProcessRunning(pid)
		if err != nil {
			return err
		}
		if !running {
			return fmt.Errorf("skill router sidecar process exited before ready")
		}
		req, _ := http.NewRequest(http.MethodGet, healthURL, nil)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	return fmt.Errorf("skill router sidecar health check timeout: %s", healthURL)
}

func skillRouterHealthEndpoint(routeEndpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(routeEndpoint))
	if err != nil || parsed == nil {
		return "http://127.0.0.1:19090/healthz"
	}
	parsed.Path = skillRouterHealthPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func skillRouterStatus(runtimePath string) (skillRouterStatusResult, error) {
	status, runtime, err := skillRouterRuntimeStatus(runtimePath)
	if err != nil {
		return skillRouterStatusResult{}, err
	}
	result := skillRouterStatusResult{
		Mode:    "status",
		Status:  status,
		Running: status == "running",
		Runtime: runtime,
	}
	if status == "stopped" {
		result.Message = "runtime state not found"
	}
	return result, nil
}

func skillRouterRuntimeStatus(runtimePath string) (string, *skillRouterRuntimeInfo, error) {
	runtime, exists, err := readSkillRouterRuntimeState(runtimePath)
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

func stopSkillRouter(runtimePath string, timeout time.Duration) (skillRouterStopResult, error) {
	runtime, exists, err := readSkillRouterRuntimeState(runtimePath)
	if err != nil {
		return skillRouterStopResult{}, err
	}
	if !exists || runtime == nil {
		return skillRouterStopResult{Mode: "stop", Status: "not_running", Message: "runtime state not found"}, nil
	}
	if runtime.PID <= 0 {
		_ = removeSkillRouterRuntimeState(runtimePath)
		return skillRouterStopResult{Mode: "stop", Status: "stale_removed", Message: "invalid runtime pid removed"}, nil
	}
	running, err := isProcessRunning(runtime.PID)
	if err != nil {
		return skillRouterStopResult{}, err
	}
	if !running {
		_ = removeSkillRouterRuntimeState(runtimePath)
		return skillRouterStopResult{Mode: "stop", Status: "stale_removed", PID: runtime.PID, Message: "stale runtime removed"}, nil
	}
	if err := terminateProcess(runtime.PID, timeout); err != nil {
		return skillRouterStopResult{}, err
	}
	if err := removeSkillRouterRuntimeState(runtimePath); err != nil {
		return skillRouterStopResult{}, err
	}
	return skillRouterStopResult{Mode: "stop", Status: "stopped", PID: runtime.PID}, nil
}

func defaultSkillRouterRuntimeStatePath() string {
	return resolveDataPath(skillRouterRuntimeStateFile)
}

func defaultSkillRouterLogPath() string {
	return resolveLogPath("skill_router.log")
}

func readSkillRouterRuntimeState(path string) (*skillRouterRuntimeInfo, bool, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = defaultSkillRouterRuntimeStatePath()
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, false, nil
	}
	var state skillRouterRuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, false, err
	}
	runtime := state.Runtime
	if runtime.PID <= 0 && strings.TrimSpace(runtime.Endpoint) == "" {
		return nil, false, nil
	}
	return &runtime, true, nil
}

func writeSkillRouterRuntimeState(path string, runtime skillRouterRuntimeInfo) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = defaultSkillRouterRuntimeStatePath()
	}
	if err := os.MkdirAll(filepath.Dir(trimmed), 0o755); err != nil {
		return err
	}
	state := skillRouterRuntimeState{
		Version: skillRouterRuntimeStateVersion,
		Runtime: runtime,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(trimmed, append(data, '\n'), 0o644)
}

func removeSkillRouterRuntimeState(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = defaultSkillRouterRuntimeStatePath()
	}
	err := os.Remove(trimmed)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

func printSkillRouterStartResult(result skillRouterStartResult, format string) {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return
	}
	if result.Runtime != nil {
		fmt.Printf("skill router start: status=%s pid=%d endpoint=%s\n", result.Status, result.Runtime.PID, result.Runtime.Endpoint)
		return
	}
	fmt.Printf("skill router start: status=%s message=%s\n", result.Status, strings.TrimSpace(result.Message))
}

func printSkillRouterStatusResult(result skillRouterStatusResult, format string) {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return
	}
	if result.Runtime != nil {
		fmt.Printf("skill router status=%s running=%t pid=%d endpoint=%s\n", result.Status, result.Running, result.Runtime.PID, result.Runtime.Endpoint)
		return
	}
	fmt.Printf("skill router status=%s running=%t\n", result.Status, result.Running)
}

func printSkillRouterStopResult(result skillRouterStopResult, format string) {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return
	}
	if result.PID > 0 {
		fmt.Printf("skill router stop: status=%s pid=%d\n", result.Status, result.PID)
		return
	}
	fmt.Printf("skill router stop: status=%s message=%s\n", result.Status, strings.TrimSpace(result.Message))
}
