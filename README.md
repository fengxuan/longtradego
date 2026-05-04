# longtradego

A small Golang CLI demo for Longbridge OpenAPI, currently focused on quote queries and designed to be easy to extend with more commands.

## Features

- `cobra`-based CLI command structure
- `quote` command with multiple symbols
- `email` command for SMTP notifications
- `email receive` command for IMAP inbox polling and action trigger
- `email monitor` command for new-mail monitoring (polling)
- `mail monitor` cursor persistence (`data/mail_monitor_cursors.json`) for restart-safe UID continuation (daemon monitors are isolated by `MONITOR_ID`)
- `email analyze` command to print current mail element fields for downstream logic
- `email --to` alias lookup via `conf/email_aliases.json`
- `sys` command for Linux/system automation commands
- `version` command for build metadata (`version/commit/build date/platform`)
- `upgrade` command (`check` / install / dry-run) with GitHub Releases
- Automatic update reminder cache (`data/update_state.json`, max once per 24h check)
- `webhook` command for signature generation, test sending, managed lifecycle (`start/status/stop/kill-port`), and dual-surface endpoints (public `/webhook/*`; admin `/admin`, `/admin/healthz`, `/admin/readyz`, `/admin/webhook/status`, `/admin/metrics`, `/admin/webhook/stop`)
- `booking` command for product/slot/reservation/query management and independent dual-surface service lifecycle (`booking service start/status/stop`)
- Unified external security key config (`conf/security_keys.json`, scopes: `booking` / `webhook`)
- Cloudflare Named Tunnel example config for booking-only public exposure (`conf-example/cloudflared_booking_tunnel.yml`)
- `admin` command for daemon-admin runtime inspection (`admin status`)
- Daemon scheduled task management (`task add/list/pause/resume/global-pause/global-resume/remove`)
- Daemon task persistence across restarts (`conf/daemon_tasks.json`)
- Removed task backup history with deletion timestamp (`data/daemon_tasks_history.json`)
- Daemon mail monitor persistence across restarts (`conf/daemon_monitors.json`)
- Daemon-owned independent admin service runtime (`conf/admin_runtime.json`)
- Daemon admin Basic Auth config (`conf/admin_auth.json`)
- Symbol-first backward compatibility (`go run . AAPL.US TSLA.US`)
- OAuth client id flow support (`LONGBRIDGE_CLIENT_ID`)
- Reuse quote session across commands; reconnect lazily only when a running command hits session expiration
- Structured command execution logs
- Log file rotation when `logs/command.log` exceeds `5MB`

## Project Layout

- `cmd/longtradego/main.go`: primary CLI entrypoint (`go run ./cmd/longtradego`)
- `main.go`: thin compatibility entrypoint (`go run .`)
- `internal/cli`: root command assembly and execution lifecycle
- `internal/core`: shared app context, args normalization, logging, atomic IO
- `internal/service`: booking/webhook/daemon/admin/email/system/version/upgrade/task implementations
- `scripts/booking_public_start.sh`: start booking service for stable public tunnel origin (`127.0.0.1:18081`, no port fallback)

## Requirements

- Go `1.24+`
- Longbridge credentials

## Environment

Use `.env` in project root:

```env
LONGBRIDGE_CLIENT_ID=your_client_id
# Optional, default is 60355
# LONGBRIDGE_CALLBACK_PORT=60355

# SMTP for `email send`
# SMTP_HOST=smtp.example.com
# SMTP_PORT=587
# SMTP_USERNAME=your_account@example.com
# SMTP_PASSWORD=your_password_or_app_password
# SMTP_FROM=your_account@example.com

# Optional monitor defaults (mail monitor)
# IMAP_MONITOR_MODE=hybrid
# IMAP_MONITOR_LIMIT=20
# IMAP_MONITOR_CONNECT_TIMEOUT=10s
# IMAP_MONITOR_POLL_INTERVAL=15s
# IMAP_MONITOR_FALLBACK_POLL_INTERVAL=2m
# IMAP_MONITOR_BODY_MAX_BYTES=20000

```

Notes:

- On first OAuth run, CLI prints an authorization URL.
- Token is managed by Longbridge SDK and persisted locally.
- IMAP for `mail receive/monitor/analyze` reads `conf/mail_receive_setting.json` only (no IMAP credential fallback from `.env`).
- Booking intent parse (`/booking/intents/parse`) reads `conf/booking_llm.json` only (no LLM credential fallback from environment variables).

## Run

Install dependencies:

```bash
go mod tidy
```

Run quote command:

```bash
go run ./cmd/longtradego quote AAPL.US TSLA.US 700.HK
```

Alias:

```bash
go run ./cmd/longtradego q AAPL.US TSLA.US
```

Version and upgrade:

```bash
# Build metadata
go run ./cmd/longtradego version

# Check latest release
go run ./cmd/longtradego upgrade check

# Dry-run planned upgrade (no binary replacement)
go run ./cmd/longtradego upgrade --dry-run --yes

# Install latest release (binary mode)
longtradego upgrade
```

Upgrade notes:

- `upgrade` downloads release assets from GitHub Releases and verifies `checksums.txt` before replacement.
- `upgrade` refuses to run while daemon is running; stop daemon first.
- `upgrade` refuses `go run .` execution mode; install and run released binary first.
- Automatic update checks are cached in `data/update_state.json` and throttled to at most once per 24 hours.
- Update reminder text is shown only in interactive terminals (non-interactive runs stay silent).

Send email notification:

```bash
go run . email send \
  --to "alice@example.com,bob@example.com" \
  --subject "Longtrade Notification" \
  --body "AAPL reached target price."
```

Send email by recipient alias (`--to` can be alias or email):

```bash
go run . email send \
  --to "qa,dev" \
  --subject "Department Alert" \
  --body "Pipeline finished."
```

Send email with body file:

```bash
go run . email send \
  --to "alice@example.com" \
  --subject "Daily Report" \
  --body-file ./report.txt
```

Receive emails via IMAP (alias: `mail recv`):

```bash
go run . email receive --limit 10 --unread-only
go run . mail recv --subject-contains "ALERT" --from-contains "ops@"
go run . mail recv --mail-alias main
```

Trigger a system command when matching mails are found:

```bash
go run . mail recv \
  --subject-contains "DEPLOY_OK" \
  --action-cmd 'echo got ${MAIL_COUNT} mail(s) from ${MAILBOX}'
```

Fetch message content and save attachments to files:

```bash
go run . mail recv \
  --with-body \
  --with-files \
  --files-dir ./logs/mail_files
```

Monitor new incoming mail and trigger follow-up commands:

```bash
go run . mail monitor --poll-interval 15s --wait-timeout 10m --with-body
go run . mail monitor --mode idle --poll-interval 30s --wait-timeout 10m
go run . mail monitor --mode hybrid --poll-interval 30s --fallback-poll-interval 2m --wait-timeout 30m
go run . mail monitor --mode hybrid --once --wait-timeout 10m
go run . mail monitor --mail-alias main --wait-timeout 10m
```

`mail monitor` mode guide:

- `--mode poll`: pure polling
- `--mode idle`: IMAP IDLE first (server fallback handled by go-imap)
- `--mode hybrid` (default): IDLE + low-frequency fallback polling
- If `--mode` is empty, it defaults to `hybrid`
- Adaptive polling: if 3 consecutive polls find no new mail, interval increases by `+5s`; when new mail is detected, interval resets to initial value (default `15s`); max interval is `300s`

Analyze mail elements (temporary loop-print for business prep):

```bash
go run . mail analyze --limit 5 --with-files --files-dir ./logs/mail_files
```

Daemon pipeline example (new mail -> analyze):

```text
longtradego> mail monitor --poll-interval 15s --wait-timeout 5m --with-body | mail analyze
```

Email alias config file (`conf/email_aliases.json`):

```json
{
  "aliases": {
    "qa": ["qa@example.com"],
    "dev": ["dev1@example.com", "dev2@example.com"],
    "trading": ["trading@example.com"]
  }
}
```

IMAP settings file (`conf/mail_receive_setting.json`):

```json
{
  "default_alias": "main",
  "aliases": {
    "main": {
      "IMAP_HOST": "imap.example.com",
      "IMAP_PORT": 993,
      "IMAP_USERNAME": "your_account@example.com",
      "IMAP_PASSWORD": "your_password_or_app_password",
      "IMAP_MAILBOX": "INBOX",
      "IMAP_TLS": true,
      "IMAP_INSECURE_SKIP_VERIFY": false,
      "IMAP_ID_NAME": "longtradego",
      "IMAP_ID_VERSION": "1.0.0",
      "IMAP_ID_VENDOR": "longtradego",
      "IMAP_ID_ADDRESS": "your_account@example.com"
    }
  }
}
```

- Use `--mail-alias <alias>` to pick a mailbox profile.
- If `--mail-alias` is omitted, `default_alias` is used.

Run Linux/system command directly:

```bash
go run . sys -- ls -la
go run . sys --shell "uname -a && date"
```

`sys --shell` uses `$SHELL` when available (falls back to `zsh`, then `sh`).
`sys` writes execution logs (stdout/stderr/exit code) to `logs/system_command.log` and does not print command output to terminal, to avoid interfering with interactive input.
For readable JSON payloads, keep using `stdout`/`stderr` for raw text compatibility and prefer `stdout_json` / `stderr_json` when present.

Webhook lifecycle and usage:

```bash
# Start webhook service in background (non-blocking)
go run . webhook start --addr :8080 --path /webhook/events

# Query background runtime status
go run . webhook status

# Stop background webhook service
go run . webhook stop

# Kill local processes occupying target webhook port (TERM then KILL if needed)
go run . webhook kill-port --addr :8080

# Preview matched pids only (no kill)
go run . webhook kill-port --addr :8080 --dry-run
```

`webhook kill-port` behavior:

- Finds listening pids by target port, tries graceful `TERM` first, then escalates to `KILL` on timeout.
- Cleans runtime state only when runtime `pid` matches one of the stopped pids.

Webhook route management (single service, multi-routes):

```bash
# Add one sync route
go run . webhook route add r-sync \
  --path /webhook/orders \
  --mode sync \
  --pipeline "webhook sign --third-party-id p1 --token t1 --data '{\"ok\":1}'"

# Add one async route
go run . webhook route add r-async \
  --path /webhook/alerts \
  --mode async \
  --pipeline "email send --to ops@example.com --subject 'Webhook Alert'"

# List / update / remove routes
go run . webhook route list
go run . webhook route update r-async --timeout 45s --max-attempts 8
go run . webhook route remove r-async r-async
```

Compatibility note:

- If no `webhook route` config exists, `webhook start` auto creates a legacy default route from `--path` (default `/webhook/events`), so existing integrations continue to work.

Webhook start mode (background only):

```bash
go run . webhook start \
  --public-addr :8080 \
  --admin-addr 127.0.0.1:8081 \
  --path /webhook/events
```

Start behavior notes:

- Runtime/log paths remain relative to current workspace (`conf/`, `data/`, `logs/`).
- Webhook runtime state now defaults to `data/webhook_runtime.json` (legacy `conf/webhook_runtime.json` is auto-migrated on read when using default runtime path).
- `webhook start` enforces best-effort single service on the same address pair (`--public-addr` + `--admin-addr`): if either port is already in use, it returns `already_running` and does not spawn a new process.
- After spawn, startup is verified by checking process liveness and management endpoint readiness to avoid false-positive "started" states.

Webhook admin/health endpoints (admin listener only, default `127.0.0.1:8081`):

```bash
curl http://127.0.0.1:8081/admin
curl http://127.0.0.1:8081/admin/healthz
curl http://127.0.0.1:8081/admin/readyz
curl http://127.0.0.1:8081/admin/webhook/status
curl http://127.0.0.1:8081/admin/metrics
# stop current webhook instance (POST only)
curl -X POST http://127.0.0.1:8081/admin/webhook/stop
```

Public listener security behavior:

- `:8080` only serves webhook business routes (`/webhook/*`).
- Accessing `/admin/*`, `/healthz`, `/readyz`, `/metrics` on the public listener returns `404`.

Admin stop behavior:

- `/admin` includes a **Stop Current Webhook** button with browser confirm dialog to reduce mis-clicks.
- `POST /admin/webhook/stop` only requests shutdown for the current webhook process.
- Runtime file cleanup is best-effort and only removes runtime state when runtime `pid` matches the current process PID.

Generate a signed webhook request package:

```bash
go run . webhook sign \
  --third-party-id partner-a \
  --token your_raw_token \
  --data '{"hello":"world"}'
```

For cross-language client integration (JS/Python helper functions, signing rules, and troubleshooting), see [Public API Signing Guide](#public-api-signing-guide).

Quickly send a signed webhook test request:

```bash
go run . webhook send \
  --url http://127.0.0.1:8080/webhook/events \
  --third-party-id partner-a \
  --token your_raw_token \
  --data '{"hello":"world"}'

# send to a route path
go run . webhook send \
  --url http://127.0.0.1:8080/webhook/orders \
  --third-party-id partner-a \
  --token your_raw_token \
  --data '{"order_id":"o-1001"}'
```

Webhook `POST` example:

```bash
curl -X POST http://127.0.0.1:8080/webhook/events \
  -H "X-Third-Party-ID: partner-a" \
  -H "X-Webhook-Timestamp: 1710000000" \
  -H "X-Webhook-Token: <signature>" \
  -H "Content-Type: application/json" \
  -d '{"hello":"world"}'
```

### Public API Signing Guide

This guide applies to all public signed endpoints:

- Webhook public routes (for example `/webhook/events`, `/webhook/<route-path>`)
- Booking public routes (for example `/booking/catalog`, `/booking/reservations`, `/booking/intents/parse`, `/booking/intents/confirm`)

Authoritative spec:

- OpenAPI: [`docs/openapi/public_api.yaml`](docs/openapi/public_api.yaml)

Required signing headers:

- `X-Third-Party-ID`
- `X-Webhook-Timestamp` (Unix seconds, UTC)
- `X-Webhook-Token`

Required idempotency header on **public POST**:

- `Idempotency-Key`

Signature algorithm (same as `webhook sign` CLI):

- `sha256(third_party_id + "\n" + timestamp + "\n" + token + "\n" + raw_body_bytes)`
- Output: lowercase hex string

Timestamp rule:

- Server validates `+-5 minutes` around server time.
- Generate and send immediately.

Raw body rule:

- Sign exact outgoing bytes.
- Do not sign one JSON string and send another re-serialized payload.

Unified public response envelope:

- Success:
  - `{"status":"ok","code":"ok","message":"","data":...,"request_id":"...","ts":"RFC3339Nano"}`
- Error:
  - `{"status":"error","code":"<machine_code>","message":"<human_message>","details":...,"request_id":"...","ts":"RFC3339Nano"}`

Core machine codes:

- `missing_required_headers`
- `invalid_timestamp_header`
- `timestamp_outside_allowed_window`
- `token_not_found`
- `token_scope_not_allowed`
- `signature_verification_failed`
- `invalid_json_body`
- `method_not_allowed`
- `validation_error`
- `conflict`
- `internal_error`
- `idempotency_key_required`
- `idempotency_key_conflict`
- `idempotency_store_error`

JavaScript helper (Node.js 18+):

```javascript
const crypto = require("node:crypto");

function buildSignedHeaders({ thirdPartyId, token, bodyJsonString, timestamp, idempotencyKey }) {
  const ts = String(timestamp ?? Math.floor(Date.now() / 1000));
  const payload = `${thirdPartyId}\n${ts}\n${token}\n${bodyJsonString}`;
  const signature = crypto.createHash("sha256").update(payload, "utf8").digest("hex");
  return {
    "X-Third-Party-ID": thirdPartyId,
    "X-Webhook-Timestamp": ts,
    "X-Webhook-Token": signature,
    "Idempotency-Key": idempotencyKey,
  };
}

async function sendSignedRequest({ url, thirdPartyId, token, data, timestamp, idempotencyKey }) {
  const bodyJsonString = JSON.stringify(data);
  const signedHeaders = buildSignedHeaders({
    thirdPartyId,
    token,
    bodyJsonString,
    timestamp,
    idempotencyKey,
  });
  const response = await fetch(url, {
    method: "POST",
    headers: {
      ...signedHeaders,
      "Content-Type": "application/json",
    },
    body: bodyJsonString,
  });
  const envelope = await response.json().catch(() => ({}));
  return {
    status: response.status,
    code: envelope.code || "",
    requestId: envelope.request_id || "",
    envelope,
  };
}
```

Python helper:

```python
import hashlib
import json
import time
import requests

def build_signed_headers(third_party_id, token, body_json_string, idempotency_key, timestamp=None):
    ts = str(int(timestamp if timestamp is not None else time.time()))
    payload = f"{third_party_id}\n{ts}\n{token}\n".encode("utf-8") + body_json_string.encode("utf-8")
    signature = hashlib.sha256(payload).hexdigest()
    return {
        "X-Third-Party-ID": third_party_id,
        "X-Webhook-Timestamp": ts,
        "X-Webhook-Token": signature,
        "Idempotency-Key": idempotency_key,
    }

def send_signed_request(url, third_party_id, token, data, idempotency_key, timestamp=None, timeout=10):
    body_json_string = json.dumps(data, ensure_ascii=False, separators=(",", ":"))
    headers = build_signed_headers(third_party_id, token, body_json_string, idempotency_key, timestamp)
    headers["Content-Type"] = "application/json"
    response = requests.post(url, headers=headers, data=body_json_string.encode("utf-8"), timeout=timeout)
    try:
        envelope = response.json()
    except ValueError:
        envelope = {}
    return response.status_code, envelope
```

CLI parity check with `webhook sign`:

```bash
cat > /tmp/payload.json <<'EOF'
{"hello":"world"}
EOF

TS=$(date +%s)

go run . webhook sign \
  --third-party-id partner-a \
  --token your_raw_token \
  --timestamp "$TS" \
  --data-file /tmp/payload.json
```

Compare your app signature with CLI output `signature`. They must match.

Common mismatch causes:

- Expired timestamp (outside +-5 minutes)
- Signed bytes differ from sent bytes
- Wrong token for `third_party_id`
- `third_party_id` whitespace/case mismatch

401/4xx quick troubleshooting:

| Code | Immediate checks | Typical fix |
| --- | --- | --- |
| `missing_required_headers` | Are all signing headers present? | Always send all three signing headers. |
| `invalid_timestamp_header` | Unix seconds string format? | Send integer seconds, not ms/RFC3339. |
| `timestamp_outside_allowed_window` | Clock skew / send delay? | Sync time, regenerate timestamp, resend. |
| `token_not_found` | Does `third_party_id` exist in `conf/security_keys.json`? | Create/reset token for that ID. |
| `token_scope_not_allowed` | Does token scope include target API? | Add scope `booking`, `webhook`, or both. |
| `signature_verification_failed` | Sign inputs exactly matched? | Re-sign exact payload with correct token. |
| `idempotency_key_required` | Is `Idempotency-Key` set on POST? | Always send unique key for each logical write operation. |
| `idempotency_key_conflict` | Same key reused for different body? | Use a new key when payload changes. |

Agent retry strategy:

- Safe auto-retry: `GET` + `POST` requests with stable `Idempotency-Key`.
- Do not retry with a changed payload under the same idempotency key.
- Use `request_id` + `code` in logs for fast provider-side support.

Webhook downstream processing model:

- Route mode `sync`: execute configured downstream pipeline in request path and return `200` on success.
- Route mode `async`: enqueue and return `202` immediately; background worker retries with backoff, then dead-letters on max attempts.
- Downstream allowlist is enabled by default; `sys/shell` requires explicit `--allow-sys-downstream`.
- Webhook endpoints only accept `POST`; non-POST requests return `405`.
- Token verification uses in-memory cache with periodic refresh from `conf/security_keys.json`.

Webhook logs and audit:

- Accepted events: `data/webhook_events.json` (`meta + data`)
- Async queue: `data/webhook_dispatch_queue.json`
- Async history: `data/webhook_dispatch_history.jsonl`
- Dead letter: `data/webhook_dead_letters.jsonl`
- Full request/response audit (raw headers/body + response): `logs/webhook_audit.log` (with rotation)
- Event/audit writes are buffered asynchronously (batch flush) and use sync fallback when queue is full.
- Backward-compatible raw fields are kept (`request_body`, `response_body`), while structured fields (`request_json`, `response_json`) are preferred for debugging and search.

Quick lookup examples (jq):

```bash
# recent webhook send results from command log (event id + status)
jq -r 'select(.request.command=="webhook" and .request.symbols[0]=="send") | [.timestamp,.result.response_event_id,.result.http_status,.result.response_meta_error] | @tsv' logs/command.log | tail -n 20

# locate one audit record by response_event_id
jq -r 'select(.response_event_id=="evt-1777592565341649000-0d5220c2") | [.timestamp,.path,.response_status,.response_third_party_id,.response_meta_error] | @tsv' logs/webhook_audit.log

# inspect structured request json directly (without escaped \\n noise)
jq -r 'select(.response_event_id=="evt-1777592565341649000-0d5220c2") | .request_json' logs/webhook_audit.log

# inspect structured response json directly (without escaped \\n noise)
jq -r 'select(.response_event_id=="evt-1777592565341649000-0d5220c2") | .response_json' logs/webhook_audit.log

# quickly find audit lines whose request body is valid json
jq -r 'select(.request_json_valid==true) | [.timestamp,.event_id,.path,.response_status] | @tsv' logs/webhook_audit.log

# find sys command results that emitted structured json in stdout
jq -r 'select(.result.stdout_json_valid==true) | [.timestamp,.result.command_line,.result.exit_code] | @tsv' logs/system_command.log
```

Webhook token management:

```bash
go run . webhook token generate partner-a --scope both
go run . webhook token query partner-a
go run . webhook token reset partner-a --scope webhook
```

Use `--security-keys` as the only token file flag.

Backward compatible symbol-first mode:

```bash
go run . AAPL.US TSLA.US
```

Daemon mode (stay alive and execute multiple commands):

```bash
go run . daemon
```

Then inside prompt:

```text
longtradego> quote AAPL.US TSLA.US 700.HK
longtradego> email send --to alice@example.com --subject "Alert" --body "hello"
longtradego> sys --shell "uptime"
longtradego> q NVDA.US
longtradego> monitor list
longtradego> monitor start m-1
longtradego> monitor stop m-1
longtradego> monitor remove m-2 m-2
longtradego> monitor stop all
longtradego> mail monitor list
longtradego> mail monitor start m-1
longtradego> mail monitor stop m-1
longtradego> mail monitor remove m-2 m-2
longtradego> exit
```

When daemon starts, it checks current workspace webhook runtime and prints a hint for `running` / `stale` state (including pid/addr/path and owner summary when available).

Daemon also starts an independent admin service (default base address `:18080`) for task/monitor/webhook unified management:

- It tries `:18080` first, then auto-fallbacks to `+1 ... +20` if port is occupied.
- If all candidate ports are unavailable, daemon keeps running and prints a warning.
- This admin service is independent from webhook lifecycle, so when webhook is stopped, task/monitor admin page still works.
- Runtime state is saved to `conf/admin_runtime.json`, and daemon exit only cleans this file when runtime `pid` matches current daemon process.

Check admin runtime quickly:

```bash
go run . admin status
```

Daemon admin endpoints (default port shown as `18080`, actual port may fallback):

```bash
curl -u admin:your-password http://127.0.0.1:18080/admin
curl -u admin:your-password http://127.0.0.1:18080/admin/status
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/webhook/start
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/webhook/stop
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/webhook/kill-port
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/task/global-pause
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/task/global-resume
curl -u admin:your-password -X POST -d \"id=task-1\" http://127.0.0.1:18080/admin/task/pause
curl -u admin:your-password -X POST -d \"id=task-1\" http://127.0.0.1:18080/admin/task/resume
curl -u admin:your-password -X POST -d \"id=m-1\" http://127.0.0.1:18080/admin/monitor/start
curl -u admin:your-password -X POST -d \"id=m-1\" http://127.0.0.1:18080/admin/monitor/stop
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/monitor/start-all
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/monitor/stop-all
curl -u admin:your-password http://127.0.0.1:18080/admin/booking/service/status
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/booking/service/start
curl -u admin:your-password -X POST http://127.0.0.1:18080/admin/booking/service/stop
```

The daemon admin page (`/admin`) intentionally hides the `kill-port` button to reduce accidental high-risk actions; use CLI (`webhook kill-port`) or direct API call when needed.

Booking system (independent service, system-first + client-ready):

- Runtime data files:
  - `data/booking_catalog.json` (products + slots)
  - `data/booking_reservations.json` (reservations + state transitions)
- Reservation states: `pending -> confirmed | rejected | cancelled`
- Capacity rule: `available_capacity = slot.capacity - sum(confirmed.party_size)`
- Default query behavior: `/booking/catalog` only returns slots with `available_capacity > 0`; use `include_full=true` to include full slots.
- Runtime file: `data/booking_runtime.json` (service pid/public+admin address/state)
- Draft intake file: `data/booking_intake_drafts.json` (text parse drafts)

Start/stop/status booking service (public default `:18081`, admin default `127.0.0.1:18082`, fallback `+1...+20` with same offset on both):

```bash
go run . booking service start
go run . booking service status
go run . booking service stop
# optional custom config path
go run . booking service start --llm-config conf/booking_llm.json
```

Use `--security-keys` as the only booking key flag.

For public exposure via Cloudflare Tunnel, use fixed origin address and disable port fallback:

```bash
go run . booking service start \
  --public-addr 127.0.0.1:18081 \
  --admin-addr 127.0.0.1:18082 \
  --max-port-fallback 0 \
  --security-keys conf/security_keys.json
# or helper script
./scripts/booking_public_start.sh
```

Booking service security config (fail-closed on missing/invalid config):

- `conf/security_keys.json` (unified external tokens, scopes: `booking` / `webhook`)
- `conf/booking_llm.json` (OpenAI-compatible parse config: `api_key`, `base_url`, optional `model`)
- `conf/admin_auth.json` (booking admin basic auth, shared with daemon admin)

Booking CLI examples:

```bash
# product
go run . booking product add --id p-1 --name "Morning Session" --enabled=true
go run . booking product list
go run . booking product remove p-1

# slot
go run . booking slot add --id slot-1 --product-id p-1 --start 2026-05-03T10:00:00+08:00 --end 2026-05-03T11:00:00+08:00 --capacity 10 --enabled=true
go run . booking slot list --include-full --include-disabled
go run . booking slot remove slot-1

# reservation
go run . booking reservation create --product-id p-1 --slot-id slot-1 --user-id u-1 --party-size 2 --contact-name Alice --contact-phone 13800138000 --member Alice --member Bob --special-requirements "Window seat"
go run . booking reservation list --user-id u-1 --status pending
go run . booking reservation confirm r-1 --note "confirmed by system"
go run . booking reservation reject r-2 --note "no capacity"
go run . booking reservation cancel r-3 --note "user canceled"

# user-view query
go run . booking query --product-id p-1 --from 2026-05-03T00:00:00+08:00 --to 2026-05-03T23:59:59+08:00

# booking agent (simulate external AI flow: parse -> confirm)
go run . booking agent reserve \
  --user-id u-1 \
  --content "我想明天上午两个人预约产品 p-1" \
  --third-party-id partner-a \
  --security-keys conf/security_keys.json \
  --url http://127.0.0.1:18081

# if parse still has missing fields, provide overrides
go run . booking agent reserve \
  --user-id u-1 \
  --content "我想明天上午两个人预约" \
  --third-party-id partner-a \
  --security-keys conf/security_keys.json \
  --slot-id slot-1 \
  --contact-phone 13800138000
```

`booking agent reserve` behavior:

- Calls `/booking/intents/parse` then `/booking/intents/confirm` using unified signed headers.
- Uses `--token` first; if empty, reads token from `--security-keys` by `third_party_id` and requires `booking` scope.
- Uses runtime public address when `--url` is empty; if service is not running, command fails and asks to run `booking service start`.
- Always sends `Idempotency-Key` to both POSTs (`<prefix>-parse` / `<prefix>-confirm`).
- If required fields are still missing after overrides, command stops before confirm and prints `missing_fields` with recommended flags.

Booking service APIs:

- Public base URL default: `http://127.0.0.1:18081` (actual port may fallback).
- Admin base URL default: `http://127.0.0.1:18082` (same fallback offset as public).
- Public endpoints accept unified signed headers (see [Public API Signing Guide](#public-api-signing-guide)):
  - `X-Third-Party-ID`
  - `X-Webhook-Timestamp`
  - `X-Webhook-Token` (same signature algorithm as webhook API)
- `GET /booking/catalog?product_id=<id>&from=<RFC3339>&to=<RFC3339>&include_full=<bool>`
- `POST /booking/reservations` (JSON body)
- `GET /booking/reservations?user_id=<id>&status=<pending|confirmed|rejected|cancelled>`
- `POST /booking/intents/parse` (text -> draft via OpenAI-compatible API)
- `POST /booking/intents/confirm` (confirm draft -> create pending reservation)
- If `conf/booking_llm.json` is missing/invalid, `POST /booking/intents/parse` returns `503` while other booking APIs remain available.

Intent parse follow-up behavior:

- `POST /booking/intents/parse` auto-detects an existing draft for the same user and tries to continue it (instead of always creating a new one).
- Match policy: same `user_id` and (if provided) same `channel` first; fallback to same `user_id`; only drafts within 24 hours are eligible.
- Merge policy: always fill missing fields; overwrite existing fields only when new parse confidence is high (`new >= 0.80` and `new-old >= 0.10`).
- Parse response keeps `status + draft` and adds:
  - `action`: `created | continued | continued_no_change`
  - `draft_id`
  - `updated_fields`
  - `override_applied`

`POST /booking/reservations` JSON schema:

```json
{
  "product_id": "p-1",
  "slot_id": "slot-1",
  "user_id": "u-1",
  "party_size": 2,
  "personnel": {
    "contact_name": "Alice",
    "contact_phone": "13800138000",
    "members": ["Alice", "Bob"]
  },
  "special_requirements": "Window seat"
}
```

Intent parse/confirm example:

```bash
curl -X POST http://127.0.0.1:18081/booking/intents/parse \
  -H "X-Third-Party-ID: partner-a" \
  -H "X-Webhook-Timestamp: 1710000000" \
  -H "X-Webhook-Token: <signature>" \
  -H "Content-Type: application/json" \
  -d '{"user_id":"u-1","channel":"chat","content":"我想明天上午两个人预约产品 p-1"}'

curl -X POST http://127.0.0.1:18081/booking/intents/confirm \
  -H "X-Third-Party-ID: partner-a" \
  -H "X-Webhook-Timestamp: 1710000000" \
  -H "X-Webhook-Token: <signature>" \
  -H "Content-Type: application/json" \
  -d '{"draft_id":"d-1","slot_id":"slot-1"}'
```

Admin booking APIs (Basic Auth required, hosted by booking service):

- `GET /admin` (Booking Admin Home HTML)
- `GET /admin/` (redirect to `/admin`)
- `POST /admin/booking/stop` (graceful stop current booking process, async `202`)
- `GET /admin/booking/status`
- `POST /admin/booking/product/upsert`
- `POST /admin/booking/product/remove`
- `POST /admin/booking/slot/upsert`
- `POST /admin/booking/slot/remove`
- `POST /admin/booking/reservation/confirm`
- `POST /admin/booking/reservation/reject`
- `POST /admin/booking/reservation/cancel`

HTTP semantics:

- `/booking/*` requires unified signed headers.
- `/admin/*` requires Basic Auth (`conf/admin_auth.json`).
- Booking public listener does not expose admin routes (`/admin/*` returns `404` on public address).
- Action endpoints are POST-only (`405` on wrong method).
- Validation errors return `400`; capacity conflicts return `409`.
- Booking Admin Home default URL: `http://127.0.0.1:18082/admin`.

### Expose Booking Public APIs via Cloudflare Named Tunnel

This setup exposes only `/booking/*` from booking public listener and blocks `/admin/*` at tunnel ingress level.

1. Start booking service on fixed local origin:

```bash
./scripts/booking_public_start.sh
```

2. Create a remotely-managed Cloudflare Tunnel and a hostname (for example `booking-api.<your-domain>`), mapped to booking public listener only:

```text
http://127.0.0.1:18081
```

3. Configure ingress rules (order matters: block admin -> allow booking -> deny all):

```yaml
ingress:
  - hostname: booking-api.example.com
    path: ^/admin(/.*)?$
    service: http_status:403
  - hostname: booking-api.example.com
    path: ^/booking(/.*)?$
    service: http://127.0.0.1:18081
  - hostname: booking-api.example.com
    service: http_status:404
  - service: http_status:404
```

Template file: `conf-example/cloudflared_booking_tunnel.yml`.

4. Run `cloudflared` as service (token mode), for example:

```bash
sudo cloudflared service install <TUNNEL_TOKEN>
sudo systemctl enable --now cloudflared
sudo systemctl status cloudflared
```

5. Client integration:

- Public base URL: `https://booking-api.<your-domain>`
- Keep using the same signed headers and algorithm from [Public API Signing Guide](#public-api-signing-guide).
- Public integrations must use unified signed headers.

6. Verification checklist:

```bash
# admin path should be blocked by tunnel ingress
curl -i https://booking-api.<your-domain>/admin/booking/status

# unknown path should return 404
curl -i https://booking-api.<your-domain>/not-allowed
```

For signed booking endpoint checks, reuse the JS/Python helper methods in [Public API Signing Guide](#public-api-signing-guide) and only replace URL with:

```text
https://booking-api.<your-domain>/booking/intents/parse
```

7. Rollback:

```bash
sudo systemctl stop cloudflared
```

This removes public ingress immediately while keeping local booking service running.

Daemon admin auth config (`conf/admin_auth.json`, hash-first):

```json
{
  "username": "admin",
  "password_hash": "$2a$12$jkV7cbbJU1CDJw.GWy2rYOCbjfActfvTzgTnfnjS3Zg1p5UylgkLK"
}
```

Legacy compatibility: `password` (plaintext) is still readable for migration, but `password_hash` takes priority when both fields exist.

Admin password operations:

- `admin auth set-password --config conf/admin_auth.json --username admin --password 'new-password'`
- `admin auth migrate --config conf/admin_auth.json` (migrate legacy plaintext to hash-only)
- `admin auth reset-password --generate --config conf/admin_auth.json --username admin`
- `admin auth verify --config conf/admin_auth.json --username admin --password 'candidate-password'`
- Raw/original password is not recoverable from `password_hash`; only verify/reset/generate are supported.

Unified security key config (`conf/security_keys.json`):

```json
{
  "version": 1,
  "tokens": [
    {
      "third_party_id": "partner-a",
      "token": "replace-with-strong-random-token",
      "scopes": ["booking", "webhook"],
      "created_at": "2026-05-03T00:00:00Z",
      "updated_at": "2026-05-03T00:00:00Z"
    }
  ]
}
```

Booking LLM config (`conf/booking_llm.json`, OpenAI-compatible):

```json
{
  "api_key": "your-openai-compatible-key",
  "base_url": "https://api.openai.com",
  "model": "gpt-4.1-mini"
}
```

`base_url` supports root URL, `/v1`, or full chat endpoint. The service normalizes it to a single `.../chat/completions` request URL (no duplicated `/v1`).

If `conf/admin_auth.json` is missing or invalid, daemon keeps running but skips daemon admin service startup.

When daemon exits (`exit` / EOF), it closes runtime contexts/connections and only cleans webhook process if ownership matches current daemon session (`owner_session_id` + `owner_start_token` double check). This avoids stopping webhook instances started by other daemon sessions or external terminals.

Daemon interactive shortcuts:

- `↑` / `↓`: history navigation (last executed commands)
- `←` / `→`: move cursor within current input line for editing
- `TAB`: command/flag auto completion (supports pipeline stages after `|`, e.g. `quote AAPL.US | em` + TAB -> `email`; `task pause ` + TAB -> task IDs; `task ` + TAB -> all task subcommands including `global-pause`, `global-resume`, `remove`)
- `TAB`: admin command completion is supported (`admin status`, `admin auth set-password|migrate|reset-password|verify`, `admin --runtime`)
- `TAB`: webhook command completion is supported (`webhook start|stop|kill-port|status|route|token|sign|send` and common flags, including route flags)
- `TAB`: booking command completion is supported (`booking product|slot|reservation|query|service` and common flags)
- `Ctrl+C`: clears current input; first time shows hint to use `exit` to stop daemon

Daemon monitor control:

- `monitor list`: show currently running background monitor jobs (such as `mail monitor`)
- `monitor list` also shows persisted stopped/paused monitor configs for later restart
- `monitor start <id|all>`: start one paused monitor config by id, or start all paused configs
- `monitor stop <id>`: stop one monitor job by `MONITOR_ID` and mark it `paused` in persisted config
- `monitor stop all`: stop all running monitor jobs and mark them `paused` in persisted config
- `monitor remove <id> <confirm-id>`: remove one monitor config permanently (requires id confirmation)
- Alias forms are supported: `mail monitor list`, `mail monitor start <id|all>`, `mail monitor stop <id|all>`, `mail monitor remove <id> <confirm-id>`
- Same mailbox/filter monitor runs as single instance in daemon (duplicate start is rejected)
- Running monitor definitions are persisted to `conf/daemon_monitors.json`; only non-paused monitors auto-restore on next daemon start
- Pipelines that start with `mail monitor` run as background monitor jobs (non-blocking daemon input), and are included in monitor list/start/stop/persistence
- Legacy input `webhook serve ...` in daemon is auto rewritten to `webhook start ...` to prevent blocking the prompt
- `booking service serve ...` in daemon is auto rewritten to `booking service start ...`

Daemon command templates:

- Input only `quote` then press Enter:
  - Auto generates and pre-fills `quote AAPL.US TSLA.US 700.HK`
- Input only `email` or `email send` then press Enter:
  - Auto generates and pre-fills `email send --to recipient@example.com --subject "Notification" --body "message"`

Daemon pipeline:

- You can chain commands with `|`.
- The previous command result will be passed to next stage.
- For `email send`, if no `--body` / `--body-file` is provided, the previous stage result is auto injected as email body.
- For `sys`, pipeline context is injected as env vars and JSON stdin:
  - `LONGTRADE_PIPELINE_SOURCE_COMMAND`
  - `LONGTRADE_PIPELINE_RESULT_JSON`
  - `LONGTRADE_PIPELINE_INPUT_JSON`
- Pipelines starting with `mail monitor` are event-triggered monitor jobs. The daemon keeps monitor connection alive (IDLE/hybrid per your `--mode`) and runs downstream stages when each new-mail event arrives.

Example:

```text
longtradego> quote AAPL.US TSLA.US | email send --to alice@example.com --subject "Quote Alert"
longtradego> mail monitor --mode hybrid --with-body | email send --to alice@example.com --subject "Mail Alert"
longtradego> mail monitor --mode hybrid --with-body | sys --shell 'echo "$LONGTRADE_PIPELINE_INPUT_JSON" | jq .'
```

Daemon scheduled tasks:

Tasks can be added, listed, paused, resumed, and removed. Each task runs periodically according to its configured interval. The daemon maintains a global pause switch that can pause all tasks at once without affecting individual task settings.

- `task` is also registered as a root command for command discovery/help output, while actual task operations run inside daemon interactive mode.

- Add periodic task:

```text
longtradego> task add --every 1m -- quote AAPL.US | email send --to alice@example.com --subject "Scheduled Alert"
longtradego> task add --cron "*/5 * * * *" -- quote AAPL.US | email send --to alice@example.com --subject "Cron Alert"
longtradego> task add --cron "*/5 * * * *" --auto-resume -- quote AAPL.US | email send --to alice@example.com --subject "Cron Alert"
```

`task add` schedule options:

- `--every <duration>`: interval schedule, e.g. `30s`, `5m`, `1h`
- `--cron "<expr>"`: cron schedule (robfig/cron v3 parser), e.g. `"*/5 * * * *"`, `"0 9 * * 1-5"`, `"@hourly"`
- `--auto-resume`: automatically run `task resume <task-id>` right after create
- Use exactly one of `--every` or `--cron`

Tasks are persisted automatically. When daemon restarts, previously saved tasks are restored from:

- `conf/daemon_tasks.json`

Newly added tasks are `paused` by default. Use `--auto-resume` to skip the manual step, or resume explicitly:

```text
longtradego> task resume task-1
```

- List tasks:

```text
longtradego> task list
```

The `STATUS` column shows the current task state, such as `scheduled`, `running`, or `paused`.

`task list` can be used as a pipeline stage, for example to email the current task list:

```text
longtradego> task list | email send --to alice@example.com --subject "Task List Snapshot"
```

- Pause/resume task by id without deleting it:

```text
longtradego> task pause task-1
longtradego> task resume task-1
```

- Pause/resume all tasks globally (master switch):

```text
longtradego> task global-pause
longtradego> task global-resume
```

When global pause is enabled, all tasks are paused regardless of their individual pause states. Global pause state is persisted, so it survives daemon restarts.

- Remove task by id (confirmation required):

```text
longtradego> task remove task-1 task-1
```

Each successful remove also appends a backup record (including deletion timestamp) to:

- `data/daemon_tasks_history.json`

Default behavior:

```bash
go run .
```

Equivalent to:

```bash
go run . quote AAPL.US
```

## Logging

Command execution logs are written to:

- `logs/command.log`
- `logs/system_command.log`
- `logs/mail_monitor.log`

Format:

- JSON Lines (one JSON object per line)
- Includes `run_id`, `status`, `duration_ms`, `request`, `result`, `error`
- Includes copyable `request.command_line` for quick replay/debug

Rotation:

- All logs above rotate at `5MB`.
- If next write would exceed `5MB`, current file is rotated to:
  - `logs/command_YYYYMMDD_HHMMSS.log`
  - `logs/system_command_YYYYMMDD_HHMMSS.log`
  - `logs/mail_monitor_YYYYMMDD_HHMMSS.log`
- Then a new active log file is created for continued writes.

## Release Packaging

GitHub Actions workflow: `.github/workflows/release.yml`

- Trigger: push tag `v*` (for example `v1.2.3`)
- Build targets (v1): `darwin/amd64`, `darwin/arm64`
- Release assets:
  - `longtradego_<version>_darwin_amd64.tar.gz`
  - `longtradego_<version>_darwin_arm64.tar.gz`
  - `checksums.txt` (SHA256)
- Build metadata injected via ldflags:
  - `main.buildVersion`
  - `main.buildCommit`
  - `main.buildDate`

Install script (user-writable bin directory by default):

```bash
# latest
./scripts/install.sh

# specific version
./scripts/install.sh v1.2.3
```

The installer defaults to `~/.local/bin` and does not require sudo.

## Add a New Command

Recommended pattern:

1. Create a new file like `kline_command.go`
2. Implement a `cobra.Command` constructor (similar to `newQuoteCommand`)
3. Register it in `newRootCommand` in `root_command.go`
4. Set execution metadata in command run function:
   - `app.SetExecution("<command>", symbolsOrNil)`
   - `app.SetResult(resultObject)`

This keeps command logic isolated and makes future maintenance easier.
