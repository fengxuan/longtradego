package service

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestComputeWebhookSignatureStable(t *testing.T) {
	signature := computeWebhookSignature(
		"partner-a",
		"1710000000",
		"secret-token",
		[]byte(`{"price":123,"symbol":"AAPL.US"}`),
	)
	const expected = "4acfbb2604224095c447243653c090e33ff8cba07570040f1f9d7ee31827206d"
	if signature != expected {
		t.Fatalf("unexpected signature: got %s want %s", signature, expected)
	}
}

func TestWebhookSignPackageCanPassWebhookValidation(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	eventLog := filepath.Join(t.TempDir(), "webhook_events.json")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "secret-token",
			CreatedAt:    now.Format(time.RFC3339Nano),
			UpdatedAt:    now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"order_id":"o-1","amount":123}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", body, "/webhook/events", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		TimestampSkew:  5 * time.Minute,
		MaxBodyBytes:   1024 * 1024,
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if !event.Meta.Validation.TokenValid || !event.Meta.Validation.TimestampValid || !event.Meta.Validation.JSONValid {
		t.Fatalf("expected all validations true, got %+v", event.Meta.Validation)
	}
	data, ok := event.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %T", event.Data)
	}
	if data["order_id"] != "o-1" {
		t.Fatalf("unexpected data payload: %+v", data)
	}

	rawLog, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatalf("read event log failed: %v", err)
	}
	logEvent := decodeWebhookResponse(t, rawLog)
	if !logEvent.Meta.Validation.TokenValid || !logEvent.Meta.Validation.JSONValid {
		t.Fatalf("expected log event validation true, got %+v", logEvent.Meta.Validation)
	}
}

func TestWebhookPublicIdempotencyEnvelopeAndReplay(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tempDir := t.TempDir()
	tokenStore := filepath.Join(tempDir, "webhook_tokens.json")
	eventLog := filepath.Join(tempDir, "webhook_events.json")
	idempotencyPath := filepath.Join(tempDir, "webhook_idempotency.json")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "secret-token",
			Scopes:       []string{securityScopeWebhook},
			CreatedAt:    now.Format(time.RFC3339Nano),
			UpdatedAt:    now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}
	store, err := newPublicIdempotencyStore(idempotencyPath, time.Hour)
	if err != nil {
		t.Fatalf("newPublicIdempotencyStore failed: %v", err)
	}

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath:   tokenStore,
		EventLogPath:     eventLog,
		TimestampSkew:    5 * time.Minute,
		MaxBodyBytes:     1024 * 1024,
		IdempotencyStore: store,
		Now: func() time.Time {
			return now
		},
	})

	send := func(body string, key string) (int, string, map[string]any) {
		t.Helper()
		timestamp := strconv.FormatInt(now.Unix(), 10)
		signature := computeWebhookSignature("partner-a", timestamp, "secret-token", []byte(body))
		req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhookHeaderThirdPartyID, "partner-a")
		req.Header.Set(webhookHeaderTimestamp, timestamp)
		req.Header.Set(webhookHeaderSignature, signature)
		if strings.TrimSpace(key) != "" {
			req.Header.Set(publicAPIHeaderIdempotencyKey, key)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		raw := rr.Body.String()
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("decode response failed: %v body=%s", err, raw)
		}
		return rr.Code, raw, payload
	}

	firstStatus, firstBody, firstPayload := send(`{"order_id":"o-1","amount":123}`, "idem-webhook-1")
	if firstStatus != http.StatusOK {
		t.Fatalf("expected first request 200, got %d body=%s", firstStatus, firstBody)
	}
	if firstPayload["status"] != publicAPIStatusOK || firstPayload["code"] != publicAPIErrorCodeOK {
		t.Fatalf("expected success envelope, got %+v", firstPayload)
	}
	if strings.TrimSpace(fmt.Sprintf("%v", firstPayload["request_id"])) == "" {
		t.Fatalf("expected request_id present, got %+v", firstPayload)
	}

	secondStatus, secondBody, _ := send(`{"order_id":"o-1","amount":123}`, "idem-webhook-1")
	if secondStatus != http.StatusOK {
		t.Fatalf("expected replay status 200, got %d body=%s", secondStatus, secondBody)
	}
	if firstBody != secondBody {
		t.Fatalf("expected replay body unchanged\nfirst=%s\nsecond=%s", firstBody, secondBody)
	}

	conflictStatus, conflictBody, conflictPayload := send(`{"order_id":"o-2","amount":123}`, "idem-webhook-1")
	if conflictStatus != http.StatusConflict {
		t.Fatalf("expected idempotency conflict 409, got %d body=%s", conflictStatus, conflictBody)
	}
	if conflictPayload["code"] != publicAPIErrorCodeIdempotencyKeyConflict {
		t.Fatalf("expected conflict code %q, got %+v", publicAPIErrorCodeIdempotencyKeyConflict, conflictPayload)
	}

	missingStatus, missingBody, missingPayload := send(`{"order_id":"o-3","amount":123}`, "")
	if missingStatus != http.StatusBadRequest {
		t.Fatalf("expected missing idempotency key 400, got %d body=%s", missingStatus, missingBody)
	}
	if missingPayload["code"] != publicAPIErrorCodeIdempotencyKeyRequired {
		t.Fatalf("expected key required code %q, got %+v", publicAPIErrorCodeIdempotencyKeyRequired, missingPayload)
	}
}

func TestWebhookSendBuildPackageUsesSameSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	signResult := buildWebhookSignResult(
		"partner-a",
		"secret-token",
		"1710000000",
		body,
		"/webhook/events",
		"http://127.0.0.1:8080/webhook/events",
	)
	sendSignResult, err := buildWebhookSendSignResult(
		"partner-a",
		"secret-token",
		"1710000000",
		body,
		"http://127.0.0.1:8080/webhook/events",
	)
	if err != nil {
		t.Fatalf("buildWebhookSendSignResult failed: %v", err)
	}
	if signResult.Signature != sendSignResult.Signature {
		t.Fatalf("signature mismatch: sign=%s send=%s", signResult.Signature, sendSignResult.Signature)
	}
	if signResult.Headers[webhookHeaderSignature] != sendSignResult.Headers[webhookHeaderSignature] {
		t.Fatalf("header signature mismatch: sign=%s send=%s", signResult.Headers[webhookHeaderSignature], sendSignResult.Headers[webhookHeaderSignature])
	}
}

func TestWebhookSendCommandSuccess(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	eventLog := filepath.Join(t.TempDir(), "webhook_events.json")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "secret-token",
			CreatedAt:    now.Format(time.RFC3339Nano),
			UpdatedAt:    now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	handler := newWebhookEventHandler(webhookServeOptions{
		Path:           "/webhook/events",
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		TimestampSkew:  5 * time.Minute,
		MaxBodyBytes:   1024 * 1024,
		Now: func() time.Time {
			return now
		},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webhook/events" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	app := NewAppContext()
	cmd := newWebhookSendCommand(app)
	cmd.SetArgs([]string{
		"--url", server.URL + "/webhook/events",
		"--third-party-id", "partner-a",
		"--token", "secret-token",
		"--data", `{"hello":"world"}`,
		"--timestamp", "1710000000",
		"--timeout", "2s",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("webhook send command failed: %v", err)
	}

	command, symbols, resultAny := app.ExecutionSnapshot()
	if command != "webhook" {
		t.Fatalf("expected webhook command, got %q", command)
	}
	if len(symbols) != 2 || symbols[0] != "send" {
		t.Fatalf("unexpected execution symbols: %v", symbols)
	}
	result, ok := resultAny.(webhookSendResult)
	if !ok {
		t.Fatalf("expected webhookSendResult, got %T", resultAny)
	}
	if result.HTTPStatus != http.StatusOK {
		t.Fatalf("expected http status 200, got %d body=%s", result.HTTPStatus, result.ResponseBody)
	}
	if result.ResponseJSON == nil {
		t.Fatalf("expected response_json populated for webhook json response")
	}
	if strings.TrimSpace(result.ResponseEventID) == "" {
		t.Fatalf("expected response_event_id populated")
	}
	if result.ResponseThirdPartyID != "partner-a" {
		t.Fatalf("expected response_third_party_id=partner-a, got %q", result.ResponseThirdPartyID)
	}
	if result.ResponseTokenValid == nil || !*result.ResponseTokenValid {
		t.Fatalf("expected response_token_valid=true, got %#v", result.ResponseTokenValid)
	}
	if result.ResponseTimestampValid == nil || !*result.ResponseTimestampValid {
		t.Fatalf("expected response_timestamp_valid=true, got %#v", result.ResponseTimestampValid)
	}
	if result.ResponseJSONValid == nil || !*result.ResponseJSONValid {
		t.Fatalf("expected response_json_valid=true, got %#v", result.ResponseJSONValid)
	}
	event := decodeWebhookResponse(t, []byte(result.ResponseBody))
	if !event.Meta.Validation.TokenValid || !event.Meta.Validation.TimestampValid || !event.Meta.Validation.JSONValid {
		t.Fatalf("expected response validations true, got %+v", event.Meta.Validation)
	}
	if result.ResponseEventID != event.Meta.EventID {
		t.Fatalf("expected response_event_id matches body event id, got event=%q body=%q", result.ResponseEventID, event.Meta.EventID)
	}
}

func TestWebhookSendRequestUnreachableURL(t *testing.T) {
	signResult, err := buildWebhookSendSignResult(
		"partner-a",
		"secret-token",
		"1710000000",
		[]byte(`{"hello":"world"}`),
		"http://127.0.0.1:1/webhook/events",
	)
	if err != nil {
		t.Fatalf("buildWebhookSendSignResult failed: %v", err)
	}
	result, err := executeWebhookSendRequest(signResult, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("expected unreachable url error")
	}
	if strings.TrimSpace(result.TargetURL) == "" {
		t.Fatalf("expected result target url")
	}
}

func TestWebhookSendRequestNonJSONResponseKeepsRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok-line-1\nok-line-2")
	}))
	defer server.Close()

	signResult, err := buildWebhookSendSignResult(
		"partner-a",
		"secret-token",
		"1710000000",
		[]byte(`{"hello":"world"}`),
		server.URL,
	)
	if err != nil {
		t.Fatalf("buildWebhookSendSignResult failed: %v", err)
	}

	result, err := executeWebhookSendRequest(signResult, 2*time.Second)
	if err != nil {
		t.Fatalf("executeWebhookSendRequest failed: %v", err)
	}
	if result.HTTPStatus != http.StatusOK {
		t.Fatalf("expected status 200, got %d", result.HTTPStatus)
	}
	if !strings.Contains(result.ResponseBody, "ok-line-2") {
		t.Fatalf("expected raw response body preserved, got %q", result.ResponseBody)
	}
	if result.ResponseJSON != nil {
		t.Fatalf("expected response_json empty for non-json body, got %#v", result.ResponseJSON)
	}
	if strings.TrimSpace(result.ResponseEventID) != "" {
		t.Fatalf("expected response_event_id empty for non-json body, got %q", result.ResponseEventID)
	}
	if result.ResponseTokenValid != nil || result.ResponseTimestampValid != nil || result.ResponseJSONValid != nil {
		t.Fatalf("expected validation index fields nil for non-json body, got token=%#v timestamp=%#v json=%#v", result.ResponseTokenValid, result.ResponseTimestampValid, result.ResponseJSONValid)
	}
}

func TestBuildWebhookSendDisplayResultPrefersResponseJSON(t *testing.T) {
	trueValue := true
	result := webhookSendResult{
		TargetURL:            "http://127.0.0.1:8080/webhook/events",
		RoutePath:            "/webhook/events",
		ResponseBody:         "{\n  \"meta\": {\"event_id\":\"evt-1\"}\n}\n",
		ResponseJSON:         map[string]any{"meta": map[string]any{"event_id": "evt-1"}},
		ResponseEventID:      "evt-1",
		ResponseThirdPartyID: "partner-a",
		ResponseTokenValid:   &trueValue,
	}
	display := buildWebhookSendDisplayResult(result)
	if _, exists := display["response_body"]; exists {
		t.Fatalf("expected response_body omitted from display when response_json exists, got %#v", display["response_body"])
	}
	if _, exists := display["response_json"]; !exists {
		t.Fatalf("expected response_json shown in display")
	}
	if display["response_event_id"] != "evt-1" {
		t.Fatalf("expected response_event_id in display, got %#v", display["response_event_id"])
	}
}

func TestWebhookSendCommandRejectsDataAndDataFileTogether(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(tempFile, []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatalf("write temp data file failed: %v", err)
	}
	app := NewAppContext()
	cmd := newWebhookSendCommand(app)
	cmd.SetArgs([]string{
		"--url", "http://127.0.0.1:8080/webhook/events",
		"--third-party-id", "partner-a",
		"--token", "secret-token",
		"--data", `{"x":1}`,
		"--data-file", tempFile,
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected data/data-file conflict error")
	}
}

func TestWebhookDebugPipelineCommandBuildsSeedFromLatestPostAudit(t *testing.T) {
	tmpDir := t.TempDir()
	auditPath := filepath.Join(tmpDir, "webhook_audit.log")

	getEntry := webhookAuditLogEntry{
		Timestamp:      "2026-04-30T18:00:00+08:00",
		RouteID:        "sayhelloMail",
		Method:         http.MethodGet,
		Path:           "/webhook/hello",
		RequestHeaders: map[string][]string{"Accept": {"*/*"}},
		RequestBody:    "",
		ResponseStatus: http.StatusMethodNotAllowed,
		ResponseBody:   `{"meta":{"error":"method not allowed"},"data":null}`,
		ReceivedAt:     "2026-04-30T18:00:00+08:00",
		RespondedAt:    "2026-04-30T18:00:00+08:00",
		DurationMs:     0,
	}
	if err := appendWebhookAuditLog(auditPath, getEntry); err != nil {
		t.Fatalf("append get audit entry failed: %v", err)
	}

	postEntry1 := webhookAuditLogEntry{
		Timestamp:      "2026-04-30T18:00:01+08:00",
		EventID:        "evt-old",
		RouteID:        "sayhelloMail",
		Method:         http.MethodPost,
		Path:           "/webhook/hello",
		RequestHeaders: map[string][]string{"Content-Type": {"application/json"}},
		RequestBody:    `{"hello":"old"}`,
		ResponseStatus: http.StatusOK,
		ResponseBody:   `{"meta":{"event_id":"evt-old"},"data":{"hello":"old"}}`,
		ReceivedAt:     "2026-04-30T18:00:01+08:00",
		RespondedAt:    "2026-04-30T18:00:01+08:00",
		DurationMs:     1,
		Meta: &webhookEventMeta{
			EventID:      "evt-old",
			ThirdPartyID: "partner-a",
			ReceivedAt:   "2026-04-30T18:00:01+08:00",
			Validation: webhookValidationStatus{
				TokenValid:     true,
				TimestampValid: true,
				JSONValid:      true,
			},
		},
		Dispatch: &webhookDispatchAuditInfo{
			Mode:   webhookRouteModeSync,
			Status: "success",
		},
	}
	if err := appendWebhookAuditLog(auditPath, postEntry1); err != nil {
		t.Fatalf("append old post audit entry failed: %v", err)
	}

	postEntry2 := webhookAuditLogEntry{
		Timestamp:      "2026-04-30T18:00:02+08:00",
		EventID:        "evt-new",
		RouteID:        "sayhelloMail",
		Method:         http.MethodPost,
		Path:           "/webhook/hello",
		RequestHeaders: map[string][]string{"Content-Type": {"application/json"}},
		RequestBody:    `{"hello":"world","x":1}`,
		ResponseStatus: http.StatusOK,
		ResponseBody:   `{"meta":{"event_id":"evt-new"},"data":{"hello":"world","x":1}}`,
		ReceivedAt:     "2026-04-30T18:00:02+08:00",
		RespondedAt:    "2026-04-30T18:00:02+08:00",
		DurationMs:     2,
		Meta: &webhookEventMeta{
			EventID:      "evt-new",
			ThirdPartyID: "partner-a",
			ReceivedAt:   "2026-04-30T18:00:02+08:00",
			Validation: webhookValidationStatus{
				TokenValid:     true,
				TimestampValid: true,
				JSONValid:      true,
			},
		},
		Dispatch: &webhookDispatchAuditInfo{
			Mode:   webhookRouteModeSync,
			Status: "success",
		},
	}
	if err := appendWebhookAuditLog(auditPath, postEntry2); err != nil {
		t.Fatalf("append new post audit entry failed: %v", err)
	}

	app := NewAppContext()
	cmd := newWebhookDebugPipelineCommand(app)
	cmd.SetArgs([]string{"--audit-log", auditPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("webhook debug-pipeline command failed: %v", err)
	}

	command, symbols, resultAny := app.ExecutionSnapshot()
	if command != "webhook" {
		t.Fatalf("expected webhook command, got %q", command)
	}
	if len(symbols) != 2 || symbols[0] != "debug-pipeline" || symbols[1] != "evt-new" {
		t.Fatalf("unexpected execution symbols: %v", symbols)
	}

	result, ok := resultAny.(webhookDebugPipelineResult)
	if !ok {
		t.Fatalf("expected webhookDebugPipelineResult, got %T", resultAny)
	}
	if result.EventID != "evt-new" {
		t.Fatalf("expected latest post event id evt-new, got %s", result.EventID)
	}
	if result.Method != http.MethodPost {
		t.Fatalf("expected method POST, got %s", result.Method)
	}
	if result.ResponseStatus != http.StatusOK {
		t.Fatalf("expected response status 200, got %d", result.ResponseStatus)
	}
	if result.DataParseError != "" {
		t.Fatalf("unexpected data parse error: %s", result.DataParseError)
	}

	rawBody, ok := result.PipelineSeed["raw_body"].(string)
	if !ok || rawBody != `{"hello":"world","x":1}` {
		t.Fatalf("expected raw_body preserved, got %#v", result.PipelineSeed["raw_body"])
	}
	data, ok := result.PipelineSeed["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object in pipeline seed, got %#v", result.PipelineSeed["data"])
	}
	if data["hello"] != "world" {
		t.Fatalf("unexpected data payload in pipeline seed: %#v", data)
	}
}

func TestWebhookDebugPipelineCommandSupportsEventIDAndRouteFilter(t *testing.T) {
	tmpDir := t.TempDir()
	auditPath := filepath.Join(tmpDir, "webhook_audit.log")

	entry := webhookAuditLogEntry{
		Timestamp:      "2026-04-30T18:01:00+08:00",
		EventID:        "evt-debug-1",
		RouteID:        "route-1",
		Method:         http.MethodPost,
		Path:           "/webhook/route-1",
		RequestHeaders: map[string][]string{"Content-Type": {"application/json"}},
		RequestBody:    `{"a":1}`,
		ResponseStatus: http.StatusOK,
		ResponseBody:   `{"meta":{"event_id":"evt-debug-1"},"data":{"a":1}}`,
		ReceivedAt:     "2026-04-30T18:01:00+08:00",
		RespondedAt:    "2026-04-30T18:01:00+08:00",
		DurationMs:     1,
		Meta: &webhookEventMeta{
			EventID:      "evt-debug-1",
			ThirdPartyID: "partner-a",
			ReceivedAt:   "2026-04-30T18:01:00+08:00",
			Validation: webhookValidationStatus{
				TokenValid:     true,
				TimestampValid: true,
				JSONValid:      true,
			},
		},
		Dispatch: &webhookDispatchAuditInfo{
			Mode:   webhookRouteModeSync,
			Status: "success",
		},
	}
	if err := appendWebhookAuditLog(auditPath, entry); err != nil {
		t.Fatalf("append audit entry failed: %v", err)
	}

	app := NewAppContext()
	cmd := newWebhookDebugPipelineCommand(app)
	cmd.SetArgs([]string{
		"--audit-log", auditPath,
		"--event-id", "evt-debug-1",
		"--route-id", "route-1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("webhook debug-pipeline command failed with filter: %v", err)
	}

	_, _, resultAny := app.ExecutionSnapshot()
	result, ok := resultAny.(webhookDebugPipelineResult)
	if !ok {
		t.Fatalf("expected webhookDebugPipelineResult, got %T", resultAny)
	}
	if result.EventID != "evt-debug-1" || result.RouteID != "route-1" {
		t.Fatalf("unexpected filtered result: %+v", result)
	}

	appMissing := NewAppContext()
	cmdMissing := newWebhookDebugPipelineCommand(appMissing)
	cmdMissing.SetArgs([]string{
		"--audit-log", auditPath,
		"--event-id", "evt-not-found",
	})
	if err := cmdMissing.Execute(); err == nil {
		t.Fatalf("expected missing event-id to fail")
	}
}

func TestWebhookHandlerRejectsInvalidSignature(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"x":1}`)
	signResult := buildWebhookSignResult("partner-a", "wrong-token", "1710000000", body, "/webhook/events", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath: tokenStore,
		EventLogPath:   filepath.Join(t.TempDir(), "events.json"),
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if event.Meta.Validation.TokenValid {
		t.Fatalf("expected token_valid false")
	}
	if !event.Meta.Validation.TimestampValid {
		t.Fatalf("expected timestamp_valid true")
	}
}

func TestWebhookHandlerRejectsExpiredTimestamp(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"x":1}`)
	oldTimestamp := strconvFormatInt(now.Add(-10 * time.Minute).Unix())
	signResult := buildWebhookSignResult("partner-a", "secret-token", oldTimestamp, body, "/webhook/events", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath: tokenStore,
		EventLogPath:   filepath.Join(t.TempDir(), "events.json"),
		TimestampSkew:  5 * time.Minute,
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if event.Meta.Validation.TimestampValid {
		t.Fatalf("expected timestamp_valid false")
	}
}

func TestWebhookHandlerRejectsModifiedBody(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	signBody := []byte(`{"amount":100}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/events", "")
	modifiedBody := `{"amount":200}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(modifiedBody))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath: tokenStore,
		EventLogPath:   filepath.Join(t.TempDir(), "events.json"),
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if event.Meta.Validation.TokenValid {
		t.Fatalf("expected token_valid false for modified body")
	}
}

func TestWebhookHandlerRejectsInvalidJSONAfterSignaturePass(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"invalid_json":`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", body, "/webhook/events", "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath: tokenStore,
		EventLogPath:   filepath.Join(t.TempDir(), "events.json"),
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if !event.Meta.Validation.TokenValid {
		t.Fatalf("expected token_valid true")
	}
	if event.Meta.Validation.JSONValid {
		t.Fatalf("expected json_valid false")
	}
}

func TestWebhookTokenGenerateQueryResetLifecycle(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "webhook_tokens.json")
	now := time.Unix(1710000000, 0).UTC()

	record1, err := createWebhookToken(storePath, "partner-a", now)
	if err != nil {
		t.Fatalf("createWebhookToken failed: %v", err)
	}
	if record1.Token == "" {
		t.Fatalf("expected generated token")
	}

	if _, err := createWebhookToken(storePath, "partner-a", now); err == nil {
		t.Fatalf("expected duplicate generate to fail")
	}

	found, ok, err := findWebhookToken(storePath, "partner-a")
	if err != nil {
		t.Fatalf("findWebhookToken failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected token exists")
	}
	if found.Token != record1.Token {
		t.Fatalf("unexpected token lookup: %s vs %s", found.Token, record1.Token)
	}

	masked := maskWebhookToken(found.Token)
	if masked == "" || masked == found.Token || !strings.Contains(masked, "...") {
		t.Fatalf("unexpected masked token: %q", masked)
	}

	record2, err := resetWebhookToken(storePath, "partner-a", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("resetWebhookToken failed: %v", err)
	}
	if record2.Token == record1.Token {
		t.Fatalf("expected token changed after reset")
	}

	if _, err := resetWebhookToken(storePath, "missing", now); err == nil {
		t.Fatalf("expected reset missing token to fail")
	}
}

func TestWebhookTokenScopeGenerateAndReset(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "webhook_tokens.json")
	now := time.Unix(1710000000, 0).UTC()

	record, err := createWebhookTokenWithScopes(storePath, "partner-scope", []string{securityScopeWebhook}, now)
	if err != nil {
		t.Fatalf("createWebhookTokenWithScopes failed: %v", err)
	}
	if !securityRecordHasScope(record, securityScopeWebhook) || securityRecordHasScope(record, securityScopeBooking) {
		t.Fatalf("expected webhook-only scope, got %+v", record.Scopes)
	}

	resetRecord, err := resetWebhookTokenWithScopes(storePath, "partner-scope", []string{securityScopeBooking}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("resetWebhookTokenWithScopes failed: %v", err)
	}
	if !securityRecordHasScope(resetRecord, securityScopeBooking) || securityRecordHasScope(resetRecord, securityScopeWebhook) {
		t.Fatalf("expected booking-only scope after reset, got %+v", resetRecord.Scopes)
	}
}

func TestWebhookTokenCommandRejectsLegacyTokenStoreFlag(t *testing.T) {
	app := NewAppContext()
	cmd := newWebhookTokenCommand(app)
	cmd.SetArgs([]string{
		"query", "partner-a",
		"--token-store", "/tmp/security-b.json",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected legacy --token-store flag rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unknown flag") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestWebhookStartRejectsLegacyTokenStoreFlag(t *testing.T) {
	app := NewAppContext()
	cmd := newWebhookStartCommand(app)
	cmd.SetArgs([]string{
		"--token-store", "/tmp/security-b.json",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected legacy --token-store flag rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unknown flag") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestWebhookHandlerRejectsBookingOnlyScopeToken(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tokenStore := filepath.Join(t.TempDir(), "webhook_tokens.json")
	eventLog := filepath.Join(t.TempDir(), "webhook_events.json")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "secret-token",
			Scopes:       []string{securityScopeBooking},
			CreatedAt:    now.Format(time.RFC3339Nano),
			UpdatedAt:    now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"order_id":"o-1"}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", body, "/webhook/events", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		TimestampSkew:  5 * time.Minute,
		MaxBodyBytes:   1024 * 1024,
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if !strings.Contains(strings.ToLower(event.Meta.Error), "scope") {
		t.Fatalf("expected scope error message, got %q", event.Meta.Error)
	}
}

func TestWebhookHandlerRejectsGETMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/webhook/events", nil)
	rr := httptest.NewRecorder()

	handler := newWebhookEventHandler(webhookServeOptions{
		Path: "/webhook/events",
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if event.Meta.Error != "method not allowed" {
		t.Fatalf("expected method not allowed error, got %q", event.Meta.Error)
	}
	if event.Meta.Validation.TokenValid || event.Meta.Validation.TimestampValid || event.Meta.Validation.JSONValid {
		t.Fatalf("expected all validation flags false for GET, got %+v", event.Meta.Validation)
	}
}

func TestWebhookHandlerRejectsGETMethodForRouteWithPipeline(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/webhook/hello", nil)
	rr := httptest.NewRecorder()

	resolved := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       "sayhelloMail",
			Path:     "/webhook/hello",
			Mode:     webhookRouteModeSync,
			Pipeline: "mail send --to xjfeng@sohu.com --subject cool",
			Enabled:  true,
		},
		Mode: webhookRouteModeSync,
	}
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:          "/webhook/hello",
		ResolvedRoute: &resolved,
	})
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "mail send --to") {
		t.Fatalf("expected response not to expose pipeline content, got body=%s", body)
	}
}

func TestStartWebhookInBackgroundWritesRuntimeAndStatusRunning(t *testing.T) {
	oldSpawn := webhookSpawnBackgroundProcess
	oldVerify := webhookVerifyBackgroundStart
	webhookSpawnBackgroundProcess = func(cfg *webhookServeConfig, _ string) (int, []string, error) {
		return os.Getpid(), []string{"webhook", "serve"}, nil
	}
	webhookVerifyBackgroundStart = func(cfg *webhookServeConfig, pid int) error {
		return nil
	}
	defer func() {
		webhookSpawnBackgroundProcess = oldSpawn
		webhookVerifyBackgroundStart = oldVerify
	}()

	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	logPath := filepath.Join(tmpDir, "webhook_server.log")
	cfg := newWebhookServeConfig()
	addrListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free addr failed: %v", err)
	}
	cfg.PublicAddr = addrListener.Addr().String()
	cfg.Addr = cfg.PublicAddr
	_ = addrListener.Close()
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free admin addr failed: %v", err)
	}
	cfg.AdminAddr = adminListener.Addr().String()
	_ = adminListener.Close()
	cfg.TokenStore = filepath.Join(tmpDir, "webhook_tokens.json")
	cfg.EventLogPath = filepath.Join(tmpDir, "webhook_events.json")
	if err := cfg.validate(); err != nil {
		t.Fatalf("cfg validate failed: %v", err)
	}

	claim := &webhookStartOwnerClaim{
		SessionID:  "daemon-session-1",
		DaemonPID:  os.Getpid(),
		ClaimedAt:  time.Now(),
		StartToken: "owner-token-1",
	}
	result, err := startWebhookInBackground(runtimePath, logPath, cfg, claim)
	if err != nil {
		t.Fatalf("startWebhookInBackground failed: %v", err)
	}
	if result.Status != "started" || result.Runtime == nil {
		t.Fatalf("unexpected start result: %+v", result)
	}
	if result.Runtime.PID != os.Getpid() {
		t.Fatalf("expected runtime pid=%d, got %d", os.Getpid(), result.Runtime.PID)
	}
	if result.Runtime.OwnerSessionID != claim.SessionID || result.Runtime.OwnerStartToken != claim.StartToken {
		t.Fatalf("expected owner fields persisted, runtime=%+v", result.Runtime)
	}
	if result.Runtime.OwnerDaemonPID != os.Getpid() {
		t.Fatalf("expected owner daemon pid=%d, got %d", os.Getpid(), result.Runtime.OwnerDaemonPID)
	}
	if strings.TrimSpace(result.Runtime.OwnerClaimedAt) == "" {
		t.Fatalf("expected owner claimed at to be populated")
	}

	status, err := webhookStatus(runtimePath)
	if err != nil {
		t.Fatalf("webhookStatus failed: %v", err)
	}
	if !status.Running || status.Status != "running" {
		t.Fatalf("expected running status, got %+v", status)
	}
}

func TestStartWebhookInBackgroundReturnsAlreadyRunningWhenAddrOccupied(t *testing.T) {
	oldSpawn := webhookSpawnBackgroundProcess
	oldVerify := webhookVerifyBackgroundStart
	spawnCalled := false
	webhookSpawnBackgroundProcess = func(cfg *webhookServeConfig, _ string) (int, []string, error) {
		spawnCalled = true
		return os.Getpid(), []string{"webhook", "serve"}, nil
	}
	webhookVerifyBackgroundStart = func(cfg *webhookServeConfig, pid int) error {
		return nil
	}
	defer func() {
		webhookSpawnBackgroundProcess = oldSpawn
		webhookVerifyBackgroundStart = oldVerify
	}()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen occupy addr failed: %v", err)
	}
	defer occupied.Close()

	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	logPath := filepath.Join(tmpDir, "webhook_server.log")
	cfg := newWebhookServeConfig()
	cfg.PublicAddr = occupied.Addr().String()
	cfg.Addr = cfg.PublicAddr
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free admin addr failed: %v", err)
	}
	cfg.AdminAddr = adminListener.Addr().String()
	_ = adminListener.Close()
	if err := cfg.validate(); err != nil {
		t.Fatalf("cfg validate failed: %v", err)
	}

	result, err := startWebhookInBackground(runtimePath, logPath, cfg, nil)
	if err != nil {
		t.Fatalf("expected no error for occupied addr, got: %v", err)
	}
	if result.Status != "already_running" {
		t.Fatalf("expected already_running status, got %+v", result)
	}
	if !strings.Contains(result.Message, "already in use") {
		t.Fatalf("expected already in use message, got %q", result.Message)
	}
	if spawnCalled {
		t.Fatalf("expected spawn to be skipped when addr is occupied")
	}
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if exists {
		t.Fatalf("expected runtime state not written when already_running by addr check")
	}
}

func TestStartWebhookInBackgroundFailsWhenStartupVerificationFails(t *testing.T) {
	oldSpawn := webhookSpawnBackgroundProcess
	oldVerify := webhookVerifyBackgroundStart

	sleepProc := exec.Command("sleep", "30")
	if err := sleepProc.Start(); err != nil {
		t.Fatalf("start sleep process failed: %v", err)
	}
	t.Cleanup(func() {
		_ = sleepProc.Process.Kill()
	})

	webhookSpawnBackgroundProcess = func(cfg *webhookServeConfig, _ string) (int, []string, error) {
		return sleepProc.Process.Pid, []string{"webhook", "serve"}, nil
	}
	webhookVerifyBackgroundStart = func(cfg *webhookServeConfig, pid int) error {
		return fmt.Errorf("probe failed")
	}
	defer func() {
		webhookSpawnBackgroundProcess = oldSpawn
		webhookVerifyBackgroundStart = oldVerify
	}()

	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	logPath := filepath.Join(tmpDir, "webhook_server.log")
	cfg := newWebhookServeConfig()
	addrListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free addr failed: %v", err)
	}
	cfg.PublicAddr = addrListener.Addr().String()
	cfg.Addr = cfg.PublicAddr
	_ = addrListener.Close()
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free admin addr failed: %v", err)
	}
	cfg.AdminAddr = adminListener.Addr().String()
	_ = adminListener.Close()
	if err := cfg.validate(); err != nil {
		t.Fatalf("cfg validate failed: %v", err)
	}

	_, err = startWebhookInBackground(runtimePath, logPath, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("expected startup verification error, got: %v", err)
	}
	if _, exists, readErr := readWebhookRuntimeState(runtimePath); readErr != nil {
		t.Fatalf("read runtime state failed: %v", readErr)
	} else if exists {
		t.Fatalf("expected runtime state not written on startup verify failure")
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- sleepProc.Wait()
	}()
	select {
	case <-time.After(2 * time.Second):
		t.Fatalf("expected spawned process to be killed after startup verify failure")
	case <-waitCh:
		// Process exits after kill; no additional assertion needed.
	}
}

func TestWebhookServeCommandHiddenAndRejectsDirectUsage(t *testing.T) {
	app := NewAppContext()
	cmd := newWebhookServeCommand(app)
	if !cmd.Hidden {
		t.Fatalf("expected webhook serve command hidden")
	}

	t.Setenv(webhookServeInternalEnv, "")
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected direct webhook serve execution to fail")
	}
	if !strings.Contains(err.Error(), "use `webhook start`") {
		t.Fatalf("expected serve error suggests webhook start, got: %v", err)
	}
}

func TestDefaultWebhookRuntimeStatePathUsesDataDir(t *testing.T) {
	got := filepath.Clean(defaultWebhookRuntimeStatePath())
	want := filepath.Clean(filepath.Join(daemonDataDir, webhookRuntimeStateFile))
	if got != want {
		t.Fatalf("unexpected runtime default path: got=%s want=%s", got, want)
	}
}

func TestReadWebhookRuntimeStateMigratesLegacyRuntimeToDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	withWebhookTestWorkingDir(t, tmpDir)

	now := time.Now().Format(time.RFC3339Nano)
	legacyPath := defaultWebhookLegacyRuntimeStatePath()
	if err := writeWebhookRuntimeState(legacyPath, webhookRuntimeInfo{
		PID:       os.Getpid(),
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write legacy runtime failed: %v", err)
	}

	runtime, exists, err := readWebhookRuntimeState(defaultWebhookRuntimeStatePath())
	if err != nil {
		t.Fatalf("read runtime with migration failed: %v", err)
	}
	if !exists || runtime == nil {
		t.Fatalf("expected runtime exists after migration")
	}
	if runtime.PID != os.Getpid() {
		t.Fatalf("expected migrated runtime pid=%d got=%d", os.Getpid(), runtime.PID)
	}
	if _, err := os.Stat(defaultWebhookRuntimeStatePath()); err != nil {
		t.Fatalf("expected migrated runtime in data path, err=%v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected legacy runtime removed after migration, err=%v", err)
	}
}

func TestReadWebhookRuntimeStateDefaultPathNoLegacyReturnsNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	withWebhookTestWorkingDir(t, tmpDir)

	runtime, exists, err := readWebhookRuntimeState(defaultWebhookRuntimeStatePath())
	if err != nil {
		t.Fatalf("read runtime without files failed: %v", err)
	}
	if exists || runtime != nil {
		t.Fatalf("expected runtime not found, got exists=%t runtime=%+v", exists, runtime)
	}
}

func TestReadWebhookRuntimeStateCustomPathSkipsDefaultMigration(t *testing.T) {
	tmpDir := t.TempDir()
	withWebhookTestWorkingDir(t, tmpDir)

	now := time.Now().Format(time.RFC3339Nano)
	legacyPath := defaultWebhookLegacyRuntimeStatePath()
	if err := writeWebhookRuntimeState(legacyPath, webhookRuntimeInfo{
		PID:       12345,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write legacy runtime failed: %v", err)
	}

	customPath := filepath.Join(tmpDir, "custom_runtime.json")
	runtime, exists, err := readWebhookRuntimeState(customPath)
	if err != nil {
		t.Fatalf("read custom runtime failed: %v", err)
	}
	if exists || runtime != nil {
		t.Fatalf("expected custom runtime not found, got exists=%t runtime=%+v", exists, runtime)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("expected legacy runtime untouched for custom path read, err=%v", err)
	}
	if _, err := os.Stat(defaultWebhookRuntimeStatePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected default data runtime not created for custom path read, err=%v", err)
	}
}

func TestWebhookStatusDetectsStaleRuntime(t *testing.T) {
	runtimePath := filepath.Join(t.TempDir(), "webhook_runtime.json")
	now := time.Now().Format(time.RFC3339Nano)
	err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       999999,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("writeWebhookRuntimeState failed: %v", err)
	}

	status, err := webhookStatus(runtimePath)
	if err != nil {
		t.Fatalf("webhookStatus failed: %v", err)
	}
	if status.Status != "stale" || status.Running {
		t.Fatalf("expected stale status, got %+v", status)
	}
}

func TestWebhookStatusDefaultPathMigratesLegacyRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	withWebhookTestWorkingDir(t, tmpDir)

	now := time.Now().Format(time.RFC3339Nano)
	legacyPath := defaultWebhookLegacyRuntimeStatePath()
	if err := writeWebhookRuntimeState(legacyPath, webhookRuntimeInfo{
		PID:       999999,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write legacy runtime failed: %v", err)
	}

	status, err := webhookStatus(defaultWebhookRuntimeStatePath())
	if err != nil {
		t.Fatalf("webhookStatus on default path failed: %v", err)
	}
	if status.Status != "stale" || status.Running {
		t.Fatalf("expected stale status, got %+v", status)
	}
	if _, err := os.Stat(defaultWebhookRuntimeStatePath()); err != nil {
		t.Fatalf("expected default runtime created after migration, err=%v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected legacy runtime removed after migration, err=%v", err)
	}
}

func TestWebhookStopTerminatesRunningProcessAndRemovesRuntime(t *testing.T) {
	sleepProc := exec.Command("sleep", "30")
	if err := sleepProc.Start(); err != nil {
		t.Fatalf("start sleep process failed: %v", err)
	}
	t.Cleanup(func() {
		_ = sleepProc.Process.Kill()
	})

	runtimePath := filepath.Join(t.TempDir(), "webhook_runtime.json")
	now := time.Now().Format(time.RFC3339Nano)
	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       sleepProc.Process.Pid,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("writeWebhookRuntimeState failed: %v", err)
	}

	result, err := stopWebhook(runtimePath, 5*time.Second)
	if err != nil {
		t.Fatalf("stopWebhook failed: %v", err)
	}
	if result.Status != "stopped" {
		t.Fatalf("expected stopped status, got %+v", result)
	}
	_ = sleepProc.Wait()

	status, err := webhookStatus(runtimePath)
	if err != nil {
		t.Fatalf("webhookStatus failed: %v", err)
	}
	if status.Status != "stopped" || status.Running {
		t.Fatalf("expected stopped status after stop, got %+v", status)
	}
}

func TestWebhookStopDefaultPathMigratesLegacyRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	withWebhookTestWorkingDir(t, tmpDir)

	sleepProc := exec.Command("sleep", "30")
	if err := sleepProc.Start(); err != nil {
		t.Fatalf("start sleep process failed: %v", err)
	}
	t.Cleanup(func() {
		_ = sleepProc.Process.Kill()
	})

	now := time.Now().Format(time.RFC3339Nano)
	legacyPath := defaultWebhookLegacyRuntimeStatePath()
	if err := writeWebhookRuntimeState(legacyPath, webhookRuntimeInfo{
		PID:       sleepProc.Process.Pid,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write legacy runtime failed: %v", err)
	}

	result, err := stopWebhook(defaultWebhookRuntimeStatePath(), 5*time.Second)
	if err != nil {
		t.Fatalf("stopWebhook with default path failed: %v", err)
	}
	if result.Status != "stopped" {
		t.Fatalf("expected stopped status, got %+v", result)
	}
	_ = sleepProc.Wait()

	status, err := webhookStatus(defaultWebhookRuntimeStatePath())
	if err != nil {
		t.Fatalf("webhookStatus after stop failed: %v", err)
	}
	if status.Status != "stopped" || status.Running {
		t.Fatalf("expected stopped status after stop, got %+v", status)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected legacy runtime removed after migration, err=%v", err)
	}
}

func TestParseWebhookListenPort(t *testing.T) {
	cases := []struct {
		addr string
		port int
	}{
		{addr: ":8080", port: 8080},
		{addr: "127.0.0.1:18080", port: 18080},
		{addr: "8081", port: 8081},
	}
	for _, tc := range cases {
		got, err := parseWebhookListenPort(tc.addr)
		if err != nil {
			t.Fatalf("parseWebhookListenPort(%q) failed: %v", tc.addr, err)
		}
		if got != tc.port {
			t.Fatalf("parseWebhookListenPort(%q)=%d want=%d", tc.addr, got, tc.port)
		}
	}

	for _, invalid := range []string{"", "abc", "127.0.0.1", ":99999"} {
		if _, err := parseWebhookListenPort(invalid); err == nil {
			t.Fatalf("expected parseWebhookListenPort(%q) to fail", invalid)
		}
	}
}

func TestExecuteWebhookKillPortNotRunning(t *testing.T) {
	oldList := webhookKillPortListListeningPIDs
	oldTerminate := webhookKillPortTerminateProcess
	webhookKillPortListListeningPIDs = func(port int) ([]int, error) {
		if port != 8080 {
			t.Fatalf("expected port 8080, got %d", port)
		}
		return []int{}, nil
	}
	webhookKillPortTerminateProcess = func(pid int, timeout time.Duration) error {
		t.Fatalf("terminate should not be called when no pid matched")
		return nil
	}
	defer func() {
		webhookKillPortListListeningPIDs = oldList
		webhookKillPortTerminateProcess = oldTerminate
	}()

	result, err := executeWebhookKillPort(":8080", time.Second, "", false)
	if err != nil {
		t.Fatalf("executeWebhookKillPort not_running failed: %v", err)
	}
	if result.Status != "not_running" {
		t.Fatalf("expected status not_running, got %+v", result)
	}
	if len(result.MatchedPIDs) != 0 || len(result.StoppedPIDs) != 0 || len(result.Failed) != 0 {
		t.Fatalf("expected no pid activity, got %+v", result)
	}
}

func TestExecuteWebhookKillPortDryRunDoesNotTerminate(t *testing.T) {
	oldList := webhookKillPortListListeningPIDs
	oldTerminate := webhookKillPortTerminateProcess
	terminateCalled := false
	webhookKillPortListListeningPIDs = func(port int) ([]int, error) {
		return []int{101, 102}, nil
	}
	webhookKillPortTerminateProcess = func(pid int, timeout time.Duration) error {
		terminateCalled = true
		return nil
	}
	defer func() {
		webhookKillPortListListeningPIDs = oldList
		webhookKillPortTerminateProcess = oldTerminate
	}()

	result, err := executeWebhookKillPort(":8080", time.Second, "", true)
	if err != nil {
		t.Fatalf("executeWebhookKillPort dry-run failed: %v", err)
	}
	if result.Status != "dry_run" {
		t.Fatalf("expected status dry_run, got %+v", result)
	}
	if terminateCalled {
		t.Fatalf("terminate should not be called in dry-run mode")
	}
	if len(result.MatchedPIDs) != 2 || result.MatchedPIDs[0] != 101 || result.MatchedPIDs[1] != 102 {
		t.Fatalf("unexpected matched pids in dry-run: %+v", result.MatchedPIDs)
	}
}

func TestExecuteWebhookKillPortStoppedAndRuntimeCleanup(t *testing.T) {
	oldList := webhookKillPortListListeningPIDs
	oldTerminate := webhookKillPortTerminateProcess
	webhookKillPortListListeningPIDs = func(port int) ([]int, error) {
		return []int{321}, nil
	}
	webhookKillPortTerminateProcess = func(pid int, timeout time.Duration) error {
		if pid != 321 {
			t.Fatalf("unexpected pid terminate=%d", pid)
		}
		return nil
	}
	defer func() {
		webhookKillPortListListeningPIDs = oldList
		webhookKillPortTerminateProcess = oldTerminate
	}()

	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)
	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       321,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: nowText,
		UpdatedAt: nowText,
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}

	result, err := executeWebhookKillPort(":8080", time.Second, runtimePath, false)
	if err != nil {
		t.Fatalf("executeWebhookKillPort stopped failed: %v", err)
	}
	if result.Status != "stopped" {
		t.Fatalf("expected status stopped, got %+v", result)
	}
	if len(result.StoppedPIDs) != 1 || result.StoppedPIDs[0] != 321 {
		t.Fatalf("unexpected stopped pids: %+v", result.StoppedPIDs)
	}
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if exists {
		t.Fatalf("expected runtime state removed when pid matched stopped list")
	}
}

func TestExecuteWebhookKillPortPartialFailed(t *testing.T) {
	oldList := webhookKillPortListListeningPIDs
	oldTerminate := webhookKillPortTerminateProcess
	webhookKillPortListListeningPIDs = func(port int) ([]int, error) {
		return []int{401, 402}, nil
	}
	webhookKillPortTerminateProcess = func(pid int, timeout time.Duration) error {
		if pid == 402 {
			return fmt.Errorf("permission denied")
		}
		return nil
	}
	defer func() {
		webhookKillPortListListeningPIDs = oldList
		webhookKillPortTerminateProcess = oldTerminate
	}()

	result, err := executeWebhookKillPort(":8080", time.Second, "", false)
	if err == nil {
		t.Fatalf("expected partial failure error")
	}
	if result.Status != "partial_failed" {
		t.Fatalf("expected status partial_failed, got %+v", result)
	}
	if len(result.StoppedPIDs) != 1 || result.StoppedPIDs[0] != 401 {
		t.Fatalf("unexpected stopped pids: %+v", result.StoppedPIDs)
	}
	if len(result.Failed) != 1 || result.Failed[0].PID != 402 {
		t.Fatalf("unexpected failed details: %+v", result.Failed)
	}
}

func TestExecuteWebhookKillPortRuntimeMismatchKeepsState(t *testing.T) {
	oldList := webhookKillPortListListeningPIDs
	oldTerminate := webhookKillPortTerminateProcess
	webhookKillPortListListeningPIDs = func(port int) ([]int, error) {
		return []int{321}, nil
	}
	webhookKillPortTerminateProcess = func(pid int, timeout time.Duration) error {
		return nil
	}
	defer func() {
		webhookKillPortListListeningPIDs = oldList
		webhookKillPortTerminateProcess = oldTerminate
	}()

	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)
	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       999,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: nowText,
		UpdatedAt: nowText,
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}

	result, err := executeWebhookKillPort(":8080", time.Second, runtimePath, false)
	if err != nil {
		t.Fatalf("executeWebhookKillPort stopped failed: %v", err)
	}
	if result.Status != "stopped" {
		t.Fatalf("expected status stopped, got %+v", result)
	}
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if !exists {
		t.Fatalf("expected runtime state kept when pid mismatched")
	}
}

func TestEnsureWebhookLegacyRouteWritesDefault(t *testing.T) {
	routesPath := filepath.Join(t.TempDir(), "webhook_routes.json")
	routes, err := ensureWebhookLegacyRoute(routesPath, "/legacy/events")
	if err != nil {
		t.Fatalf("ensureWebhookLegacyRoute failed: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected one legacy route, got %d", len(routes))
	}
	if routes[0].ID != defaultWebhookLegacyRouteID {
		t.Fatalf("unexpected legacy route id: %s", routes[0].ID)
	}
	if routes[0].Path != "/legacy/events" {
		t.Fatalf("unexpected legacy route path: %s", routes[0].Path)
	}
}

func TestWebhookHandlerSyncDownstreamPipelineSuccess(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	auditLog := filepath.Join(tmpDir, "webhook_audit.log")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	signBody := []byte(`{"hello":"world"}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/sync", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/sync", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	resolved := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       "sync-1",
			Path:     "/webhook/sync",
			Mode:     webhookRouteModeSync,
			Pipeline: "webhook sign --third-party-id p1 --token tok --data '{\"x\":1}'",
			Enabled:  true,
		},
		Mode:     webhookRouteModeSync,
		Commands: [][]string{{"webhook", "sign", "--third-party-id", "p1", "--token", "tok", "--data", "{\"x\":1}"}},
		Timeout:  5 * time.Second,
	}
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:               "/webhook/sync",
		TokenStorePath:     tokenStore,
		EventLogPath:       eventLog,
		AuditLogPath:       auditLog,
		AllowSysDownstream: false,
		ResolvedRoute:      &resolved,
		Now: func() time.Time {
			return now
		},
	})

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	entries := readWebhookAuditEntries(t, auditLog)
	if len(entries) == 0 {
		t.Fatalf("expected audit entries")
	}
	last := entries[len(entries)-1]
	if int(last["response_status"].(float64)) != http.StatusOK {
		t.Fatalf("expected audit response_status 200, got %#v", last["response_status"])
	}
	if !strings.Contains(last["request_body"].(string), "\"hello\":\"world\"") {
		t.Fatalf("expected audit request_body to include payload, got %q", last["request_body"].(string))
	}
	requestJSON, ok := last["request_json"].(map[string]any)
	if !ok {
		t.Fatalf("expected request_json object in audit entry, got %#v", last["request_json"])
	}
	if requestJSON["hello"] != "world" {
		t.Fatalf("expected request_json.hello=world, got %#v", requestJSON["hello"])
	}
	if requestJSONValid, ok := last["request_json_valid"].(bool); !ok || !requestJSONValid {
		t.Fatalf("expected request_json_valid=true, got %#v", last["request_json_valid"])
	}
	responseJSON, ok := last["response_json"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_json object in audit entry, got %#v", last["response_json"])
	}
	metaRaw, ok := responseJSON["meta"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_json.meta object, got %#v", responseJSON["meta"])
	}
	if strings.TrimSpace(last["response_event_id"].(string)) == "" {
		t.Fatalf("expected response_event_id populated in audit entry")
	}
	if last["response_event_id"].(string) != metaRaw["event_id"].(string) {
		t.Fatalf("expected response_event_id synced with response_json.meta.event_id, got %q vs %q", last["response_event_id"].(string), metaRaw["event_id"].(string))
	}
	if last["response_third_party_id"] != "partner-a" {
		t.Fatalf("expected response_third_party_id=partner-a, got %#v", last["response_third_party_id"])
	}
	if tokenValid, ok := last["response_token_valid"].(bool); !ok || !tokenValid {
		t.Fatalf("expected response_token_valid=true, got %#v", last["response_token_valid"])
	}
	if timestampValid, ok := last["response_timestamp_valid"].(bool); !ok || !timestampValid {
		t.Fatalf("expected response_timestamp_valid=true, got %#v", last["response_timestamp_valid"])
	}
	if jsonValid, ok := last["response_json_valid"].(bool); !ok || !jsonValid {
		t.Fatalf("expected response_json_valid=true, got %#v", last["response_json_valid"])
	}
}

func TestWebhookHandlerSyncDownstreamPipelineFailure(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	auditLog := filepath.Join(tmpDir, "webhook_audit.log")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	signBody := []byte(`{"hello":"world"}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/sync-fail", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/sync-fail", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	resolved := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       "sync-fail",
			Path:     "/webhook/sync-fail",
			Mode:     webhookRouteModeSync,
			Pipeline: "sys --shell 'echo blocked'",
			Enabled:  true,
		},
		Mode:     webhookRouteModeSync,
		Commands: [][]string{{"sys", "--shell", "echo blocked"}},
		Timeout:  5 * time.Second,
	}
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:               "/webhook/sync-fail",
		TokenStorePath:     tokenStore,
		EventLogPath:       eventLog,
		AuditLogPath:       auditLog,
		AllowSysDownstream: false,
		ResolvedRoute:      &resolved,
		Now: func() time.Time {
			return now
		},
	})

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if !strings.Contains(event.Meta.Error, "downstream pipeline failed") {
		t.Fatalf("expected downstream failure message, got %q", event.Meta.Error)
	}
}

func TestBuildWebhookPipelineSeedCarriesRawBody(t *testing.T) {
	route := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:   "r1",
			Path: "/webhook/r1",
			Mode: webhookRouteModeSync,
		},
		Mode: webhookRouteModeSync,
	}
	event := webhookEventEnvelope{
		Meta: webhookEventMeta{
			EventID:      "evt-1",
			ThirdPartyID: "partner-a",
		},
		Data:    map[string]any{"hello": "world"},
		RawBody: []byte(`{"hello":"world"}`),
	}

	seed := buildWebhookPipelineSeed(route, event)
	result, ok := seed.result.(map[string]any)
	if !ok {
		t.Fatalf("expected pipeline seed result map, got %T", seed.result)
	}
	raw, ok := result["raw_body"].(string)
	if !ok {
		t.Fatalf("expected raw_body string in pipeline seed, got %#v", result["raw_body"])
	}
	if raw != `{"hello":"world"}` {
		t.Fatalf("unexpected raw_body in pipeline seed: %q", raw)
	}
}

func TestWebhookHandlerSyncDownstreamMailSendSuccess(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	auditLog := filepath.Join(tmpDir, "webhook_audit.log")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	smtpServer := startWebhookTestSMTPServer(t)
	defer smtpServer.Close()
	t.Setenv("SMTP_HOST", smtpServer.host)
	t.Setenv("SMTP_PORT", strconv.Itoa(smtpServer.port))
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	t.Setenv("SMTP_USERNAME", "")
	t.Setenv("SMTP_PASSWORD", "")
	t.Setenv("MAIL_HOST", "")
	t.Setenv("MAIL_PORT", "")
	t.Setenv("MAIL_USERNAME", "")
	t.Setenv("MAIL_PASSWORD", "")
	t.Setenv("MAIL_FROM", "")

	signBody := []byte(`{"hello":"world"}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/mail-sync", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/mail-sync", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	resolved := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       "mail-sync",
			Path:     "/webhook/mail-sync",
			Mode:     webhookRouteModeSync,
			Pipeline: "mail send --to receiver@example.com --subject cool",
			Enabled:  true,
		},
		Mode: webhookRouteModeSync,
	}
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:           "/webhook/mail-sync",
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		AuditLogPath:   auditLog,
		ResolvedRoute:  &resolved,
		Now: func() time.Time {
			return now
		},
	})

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	rawMessage := smtpServer.WaitMessage(t, 3*time.Second)
	if !strings.Contains(rawMessage, "To: receiver@example.com") {
		t.Fatalf("expected recipient in smtp message, got:\n%s", rawMessage)
	}
	decodedBody := decodeWebhookSMTPBody(t, rawMessage)
	if decodedBody != signResult.Body {
		t.Fatalf("expected email body to equal raw webhook body, got=%q want=%q", decodedBody, signResult.Body)
	}

	entries := readWebhookAuditEntries(t, auditLog)
	if len(entries) == 0 {
		t.Fatalf("expected audit entries")
	}
	last := entries[len(entries)-1]
	dispatchRaw, ok := last["dispatch"].(map[string]any)
	if !ok {
		t.Fatalf("expected dispatch audit payload, got %#v", last["dispatch"])
	}
	if dispatchRaw["status"] != "success" {
		t.Fatalf("expected dispatch status success, got %#v", dispatchRaw["status"])
	}
	resultRaw, ok := dispatchRaw["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected dispatch result payload, got %#v", dispatchRaw["result"])
	}
	if resultRaw["subject"] != "cool" {
		t.Fatalf("expected dispatch result subject=cool, got %#v", resultRaw["subject"])
	}
}

func TestWebhookHandlerSyncDownstreamMailSendFailureReturns500(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	t.Setenv("SMTP_HOST", "127.0.0.1")
	t.Setenv("SMTP_PORT", "1")
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	t.Setenv("SMTP_USERNAME", "")
	t.Setenv("SMTP_PASSWORD", "")
	t.Setenv("MAIL_HOST", "")
	t.Setenv("MAIL_PORT", "")
	t.Setenv("MAIL_USERNAME", "")
	t.Setenv("MAIL_PASSWORD", "")
	t.Setenv("MAIL_FROM", "")

	signBody := []byte(`{"hello":"world"}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/mail-sync-fail", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/mail-sync-fail", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	resolved := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       "mail-sync-fail",
			Path:     "/webhook/mail-sync-fail",
			Mode:     webhookRouteModeSync,
			Pipeline: "mail send --to receiver@example.com --subject cool",
			Enabled:  true,
		},
		Mode: webhookRouteModeSync,
	}
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:           "/webhook/mail-sync-fail",
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		ResolvedRoute:  &resolved,
		Now: func() time.Time {
			return now
		},
	})

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	event := decodeWebhookResponse(t, rr.Body.Bytes())
	if !strings.Contains(event.Meta.Error, "downstream pipeline failed") {
		t.Fatalf("expected downstream failure message, got %q", event.Meta.Error)
	}
	if !strings.Contains(strings.ToLower(event.Meta.Error), "stage 1") {
		t.Fatalf("expected stage info in downstream failure message, got %q", event.Meta.Error)
	}
}

func TestWebhookHandlerAsyncEnqueueReturnsAccepted(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	queuePath := filepath.Join(tmpDir, "dispatch_queue.json")
	historyPath := filepath.Join(tmpDir, "dispatch_history.jsonl")
	deadPath := filepath.Join(tmpDir, "dispatch_dead.jsonl")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	dispatcher, err := newWebhookDispatchManager(webhookDispatcherOptions{
		QueuePath:      queuePath,
		HistoryPath:    historyPath,
		DeadLetterPath: deadPath,
		Workers:        1,
		PollInterval:   10 * time.Second,
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("newWebhookDispatchManager failed: %v", err)
	}

	signBody := []byte(`{"async":true}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/async", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/async", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	resolved := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:       "async-1",
			Path:     "/webhook/async",
			Mode:     webhookRouteModeAsync,
			Pipeline: "webhook sign --third-party-id p1 --token tok --data '{\"x\":1}'",
			Enabled:  true,
		},
		Mode:     webhookRouteModeAsync,
		Commands: [][]string{{"webhook", "sign", "--third-party-id", "p1", "--token", "tok", "--data", "{\"x\":1}"}},
		Timeout:  5 * time.Second,
	}
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:               "/webhook/async",
		TokenStorePath:     tokenStore,
		EventLogPath:       eventLog,
		AllowSysDownstream: false,
		Dispatcher:         dispatcher,
		ResolvedRoute:      &resolved,
		Now: func() time.Time {
			return now
		},
	})

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rr.Code, rr.Body.String())
	}

	jobs, err := readWebhookDispatchQueue(queuePath)
	if err != nil {
		t.Fatalf("readWebhookDispatchQueue failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 queued job, got %d", len(jobs))
	}
}

func TestWebhookAuditLogCapturesUnauthorizedResponse(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	auditLog := filepath.Join(tmpDir, "webhook_audit.log")
	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"x":1}`)
	signResult := buildWebhookSignResult("partner-a", "wrong-token", "1710000000", body, "/webhook/events", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:           "/webhook/events",
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		AuditLogPath:   auditLog,
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}

	entries := readWebhookAuditEntries(t, auditLog)
	if len(entries) == 0 {
		t.Fatalf("expected audit entry")
	}
	last := entries[len(entries)-1]
	if int(last["response_status"].(float64)) != http.StatusUnauthorized {
		t.Fatalf("expected response_status 401, got %#v", last["response_status"])
	}
	if !strings.Contains(last["request_body"].(string), `"x":1`) && !strings.Contains(last["request_body"].(string), `"x": 1`) {
		t.Fatalf("expected request body in audit entry, got %q", last["request_body"].(string))
	}
	requestJSON, ok := last["request_json"].(map[string]any)
	if !ok {
		t.Fatalf("expected request_json object in audit entry, got %#v", last["request_json"])
	}
	if requestJSON["x"].(float64) != 1 {
		t.Fatalf("expected request_json.x=1, got %#v", requestJSON["x"])
	}
	if requestJSONValid, ok := last["request_json_valid"].(bool); !ok || !requestJSONValid {
		t.Fatalf("expected request_json_valid=true, got %#v", last["request_json_valid"])
	}
}

func TestWebhookAuditLogCapturesInvalidJSONRequestInsights(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	auditLog := filepath.Join(tmpDir, "webhook_audit.log")
	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	body := []byte(`{"x":`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", body, "/webhook/events", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook/events", strings.NewReader(signResult.Body))
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:           "/webhook/events",
		TokenStorePath: tokenStore,
		EventLogPath:   eventLog,
		AuditLogPath:   auditLog,
		Now: func() time.Time {
			return now
		},
	})
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}

	entries := readWebhookAuditEntries(t, auditLog)
	if len(entries) == 0 {
		t.Fatalf("expected audit entry")
	}
	last := entries[len(entries)-1]
	if int(last["response_status"].(float64)) != http.StatusBadRequest {
		t.Fatalf("expected response_status 400, got %#v", last["response_status"])
	}
	if !strings.Contains(last["request_body"].(string), `{"x":`) {
		t.Fatalf("expected request body in audit entry, got %q", last["request_body"].(string))
	}
	if _, exists := last["request_json"]; exists {
		t.Fatalf("expected request_json omitted for invalid json body, got %#v", last["request_json"])
	}
	if requestJSONValid, ok := last["request_json_valid"].(bool); !ok || requestJSONValid {
		t.Fatalf("expected request_json_valid=false, got %#v", last["request_json_valid"])
	}
}

func TestBuildWebhookPipelineDebugResultPrefersStructuredRequestJSON(t *testing.T) {
	entry := webhookAuditLogEntry{
		EventID:        "evt-prefer-structured",
		RouteID:        "route-a",
		Method:         http.MethodPost,
		Path:           "/webhook/route-a",
		RequestBody:    `{"broken":`,
		RequestJSON:    map[string]any{"hello": "structured"},
		ResponseStatus: http.StatusOK,
		ReceivedAt:     "2026-05-01T10:00:00+08:00",
	}

	result := buildWebhookPipelineDebugResult(entry)
	if result.DataParseError != "" {
		t.Fatalf("expected no data parse error when request_json is present, got %s", result.DataParseError)
	}
	data, ok := result.PipelineSeed["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected structured data map in pipeline seed, got %#v", result.PipelineSeed["data"])
	}
	if data["hello"] != "structured" {
		t.Fatalf("unexpected structured data payload: %#v", data)
	}
	if rawBody, ok := result.PipelineSeed["raw_body"].(string); !ok || rawBody != `{"broken":` {
		t.Fatalf("expected raw_body preserved, got %#v", result.PipelineSeed["raw_body"])
	}
}

func TestWebhookRouteCommandLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	routesPath := filepath.Join(tmpDir, "routes.json")

	runRoute := func(args ...string) error {
		app := NewAppContext()
		cmd := newWebhookRouteCommand(app)
		cmd.SetArgs(args)
		return cmd.Execute()
	}

	if err := runRoute(
		"--routes-file", routesPath,
		"add", "r-1",
		"--path", "/r1",
		"--mode", "sync",
		"--pipeline", "webhook sign --third-party-id p1 --token t1 --data '{\"x\":1}'",
	); err != nil {
		t.Fatalf("route add failed: %v", err)
	}

	records, err := loadWebhookRouteRecords(routesPath)
	if err != nil {
		t.Fatalf("loadWebhookRouteRecords failed: %v", err)
	}
	if _, exists := records["r-1"]; !exists {
		t.Fatalf("expected route r-1 after add")
	}

	if err := runRoute(
		"--routes-file", routesPath,
		"update", "r-1",
		"--mode", "async",
	); err != nil {
		t.Fatalf("route update failed: %v", err)
	}
	records, err = loadWebhookRouteRecords(routesPath)
	if err != nil {
		t.Fatalf("loadWebhookRouteRecords failed: %v", err)
	}
	if normalizeWebhookRouteMode(records["r-1"].Mode) != webhookRouteModeAsync {
		t.Fatalf("expected route mode async after update, got %s", records["r-1"].Mode)
	}

	if err := runRoute("--routes-file", routesPath, "remove", "r-1", "r-1"); err != nil {
		t.Fatalf("route remove failed: %v", err)
	}
	records, err = loadWebhookRouteRecords(routesPath)
	if err != nil {
		t.Fatalf("loadWebhookRouteRecords failed: %v", err)
	}
	if _, exists := records["r-1"]; exists {
		t.Fatalf("expected route r-1 removed")
	}
}

func TestWebhookTokenCacheReloadsOnFileChange(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "webhook_tokens.json")
	now := time.Unix(1710000000, 0).UTC()
	if err := writeWebhookTokenRecords(storePath, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "token-v1",
			CreatedAt:    now.Format(time.RFC3339Nano),
			UpdatedAt:    now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	cache, err := newWebhookTokenCache(storePath, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("newWebhookTokenCache failed: %v", err)
	}
	cache.Start()
	defer cache.Stop()

	record, ok, err := cache.Find("partner-a")
	if err != nil {
		t.Fatalf("cache find failed: %v", err)
	}
	if !ok || record.Token != "token-v1" {
		t.Fatalf("unexpected initial token from cache: ok=%t record=%+v", ok, record)
	}

	if err := writeWebhookTokenRecords(storePath, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "token-v2",
			CreatedAt:    now.Format(time.RFC3339Nano),
			UpdatedAt:    now.Add(1 * time.Minute).Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("update token store failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		updated, exists, findErr := cache.Find("partner-a")
		if findErr == nil && exists && updated.Token == "token-v2" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("token cache did not pick updated token within timeout")
}

func TestWebhookManagementEndpointsExposeStatusAndMetrics(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	tmpDir := t.TempDir()
	tokenStore := filepath.Join(tmpDir, "tokens.json")
	eventLog := filepath.Join(tmpDir, "events.json")
	auditLog := filepath.Join(tmpDir, "webhook_audit.log")
	queuePath := filepath.Join(tmpDir, "dispatch_queue.json")
	deadPath := filepath.Join(tmpDir, "dispatch_dead.jsonl")

	if err := writeWebhookTokenRecords(tokenStore, map[string]webhookTokenRecord{
		"partner-a": {ThirdPartyID: "partner-a", Token: "secret-token"},
	}); err != nil {
		t.Fatalf("writeWebhookTokenRecords failed: %v", err)
	}

	cache, err := newWebhookTokenCache(tokenStore, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("newWebhookTokenCache failed: %v", err)
	}
	cache.Start()
	defer cache.Stop()

	eventWriter, err := newWebhookEventLogWriter(eventLog)
	if err != nil {
		t.Fatalf("newWebhookEventLogWriter failed: %v", err)
	}
	eventWriter.Start()
	defer eventWriter.Stop()

	auditWriter, err := newWebhookAuditLogWriter(auditLog)
	if err != nil {
		t.Fatalf("newWebhookAuditLogWriter failed: %v", err)
	}
	auditWriter.Start()
	defer auditWriter.Stop()

	metrics := newWebhookServerMetrics(128)
	cfg := newWebhookServeConfig()
	cfg.PublicAddr = ":18080"
	cfg.Addr = cfg.PublicAddr
	cfg.Path = "/webhook/events"
	cfg.DispatchQueuePath = queuePath
	cfg.DeadLetterPath = deadPath

	route := webhookResolvedRoute{
		Record: webhookRouteRecord{
			ID:      "route-1",
			Path:    "/webhook/events",
			Mode:    webhookRouteModeSync,
			Enabled: true,
		},
		Mode: webhookRouteModeSync,
	}

	mux := http.NewServeMux()
	registerWebhookManagementHandlers(mux, cfg, []webhookResolvedRoute{route}, metrics, cache, eventWriter, auditWriter, time.Now(), nil)
	handler := newWebhookEventHandler(webhookServeOptions{
		Path:         "/webhook/events",
		TokenFinder:  cache.Find,
		EventLogPath: eventLog,
		AuditLogPath: auditLog,
		EventLogAppender: func(event webhookEventEnvelope) error {
			return appendWebhookEventLogAsync(eventWriter, event)
		},
		AuditLogAppender: func(entry webhookAuditLogEntry) error {
			return appendWebhookAuditLogAsync(auditWriter, entry)
		},
		Metrics: metrics,
		Now: func() time.Time {
			return now
		},
	})
	mux.Handle(route.Record.Path, handler)

	server := httptest.NewServer(mux)
	defer server.Close()

	healthResp, err := http.Get(server.URL + webhookHealthzPath)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /healthz 200, got %d", healthResp.StatusCode)
	}
	_ = healthResp.Body.Close()

	readyResp, err := http.Get(server.URL + webhookReadyzPath)
	if err != nil {
		t.Fatalf("GET /readyz failed: %v", err)
	}
	if readyResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /readyz 200, got %d", readyResp.StatusCode)
	}
	_ = readyResp.Body.Close()

	signBody := []byte(`{"hello":"world"}`)
	signResult := buildWebhookSignResult("partner-a", "secret-token", "1710000000", signBody, "/webhook/events", "")
	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/events", strings.NewReader(signResult.Body))
	if err != nil {
		t.Fatalf("new webhook request failed: %v", err)
	}
	for key, value := range signResult.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second}
	postResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /webhook/events failed: %v", err)
	}
	if postResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected /webhook/events 200, got %d body=%s", postResp.StatusCode, string(body))
	}
	_ = postResp.Body.Close()

	statusResp, err := http.Get(server.URL + webhookAdminStatusPath)
	if err != nil {
		t.Fatalf("GET /admin/webhook/status failed: %v", err)
	}
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /admin/webhook/status 200, got %d", statusResp.StatusCode)
	}
	var statusPayload webhookAdminStatusResponse
	if err := json.NewDecoder(statusResp.Body).Decode(&statusPayload); err != nil {
		t.Fatalf("decode /admin/webhook/status failed: %v", err)
	}
	_ = statusResp.Body.Close()

	if statusPayload.RouteCount != 1 {
		t.Fatalf("expected route_count=1, got %d", statusPayload.RouteCount)
	}
	if statusPayload.Metrics.RequestTotal < 1 {
		t.Fatalf("expected request_total >= 1, got %+v", statusPayload.Metrics)
	}
	if statusPayload.LogQueueDepth.Event < 0 || statusPayload.LogQueueDepth.Audit < 0 {
		t.Fatalf("invalid log queue depth: %+v", statusPayload.LogQueueDepth)
	}

	metricsResp, err := http.Get(server.URL + webhookMetricsPath)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /metrics 200, got %d", metricsResp.StatusCode)
	}
	rawMetrics, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	text := string(rawMetrics)
	if !strings.Contains(text, "webhook_requests_total") {
		t.Fatalf("expected metrics output contains webhook_requests_total, got %q", text)
	}
	if !strings.Contains(text, "webhook_log_queue_depth_event") {
		t.Fatalf("expected metrics output contains webhook_log_queue_depth_event, got %q", text)
	}

	adminResp, err := http.Get(server.URL + webhookAdminHomePath)
	if err != nil {
		t.Fatalf("GET /admin failed: %v", err)
	}
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /admin 200, got %d", adminResp.StatusCode)
	}
	adminContentType := adminResp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(adminContentType)), "text/html") {
		t.Fatalf("expected /admin text/html content-type, got %q", adminContentType)
	}
	adminBodyBytes, _ := io.ReadAll(adminResp.Body)
	_ = adminResp.Body.Close()
	adminBody := string(adminBodyBytes)

	for _, snippet := range []string{
		"Webhook Admin Home",
		webhookHealthzPath,
		webhookReadyzPath,
		webhookAdminStatusPath,
		webhookMetricsPath,
		webhookAdminStopPath,
		"POST-only",
		"route-1",
		"/webhook/events",
		"Refresh Page",
		"Stop Current Webhook",
		"Stop current webhook now?",
		"stopCurrentWebhook(",
		"id=\"stop-status\"",
	} {
		if !strings.Contains(adminBody, snippet) {
			t.Fatalf("expected /admin response contains %q", snippet)
		}
	}

	adminPostReq, err := http.NewRequest(http.MethodPost, server.URL+webhookAdminHomePath, nil)
	if err != nil {
		t.Fatalf("build POST /admin request failed: %v", err)
	}
	adminPostResp, err := client.Do(adminPostReq)
	if err != nil {
		t.Fatalf("POST /admin failed: %v", err)
	}
	if adminPostResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected POST /admin 405, got %d", adminPostResp.StatusCode)
	}
	_ = adminPostResp.Body.Close()

	adminSlashResp, err := client.Get(server.URL + webhookAdminHomeSlash)
	if err != nil {
		t.Fatalf("GET /admin/ failed: %v", err)
	}
	if adminSlashResp.Request == nil || adminSlashResp.Request.URL == nil || adminSlashResp.Request.URL.Path != webhookAdminHomePath {
		t.Fatalf("expected GET /admin/ redirect target path %q, got request=%v", webhookAdminHomePath, adminSlashResp.Request)
	}
	_ = adminSlashResp.Body.Close()
}

func TestWebhookServeDualSurfaceRouteIsolation(t *testing.T) {
	cfg := newWebhookServeConfig()
	cfg.PublicAddr = "127.0.0.1:18080"
	cfg.Addr = cfg.PublicAddr
	cfg.AdminAddr = "127.0.0.1:8081"
	cfg.Path = "/webhook/events"

	publicMux := http.NewServeMux()
	adminMux := http.NewServeMux()
	registerWebhookManagementHandlers(
		adminMux,
		cfg,
		nil,
		newWebhookServerMetrics(1),
		&webhookTokenCache{},
		&webhookAsyncLineWriter{},
		&webhookAsyncLineWriter{},
		time.Now(),
		nil,
	)
	publicMux.Handle(cfg.Path, newWebhookEventHandler(webhookServeOptions{Path: cfg.Path}))

	publicServer := httptest.NewServer(publicMux)
	defer publicServer.Close()
	adminServer := httptest.NewServer(adminMux)
	defer adminServer.Close()

	publicWebhookResp, err := http.Get(publicServer.URL + cfg.Path)
	if err != nil {
		t.Fatalf("GET public webhook path failed: %v", err)
	}
	if publicWebhookResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected public webhook path status 405, got %d", publicWebhookResp.StatusCode)
	}
	_ = publicWebhookResp.Body.Close()

	publicAdminStatusResp, err := http.Get(publicServer.URL + webhookAdminStatusPath)
	if err != nil {
		t.Fatalf("GET public admin status path failed: %v", err)
	}
	if publicAdminStatusResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected public admin status path 404, got %d", publicAdminStatusResp.StatusCode)
	}
	_ = publicAdminStatusResp.Body.Close()

	publicAdminHealthResp, err := http.Get(publicServer.URL + webhookHealthzPath)
	if err != nil {
		t.Fatalf("GET public admin healthz path failed: %v", err)
	}
	if publicAdminHealthResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected public admin healthz path 404, got %d", publicAdminHealthResp.StatusCode)
	}
	_ = publicAdminHealthResp.Body.Close()

	publicLegacyHealthResp, err := http.Get(publicServer.URL + legacyWebhookHealthzPath)
	if err != nil {
		t.Fatalf("GET public legacy healthz path failed: %v", err)
	}
	if publicLegacyHealthResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected public legacy healthz path 404, got %d", publicLegacyHealthResp.StatusCode)
	}
	_ = publicLegacyHealthResp.Body.Close()

	adminStatusResp, err := http.Get(adminServer.URL + webhookAdminStatusPath)
	if err != nil {
		t.Fatalf("GET admin status path failed: %v", err)
	}
	if adminStatusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin status path 200, got %d", adminStatusResp.StatusCode)
	}
	_ = adminStatusResp.Body.Close()

	adminHealthResp, err := http.Get(adminServer.URL + webhookHealthzPath)
	if err != nil {
		t.Fatalf("GET admin healthz path failed: %v", err)
	}
	if adminHealthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin healthz path 200, got %d", adminHealthResp.StatusCode)
	}
	_ = adminHealthResp.Body.Close()

	adminWebhookResp, err := http.Get(adminServer.URL + cfg.Path)
	if err != nil {
		t.Fatalf("GET admin webhook path failed: %v", err)
	}
	if adminWebhookResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected admin webhook path 404, got %d", adminWebhookResp.StatusCode)
	}
	_ = adminWebhookResp.Body.Close()
}

func TestValidateWebhookManagementPathConflictsIncludesAdminHome(t *testing.T) {
	cases := []string{
		webhookAdminHomePath,
		webhookAdminHomeSlash,
		webhookAdminStopPath,
	}
	for _, reservedPath := range cases {
		routes := []webhookResolvedRoute{
			{
				Record: webhookRouteRecord{
					ID:      "route-admin",
					Path:    reservedPath,
					Mode:    webhookRouteModeSync,
					Enabled: true,
				},
				Mode: webhookRouteModeSync,
			},
		}
		err := validateWebhookManagementPathConflicts(routes)
		if err == nil {
			t.Fatalf("expected reserved admin path conflict error for %q", reservedPath)
		}
		if !strings.Contains(err.Error(), reservedPath) {
			t.Fatalf("expected error includes %q, got %v", reservedPath, err)
		}
	}
}

func TestAdminHomeSlashRedirectKeepsQuery(t *testing.T) {
	mux := http.NewServeMux()
	registerWebhookManagementHandlers(mux, newWebhookServeConfig(), nil, newWebhookServerMetrics(1), &webhookTokenCache{}, &webhookAsyncLineWriter{}, &webhookAsyncLineWriter{}, time.Now(), nil)
	server := httptest.NewServer(mux)
	defer server.Close()

	response, err := http.Get(server.URL + webhookAdminHomeSlash + "?from=test")
	if err != nil {
		t.Fatalf("GET /admin/?from=test failed: %v", err)
	}
	if response.Request == nil || response.Request.URL == nil {
		t.Fatalf("expected final request URL after redirect")
	}
	if response.Request.URL.Path != webhookAdminHomePath {
		t.Fatalf("expected redirected path %q, got %q", webhookAdminHomePath, response.Request.URL.Path)
	}
	if response.Request.URL.RawQuery != "from=test" {
		t.Fatalf("expected redirected query kept, got %q", response.Request.URL.RawQuery)
	}
	_ = response.Body.Close()
}

func TestWebhookAdminStopEndpointTriggersShutdownOnce(t *testing.T) {
	mux := http.NewServeMux()
	triggerCh := make(chan string, 2)
	registerWebhookManagementHandlers(
		mux,
		newWebhookServeConfig(),
		nil,
		newWebhookServerMetrics(1),
		&webhookTokenCache{},
		&webhookAsyncLineWriter{},
		&webhookAsyncLineWriter{},
		time.Now(),
		func(source string) {
			triggerCh <- source
		},
	)
	server := httptest.NewServer(mux)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+webhookAdminStopPath, nil)
		if err != nil {
			t.Fatalf("build POST /admin/webhook/stop failed: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /admin/webhook/stop failed: %v", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected POST /admin/webhook/stop 202, got %d", resp.StatusCode)
		}
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decode stop response failed: %v", err)
		}
		_ = resp.Body.Close()
		if strings.TrimSpace(fmt.Sprintf("%v", payload["status"])) != "stopping" {
			t.Fatalf("expected stop response status=stopping, got %#v", payload["status"])
		}
	}

	select {
	case source := <-triggerCh:
		if source != "admin_stop" {
			t.Fatalf("expected shutdown source admin_stop, got %q", source)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected admin stop callback to be triggered")
	}

	select {
	case source := <-triggerCh:
		t.Fatalf("expected admin stop callback only once, got extra source=%q", source)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWebhookAdminStopEndpointRejectsNonPost(t *testing.T) {
	mux := http.NewServeMux()
	registerWebhookManagementHandlers(
		mux,
		newWebhookServeConfig(),
		nil,
		newWebhookServerMetrics(1),
		&webhookTokenCache{},
		&webhookAsyncLineWriter{},
		&webhookAsyncLineWriter{},
		time.Now(),
		nil,
	)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + webhookAdminStopPath)
	if err != nil {
		t.Fatalf("GET /admin/webhook/stop failed: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected GET /admin/webhook/stop 405, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestCleanupWebhookRuntimeStateForCurrentProcess(t *testing.T) {
	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)

	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       os.Getpid(),
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: nowText,
		UpdatedAt: nowText,
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}
	removed, err := cleanupWebhookRuntimeStateForCurrentProcess(runtimePath)
	if err != nil {
		t.Fatalf("cleanup runtime state failed: %v", err)
	}
	if !removed {
		t.Fatalf("expected runtime file removed for current pid")
	}
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if exists {
		t.Fatalf("expected runtime state removed for current pid")
	}

	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       999999,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: nowText,
		UpdatedAt: nowText,
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}
	removed, err = cleanupWebhookRuntimeStateForCurrentProcess(runtimePath)
	if err != nil {
		t.Fatalf("cleanup runtime state mismatch pid failed: %v", err)
	}
	if removed {
		t.Fatalf("expected runtime file kept when pid mismatches current process")
	}
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if !exists {
		t.Fatalf("expected runtime state kept when pid mismatches")
	}
}

func TestDaemonCompletionIncludesWebhookRootCommand(t *testing.T) {
	candidates := completionCandidatesFromLine("quote AAPL.US | ")
	if !slices.Contains(candidates, "webhook") {
		t.Fatalf("expected webhook candidate after pipeline, got: %v", candidates)
	}
}

func decodeWebhookResponse(t *testing.T, raw []byte) webhookEventEnvelope {
	t.Helper()
	var direct webhookEventEnvelope
	if err := json.Unmarshal(raw, &direct); err == nil {
		if strings.TrimSpace(direct.Meta.EventID) != "" || strings.TrimSpace(direct.Meta.Error) != "" {
			return direct
		}
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode response failed: %v, raw=%s", err, string(raw))
	}
	if data, ok := root["data"]; ok {
		encoded, _ := json.Marshal(data)
		var event webhookEventEnvelope
		if err := json.Unmarshal(encoded, &event); err == nil {
			if strings.TrimSpace(event.Meta.EventID) != "" || strings.TrimSpace(event.Meta.Error) != "" {
				return event
			}
		}
	}
	if details, ok := root["details"].(map[string]any); ok {
		if eventRaw, ok := details["event"]; ok {
			encoded, _ := json.Marshal(eventRaw)
			var event webhookEventEnvelope
			if err := json.Unmarshal(encoded, &event); err == nil {
				return event
			}
		}
	}
	return direct
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}

func readWebhookAuditEntries(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit log failed: %v", err)
	}
	defer file.Close()

	entries := make([]map[string]any, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode audit line failed: %v line=%s", err, line)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan audit log failed: %v", err)
	}
	return entries
}

type webhookTestSMTPServer struct {
	listener net.Listener
	host     string
	port     int
	received chan string
	done     chan struct{}
	once     sync.Once
}

func startWebhookTestSMTPServer(t *testing.T) *webhookTestSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start smtp test server failed: %v", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		t.Fatalf("unexpected smtp listener addr type: %T", listener.Addr())
	}

	server := &webhookTestSMTPServer{
		listener: listener,
		host:     "127.0.0.1",
		port:     address.Port,
		received: make(chan string, 8),
		done:     make(chan struct{}),
	}

	go server.acceptLoop()
	t.Cleanup(func() {
		server.Close()
	})
	return server
}

func (s *webhookTestSMTPServer) Close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.listener.Close()
	})
}

func (s *webhookTestSMTPServer) WaitMessage(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case message := <-s.received:
		return message
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for smtp message")
		return ""
	}
}

func (s *webhookTestSMTPServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				return
			}
		}
		go s.handleConn(conn)
	}
}

func (s *webhookTestSMTPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	writeLine := func(line string) bool {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}

	if !writeLine("220 localhost ESMTP test") {
		return
	}

	var dataBuffer bytes.Buffer
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(trimmed)

		if inData {
			if trimmed == "." {
				inData = false
				select {
				case s.received <- dataBuffer.String():
				default:
				}
				if !writeLine("250 queued as webhook-test") {
					return
				}
				continue
			}
			dataBuffer.WriteString(line)
			continue
		}

		switch {
		case strings.HasPrefix(upper, "EHLO "), strings.HasPrefix(upper, "HELO "):
			if _, err := writer.WriteString("250-localhost\r\n250 OK\r\n"); err != nil {
				return
			}
			if err := writer.Flush(); err != nil {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			if !writeLine("250 OK") {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			if !writeLine("250 OK") {
				return
			}
		case upper == "DATA":
			dataBuffer.Reset()
			inData = true
			if !writeLine("354 End data with <CR><LF>.<CR><LF>") {
				return
			}
		case upper == "QUIT":
			_ = writeLine("221 Bye")
			return
		default:
			if !writeLine("250 OK") {
				return
			}
		}
	}
}

func decodeWebhookSMTPBody(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid smtp raw message, missing header/body separator: %q", raw)
	}
	encoded := strings.ReplaceAll(parts[1], "\r\n", "")
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("decode smtp body failed: %v raw=%q", err, parts[1])
	}
	return string(decoded)
}

func withWebhookTestWorkingDir(t *testing.T, dir string) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir to %s failed: %v", dir, err)
	}
	t.Cleanup(func() {
		if chdirErr := os.Chdir(oldDir); chdirErr != nil {
			t.Fatalf("restore working dir failed: %v", chdirErr)
		}
	})
}
