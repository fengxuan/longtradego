package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadBookingAPIKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "booking_api_keys.json")
	if err := os.WriteFile(cfgPath, []byte(`{"keys":[" key-1 ","key-2"],"api_keys":["key-2","key-3"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys config failed: %v", err)
	}
	keys, err := loadBookingAPIKeys(cfgPath)
	if err != nil {
		t.Fatalf("loadBookingAPIKeys failed: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 deduplicated keys, got %v", keys)
	}

	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte(`{"keys":[]}`), 0o644); err != nil {
		t.Fatalf("write empty booking api keys config failed: %v", err)
	}
	if _, err := loadBookingAPIKeys(emptyPath); err == nil {
		t.Fatalf("expected empty booking api keys validation error")
	}
}

func TestBookingPublicAuthSignedHeadersAndLegacyFallback(t *testing.T) {
	now := time.Now().UTC()
	thirdPartyID := "partner-a"
	token := "booking-shared-token"
	body := []byte(`{"user_id":"u-1","content":"hello"}`)
	timestampText := strconv.FormatInt(now.Unix(), 10)
	signature := computeWebhookSignature(thirdPartyID, timestampText, token, body)

	controller := &bookingServiceController{
		apiKeys: map[string]struct{}{
			token: {},
		},
		tokenRecords: map[string]webhookTokenRecord{
			thirdPartyID: {
				ThirdPartyID: thirdPartyID,
				Token:        token,
				Scopes:       []string{securityScopeBooking},
			},
			"booking-blocked": {
				ThirdPartyID: "booking-blocked",
				Token:        "token-2",
				Scopes:       []string{securityScopeWebhook},
			},
		},
	}

	signedReq := httptest.NewRequest(http.MethodPost, bookingPublicIntentParsePath, bytes.NewReader(body))
	signedReq.Header.Set(webhookHeaderThirdPartyID, thirdPartyID)
	signedReq.Header.Set(webhookHeaderTimestamp, timestampText)
	signedReq.Header.Set(webhookHeaderSignature, signature)
	if !controller.isAPIKeyAuthorized(signedReq) {
		t.Fatalf("expected signed booking request authorized")
	}
	restoredBody, err := io.ReadAll(signedReq.Body)
	if err != nil {
		t.Fatalf("read restored body failed: %v", err)
	}
	if string(restoredBody) != string(body) {
		t.Fatalf("expected request body restored after signature check, got %s", string(restoredBody))
	}

	legacyReq := httptest.NewRequest(http.MethodGet, bookingPublicCatalogPath, nil)
	legacyReq.Header.Set(bookingHeaderAPIKey, token)
	if !controller.isAPIKeyAuthorized(legacyReq) {
		t.Fatalf("expected legacy X-Booking-API-Key authorized")
	}

	priorityReq := httptest.NewRequest(http.MethodPost, bookingPublicIntentParsePath, bytes.NewReader(body))
	priorityReq.Header.Set(bookingHeaderAPIKey, token)
	priorityReq.Header.Set(webhookHeaderThirdPartyID, thirdPartyID)
	priorityReq.Header.Set(webhookHeaderTimestamp, timestampText)
	priorityReq.Header.Set(webhookHeaderSignature, "bad-signature")
	if controller.isAPIKeyAuthorized(priorityReq) {
		t.Fatalf("expected signed header validation takes priority over legacy api key")
	}

	scopeReq := httptest.NewRequest(http.MethodPost, bookingPublicIntentParsePath, bytes.NewReader(body))
	scopeReq.Header.Set(webhookHeaderThirdPartyID, "booking-blocked")
	scopeReq.Header.Set(webhookHeaderTimestamp, timestampText)
	scopeReq.Header.Set(webhookHeaderSignature, computeWebhookSignature("booking-blocked", timestampText, "token-2", body))
	if controller.isAPIKeyAuthorized(scopeReq) {
		t.Fatalf("expected webhook-only scope token rejected by booking endpoint")
	}
}

func TestBookingPublicAuthFailureReasons(t *testing.T) {
	now := time.Unix(1777797488, 0).UTC()
	thirdPartyID := "partner-a"
	token := "booking-shared-token"
	blockedThirdPartyID := "partner-blocked"
	blockedToken := "blocked-token"
	body := []byte(`{"user_id":"u-1","content":"hello"}`)
	timestampText := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	signature := computeWebhookSignature(thirdPartyID, timestampText, token, body)
	validTimestampText := strconv.FormatInt(now.Unix(), 10)
	validSignature := computeWebhookSignature(thirdPartyID, validTimestampText, token, body)
	blockedSignature := computeWebhookSignature(blockedThirdPartyID, validTimestampText, blockedToken, body)

	controller := &bookingServiceController{
		tokenRecords: map[string]webhookTokenRecord{
			thirdPartyID: {
				ThirdPartyID: thirdPartyID,
				Token:        token,
				Scopes:       []string{securityScopeBooking},
			},
			blockedThirdPartyID: {
				ThirdPartyID: blockedThirdPartyID,
				Token:        blockedToken,
				Scopes:       []string{securityScopeWebhook},
			},
		},
		nowFn: func() time.Time { return now },
	}
	publicMux := http.NewServeMux()
	adminMux := http.NewServeMux()
	controller.registerHandlers(publicMux, adminMux)
	server := httptest.NewServer(publicMux)
	defer server.Close()

	noKeyResp, err := http.Get(server.URL + bookingPublicCatalogPath)
	if err != nil {
		t.Fatalf("GET without key failed: %v", err)
	}
	noKeyBody := readAllAndClose(t, noKeyResp)
	if noKeyResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without key, got %d body=%s", noKeyResp.StatusCode, noKeyBody)
	}
	if !strings.Contains(noKeyBody, "missing X-Booking-API-Key") {
		t.Fatalf("expected missing legacy key reason, got %s", noKeyBody)
	}

	signedParse := func(headers map[string]string) (int, string) {
		t.Helper()
		parseReq, _ := http.NewRequest(http.MethodPost, server.URL+bookingPublicIntentParsePath, bytes.NewReader(body))
		parseReq.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			parseReq.Header.Set(key, value)
		}
		parseResp, err := http.DefaultClient.Do(parseReq)
		if err != nil {
			t.Fatalf("signed parse request failed: %v", err)
		}
		parseBody := readAllAndClose(t, parseResp)
		return parseResp.StatusCode, parseBody
	}

	outOfWindowStatus, outOfWindowBody := signedParse(map[string]string{
		webhookHeaderThirdPartyID: thirdPartyID,
		webhookHeaderTimestamp:    timestampText,
		webhookHeaderSignature:    signature,
	})
	if outOfWindowStatus != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for out-of-window timestamp, got %d body=%s", outOfWindowStatus, outOfWindowBody)
	}
	if !strings.Contains(strings.ToLower(outOfWindowBody), "timestamp outside allowed window") {
		t.Fatalf("expected timestamp reason, got %s", outOfWindowBody)
	}

	missingHeadersStatus, missingHeadersBody := signedParse(map[string]string{
		webhookHeaderThirdPartyID: thirdPartyID,
	})
	if missingHeadersStatus != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for missing signed headers, got %d body=%s", missingHeadersStatus, missingHeadersBody)
	}
	if !strings.Contains(strings.ToLower(missingHeadersBody), "missing required headers") {
		t.Fatalf("expected missing headers reason, got %s", missingHeadersBody)
	}

	invalidTimestampStatus, invalidTimestampBody := signedParse(map[string]string{
		webhookHeaderThirdPartyID: thirdPartyID,
		webhookHeaderTimestamp:    "bad-ts",
		webhookHeaderSignature:    validSignature,
	})
	if invalidTimestampStatus != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for invalid timestamp, got %d body=%s", invalidTimestampStatus, invalidTimestampBody)
	}
	if !strings.Contains(strings.ToLower(invalidTimestampBody), "invalid timestamp") {
		t.Fatalf("expected invalid timestamp reason, got %s", invalidTimestampBody)
	}

	tokenNotFoundStatus, tokenNotFoundBody := signedParse(map[string]string{
		webhookHeaderThirdPartyID: "missing-partner",
		webhookHeaderTimestamp:    validTimestampText,
		webhookHeaderSignature:    computeWebhookSignature("missing-partner", validTimestampText, token, body),
	})
	if tokenNotFoundStatus != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for missing token record, got %d body=%s", tokenNotFoundStatus, tokenNotFoundBody)
	}
	if !strings.Contains(strings.ToLower(tokenNotFoundBody), "token not found for third-party-id") {
		t.Fatalf("expected token not found reason, got %s", tokenNotFoundBody)
	}

	scopeMismatchStatus, scopeMismatchBody := signedParse(map[string]string{
		webhookHeaderThirdPartyID: blockedThirdPartyID,
		webhookHeaderTimestamp:    validTimestampText,
		webhookHeaderSignature:    blockedSignature,
	})
	if scopeMismatchStatus != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for scope mismatch, got %d body=%s", scopeMismatchStatus, scopeMismatchBody)
	}
	if !strings.Contains(strings.ToLower(scopeMismatchBody), "token scope not allowed for booking") {
		t.Fatalf("expected scope mismatch reason, got %s", scopeMismatchBody)
	}

	signatureFailureStatus, signatureFailureBody := signedParse(map[string]string{
		webhookHeaderThirdPartyID: thirdPartyID,
		webhookHeaderTimestamp:    validTimestampText,
		webhookHeaderSignature:    "not-a-real-signature",
	})
	if signatureFailureStatus != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for signature mismatch, got %d body=%s", signatureFailureStatus, signatureFailureBody)
	}
	if !strings.Contains(strings.ToLower(signatureFailureBody), "signature verification failed") {
		t.Fatalf("expected signature mismatch reason, got %s", signatureFailureBody)
	}
}

func TestBookingSignedAuthTokenHotReloadWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	securityKeysPath := filepath.Join(dir, "security_keys.json")
	llmConfigPath := filepath.Join(dir, "booking_llm.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	initialToken := "token-v1"
	rotatedToken := "token-v2"
	thirdPartyID := "partner-a"

	if err := writeSecurityTokenRecords(securityKeysPath, map[string]webhookTokenRecord{
		thirdPartyID: {
			ThirdPartyID: thirdPartyID,
			Token:        initialToken,
			Scopes:       []string{securityScopeBooking},
			CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("write initial security keys failed: %v", err)
	}
	if err := os.WriteFile(llmConfigPath, []byte(`{"api_key":"sk-test","base_url":"https://api.openai.com"}`), 0o644); err != nil {
		t.Fatalf("write booking llm config failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	oldParser := bookingParseIntentWithLLM
	bookingParseIntentWithLLM = func(ctx context.Context, req bookingIntentParseRequest, parseCtx bookingIntentParseContext) (bookingIntentParseExtracted, error) {
		return bookingIntentParseExtracted{
			ProductID:  "p-1",
			PartySize:  2,
			Personnel:  bookingReservationPersonnel{ContactName: "Alice", ContactPhone: "13800138000"},
			Confidence: 0.88,
		}, nil
	}
	defer func() {
		bookingParseIntentWithLLM = oldParser
	}()

	handle, err := startBookingHTTPService(context.Background(), &bookingServiceServeConfig{
		Addr:             "127.0.0.1:0",
		AdminAddr:        "127.0.0.1:0",
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      securityKeysPath,
		LLMConfigPath:    llmConfigPath,
		AdminAuthPath:    adminAuthPath,
		MaxPortFallback:  0,
	}, io.Discard)
	if err != nil {
		t.Fatalf("startBookingHTTPService failed: %v", err)
	}
	defer func() {
		_ = handle.Close()
	}()

	runtime, exists, err := readBookingRuntimeState(runtimePath)
	if err != nil || !exists || runtime == nil {
		t.Fatalf("read runtime failed: %v runtime=%+v", err, runtime)
	}
	publicBaseURL := "http://" + bookingRuntimePublicAddress(runtime)

	callSignedParseStatus := func(token string) (int, string) {
		t.Helper()
		body := `{"user_id":"u-1","channel":"chat","content":"hot reload test"}`
		timestampText := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		signature := computeWebhookSignature(thirdPartyID, timestampText, token, []byte(body))
		req, _ := http.NewRequest(http.MethodPost, publicBaseURL+bookingPublicIntentParsePath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhookHeaderThirdPartyID, thirdPartyID)
		req.Header.Set(webhookHeaderTimestamp, timestampText)
		req.Header.Set(webhookHeaderSignature, signature)
		resp, reqErr := http.DefaultClient.Do(req)
		if reqErr != nil {
			t.Fatalf("signed parse request failed: %v", reqErr)
		}
		respBody := readAllAndClose(t, resp)
		return resp.StatusCode, respBody
	}

	initialStatus, initialBody := callSignedParseStatus(initialToken)
	if initialStatus != http.StatusOK {
		t.Fatalf("expected initial token authorized, got %d body=%s", initialStatus, initialBody)
	}

	if err := writeSecurityTokenRecords(securityKeysPath, map[string]webhookTokenRecord{
		thirdPartyID: {
			ThirdPartyID: thirdPartyID,
			Token:        rotatedToken,
			Scopes:       []string{securityScopeBooking},
			CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("write rotated security keys failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		lastStatus, lastBody = callSignedParseStatus(rotatedToken)
		if lastStatus == http.StatusOK {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lastStatus != http.StatusOK {
		t.Fatalf("expected rotated token eventually authorized without restart, got %d body=%s", lastStatus, lastBody)
	}

	oldStatus, oldBody := callSignedParseStatus(initialToken)
	if oldStatus != http.StatusUnauthorized {
		t.Fatalf("expected old token rejected after rotation, got %d body=%s", oldStatus, oldBody)
	}
	if !strings.Contains(strings.ToLower(oldBody), "signature verification failed") {
		t.Fatalf("expected signature failure for old token, got %s", oldBody)
	}
}

func TestLoadBookingLLMConfig(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "booking_llm.json")
	if err := os.WriteFile(validPath, []byte(`{"api_key":"sk-test","base_url":"https://api.openai.com","model":"gpt-4.1-mini"}`), 0o644); err != nil {
		t.Fatalf("write valid booking llm config failed: %v", err)
	}
	cfg, err := loadBookingLLMConfig(validPath)
	if err != nil {
		t.Fatalf("loadBookingLLMConfig valid failed: %v", err)
	}
	if cfg.APIKey != "sk-test" || cfg.BaseURL != "https://api.openai.com" || cfg.Model != "gpt-4.1-mini" {
		t.Fatalf("unexpected llm config loaded: %+v", cfg)
	}

	defaultModelPath := filepath.Join(dir, "booking_llm_default_model.json")
	if err := os.WriteFile(defaultModelPath, []byte(`{"api_key":"sk-test","base_url":"https://api.openai.com"}`), 0o644); err != nil {
		t.Fatalf("write default model booking llm config failed: %v", err)
	}
	defaultModelCfg, err := loadBookingLLMConfig(defaultModelPath)
	if err != nil {
		t.Fatalf("loadBookingLLMConfig default model failed: %v", err)
	}
	if defaultModelCfg.Model != "gpt-4.1-mini" {
		t.Fatalf("expected default model gpt-4.1-mini, got %+v", defaultModelCfg)
	}

	if _, err := loadBookingLLMConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatalf("expected missing booking llm config error")
	}

	invalidJSONPath := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalidJSONPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid booking llm config failed: %v", err)
	}
	if _, err := loadBookingLLMConfig(invalidJSONPath); err == nil {
		t.Fatalf("expected invalid JSON booking llm config error")
	}

	emptyAPIKeyPath := filepath.Join(dir, "empty_api_key.json")
	if err := os.WriteFile(emptyAPIKeyPath, []byte(`{"api_key":"","base_url":"https://api.openai.com"}`), 0o644); err != nil {
		t.Fatalf("write empty api_key booking llm config failed: %v", err)
	}
	if _, err := loadBookingLLMConfig(emptyAPIKeyPath); err == nil {
		t.Fatalf("expected empty api_key validation error")
	}

	emptyBaseURLPath := filepath.Join(dir, "empty_base_url.json")
	if err := os.WriteFile(emptyBaseURLPath, []byte(`{"api_key":"sk-test","base_url":""}`), 0o644); err != nil {
		t.Fatalf("write empty base_url booking llm config failed: %v", err)
	}
	if _, err := loadBookingLLMConfig(emptyBaseURLPath); err == nil {
		t.Fatalf("expected empty base_url validation error")
	}
}

func TestNormalizeBookingOpenAIChatCompletionsURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "root base url",
			baseURL: "https://api.openai.com",
			want:    "https://api.openai.com/v1/chat/completions",
		},
		{
			name:    "v1 base url",
			baseURL: "https://api.siliconflow.cn/v1",
			want:    "https://api.siliconflow.cn/v1/chat/completions",
		},
		{
			name:    "prefixed v1 path",
			baseURL: "https://example.com/openai/v1",
			want:    "https://example.com/openai/v1/chat/completions",
		},
		{
			name:    "full endpoint provided",
			baseURL: "https://example.com/openai/v1/chat/completions",
			want:    "https://example.com/openai/v1/chat/completions",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeBookingOpenAIChatCompletionsURL(tc.baseURL)
			if err != nil {
				t.Fatalf("normalizeBookingOpenAIChatCompletionsURL(%q) failed: %v", tc.baseURL, err)
			}
			if got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}

	if _, err := normalizeBookingOpenAIChatCompletionsURL("://bad"); err == nil {
		t.Fatalf("expected invalid base URL error")
	}
}

func TestParseBookingIntentExtractedFromModelJSONCoercesMembers(t *testing.T) {
	raw := `{
  "product_id": "p-1",
  "slot_id": "slot-1",
  "party_size": 2,
  "personnel": {
    "contact_name": "Alice",
    "contact_phone": 13800138000,
    "members": [1, "Bob", 3.5, true, "", null]
  },
  "confidence": "0.92"
}`
	parsed, err := parseBookingIntentExtractedFromModelJSON(raw)
	if err != nil {
		t.Fatalf("parseBookingIntentExtractedFromModelJSON failed: %v", err)
	}
	if parsed.PartySize != 2 {
		t.Fatalf("expected party_size=2, got %+v", parsed)
	}
	if parsed.Personnel.ContactPhone != "13800138000" {
		t.Fatalf("expected contact_phone coerced to string, got %+v", parsed.Personnel)
	}
	wantMembers := []string{"1", "Bob", "3.5", "true"}
	if len(parsed.Personnel.Members) != len(wantMembers) {
		t.Fatalf("expected %d members, got %v", len(wantMembers), parsed.Personnel.Members)
	}
	for i := range wantMembers {
		if parsed.Personnel.Members[i] != wantMembers[i] {
			t.Fatalf("member[%d] expected %q got %q", i, wantMembers[i], parsed.Personnel.Members[i])
		}
	}
	if parsed.Confidence != 0.92 {
		t.Fatalf("expected confidence=0.92 got %v", parsed.Confidence)
	}
}

func TestBookingServiceServeConfigDefaultsIncludeLLMConfig(t *testing.T) {
	cfg := newBookingServiceServeConfig()
	if strings.TrimSpace(cfg.LLMConfigPath) != defaultBookingLLMConfigPath() {
		t.Fatalf("expected default llm config path %s, got %s", defaultBookingLLMConfigPath(), cfg.LLMConfigPath)
	}
}

func TestBuildBookingServiceServeArgsIncludesLLMConfig(t *testing.T) {
	cfg := &bookingServiceServeConfig{
		Addr:             "127.0.0.1:18081",
		AdminAddr:        "127.0.0.1:18082",
		RuntimePath:      "/tmp/runtime.json",
		CatalogPath:      "/tmp/catalog.json",
		ReservationsPath: "/tmp/reservations.json",
		DraftsPath:       "/tmp/drafts.json",
		APIKeysPath:      "/tmp/keys.json",
		LLMConfigPath:    "/tmp/booking_llm.json",
		AdminAuthPath:    "/tmp/admin_auth.json",
	}
	args := buildBookingServiceServeArgs(cfg)
	if !strings.Contains(strings.Join(args, " "), "--llm-config /tmp/booking_llm.json") {
		t.Fatalf("expected serve args to include llm-config, got %v", args)
	}
}

func TestBookingServiceStartStopWithFallback(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	logPath := filepath.Join(dir, "booking_service.log")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["test-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen base addr failed: %v", err)
	}
	defer baseListener.Close()
	baseAddr := baseListener.Addr().String()

	oldSpawn := bookingSpawnBackgroundProcess
	oldVerify := bookingVerifyBackgroundStart
	defer func() {
		bookingSpawnBackgroundProcess = oldSpawn
		bookingVerifyBackgroundStart = oldVerify
	}()

	bookingSpawnBackgroundProcess = func(cfg *bookingServiceServeConfig, logPath string) (int, []string, error) {
		return 54321, []string{"booking", "service", "serve", "--addr", cfg.Addr}, nil
	}
	bookingVerifyBackgroundStart = func(cfg *bookingServiceServeConfig, pid int, authCfg daemonAdminAuthConfig) error {
		return nil
	}

	cfg := &bookingServiceServeConfig{
		Addr:             baseAddr,
		AdminAddr:        "127.0.0.1:" + pickFreePort(t),
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		AdminAuthPath:    adminAuthPath,
		MaxPortFallback:  1,
	}

	result, err := startBookingServiceInBackground(runtimePath, logPath, cfg)
	if err != nil {
		t.Fatalf("startBookingServiceInBackground failed: %v", err)
	}
	if result.Status != "started" || result.Runtime == nil {
		t.Fatalf("expected started runtime result, got %+v", result)
	}
	if result.Runtime.PID != 54321 {
		t.Fatalf("expected runtime pid 54321, got %+v", result.Runtime)
	}
	if result.Runtime.Address == baseAddr {
		t.Fatalf("expected fallback address because base addr occupied, got %s", result.Runtime.Address)
	}
	if result.Runtime.OwnerSessionID != "" || result.Runtime.OwnerStartToken != "" || result.Runtime.OwnerDaemonPID != 0 || result.Runtime.OwnerClaimedAt != "" {
		t.Fatalf("expected default background start without owner metadata, got %+v", result.Runtime)
	}

	status, runtime, err := bookingRuntimeStatus(runtimePath)
	if err != nil {
		t.Fatalf("bookingRuntimeStatus failed: %v", err)
	}
	if status != "stale" || runtime == nil {
		t.Fatalf("expected stale runtime for fake pid, got status=%s runtime=%+v", status, runtime)
	}

	stopResult, stopErr := stopBookingService(runtimePath, defaultBookingServiceStopTimeout)
	if stopErr != nil {
		t.Fatalf("stopBookingService failed: %v", stopErr)
	}
	if stopResult.Status != "stale_removed" {
		t.Fatalf("expected stale_removed stop status, got %+v", stopResult)
	}
}

func TestBookingServiceStartWithOwnerClaimWritesRuntimeOwner(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	logPath := filepath.Join(dir, "booking_service.log")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["test-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	oldSpawn := bookingSpawnBackgroundProcess
	oldVerify := bookingVerifyBackgroundStart
	defer func() {
		bookingSpawnBackgroundProcess = oldSpawn
		bookingVerifyBackgroundStart = oldVerify
	}()
	bookingSpawnBackgroundProcess = func(cfg *bookingServiceServeConfig, logPath string) (int, []string, error) {
		return 54322, []string{"booking", "service", "serve", "--addr", cfg.Addr}, nil
	}
	bookingVerifyBackgroundStart = func(cfg *bookingServiceServeConfig, pid int, authCfg daemonAdminAuthConfig) error {
		return nil
	}

	cfg := &bookingServiceServeConfig{
		Addr:             "127.0.0.1:" + pickFreePort(t),
		AdminAddr:        "127.0.0.1:" + pickFreePort(t),
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		AdminAuthPath:    adminAuthPath,
		MaxPortFallback:  0,
	}
	claim := &bookingServiceStartOwnerClaim{
		SessionID:  "daemon-session-booking",
		DaemonPID:  13579,
		ClaimedAt:  time.Now().Add(-2 * time.Second).UTC(),
		StartToken: "booking-owner-token-x",
	}

	result, err := startBookingServiceInBackgroundWithOwner(runtimePath, logPath, cfg, claim)
	if err != nil {
		t.Fatalf("startBookingServiceInBackgroundWithOwner failed: %v", err)
	}
	if result.Status != "started" || result.Runtime == nil {
		t.Fatalf("expected started runtime result, got %+v", result)
	}
	if result.Runtime.OwnerSessionID != claim.SessionID || result.Runtime.OwnerStartToken != claim.StartToken || result.Runtime.OwnerDaemonPID != claim.DaemonPID {
		t.Fatalf("expected owner metadata in start result, got %+v", result.Runtime)
	}
	if strings.TrimSpace(result.Runtime.OwnerClaimedAt) == "" {
		t.Fatalf("expected owner_claimed_at in start result, got %+v", result.Runtime)
	}

	runtime, exists, err := readBookingRuntimeState(runtimePath)
	if err != nil {
		t.Fatalf("readBookingRuntimeState failed: %v", err)
	}
	if !exists || runtime == nil {
		t.Fatalf("expected booking runtime file after owner start")
	}
	if runtime.OwnerSessionID != claim.SessionID || runtime.OwnerStartToken != claim.StartToken || runtime.OwnerDaemonPID != claim.DaemonPID {
		t.Fatalf("expected owner metadata persisted to runtime file, got %+v", runtime)
	}
	if strings.TrimSpace(runtime.OwnerClaimedAt) == "" {
		t.Fatalf("expected persisted owner_claimed_at, got %+v", runtime)
	}
}

func TestBookingServiceStartFailsWhenAPIKeysConfigMissing(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	logPath := filepath.Join(dir, "booking_service.log")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	cfg := &bookingServiceServeConfig{
		Addr:             "127.0.0.1:18081",
		AdminAddr:        "127.0.0.1:18082",
		RuntimePath:      runtimePath,
		CatalogPath:      filepath.Join(dir, "booking_catalog.json"),
		ReservationsPath: filepath.Join(dir, "booking_reservations.json"),
		DraftsPath:       filepath.Join(dir, "booking_intake_drafts.json"),
		APIKeysPath:      apiKeysPath,
		LLMConfigPath:    filepath.Join(dir, "booking_llm.json"),
		AdminAuthPath:    adminAuthPath,
		MaxPortFallback:  0,
	}

	_, err := startBookingServiceInBackground(runtimePath, logPath, cfg)
	if err == nil {
		t.Fatalf("expected booking start failure when api keys config missing")
	}
	if !strings.Contains(err.Error(), apiKeysPath) {
		t.Fatalf("expected error includes missing api keys path %s, got %v", apiKeysPath, err)
	}
	if !strings.Contains(err.Error(), "copy conf-example/security_keys.json") {
		t.Fatalf("expected error includes copy hint, got %v", err)
	}

	rawLog, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read booking service log failed: %v", readErr)
	}
	logText := string(rawLog)
	if !strings.Contains(logText, "booking service start failed:") {
		t.Fatalf("expected booking service start failure log line, got %s", logText)
	}
	if !strings.Contains(logText, apiKeysPath) {
		t.Fatalf("expected booking service log includes missing api keys path %s, got %s", apiKeysPath, logText)
	}
}

func TestStartBookingHTTPServiceAuthAndIntentFlow(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	llmConfigPath := filepath.Join(dir, "booking_llm.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["booking-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	if err := os.WriteFile(llmConfigPath, []byte(`{"api_key":"sk-test","base_url":"https://api.openai.com"}`), 0o644); err != nil {
		t.Fatalf("write booking llm config failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	service := newBookingService(catalogPath, reservationsPath)
	trueValue := true
	capacity := 2
	if _, err := service.UpsertProduct(bookingProductUpsertInput{ID: "p-1", Name: "Morning", Enabled: &trueValue}); err != nil {
		t.Fatalf("seed product failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-1",
		ProductID: "p-1",
		StartAt:   "2026-05-03T10:00:00+08:00",
		EndAt:     "2026-05-03T11:00:00+08:00",
		Capacity:  &capacity,
		Enabled:   &trueValue,
	}); err != nil {
		t.Fatalf("seed slot failed: %v", err)
	}

	cfg := &bookingServiceServeConfig{
		Addr:             "127.0.0.1:0",
		AdminAddr:        "127.0.0.1:0",
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		LLMConfigPath:    llmConfigPath,
		AdminAuthPath:    adminAuthPath,
		MaxPortFallback:  0,
	}

	oldParser := bookingParseIntentWithLLM
	bookingParseIntentWithLLM = func(ctx context.Context, req bookingIntentParseRequest, parseCtx bookingIntentParseContext) (bookingIntentParseExtracted, error) {
		return bookingIntentParseExtracted{
			ProductID:  "p-1",
			PartySize:  2,
			Personnel:  bookingReservationPersonnel{ContactName: "Alice", ContactPhone: "13800138000"},
			Confidence: 0.88,
		}, nil
	}
	defer func() {
		bookingParseIntentWithLLM = oldParser
	}()

	handle, err := startBookingHTTPService(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("startBookingHTTPService failed: %v", err)
	}
	defer func() {
		_ = handle.Close()
	}()

	runtime, exists, err := readBookingRuntimeState(runtimePath)
	if err != nil {
		t.Fatalf("readBookingRuntimeState failed: %v", err)
	}
	if !exists || runtime == nil {
		t.Fatalf("expected runtime after service start")
	}
	publicBaseURL := "http://" + bookingRuntimePublicAddress(runtime)
	adminBaseURL := "http://" + bookingRuntimeAdminAddress(runtime)

	unauthCatalogResp, err := http.Get(publicBaseURL + bookingPublicCatalogPath)
	if err != nil {
		t.Fatalf("GET catalog without key failed: %v", err)
	}
	if unauthCatalogResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, unauthCatalogResp)
		t.Fatalf("expected catalog unauthorized without API key, got %d body=%s", unauthCatalogResp.StatusCode, body)
	}
	_ = unauthCatalogResp.Body.Close()

	authCatalogReq, _ := http.NewRequest(http.MethodGet, publicBaseURL+bookingPublicCatalogPath, nil)
	authCatalogReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	authCatalogResp, err := http.DefaultClient.Do(authCatalogReq)
	if err != nil {
		t.Fatalf("GET catalog with key failed: %v", err)
	}
	if authCatalogResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, authCatalogResp)
		t.Fatalf("expected catalog 200 with API key, got %d body=%s", authCatalogResp.StatusCode, body)
	}
	_ = authCatalogResp.Body.Close()

	parseBody := `{"user_id":"u-1","content":"我想预定明天两个人"}`
	parseReq, _ := http.NewRequest(http.MethodPost, publicBaseURL+bookingPublicIntentParsePath, strings.NewReader(parseBody))
	parseReq.Header.Set("Content-Type", "application/json")
	parseReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	parseResp, err := http.DefaultClient.Do(parseReq)
	if err != nil {
		t.Fatalf("POST intents/parse failed: %v", err)
	}
	parseRespBody := readAllAndClose(t, parseResp)
	if parseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected parse 200, got %d body=%s", parseResp.StatusCode, parseRespBody)
	}
	var parsePayload map[string]any
	if err := json.Unmarshal([]byte(parseRespBody), &parsePayload); err != nil {
		t.Fatalf("decode parse payload failed: %v body=%s", err, parseRespBody)
	}
	draftObj, ok := parsePayload["draft"].(map[string]any)
	if !ok {
		t.Fatalf("expected draft payload in parse response: %s", parseRespBody)
	}
	draftID, _ := draftObj["id"].(string)
	if strings.TrimSpace(draftID) == "" {
		t.Fatalf("expected draft id in parse response: %s", parseRespBody)
	}

	confirmBody := `{"draft_id":"` + draftID + `","slot_id":"slot-1"}`
	confirmReq, _ := http.NewRequest(http.MethodPost, publicBaseURL+bookingPublicIntentConfirmPath, strings.NewReader(confirmBody))
	confirmReq.Header.Set("Content-Type", "application/json")
	confirmReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	confirmResp, err := http.DefaultClient.Do(confirmReq)
	if err != nil {
		t.Fatalf("POST intents/confirm failed: %v", err)
	}
	confirmRespBody := readAllAndClose(t, confirmResp)
	if confirmResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected confirm 201, got %d body=%s", confirmResp.StatusCode, confirmRespBody)
	}
	if !strings.Contains(confirmRespBody, `"status": "pending"`) {
		t.Fatalf("expected pending reservation in confirm response: %s", confirmRespBody)
	}

	reservationsReq, _ := http.NewRequest(http.MethodGet, publicBaseURL+bookingPublicReservationsPath+"?user_id=u-1&status=pending", nil)
	reservationsReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	reservationsResp, err := http.DefaultClient.Do(reservationsReq)
	if err != nil {
		t.Fatalf("GET reservations failed: %v", err)
	}
	reservationsBody := readAllAndClose(t, reservationsResp)
	if reservationsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected reservations 200, got %d body=%s", reservationsResp.StatusCode, reservationsBody)
	}
	if !strings.Contains(reservationsBody, "r-") {
		t.Fatalf("expected reservation id in reservation list: %s", reservationsBody)
	}

	adminHomeResp, err := http.Get(adminBaseURL + bookingAdminHomePath)
	if err != nil {
		t.Fatalf("GET booking admin home without auth failed: %v", err)
	}
	if adminHomeResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, adminHomeResp)
		t.Fatalf("expected booking admin home 401 without basic auth, got %d body=%s", adminHomeResp.StatusCode, body)
	}
	_ = adminHomeResp.Body.Close()

	adminStatusResp, err := http.Get(adminBaseURL + bookingAdminStatusPath)
	if err != nil {
		t.Fatalf("GET booking admin status without auth failed: %v", err)
	}
	if adminStatusResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, adminStatusResp)
		t.Fatalf("expected booking admin status 401 without basic auth, got %d body=%s", adminStatusResp.StatusCode, body)
	}
	_ = adminStatusResp.Body.Close()

	adminStatusReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminStatusPath, nil)
	adminStatusReq.SetBasicAuth("admin", "secret")
	adminStatusRespOK, err := http.DefaultClient.Do(adminStatusReq)
	if err != nil {
		t.Fatalf("GET booking admin status with auth failed: %v", err)
	}
	if adminStatusRespOK.StatusCode != http.StatusOK {
		body := readAllAndClose(t, adminStatusRespOK)
		t.Fatalf("expected booking admin status 200 with auth, got %d body=%s", adminStatusRespOK.StatusCode, body)
	}
	_ = adminStatusRespOK.Body.Close()

	adminHomeReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminHomePath, nil)
	adminHomeReq.SetBasicAuth("admin", "secret")
	adminHomeRespOK, err := http.DefaultClient.Do(adminHomeReq)
	if err != nil {
		t.Fatalf("GET booking admin home with auth failed: %v", err)
	}
	if adminHomeRespOK.StatusCode != http.StatusOK {
		body := readAllAndClose(t, adminHomeRespOK)
		t.Fatalf("expected booking admin home 200 with auth, got %d body=%s", adminHomeRespOK.StatusCode, body)
	}
	adminHomeContentType := adminHomeRespOK.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(adminHomeContentType)), "text/html") {
		t.Fatalf("expected booking admin home text/html content-type, got %q", adminHomeContentType)
	}
	adminHomeBody := readAllAndClose(t, adminHomeRespOK)
	for _, snippet := range []string{
		"Booking Admin Home",
		"Refresh Page",
		bookingAdminReservationConfirmPath,
		bookingAdminReservationRejectPath,
		bookingAdminReservationCancelPath,
		bookingAdminStopPath,
	} {
		if !strings.Contains(adminHomeBody, snippet) {
			t.Fatalf("expected booking admin home contains %q", snippet)
		}
	}

	publicAdminReq, _ := http.NewRequest(http.MethodGet, publicBaseURL+bookingAdminStatusPath, nil)
	publicAdminReq.SetBasicAuth("admin", "secret")
	publicAdminResp, err := http.DefaultClient.Do(publicAdminReq)
	if err != nil {
		t.Fatalf("GET public booking admin endpoint failed: %v", err)
	}
	if publicAdminResp.StatusCode != http.StatusNotFound {
		body := readAllAndClose(t, publicAdminResp)
		t.Fatalf("expected public booking admin endpoint 404, got %d body=%s", publicAdminResp.StatusCode, body)
	}
	_ = publicAdminResp.Body.Close()

	publicAdminHomeReq, _ := http.NewRequest(http.MethodGet, publicBaseURL+bookingAdminHomePath, nil)
	publicAdminHomeReq.SetBasicAuth("admin", "secret")
	publicAdminHomeResp, err := http.DefaultClient.Do(publicAdminHomeReq)
	if err != nil {
		t.Fatalf("GET public booking admin home endpoint failed: %v", err)
	}
	if publicAdminHomeResp.StatusCode != http.StatusNotFound {
		body := readAllAndClose(t, publicAdminHomeResp)
		t.Fatalf("expected public booking admin home endpoint 404, got %d body=%s", publicAdminHomeResp.StatusCode, body)
	}
	_ = publicAdminHomeResp.Body.Close()

	adminPublicReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingPublicCatalogPath, nil)
	adminPublicReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	adminPublicResp, err := http.DefaultClient.Do(adminPublicReq)
	if err != nil {
		t.Fatalf("GET admin listener booking public endpoint failed: %v", err)
	}
	if adminPublicResp.StatusCode != http.StatusNotFound {
		body := readAllAndClose(t, adminPublicResp)
		t.Fatalf("expected booking public endpoint on admin listener 404, got %d body=%s", adminPublicResp.StatusCode, body)
	}
	_ = adminPublicResp.Body.Close()

	adminActionReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminProductUpsertPath, nil)
	adminActionReq.SetBasicAuth("admin", "secret")
	adminActionResp, err := http.DefaultClient.Do(adminActionReq)
	if err != nil {
		t.Fatalf("GET booking admin action endpoint failed: %v", err)
	}
	if adminActionResp.StatusCode != http.StatusMethodNotAllowed {
		body := readAllAndClose(t, adminActionResp)
		t.Fatalf("expected booking admin action GET 405, got %d body=%s", adminActionResp.StatusCode, body)
	}
	_ = adminActionResp.Body.Close()
}

func TestBookingAdminHomeSlashRedirectKeepsQuery(t *testing.T) {
	dir := t.TempDir()
	controller := &bookingServiceController{
		service: newBookingService(
			filepath.Join(dir, "booking_catalog.json"),
			filepath.Join(dir, "booking_reservations.json"),
		),
		authConfig: daemonAdminAuthConfig{
			Username: "admin",
			Password: "secret",
		},
		nowFn: time.Now,
	}
	publicMux := http.NewServeMux()
	adminMux := http.NewServeMux()
	controller.registerHandlers(publicMux, adminMux)
	server := httptest.NewServer(adminMux)
	defer server.Close()

	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+bookingAdminHomeSlash+"?from=test", nil)
	if err != nil {
		t.Fatalf("build GET /admin/ request failed: %v", err)
	}
	req.SetBasicAuth("admin", "secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/?from=test failed: %v", err)
	}
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("expected /admin/ redirect 308, got %d", resp.StatusCode)
	}
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location != bookingAdminHomePath+"?from=test" {
		t.Fatalf("expected redirect location %q, got %q", bookingAdminHomePath+"?from=test", location)
	}
	_ = resp.Body.Close()
}

func TestBookingAdminStopEndpointTriggersShutdownOnce(t *testing.T) {
	dir := t.TempDir()
	triggerCh := make(chan string, 2)
	controller := &bookingServiceController{
		service: newBookingService(
			filepath.Join(dir, "booking_catalog.json"),
			filepath.Join(dir, "booking_reservations.json"),
		),
		authConfig: daemonAdminAuthConfig{
			Username: "admin",
			Password: "secret",
		},
		nowFn: time.Now,
		requestShutdown: func(source string) {
			triggerCh <- source
		},
	}
	publicMux := http.NewServeMux()
	adminMux := http.NewServeMux()
	controller.registerHandlers(publicMux, adminMux)
	server := httptest.NewServer(adminMux)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+bookingAdminStopPath, nil)
		if err != nil {
			t.Fatalf("build POST /admin/booking/stop failed: %v", err)
		}
		req.SetBasicAuth("admin", "secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /admin/booking/stop failed: %v", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected POST /admin/booking/stop 202, got %d", resp.StatusCode)
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

	getReq, err := http.NewRequest(http.MethodGet, server.URL+bookingAdminStopPath, nil)
	if err != nil {
		t.Fatalf("build GET /admin/booking/stop failed: %v", err)
	}
	getReq.SetBasicAuth("admin", "secret")
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatalf("GET /admin/booking/stop failed: %v", err)
	}
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected GET /admin/booking/stop 405, got %d", getResp.StatusCode)
	}
	_ = getResp.Body.Close()
}

func TestStartBookingHTTPServiceAdminAuthSupportsPasswordHash(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	llmConfigPath := filepath.Join(dir, "booking_llm.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["booking-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	if err := os.WriteFile(llmConfigPath, []byte(`{"api_key":"sk-test","base_url":"https://api.openai.com"}`), 0o644); err != nil {
		t.Fatalf("write booking llm config failed: %v", err)
	}
	hash, err := hashDaemonAdminPassword("hash-secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password_hash":"`+hash+`"}`), 0o644); err != nil {
		t.Fatalf("write admin auth hash failed: %v", err)
	}

	handle, err := startBookingHTTPService(context.Background(), &bookingServiceServeConfig{
		Addr:             "127.0.0.1:0",
		AdminAddr:        "127.0.0.1:0",
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		LLMConfigPath:    llmConfigPath,
		AdminAuthPath:    adminAuthPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("startBookingHTTPService failed: %v", err)
	}
	defer func() {
		_ = handle.Close()
	}()

	runtime, exists, err := readBookingRuntimeState(runtimePath)
	if err != nil {
		t.Fatalf("readBookingRuntimeState failed: %v", err)
	}
	if !exists || runtime == nil {
		t.Fatalf("expected runtime after service start")
	}
	adminBaseURL := "http://" + bookingRuntimeAdminAddress(runtime)

	okReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminStatusPath, nil)
	okReq.SetBasicAuth("admin", "hash-secret")
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatalf("GET booking admin status with hash auth failed: %v", err)
	}
	if okResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, okResp)
		t.Fatalf("expected booking admin status 200 with hash auth, got %d body=%s", okResp.StatusCode, body)
	}
	_ = okResp.Body.Close()

	badReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminStatusPath, nil)
	badReq.SetBasicAuth("admin", "wrong-secret")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("GET booking admin status with wrong hash auth failed: %v", err)
	}
	if badResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, badResp)
		t.Fatalf("expected booking admin status 401 with wrong hash auth, got %d body=%s", badResp.StatusCode, body)
	}
	_ = badResp.Body.Close()
}

func TestStartBookingHTTPServiceReloadsAdminAuthConfigWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["booking-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	oldHash, err := hashDaemonAdminPassword("old-secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword old failed: %v", err)
	}
	if err := writeDaemonAdminAuthConfig(adminAuthPath, daemonAdminAuthConfig{
		Username:     "admin",
		PasswordHash: oldHash,
	}); err != nil {
		t.Fatalf("write initial booking admin auth failed: %v", err)
	}

	handle, err := startBookingHTTPService(context.Background(), &bookingServiceServeConfig{
		Addr:             "127.0.0.1:0",
		AdminAddr:        "127.0.0.1:0",
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		AdminAuthPath:    adminAuthPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("startBookingHTTPService failed: %v", err)
	}
	defer func() {
		_ = handle.Close()
	}()

	runtime, exists, err := readBookingRuntimeState(runtimePath)
	if err != nil {
		t.Fatalf("readBookingRuntimeState failed: %v", err)
	}
	if !exists || runtime == nil {
		t.Fatalf("expected runtime after service start")
	}
	adminBaseURL := "http://" + bookingRuntimeAdminAddress(runtime)

	oldReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminStatusPath, nil)
	oldReq.SetBasicAuth("admin", "old-secret")
	oldResp, err := http.DefaultClient.Do(oldReq)
	if err != nil {
		t.Fatalf("GET booking admin status with old password failed: %v", err)
	}
	if oldResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, oldResp)
		t.Fatalf("expected old password 200 before reload, got %d body=%s", oldResp.StatusCode, body)
	}
	_ = oldResp.Body.Close()

	newHash, err := hashDaemonAdminPassword("new-secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword new failed: %v", err)
	}
	if err := writeDaemonAdminAuthConfig(adminAuthPath, daemonAdminAuthConfig{
		Username:     "admin",
		PasswordHash: newHash,
	}); err != nil {
		t.Fatalf("write updated booking admin auth failed: %v", err)
	}

	oldAfterReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminStatusPath, nil)
	oldAfterReq.SetBasicAuth("admin", "old-secret")
	oldAfterResp, err := http.DefaultClient.Do(oldAfterReq)
	if err != nil {
		t.Fatalf("GET booking admin status old password after reload failed: %v", err)
	}
	if oldAfterResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, oldAfterResp)
		t.Fatalf("expected old password 401 after reload, got %d body=%s", oldAfterResp.StatusCode, body)
	}
	_ = oldAfterResp.Body.Close()

	newReq, _ := http.NewRequest(http.MethodGet, adminBaseURL+bookingAdminStatusPath, nil)
	newReq.SetBasicAuth("admin", "new-secret")
	newResp, err := http.DefaultClient.Do(newReq)
	if err != nil {
		t.Fatalf("GET booking admin status with new password failed: %v", err)
	}
	if newResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, newResp)
		t.Fatalf("expected new password 200 after reload, got %d body=%s", newResp.StatusCode, body)
	}
	_ = newResp.Body.Close()
}

func TestBookingIntentParseLLMUnavailable(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	llmConfigPath := filepath.Join(dir, "booking_llm_missing.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")
	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["booking-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	trueValue := true
	capacity := 2
	service := newBookingService(catalogPath, reservationsPath)
	if _, err := service.UpsertProduct(bookingProductUpsertInput{ID: "p-1", Name: "Morning", Enabled: &trueValue}); err != nil {
		t.Fatalf("seed product failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{ID: "slot-1", ProductID: "p-1", StartAt: "2026-05-03T10:00:00+08:00", EndAt: "2026-05-03T11:00:00+08:00", Capacity: &capacity, Enabled: &trueValue}); err != nil {
		t.Fatalf("seed slot failed: %v", err)
	}

	handle, err := startBookingHTTPService(context.Background(), &bookingServiceServeConfig{
		Addr:             "127.0.0.1:0",
		AdminAddr:        "127.0.0.1:0",
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		LLMConfigPath:    llmConfigPath,
		AdminAuthPath:    adminAuthPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("startBookingHTTPService failed: %v", err)
	}
	defer func() {
		_ = handle.Close()
	}()

	runtime, _, err := readBookingRuntimeState(runtimePath)
	if err != nil || runtime == nil {
		t.Fatalf("read runtime failed: %v runtime=%+v", err, runtime)
	}

	publicBaseURL := "http://" + bookingRuntimePublicAddress(runtime)
	catalogReq, _ := http.NewRequest(http.MethodGet, publicBaseURL+bookingPublicCatalogPath, nil)
	catalogReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	catalogResp, err := http.DefaultClient.Do(catalogReq)
	if err != nil {
		t.Fatalf("GET catalog failed: %v", err)
	}
	catalogBody := readAllAndClose(t, catalogResp)
	if catalogResp.StatusCode != http.StatusOK {
		t.Fatalf("expected catalog still available when llm config missing, got %d body=%s", catalogResp.StatusCode, catalogBody)
	}

	parseReq, _ := http.NewRequest(http.MethodPost, publicBaseURL+bookingPublicIntentParsePath, strings.NewReader(`{"user_id":"u-1","content":"test"}`))
	parseReq.Header.Set("Content-Type", "application/json")
	parseReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	parseResp, err := http.DefaultClient.Do(parseReq)
	if err != nil {
		t.Fatalf("POST intents/parse failed: %v", err)
	}
	if parseResp.StatusCode != http.StatusServiceUnavailable {
		body := readAllAndClose(t, parseResp)
		t.Fatalf("expected parse 503 when llm unavailable, got %d body=%s", parseResp.StatusCode, body)
	}
	parseBody := readAllAndClose(t, parseResp)
	if !strings.Contains(strings.ToLower(parseBody), "booking llm") {
		t.Fatalf("expected parse failure body to mention booking llm config, got: %s", parseBody)
	}
}

func TestBookingServiceStatusAlreadyRunning(t *testing.T) {
	runtimePath := filepath.Join(t.TempDir(), "booking_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)
	if err := writeBookingRuntimeState(runtimePath, bookingServiceRuntimeInfo{PID: os.Getpid(), Address: "127.0.0.1:18081", StartedAt: nowText, UpdatedAt: nowText}); err != nil {
		t.Fatalf("write runtime failed: %v", err)
	}

	cfg := newBookingServiceServeConfig()
	cfg.RuntimePath = runtimePath
	cfg.APIKeysPath = filepath.Join(t.TempDir(), "keys.json")
	cfg.AdminAuthPath = filepath.Join(t.TempDir(), "auth.json")
	cfg.CatalogPath = filepath.Join(t.TempDir(), "catalog.json")
	cfg.ReservationsPath = filepath.Join(t.TempDir(), "reservations.json")
	cfg.DraftsPath = filepath.Join(t.TempDir(), "drafts.json")
	if err := os.WriteFile(cfg.APIKeysPath, []byte(`{"keys":["k"]}`), 0o644); err != nil {
		t.Fatalf("write api keys failed: %v", err)
	}
	if err := os.WriteFile(cfg.AdminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	result, err := startBookingServiceInBackground(runtimePath, filepath.Join(t.TempDir(), "booking.log"), cfg)
	if err != nil {
		t.Fatalf("startBookingServiceInBackground already_running failed: %v", err)
	}
	if result.Status != "already_running" || result.Runtime == nil {
		t.Fatalf("expected already_running result, got %+v", result)
	}
}

func TestBookingIntentParseAutoContinueDraft(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "booking_runtime.json")
	catalogPath := filepath.Join(dir, "booking_catalog.json")
	reservationsPath := filepath.Join(dir, "booking_reservations.json")
	draftsPath := filepath.Join(dir, "booking_intake_drafts.json")
	apiKeysPath := filepath.Join(dir, "booking_api_keys.json")
	llmConfigPath := filepath.Join(dir, "booking_llm.json")
	adminAuthPath := filepath.Join(dir, "admin_auth.json")

	if err := os.WriteFile(apiKeysPath, []byte(`{"keys":["booking-key"]}`), 0o644); err != nil {
		t.Fatalf("write booking api keys failed: %v", err)
	}
	if err := os.WriteFile(llmConfigPath, []byte(`{"api_key":"sk-test","base_url":"https://api.openai.com"}`), 0o644); err != nil {
		t.Fatalf("write booking llm config failed: %v", err)
	}
	if err := os.WriteFile(adminAuthPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write admin auth failed: %v", err)
	}

	service := newBookingService(catalogPath, reservationsPath)
	trueValue := true
	capacity := 3
	if _, err := service.UpsertProduct(bookingProductUpsertInput{ID: "p-1", Name: "Morning", Enabled: &trueValue}); err != nil {
		t.Fatalf("seed product failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-1",
		ProductID: "p-1",
		StartAt:   "2026-05-03T10:00:00+08:00",
		EndAt:     "2026-05-03T11:00:00+08:00",
		Capacity:  &capacity,
		Enabled:   &trueValue,
	}); err != nil {
		t.Fatalf("seed slot failed: %v", err)
	}

	oldParser := bookingParseIntentWithLLM
	bookingParseIntentWithLLM = func(ctx context.Context, req bookingIntentParseRequest, parseCtx bookingIntentParseContext) (bookingIntentParseExtracted, error) {
		switch {
		case strings.Contains(req.Content, "first"):
			return bookingIntentParseExtracted{
				ProductID:  "p-1",
				SlotID:     "slot-1",
				PartySize:  2,
				Confidence: 0.72,
			}, nil
		case strings.Contains(req.Content, "contact"):
			return bookingIntentParseExtracted{
				Personnel: bookingReservationPersonnel{
					ContactName:  "Alice",
					ContactPhone: "13800138000",
				},
				Confidence: 0.65,
			}, nil
		default:
			return bookingIntentParseExtracted{Confidence: 0.60}, nil
		}
	}
	defer func() {
		bookingParseIntentWithLLM = oldParser
	}()

	handle, err := startBookingHTTPService(context.Background(), &bookingServiceServeConfig{
		Addr:             "127.0.0.1:0",
		AdminAddr:        "127.0.0.1:0",
		RuntimePath:      runtimePath,
		CatalogPath:      catalogPath,
		ReservationsPath: reservationsPath,
		DraftsPath:       draftsPath,
		APIKeysPath:      apiKeysPath,
		LLMConfigPath:    llmConfigPath,
		AdminAuthPath:    adminAuthPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("startBookingHTTPService failed: %v", err)
	}
	defer func() {
		_ = handle.Close()
	}()

	runtime, _, err := readBookingRuntimeState(runtimePath)
	if err != nil || runtime == nil {
		t.Fatalf("read runtime failed: %v runtime=%+v", err, runtime)
	}
	baseURL := "http://" + bookingRuntimePublicAddress(runtime)

	parseOnce := func(body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, baseURL+bookingPublicIntentParsePath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(bookingHeaderAPIKey, "booking-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST intents/parse failed: %v", err)
		}
		respBody := readAllAndClose(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected parse 200, got %d body=%s", resp.StatusCode, respBody)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(respBody), &payload); err != nil {
			t.Fatalf("decode parse payload failed: %v body=%s", err, respBody)
		}
		return payload
	}

	first := parseOnce(`{"user_id":"u-1","channel":"chat","content":"first request"}`)
	if first["action"] != bookingIntentParseActionCreated {
		t.Fatalf("expected action=created for first parse, got %+v", first)
	}
	draftID, _ := first["draft_id"].(string)
	if strings.TrimSpace(draftID) == "" {
		t.Fatalf("expected draft_id in first parse response: %+v", first)
	}

	second := parseOnce(`{"user_id":"u-1","channel":"chat","content":"contact Alice 13800138000"}`)
	if second["action"] != bookingIntentParseActionContinued {
		t.Fatalf("expected action=continued for follow-up parse, got %+v", second)
	}
	secondDraftID, _ := second["draft_id"].(string)
	if secondDraftID != draftID {
		t.Fatalf("expected same draft_id on continuation, first=%s second=%s", draftID, secondDraftID)
	}
	if override, ok := second["override_applied"].(bool); !ok || override {
		t.Fatalf("expected override_applied=false for missing-field fill, got %+v", second["override_applied"])
	}
	updatedFields, ok := second["updated_fields"].([]any)
	if !ok {
		t.Fatalf("expected updated_fields array, got %+v", second["updated_fields"])
	}
	updatedTexts := make([]string, 0, len(updatedFields))
	for _, item := range updatedFields {
		if text, ok := item.(string); ok {
			updatedTexts = append(updatedTexts, text)
		}
	}
	if !containsAllStrings(updatedTexts, []string{"personnel.contact_name", "personnel.contact_phone"}) {
		t.Fatalf("expected contact fields updated, got %v", updatedTexts)
	}
	draftObj, ok := second["draft"].(map[string]any)
	if !ok {
		t.Fatalf("expected draft object in second response: %+v", second)
	}
	missingFields, _ := draftObj["missing_fields"].([]any)
	for _, item := range missingFields {
		if text, ok := item.(string); ok && (text == "personnel.contact_name" || text == "personnel.contact_phone") {
			t.Fatalf("expected contact fields removed from missing_fields, got %+v", missingFields)
		}
	}

	third := parseOnce(`{"user_id":"u-1","channel":"chat","content":"thanks"}`)
	if third["action"] != bookingIntentParseActionNoChange {
		t.Fatalf("expected action=continued_no_change for no-op parse, got %+v", third)
	}
}

func TestFindContinuableDraftIndexPolicy(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	state := bookingIntakeDraftState{
		Drafts: []bookingIntakeDraft{
			{
				ID:        "d-email-newer",
				UserID:    "u-1",
				Channel:   "email",
				Status:    bookingDraftStatusDraft,
				UpdatedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
			},
			{
				ID:        "d-chat-older",
				UserID:    "u-1",
				Channel:   "chat",
				Status:    bookingDraftStatusDraft,
				UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			},
			{
				ID:        "d-chat-stale",
				UserID:    "u-1",
				Channel:   "chat",
				Status:    bookingDraftStatusDraft,
				UpdatedAt: now.Add(-30 * time.Hour).Format(time.RFC3339Nano),
			},
		},
	}

	idx, reason := findContinuableDraftIndex(state, "u-1", "chat", now)
	if idx < 0 || state.Drafts[idx].ID != "d-chat-older" || reason != "matched_user_channel" {
		t.Fatalf("expected channel-priority match d-chat-older, got idx=%d reason=%s", idx, reason)
	}

	idx, reason = findContinuableDraftIndex(state, "u-1", "sms", now)
	if idx < 0 || state.Drafts[idx].ID != "d-email-newer" || reason != "matched_user_fallback" {
		t.Fatalf("expected fallback-user match d-email-newer, got idx=%d reason=%s", idx, reason)
	}

	idx, reason = findContinuableDraftIndex(state, "u-1", "chat", now.Add(40*time.Hour))
	if idx >= 0 || reason != "not_found" {
		t.Fatalf("expected stale drafts not match, got idx=%d reason=%s", idx, reason)
	}
}

func TestMergeBookingIntentForContinuationOverrideThreshold(t *testing.T) {
	base := bookingIntentParseExtracted{
		ProductID: "p-old",
		SlotID:    "slot-1",
		PartySize: 2,
		Personnel: bookingReservationPersonnel{
			ContactName:  "Alice",
			ContactPhone: "13800138000",
		},
		Confidence: 0.70,
	}

	lowConfidence := bookingIntentParseExtracted{
		ProductID:  "p-new",
		Confidence: 0.79,
	}
	mergedLow, overrideLow := mergeBookingIntentForContinuation(base, lowConfidence)
	if overrideLow {
		t.Fatalf("expected no override when confidence threshold not met, got %+v", mergedLow)
	}
	if mergedLow.ProductID != "p-old" {
		t.Fatalf("expected product_id unchanged without override, got %+v", mergedLow)
	}

	highConfidence := bookingIntentParseExtracted{
		ProductID:  "p-new",
		Confidence: 0.82,
	}
	mergedHigh, overrideHigh := mergeBookingIntentForContinuation(base, highConfidence)
	if !overrideHigh {
		t.Fatalf("expected override when threshold met, got %+v", mergedHigh)
	}
	if mergedHigh.ProductID != "p-new" {
		t.Fatalf("expected product_id overridden, got %+v", mergedHigh)
	}

	fillMissing := bookingIntentParseExtracted{
		Personnel: bookingReservationPersonnel{
			ContactName:  "Bob",
			ContactPhone: "13900139000",
		},
		Confidence: 0.50,
	}
	baseMissing := bookingIntentParseExtracted{
		ProductID:  "p-1",
		Confidence: 0.60,
	}
	mergedFill, overrideFill := mergeBookingIntentForContinuation(baseMissing, fillMissing)
	if overrideFill {
		t.Fatalf("expected missing-field fill without override, got %+v", mergedFill)
	}
	if mergedFill.Personnel.ContactName != "Bob" || mergedFill.Personnel.ContactPhone != "13900139000" {
		t.Fatalf("expected missing contact fields filled, got %+v", mergedFill.Personnel)
	}
}

func containsAllStrings(haystack []string, need []string) bool {
	seen := make(map[string]struct{}, len(haystack))
	for _, item := range haystack {
		seen[strings.TrimSpace(item)] = struct{}{}
	}
	for _, item := range need {
		if _, ok := seen[strings.TrimSpace(item)]; !ok {
			return false
		}
	}
	return true
}
