package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	if !strings.Contains(err.Error(), "copy conf-example/booking_api_keys.json") {
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
	baseURL := "http://" + runtime.Address

	unauthCatalogResp, err := http.Get(baseURL + bookingPublicCatalogPath)
	if err != nil {
		t.Fatalf("GET catalog without key failed: %v", err)
	}
	if unauthCatalogResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, unauthCatalogResp)
		t.Fatalf("expected catalog unauthorized without API key, got %d body=%s", unauthCatalogResp.StatusCode, body)
	}
	_ = unauthCatalogResp.Body.Close()

	authCatalogReq, _ := http.NewRequest(http.MethodGet, baseURL+bookingPublicCatalogPath, nil)
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
	parseReq, _ := http.NewRequest(http.MethodPost, baseURL+bookingPublicIntentParsePath, strings.NewReader(parseBody))
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
	confirmReq, _ := http.NewRequest(http.MethodPost, baseURL+bookingPublicIntentConfirmPath, strings.NewReader(confirmBody))
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

	reservationsReq, _ := http.NewRequest(http.MethodGet, baseURL+bookingPublicReservationsPath+"?user_id=u-1&status=pending", nil)
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

	adminStatusResp, err := http.Get(baseURL + bookingAdminStatusPath)
	if err != nil {
		t.Fatalf("GET booking admin status without auth failed: %v", err)
	}
	if adminStatusResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, adminStatusResp)
		t.Fatalf("expected booking admin status 401 without basic auth, got %d body=%s", adminStatusResp.StatusCode, body)
	}
	_ = adminStatusResp.Body.Close()

	adminStatusReq, _ := http.NewRequest(http.MethodGet, baseURL+bookingAdminStatusPath, nil)
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

	adminActionReq, _ := http.NewRequest(http.MethodGet, baseURL+bookingAdminProductUpsertPath, nil)
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

	catalogReq, _ := http.NewRequest(http.MethodGet, "http://"+runtime.Address+bookingPublicCatalogPath, nil)
	catalogReq.Header.Set(bookingHeaderAPIKey, "booking-key")
	catalogResp, err := http.DefaultClient.Do(catalogReq)
	if err != nil {
		t.Fatalf("GET catalog failed: %v", err)
	}
	catalogBody := readAllAndClose(t, catalogResp)
	if catalogResp.StatusCode != http.StatusOK {
		t.Fatalf("expected catalog still available when llm config missing, got %d body=%s", catalogResp.StatusCode, catalogBody)
	}

	parseReq, _ := http.NewRequest(http.MethodPost, "http://"+runtime.Address+bookingPublicIntentParsePath, strings.NewReader(`{"user_id":"u-1","content":"test"}`))
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
	baseURL := "http://" + runtime.Address

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
