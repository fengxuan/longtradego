package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type bookingAgentRoundTripper func(*http.Request) (*http.Response, error)

func (fn bookingAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func withBookingAgentMockTransport(t *testing.T, fn bookingAgentRoundTripper) {
	t.Helper()
	oldFactory := bookingAgentHTTPClientFactory
	bookingAgentHTTPClientFactory = func(timeout time.Duration) *http.Client {
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		return &http.Client{
			Timeout:   timeout,
			Transport: fn,
		}
	}
	t.Cleanup(func() {
		bookingAgentHTTPClientFactory = oldFactory
	})
}

func newPublicEnvelopeResponse(status int, envelope publicAPIEnvelope) *http.Response {
	raw := writePublicAPIEnvelope(nil, status, envelope)
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(bytes.NewReader(raw)),
	}
}

func TestBookingCommandFlow(t *testing.T) {
	tempDir := t.TempDir()
	catalogPath := filepath.Join(tempDir, "booking_catalog.json")
	reservationsPath := filepath.Join(tempDir, "booking_reservations.json")
	app := NewAppContext()

	exec := func(args ...string) bookingCommandResult {
		t.Helper()
		cmd := newBookingCommand(app)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("booking command %v failed: %v", args, err)
		}
		_, _, resultAny := app.ExecutionSnapshot()
		result, ok := resultAny.(bookingCommandResult)
		if !ok {
			t.Fatalf("expected bookingCommandResult for %v, got %T", args, resultAny)
		}
		return result
	}

	exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"product", "add",
		"--id", "p-1",
		"--name", "Product One",
		"--enabled=true",
	)

	exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"slot", "add",
		"--id", "slot-1",
		"--product-id", "p-1",
		"--start", "2026-05-03T10:00:00+08:00",
		"--end", "2026-05-03T11:00:00+08:00",
		"--capacity", "1",
		"--enabled=true",
	)

	createResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "create",
		"--product-id", "p-1",
		"--slot-id", "slot-1",
		"--user-id", "u-1",
		"--party-size", "1",
		"--contact-name", "Alice",
		"--contact-phone", "13800138000",
		"--member", "Alice",
	)
	if createResult.Reservation == nil {
		t.Fatalf("expected reservation result after create, got %+v", createResult)
	}
	if createResult.Reservation.Status != bookingReservationStatusPending {
		t.Fatalf("expected pending reservation after create, got %+v", createResult.Reservation)
	}

	reservationID := createResult.Reservation.ID
	confirmResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "confirm",
		reservationID,
		"--note", "confirmed",
	)
	if confirmResult.Reservation == nil || confirmResult.Reservation.Status != bookingReservationStatusConfirmed {
		t.Fatalf("expected confirmed reservation result, got %+v", confirmResult)
	}

	listResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "list",
		"--user-id", "u-1",
		"--status", "confirmed",
	)
	if len(listResult.Reservations) != 1 || listResult.Reservations[0].ID != reservationID {
		t.Fatalf("expected confirmed reservation listed, got %+v", listResult.Reservations)
	}

	queryResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"query",
	)
	if queryResult.Query == nil {
		t.Fatalf("expected query result payload, got %+v", queryResult)
	}
	if len(queryResult.Query.Slots) != 0 {
		t.Fatalf("expected query default to hide full slot, got %+v", queryResult.Query.Slots)
	}
}

func TestBookingCommandValidationErrors(t *testing.T) {
	tempDir := t.TempDir()
	catalogPath := filepath.Join(tempDir, "booking_catalog.json")
	reservationsPath := filepath.Join(tempDir, "booking_reservations.json")
	app := NewAppContext()

	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "create",
		"--product-id", "p-1",
		"--slot-id", "slot-1",
		"--user-id", "u-1",
		"--party-size", "1",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected reservation create validation error for missing personnel fields")
	}
}

func TestBookingCommandServiceStatus(t *testing.T) {
	tempDir := t.TempDir()
	runtimePath := filepath.Join(tempDir, "booking_runtime.json")
	app := NewAppContext()

	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"service", "status",
		"--runtime", runtimePath,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("booking service status command failed: %v", err)
	}

	_, _, resultAny := app.ExecutionSnapshot()
	result, ok := resultAny.(bookingCommandResult)
	if !ok {
		t.Fatalf("expected bookingCommandResult for service status, got %T", resultAny)
	}
	if result.ServiceState == nil {
		t.Fatalf("expected service status payload, got %+v", result)
	}
	if result.ServiceState.Status != "stopped" || result.ServiceState.Running {
		t.Fatalf("expected stopped booking service status, got %+v", result.ServiceState)
	}
}

func TestBookingServiceStartRejectsLegacyAPIKeysFlag(t *testing.T) {
	app := NewAppContext()
	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"service", "start",
		"--api-keys", "/tmp/security-b.json",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected legacy --api-keys flag rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unknown flag") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestBookingAgentReserveSuccessAndIdempotency(t *testing.T) {
	tempDir := t.TempDir()
	securityPath := filepath.Join(tempDir, "security_keys.json")
	if err := writeSecurityTokenRecords(securityPath, map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "token-store-a",
			Scopes:       []string{securityScopeBooking},
		},
	}); err != nil {
		t.Fatalf("writeSecurityTokenRecords failed: %v", err)
	}

	type capturedRequest struct {
		path           string
		idempotencyKey string
		body           string
	}
	var (
		mu       sync.Mutex
		requests []capturedRequest
	)
	withBookingAgentMockTransport(t, func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		timestamp := strings.TrimSpace(r.Header.Get(webhookHeaderTimestamp))
		expectedSignature := computeWebhookSignature(
			strings.TrimSpace(r.Header.Get(webhookHeaderThirdPartyID)),
			timestamp,
			"token-store-a",
			body,
		)
		if strings.TrimSpace(r.Header.Get(webhookHeaderSignature)) != expectedSignature {
			t.Fatalf("signature mismatch: got=%s want=%s", strings.TrimSpace(r.Header.Get(webhookHeaderSignature)), expectedSignature)
		}
		mu.Lock()
		requests = append(requests, capturedRequest{
			path:           strings.TrimSpace(r.URL.Path),
			idempotencyKey: strings.TrimSpace(r.Header.Get(publicAPIHeaderIdempotencyKey)),
			body:           string(body),
		})
		mu.Unlock()

		switch r.URL.Path {
		case bookingPublicIntentParsePath:
			return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-parse-ok",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"draft_id": "d-1",
					"draft": map[string]any{
						"id": "d-1",
						"extracted": map[string]any{
							"product_id": "p-1",
							"slot_id":    "slot-1",
							"party_size": 2,
							"personnel": map[string]any{
								"contact_name":  "Alice",
								"contact_phone": "13800138000",
							},
						},
						"missing_fields": []string{},
					},
				},
			}), nil
		case bookingPublicIntentConfirmPath:
			return newPublicEnvelopeResponse(http.StatusCreated, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-confirm-ok",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"draft_id":       "d-1",
					"reservation_id": "r-1",
					"reservation": map[string]any{
						"status": "pending",
					},
				},
			}), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})

	app := NewAppContext()
	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"agent", "reserve",
		"--user-id", "u-1",
		"--content", "我想预定",
		"--third-party-id", "partner-a",
		"--security-keys", securityPath,
		"--url", "http://booking.test",
		"--idempotency-key", "agent-reserve-idem",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("booking agent reserve failed: %v", err)
	}

	_, _, resultAny := app.ExecutionSnapshot()
	result, ok := resultAny.(bookingCommandResult)
	if !ok {
		t.Fatalf("expected bookingCommandResult, got %T", resultAny)
	}
	if result.AgentReserve == nil {
		t.Fatalf("expected agent reserve result payload")
	}
	if result.AgentReserve.Parse.Code != publicAPIErrorCodeOK {
		t.Fatalf("expected parse code ok, got %+v", result.AgentReserve.Parse)
	}
	if result.AgentReserve.Confirm == nil || result.AgentReserve.Confirm.Code != publicAPIErrorCodeOK {
		t.Fatalf("expected confirm code ok, got %+v", result.AgentReserve.Confirm)
	}
	if result.AgentReserve.ReservationID != "r-1" || result.AgentReserve.FinalStatus != "pending" {
		t.Fatalf("expected reservation result, got %+v", result.AgentReserve)
	}
	if result.AgentReserve.IdempotencyParse != "agent-reserve-idem-parse" || result.AgentReserve.IdempotencyConfirm != "agent-reserve-idem-confirm" {
		t.Fatalf("unexpected idempotency keys in result: %+v", result.AgentReserve)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("expected two public requests, got %d", len(requests))
	}
	if requests[0].path != bookingPublicIntentParsePath || requests[0].idempotencyKey != "agent-reserve-idem-parse" {
		t.Fatalf("unexpected parse request capture: %+v", requests[0])
	}
	if requests[1].path != bookingPublicIntentConfirmPath || requests[1].idempotencyKey != "agent-reserve-idem-confirm" {
		t.Fatalf("unexpected confirm request capture: %+v", requests[1])
	}
}

func TestBookingAgentReserveFailsWhenServiceNotRunning(t *testing.T) {
	app := NewAppContext()
	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"agent", "reserve",
		"--user-id", "u-1",
		"--content", "我想预定",
		"--third-party-id", "partner-a",
		"--token", "token-inline",
		"--runtime", filepath.Join(t.TempDir(), "missing_runtime.json"),
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected booking agent reserve to fail when runtime is missing")
	}
	if !strings.Contains(err.Error(), "booking service start") {
		t.Fatalf("expected startup hint error, got %v", err)
	}
}

func TestBookingAgentReserveMissingFields(t *testing.T) {
	withBookingAgentMockTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == bookingPublicIntentConfirmPath {
			t.Fatalf("confirm should not be called when missing_fields remain")
		}
		if r.URL.Path != bookingPublicIntentParsePath {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
			Status:    publicAPIStatusOK,
			Code:      publicAPIErrorCodeOK,
			RequestID: "req-parse-missing",
			TS:        time.Now().UTC().Format(time.RFC3339Nano),
			Data: map[string]any{
				"draft_id": "d-2",
				"draft": map[string]any{
					"id": "d-2",
					"extracted": map[string]any{
						"product_id": "p-1",
						"party_size": 2,
						"personnel": map[string]any{
							"contact_name": "Alice",
						},
					},
					"missing_fields": []string{"slot_id", "personnel.contact_phone"},
				},
			},
		}), nil
	})

	app := NewAppContext()
	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"agent", "reserve",
		"--user-id", "u-2",
		"--content", "我想预定",
		"--third-party-id", "partner-a",
		"--token", "token-inline",
		"--url", "http://booking.test",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected missing_fields error")
	}
	if !strings.Contains(err.Error(), "missing_fields") || !strings.Contains(err.Error(), "--slot-id") || !strings.Contains(err.Error(), "--contact-phone") {
		t.Fatalf("expected missing fields guidance, got %v", err)
	}
}

func TestBookingAgentReserveMissingFieldsWithOverrides(t *testing.T) {
	withBookingAgentMockTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case bookingPublicIntentParsePath:
			return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-parse-override",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"draft_id": "d-3",
					"draft": map[string]any{
						"id": "d-3",
						"extracted": map[string]any{
							"product_id": "p-1",
							"party_size": 2,
							"personnel": map[string]any{
								"contact_name": "Alice",
							},
						},
						"missing_fields": []string{"slot_id", "personnel.contact_phone"},
					},
				},
			}), nil
		case bookingPublicIntentConfirmPath:
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode confirm payload failed: %v body=%s", err, string(body))
			}
			if strings.TrimSpace(anyString(payload["slot_id"])) != "slot-9" {
				t.Fatalf("expected slot override in confirm payload, got %+v", payload)
			}
			personnel, _ := payload["personnel"].(map[string]any)
			if strings.TrimSpace(anyString(personnel["contact_phone"])) != "13800138001" {
				t.Fatalf("expected contact_phone override in confirm payload, got %+v", payload)
			}
			return newPublicEnvelopeResponse(http.StatusCreated, publicAPIEnvelope{
				Status:    publicAPIStatusOK,
				Code:      publicAPIErrorCodeOK,
				RequestID: "req-confirm-override",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
				Data: map[string]any{
					"draft_id":       "d-3",
					"reservation_id": "r-3",
					"reservation": map[string]any{
						"status": "pending",
					},
				},
			}), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})

	app := NewAppContext()
	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"agent", "reserve",
		"--user-id", "u-3",
		"--content", "我想预定",
		"--third-party-id", "partner-a",
		"--token", "token-inline",
		"--url", "http://booking.test",
		"--slot-id", "slot-9",
		"--contact-phone", "13800138001",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("booking agent reserve with overrides failed: %v", err)
	}
}

func TestBookingAgentReserveTokenResolutionAndErrors(t *testing.T) {
	t.Run("token flag overrides security-keys token", func(t *testing.T) {
		tempDir := t.TempDir()
		securityPath := filepath.Join(tempDir, "security_keys.json")
		if err := writeSecurityTokenRecords(securityPath, map[string]webhookTokenRecord{
			"partner-a": {
				ThirdPartyID: "partner-a",
				Token:        "token-from-store",
				Scopes:       []string{securityScopeBooking},
			},
		}); err != nil {
			t.Fatalf("writeSecurityTokenRecords failed: %v", err)
		}

		withBookingAgentMockTransport(t, func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			ts := strings.TrimSpace(r.Header.Get(webhookHeaderTimestamp))
			sig := strings.TrimSpace(r.Header.Get(webhookHeaderSignature))
			want := computeWebhookSignature("partner-a", ts, "token-inline", body)
			if sig != want {
				t.Fatalf("expected signature built with --token value, got=%s want=%s", sig, want)
			}
			switch r.URL.Path {
			case bookingPublicIntentParsePath:
				return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
					Status:    publicAPIStatusOK,
					Code:      publicAPIErrorCodeOK,
					RequestID: "req-parse-token-inline",
					TS:        time.Now().UTC().Format(time.RFC3339Nano),
					Data: map[string]any{
						"draft_id": "d-4",
						"draft": map[string]any{
							"id": "d-4",
							"extracted": map[string]any{
								"product_id": "p-1",
								"slot_id":    "slot-1",
								"party_size": 1,
								"personnel": map[string]any{
									"contact_name":  "A",
									"contact_phone": "1",
								},
							},
							"missing_fields": []string{},
						},
					},
				}), nil
			case bookingPublicIntentConfirmPath:
				return newPublicEnvelopeResponse(http.StatusCreated, publicAPIEnvelope{
					Status:    publicAPIStatusOK,
					Code:      publicAPIErrorCodeOK,
					RequestID: "req-confirm-token-inline",
					TS:        time.Now().UTC().Format(time.RFC3339Nano),
					Data: map[string]any{
						"draft_id":       "d-4",
						"reservation_id": "r-4",
						"reservation": map[string]any{
							"status": "pending",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
				return nil, nil
			}
		})

		app := NewAppContext()
		cmd := newBookingCommand(app)
		cmd.SetArgs([]string{
			"agent", "reserve",
			"--user-id", "u-4",
			"--content", "预定",
			"--third-party-id", "partner-a",
			"--security-keys", securityPath,
			"--token", "token-inline",
			"--url", "http://booking.test",
		})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("booking agent reserve failed: %v", err)
		}
	})

	t.Run("scope mismatch rejected", func(t *testing.T) {
		tempDir := t.TempDir()
		securityPath := filepath.Join(tempDir, "security_keys.json")
		if err := writeSecurityTokenRecords(securityPath, map[string]webhookTokenRecord{
			"partner-a": {
				ThirdPartyID: "partner-a",
				Token:        "token-webhook-only",
				Scopes:       []string{securityScopeWebhook},
			},
		}); err != nil {
			t.Fatalf("writeSecurityTokenRecords failed: %v", err)
		}

		cmd := newBookingCommand(NewAppContext())
		cmd.SetArgs([]string{
			"agent", "reserve",
			"--user-id", "u-5",
			"--content", "预定",
			"--third-party-id", "partner-a",
			"--security-keys", securityPath,
			"--url", "http://booking.test",
		})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("expected scope mismatch error")
		}
		if !strings.Contains(err.Error(), "token scope not allowed for booking") {
			t.Fatalf("expected scope mismatch message, got %v", err)
		}
	})
}

func TestBookingAgentReserveEnvelopeErrors(t *testing.T) {
	t.Run("parse returns status error", func(t *testing.T) {
		withBookingAgentMockTransport(t, func(r *http.Request) (*http.Response, error) {
			return newPublicEnvelopeResponse(http.StatusUnauthorized, publicAPIEnvelope{
				Status:    publicAPIStatusError,
				Code:      publicAPIErrorCodeTokenNotFound,
				Message:   "token not found for third-party-id",
				RequestID: "req-parse-401",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
			}), nil
		})

		cmd := newBookingCommand(NewAppContext())
		cmd.SetArgs([]string{
			"agent", "reserve",
			"--user-id", "u-6",
			"--content", "预定",
			"--third-party-id", "partner-a",
			"--token", "token-inline",
			"--url", "http://booking.test",
		})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("expected parse envelope error")
		}
		if !strings.Contains(err.Error(), publicAPIErrorCodeTokenNotFound) || !strings.Contains(err.Error(), "req-parse-401") {
			t.Fatalf("expected machine code + request_id in error, got %v", err)
		}
	})

	t.Run("confirm returns status error", func(t *testing.T) {
		withBookingAgentMockTransport(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == bookingPublicIntentParsePath {
				return newPublicEnvelopeResponse(http.StatusOK, publicAPIEnvelope{
					Status:    publicAPIStatusOK,
					Code:      publicAPIErrorCodeOK,
					RequestID: "req-parse-ok-7",
					TS:        time.Now().UTC().Format(time.RFC3339Nano),
					Data: map[string]any{
						"draft_id": "d-7",
						"draft": map[string]any{
							"id": "d-7",
							"extracted": map[string]any{
								"product_id": "p-1",
								"slot_id":    "slot-1",
								"party_size": 1,
								"personnel": map[string]any{
									"contact_name":  "A",
									"contact_phone": "1",
								},
							},
							"missing_fields": []string{},
						},
					},
				}), nil
			}
			return newPublicEnvelopeResponse(http.StatusConflict, publicAPIEnvelope{
				Status:    publicAPIStatusError,
				Code:      publicAPIErrorCodeConflict,
				Message:   "slot capacity exceeded",
				RequestID: "req-confirm-409",
				TS:        time.Now().UTC().Format(time.RFC3339Nano),
			}), nil
		})

		cmd := newBookingCommand(NewAppContext())
		cmd.SetArgs([]string{
			"agent", "reserve",
			"--user-id", "u-7",
			"--content", "预定",
			"--third-party-id", "partner-a",
			"--token", "token-inline",
			"--url", "http://booking.test",
		})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("expected confirm envelope error")
		}
		if !strings.Contains(err.Error(), publicAPIErrorCodeConflict) || !strings.Contains(err.Error(), "req-confirm-409") {
			t.Fatalf("expected machine code + request_id in error, got %v", err)
		}
	})
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
