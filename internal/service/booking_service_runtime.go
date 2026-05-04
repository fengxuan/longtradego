package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
	defaultBookingServiceAdminAddr    = "127.0.0.1:18082"
	defaultBookingServicePortFallback = 20
	defaultBookingServiceStopTimeout  = 5 * time.Second
	defaultBookingServiceStartTimeout = 3 * time.Second
	defaultBookingServiceStartPoll    = 100 * time.Millisecond

	bookingServiceInternalEnv = "LONGTRADEGO_BOOKING_INTERNAL_SERVE"

	bookingPublicCatalogPath       = "/booking/catalog"
	bookingPublicReservationsPath  = "/booking/reservations"
	bookingPublicIntentParsePath   = "/booking/intents/parse"
	bookingPublicIntentConfirmPath = "/booking/intents/confirm"

	bookingAdminHomePath               = "/admin"
	bookingAdminHomeSlash              = "/admin/"
	bookingAdminStopPath               = "/admin/booking/stop"
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
	PID             int    `json:"pid"`
	Address         string `json:"address"`
	PublicAddress   string `json:"public_address,omitempty"`
	AdminAddress    string `json:"admin_address,omitempty"`
	StartedAt       string `json:"started_at"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	OwnerSessionID  string `json:"owner_session_id,omitempty"`
	OwnerDaemonPID  int    `json:"owner_daemon_pid,omitempty"`
	OwnerClaimedAt  string `json:"owner_claimed_at,omitempty"`
	OwnerStartToken string `json:"owner_start_token,omitempty"`
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
	Mode      string                     `json:"mode"`
	Status    string                     `json:"status"`
	Running   bool                       `json:"running"`
	Runtime   *bookingServiceRuntimeInfo `json:"runtime,omitempty"`
	URL       string                     `json:"url,omitempty"`
	PublicURL string                     `json:"public_url,omitempty"`
	AdminURL  string                     `json:"admin_url,omitempty"`
	Message   string                     `json:"message,omitempty"`
}

type bookingServiceStopResult struct {
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	PID     int    `json:"pid,omitempty"`
	Message string `json:"message,omitempty"`
}

type bookingServiceStartOwnerClaim struct {
	SessionID  string
	DaemonPID  int
	ClaimedAt  time.Time
	StartToken string
}

type bookingServiceStartOwnerClaimContextKey struct{}

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
	PublicAddr       string
	AdminAddr        string
	RuntimePath      string
	CatalogPath      string
	ReservationsPath string
	DraftsPath       string
	IdempotencyPath  string
	APIKeysPath      string
	LLMConfigPath    string
	AdminAuthPath    string
	MaxPortFallback  int
}

type bookingServiceHandle struct {
	publicServer   *http.Server
	publicListener net.Listener
	adminServer    *http.Server
	adminListener  net.Listener
	runtimePath    string
	tokenCache     *webhookTokenCache
	out            io.Writer
	stopOnce       sync.Once
	errCh          chan error
}

type bookingServiceController struct {
	service         *bookingService
	apiKeys         map[string]struct{}
	tokenRecords    map[string]webhookTokenRecord
	tokenCache      *webhookTokenCache
	idempotency     *publicIdempotencyStore
	authConfig      daemonAdminAuthConfig
	authConfigPath  string
	draftsPath      string
	llmConfigPath   string
	nowFn           func() time.Time
	parseWithLLM    bookingIntentParseFn
	requestShutdown func(source string)
}

type bookingAdminHomeLink struct {
	Label string
	Path  string
	Note  string
}

type bookingAdminHomeView struct {
	Name              string
	Now               string
	PublicAddress     string
	AdminAddress      string
	Status            bookingAdminStatus
	ManagementLinks   []bookingAdminHomeLink
	StopActionPath    string
	ConfirmActionPath string
	RejectActionPath  string
	CancelActionPath  string
}

var bookingAdminHomeTemplate = template.Must(template.New("booking_admin_home").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Booking Admin</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f4f7fb;
      --panel: #ffffff;
      --text: #111827;
      --muted: #6b7280;
      --accent: #0f766e;
      --border: #d1d5db;
      --warn: #9f1239;
      --warn-bg: #fff1f2;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 20px;
      background: var(--bg);
      color: var(--text);
      font-family: "IBM Plex Sans", "Avenir Next", "Segoe UI", sans-serif;
    }
    .container {
      max-width: 1200px;
      margin: 0 auto;
      display: grid;
      gap: 14px;
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 14px;
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
    h1 { font-size: 1.5rem; }
    h2 { font-size: 1.05rem; margin-bottom: 8px; }
    .meta {
      color: var(--muted);
      font-size: 0.9rem;
      margin-top: 6px;
    }
    .toolbar {
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
    }
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
      font-weight: 600;
    }
    button.danger:hover {
      border-color: #991b1b;
      background: #991b1b;
    }
    .status-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
      gap: 8px;
      margin-top: 10px;
    }
    .kv {
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 8px;
      background: #fafafa;
    }
    .kv .k {
      font-size: 0.76rem;
      text-transform: uppercase;
      color: var(--muted);
    }
    .kv .v {
      font-weight: 600;
      margin-top: 2px;
      word-break: break-word;
    }
    .links {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      gap: 10px;
      margin-top: 10px;
    }
    .link-card {
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px;
      background: #fafafa;
    }
    .link-card a {
      color: var(--accent);
      text-decoration: none;
      font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
      font-size: 0.95rem;
      font-weight: 600;
    }
    .link-card a:hover { text-decoration: underline; }
    .link-note {
      margin-top: 6px;
      color: var(--muted);
      font-size: 0.82rem;
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
    .op-row { display: flex; gap: 6px; flex-wrap: wrap; }
    .feedback {
      margin-top: 8px;
      font-size: 0.86rem;
      min-height: 18px;
      color: var(--muted);
    }
    .feedback.error {
      color: var(--warn);
      font-weight: 600;
      background: var(--warn-bg);
      border: 1px solid #fecdd3;
      border-radius: 6px;
      padding: 6px 8px;
    }
    .feedback.success {
      color: #065f46;
      font-weight: 600;
    }
    .hint {
      color: var(--muted);
      font-size: 0.85rem;
      margin-top: 8px;
    }
    @media (max-width: 640px) {
      body { padding: 14px; }
      .panel { padding: 12px; }
    }
  </style>
</head>
<body>
  <div class="container">
    <section class="panel">
      <div class="header-row">
        <div>
          <h1>Booking Admin Home</h1>
          <div class="meta">Service: <code>{{.Name}}</code> | Updated: <code>{{.Now}}</code></div>
        </div>
        <div class="toolbar">
          <button type="button" onclick="window.location.reload()">Refresh Page</button>
          <button class="danger" type="button" data-action="{{.StopActionPath}}" onclick="return stopCurrentBooking(this)">Stop Current Service</button>
        </div>
      </div>
      <div id="booking-feedback" class="feedback" aria-live="polite"></div>
      <div class="status-grid">
        <div class="kv"><div class="k">Public Address</div><div class="v"><code>{{.PublicAddress}}</code></div></div>
        <div class="kv"><div class="k">Admin Address</div><div class="v"><code>{{.AdminAddress}}</code></div></div>
        <div class="kv"><div class="k">Products</div><div class="v">{{.Status.Summary.ProductCount}}</div></div>
        <div class="kv"><div class="k">Slots</div><div class="v">{{.Status.Summary.SlotCount}}</div></div>
        <div class="kv"><div class="k">Reservations</div><div class="v">{{.Status.Summary.ReservationCount}}</div></div>
      </div>
      <div class="status-grid">
        <div class="kv"><div class="k">Pending</div><div class="v">{{.Status.Summary.Pending}}</div></div>
        <div class="kv"><div class="k">Confirmed</div><div class="v">{{.Status.Summary.Confirmed}}</div></div>
        <div class="kv"><div class="k">Rejected</div><div class="v">{{.Status.Summary.Rejected}}</div></div>
        <div class="kv"><div class="k">Cancelled</div><div class="v">{{.Status.Summary.Cancelled}}</div></div>
      </div>
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
      <h2>Products</h2>
      {{if .Status.Products}}
      <table>
        <thead><tr><th>ID</th><th>Name</th><th>Description</th><th>Enabled</th><th>Updated</th></tr></thead>
        <tbody>
          {{range .Status.Products}}
          <tr>
            <td><code>{{.ID}}</code></td>
            <td>{{.Name}}</td>
            <td>{{if .Description}}{{.Description}}{{else}}-{{end}}</td>
            <td>{{.Enabled}}</td>
            <td><code>{{if .UpdatedAt}}{{.UpdatedAt}}{{else}}-{{end}}</code></td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">No products found.</div>
      {{end}}
    </section>

    <section class="panel">
      <h2>Slots</h2>
      {{if .Status.Slots}}
      <table>
        <thead><tr><th>ID</th><th>Product</th><th>Start</th><th>End</th><th>Capacity</th><th>Available</th><th>Enabled</th></tr></thead>
        <tbody>
          {{range .Status.Slots}}
          <tr>
            <td><code>{{.ID}}</code></td>
            <td><code>{{.ProductID}}</code></td>
            <td><code>{{.StartAt}}</code></td>
            <td><code>{{.EndAt}}</code></td>
            <td>{{.Capacity}}</td>
            <td>{{.AvailableCapacity}}</td>
            <td>{{.Enabled}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">No slots found.</div>
      {{end}}
    </section>

    <section class="panel">
      <h2>Reservations</h2>
      {{if .Status.Reservations}}
      <table>
        <thead><tr><th>ID</th><th>Status</th><th>User</th><th>Product</th><th>Slot</th><th>Party</th><th>Contact</th><th>Phone</th><th>Operation</th></tr></thead>
        <tbody>
          {{range .Status.Reservations}}
          <tr>
            <td><code>{{.ID}}</code></td>
            <td>{{.Status}}</td>
            <td><code>{{.UserID}}</code></td>
            <td><code>{{.ProductID}}</code></td>
            <td><code>{{.SlotID}}</code></td>
            <td>{{.PartySize}}</td>
            <td>{{.Personnel.ContactName}}</td>
            <td>{{.Personnel.ContactPhone}}</td>
            <td>
              <div class="op-row">
                <button type="button" onclick="postReservationAction('{{$.ConfirmActionPath}}', '{{.ID}}', 'Confirm reservation {{.ID}}?')">Confirm</button>
                <button type="button" onclick="postReservationAction('{{$.RejectActionPath}}', '{{.ID}}', 'Reject reservation {{.ID}}?')">Reject</button>
                <button type="button" onclick="postReservationAction('{{$.CancelActionPath}}', '{{.ID}}', 'Cancel reservation {{.ID}}?')">Cancel</button>
              </div>
            </td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">No reservations found.</div>
      {{end}}
    </section>
  </div>

  <script>
    function updateFeedback(message, state) {
      var node = document.getElementById("booking-feedback");
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

    async function postAction(path) {
      updateFeedback("Submitting action...", "");
      try {
        var response = await fetch(path, { method: "POST" });
        var payload = await response.json().catch(function() { return {}; });
        if (!response.ok) {
          var failed = (payload && payload.message) ? String(payload.message) : ("request failed: HTTP " + response.status);
          updateFeedback(failed, "error");
          return false;
        }
        var ok = (payload && payload.message) ? String(payload.message) : "ok";
        updateFeedback(ok, "success");
        window.setTimeout(function() { window.location.reload(); }, 400);
      } catch (err) {
        updateFeedback("request failed: " + String(err), "error");
      }
      return false;
    }

    function postReservationAction(path, id, confirmText) {
      if (confirmText && !window.confirm(confirmText)) {
        return false;
      }
      var target = String(path || "");
      if (String(id || "") !== "") {
        target += "?id=" + encodeURIComponent(String(id));
      }
      return postAction(target);
    }

    function stopCurrentBooking(button) {
      var action = button && button.dataset ? String(button.dataset.action || "") : "";
      if (!action) {
        return false;
      }
      if (!window.confirm("Stop current booking service now? This will terminate this booking process.")) {
        return false;
      }
      return postAction(action);
    }
  </script>
</body>
</html>
`))

type bookingIntentParseFn func(context.Context, bookingIntentParseRequest, bookingIntentParseContext) (bookingIntentParseExtracted, error)

var (
	bookingSpawnBackgroundProcess = spawnBookingBackgroundProcess
	bookingVerifyBackgroundStart  = verifyBookingBackgroundStart
	bookingStateDraftMu           sync.Mutex
	bookingParseIntentWithLLM     bookingIntentParseFn = parseBookingIntentWithOpenAI
)

func withBookingServiceStartOwnerClaim(ctx context.Context, claim bookingServiceStartOwnerClaim) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, bookingServiceStartOwnerClaimContextKey{}, claim)
}

func bookingServiceStartOwnerClaimFromContext(ctx context.Context) (bookingServiceStartOwnerClaim, bool) {
	if ctx == nil {
		return bookingServiceStartOwnerClaim{}, false
	}
	value := ctx.Value(bookingServiceStartOwnerClaimContextKey{})
	claim, ok := value.(bookingServiceStartOwnerClaim)
	if !ok {
		return bookingServiceStartOwnerClaim{}, false
	}
	claim.SessionID = strings.TrimSpace(claim.SessionID)
	claim.StartToken = strings.TrimSpace(claim.StartToken)
	if claim.SessionID == "" || claim.StartToken == "" {
		return bookingServiceStartOwnerClaim{}, false
	}
	if claim.ClaimedAt.IsZero() {
		claim.ClaimedAt = time.Now()
	}
	return claim, true
}

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
	return defaultSecurityKeysPath()
}

func defaultBookingLLMConfigPath() string {
	return filepath.Join(daemonConfigDir, bookingLLMConfigFile)
}

func defaultBookingIntakeDraftsPath() string {
	return filepath.Join(daemonDataDir, bookingIntakeDraftsFile)
}

func defaultBookingIdempotencyPath(runtimePath string) string {
	return defaultBookingIdempotencyStatePath(runtimePath)
}

func newBookingServiceServeConfig() *bookingServiceServeConfig {
	return &bookingServiceServeConfig{
		Addr:             defaultBookingServiceAddr,
		PublicAddr:       defaultBookingServiceAddr,
		AdminAddr:        defaultBookingServiceAdminAddr,
		RuntimePath:      defaultBookingRuntimeStatePath(),
		CatalogPath:      defaultBookingCatalogStatePath(),
		ReservationsPath: defaultBookingReservationsStatePath(),
		DraftsPath:       defaultBookingIntakeDraftsPath(),
		IdempotencyPath:  defaultBookingIdempotencyPath(defaultBookingRuntimeStatePath()),
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
	cfg.PublicAddr = strings.TrimSpace(cfg.PublicAddr)
	cfg.AdminAddr = strings.TrimSpace(cfg.AdminAddr)
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = cfg.Addr
	}
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = defaultBookingServiceAddr
	}
	cfg.Addr = cfg.PublicAddr
	if cfg.AdminAddr == "" {
		cfg.AdminAddr = defaultBookingServiceAdminAddr
	}
	cfg.RuntimePath = strings.TrimSpace(cfg.RuntimePath)
	cfg.CatalogPath = strings.TrimSpace(cfg.CatalogPath)
	cfg.ReservationsPath = strings.TrimSpace(cfg.ReservationsPath)
	cfg.DraftsPath = strings.TrimSpace(cfg.DraftsPath)
	cfg.IdempotencyPath = strings.TrimSpace(cfg.IdempotencyPath)
	cfg.APIKeysPath = strings.TrimSpace(cfg.APIKeysPath)
	cfg.LLMConfigPath = strings.TrimSpace(cfg.LLMConfigPath)
	cfg.AdminAuthPath = strings.TrimSpace(cfg.AdminAuthPath)
	if cfg.PublicAddr == "" {
		return fmt.Errorf("public-addr is required")
	}
	if cfg.AdminAddr == "" {
		return fmt.Errorf("admin-addr is required")
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
	if cfg.IdempotencyPath == "" {
		cfg.IdempotencyPath = defaultBookingIdempotencyPath(cfg.RuntimePath)
	}
	if cfg.IdempotencyPath == "" {
		return fmt.Errorf("idempotency path is required")
	}
	if cfg.APIKeysPath == "" {
		return fmt.Errorf("security-keys is required")
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
	records, err := loadSecurityTokenRecords(path, false)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(records))
	keys := make([]string, 0, len(records))
	for _, record := range records {
		if !securityRecordHasScope(record, securityScopeBooking) {
			continue
		}
		token := strings.TrimSpace(record.Token)
		if token == "" {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		keys = append(keys, token)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("security keys config %s has no valid booking tokens", strings.TrimSpace(path))
	}
	sort.Strings(keys)
	return keys, nil
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
	if state.Runtime.PID == 0 &&
		strings.TrimSpace(state.Runtime.Address) == "" &&
		strings.TrimSpace(state.Runtime.PublicAddress) == "" &&
		strings.TrimSpace(state.Runtime.AdminAddress) == "" {
		return nil, false, nil
	}
	state.Runtime.Address = strings.TrimSpace(state.Runtime.Address)
	state.Runtime.PublicAddress = strings.TrimSpace(state.Runtime.PublicAddress)
	state.Runtime.AdminAddress = strings.TrimSpace(state.Runtime.AdminAddress)
	if state.Runtime.PublicAddress == "" {
		state.Runtime.PublicAddress = state.Runtime.Address
	}
	if state.Runtime.Address == "" {
		state.Runtime.Address = state.Runtime.PublicAddress
	}
	return &state.Runtime, true, nil
}

func writeBookingRuntimeState(path string, runtime bookingServiceRuntimeInfo) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("booking runtime path is empty")
	}
	runtime.Address = strings.TrimSpace(runtime.Address)
	runtime.PublicAddress = strings.TrimSpace(runtime.PublicAddress)
	runtime.AdminAddress = strings.TrimSpace(runtime.AdminAddress)
	if runtime.PublicAddress == "" {
		runtime.PublicAddress = runtime.Address
	}
	if runtime.Address == "" {
		runtime.Address = runtime.PublicAddress
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
	return writeFileAtomic(trimmedPath, append(encoded, '\n'), 0o644)
}

func bookingRuntimePublicAddress(runtime *bookingServiceRuntimeInfo) string {
	if runtime == nil {
		return ""
	}
	publicAddr := strings.TrimSpace(runtime.PublicAddress)
	if publicAddr != "" {
		return publicAddr
	}
	return strings.TrimSpace(runtime.Address)
}

func bookingRuntimeAdminAddress(runtime *bookingServiceRuntimeInfo) string {
	if runtime == nil {
		return ""
	}
	adminAddr := strings.TrimSpace(runtime.AdminAddress)
	if adminAddr != "" {
		return adminAddr
	}
	return strings.TrimSpace(runtime.Address)
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
		publicAddr := bookingRuntimePublicAddress(runtime)
		adminAddr := bookingRuntimeAdminAddress(runtime)
		result.PublicURL = buildWebhookManagementURL(publicAddr, bookingPublicCatalogPath)
		result.AdminURL = buildWebhookManagementURL(adminAddr, bookingAdminStatusPath)
		result.URL = result.AdminURL
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
	return startBookingServiceInBackgroundWithOwner(runtimePath, logPath, cfg, nil)
}

func startBookingServiceInBackgroundWithOwner(
	runtimePath string,
	logPath string,
	cfg *bookingServiceServeConfig,
	ownerClaim *bookingServiceStartOwnerClaim,
) (bookingServiceStartResult, error) {
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
	if _, err := loadSecurityTokenRecords(cfg.APIKeysPath, false); err != nil {
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
		candidatePublicAddr, publicAddrErr := daemonAdminAddrWithPortOffset(cfg.PublicAddr, offset)
		if publicAddrErr != nil {
			appendBookingServiceStartFailureLog(logPath, publicAddrErr)
			return bookingServiceStartResult{}, publicAddrErr
		}
		candidateAdminAddr, adminAddrErr := daemonAdminAddrWithPortOffset(cfg.AdminAddr, offset)
		if adminAddrErr != nil {
			appendBookingServiceStartFailureLog(logPath, adminAddrErr)
			return bookingServiceStartResult{}, adminAddrErr
		}
		if err := ensureBookingAddrAvailable(candidatePublicAddr); err != nil {
			lastErr = err
			continue
		}
		if err := ensureBookingAddrAvailable(candidateAdminAddr); err != nil {
			lastErr = err
			continue
		}

		spawnCfg := *cfg
		spawnCfg.Addr = candidatePublicAddr
		spawnCfg.PublicAddr = candidatePublicAddr
		spawnCfg.AdminAddr = candidateAdminAddr
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
			PID:           pid,
			Address:       candidatePublicAddr,
			PublicAddress: candidatePublicAddr,
			AdminAddress:  candidateAdminAddr,
			StartedAt:     nowText,
			UpdatedAt:     nowText,
		}
		if ownerClaim != nil {
			sessionID := strings.TrimSpace(ownerClaim.SessionID)
			startToken := strings.TrimSpace(ownerClaim.StartToken)
			if sessionID != "" && startToken != "" {
				claimedAt := ownerClaim.ClaimedAt
				if claimedAt.IsZero() {
					claimedAt = time.Now()
				}
				runtimeInfo.OwnerSessionID = sessionID
				runtimeInfo.OwnerDaemonPID = ownerClaim.DaemonPID
				runtimeInfo.OwnerClaimedAt = claimedAt.Format(time.RFC3339Nano)
				runtimeInfo.OwnerStartToken = startToken
			}
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
			result.Message = fmt.Sprintf("preferred addresses unavailable, fallback to public=%s admin=%s", candidatePublicAddr, candidateAdminAddr)
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
	_ = authCfg
	tokenRecords, err := loadSecurityTokenRecords(cfg.APIKeysPath, false)
	if err != nil {
		return fmt.Errorf("load security keys for readiness check failed: %w", err)
	}
	publicAddr := strings.TrimSpace(cfg.PublicAddr)
	if publicAddr == "" {
		publicAddr = strings.TrimSpace(cfg.Addr)
	}
	var bookingToken webhookTokenRecord
	foundBookingToken := false
	for _, record := range tokenRecords {
		if securityRecordHasScope(record, securityScopeBooking) {
			bookingToken = record
			foundBookingToken = true
			break
		}
	}
	if !foundBookingToken {
		return fmt.Errorf("security keys config %s has no booking-scoped token", strings.TrimSpace(cfg.APIKeysPath))
	}
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

		if err := fetchBookingPublicCatalogStatus(publicAddr, bookingToken, defaultWebhookManagementHTTPTimeout); err == nil {
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
		return fmt.Errorf("booking service endpoint not ready: %w", lastErr)
	}
	return fmt.Errorf("booking service endpoint not ready")
}

func fetchBookingPublicCatalogStatus(address string, record webhookTokenRecord, timeout time.Duration) error {
	url := buildWebhookManagementURL(address, bookingPublicCatalogPath)
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("booking address is empty")
	}
	clientTimeout := timeout
	if clientTimeout <= 0 {
		clientTimeout = 2 * time.Second
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := computeWebhookSignature(record.ThirdPartyID, timestamp, record.Token, nil)
	req.Header.Set(webhookHeaderThirdPartyID, record.ThirdPartyID)
	req.Header.Set(webhookHeaderTimestamp, timestamp)
	req.Header.Set(webhookHeaderSignature, signature)
	resp, err := (&http.Client{Timeout: clientTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("booking catalog status http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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
	args := make([]string, 0, 24)
	args = append(args, "--public-addr", cfg.PublicAddr)
	args = append(args, "--admin-addr", cfg.AdminAddr)
	args = append(args, "--runtime", cfg.RuntimePath)
	args = append(args, "--catalog", cfg.CatalogPath)
	args = append(args, "--reservations", cfg.ReservationsPath)
	args = append(args, "--drafts", cfg.DraftsPath)
	args = append(args, "--security-keys", cfg.APIKeysPath)
	args = append(args, "--llm-config", cfg.LLMConfigPath)
	args = append(args, "--admin-auth", cfg.AdminAuthPath)
	return args
}

func startBookingHTTPService(ctx context.Context, cfg *bookingServiceServeConfig, out io.Writer) (*bookingServiceHandle, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	tokenRecords, err := loadSecurityTokenRecords(cfg.APIKeysPath, false)
	if err != nil {
		return nil, err
	}
	tokenCache, err := newWebhookTokenCache(cfg.APIKeysPath, defaultWebhookTokenRefreshInterval)
	if err != nil {
		return nil, err
	}
	idempotencyStore, err := newPublicIdempotencyStore(cfg.IdempotencyPath, defaultPublicIdempotencyTTL)
	if err != nil {
		return nil, err
	}
	tokenCache.Start()
	tokenCacheStarted := true
	defer func() {
		if tokenCacheStarted {
			tokenCache.Stop()
		}
	}()
	authCfg, err := loadDaemonAdminAuthConfig(cfg.AdminAuthPath)
	if err != nil {
		return nil, err
	}

	publicListener, err := net.Listen("tcp", cfg.PublicAddr)
	if err != nil {
		return nil, err
	}
	publicAddr := bookingListenerAddress(publicListener)
	adminListener, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		_ = publicListener.Close()
		return nil, err
	}
	adminAddr := bookingListenerAddress(adminListener)

	var handle *bookingServiceHandle
	requestShutdown := func(source string) {
		if out != nil {
			_, _ = fmt.Fprintf(out, "booking shutdown requested source=%s\n", strings.TrimSpace(source))
		}
		if handle != nil {
			_ = handle.Close()
		}
	}

	controller := &bookingServiceController{
		service:         newBookingService(cfg.CatalogPath, cfg.ReservationsPath),
		tokenRecords:    tokenRecords,
		tokenCache:      tokenCache,
		idempotency:     idempotencyStore,
		authConfig:      authCfg,
		authConfigPath:  strings.TrimSpace(cfg.AdminAuthPath),
		draftsPath:      cfg.DraftsPath,
		llmConfigPath:   cfg.LLMConfigPath,
		nowFn:           time.Now,
		parseWithLLM:    bookingParseIntentWithLLM,
		requestShutdown: requestShutdown,
	}

	publicMux := http.NewServeMux()
	adminMux := http.NewServeMux()
	controller.registerHandlers(publicMux, adminMux)
	publicServer := &http.Server{
		Addr:              cfg.PublicAddr,
		Handler:           publicMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	adminServer := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	nowText := time.Now().Format(time.RFC3339Nano)
	runtimeInfo := bookingServiceRuntimeInfo{
		PID:           os.Getpid(),
		Address:       publicAddr,
		PublicAddress: publicAddr,
		AdminAddress:  adminAddr,
		StartedAt:     nowText,
		UpdatedAt:     nowText,
	}
	if err := writeBookingRuntimeState(cfg.RuntimePath, runtimeInfo); err != nil {
		_ = publicListener.Close()
		_ = adminListener.Close()
		return nil, err
	}

	handle = &bookingServiceHandle{
		publicServer:   publicServer,
		publicListener: publicListener,
		adminServer:    adminServer,
		adminListener:  adminListener,
		runtimePath:    cfg.RuntimePath,
		tokenCache:     tokenCache,
		out:            out,
		errCh:          make(chan error, 2),
	}
	tokenCacheStarted = false
	serveOnce := func(name string, server *http.Server, listener net.Listener) {
		go func() {
			serveErr := server.Serve(listener)
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				if out != nil {
					_, _ = fmt.Fprintf(out, "booking service %s listener stopped with error: %v\n", name, serveErr)
				}
				handle.errCh <- serveErr
				return
			}
			handle.errCh <- nil
		}()
	}
	serveOnce("public", publicServer, publicListener)
	serveOnce("admin", adminServer, adminListener)

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
		if s.tokenCache != nil {
			s.tokenCache.Stop()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if s.publicServer != nil {
			if err := s.publicServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				closeErr = err
			}
		}
		if s.adminServer != nil {
			if err := s.adminServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				closeErr = err
			}
		}
		if s.publicListener != nil {
			_ = s.publicListener.Close()
		}
		if s.adminListener != nil {
			_ = s.adminListener.Close()
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
	var firstErr error
	for index := 0; index < 2; index++ {
		serveErr := <-s.errCh
		if serveErr != nil && firstErr == nil {
			firstErr = serveErr
		}
	}
	return firstErr
}

func bookingListenerAddress(listener net.Listener) string {
	if listener == nil {
		return ""
	}
	actualAddr := listener.Addr().String()
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		host := "127.0.0.1"
		if tcpAddr.IP != nil && !tcpAddr.IP.IsUnspecified() {
			host = tcpAddr.IP.String()
		}
		actualAddr = net.JoinHostPort(host, strconv.Itoa(tcpAddr.Port))
	}
	return actualAddr
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

func (c *bookingServiceController) registerHandlers(publicMux *http.ServeMux, adminMux *http.ServeMux) {
	if publicMux == nil && adminMux == nil {
		return
	}
	var adminStopOnce sync.Once

	newRequestID := func(r *http.Request) string {
		headerRequestID := ""
		if r != nil {
			headerRequestID = strings.TrimSpace(r.Header.Get(publicAPIHeaderRequestID))
		}
		if headerRequestID != "" {
			return headerRequestID
		}
		return nextPublicRequestID(c.now())
	}
	writePublicMethodNotAllowed := func(w http.ResponseWriter, requestID string) {
		_ = writePublicAPIError(w, requestID, newPublicAPIError(
			http.StatusMethodNotAllowed,
			publicAPIErrorCodeMethodNotAllowed,
			"method not allowed",
			nil,
		))
	}
	writePublicBusinessError := func(w http.ResponseWriter, requestID string, err error) {
		if err == nil {
			return
		}
		_ = writePublicAPIError(w, requestID, bookingBusinessErrorToPublic(err))
	}
	writePublicAuthError := func(w http.ResponseWriter, requestID string, err publicAPIError) {
		_ = writePublicAPIError(w, requestID, err)
	}

	requireAPIKey := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			requestID := newRequestID(r)
			authorized, authErr := c.isPublicAuthorized(r)
			if !authorized {
				c.logPublicAuthFailure(r, authErr.Code, authErr.Message, requestID)
				writePublicAuthError(w, requestID, authErr)
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

	registerPublic := func(path string, handler http.HandlerFunc, idempotent bool) {
		if publicMux == nil {
			return
		}
		wrapped := handler
		if idempotent {
			wrapped = applyPublicIdempotencyMiddleware(wrapped, c.idempotency, path)
		}
		wrapped = requireAPIKey(wrapped)
		publicMux.HandleFunc(path, wrapped)
	}
	registerAdminGet := func(path string, handler http.HandlerFunc) {
		if adminMux == nil {
			return
		}
		adminMux.HandleFunc(path, requireAdmin(handler))
	}
	registerAdminPost := func(path string, handler func(*http.Request) (any, error)) {
		if adminMux == nil {
			return
		}
		adminMux.HandleFunc(path, requireAdmin(func(w http.ResponseWriter, r *http.Request) {
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

	registerAdminGet(bookingAdminHomePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status, err := c.service.AdminStatus()
		if err != nil {
			http.Error(w, err.Error(), bookingHTTPStatus(err))
			return
		}
		adminAddress := strings.TrimSpace(r.Host)
		if adminAddress == "" {
			adminAddress = "127.0.0.1:18082"
		}
		view := bookingAdminHomeView{
			Name:          "longtradego booking",
			Now:           c.now().Format(time.RFC3339Nano),
			PublicAddress: "see /admin/booking/status",
			AdminAddress:  adminAddress,
			Status:        status,
			ManagementLinks: []bookingAdminHomeLink{
				{
					Label: "Booking admin status endpoint",
					Path:  bookingAdminStatusPath,
					Note:  "Structured JSON snapshot for automation.",
				},
				{
					Label: "Reservation confirm endpoint",
					Path:  bookingAdminReservationConfirmPath,
					Note:  "POST-only reservation state transition.",
				},
				{
					Label: "Reservation reject endpoint",
					Path:  bookingAdminReservationRejectPath,
					Note:  "POST-only reservation state transition.",
				},
				{
					Label: "Reservation cancel endpoint",
					Path:  bookingAdminReservationCancelPath,
					Note:  "POST-only reservation state transition.",
				},
				{
					Label: "Booking stop endpoint",
					Path:  bookingAdminStopPath,
					Note:  "POST-only graceful shutdown for current booking process.",
				},
			},
			StopActionPath:    bookingAdminStopPath,
			ConfirmActionPath: bookingAdminReservationConfirmPath,
			RejectActionPath:  bookingAdminReservationRejectPath,
			CancelActionPath:  bookingAdminReservationCancelPath,
		}
		renderBookingAdminHome(w, view)
	})

	registerAdminGet(bookingAdminHomeSlash, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		target := bookingAdminHomePath
		if raw := strings.TrimSpace(r.URL.RawQuery); raw != "" {
			target = target + "?" + raw
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})

	if adminMux != nil {
		adminMux.HandleFunc(bookingAdminStopPath, requireAdmin(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeBookingJSON(w, http.StatusAccepted, map[string]any{
				"status":  "stopping",
				"message": "booking shutdown requested",
			})
			if c.requestShutdown == nil {
				return
			}
			adminStopOnce.Do(func() {
				go c.requestShutdown("admin_stop")
			})
		}))
	}

	registerPublic(bookingPublicCatalogPath, func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID(r)
		if r.Method != http.MethodGet {
			writePublicMethodNotAllowed(w, requestID)
			return
		}
		query, err := parseDaemonBookingCatalogQuery(r)
		if err != nil {
			writePublicBusinessError(w, requestID, err)
			return
		}
		result, err := c.service.QueryCatalog(query)
		if err != nil {
			writePublicBusinessError(w, requestID, err)
			return
		}
		_ = writePublicAPISuccess(w, http.StatusOK, requestID, "", result)
	}, false)

	registerPublic(bookingPublicReservationsPath, func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID(r)
		switch r.Method {
		case http.MethodGet:
			filter := bookingReservationListFilter{
				UserID: strings.TrimSpace(r.URL.Query().Get("user_id")),
				Status: strings.TrimSpace(r.URL.Query().Get("status")),
			}
			reservations, err := c.service.ListReservations(filter)
			if err != nil {
				writePublicBusinessError(w, requestID, err)
				return
			}
			_ = writePublicAPISuccess(w, http.StatusOK, requestID, "", map[string]any{"reservations": reservations})
		case http.MethodPost:
			rawBody, readErr := readPublicBody(r, defaultWebhookMaxBodyBytes)
			if readErr != nil {
				apiErr, ok := readErr.(publicAPIError)
				if !ok {
					apiErr = newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, readErr.Error(), nil)
				}
				_ = writePublicAPIError(w, requestID, apiErr)
				return
			}
			r.Body = cloneReadCloserFromBytes(rawBody)
			var payload daemonBookingCreateReservationRequest
			if err := decodePublicJSONBody(rawBody, &payload); err != nil {
				apiErr, ok := err.(publicAPIError)
				if !ok {
					apiErr = newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, err.Error(), nil)
				}
				_ = writePublicAPIError(w, requestID, apiErr)
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
				writePublicBusinessError(w, requestID, err)
				return
			}
			_ = writePublicAPISuccess(w, http.StatusCreated, requestID, "", map[string]any{
				"reservation_id": reservation.ID,
				"reservation":    reservation,
			})
		default:
			writePublicMethodNotAllowed(w, requestID)
		}
	}, true)

	registerPublic(bookingPublicIntentParsePath, func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID(r)
		if r.Method != http.MethodPost {
			writePublicMethodNotAllowed(w, requestID)
			return
		}
		rawBody, readErr := readPublicBody(r, defaultWebhookMaxBodyBytes)
		if readErr != nil {
			apiErr, ok := readErr.(publicAPIError)
			if !ok {
				apiErr = newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, readErr.Error(), nil)
			}
			_ = writePublicAPIError(w, requestID, apiErr)
			return
		}
		r.Body = cloneReadCloserFromBytes(rawBody)
		var payload bookingIntentParseRequest
		if err := decodePublicJSONBody(rawBody, &payload); err != nil {
			apiErr, ok := err.(publicAPIError)
			if !ok {
				apiErr = newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, err.Error(), nil)
			}
			_ = writePublicAPIError(w, requestID, apiErr)
			return
		}
		payload.UserID = strings.TrimSpace(payload.UserID)
		payload.Content = strings.TrimSpace(payload.Content)
		payload.Channel = strings.TrimSpace(payload.Channel)
		if payload.UserID == "" || payload.Content == "" {
			_ = writePublicAPIError(w, requestID, newPublicAPIError(
				http.StatusBadRequest,
				publicAPIErrorCodeValidationError,
				"user_id and content are required",
				nil,
			))
			return
		}
		result, err := c.parseIntentDraft(r.Context(), payload)
		if err != nil {
			if errors.Is(err, errBookingLLMNotConfigured) {
				_ = writePublicAPIError(w, requestID, newPublicAPIError(
					http.StatusServiceUnavailable,
					publicAPIErrorCodeInternalError,
					err.Error(),
					nil,
				))
				return
			}
			writePublicBusinessError(w, requestID, err)
			return
		}
		_ = writePublicAPISuccess(w, http.StatusOK, requestID, "", map[string]any{
			"action":           result.Action,
			"draft_id":         result.DraftID,
			"updated_fields":   result.UpdatedFields,
			"override_applied": result.OverrideApplied,
			"draft":            result.Draft,
		})
	}, true)

	registerPublic(bookingPublicIntentConfirmPath, func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID(r)
		if r.Method != http.MethodPost {
			writePublicMethodNotAllowed(w, requestID)
			return
		}
		rawBody, readErr := readPublicBody(r, defaultWebhookMaxBodyBytes)
		if readErr != nil {
			apiErr, ok := readErr.(publicAPIError)
			if !ok {
				apiErr = newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, readErr.Error(), nil)
			}
			_ = writePublicAPIError(w, requestID, apiErr)
			return
		}
		r.Body = cloneReadCloserFromBytes(rawBody)
		var payload bookingIntentConfirmRequest
		if err := decodePublicJSONBody(rawBody, &payload); err != nil {
			apiErr, ok := err.(publicAPIError)
			if !ok {
				apiErr = newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, err.Error(), nil)
			}
			_ = writePublicAPIError(w, requestID, apiErr)
			return
		}
		result, err := c.confirmIntentDraft(payload)
		if err != nil {
			writePublicBusinessError(w, requestID, err)
			return
		}
		_ = writePublicAPISuccess(w, http.StatusCreated, requestID, "", result)
	}, true)

	registerAdminGet(bookingAdminStatusPath, func(w http.ResponseWriter, r *http.Request) {
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
	})

	registerAdminPost(bookingAdminProductUpsertPath, func(r *http.Request) (any, error) {
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

	registerAdminPost(bookingAdminProductRemovePath, func(r *http.Request) (any, error) {
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

	registerAdminPost(bookingAdminSlotUpsertPath, func(r *http.Request) (any, error) {
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

	registerAdminPost(bookingAdminSlotRemovePath, func(r *http.Request) (any, error) {
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

	registerAdminPost(bookingAdminReservationConfirmPath, func(r *http.Request) (any, error) {
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

	registerAdminPost(bookingAdminReservationRejectPath, func(r *http.Request) (any, error) {
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

	registerAdminPost(bookingAdminReservationCancelPath, func(r *http.Request) (any, error) {
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
	authorized, _ := c.isPublicAuthorized(r)
	return authorized
}

func (c *bookingServiceController) isPublicAuthorized(r *http.Request) (bool, publicAPIError) {
	if r == nil {
		return false, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeValidationError, "invalid request", nil)
	}
	thirdPartyID := normalizeThirdPartyID(r.Header.Get(webhookHeaderThirdPartyID))
	timestampText := strings.TrimSpace(r.Header.Get(webhookHeaderTimestamp))
	signatureText := strings.ToLower(strings.TrimSpace(r.Header.Get(webhookHeaderSignature)))
	return c.isBookingSignedAuthorized(r, thirdPartyID, timestampText, signatureText)
}

func (c *bookingServiceController) isBookingSignedAuthorized(
	r *http.Request,
	thirdPartyID string,
	timestampText string,
	signatureText string,
) (bool, publicAPIError) {
	if r == nil {
		return false, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeValidationError, "invalid request", nil)
	}
	if thirdPartyID == "" || timestampText == "" || signatureText == "" {
		return false, newPublicAPIError(http.StatusUnauthorized, publicAPIErrorCodeMissingRequiredHeaders, "missing required headers", nil)
	}
	timestamp, err := parseWebhookTimestamp(timestampText)
	if err != nil {
		return false, newPublicAPIError(http.StatusUnauthorized, publicAPIErrorCodeInvalidTimestampHeader, err.Error(), nil)
	}
	if !webhookTimestampWithinWindow(c.now(), timestamp, defaultWebhookTimestampSkew) {
		return false, newPublicAPIError(http.StatusUnauthorized, publicAPIErrorCodeTimestampOutsideWindow, "timestamp outside allowed window", nil)
	}
	record, found, err := c.findBookingTokenByThirdPartyID(thirdPartyID)
	if err != nil {
		return false, newPublicAPIError(http.StatusInternalServerError, publicAPIErrorCodeInternalError, fmt.Sprintf("token lookup failed: %v", err), nil)
	}
	if !found {
		return false, newPublicAPIError(http.StatusUnauthorized, publicAPIErrorCodeTokenNotFound, "token not found for third-party-id", nil)
	}
	if !securityRecordHasScope(record, securityScopeBooking) {
		return false, newPublicAPIError(http.StatusUnauthorized, publicAPIErrorCodeTokenScopeNotAllowed, "token scope not allowed for booking", nil)
	}
	body, err := readWebhookBody(r.Body, defaultWebhookMaxBodyBytes)
	if err != nil {
		return false, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, err.Error(), nil)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	expected := computeWebhookSignature(thirdPartyID, timestampText, record.Token, body)
	if !hmac.Equal([]byte(signatureText), []byte(expected)) {
		return false, newPublicAPIError(http.StatusUnauthorized, publicAPIErrorCodeSignatureVerification, "signature verification failed", nil)
	}
	return true, publicAPIError{}
}

func (c *bookingServiceController) findBookingTokenByThirdPartyID(thirdPartyID string) (webhookTokenRecord, bool, error) {
	normalizedID := normalizeThirdPartyID(thirdPartyID)
	if normalizedID == "" {
		return webhookTokenRecord{}, false, nil
	}
	if c.tokenCache != nil {
		record, found, err := c.tokenCache.Find(normalizedID)
		if err != nil {
			return webhookTokenRecord{}, false, err
		}
		if found {
			return record, true, nil
		}
	}
	if len(c.tokenRecords) == 0 {
		return webhookTokenRecord{}, false, nil
	}
	record, found := c.tokenRecords[normalizedID]
	return record, found, nil
}

func (c *bookingServiceController) logPublicAuthFailure(r *http.Request, code string, reason string, requestID string) {
	if strings.TrimSpace(reason) == "" {
		reason = "unauthorized"
	}
	if strings.TrimSpace(code) == "" {
		code = publicAPIErrorCodeInternalError
	}
	path := ""
	method := ""
	remote := ""
	thirdPartyID := ""
	if r != nil {
		path = strings.TrimSpace(r.URL.Path)
		method = strings.TrimSpace(r.Method)
		remote = strings.TrimSpace(r.RemoteAddr)
		thirdPartyID = normalizeThirdPartyID(r.Header.Get(webhookHeaderThirdPartyID))
	}
	log.Printf("booking public auth failed: request_id=%s method=%s path=%s remote=%s third_party_id=%s code=%s reason=%s", strings.TrimSpace(requestID), method, path, remote, thirdPartyID, code, reason)
}

func (c *bookingServiceController) isAdminAuthorized(r *http.Request) bool {
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

func renderBookingAdminHome(w http.ResponseWriter, view bookingAdminHomeView) {
	if w == nil {
		return
	}
	var buffer bytes.Buffer
	if err := bookingAdminHomeTemplate.Execute(&buffer, view); err != nil {
		http.Error(w, "render booking admin home failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, &buffer)
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

func bookingBusinessErrorToPublic(err error) publicAPIError {
	if err == nil {
		return newPublicAPIError(http.StatusOK, publicAPIErrorCodeOK, "", nil)
	}
	status := bookingHTTPStatus(err)
	switch status {
	case http.StatusConflict:
		return newPublicAPIError(status, publicAPIErrorCodeConflict, err.Error(), nil)
	case http.StatusBadRequest:
		return newPublicAPIError(status, publicAPIErrorCodeValidationError, err.Error(), nil)
	default:
		return newPublicAPIError(status, publicAPIErrorCodeInternalError, err.Error(), nil)
	}
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
