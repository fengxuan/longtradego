# Public API JS Integration Guide (Node.js 18+)

This guide is for third-party backend integrators who call Longtradego public APIs from JavaScript.

Scope:

- Booking public APIs (`/booking/*`)
- Webhook public APIs (`/webhook/*`)
- Excludes admin APIs (`/admin/*`)

## 1. Prerequisites

You need:

- `third_party_id` (for example `partner-a`)
- Raw security token for that `third_party_id`
- Public base URL (for example `https://booking-api.example.com`)
- System clock in sync (NTP). Timestamp skew beyond +-5 minutes will fail auth.

Runtime recommendation:

- Node.js 18+ (native `fetch`)

## 2. Canonical Contract Sources

Use OpenAPI as the source of truth:

- Portal: `/openapi/v1`
- YAML: `/openapi/v1/spec.yaml`
- JSON: `/openapi/v1/spec.json`
- API Reference UI: `/openapi/v1/reference`
- This guide (HTML): `/openapi/v1/guide/js`
- This guide (Markdown): `/openapi/v1/guide/js.md`

Example:

- `https://booking-api.example.com/openapi/v1`
- `https://booking-api.example.com/openapi/v1/reference`

## 3. Unified Signing Protocol

Required headers for signed requests:

- `X-Third-Party-ID`
- `X-Webhook-Timestamp` (Unix seconds, UTC)
- `X-Webhook-Token`

Required header for public `POST`:

- `Idempotency-Key`

Signature algorithm:

- `sha256(third_party_id + "\n" + timestamp + "\n" + token + "\n" + raw_body_bytes)`
- Output is lowercase hex.

Raw body rule:

- Sign the exact bytes you send.
- Do not sign one JSON serialization and send a different one.

## 4. Reusable JS Helpers

```js
import crypto from "node:crypto";

export function buildSignedHeaders({
  thirdPartyId,
  token,
  bodyJsonString,
  idempotencyKey,
  timestamp = Math.floor(Date.now() / 1000),
}) {
  const ts = String(timestamp);
  const payload = `${thirdPartyId}\n${ts}\n${token}\n${bodyJsonString}`;
  const signature = crypto.createHash("sha256").update(payload, "utf8").digest("hex");
  const headers = {
    "X-Third-Party-ID": thirdPartyId,
    "X-Webhook-Timestamp": ts,
    "X-Webhook-Token": signature,
    "Content-Type": "application/json",
  };
  if (idempotencyKey) {
    headers["Idempotency-Key"] = idempotencyKey;
  }
  return headers;
}

export async function sendSignedRequest({
  url,
  method = "POST",
  thirdPartyId,
  token,
  data,
  idempotencyKey,
  timestamp,
  timeoutMs = 10000,
}) {
  const methodUpper = String(method || "POST").toUpperCase();
  const ts = Number.isFinite(timestamp)
    ? Number(timestamp)
    : Math.floor(Date.now() / 1000);
  const bodyJsonString =
    methodUpper === "POST"
      ? JSON.stringify(data ?? {}, null, 0)
      : "";

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const headers = buildSignedHeaders({
      thirdPartyId,
      token,
      bodyJsonString,
      idempotencyKey: methodUpper === "POST" ? idempotencyKey : undefined,
      timestamp: ts,
    });
    if (methodUpper !== "POST") {
      delete headers["Content-Type"];
    }
    const response = await fetch(url, {
      method: methodUpper,
      headers,
      body: methodUpper === "POST" ? bodyJsonString : undefined,
      signal: controller.signal,
    });
    const text = await response.text();
    let envelope = null;
    try {
      envelope = text ? JSON.parse(text) : null;
    } catch {
      envelope = null;
    }
    return { response, envelope, rawText: text };
  } finally {
    clearTimeout(timer);
  }
}

export function parseEnvelopeOrThrow(response, envelope) {
  const requestId =
    response.headers.get("X-Request-ID") || envelope?.request_id || "";
  const code = envelope?.code || "";
  const message = envelope?.message || response.statusText || "request failed";
  if (!response.ok || envelope?.status === "error") {
    const err = new Error(message);
    err.httpStatus = response.status;
    err.code = code;
    err.requestId = requestId;
    err.details = envelope?.details ?? null;
    throw err;
  }
  return {
    code: code || "ok",
    requestId,
    data: envelope?.data ?? null,
    ts: envelope?.ts || "",
  };
}
```

## 5. Booking Examples

### 5.1 Parse Intent

```js
import { sendSignedRequest, parseEnvelopeOrThrow } from "./api-signing.js";

const baseUrl = "https://booking-api.example.com";
const thirdPartyId = "partner-a";
const token = process.env.LONGTRADEGO_TOKEN;

const parseIdem = `booking-parse-${Date.now()}`;
const { response: parseResp, envelope: parseEnvelope } = await sendSignedRequest({
  url: `${baseUrl}/booking/intents/parse`,
  method: "POST",
  thirdPartyId,
  token,
  idempotencyKey: parseIdem,
  data: {
    user_id: "u-1",
    channel: "chat",
    content: "Book product p-1 for two people tomorrow morning",
  },
});
const parseData = parseEnvelopeOrThrow(parseResp, parseEnvelope).data;
const draftId = parseData?.draft_id;
```

### 5.2 Confirm Intent

```js
const confirmIdem = `booking-confirm-${Date.now()}`;
const { response: confirmResp, envelope: confirmEnvelope } = await sendSignedRequest({
  url: `${baseUrl}/booking/intents/confirm`,
  method: "POST",
  thirdPartyId,
  token,
  idempotencyKey: confirmIdem,
  data: {
    draft_id: draftId,
    slot_id: "slot-1",
    personnel: {
      contact_name: "Alice",
      contact_phone: "13800138000",
    },
  },
});
const confirmData = parseEnvelopeOrThrow(confirmResp, confirmEnvelope).data;
console.log("reservation_id:", confirmData?.reservation_id);
console.log("status:", confirmData?.reservation?.status);
```

### 5.3 Query Catalog and Reservations

`GET` endpoints are also signed with the same 3 auth headers.

```js
// Catalog example:
// GET /booking/catalog?product_id=p-1&include_full=true
//
// Reservations example:
// GET /booking/reservations?user_id=u-1&status=pending
```

Idempotency strategy for parse+confirm:

- Use different keys for each logical write call.
- A common pattern is `<business-id>-parse` and `<business-id>-confirm`.
- Reuse the same key only when retrying the exact same request payload.

## 6. Webhook Example

Example `POST /webhook/{routePath}`:

```js
const webhookIdem = `webhook-send-${Date.now()}`;
const { response: webhookResp, envelope: webhookEnvelope } = await sendSignedRequest({
  url: "https://booking-api.example.com/webhook/events",
  method: "POST",
  thirdPartyId: "partner-a",
  token: process.env.LONGTRADEGO_TOKEN,
  idempotencyKey: webhookIdem,
  data: { order_id: "o-1001", event: "created" },
});

const webhookResult = parseEnvelopeOrThrow(webhookResp, webhookEnvelope);
console.log("http:", webhookResp.status);
console.log("event_id:", webhookResult.data?.meta?.event_id);
```

Response expectations:

- `200`: accepted and processed synchronously.
- `202`: accepted and queued asynchronously.

## 7. Machine Error Code Playbook

| Code | What it usually means | Immediate action |
| --- | --- | --- |
| `missing_required_headers` | Missing one or more required auth headers | Send all 3 required signing headers |
| `invalid_timestamp_header` | Timestamp is not valid Unix seconds | Send integer seconds (`Math.floor(Date.now()/1000)`) |
| `timestamp_outside_allowed_window` | Request delayed or client clock drifted | Sync clock and regenerate signature |
| `token_not_found` | Unknown `third_party_id` | Verify ID and token mapping in server security keys |
| `token_scope_not_allowed` | Token scope does not allow target API | Use token with correct scope (`booking` and/or `webhook`) |
| `signature_verification_failed` | Signature and payload do not match | Recompute signature from exact outgoing bytes |
| `idempotency_key_required` | Missing `Idempotency-Key` on POST | Add unique idempotency key |
| `idempotency_key_conflict` | Same key used with different payload | Use a new key when payload changes |
| `validation_error` | Request payload/query invalid | Fix required fields and data formats |
| `conflict` | Business conflict (for example capacity) | Retry only after business state changes |
| `internal_error` | Server-side processing issue | Log `request_id` and contact provider |
| `idempotency_store_error` | Server idempotency storage issue | Retry with same key; include `request_id` in support ticket |

## 8. Debug Checklist

1. Reproduce with `webhook sign` parity:
   - Same `third_party_id`
   - Same raw token
   - Same timestamp
   - Same raw body bytes
2. Confirm timestamp freshness (within +-5 minutes).
3. Confirm signed body exactly matches sent bytes.
4. Confirm `Idempotency-Key` exists on every public `POST`.
5. Capture and keep:
   - HTTP status
   - `code`
   - `message`
   - `request_id`

CLI parity example:

```bash
cat > /tmp/body.json <<'EOF'
{"hello":"world"}
EOF

TS=$(date +%s)

go run . webhook sign \
  --third-party-id partner-a \
  --token your_raw_token \
  --timestamp "$TS" \
  --data-file /tmp/body.json
```

## 9. Production Integration Checklist

- Keep token only on backend. Do not expose raw token in browser clients.
- Log `request_id` on every non-2xx response.
- Retry policy:
  - Safe: `GET` and `POST` with same `Idempotency-Key` and unchanged payload
  - Do not reuse key for changed payload
- Monitor error-code distribution (`token_not_found`, `signature_verification_failed`, `timestamp_outside_allowed_window`) to catch integration drift early.
- Plan token rotation:
  - Update token in your secret manager
  - Roll out with canary calls
  - Monitor 401 rates and machine error codes

## 10. Smoke Checks

Run these checks before production rollout:

1. Booking happy path:
   - `POST /booking/intents/parse` returns `200` with `code=ok`
   - `POST /booking/intents/confirm` returns `201` with `reservation_id`
2. Webhook happy path:
   - `POST /webhook/{routePath}` returns `200` or `202` with event metadata
3. Negative timestamp case:
   - Reuse request with an old timestamp and verify `401` + `timestamp_outside_allowed_window`
4. Negative signature case:
   - Send modified payload with original signature and verify `401` + `signature_verification_failed`
