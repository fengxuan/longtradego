package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	publicAPIHeaderRequestID      = "X-Request-ID"
	publicAPIHeaderIdempotencyKey = "Idempotency-Key"

	publicAPIStatusOK    = "ok"
	publicAPIStatusError = "error"

	publicAPIErrorCodeOK                       = "ok"
	publicAPIErrorCodeMissingRequiredHeaders   = "missing_required_headers"
	publicAPIErrorCodeInvalidTimestampHeader   = "invalid_timestamp_header"
	publicAPIErrorCodeTimestampOutsideWindow   = "timestamp_outside_allowed_window"
	publicAPIErrorCodeTokenNotFound            = "token_not_found"
	publicAPIErrorCodeTokenScopeNotAllowed     = "token_scope_not_allowed"
	publicAPIErrorCodeSignatureVerification    = "signature_verification_failed"
	publicAPIErrorCodeInvalidJSONBody          = "invalid_json_body"
	publicAPIErrorCodeMethodNotAllowed         = "method_not_allowed"
	publicAPIErrorCodeValidationError          = "validation_error"
	publicAPIErrorCodeConflict                 = "conflict"
	publicAPIErrorCodeInternalError            = "internal_error"
	publicAPIErrorCodeIdempotencyKeyConflict   = "idempotency_key_conflict"
	publicAPIErrorCodeIdempotencyKeyRequired   = "idempotency_key_required"
	publicAPIErrorCodeIdempotencyStoreInternal = "idempotency_store_error"
)

type publicAPIEnvelope struct {
	Status    string `json:"status"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	Details   any    `json:"details,omitempty"`
	RequestID string `json:"request_id"`
	TS        string `json:"ts"`
}

type publicAPIError struct {
	HTTPStatus int
	Code       string
	Message    string
	Details    any
}

func newPublicAPIError(httpStatus int, code string, message string, details any) publicAPIError {
	normalizedCode := strings.TrimSpace(code)
	if normalizedCode == "" {
		normalizedCode = publicAPIErrorCodeInternalError
	}
	normalizedMessage := strings.TrimSpace(message)
	if normalizedMessage == "" {
		normalizedMessage = http.StatusText(httpStatus)
	}
	return publicAPIError{
		HTTPStatus: httpStatus,
		Code:       normalizedCode,
		Message:    normalizedMessage,
		Details:    details,
	}
}

func (e publicAPIError) Error() string {
	return strings.TrimSpace(e.Message)
}

var publicRequestCounter uint64

func nextPublicRequestID(now time.Time) string {
	ts := now.UTC().Format("20060102T150405.000000000Z07:00")
	seq := atomic.AddUint64(&publicRequestCounter, 1)
	randomPart := rand.Uint64()
	return fmt.Sprintf("req-%s-%x-%x", ts, seq, randomPart)
}

func writePublicAPISuccess(w http.ResponseWriter, status int, requestID string, message string, data any) []byte {
	nowText := time.Now().UTC().Format(time.RFC3339Nano)
	normalizedMessage := strings.TrimSpace(message)
	envelope := publicAPIEnvelope{
		Status:    publicAPIStatusOK,
		Code:      publicAPIErrorCodeOK,
		Message:   normalizedMessage,
		Data:      data,
		RequestID: strings.TrimSpace(requestID),
		TS:        nowText,
	}
	return writePublicAPIEnvelope(w, status, envelope)
}

func writePublicAPIError(w http.ResponseWriter, requestID string, apiErr publicAPIError) []byte {
	nowText := time.Now().UTC().Format(time.RFC3339Nano)
	envelope := publicAPIEnvelope{
		Status:    publicAPIStatusError,
		Code:      strings.TrimSpace(apiErr.Code),
		Message:   strings.TrimSpace(apiErr.Message),
		Details:   apiErr.Details,
		RequestID: strings.TrimSpace(requestID),
		TS:        nowText,
	}
	if envelope.Code == "" {
		envelope.Code = publicAPIErrorCodeInternalError
	}
	if envelope.Message == "" {
		envelope.Message = http.StatusText(apiErr.HTTPStatus)
	}
	status := apiErr.HTTPStatus
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	return writePublicAPIEnvelope(w, status, envelope)
}

func marshalPublicAPIEnvelope(status int, envelope publicAPIEnvelope) ([]byte, int, string) {
	normalizedRequestID := strings.TrimSpace(envelope.RequestID)
	if normalizedRequestID == "" {
		normalizedRequestID = nextPublicRequestID(time.Now())
		envelope.RequestID = normalizedRequestID
	}
	normalizedStatus := strings.TrimSpace(envelope.Status)
	if normalizedStatus == "" {
		normalizedStatus = publicAPIStatusError
		envelope.Status = normalizedStatus
	}
	normalizedCode := strings.TrimSpace(envelope.Code)
	if normalizedCode == "" {
		if normalizedStatus == publicAPIStatusOK {
			normalizedCode = publicAPIErrorCodeOK
		} else {
			normalizedCode = publicAPIErrorCodeInternalError
		}
		envelope.Code = normalizedCode
	}
	if strings.TrimSpace(envelope.Message) == "" {
		if normalizedStatus == publicAPIStatusOK {
			envelope.Message = ""
		} else {
			envelope.Message = http.StatusText(status)
		}
	}
	if strings.TrimSpace(envelope.TS) == "" {
		envelope.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if status <= 0 {
		if normalizedStatus == publicAPIStatusOK {
			status = http.StatusOK
		} else {
			status = http.StatusInternalServerError
		}
	}

	encoded, err := marshalJSONIndentNoHTMLEscape(envelope, "", defaultWebhookResponseIndent)
	if err != nil {
		fallback := []byte(`{"status":"error","code":"internal_error","message":"encode response failed","request_id":"` + normalizedRequestID + `","ts":"` + envelope.TS + `"}` + "\n")
		return fallback, status, normalizedRequestID
	}
	return append(encoded, '\n'), status, normalizedRequestID
}

func writePublicAPIEnvelope(w http.ResponseWriter, status int, envelope publicAPIEnvelope) []byte {
	encoded, finalStatus, requestID := marshalPublicAPIEnvelope(status, envelope)
	if w == nil {
		return encoded
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(publicAPIHeaderRequestID, requestID)
	w.WriteHeader(finalStatus)
	_, _ = w.Write(encoded)
	return encoded
}

func readPublicAPIEnvelope(raw []byte) (publicAPIEnvelope, error) {
	var envelope publicAPIEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return publicAPIEnvelope{}, err
	}
	return envelope, nil
}

type publicResponseCapture struct {
	header http.Header
	status int
	body   []byte
}

func newPublicResponseCapture() *publicResponseCapture {
	return &publicResponseCapture{
		header: make(http.Header),
	}
}

func (c *publicResponseCapture) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}

func (c *publicResponseCapture) WriteHeader(status int) {
	if c.status != 0 {
		return
	}
	c.status = status
}

func (c *publicResponseCapture) Write(data []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.body = append(c.body, data...)
	return len(data), nil
}

func (c *publicResponseCapture) FlushTo(w http.ResponseWriter) {
	if w == nil {
		return
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	for key, values := range c.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body)
}

func (c *publicResponseCapture) RequestID() string {
	if c == nil || c.header == nil {
		return ""
	}
	return strings.TrimSpace(c.header.Get(publicAPIHeaderRequestID))
}

func decodePublicJSONBody(raw []byte, payload any) error {
	if payload == nil {
		return newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeValidationError, "request payload target is required", nil)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, "request body is empty", nil)
	}
	if err := json.Unmarshal(raw, payload); err != nil {
		return newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, fmt.Sprintf("invalid json body: %v", err), nil)
	}
	return nil
}

func readPublicBody(r *http.Request, maxBodyBytes int64) ([]byte, error) {
	if r == nil {
		return nil, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeValidationError, "request is required", nil)
	}
	if r.Body == nil {
		return nil, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, "request body is empty", nil)
	}
	body, err := readWebhookBody(r.Body, maxBodyBytes)
	if err != nil {
		message := strings.TrimSpace(err.Error())
		lower := strings.ToLower(message)
		if strings.Contains(lower, "too large") || strings.Contains(lower, "empty") || strings.Contains(lower, "read request body") {
			return nil, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeInvalidJSONBody, message, nil)
		}
		return nil, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeValidationError, message, nil)
	}
	return body, nil
}

func cloneReadCloserFromBytes(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}
