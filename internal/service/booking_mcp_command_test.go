package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type bookingMCPRoundTripper func(*http.Request) (*http.Response, error)

func (fn bookingMCPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func withBookingMCPMockTransport(t *testing.T, fn bookingMCPRoundTripper) {
	t.Helper()
	oldFactory := bookingMCPHTTPClientFactory
	bookingMCPHTTPClientFactory = func(timeout time.Duration) *http.Client {
		if timeout <= 0 {
			timeout = bookingMCPDefaultRequestTimeout
		}
		return &http.Client{
			Timeout:   timeout,
			Transport: fn,
		}
	}
	t.Cleanup(func() {
		bookingMCPHTTPClientFactory = oldFactory
	})
}

func withBookingMCPRuntimeMocks(
	t *testing.T,
	statusFn func(string) (bookingServiceStatusResult, error),
	startFn func(string, string, *bookingServiceServeConfig) (bookingServiceStartResult, error),
	readyFn func(string, webhookTokenRecord, time.Duration) error,
) {
	t.Helper()
	oldStatusFn := bookingMCPStatusFn
	oldStartFn := bookingMCPStartFn
	oldReadyFn := bookingMCPFetchCatalogStatusFn
	bookingMCPStatusFn = statusFn
	bookingMCPStartFn = startFn
	bookingMCPFetchCatalogStatusFn = readyFn
	t.Cleanup(func() {
		bookingMCPStatusFn = oldStatusFn
		bookingMCPStartFn = oldStartFn
		bookingMCPFetchCatalogStatusFn = oldReadyFn
	})
}

func decodeBookingMCPToolOutput(t *testing.T, result bookingMCPToolCallResult) bookingMCPToolOutput {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content failed: %v", err)
	}
	var output bookingMCPToolOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode structured content failed: %v raw=%s", err, string(raw))
	}
	return output
}

func toolCallParams(name string, args map[string]any) json.RawMessage {
	payload := map[string]any{
		"name": name,
	}
	if args != nil {
		payload["arguments"] = args
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func TestBookingMCPServerScopeValidation(t *testing.T) {
	securityPath := filepath.Join(t.TempDir(), "security_keys.json")
	if err := writeSecurityTokenRecords(securityPath, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "token-webhook-only",
			Scopes:       []string{securityScopeWebhook},
		},
	}); err != nil {
		t.Fatalf("writeSecurityTokenRecords failed: %v", err)
	}

	_, err := newBookingMCPServer(bookingMCPServeConfig{
		ThirdPartyID:   "partner-a",
		SecurityKeys:   securityPath,
		PublicBaseURL:  "http://booking.test",
		RuntimePath:    filepath.Join(t.TempDir(), "runtime.json"),
		AutoStart:      false,
		StartupTimeout: time.Second,
	})
	if err == nil {
		t.Fatalf("expected token scope validation error")
	}
	if !strings.Contains(err.Error(), "token scope not allowed for booking") {
		t.Fatalf("unexpected scope validation error: %v", err)
	}
}

func TestBookingMCPToolsCallAndHeaders(t *testing.T) {
	type capturedRequest struct {
		Method      string
		Path        string
		RawQuery    string
		Body        string
		Idempotency string
	}
	var (
		mu       sync.Mutex
		captured []capturedRequest
	)

	withBookingMCPMockTransport(t, func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		timestamp := strings.TrimSpace(r.Header.Get(webhookHeaderTimestamp))
		signature := strings.TrimSpace(r.Header.Get(webhookHeaderSignature))
		thirdPartyID := strings.TrimSpace(r.Header.Get(webhookHeaderThirdPartyID))
		wantSignature := computeWebhookSignature(thirdPartyID, timestamp, "token-inline", body)
		if signature != wantSignature {
			t.Fatalf("signature mismatch: got=%s want=%s", signature, wantSignature)
		}

		mu.Lock()
		captured = append(captured, capturedRequest{
			Method:      strings.TrimSpace(r.Method),
			Path:        strings.TrimSpace(r.URL.Path),
			RawQuery:    strings.TrimSpace(r.URL.RawQuery),
			Body:        string(body),
			Idempotency: strings.TrimSpace(r.Header.Get(publicAPIHeaderIdempotencyKey)),
		})
		mu.Unlock()

		switch r.URL.Path {
		case bookingPublicCatalogPath:
			return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-catalog",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"products": []any{},
				},
			}), nil
		case bookingPublicReservationsPath:
			return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-reservations",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"reservations": []any{},
				},
			}), nil
		case bookingPublicIntentParsePath:
			if strings.TrimSpace(r.Header.Get(publicAPIHeaderIdempotencyKey)) == "" {
				t.Fatalf("parse request is missing idempotency key")
			}
			return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-parse",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"draft_id": "d-1",
				},
			}), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})

	server, err := newBookingMCPServer(bookingMCPServeConfig{
		ThirdPartyID:   "partner-a",
		Token:          "token-inline",
		SecurityKeys:   "unused-security-keys.json",
		PublicBaseURL:  "http://booking.test",
		RuntimePath:    filepath.Join(t.TempDir(), "runtime.json"),
		AutoStart:      false,
		StartupTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("newBookingMCPServer failed: %v", err)
	}

	catalogResult, catalogErr := server.handleToolsCall(context.Background(), toolCallParams(bookingMCPToolCatalogQuery, map[string]any{
		"product_id":   "p-1",
		"include_full": true,
		"from":         "2026-05-01T00:00:00+08:00",
		"to":           "2026-05-31T23:59:59+08:00",
	}))
	if catalogErr != nil {
		t.Fatalf("catalog tool call returned rpc error: %+v", catalogErr)
	}
	catalogOutput := decodeBookingMCPToolOutput(t, catalogResult)
	if !catalogOutput.OK || catalogOutput.Code != publicAPIErrorCodeOK || catalogOutput.RequestID != "req-catalog" {
		t.Fatalf("unexpected catalog output: %+v", catalogOutput)
	}

	reservationsResult, reservationsErr := server.handleToolsCall(context.Background(), toolCallParams(bookingMCPToolReservationsList, map[string]any{
		"user_id": "u-1",
		"status":  bookingReservationStatusPending,
	}))
	if reservationsErr != nil {
		t.Fatalf("reservations tool call returned rpc error: %+v", reservationsErr)
	}
	reservationsOutput := decodeBookingMCPToolOutput(t, reservationsResult)
	if !reservationsOutput.OK || reservationsOutput.Code != publicAPIErrorCodeOK || reservationsOutput.RequestID != "req-reservations" {
		t.Fatalf("unexpected reservations output: %+v", reservationsOutput)
	}

	parseResult, parseErr := server.handleToolsCall(context.Background(), toolCallParams(bookingMCPToolIntentParse, map[string]any{
		"user_id": "u-2",
		"content": "我想预定明天下午的体验课",
		"channel": "chat",
	}))
	if parseErr != nil {
		t.Fatalf("parse tool call returned rpc error: %+v", parseErr)
	}
	parseOutput := decodeBookingMCPToolOutput(t, parseResult)
	if !parseOutput.OK || parseOutput.Code != publicAPIErrorCodeOK || parseOutput.RequestID != "req-parse" {
		t.Fatalf("unexpected parse output: %+v", parseOutput)
	}
	if len(parseResult.Content) == 0 || !strings.Contains(parseResult.Content[0].Text, "Idempotency-Key") {
		t.Fatalf("expected parse text content to include idempotency key, got %+v", parseResult.Content)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("expected 3 HTTP requests, got %d", len(captured))
	}
	if captured[0].Path != bookingPublicCatalogPath || !strings.Contains(captured[0].RawQuery, "product_id=p-1") || !strings.Contains(captured[0].RawQuery, "include_full=true") {
		t.Fatalf("unexpected catalog request capture: %+v", captured[0])
	}
	if captured[1].Path != bookingPublicReservationsPath || !strings.Contains(captured[1].RawQuery, "user_id=u-1") {
		t.Fatalf("unexpected reservations request capture: %+v", captured[1])
	}
	if captured[2].Path != bookingPublicIntentParsePath || captured[2].Method != http.MethodPost || captured[2].Idempotency == "" {
		t.Fatalf("unexpected parse request capture: %+v", captured[2])
	}
}

func TestBookingMCPToolIntentParseValidationAndErrorPassthrough(t *testing.T) {
	t.Run("validation error on missing required fields", func(t *testing.T) {
		server, err := newBookingMCPServer(bookingMCPServeConfig{
			ThirdPartyID:   "partner-a",
			Token:          "token-inline",
			SecurityKeys:   "unused-security-keys.json",
			PublicBaseURL:  "http://booking.test",
			RuntimePath:    filepath.Join(t.TempDir(), "runtime.json"),
			AutoStart:      false,
			StartupTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("newBookingMCPServer failed: %v", err)
		}

		result, rpcErr := server.handleToolsCall(context.Background(), toolCallParams(bookingMCPToolIntentParse, map[string]any{
			"content": "only content",
		}))
		if rpcErr != nil {
			t.Fatalf("expected tool error result, got rpc error %+v", rpcErr)
		}
		output := decodeBookingMCPToolOutput(t, result)
		if output.OK {
			t.Fatalf("expected non-ok output for validation error: %+v", output)
		}
		if output.Code != publicAPIErrorCodeValidationError || output.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("expected validation error output, got %+v", output)
		}
		if !result.IsError {
			t.Fatalf("expected isError=true for validation output")
		}
	})

	t.Run("public api error passthrough retains code and request_id", func(t *testing.T) {
		withBookingMCPMockTransport(t, func(r *http.Request) (*http.Response, error) {
			return newPublicEnvelopeResponse(http.StatusUnauthorized, publicAPIEnvelope{
				Status:    publicAPIStatusError,
				Code:      publicAPIErrorCodeTokenNotFound,
				Message:   "token not found for third-party-id",
				RequestID: "req-401",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
			}), nil
		})
		server, err := newBookingMCPServer(bookingMCPServeConfig{
			ThirdPartyID:   "partner-a",
			Token:          "token-inline",
			SecurityKeys:   "unused-security-keys.json",
			PublicBaseURL:  "http://booking.test",
			RuntimePath:    filepath.Join(t.TempDir(), "runtime.json"),
			AutoStart:      false,
			StartupTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("newBookingMCPServer failed: %v", err)
		}

		result, rpcErr := server.handleToolsCall(context.Background(), toolCallParams(bookingMCPToolCatalogQuery, nil))
		if rpcErr != nil {
			t.Fatalf("expected tool error passthrough result, got rpc error %+v", rpcErr)
		}
		output := decodeBookingMCPToolOutput(t, result)
		if output.OK {
			t.Fatalf("expected non-ok output for public api error: %+v", output)
		}
		if output.Code != publicAPIErrorCodeTokenNotFound || output.RequestID != "req-401" || output.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("expected request_id/code/http_status passthrough, got %+v", output)
		}
		if !result.IsError {
			t.Fatalf("expected isError=true for public api error output")
		}
	})
}

func TestBookingMCPAutoStartFlow(t *testing.T) {
	t.Run("auto-start success", func(t *testing.T) {
		var (
			statusCalls int
			startCalled bool
			readyCalled bool
		)
		withBookingMCPRuntimeMocks(
			t,
			func(runtimePath string) (bookingServiceStatusResult, error) {
				statusCalls++
				return bookingServiceStatusResult{
					Mode:    "service_status",
					Status:  "stopped",
					Running: false,
				}, nil
			},
			func(runtimePath string, logPath string, cfg *bookingServiceServeConfig) (bookingServiceStartResult, error) {
				startCalled = true
				return bookingServiceStartResult{
					Mode:   "service_start",
					Status: "started",
					Runtime: &bookingServiceRuntimeInfo{
						PID:           12345,
						PublicAddress: "127.0.0.1:19081",
					},
				}, nil
			},
			func(address string, record webhookTokenRecord, timeout time.Duration) error {
				readyCalled = true
				if strings.TrimSpace(address) != "127.0.0.1:19081" {
					t.Fatalf("unexpected readiness address %q", address)
				}
				if strings.TrimSpace(record.ThirdPartyID) != "partner-a" || strings.TrimSpace(record.Token) != "token-inline" {
					t.Fatalf("unexpected readiness record %+v", record)
				}
				return nil
			},
		)

		server, err := newBookingMCPServer(bookingMCPServeConfig{
			ThirdPartyID:   "partner-a",
			Token:          "token-inline",
			SecurityKeys:   filepath.Join(t.TempDir(), "unused.json"),
			RuntimePath:    filepath.Join(t.TempDir(), "booking_runtime.json"),
			AutoStart:      true,
			StartupTimeout: 200 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("expected auto-start success, got %v", err)
		}
		if server == nil {
			t.Fatalf("expected server instance")
		}
		if !startCalled || !readyCalled || statusCalls == 0 {
			t.Fatalf("expected status/start/ready flow, got statusCalls=%d start=%t ready=%t", statusCalls, startCalled, readyCalled)
		}
	})

	t.Run("auto-start disabled returns actionable error", func(t *testing.T) {
		withBookingMCPRuntimeMocks(
			t,
			func(runtimePath string) (bookingServiceStatusResult, error) {
				return bookingServiceStatusResult{
					Mode:    "service_status",
					Status:  "stopped",
					Running: false,
				}, nil
			},
			func(runtimePath string, logPath string, cfg *bookingServiceServeConfig) (bookingServiceStartResult, error) {
				t.Fatalf("start should not be called when auto-start is disabled")
				return bookingServiceStartResult{}, nil
			},
			func(address string, record webhookTokenRecord, timeout time.Duration) error {
				t.Fatalf("readiness check should not be called when auto-start is disabled")
				return nil
			},
		)

		_, err := newBookingMCPServer(bookingMCPServeConfig{
			ThirdPartyID:   "partner-a",
			Token:          "token-inline",
			SecurityKeys:   filepath.Join(t.TempDir(), "unused.json"),
			RuntimePath:    filepath.Join(t.TempDir(), "booking_runtime.json"),
			AutoStart:      false,
			StartupTimeout: time.Second,
		})
		if err == nil {
			t.Fatalf("expected not running error when auto-start is disabled")
		}
		if !strings.Contains(err.Error(), "booking service is not running") {
			t.Fatalf("expected actionable error message, got %v", err)
		}
	})
}
