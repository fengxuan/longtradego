package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultWebhookTokenRefreshInterval   = 2 * time.Second
	defaultWebhookLogFlushInterval       = 100 * time.Millisecond
	defaultWebhookLogBatchSize           = 64
	defaultWebhookLogQueueSize           = 2048
	defaultWebhookLogEnqueueBlockTimeout = 200 * time.Millisecond
	defaultWebhookMetricsWindowSize      = 2048
	defaultWebhookManagementHTTPTimeout  = 500 * time.Millisecond

	webhookHealthzPath     = "/healthz"
	webhookReadyzPath      = "/readyz"
	webhookMetricsPath     = "/metrics"
	webhookAdminHomePath   = "/admin"
	webhookAdminHomeSlash  = "/admin/"
	webhookAdminStatusPath = "/admin/webhook/status"
	webhookAdminStopPath   = "/admin/webhook/stop"
)

type webhookTokenFinder func(thirdPartyID string) (webhookTokenRecord, bool, error)

type webhookMetricsSnapshot struct {
	RequestTotal     int64   `json:"request_total"`
	SuccessTotal     int64   `json:"success_total"`
	ClientErrorTotal int64   `json:"client_error_total"`
	ServerErrorTotal int64   `json:"server_error_total"`
	AuthFailureTotal int64   `json:"auth_failure_total"`
	AverageLatencyMs float64 `json:"average_latency_ms"`
	P95LatencyMs     int64   `json:"p95_latency_ms"`
	SampleSize       int     `json:"sample_size"`
}

type webhookLogQueueDepth struct {
	Event int `json:"event"`
	Audit int `json:"audit"`
}

type webhookTokenCacheStatus struct {
	Path            string `json:"path"`
	RefreshInterval string `json:"refresh_interval"`
	Entries         int    `json:"entries"`
	LastReloadAt    string `json:"last_reload_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
}

type webhookAdminStatusResponse struct {
	Name          string                  `json:"name"`
	Now           string                  `json:"now"`
	UptimeSeconds int64                   `json:"uptime_seconds"`
	Address       string                  `json:"address"`
	Path          string                  `json:"path"`
	RouteCount    int                     `json:"route_count"`
	Routes        []webhookRouteSummary   `json:"routes"`
	Dispatch      webhookDispatchStats    `json:"dispatch"`
	Metrics       webhookMetricsSnapshot  `json:"metrics"`
	LogQueueDepth webhookLogQueueDepth    `json:"log_queue_depth"`
	TokenCache    webhookTokenCacheStatus `json:"token_cache"`
}

type webhookAdminHomeLink struct {
	Label string
	Path  string
	Note  string
}

type webhookAdminHomeView struct {
	Status          webhookAdminStatusResponse
	Ready           bool
	ManagementLinks []webhookAdminHomeLink
	StopActionPath  string
}

var webhookAdminHomeTemplate = template.Must(template.New("webhook_admin_home").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Webhook Admin</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f4f7fb;
      --panel: #ffffff;
      --text: #111827;
      --muted: #6b7280;
      --accent: #0f766e;
      --border: #d1d5db;
      --ok-bg: #ecfdf3;
      --ok-text: #166534;
      --bad-bg: #fef2f2;
      --bad-text: #991b1b;
    }
    * {
      box-sizing: border-box;
    }
    body {
      margin: 0;
      padding: 24px;
      background: var(--bg);
      color: var(--text);
      font-family: "IBM Plex Sans", "Avenir Next", "Segoe UI", sans-serif;
    }
    .container {
      max-width: 1120px;
      margin: 0 auto;
      display: grid;
      gap: 16px;
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 16px;
      box-shadow: 0 8px 24px rgba(15, 23, 42, 0.06);
    }
    .header-row {
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 12px;
      flex-wrap: wrap;
    }
    h1, h2 {
      margin: 0;
      line-height: 1.2;
    }
    h1 {
      font-size: 1.5rem;
    }
    h2 {
      font-size: 1.05rem;
    }
    .meta {
      color: var(--muted);
      font-size: 0.9rem;
      margin-top: 6px;
    }
    .status-pill {
      display: inline-flex;
      align-items: center;
      border-radius: 999px;
      font-size: 0.86rem;
      padding: 4px 10px;
      font-weight: 600;
      border: 1px solid transparent;
    }
    .status-pill.ready {
      background: var(--ok-bg);
      color: var(--ok-text);
      border-color: #bbf7d0;
    }
    .status-pill.not-ready {
      background: var(--bad-bg);
      color: var(--bad-text);
      border-color: #fecaca;
    }
    .refresh-btn {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: #ffffff;
      padding: 8px 12px;
      cursor: pointer;
      font-size: 0.9rem;
      color: var(--text);
    }
    .refresh-btn:hover {
      border-color: #9ca3af;
    }
    .stop-btn {
      border: 1px solid #b91c1c;
      border-radius: 8px;
      background: #b91c1c;
      color: #fff;
      padding: 8px 12px;
      cursor: pointer;
      font-size: 0.9rem;
      font-weight: 600;
      margin-left: 8px;
    }
    .stop-btn:hover {
      background: #991b1b;
      border-color: #991b1b;
    }
    .kv-grid {
      display: grid;
      gap: 8px;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      margin-top: 12px;
    }
    .kv {
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px;
      background: #fafafa;
    }
    .kv .k {
      font-size: 0.78rem;
      color: var(--muted);
      text-transform: uppercase;
      letter-spacing: 0.04em;
    }
    .kv .v {
      margin-top: 3px;
      font-size: 1.02rem;
      font-weight: 600;
    }
    .links {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 10px;
      margin-top: 12px;
    }
    .link-card {
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px;
      background: #fafafa;
    }
    .link-card a {
      color: var(--accent);
      font-weight: 600;
      text-decoration: none;
      font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
      font-size: 0.95rem;
    }
    .link-card a:hover {
      text-decoration: underline;
    }
    .link-note {
      margin-top: 6px;
      font-size: 0.82rem;
      color: var(--muted);
    }
    table {
      width: 100%;
      border-collapse: collapse;
      margin-top: 10px;
      font-size: 0.9rem;
    }
    th, td {
      border-bottom: 1px solid var(--border);
      text-align: left;
      padding: 8px 6px;
      vertical-align: top;
    }
    th {
      color: var(--muted);
      font-size: 0.78rem;
      text-transform: uppercase;
      letter-spacing: 0.04em;
    }
    code {
      font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
      white-space: pre-wrap;
      word-break: break-word;
    }
    .hint {
      margin-top: 10px;
      color: var(--muted);
      font-size: 0.85rem;
    }
    @media (max-width: 640px) {
      body {
        padding: 14px;
      }
      .panel {
        padding: 12px;
      }
    }
  </style>
</head>
<body>
  <div class="container">
    <section class="panel">
      <div class="header-row">
        <div>
          <h1>Webhook Admin Home</h1>
          <div class="meta">Service: <code>{{.Status.Name}}</code> | Updated: <code>{{.Status.Now}}</code></div>
        </div>
        <div>
          {{if .Ready}}
          <span class="status-pill ready">Ready</span>
          {{else}}
          <span class="status-pill not-ready">Not Ready</span>
          {{end}}
          <button class="refresh-btn" type="button" onclick="window.location.reload()">Refresh Page</button>
          <button class="stop-btn" type="button" data-stop-action="{{.StopActionPath}}" onclick="return stopCurrentWebhook(this)">Stop Current Webhook</button>
        </div>
      </div>
      <div class="kv-grid">
        <div class="kv"><div class="k">Address</div><div class="v"><code>{{.Status.Address}}</code></div></div>
        <div class="kv"><div class="k">Primary Path</div><div class="v"><code>{{.Status.Path}}</code></div></div>
        <div class="kv"><div class="k">Uptime (seconds)</div><div class="v">{{.Status.UptimeSeconds}}</div></div>
        <div class="kv"><div class="k">Route Count</div><div class="v">{{.Status.RouteCount}}</div></div>
      </div>
      <div class="kv-grid">
        <div class="kv"><div class="k">Requests</div><div class="v">{{.Status.Metrics.RequestTotal}}</div></div>
        <div class="kv"><div class="k">Auth Failures</div><div class="v">{{.Status.Metrics.AuthFailureTotal}}</div></div>
        <div class="kv"><div class="k">Client Errors</div><div class="v">{{.Status.Metrics.ClientErrorTotal}}</div></div>
        <div class="kv"><div class="k">Server Errors</div><div class="v">{{.Status.Metrics.ServerErrorTotal}}</div></div>
      </div>
      <div class="kv-grid">
        <div class="kv"><div class="k">Dispatch Pending</div><div class="v">{{.Status.Dispatch.Pending}}</div></div>
        <div class="kv"><div class="k">Dispatch Retrying</div><div class="v">{{.Status.Dispatch.Retrying}}</div></div>
        <div class="kv"><div class="k">Dead Letter</div><div class="v">{{.Status.Dispatch.DeadLetter}}</div></div>
        <div class="kv"><div class="k">Token Cache Entries</div><div class="v">{{.Status.TokenCache.Entries}}</div></div>
      </div>
      {{if .Status.TokenCache.LastError}}
      <div class="hint">Token cache last error: <code>{{.Status.TokenCache.LastError}}</code></div>
      {{end}}
      <div class="hint">Stop action only affects this current webhook instance.</div>
      <div class="hint" id="stop-status" aria-live="polite"></div>
    </section>

    <section class="panel">
      <h2>Management Endpoints</h2>
      <div class="links">
        {{range .ManagementLinks}}
        <div class="link-card">
          <div><a href="{{.Path}}">{{.Path}}</a></div>
          <div class="link-note">{{.Label}}. {{.Note}}</div>
        </div>
        {{end}}
      </div>
    </section>

    <section class="panel">
      <h2>Webhook Route List</h2>
      {{if .Status.Routes}}
      <table>
        <thead>
          <tr>
            <th>ID</th>
            <th>Path</th>
            <th>Mode</th>
            <th>Enabled</th>
            <th>Pipeline</th>
          </tr>
        </thead>
        <tbody>
          {{range .Status.Routes}}
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
      {{else}}
      <div class="hint">No enabled webhook routes.</div>
      {{end}}
      <div class="hint">POST-only: webhook business route paths are designed for signed POST requests. Opening them in a browser may return 405.</div>
    </section>
  </div>
  <script>
    async function stopCurrentWebhook(button) {
      const action = button && button.dataset ? button.dataset.stopAction : "";
      if (!action) {
        return false;
      }
      if (!window.confirm("Stop current webhook now? This will terminate this webhook server process.")) {
        return false;
      }
      const statusEl = document.getElementById("stop-status");
      const originalText = button.textContent;
      button.disabled = true;
      button.textContent = "Stopping...";
      if (statusEl) {
        statusEl.textContent = "Submitting stop request...";
      }
      try {
        const response = await fetch(action, { method: "POST" });
        const payload = await response.json().catch(function () { return {}; });
        if (!response.ok) {
          const message = (payload && payload.message) ? String(payload.message) : ("stop request failed with status " + response.status);
          if (statusEl) {
            statusEl.textContent = message;
          }
          button.disabled = false;
          button.textContent = originalText;
          return false;
        }
        const message = (payload && payload.message) ? String(payload.message) : "Shutdown requested. This page stays open.";
        if (statusEl) {
          statusEl.textContent = message + " Refresh may fail after service fully stops.";
        }
      } catch (err) {
        if (statusEl) {
          statusEl.textContent = "Failed to request shutdown: " + String(err);
        }
        button.disabled = false;
        button.textContent = originalText;
      }
      return false;
    }
  </script>
</body>
</html>
`))

type webhookServerMetrics struct {
	requestTotal     atomic.Int64
	successTotal     atomic.Int64
	clientErrorTotal atomic.Int64
	serverErrorTotal atomic.Int64
	authFailureTotal atomic.Int64
	totalLatencyMs   atomic.Int64

	mu          sync.Mutex
	latenciesMs []int64
	latIdx      int
	latCount    int
}

func newWebhookServerMetrics(windowSize int) *webhookServerMetrics {
	size := windowSize
	if size <= 0 {
		size = defaultWebhookMetricsWindowSize
	}
	return &webhookServerMetrics{
		latenciesMs: make([]int64, size),
	}
}

func (m *webhookServerMetrics) Observe(status int, duration time.Duration) {
	if m == nil {
		return
	}
	durMs := duration.Milliseconds()
	if durMs < 0 {
		durMs = 0
	}

	m.requestTotal.Add(1)
	m.totalLatencyMs.Add(durMs)
	switch {
	case status >= 200 && status < 300:
		m.successTotal.Add(1)
	case status >= 400 && status < 500:
		m.clientErrorTotal.Add(1)
	case status >= 500:
		m.serverErrorTotal.Add(1)
	}
	if status == http.StatusUnauthorized {
		m.authFailureTotal.Add(1)
	}

	m.mu.Lock()
	m.latenciesMs[m.latIdx] = durMs
	m.latIdx = (m.latIdx + 1) % len(m.latenciesMs)
	if m.latCount < len(m.latenciesMs) {
		m.latCount++
	}
	m.mu.Unlock()
}

func (m *webhookServerMetrics) Snapshot() webhookMetricsSnapshot {
	if m == nil {
		return webhookMetricsSnapshot{}
	}
	total := m.requestTotal.Load()
	avg := 0.0
	if total > 0 {
		avg = float64(m.totalLatencyMs.Load()) / float64(total)
	}

	m.mu.Lock()
	samples := m.latCount
	latencies := make([]int64, samples)
	copy(latencies, m.latenciesMs[:samples])
	m.mu.Unlock()

	p95 := int64(0)
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		index := ((95 * len(latencies)) + 99) / 100
		if index <= 0 {
			index = 1
		}
		if index > len(latencies) {
			index = len(latencies)
		}
		p95 = latencies[index-1]
	}

	return webhookMetricsSnapshot{
		RequestTotal:     total,
		SuccessTotal:     m.successTotal.Load(),
		ClientErrorTotal: m.clientErrorTotal.Load(),
		ServerErrorTotal: m.serverErrorTotal.Load(),
		AuthFailureTotal: m.authFailureTotal.Load(),
		AverageLatencyMs: avg,
		P95LatencyMs:     p95,
		SampleSize:       len(latencies),
	}
}

type webhookTokenCache struct {
	path            string
	refreshInterval time.Duration

	mu           sync.RWMutex
	records      map[string]webhookTokenRecord
	lastReloadAt time.Time
	lastError    string
	lastModTime  int64
	lastSize     int64
	lastPresent  bool

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newWebhookTokenCache(path string, refreshInterval time.Duration) (*webhookTokenCache, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("token store path is empty")
	}
	interval := refreshInterval
	if interval <= 0 {
		interval = defaultWebhookTokenRefreshInterval
	}
	cache := &webhookTokenCache{
		path:            trimmedPath,
		refreshInterval: interval,
		records:         map[string]webhookTokenRecord{},
		stopCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
	}
	if err := cache.reload(true); err != nil {
		return nil, err
	}
	return cache, nil
}

func (c *webhookTokenCache) Start() {
	if c == nil {
		return
	}
	go c.loop()
}

func (c *webhookTokenCache) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
	select {
	case <-c.doneCh:
	case <-time.After(2 * time.Second):
	}
}

func (c *webhookTokenCache) loop() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			_ = c.reloadIfChanged()
		}
	}
}

func (c *webhookTokenCache) reloadIfChanged() error {
	stat, err := os.Stat(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			c.mu.RLock()
			previouslyPresent := c.lastPresent
			c.mu.RUnlock()
			if previouslyPresent {
				return c.reload(false)
			}
			return nil
		}
		c.mu.Lock()
		c.lastError = err.Error()
		c.mu.Unlock()
		return err
	}

	modTime := stat.ModTime().UnixNano()
	size := stat.Size()

	c.mu.RLock()
	unchanged := c.lastPresent && c.lastModTime == modTime && c.lastSize == size
	c.mu.RUnlock()
	if unchanged {
		return nil
	}
	return c.reload(false)
}

func (c *webhookTokenCache) reload(failOnError bool) error {
	records, err := loadWebhookTokenRecords(c.path)
	if err != nil {
		c.mu.Lock()
		c.lastError = err.Error()
		c.mu.Unlock()
		if failOnError {
			return err
		}
		return nil
	}

	var (
		present bool
		modTime int64
		size    int64
	)
	if stat, statErr := os.Stat(c.path); statErr == nil {
		present = true
		modTime = stat.ModTime().UnixNano()
		size = stat.Size()
	}

	c.mu.Lock()
	c.records = records
	c.lastReloadAt = time.Now()
	c.lastError = ""
	c.lastPresent = present
	c.lastModTime = modTime
	c.lastSize = size
	c.mu.Unlock()
	return nil
}

func (c *webhookTokenCache) Find(thirdPartyID string) (webhookTokenRecord, bool, error) {
	if c == nil {
		return webhookTokenRecord{}, false, fmt.Errorf("token cache is not initialized")
	}
	key := normalizeThirdPartyID(thirdPartyID)
	if key == "" {
		return webhookTokenRecord{}, false, nil
	}

	c.mu.RLock()
	record, ok := c.records[key]
	c.mu.RUnlock()
	if !ok {
		return webhookTokenRecord{}, false, nil
	}
	return record, true, nil
}

func (c *webhookTokenCache) Status() webhookTokenCacheStatus {
	if c == nil {
		return webhookTokenCacheStatus{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return webhookTokenCacheStatus{
		Path:            c.path,
		RefreshInterval: c.refreshInterval.String(),
		Entries:         len(c.records),
		LastReloadAt:    formatTaskTimestamp(c.lastReloadAt),
		LastError:       c.lastError,
	}
}

type webhookAsyncLineWriter struct {
	name          string
	queue         chan []byte
	flushInterval time.Duration
	batchSize     int
	blockTimeout  time.Duration
	writeBatch    func(lines [][]byte) error

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopped  atomic.Bool
}

func newWebhookAsyncLineWriter(
	name string,
	queueSize int,
	batchSize int,
	flushInterval time.Duration,
	blockTimeout time.Duration,
	writeBatch func(lines [][]byte) error,
) (*webhookAsyncLineWriter, error) {
	if writeBatch == nil {
		return nil, fmt.Errorf("async line writer %q writeBatch is nil", name)
	}
	if queueSize <= 0 {
		queueSize = defaultWebhookLogQueueSize
	}
	if batchSize <= 0 {
		batchSize = defaultWebhookLogBatchSize
	}
	if flushInterval <= 0 {
		flushInterval = defaultWebhookLogFlushInterval
	}
	if blockTimeout <= 0 {
		blockTimeout = defaultWebhookLogEnqueueBlockTimeout
	}

	return &webhookAsyncLineWriter{
		name:          strings.TrimSpace(name),
		queue:         make(chan []byte, queueSize),
		flushInterval: flushInterval,
		batchSize:     batchSize,
		blockTimeout:  blockTimeout,
		writeBatch:    writeBatch,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}, nil
}

func (w *webhookAsyncLineWriter) Start() {
	if w == nil {
		return
	}
	go w.loop()
}

func (w *webhookAsyncLineWriter) Stop() {
	if w == nil {
		return
	}
	w.stopped.Store(true)
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	select {
	case <-w.doneCh:
	case <-time.After(3 * time.Second):
	}
}

func (w *webhookAsyncLineWriter) QueueDepth() int {
	if w == nil {
		return 0
	}
	return len(w.queue)
}

func (w *webhookAsyncLineWriter) AppendLine(line []byte) error {
	if w == nil || len(line) == 0 {
		return nil
	}
	next := append([]byte(nil), line...)
	if w.stopped.Load() {
		return w.writeBatch([][]byte{next})
	}

	select {
	case w.queue <- next:
		return nil
	default:
	}

	timer := time.NewTimer(w.blockTimeout)
	defer timer.Stop()

	select {
	case w.queue <- next:
		return nil
	case <-timer.C:
		return w.writeBatch([][]byte{next})
	case <-w.stopCh:
		return w.writeBatch([][]byte{next})
	}
}

func (w *webhookAsyncLineWriter) loop() {
	defer close(w.doneCh)

	batch := make([][]byte, 0, w.batchSize)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		for {
			if err := w.writeBatch(batch); err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			break
		}
		batch = batch[:0]
	}

	drain := func() {
		for {
			select {
			case line := <-w.queue:
				batch = append(batch, line)
				if len(batch) >= w.batchSize {
					flush()
				}
			default:
				flush()
				return
			}
		}
	}

	for {
		select {
		case <-w.stopCh:
			drain()
			return
		case line := <-w.queue:
			batch = append(batch, line)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func newWebhookEventLogWriter(path string) (*webhookAsyncLineWriter, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("event log path is empty")
	}
	writeBatch := func(lines [][]byte) error {
		return appendWebhookEventLogLines(trimmedPath, lines)
	}
	return newWebhookAsyncLineWriter("webhook_event_log", defaultWebhookLogQueueSize, defaultWebhookLogBatchSize, defaultWebhookLogFlushInterval, defaultWebhookLogEnqueueBlockTimeout, writeBatch)
}

func newWebhookAuditLogWriter(path string) (*webhookAsyncLineWriter, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("audit log path is empty")
	}
	writeBatch := func(lines [][]byte) error {
		for _, line := range lines {
			if err := appendLogLineWithRotation(trimmedPath, line, commandLogMaxSize, "webhook_audit"); err != nil {
				return err
			}
		}
		return nil
	}
	return newWebhookAsyncLineWriter("webhook_audit_log", defaultWebhookLogQueueSize, defaultWebhookLogBatchSize, defaultWebhookLogFlushInterval, defaultWebhookLogEnqueueBlockTimeout, writeBatch)
}

func appendWebhookEventLogLines(path string, lines [][]byte) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("event log path is empty")
	}
	if len(lines) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(trimmedPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	var payload bytes.Buffer
	for _, line := range lines {
		payload.Write(line)
	}
	_, err = file.Write(payload.Bytes())
	return err
}

func appendWebhookEventLogAsync(writer *webhookAsyncLineWriter, event webhookEventEnvelope) error {
	if writer == nil {
		return fmt.Errorf("event log writer is not initialized")
	}
	encoded, err := marshalJSONNoHTMLEscape(event)
	if err != nil {
		return err
	}
	return writer.AppendLine(append(encoded, '\n'))
}

func appendWebhookAuditLogAsync(writer *webhookAsyncLineWriter, entry webhookAuditLogEntry) error {
	if writer == nil {
		return nil
	}
	encoded, err := marshalJSONNoHTMLEscape(entry)
	if err != nil {
		return err
	}
	return writer.AppendLine(append(encoded, '\n'))
}

func validateWebhookManagementPathConflicts(routes []webhookResolvedRoute) error {
	reserved := map[string]struct{}{
		webhookHealthzPath:     {},
		webhookReadyzPath:      {},
		webhookMetricsPath:     {},
		webhookAdminHomePath:   {},
		webhookAdminHomeSlash:  {},
		webhookAdminStatusPath: {},
		webhookAdminStopPath:   {},
	}
	for _, route := range routes {
		if _, exists := reserved[route.Record.Path]; exists {
			return fmt.Errorf("route path %s conflicts with reserved management endpoint", route.Record.Path)
		}
	}
	return nil
}

func registerWebhookManagementHandlers(
	mux *http.ServeMux,
	cfg *webhookServeConfig,
	routes []webhookResolvedRoute,
	metrics *webhookServerMetrics,
	tokenCache *webhookTokenCache,
	eventWriter *webhookAsyncLineWriter,
	auditWriter *webhookAsyncLineWriter,
	startedAt time.Time,
	requestShutdown func(source string),
) {
	if mux == nil {
		return
	}
	if cfg == nil {
		cfg = newWebhookServeConfig()
	}
	nowFn := time.Now
	var stopOnce sync.Once

	summaries := summarizeWebhookRoutes(routes)
	buildStatus := func() webhookAdminStatusResponse {
		dispatchStats, err := readWebhookDispatchStats(cfg.DispatchQueuePath, cfg.DeadLetterPath)
		if err != nil {
			dispatchStats = webhookDispatchStats{}
		}
		uptime := nowFn().Sub(startedAt)
		if uptime < 0 {
			uptime = 0
		}
		return webhookAdminStatusResponse{
			Name:          "longtradego webhook",
			Now:           nowFn().Format(time.RFC3339Nano),
			UptimeSeconds: int64(uptime.Seconds()),
			Address:       cfg.Addr,
			Path:          cfg.Path,
			RouteCount:    len(summaries),
			Routes:        summaries,
			Dispatch:      dispatchStats,
			Metrics:       metrics.Snapshot(),
			LogQueueDepth: webhookLogQueueDepth{
				Event: eventWriter.QueueDepth(),
				Audit: auditWriter.QueueDepth(),
			},
			TokenCache: tokenCache.Status(),
		}
	}

	mux.HandleFunc(webhookHealthzPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeWebhookManagementJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"name":   "longtradego webhook",
		})
	})

	mux.HandleFunc(webhookReadyzPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status := buildStatus()
		ready := strings.TrimSpace(status.TokenCache.LastError) == ""
		code := http.StatusOK
		if !ready {
			code = http.StatusServiceUnavailable
		}
		writeWebhookManagementJSON(w, code, map[string]any{
			"ready":  ready,
			"status": status,
		})
	})

	mux.HandleFunc(webhookAdminStatusPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeWebhookManagementJSON(w, http.StatusOK, buildStatus())
	})

	mux.HandleFunc(webhookAdminStopPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeWebhookManagementJSON(w, http.StatusAccepted, map[string]any{
			"status":  "stopping",
			"message": "webhook shutdown requested",
		})
		if requestShutdown == nil {
			return
		}
		stopOnce.Do(func() {
			go requestShutdown("admin_stop")
		})
	})

	mux.HandleFunc(webhookAdminHomePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status := buildStatus()
		renderWebhookAdminHome(w, webhookAdminHomeView{
			Status: status,
			Ready:  strings.TrimSpace(status.TokenCache.LastError) == "",
			ManagementLinks: []webhookAdminHomeLink{
				{
					Label: "Health endpoint",
					Path:  webhookHealthzPath,
					Note:  "Readiness-independent liveness check.",
				},
				{
					Label: "Ready endpoint",
					Path:  webhookReadyzPath,
					Note:  "Includes readiness judgement with full status payload.",
				},
				{
					Label: "Admin status endpoint",
					Path:  webhookAdminStatusPath,
					Note:  "Structured JSON snapshot for automation.",
				},
				{
					Label: "Metrics endpoint",
					Path:  webhookMetricsPath,
					Note:  "Plain text counters and latency metrics.",
				},
			},
			StopActionPath: webhookAdminStopPath,
		})
	})

	mux.HandleFunc(webhookAdminHomeSlash, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		target := webhookAdminHomePath
		if raw := strings.TrimSpace(r.URL.RawQuery); raw != "" {
			target = target + "?" + raw
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})

	mux.HandleFunc(webhookMetricsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status := buildStatus()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, buildWebhookMetricsPayload(status))
	})

}

func buildWebhookMetricsPayload(status webhookAdminStatusResponse) string {
	var payload strings.Builder
	payload.WriteString(fmt.Sprintf("webhook_requests_total %d\n", status.Metrics.RequestTotal))
	payload.WriteString(fmt.Sprintf("webhook_auth_failures_total %d\n", status.Metrics.AuthFailureTotal))
	payload.WriteString(fmt.Sprintf("webhook_success_total %d\n", status.Metrics.SuccessTotal))
	payload.WriteString(fmt.Sprintf("webhook_client_errors_total %d\n", status.Metrics.ClientErrorTotal))
	payload.WriteString(fmt.Sprintf("webhook_server_errors_total %d\n", status.Metrics.ServerErrorTotal))
	payload.WriteString(fmt.Sprintf("webhook_latency_average_ms %.2f\n", status.Metrics.AverageLatencyMs))
	payload.WriteString(fmt.Sprintf("webhook_latency_p95_ms %d\n", status.Metrics.P95LatencyMs))
	payload.WriteString(fmt.Sprintf("webhook_log_queue_depth_event %d\n", status.LogQueueDepth.Event))
	payload.WriteString(fmt.Sprintf("webhook_log_queue_depth_audit %d\n", status.LogQueueDepth.Audit))
	payload.WriteString(fmt.Sprintf("webhook_dispatch_pending %d\n", status.Dispatch.Pending))
	payload.WriteString(fmt.Sprintf("webhook_dispatch_retrying %d\n", status.Dispatch.Retrying))
	payload.WriteString(fmt.Sprintf("webhook_dispatch_dead_letter %d\n", status.Dispatch.DeadLetter))
	return payload.String()
}

func renderWebhookAdminHome(w http.ResponseWriter, view webhookAdminHomeView) {
	if w == nil {
		return
	}
	var buffer bytes.Buffer
	if err := webhookAdminHomeTemplate.Execute(&buffer, view); err != nil {
		http.Error(w, "render admin home failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, &buffer)
}

func writeWebhookManagementJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, err := marshalJSONIndentNoHTMLEscape(payload, "", defaultWebhookResponseIndent)
	if err != nil {
		_, _ = io.WriteString(w, `{"error":"encode failed"}`)
		return
	}
	_, _ = w.Write(append(encoded, '\n'))
}

func buildWebhookManagementURL(addr string, endpointPath string) string {
	trimmedAddr := strings.TrimSpace(addr)
	trimmedPath := ensureWebhookPath(endpointPath)
	if trimmedAddr == "" {
		return ""
	}
	if strings.HasPrefix(trimmedAddr, "http://") || strings.HasPrefix(trimmedAddr, "https://") {
		return strings.TrimRight(trimmedAddr, "/") + trimmedPath
	}

	if strings.HasPrefix(trimmedAddr, ":") {
		return "http://127.0.0.1" + trimmedAddr + trimmedPath
	}

	host, port, err := net.SplitHostPort(trimmedAddr)
	if err != nil {
		return "http://" + trimmedAddr + trimmedPath
	}
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + trimmedPath
}

func fetchWebhookAdminStatus(address string, timeout time.Duration) (webhookAdminStatusResponse, error) {
	url := buildWebhookManagementURL(address, webhookAdminStatusPath)
	if strings.TrimSpace(url) == "" {
		return webhookAdminStatusResponse{}, fmt.Errorf("webhook address is empty")
	}
	clientTimeout := timeout
	if clientTimeout <= 0 {
		clientTimeout = defaultWebhookManagementHTTPTimeout
	}
	client := &http.Client{Timeout: clientTimeout}
	response, err := client.Get(url)
	if err != nil {
		return webhookAdminStatusResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return webhookAdminStatusResponse{}, fmt.Errorf("unexpected status code %d", response.StatusCode)
	}
	var payload webhookAdminStatusResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return webhookAdminStatusResponse{}, err
	}
	return payload, nil
}
