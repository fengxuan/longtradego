package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	daemonAdminRuntimeStateVersion = 1
	daemonAdminRuntimeStateFile    = "admin_runtime.json"
	daemonAdminAuthConfigFile      = "admin_auth.json"

	defaultDaemonAdminAddr         = ":18080"
	defaultDaemonAdminPortFallback = 20

	daemonAdminHomePath                 = "/admin"
	daemonAdminHomeSlash                = "/admin/"
	daemonAdminStatusPath               = "/admin/status"
	daemonAdminWebhookStartPath         = "/admin/webhook/start"
	daemonAdminWebhookStopPath          = "/admin/webhook/stop"
	daemonAdminWebhookKillPortPath      = "/admin/webhook/kill-port"
	daemonAdminBookingServiceStatusPath = "/admin/booking/service/status"
	daemonAdminBookingServiceStartPath  = "/admin/booking/service/start"
	daemonAdminBookingServiceStopPath   = "/admin/booking/service/stop"
	daemonAdminTaskGlobalPausePath      = "/admin/task/global-pause"
	daemonAdminTaskGlobalResumePath     = "/admin/task/global-resume"
	daemonAdminTaskPausePath            = "/admin/task/pause"
	daemonAdminTaskResumePath           = "/admin/task/resume"
	daemonAdminMonitorStartPath         = "/admin/monitor/start"
	daemonAdminMonitorStopPath          = "/admin/monitor/stop"
	daemonAdminMonitorStartAllPath      = "/admin/monitor/start-all"
	daemonAdminMonitorStopAllPath       = "/admin/monitor/stop-all"
)

type daemonAdminRuntimeState struct {
	Version int                    `json:"version"`
	Runtime daemonAdminRuntimeInfo `json:"runtime"`
}

type daemonAdminRuntimeInfo struct {
	PID       int    `json:"pid"`
	Address   string `json:"address"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type daemonAdminAuthConfig struct {
	Username     string `json:"username"`
	Password     string `json:"password,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
}

type daemonAdminServiceOptions struct {
	PreferredAddr       string
	MaxPortFallback     int
	RuntimePath         string
	AuthConfigPath      string
	DaemonPID           int
	DaemonSessionID     string
	DaemonStartedAt     time.Time
	Out                 io.Writer
	TaskManager         *daemonTaskManager
	MonitorMu           *sync.Mutex
	Monitors            map[int]*daemonMonitorRuntime
	MonitorRecords      map[string]daemonMonitorRecord
	SaveMonitorState    func() error
	StartMonitorByID    func(string) error
	StartAllMonitors    func() (int, error)
	WebhookOwnedMu      *sync.Mutex
	WebhookOwnedPIDs    map[int]daemonOwnedWebhookRuntime
	BookingOwnedMu      *sync.Mutex
	BookingOwnedPIDs    map[int]daemonOwnedBookingRuntime
	WebhookRuntime      string
	WebhookLogPath      string
	WebhookStartFn      func() (webhookStartResult, error)
	WebhookStopFn       func() (webhookStopResult, error)
	WebhookKillFn       func() (webhookKillPortResult, error)
	BookingStartFn      func() (bookingServiceStartResult, error)
	BookingStopFn       func() (bookingServiceStopResult, error)
	BookingStatusFn     func() (bookingServiceStatusResult, error)
	BookingRuntime      string
	BookingLogPath      string
	BookingAddr         string
	BookingAdminAddr    string
	BookingCatalog      string
	BookingReservations string
	BookingDrafts       string
	BookingAPIKeys      string
	BookingLLMConfig    string
	BookingAdminAuth    string
	AuthConfig          daemonAdminAuthConfig
}

type daemonAdminService struct {
	server      *http.Server
	listener    net.Listener
	runtimePath string
	address     string
	out         io.Writer
	stopOnce    sync.Once
}

type daemonAdminStatusResponse struct {
	Name    string                          `json:"name"`
	Now     string                          `json:"now"`
	Daemon  daemonAdminDaemonStatus         `json:"daemon"`
	Admin   daemonAdminServiceStatus        `json:"admin"`
	Webhook daemonAdminWebhookStatus        `json:"webhook"`
	Booking daemonAdminBookingServiceStatus `json:"booking"`
	Task    daemonAdminTaskStatus           `json:"task"`
	Monitor daemonAdminMonitorStatus        `json:"monitor"`
}

type daemonAdminDaemonStatus struct {
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	StartedAt string `json:"started_at"`
}

type daemonAdminServiceStatus struct {
	Address string `json:"address"`
	URL     string `json:"url"`
}

type daemonAdminWebhookStatus struct {
	Status        string                  `json:"status"`
	Running       bool                    `json:"running"`
	Address       string                  `json:"address,omitempty"`
	PublicAddress string                  `json:"public_address,omitempty"`
	AdminAddress  string                  `json:"admin_address,omitempty"`
	Path          string                  `json:"path,omitempty"`
	RouteCount    int                     `json:"route_count,omitempty"`
	Routes        []webhookRouteSummary   `json:"routes,omitempty"`
	Dispatch      webhookDispatchStats    `json:"dispatch"`
	Metrics       *webhookMetricsSnapshot `json:"metrics,omitempty"`
	LogQueue      *webhookLogQueueDepth   `json:"log_queue_depth,omitempty"`
	Message       string                  `json:"message,omitempty"`
}

type daemonAdminBookingServiceStatus struct {
	Status        string                     `json:"status"`
	Running       bool                       `json:"running"`
	PID           int                        `json:"pid,omitempty"`
	Address       string                     `json:"address,omitempty"`
	PublicAddress string                     `json:"public_address,omitempty"`
	AdminAddress  string                     `json:"admin_address,omitempty"`
	URL           string                     `json:"url,omitempty"`
	PublicURL     string                     `json:"public_url,omitempty"`
	AdminURL      string                     `json:"admin_url,omitempty"`
	Message       string                     `json:"message,omitempty"`
	Runtime       *bookingServiceRuntimeInfo `json:"runtime,omitempty"`
}

type daemonAdminTaskStatus struct {
	GlobalPaused bool                  `json:"global_paused"`
	Total        int                   `json:"total"`
	Running      int                   `json:"running"`
	Paused       int                   `json:"paused"`
	Scheduled    int                   `json:"scheduled"`
	Tasks        []daemonAdminTaskView `json:"tasks"`
}

type daemonAdminTaskView struct {
	daemonTaskSnapshot

	LastStatusCompat string `json:"last_status,omitempty"`
	LastErrorCompat  string `json:"last_error,omitempty"`
	PausedCompat     bool   `json:"paused"`
	RunningCompat    bool   `json:"running"`
	State            string `json:"state"`
	StateText        string `json:"state_text"`
	LastResult       string `json:"last_result"`
}

type daemonAdminMonitorStatus struct {
	Total     int                     `json:"total"`
	Running   int                     `json:"running"`
	Paused    int                     `json:"paused"`
	Scheduled int                     `json:"scheduled"`
	Monitors  []daemonMonitorSnapshot `json:"monitors"`
}

type daemonAdminHomeView struct {
	Status daemonAdminStatusResponse
}

type daemonAdminController struct {
	daemonPID           int
	daemonSessionID     string
	daemonStartedAt     time.Time
	adminAddress        string
	taskManager         *daemonTaskManager
	monitorMu           *sync.Mutex
	monitors            map[int]*daemonMonitorRuntime
	monitorRecords      map[string]daemonMonitorRecord
	saveMonitorState    func() error
	startMonitorByID    func(string) error
	startAllMonitors    func() (int, error)
	webhookOwnedMu      *sync.Mutex
	webhookOwnedPIDs    map[int]daemonOwnedWebhookRuntime
	bookingOwnedMu      *sync.Mutex
	bookingOwnedPIDs    map[int]daemonOwnedBookingRuntime
	webhookRuntime      string
	webhookLogPath      string
	webhookStartFn      func() (webhookStartResult, error)
	webhookStopFn       func() (webhookStopResult, error)
	webhookKillFn       func() (webhookKillPortResult, error)
	bookingStartFn      func() (bookingServiceStartResult, error)
	bookingStopFn       func() (bookingServiceStopResult, error)
	bookingStatusFn     func() (bookingServiceStatusResult, error)
	bookingRuntime      string
	bookingLogPath      string
	bookingAddr         string
	bookingAdminAddr    string
	bookingCatalog      string
	bookingReservations string
	bookingDrafts       string
	bookingAPIKeys      string
	bookingLLMConfig    string
	bookingAdminAuth    string
	authConfigPath      string
	authConfig          daemonAdminAuthConfig
}

var daemonAdminHomeTemplate = template.Must(template.New("daemon_admin_home").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Daemon Admin</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f4f7fb;
      --panel: #ffffff;
      --text: #111827;
      --muted: #6b7280;
      --accent: #0369a1;
      --border: #d1d5db;
      --warn: #9a3412;
      --warn-bg: #fff7ed;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 20px;
      background: var(--bg);
      color: var(--text);
      font-family: "IBM Plex Sans", "Avenir Next", "Segoe UI", sans-serif;
    }
    .container { max-width: 1180px; margin: 0 auto; display: grid; gap: 16px; }
    .panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 14px;
    }
    h1, h2 { margin: 0; }
    h1 { font-size: 1.45rem; }
    h2 { font-size: 1.05rem; margin-bottom: 8px; }
    .meta { color: var(--muted); font-size: 0.88rem; margin-top: 6px; }
    .toolbar { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 10px; }
    button {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: #fff;
      color: var(--text);
      padding: 7px 10px;
      cursor: pointer;
      font-size: 0.9rem;
    }
    button:hover { border-color: #9ca3af; }
    button.danger {
      border-color: #b91c1c;
      background: #b91c1c;
      color: #fff;
    }
    button.danger:hover {
      border-color: #991b1b;
      background: #991b1b;
    }
    .status-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 8px;
      margin-top: 10px;
    }
    .kv { border: 1px solid var(--border); border-radius: 8px; padding: 8px; background: #fafafa; }
    .kv .k { font-size: 0.76rem; text-transform: uppercase; color: var(--muted); }
    .kv .v { font-weight: 600; margin-top: 2px; }
    table {
      width: 100%;
      border-collapse: collapse;
      margin-top: 8px;
      font-size: 0.9rem;
    }
    th, td {
      border-bottom: 1px solid var(--border);
      text-align: left;
      padding: 7px 6px;
      vertical-align: top;
    }
    th {
      font-size: 0.76rem;
      text-transform: uppercase;
      color: var(--muted);
    }
    code {
      font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
      white-space: pre-wrap;
      word-break: break-word;
    }
    .hint { font-size: 0.84rem; color: var(--muted); margin-top: 8px; }
    .warn {
      color: var(--warn);
      background: var(--warn-bg);
      border: 1px solid #fed7aa;
      border-radius: 8px;
      padding: 8px;
      margin-top: 10px;
      font-size: 0.86rem;
    }
    .op-cell { min-width: 160px; }
    .op-row { display: flex; gap: 6px; flex-wrap: wrap; }
    .feedback {
      margin-top: 8px;
      font-size: 0.86rem;
      min-height: 18px;
      color: var(--muted);
    }
    .feedback.error {
      color: #b91c1c;
      font-weight: 600;
    }
    .feedback.success {
      color: #065f46;
    }
  </style>
</head>
<body>
  <div class="container">
    <section class="panel">
      <h1>Daemon Admin Home</h1>
      <div class="meta">Updated: <code>{{.Status.Now}}</code> | Daemon PID: <code>{{.Status.Daemon.PID}}</code> | Session: <code>{{.Status.Daemon.SessionID}}</code></div>
	      <div class="toolbar">
	        <button type="button" onclick="window.location.reload()">Refresh Page</button>
	      </div>
      <div id="action-feedback" class="feedback" aria-live="polite"></div>
      <div class="hint">Status API: <code>` + daemonAdminStatusPath + `</code></div>
    </section>

    <section class="panel">
      <h2>Daemon / Admin Summary</h2>
      <div class="status-grid">
        <div class="kv"><div class="k">Daemon Start</div><div class="v"><code>{{.Status.Daemon.StartedAt}}</code></div></div>
        <div class="kv"><div class="k">Admin Address</div><div class="v"><code>{{.Status.Admin.Address}}</code></div></div>
        <div class="kv"><div class="k">Admin URL</div><div class="v"><code>{{.Status.Admin.URL}}</code></div></div>
        <div class="kv"><div class="k">Webhook Status</div><div class="v"><code>{{.Status.Webhook.Status}}</code></div></div>
        <div class="kv"><div class="k">Booking Status</div><div class="v"><code>{{.Status.Booking.Status}}</code></div></div>
      </div>
    </section>

    <section class="panel">
      <h2>Webhook</h2>
      <div class="toolbar">
        <button type="button" onclick="postAction('` + daemonAdminWebhookStartPath + `', '', '', false, 'webhook-feedback')">Start Webhook</button>
        <button class="danger" type="button" onclick="postAction('` + daemonAdminWebhookStopPath + `', '', 'Stop current webhook process now?', true, 'webhook-feedback')">Stop Webhook</button>
      </div>
      <div id="webhook-feedback" class="feedback" aria-live="polite"></div>
      <div class="status-grid">
        <div class="kv"><div class="k">Running</div><div class="v">{{.Status.Webhook.Running}}</div></div>
        <div class="kv"><div class="k">Public Address</div><div class="v"><code>{{.Status.Webhook.PublicAddress}}</code></div></div>
        <div class="kv"><div class="k">Admin Address</div><div class="v"><code>{{.Status.Webhook.AdminAddress}}</code></div></div>
        <div class="kv"><div class="k">Path</div><div class="v"><code>{{.Status.Webhook.Path}}</code></div></div>
        <div class="kv"><div class="k">Route Count</div><div class="v">{{.Status.Webhook.RouteCount}}</div></div>
      </div>
      {{if .Status.Webhook.Message}}<div class="hint">Message: <code>{{.Status.Webhook.Message}}</code></div>{{end}}
      {{if .Status.Webhook.Routes}}
      <table>
        <thead><tr><th>ID</th><th>Path</th><th>Mode</th><th>Enabled</th><th>Pipeline</th></tr></thead>
        <tbody>
          {{range .Status.Webhook.Routes}}
          <tr>
            <td><code>{{.ID}}</code></td>
            <td><code>{{.Path}}</code></td>
            <td>{{.Mode}}</td>
            <td>{{.Enabled}}</td>
            <td><code>{{if .Pipeline}}{{.Pipeline}}{{else}}-{{end}}</code></td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{end}}
    </section>

    <section class="panel">
      <h2>Booking Service</h2>
      <div class="toolbar">
        <button type="button" onclick="postAction('` + daemonAdminBookingServiceStartPath + `', '', '', false, 'booking-feedback')">Start Booking Service</button>
        <button class="danger" type="button" onclick="postAction('` + daemonAdminBookingServiceStopPath + `', '', 'Stop current booking service now?', true, 'booking-feedback')">Stop Booking Service</button>
      </div>
      <div id="booking-feedback" class="feedback" aria-live="polite"></div>
      <div class="status-grid">
        <div class="kv"><div class="k">Running</div><div class="v">{{.Status.Booking.Running}}</div></div>
        <div class="kv"><div class="k">Status</div><div class="v"><code>{{.Status.Booking.Status}}</code></div></div>
        <div class="kv"><div class="k">PID</div><div class="v">{{.Status.Booking.PID}}</div></div>
        <div class="kv"><div class="k">Public Address</div><div class="v"><code>{{.Status.Booking.PublicAddress}}</code></div></div>
        <div class="kv"><div class="k">Admin Address</div><div class="v"><code>{{.Status.Booking.AdminAddress}}</code></div></div>
        <div class="kv"><div class="k">Public URL</div><div class="v"><code>{{.Status.Booking.PublicURL}}</code></div></div>
        <div class="kv"><div class="k">Admin URL</div><div class="v"><code>{{.Status.Booking.AdminURL}}</code></div></div>
      </div>
      {{if .Status.Booking.Message}}<div class="hint">Message: <code>{{.Status.Booking.Message}}</code></div>{{end}}
      <div class="hint">Service status API: <code>` + daemonAdminBookingServiceStatusPath + `</code></div>
    </section>

	    <section class="panel">
	      <h2>Tasks</h2>
	      <div class="toolbar">
	        <button type="button" onclick="postAction('` + daemonAdminTaskGlobalPausePath + `', '', 'Pause all tasks globally?', true)">Global Pause</button>
	        <button type="button" onclick="postAction('` + daemonAdminTaskGlobalResumePath + `', '', false, false)">Global Resume</button>
	      </div>
	      <div class="status-grid">
	        <div class="kv"><div class="k">Global Paused</div><div class="v">{{.Status.Task.GlobalPaused}}</div></div>
	        <div class="kv"><div class="k">Total</div><div class="v">{{.Status.Task.Total}}</div></div>
	        <div class="kv"><div class="k">Running</div><div class="v">{{.Status.Task.Running}}</div></div>
	        <div class="kv"><div class="k">Paused</div><div class="v">{{.Status.Task.Paused}}</div></div>
	        <div class="kv"><div class="k">Scheduled</div><div class="v">{{.Status.Task.Scheduled}}</div></div>
	      </div>
	      {{if .Status.Task.Tasks}}
	      <table>
	        <thead><tr><th>ID</th><th>Schedule</th><th>Next</th><th>Last</th><th>Runs</th><th>State</th><th>Last Result</th><th>Command</th><th>Operation</th></tr></thead>
	        <tbody>
	          {{range .Status.Task.Tasks}}
	          <tr>
	            <td><code>{{.ID}}</code></td>
	            <td><code>{{.Schedule}}</code></td>
	            <td><code>{{.NextRunAt}}</code></td>
	            <td><code>{{.LastRunAt}}</code></td>
	            <td>{{.RunCount}}</td>
	            <td>{{.StateText}}</td>
	            <td>{{.LastResult}}</td>
	            <td><code>{{.CommandLine}}</code></td>
	            <td class="op-cell"><div class="op-row"><button type="button" onclick="postAction('` + daemonAdminTaskPausePath + `', '{{.ID}}', '', false)">Pause</button><button type="button" onclick="postAction('` + daemonAdminTaskResumePath + `', '{{.ID}}', '', false)">Resume</button></div></td>
	          </tr>
	          {{end}}
	        </tbody>
      </table>
      {{else}}
      <div class="hint">No task configured.</div>
      {{end}}
    </section>

    <section class="panel">
      <h2>Monitors</h2>
      <div class="toolbar">
        <button type="button" onclick="postAction('` + daemonAdminMonitorStartAllPath + `', '', '', false)">Start All Paused</button>
        <button type="button" onclick="postAction('` + daemonAdminMonitorStopAllPath + `', '', 'Stop all running monitors?', true)">Stop All Running</button>
      </div>
      <div class="status-grid">
        <div class="kv"><div class="k">Total</div><div class="v">{{.Status.Monitor.Total}}</div></div>
        <div class="kv"><div class="k">Running</div><div class="v">{{.Status.Monitor.Running}}</div></div>
        <div class="kv"><div class="k">Paused</div><div class="v">{{.Status.Monitor.Paused}}</div></div>
        <div class="kv"><div class="k">Scheduled</div><div class="v">{{.Status.Monitor.Scheduled}}</div></div>
      </div>
      {{if .Status.Monitor.Monitors}}
      <table>
        <thead><tr><th>Monitor ID</th><th>Status</th><th>Created</th><th>Started</th><th>Command</th><th>Operation</th></tr></thead>
        <tbody>
          {{range .Status.Monitor.Monitors}}
          <tr>
            <td><code>{{if .MonitorID}}{{.MonitorID}}{{else}}-{{end}}</code></td>
            <td>{{.Status}}</td>
            <td><code>{{if .CreatedAt}}{{.CreatedAt}}{{else}}-{{end}}</code></td>
            <td><code>{{if .StartedAt}}{{.StartedAt}}{{else}}-{{end}}</code></td>
            <td><code>{{.CommandLine}}</code></td>
            <td class="op-cell"><div class="op-row">{{if .MonitorID}}<button type="button" onclick="postAction('` + daemonAdminMonitorStartPath + `', '{{.MonitorID}}', '', false)">Start</button><button type="button" onclick="postAction('` + daemonAdminMonitorStopPath + `', '{{.MonitorID}}', '', false)">Stop</button>{{else}}-{{end}}</div></td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">No monitor configured.</div>
      {{end}}
    </section>
  </div>

  <script>
    function updateFeedback(node, message, state) {
      if (!node) {
        return;
      }
      node.textContent = String(message || "");
      node.classList.remove("error", "success");
      if (state === "error") {
        node.classList.add("error");
      } else if (state === "success") {
        node.classList.add("success");
      }
    }

    async function postAction(path, id, confirmText, risky, feedbackTargetId) {
      if (risky && confirmText && !window.confirm(confirmText)) {
        return;
      }
      const globalFeedback = document.getElementById("action-feedback");
      const targetFeedback = feedbackTargetId ? document.getElementById(feedbackTargetId) : null;
      updateFeedback(globalFeedback, "Submitting action...", "");
      updateFeedback(targetFeedback, "Submitting action...", "");
      try {
        const body = new URLSearchParams();
        if (id) {
          body.set("id", id);
        }
        const response = await fetch(path, {
          method: "POST",
          headers: {"Content-Type": "application/x-www-form-urlencoded"},
          body: body.toString(),
        });
        const payload = await response.json().catch(function() { return {}; });
        if (!response.ok) {
          const message = payload && payload.message ? String(payload.message) : ("request failed: HTTP " + response.status);
          updateFeedback(globalFeedback, message, "error");
          updateFeedback(targetFeedback, message, "error");
          return;
        }
        const message = payload && payload.message ? String(payload.message) : "ok";
        updateFeedback(globalFeedback, message, "success");
        updateFeedback(targetFeedback, message, "success");
        window.setTimeout(function() { window.location.reload(); }, 400);
      } catch (err) {
        const message = "request failed: " + String(err);
        updateFeedback(globalFeedback, message, "error");
        updateFeedback(targetFeedback, message, "error");
      }
    }
  </script>
</body>
</html>
`))

func defaultDaemonAdminRuntimePath() string {
	return filepath.Join(daemonConfigDir, daemonAdminRuntimeStateFile)
}

func defaultDaemonAdminAuthConfigPath() string {
	return filepath.Join(daemonConfigDir, daemonAdminAuthConfigFile)
}

func defaultDaemonAdminURL(address string) string {
	return buildWebhookManagementURL(address, daemonAdminHomePath)
}

func loadDaemonAdminAuthConfig(path string) (daemonAdminAuthConfig, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return daemonAdminAuthConfig{}, fmt.Errorf("admin auth config path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		return daemonAdminAuthConfig{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return daemonAdminAuthConfig{}, fmt.Errorf("admin auth config %s is empty", trimmedPath)
	}
	var cfg daemonAdminAuthConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return daemonAdminAuthConfig{}, fmt.Errorf("parse admin auth config %s: %w", trimmedPath, err)
	}
	cfg = normalizeDaemonAdminAuthConfig(cfg)
	if err := validateDaemonAdminAuthConfig(cfg); err != nil {
		return daemonAdminAuthConfig{}, fmt.Errorf("invalid admin auth config %s: %w", trimmedPath, err)
	}
	return cfg, nil
}

func normalizeDaemonAdminAuthConfig(cfg daemonAdminAuthConfig) daemonAdminAuthConfig {
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.Password = strings.TrimSpace(cfg.Password)
	cfg.PasswordHash = strings.TrimSpace(cfg.PasswordHash)
	return cfg
}

func validateDaemonAdminAuthConfig(cfg daemonAdminAuthConfig) error {
	normalized := normalizeDaemonAdminAuthConfig(cfg)
	if normalized.Username == "" {
		return fmt.Errorf("username is required")
	}
	if normalized.PasswordHash != "" {
		if _, err := bcrypt.Cost([]byte(normalized.PasswordHash)); err != nil {
			return fmt.Errorf("password_hash is invalid: %w", err)
		}
		return nil
	}
	if normalized.Password == "" {
		return fmt.Errorf("password or password_hash is required")
	}
	return nil
}

func writeDaemonAdminAuthConfig(path string, cfg daemonAdminAuthConfig) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("admin auth config path is empty")
	}
	normalized := normalizeDaemonAdminAuthConfig(cfg)
	if err := validateDaemonAdminAuthConfig(normalized); err != nil {
		return err
	}
	if normalized.PasswordHash != "" {
		normalized.Password = ""
	}
	encoded, err := json.MarshalIndent(normalized, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	return writeFileAtomic(trimmedPath, append(encoded, '\n'), 0o644)
}

func daemonAdminRuntimeStatus(runtimePath string) (string, *daemonAdminRuntimeInfo, error) {
	runtime, exists, err := readDaemonAdminRuntimeState(runtimePath)
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

func readDaemonAdminRuntimeState(path string) (*daemonAdminRuntimeInfo, bool, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, false, fmt.Errorf("admin runtime path is empty")
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
	var state daemonAdminRuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, false, err
	}
	if state.Runtime.PID == 0 && strings.TrimSpace(state.Runtime.Address) == "" {
		return nil, false, nil
	}
	return &state.Runtime, true, nil
}

func writeDaemonAdminRuntimeState(path string, runtime daemonAdminRuntimeInfo) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("admin runtime path is empty")
	}
	if strings.TrimSpace(runtime.UpdatedAt) == "" {
		runtime.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	}
	state := daemonAdminRuntimeState{
		Version: daemonAdminRuntimeStateVersion,
		Runtime: runtime,
	}
	data, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	return writeFileAtomic(trimmedPath, append(data, '\n'), 0o644)
}

func removeDaemonAdminRuntimeState(path string) error {
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

func cleanupDaemonAdminRuntimeStateForCurrentProcess(runtimePath string) (bool, error) {
	trimmedPath := strings.TrimSpace(runtimePath)
	if trimmedPath == "" {
		return false, nil
	}
	runtime, exists, err := readDaemonAdminRuntimeState(trimmedPath)
	if err != nil {
		return false, err
	}
	if !exists || runtime == nil {
		return false, nil
	}
	if runtime.PID != os.Getpid() {
		return false, nil
	}
	if err := removeDaemonAdminRuntimeState(trimmedPath); err != nil {
		return false, err
	}
	return true, nil
}

func startDaemonAdminService(options daemonAdminServiceOptions) (*daemonAdminService, error) {
	preferredAddr := strings.TrimSpace(options.PreferredAddr)
	if preferredAddr == "" {
		preferredAddr = defaultDaemonAdminAddr
	}
	maxFallback := options.MaxPortFallback
	if maxFallback < 0 {
		maxFallback = defaultDaemonAdminPortFallback
	}
	runtimePath := strings.TrimSpace(options.RuntimePath)
	if runtimePath == "" {
		runtimePath = defaultDaemonAdminRuntimePath()
	}

	controller := &daemonAdminController{
		daemonPID:           options.DaemonPID,
		daemonSessionID:     strings.TrimSpace(options.DaemonSessionID),
		daemonStartedAt:     options.DaemonStartedAt,
		taskManager:         options.TaskManager,
		monitorMu:           options.MonitorMu,
		monitors:            options.Monitors,
		monitorRecords:      options.MonitorRecords,
		saveMonitorState:    options.SaveMonitorState,
		startMonitorByID:    options.StartMonitorByID,
		startAllMonitors:    options.StartAllMonitors,
		webhookOwnedMu:      options.WebhookOwnedMu,
		webhookOwnedPIDs:    options.WebhookOwnedPIDs,
		bookingOwnedMu:      options.BookingOwnedMu,
		bookingOwnedPIDs:    options.BookingOwnedPIDs,
		webhookRuntime:      strings.TrimSpace(options.WebhookRuntime),
		webhookLogPath:      strings.TrimSpace(options.WebhookLogPath),
		webhookStartFn:      options.WebhookStartFn,
		webhookStopFn:       options.WebhookStopFn,
		webhookKillFn:       options.WebhookKillFn,
		bookingStartFn:      options.BookingStartFn,
		bookingStopFn:       options.BookingStopFn,
		bookingStatusFn:     options.BookingStatusFn,
		bookingRuntime:      strings.TrimSpace(options.BookingRuntime),
		bookingLogPath:      strings.TrimSpace(options.BookingLogPath),
		bookingAddr:         strings.TrimSpace(options.BookingAddr),
		bookingAdminAddr:    strings.TrimSpace(options.BookingAdminAddr),
		bookingCatalog:      strings.TrimSpace(options.BookingCatalog),
		bookingReservations: strings.TrimSpace(options.BookingReservations),
		bookingDrafts:       strings.TrimSpace(options.BookingDrafts),
		bookingAPIKeys:      strings.TrimSpace(options.BookingAPIKeys),
		bookingLLMConfig:    strings.TrimSpace(options.BookingLLMConfig),
		bookingAdminAuth:    strings.TrimSpace(options.BookingAdminAuth),
		authConfigPath:      strings.TrimSpace(options.AuthConfigPath),
		authConfig:          normalizeDaemonAdminAuthConfig(options.AuthConfig),
	}
	if controller.daemonPID <= 0 {
		controller.daemonPID = os.Getpid()
	}
	if controller.daemonStartedAt.IsZero() {
		controller.daemonStartedAt = time.Now()
	}
	if controller.webhookRuntime == "" {
		controller.webhookRuntime = defaultWebhookRuntimeStatePath()
	}
	if controller.webhookLogPath == "" {
		controller.webhookLogPath = defaultWebhookServerLogPath()
	}
	if controller.bookingRuntime == "" {
		controller.bookingRuntime = defaultBookingRuntimeStatePath()
	}
	if controller.bookingLogPath == "" {
		controller.bookingLogPath = defaultBookingServiceLogPath()
	}
	if controller.bookingAddr == "" {
		controller.bookingAddr = defaultBookingServiceAddr
	}
	if controller.bookingAdminAddr == "" {
		controller.bookingAdminAddr = defaultBookingServiceAdminAddr
	}
	if controller.bookingCatalog == "" {
		controller.bookingCatalog = defaultBookingCatalogStatePath()
	}
	if controller.bookingReservations == "" {
		controller.bookingReservations = defaultBookingReservationsStatePath()
	}
	if controller.bookingDrafts == "" {
		controller.bookingDrafts = defaultBookingIntakeDraftsPath()
	}
	if controller.bookingAPIKeys == "" {
		controller.bookingAPIKeys = defaultBookingAPIKeysConfigPath()
	}
	if controller.bookingLLMConfig == "" {
		controller.bookingLLMConfig = defaultBookingLLMConfigPath()
	}
	if controller.bookingAdminAuth == "" {
		controller.bookingAdminAuth = defaultDaemonAdminAuthConfigPath()
	}
	if err := validateDaemonAdminAuthConfig(controller.authConfig); err != nil {
		return nil, fmt.Errorf("daemon admin auth config invalid: %w", err)
	}

	var lastErr error
	for offset := 0; offset <= maxFallback; offset++ {
		candidateAddr, err := daemonAdminAddrWithPortOffset(preferredAddr, offset)
		if err != nil {
			return nil, err
		}
		listener, err := net.Listen("tcp", candidateAddr)
		if err != nil {
			if isDaemonAdminAddrAlreadyInUse(err) {
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("daemon admin listen on %s failed: %w", candidateAddr, err)
		}

		controller.adminAddress = candidateAddr
		mux := http.NewServeMux()
		controller.registerHandlers(mux)
		server := &http.Server{
			Addr:              candidateAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		runtime := daemonAdminRuntimeInfo{
			PID:       controller.daemonPID,
			Address:   candidateAddr,
			StartedAt: time.Now().Format(time.RFC3339Nano),
			UpdatedAt: time.Now().Format(time.RFC3339Nano),
		}
		if err := writeDaemonAdminRuntimeState(runtimePath, runtime); err != nil {
			_ = listener.Close()
			return nil, err
		}
		svc := &daemonAdminService{
			server:      server,
			listener:    listener,
			runtimePath: runtimePath,
			address:     candidateAddr,
			out:         options.Out,
		}
		go func() {
			err := server.Serve(listener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				if options.Out != nil {
					_, _ = fmt.Fprintf(options.Out, "daemon admin server stopped with error: %v\n", err)
				}
			}
		}()
		return svc, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("unknown listen error")
	}
	return nil, fmt.Errorf("daemon admin address %s unavailable after %d attempts: %w", preferredAddr, maxFallback+1, lastErr)
}

func daemonAdminAddrWithPortOffset(addr string, offset int) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", fmt.Errorf("admin addr is empty")
	}
	if offset < 0 {
		offset = 0
	}

	host := ""
	portText := ""
	if strings.HasPrefix(trimmed, ":") {
		portText = strings.TrimPrefix(trimmed, ":")
	} else {
		h, p, err := net.SplitHostPort(trimmed)
		if err != nil {
			return "", fmt.Errorf("invalid admin addr %q: %w", trimmed, err)
		}
		host = strings.TrimSpace(h)
		portText = strings.TrimSpace(p)
	}
	basePort, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil {
		return "", fmt.Errorf("invalid admin addr %q: parse port failed", trimmed)
	}
	nextPort := basePort + offset
	if nextPort <= 0 || nextPort > 65535 {
		return "", fmt.Errorf("invalid admin port %d after offset %d", nextPort, offset)
	}
	if host == "" {
		return fmt.Sprintf(":%d", nextPort), nil
	}
	return net.JoinHostPort(host, strconv.Itoa(nextPort)), nil
}

func isDaemonAdminAddrAlreadyInUse(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

func (s *daemonAdminService) Address() string {
	if s == nil {
		return ""
	}
	return s.address
}

func (s *daemonAdminService) URL() string {
	if s == nil {
		return ""
	}
	return defaultDaemonAdminURL(s.address)
}

func (s *daemonAdminService) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if s.server != nil {
			if err := s.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				closeErr = err
			}
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if removed, err := cleanupDaemonAdminRuntimeStateForCurrentProcess(s.runtimePath); err != nil {
			if s.out != nil {
				_, _ = fmt.Fprintf(s.out, "daemon admin runtime cleanup skipped: %v\n", err)
			}
		} else if removed && s.out != nil {
			_, _ = fmt.Fprintln(s.out, "daemon admin runtime state removed on shutdown")
		}
	})
	return closeErr
}

func (c *daemonAdminController) registerHandlers(mux *http.ServeMux) {
	if mux == nil {
		return
	}
	handleAuth := func(path string, handler http.HandlerFunc) {
		mux.HandleFunc(path, c.requireAuth(handler))
	}

	handleAuth(daemonAdminHomePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status := c.buildStatus()
		renderDaemonAdminHome(w, daemonAdminHomeView{Status: status})
	})
	handleAuth(daemonAdminHomeSlash, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.TrimSpace(r.URL.Path) != daemonAdminHomeSlash {
			http.NotFound(w, r)
			return
		}
		target := daemonAdminHomePath
		if raw := strings.TrimSpace(r.URL.RawQuery); raw != "" {
			target = target + "?" + raw
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
	handleAuth(daemonAdminStatusPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeDaemonAdminJSON(w, http.StatusOK, c.buildStatus())
	})
	handleAuth(daemonAdminBookingServiceStatusPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := c.bookingServiceStatus()
		if err != nil {
			writeDaemonAdminJSON(w, daemonAdminActionHTTPStatus(err), map[string]any{
				"status":  "error",
				"message": err.Error(),
			})
			return
		}
		writeDaemonAdminJSON(w, http.StatusOK, result)
	})

	register := func(path string, handler func(*http.Request) (any, error)) {
		handleAuth(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			result, err := handler(r)
			if err != nil {
				code := daemonAdminActionHTTPStatus(err)
				writeDaemonAdminJSON(w, code, map[string]any{
					"status":  "error",
					"message": err.Error(),
					"result":  result,
				})
				return
			}
			writeDaemonAdminJSON(w, http.StatusOK, map[string]any{
				"status":  "ok",
				"message": "action completed",
				"result":  result,
			})
		})
	}

	register(daemonAdminWebhookStartPath, func(r *http.Request) (any, error) {
		result, err := c.webhookStart()
		if err != nil {
			return result, err
		}
		return result, nil
	})
	register(daemonAdminWebhookStopPath, func(r *http.Request) (any, error) {
		result, err := c.webhookStop()
		if err != nil {
			return result, err
		}
		return result, nil
	})
	register(daemonAdminWebhookKillPortPath, func(r *http.Request) (any, error) {
		result, err := c.webhookKillPort()
		if err != nil {
			return result, err
		}
		return result, nil
	})
	register(daemonAdminTaskGlobalPausePath, func(r *http.Request) (any, error) {
		return c.taskGlobalPause()
	})
	register(daemonAdminTaskGlobalResumePath, func(r *http.Request) (any, error) {
		return c.taskGlobalResume()
	})
	register(daemonAdminTaskPausePath, func(r *http.Request) (any, error) {
		id := daemonAdminRequestID(r)
		return c.taskPause(id)
	})
	register(daemonAdminTaskResumePath, func(r *http.Request) (any, error) {
		id := daemonAdminRequestID(r)
		return c.taskResume(id)
	})
	register(daemonAdminMonitorStartPath, func(r *http.Request) (any, error) {
		id := daemonAdminRequestID(r)
		return c.monitorStart(id)
	})
	register(daemonAdminMonitorStopPath, func(r *http.Request) (any, error) {
		id := daemonAdminRequestID(r)
		return c.monitorStop(id)
	})
	register(daemonAdminMonitorStartAllPath, func(r *http.Request) (any, error) {
		return c.monitorStartAll()
	})
	register(daemonAdminMonitorStopAllPath, func(r *http.Request) (any, error) {
		return c.monitorStopAll()
	})
	register(daemonAdminBookingServiceStartPath, func(r *http.Request) (any, error) {
		return c.bookingServiceStart()
	})
	register(daemonAdminBookingServiceStopPath, func(r *http.Request) (any, error) {
		return c.bookingServiceStop()
	})
}

func (c *daemonAdminController) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.isAuthorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Daemon Admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (c *daemonAdminController) isAuthorized(r *http.Request) bool {
	if r == nil {
		return false
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	authCfg := c.authConfig
	if strings.TrimSpace(c.authConfigPath) != "" {
		reloadedCfg, err := loadDaemonAdminAuthConfig(c.authConfigPath)
		if err != nil {
			return false
		}
		authCfg = reloadedCfg
	}
	return daemonAdminCredentialsMatch(authCfg, username, password)
}

func daemonAdminConstantTimeEqual(left string, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func daemonAdminCredentialsMatch(cfg daemonAdminAuthConfig, username string, password string) bool {
	normalized := normalizeDaemonAdminAuthConfig(cfg)
	if !daemonAdminConstantTimeEqual(strings.TrimSpace(username), normalized.Username) {
		return false
	}
	if normalized.PasswordHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(normalized.PasswordHash), []byte(password)) == nil
	}
	return daemonAdminConstantTimeEqual(password, normalized.Password)
}

func daemonAdminRequestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.FormValue("id"))
	if id != "" {
		return id
	}
	return strings.TrimSpace(r.URL.Query().Get("id"))
}

type daemonBookingProductUpsertRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type daemonBookingSlotUpsertRequest struct {
	ID        string `json:"id"`
	ProductID string `json:"product_id"`
	StartAt   string `json:"start_at"`
	EndAt     string `json:"end_at"`
	Capacity  *int   `json:"capacity,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

type daemonBookingCreateReservationRequest struct {
	ProductID           string                      `json:"product_id"`
	SlotID              string                      `json:"slot_id"`
	UserID              string                      `json:"user_id"`
	PartySize           int                         `json:"party_size"`
	Personnel           bookingReservationPersonnel `json:"personnel"`
	SpecialRequirements string                      `json:"special_requirements"`
}

type daemonBookingReservationActionRequest struct {
	ID   string `json:"id"`
	Note string `json:"note"`
}

func parseDaemonBookingCatalogQuery(r *http.Request) (bookingCatalogQuery, error) {
	if r == nil {
		return bookingCatalogQuery{}, bookingValidationf("request is required")
	}
	query := bookingCatalogQuery{
		ProductID: strings.TrimSpace(r.URL.Query().Get("product_id")),
	}

	fromRaw := strings.TrimSpace(r.URL.Query().Get("from"))
	if fromRaw != "" {
		from, err := parseBookingRFC3339(fromRaw, "from")
		if err != nil {
			return bookingCatalogQuery{}, err
		}
		query.From = &from
	}
	toRaw := strings.TrimSpace(r.URL.Query().Get("to"))
	if toRaw != "" {
		to, err := parseBookingRFC3339(toRaw, "to")
		if err != nil {
			return bookingCatalogQuery{}, err
		}
		query.To = &to
	}
	includeFullRaw := strings.TrimSpace(r.URL.Query().Get("include_full"))
	if includeFullRaw != "" {
		value, err := strconv.ParseBool(includeFullRaw)
		if err != nil {
			return bookingCatalogQuery{}, bookingValidationf("invalid include_full value %q", includeFullRaw)
		}
		query.IncludeFull = value
	}
	return query, nil
}

func decodeDaemonAdminJSONBody(r *http.Request, payload any) error {
	if r == nil {
		return bookingValidationf("request is required")
	}
	if payload == nil {
		return bookingValidationf("request payload target is required")
	}
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, defaultWebhookMaxBodyBytes))
	if err != nil {
		return bookingValidationf("read request body failed: %v", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	if err := json.Unmarshal(raw, payload); err != nil {
		return bookingValidationf("invalid json body: %v", err)
	}
	return nil
}

func daemonBookingRoutePath(path string) bool {
	trimmed := strings.TrimSpace(path)
	return strings.HasPrefix(trimmed, "/admin/booking/")
}

func daemonBookingHTTPStatus(err error) int {
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

func daemonAdminActionHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "not found") || strings.Contains(message, "missing") || strings.Contains(message, "required") || strings.Contains(message, "invalid") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (c *daemonAdminController) buildStatus() daemonAdminStatusResponse {
	now := time.Now()
	status := daemonAdminStatusResponse{
		Name: "longtradego daemon",
		Now:  now.Format(time.RFC3339Nano),
		Daemon: daemonAdminDaemonStatus{
			PID:       c.daemonPID,
			SessionID: c.daemonSessionID,
			StartedAt: formatTaskTimestamp(c.daemonStartedAt),
		},
		Admin: daemonAdminServiceStatus{
			Address: c.adminAddress,
			URL:     defaultDaemonAdminURL(c.adminAddress),
		},
		Booking: daemonAdminBookingServiceStatus{
			Status: "stopped",
		},
		Task:    daemonAdminTaskStatus{Tasks: []daemonAdminTaskView{}},
		Monitor: daemonAdminMonitorStatus{Monitors: []daemonMonitorSnapshot{}},
	}

	if c.taskManager != nil {
		tasks := c.taskManager.listTasks()
		globalPaused := c.taskManager.isGloballyPaused()
		status.Task.Total = len(tasks)
		status.Task.GlobalPaused = globalPaused
		for _, task := range tasks {
			view := buildDaemonAdminTaskView(task, globalPaused)
			status.Task.Tasks = append(status.Task.Tasks, view)
			if task.Running {
				status.Task.Running++
			}
			if task.Paused {
				status.Task.Paused++
			}
			if view.State == "scheduled" {
				status.Task.Scheduled++
			}
		}
	}

	if c.monitorMu != nil {
		monitors := listDaemonMonitors(c.monitorMu, c.monitors, c.monitorRecords)
		status.Monitor.Monitors = monitors
		status.Monitor.Total = len(monitors)
		for _, monitor := range monitors {
			switch strings.ToLower(strings.TrimSpace(monitor.Status)) {
			case "running":
				status.Monitor.Running++
			case "paused":
				status.Monitor.Paused++
			default:
				status.Monitor.Scheduled++
			}
		}
	}

	bookingResult, bookingErr := c.bookingServiceStatus()
	if bookingErr != nil {
		status.Booking = daemonAdminBookingServiceStatus{
			Status:  "error",
			Running: false,
			Message: bookingErr.Error(),
		}
	} else {
		publicAddr := ""
		adminAddr := ""
		if bookingResult.Runtime != nil {
			publicAddr = bookingRuntimePublicAddress(bookingResult.Runtime)
			adminAddr = bookingRuntimeAdminAddress(bookingResult.Runtime)
		}
		status.Booking = daemonAdminBookingServiceStatus{
			Status:        bookingResult.Status,
			Running:       bookingResult.Running,
			Address:       publicAddr,
			PublicAddress: publicAddr,
			AdminAddress:  adminAddr,
			URL:           strings.TrimSpace(bookingResult.URL),
			PublicURL:     strings.TrimSpace(bookingResult.PublicURL),
			AdminURL:      strings.TrimSpace(bookingResult.AdminURL),
			Message:       strings.TrimSpace(bookingResult.Message),
			Runtime:       bookingResult.Runtime,
		}
		if bookingResult.Runtime != nil {
			status.Booking.PID = bookingResult.Runtime.PID
		}
	}

	webhookResult, err := webhookStatus(c.webhookRuntime)
	if err != nil {
		status.Webhook = daemonAdminWebhookStatus{
			Status:  "error",
			Running: false,
			Message: err.Error(),
		}
		return status
	}
	status.Webhook = daemonAdminWebhookStatus{
		Status:     webhookResult.Status,
		Running:    webhookResult.Running,
		RouteCount: webhookResult.RouteCount,
		Routes:     webhookResult.Routes,
		Dispatch:   webhookResult.Dispatch,
		Metrics:    webhookResult.Metrics,
		LogQueue:   webhookResult.LogQueue,
	}
	if webhookResult.Runtime != nil {
		status.Webhook.Address = strings.TrimSpace(webhookResult.Runtime.Address)
		status.Webhook.PublicAddress = webhookRuntimePublicAddress(webhookResult.Runtime)
		status.Webhook.AdminAddress = webhookRuntimeAdminAddress(webhookResult.Runtime)
		status.Webhook.Path = webhookResult.Runtime.Path
	}
	if !webhookResult.Running {
		status.Webhook.Message = strings.TrimSpace(webhookResult.Status)
	}
	return status
}

func (c *daemonAdminController) webhookStart() (webhookStartResult, error) {
	if c.webhookStartFn != nil {
		result, err := c.webhookStartFn()
		if err == nil {
			c.trackWebhookLifecycle([]string{"webhook", "start"}, result)
		}
		return result, err
	}
	cfg := newWebhookServeConfig()
	if err := cfg.validate(); err != nil {
		return webhookStartResult{}, err
	}
	claim := &webhookStartOwnerClaim{}
	if strings.TrimSpace(c.daemonSessionID) != "" {
		claim = &webhookStartOwnerClaim{
			SessionID:  c.daemonSessionID,
			DaemonPID:  c.daemonPID,
			ClaimedAt:  time.Now(),
			StartToken: newDaemonWebhookOwnerStartToken(),
		}
	}
	result, err := startWebhookInBackground(c.webhookRuntime, c.webhookLogPath, cfg, claim)
	if err == nil {
		c.trackWebhookLifecycle([]string{"webhook", "start"}, result)
	}
	return result, err
}

func (c *daemonAdminController) webhookStop() (webhookStopResult, error) {
	if c.webhookStopFn != nil {
		result, err := c.webhookStopFn()
		if err == nil {
			c.trackWebhookLifecycle([]string{"webhook", "stop"}, result)
		}
		return result, err
	}
	result, err := stopWebhook(c.webhookRuntime, defaultWebhookStopTimeout)
	if err == nil {
		c.trackWebhookLifecycle([]string{"webhook", "stop"}, result)
	}
	return result, err
}

func (c *daemonAdminController) webhookKillPort() (webhookKillPortResult, error) {
	if c.webhookKillFn != nil {
		result, err := c.webhookKillFn()
		if len(result.StoppedPIDs) > 0 {
			c.untrackWebhookPIDs(result.StoppedPIDs)
		}
		return result, err
	}
	result, err := executeWebhookKillPort(defaultWebhookServeAddr, defaultWebhookStopTimeout, c.webhookRuntime, false)
	if len(result.StoppedPIDs) > 0 {
		c.untrackWebhookPIDs(result.StoppedPIDs)
	}
	return result, err
}

func (c *daemonAdminController) taskGlobalPause() (map[string]any, error) {
	if c.taskManager == nil {
		return nil, fmt.Errorf("task manager not initialized")
	}
	if err := c.taskManager.pauseAll(); err != nil {
		return nil, err
	}
	return map[string]any{"status": "paused", "scope": "global"}, nil
}

func (c *daemonAdminController) taskGlobalResume() (map[string]any, error) {
	if c.taskManager == nil {
		return nil, fmt.Errorf("task manager not initialized")
	}
	if err := c.taskManager.resumeAll(); err != nil {
		return nil, err
	}
	return map[string]any{"status": "resumed", "scope": "global"}, nil
}

func (c *daemonAdminController) taskPause(taskID string) (map[string]any, error) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if c.taskManager == nil {
		return nil, fmt.Errorf("task manager not initialized")
	}
	paused, err := c.taskManager.pauseTask(id)
	if err != nil {
		return nil, err
	}
	if !paused {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return map[string]any{"status": "paused", "id": id}, nil
}

func (c *daemonAdminController) taskResume(taskID string) (map[string]any, error) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if c.taskManager == nil {
		return nil, fmt.Errorf("task manager not initialized")
	}
	resumed, err := c.taskManager.resumeTask(id)
	if err != nil {
		return nil, err
	}
	if !resumed {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return map[string]any{"status": "resumed", "id": id}, nil
}

func (c *daemonAdminController) monitorStart(monitorID string) (map[string]any, error) {
	id := strings.TrimSpace(monitorID)
	if id == "" {
		return nil, fmt.Errorf("monitor id is required")
	}
	if c.startMonitorByID == nil {
		return nil, fmt.Errorf("monitor start is unavailable")
	}
	if err := c.startMonitorByID(id); err != nil {
		return nil, err
	}
	return map[string]any{"status": "started", "id": id}, nil
}

func (c *daemonAdminController) monitorStop(monitorID string) (map[string]any, error) {
	id := strings.TrimSpace(monitorID)
	if id == "" {
		return nil, fmt.Errorf("monitor id is required")
	}
	if c.monitorMu == nil {
		return nil, fmt.Errorf("monitor state unavailable")
	}
	if !stopDaemonMonitorByTarget(c.monitorMu, c.monitors, c.monitorRecords, id) {
		return nil, fmt.Errorf("monitor not found: %s", id)
	}
	if c.saveMonitorState != nil {
		if err := c.saveMonitorState(); err != nil {
			return nil, fmt.Errorf("save monitor state failed: %w", err)
		}
	}
	return map[string]any{"status": "stopped", "id": id}, nil
}

func (c *daemonAdminController) monitorStartAll() (map[string]any, error) {
	if c.startAllMonitors == nil {
		return nil, fmt.Errorf("monitor start-all is unavailable")
	}
	count, err := c.startAllMonitors()
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "started", "count": count}, nil
}

func (c *daemonAdminController) monitorStopAll() (map[string]any, error) {
	if c.monitorMu == nil {
		return nil, fmt.Errorf("monitor state unavailable")
	}
	stopped := stopAllDaemonMonitors(c.monitorMu, c.monitors, c.monitorRecords)
	if c.saveMonitorState != nil {
		if err := c.saveMonitorState(); err != nil {
			return nil, fmt.Errorf("save monitor state failed: %w", err)
		}
	}
	return map[string]any{"status": "stopped", "count": stopped}, nil
}

func (c *daemonAdminController) bookingServiceStatus() (bookingServiceStatusResult, error) {
	if c.bookingStatusFn != nil {
		return c.bookingStatusFn()
	}
	return bookingServiceStatus(c.bookingRuntime)
}

func (c *daemonAdminController) bookingServiceStart() (bookingServiceStartResult, error) {
	if c.bookingStartFn != nil {
		result, err := c.bookingStartFn()
		if err == nil {
			c.trackBookingLifecycle([]string{"booking", "service", "start"}, result)
		}
		return result, err
	}
	cfg := &bookingServiceServeConfig{
		Addr:             c.bookingAddr,
		PublicAddr:       c.bookingAddr,
		AdminAddr:        c.bookingAdminAddr,
		RuntimePath:      c.bookingRuntime,
		CatalogPath:      c.bookingCatalog,
		ReservationsPath: c.bookingReservations,
		DraftsPath:       c.bookingDrafts,
		APIKeysPath:      c.bookingAPIKeys,
		LLMConfigPath:    c.bookingLLMConfig,
		AdminAuthPath:    c.bookingAdminAuth,
		MaxPortFallback:  defaultBookingServicePortFallback,
	}
	var ownerClaim *bookingServiceStartOwnerClaim
	if strings.TrimSpace(c.daemonSessionID) != "" {
		ownerClaim = &bookingServiceStartOwnerClaim{
			SessionID:  strings.TrimSpace(c.daemonSessionID),
			DaemonPID:  c.daemonPID,
			ClaimedAt:  time.Now(),
			StartToken: newDaemonBookingOwnerStartToken(),
		}
	}
	result, err := startBookingServiceInBackgroundWithOwner(c.bookingRuntime, c.bookingLogPath, cfg, ownerClaim)
	if err == nil {
		c.trackBookingLifecycle([]string{"booking", "service", "start"}, result)
	}
	return result, err
}

func (c *daemonAdminController) bookingServiceStop() (bookingServiceStopResult, error) {
	if c.bookingStopFn != nil {
		result, err := c.bookingStopFn()
		if err == nil {
			c.trackBookingLifecycle([]string{"booking", "service", "stop"}, result)
		}
		return result, err
	}
	result, err := stopBookingService(c.bookingRuntime, defaultBookingServiceStopTimeout)
	if err == nil {
		c.trackBookingLifecycle([]string{"booking", "service", "stop"}, result)
	}
	return result, err
}

func (c *daemonAdminController) trackWebhookLifecycle(args []string, result any) {
	if c.webhookOwnedMu == nil || c.webhookOwnedPIDs == nil {
		return
	}
	c.webhookOwnedMu.Lock()
	defer c.webhookOwnedMu.Unlock()
	trackDaemonOwnedWebhookLifecycle(args, result, c.webhookOwnedPIDs, c.daemonSessionID)
}

func (c *daemonAdminController) untrackWebhookPIDs(stoppedPIDs []int) {
	if len(stoppedPIDs) == 0 || c.webhookOwnedMu == nil || c.webhookOwnedPIDs == nil {
		return
	}
	c.webhookOwnedMu.Lock()
	defer c.webhookOwnedMu.Unlock()
	for _, pid := range stoppedPIDs {
		delete(c.webhookOwnedPIDs, pid)
	}
}

func (c *daemonAdminController) trackBookingLifecycle(args []string, result any) {
	if c.bookingOwnedMu == nil || c.bookingOwnedPIDs == nil {
		return
	}
	c.bookingOwnedMu.Lock()
	defer c.bookingOwnedMu.Unlock()
	trackDaemonOwnedBookingLifecycle(args, result, c.bookingOwnedPIDs, c.daemonSessionID)
}

func renderDaemonAdminHome(w http.ResponseWriter, view daemonAdminHomeView) {
	if w == nil {
		return
	}
	var buffer bytes.Buffer
	if err := daemonAdminHomeTemplate.Execute(&buffer, view); err != nil {
		http.Error(w, "render daemon admin failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, &buffer)
}

func writeDaemonAdminJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, err := marshalJSONIndentNoHTMLEscape(payload, "", defaultWebhookResponseIndent)
	if err != nil {
		_, _ = io.WriteString(w, `{"error":"encode failed"}`)
		return
	}
	_, _ = w.Write(append(encoded, '\n'))
}

func daemonAdminRoutes() []string {
	routes := []string{
		daemonAdminHomePath,
		daemonAdminStatusPath,
		daemonAdminWebhookStartPath,
		daemonAdminWebhookStopPath,
		daemonAdminWebhookKillPortPath,
		daemonAdminBookingServiceStatusPath,
		daemonAdminBookingServiceStartPath,
		daemonAdminBookingServiceStopPath,
		daemonAdminTaskGlobalPausePath,
		daemonAdminTaskGlobalResumePath,
		daemonAdminTaskPausePath,
		daemonAdminTaskResumePath,
		daemonAdminMonitorStartPath,
		daemonAdminMonitorStopPath,
		daemonAdminMonitorStartAllPath,
		daemonAdminMonitorStopAllPath,
	}
	sort.Strings(routes)
	return routes
}

func buildDaemonAdminTaskView(task daemonTaskSnapshot, globalPaused bool) daemonAdminTaskView {
	state, stateText := deriveDaemonAdminTaskState(task, globalPaused)
	lastResult := deriveDaemonAdminTaskLastResult(task)
	return daemonAdminTaskView{
		daemonTaskSnapshot: task,
		LastStatusCompat:   task.LastStatus,
		LastErrorCompat:    task.LastError,
		PausedCompat:       task.Paused,
		RunningCompat:      task.Running,
		State:              state,
		StateText:          stateText,
		LastResult:         lastResult,
	}
}

func deriveDaemonAdminTaskState(task daemonTaskSnapshot, globalPaused bool) (string, string) {
	if task.Running && task.Paused {
		return "running_pause_pending", "Running (pause pending)"
	}
	if task.Running {
		return "running", "Running"
	}
	if task.Paused {
		return "paused", "Paused"
	}
	if globalPaused {
		return "globally_paused", "Globally Paused"
	}
	lastResult := deriveDaemonAdminTaskLastResult(task)
	switch lastResult {
	case "success":
		return "scheduled", "Scheduled (last success)"
	case "failed":
		return "scheduled", "Scheduled (last failed)"
	default:
		return "scheduled", "Scheduled"
	}
}

func deriveDaemonAdminTaskLastResult(task daemonTaskSnapshot) string {
	lastError := strings.TrimSpace(task.LastError)
	if lastError != "" {
		return "failed"
	}
	switch strings.ToLower(strings.TrimSpace(task.LastStatus)) {
	case "success":
		return "success"
	case "failed", "error":
		return "failed"
	default:
		return "unknown"
	}
}
