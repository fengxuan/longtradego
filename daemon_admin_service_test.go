package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testDaemonAdminUsername = "admin"
	testDaemonAdminPassword = "test-password"
)

func TestLoadDaemonAdminAuthConfig(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "admin_auth.json")
	if err := os.WriteFile(validPath, []byte(`{"username":"admin","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write valid auth config failed: %v", err)
	}
	cfg, err := loadDaemonAdminAuthConfig(validPath)
	if err != nil {
		t.Fatalf("loadDaemonAdminAuthConfig valid failed: %v", err)
	}
	if cfg.Username != "admin" || cfg.Password != "secret" {
		t.Fatalf("unexpected auth config loaded: %+v", cfg)
	}
	hash, err := hashDaemonAdminPassword("secret-hash", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword failed: %v", err)
	}
	hashPath := filepath.Join(dir, "password_hash.json")
	if err := os.WriteFile(hashPath, []byte(`{"username":"admin","password_hash":"`+hash+`"}`), 0o644); err != nil {
		t.Fatalf("write hash auth config failed: %v", err)
	}
	hashCfg, err := loadDaemonAdminAuthConfig(hashPath)
	if err != nil {
		t.Fatalf("loadDaemonAdminAuthConfig hash failed: %v", err)
	}
	if hashCfg.Username != "admin" || strings.TrimSpace(hashCfg.PasswordHash) == "" {
		t.Fatalf("unexpected hash auth config loaded: %+v", hashCfg)
	}

	if _, err := loadDaemonAdminAuthConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatalf("expected missing auth config error")
	}

	invalidJSONPath := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalidJSONPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid auth config failed: %v", err)
	}
	if _, err := loadDaemonAdminAuthConfig(invalidJSONPath); err == nil {
		t.Fatalf("expected invalid JSON auth config error")
	}

	emptyUsernamePath := filepath.Join(dir, "empty_username.json")
	if err := os.WriteFile(emptyUsernamePath, []byte(`{"username":"","password":"secret"}`), 0o644); err != nil {
		t.Fatalf("write empty username config failed: %v", err)
	}
	if _, err := loadDaemonAdminAuthConfig(emptyUsernamePath); err == nil {
		t.Fatalf("expected empty username auth config error")
	}

	emptyPasswordPath := filepath.Join(dir, "empty_password.json")
	if err := os.WriteFile(emptyPasswordPath, []byte(`{"username":"admin","password":""}`), 0o644); err != nil {
		t.Fatalf("write empty password config failed: %v", err)
	}
	if _, err := loadDaemonAdminAuthConfig(emptyPasswordPath); err == nil {
		t.Fatalf("expected empty password auth config error")
	}

	invalidHashPath := filepath.Join(dir, "invalid_hash.json")
	if err := os.WriteFile(invalidHashPath, []byte(`{"username":"admin","password_hash":"invalid"}`), 0o644); err != nil {
		t.Fatalf("write invalid hash config failed: %v", err)
	}
	if _, err := loadDaemonAdminAuthConfig(invalidHashPath); err == nil {
		t.Fatalf("expected invalid password_hash auth config error")
	}
}

func TestDaemonAdminCredentialsMatchUsesHashPriority(t *testing.T) {
	hash, err := hashDaemonAdminPassword("right-password", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword failed: %v", err)
	}
	cfg := daemonAdminAuthConfig{
		Username:     "admin",
		Password:     "legacy-password",
		PasswordHash: hash,
	}
	if !daemonAdminCredentialsMatch(cfg, "admin", "right-password") {
		t.Fatalf("expected hash credential verification success")
	}
	if daemonAdminCredentialsMatch(cfg, "admin", "legacy-password") {
		t.Fatalf("expected hash priority rejects legacy plaintext password when mismatch")
	}
}

func TestWriteDaemonAdminAuthConfigHashClearsLegacyPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin_auth.json")
	hash, err := hashDaemonAdminPassword("secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword failed: %v", err)
	}
	if err := writeDaemonAdminAuthConfig(path, daemonAdminAuthConfig{
		Username:     "admin",
		Password:     "legacy-plain",
		PasswordHash: hash,
	}); err != nil {
		t.Fatalf("writeDaemonAdminAuthConfig failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written auth config failed: %v", err)
	}
	if strings.Contains(string(raw), `"password":`) {
		t.Fatalf("expected hash writeback to clear legacy password field, got %s", string(raw))
	}

	cfg, err := loadDaemonAdminAuthConfig(path)
	if err != nil {
		t.Fatalf("loadDaemonAdminAuthConfig after write failed: %v", err)
	}
	if cfg.Username != "admin" || strings.TrimSpace(cfg.PasswordHash) == "" {
		t.Fatalf("unexpected config after write: %+v", cfg)
	}
}

func TestDaemonAdminRuntimeStatusAndCleanup(t *testing.T) {
	runtimePath := filepath.Join(t.TempDir(), "admin_runtime.json")

	status, runtime, err := daemonAdminRuntimeStatus(runtimePath)
	if err != nil {
		t.Fatalf("daemonAdminRuntimeStatus stopped failed: %v", err)
	}
	if status != "stopped" || runtime != nil {
		t.Fatalf("expected stopped with nil runtime, got status=%s runtime=%+v", status, runtime)
	}

	now := time.Now().Format(time.RFC3339Nano)
	if err := writeDaemonAdminRuntimeState(runtimePath, daemonAdminRuntimeInfo{
		PID:       os.Getpid(),
		Address:   ":18080",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("writeDaemonAdminRuntimeState running failed: %v", err)
	}
	status, runtime, err = daemonAdminRuntimeStatus(runtimePath)
	if err != nil {
		t.Fatalf("daemonAdminRuntimeStatus running failed: %v", err)
	}
	if status != "running" || runtime == nil || runtime.PID != os.Getpid() {
		t.Fatalf("expected running current pid, got status=%s runtime=%+v", status, runtime)
	}

	if err := writeDaemonAdminRuntimeState(runtimePath, daemonAdminRuntimeInfo{
		PID:       0,
		Address:   ":18080",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("writeDaemonAdminRuntimeState stale failed: %v", err)
	}
	status, runtime, err = daemonAdminRuntimeStatus(runtimePath)
	if err != nil {
		t.Fatalf("daemonAdminRuntimeStatus stale failed: %v", err)
	}
	if status != "stale" || runtime == nil {
		t.Fatalf("expected stale runtime, got status=%s runtime=%+v", status, runtime)
	}

	if err := writeDaemonAdminRuntimeState(runtimePath, daemonAdminRuntimeInfo{
		PID:       os.Getpid(),
		Address:   ":18080",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("writeDaemonAdminRuntimeState cleanup case failed: %v", err)
	}
	removed, err := cleanupDaemonAdminRuntimeStateForCurrentProcess(runtimePath)
	if err != nil {
		t.Fatalf("cleanupDaemonAdminRuntimeStateForCurrentProcess failed: %v", err)
	}
	if !removed {
		t.Fatalf("expected runtime removed for current pid")
	}

	if err := writeDaemonAdminRuntimeState(runtimePath, daemonAdminRuntimeInfo{
		PID:       os.Getpid() + 1,
		Address:   ":18080",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("writeDaemonAdminRuntimeState mismatch cleanup failed: %v", err)
	}
	removed, err = cleanupDaemonAdminRuntimeStateForCurrentProcess(runtimePath)
	if err != nil {
		t.Fatalf("cleanupDaemonAdminRuntimeStateForCurrentProcess mismatch failed: %v", err)
	}
	if removed {
		t.Fatalf("expected runtime kept for mismatched pid")
	}
}

func TestDaemonAdminAddrWithPortOffset(t *testing.T) {
	addr, err := daemonAdminAddrWithPortOffset(":18080", 2)
	if err != nil {
		t.Fatalf("daemonAdminAddrWithPortOffset failed: %v", err)
	}
	if addr != ":18082" {
		t.Fatalf("expected :18082, got %s", addr)
	}

	addr, err = daemonAdminAddrWithPortOffset("127.0.0.1:18080", 3)
	if err != nil {
		t.Fatalf("daemonAdminAddrWithPortOffset host failed: %v", err)
	}
	if addr != "127.0.0.1:18083" {
		t.Fatalf("expected 127.0.0.1:18083, got %s", addr)
	}

	if _, err := daemonAdminAddrWithPortOffset("", 0); err == nil {
		t.Fatalf("expected empty addr failure")
	}
	if _, err := daemonAdminAddrWithPortOffset("bad", 0); err == nil {
		t.Fatalf("expected invalid addr failure")
	}
}

func TestStartDaemonAdminServiceRequiresAuthConfig(t *testing.T) {
	taskManager := newDaemonTaskManagerWithPaths(func(ctx context.Context, commands [][]string) error { return nil }, filepath.Join(t.TempDir(), "daemon_tasks.json"), filepath.Join(t.TempDir(), "daemon_tasks_history.json"))
	defer taskManager.Close()
	_, err := startDaemonAdminService(daemonAdminServiceOptions{
		PreferredAddr:   "127.0.0.1:" + pickFreePort(t),
		MaxPortFallback: 0,
		RuntimePath:     filepath.Join(t.TempDir(), "admin_runtime.json"),
		DaemonPID:       os.Getpid(),
		DaemonSessionID: "daemon-test-session",
		DaemonStartedAt: time.Now(),
		TaskManager:     taskManager,
		MonitorMu:       &sync.Mutex{},
		Monitors:        map[int]*daemonMonitorRuntime{},
		MonitorRecords:  map[string]daemonMonitorRecord{},
	})
	if err == nil {
		t.Fatalf("expected auth config validation failure")
	}
}

func TestStartDaemonAdminServiceFallbackAndEndpoints(t *testing.T) {
	basePort := pickFreePort(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:"+basePort)
	if err != nil {
		t.Fatalf("occupy base admin port failed: %v", err)
	}
	defer occupied.Close()

	taskManager := newDaemonTaskManagerWithPaths(func(ctx context.Context, commands [][]string) error { return nil }, filepath.Join(t.TempDir(), "daemon_tasks.json"), filepath.Join(t.TempDir(), "daemon_tasks_history.json"))
	defer taskManager.Close()
	taskSnapshot, err := taskManager.addTask(context.Background(), time.Minute, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("add task failed: %v", err)
	}

	var monitorMu sync.Mutex
	monitors := map[int]*daemonMonitorRuntime{}
	monitorRecords := map[string]daemonMonitorRecord{
		"mailbox=inbox|unread=true": {
			ID:          "m-1",
			Key:         "mailbox=inbox|unread=true",
			CommandLine: "mail monitor --wait-timeout 1m",
			CreatedAt:   time.Now().Format(time.RFC3339Nano),
			Paused:      true,
		},
	}
	saveCalls := 0
	startedMonitorID := ""
	startAllCalls := 0
	stubWebhookStartCalls := 0
	stubWebhookStopCalls := 0
	stubWebhookKillCalls := 0
	stubBookingStartCalls := 0
	stubBookingStopCalls := 0
	stubBookingStatusCalls := 0
	daemonSessionID := "daemon-test-session"
	bookingOwnedMu := &sync.Mutex{}
	bookingOwnedPIDs := map[int]daemonOwnedBookingRuntime{}
	authConfig := daemonAdminAuthConfig{
		Username: testDaemonAdminUsername,
		Password: testDaemonAdminPassword,
	}

	runtimePath := filepath.Join(t.TempDir(), "admin_runtime.json")
	svc, err := startDaemonAdminService(daemonAdminServiceOptions{
		PreferredAddr:   "127.0.0.1:" + basePort,
		MaxPortFallback: 2,
		RuntimePath:     runtimePath,
		DaemonPID:       os.Getpid(),
		DaemonSessionID: daemonSessionID,
		DaemonStartedAt: time.Now(),
		TaskManager:     taskManager,
		MonitorMu:       &monitorMu,
		Monitors:        monitors,
		MonitorRecords:  monitorRecords,
		SaveMonitorState: func() error {
			saveCalls++
			return nil
		},
		StartMonitorByID: func(id string) error {
			startedMonitorID = id
			return nil
		},
		StartAllMonitors: func() (int, error) {
			startAllCalls++
			return 1, nil
		},
		WebhookOwnedMu:   &sync.Mutex{},
		WebhookOwnedPIDs: map[int]daemonOwnedWebhookRuntime{},
		BookingOwnedMu:   bookingOwnedMu,
		BookingOwnedPIDs: bookingOwnedPIDs,
		WebhookRuntime:   filepath.Join(t.TempDir(), "webhook_runtime.json"),
		WebhookLogPath:   filepath.Join(t.TempDir(), "webhook_server.log"),
		WebhookStartFn: func() (webhookStartResult, error) {
			stubWebhookStartCalls++
			return webhookStartResult{Mode: "start", Status: "started", Message: "stub start"}, nil
		},
		WebhookStopFn: func() (webhookStopResult, error) {
			stubWebhookStopCalls++
			return webhookStopResult{Mode: "stop", Status: "stopped"}, nil
		},
		WebhookKillFn: func() (webhookKillPortResult, error) {
			stubWebhookKillCalls++
			return webhookKillPortResult{Mode: "kill-port", Status: "stopped", Port: 8080}, nil
		},
		BookingStartFn: func() (bookingServiceStartResult, error) {
			stubBookingStartCalls++
			return bookingServiceStartResult{
				Mode:   "service_start",
				Status: "started",
				Runtime: &bookingServiceRuntimeInfo{
					PID:             12001,
					Address:         "127.0.0.1:18081",
					StartedAt:       time.Now().Format(time.RFC3339Nano),
					OwnerSessionID:  daemonSessionID,
					OwnerDaemonPID:  os.Getpid(),
					OwnerClaimedAt:  time.Now().Format(time.RFC3339Nano),
					OwnerStartToken: "admin-booking-owner-token",
				},
			}, nil
		},
		BookingStopFn: func() (bookingServiceStopResult, error) {
			stubBookingStopCalls++
			return bookingServiceStopResult{
				Mode:   "service_stop",
				Status: "stopped",
				PID:    12001,
			}, nil
		},
		BookingStatusFn: func() (bookingServiceStatusResult, error) {
			stubBookingStatusCalls++
			return bookingServiceStatusResult{
				Mode:    "service_status",
				Status:  "running",
				Running: true,
				Runtime: &bookingServiceRuntimeInfo{
					PID:       12001,
					Address:   "127.0.0.1:18081",
					StartedAt: time.Now().Format(time.RFC3339Nano),
				},
				URL: "http://127.0.0.1:18081/admin/booking/status",
			}, nil
		},
		AuthConfig: authConfig,
	})
	if err != nil {
		t.Fatalf("startDaemonAdminService failed: %v", err)
	}
	defer func() {
		if closeErr := svc.Close(); closeErr != nil {
			t.Fatalf("daemon admin close failed: %v", closeErr)
		}
	}()

	if svc.Address() == "127.0.0.1:"+basePort {
		t.Fatalf("expected fallback address due occupied base port, got %s", svc.Address())
	}
	if !strings.Contains(svc.URL(), "/admin") {
		t.Fatalf("expected admin url includes /admin, got %s", svc.URL())
	}

	statusRuntime, exists, err := readDaemonAdminRuntimeState(runtimePath)
	if err != nil {
		t.Fatalf("readDaemonAdminRuntimeState failed: %v", err)
	}
	if !exists || statusRuntime == nil {
		t.Fatalf("expected daemon admin runtime persisted")
	}
	if statusRuntime.Address != svc.Address() {
		t.Fatalf("expected runtime address %s, got %s", svc.Address(), statusRuntime.Address)
	}

	baseURL := strings.TrimSuffix(svc.URL(), daemonAdminHomePath)

	unauthHomeResp, err := http.Get(svc.URL())
	if err != nil {
		t.Fatalf("GET admin home without auth failed: %v", err)
	}
	if unauthHomeResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, unauthHomeResp)
		t.Fatalf("expected /admin unauthorized without auth, got %d body=%s", unauthHomeResp.StatusCode, body)
	}
	if challenge := strings.TrimSpace(unauthHomeResp.Header.Get("WWW-Authenticate")); !strings.Contains(challenge, "Basic") {
		t.Fatalf("expected WWW-Authenticate Basic header, got %q", challenge)
	}
	_ = unauthHomeResp.Body.Close()

	unauthStatusResp, err := http.Get(baseURL + daemonAdminStatusPath)
	if err != nil {
		t.Fatalf("GET admin status without auth failed: %v", err)
	}
	if unauthStatusResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, unauthStatusResp)
		t.Fatalf("expected /admin/status unauthorized without auth, got %d body=%s", unauthStatusResp.StatusCode, body)
	}
	_ = unauthStatusResp.Body.Close()

	unauthPostResp, err := http.PostForm(baseURL+daemonAdminTaskGlobalResumePath, url.Values{})
	if err != nil {
		t.Fatalf("POST global-resume without auth failed: %v", err)
	}
	if unauthPostResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, unauthPostResp)
		t.Fatalf("expected POST action unauthorized without auth, got %d body=%s", unauthPostResp.StatusCode, body)
	}
	_ = unauthPostResp.Body.Close()

	unauthBookingAdminResp, err := http.Get(baseURL + daemonAdminBookingServiceStatusPath)
	if err != nil {
		t.Fatalf("GET admin booking service status without auth failed: %v", err)
	}
	if unauthBookingAdminResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, unauthBookingAdminResp)
		t.Fatalf("expected /admin/booking/service/status unauthorized without auth, got %d body=%s", unauthBookingAdminResp.StatusCode, body)
	}
	_ = unauthBookingAdminResp.Body.Close()

	publicCatalogResp, err := http.Get(baseURL + bookingPublicCatalogPath)
	if err != nil {
		t.Fatalf("GET daemon legacy public booking catalog failed: %v", err)
	}
	if publicCatalogResp.StatusCode != http.StatusNotFound {
		body := readAllAndClose(t, publicCatalogResp)
		t.Fatalf("expected daemon legacy /booking/catalog removed (404), got %d body=%s", publicCatalogResp.StatusCode, body)
	}
	_ = publicCatalogResp.Body.Close()

	legacyAdminBookingResp, err := httpGetWithBasicAuth(baseURL+bookingAdminStatusPath, authConfig.Username, authConfig.Password)
	if err != nil {
		t.Fatalf("GET daemon legacy /admin/booking/status failed: %v", err)
	}
	if legacyAdminBookingResp.StatusCode != http.StatusNotFound {
		body := readAllAndClose(t, legacyAdminBookingResp)
		t.Fatalf("expected daemon legacy /admin/booking/status removed (404), got %d body=%s", legacyAdminBookingResp.StatusCode, body)
	}
	_ = legacyAdminBookingResp.Body.Close()

	wrongAuthResp, err := httpGetWithBasicAuth(svc.URL(), "wrong-user", "wrong-password")
	if err != nil {
		t.Fatalf("GET admin home with wrong auth failed: %v", err)
	}
	if wrongAuthResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, wrongAuthResp)
		t.Fatalf("expected /admin unauthorized with wrong auth, got %d body=%s", wrongAuthResp.StatusCode, body)
	}
	_ = wrongAuthResp.Body.Close()

	homeResp, err := httpGetWithBasicAuth(svc.URL(), authConfig.Username, authConfig.Password)
	if err != nil {
		t.Fatalf("GET admin home failed: %v", err)
	}
	if homeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /admin 200, got %d", homeResp.StatusCode)
	}
	homeBody := readAllAndClose(t, homeResp)
	if !strings.Contains(homeBody, "Daemon Admin Home") ||
		!strings.Contains(homeBody, "Refresh Page") ||
		!strings.Contains(homeBody, "Start Webhook") ||
		!strings.Contains(homeBody, "Stop Webhook") ||
		!strings.Contains(homeBody, "Start Booking Service") ||
		!strings.Contains(homeBody, "Stop Booking Service") ||
		!strings.Contains(homeBody, daemonAdminBookingServiceStatusPath) ||
		!strings.Contains(homeBody, daemonAdminTaskGlobalPausePath) ||
		!strings.Contains(homeBody, "State</th>") ||
		!strings.Contains(homeBody, "Last Result</th>") ||
		!strings.Contains(homeBody, "window.setTimeout(function() { window.location.reload(); }, 400);") {
		t.Fatalf("unexpected admin home content: %s", homeBody)
	}
	if strings.Contains(homeBody, "Kill Port :8080") || strings.Contains(homeBody, daemonAdminWebhookKillPortPath) {
		t.Fatalf("expected admin home to hide kill-port button, got: %s", homeBody)
	}
	if !strings.Contains(homeBody, `id="webhook-feedback"`) || !strings.Contains(homeBody, `id="booking-feedback"`) {
		t.Fatalf("expected module feedback containers in admin home, got: %s", homeBody)
	}
	webhookSectionIndex := strings.Index(homeBody, "<h2>Webhook</h2>")
	startWebhookIndex := strings.Index(homeBody, "Start Webhook")
	stopWebhookIndex := strings.Index(homeBody, "Stop Webhook")
	if webhookSectionIndex < 0 || startWebhookIndex < 0 || stopWebhookIndex < 0 {
		t.Fatalf("expected webhook section and controls in admin home, got: %s", homeBody)
	}
	if startWebhookIndex < webhookSectionIndex || stopWebhookIndex < webhookSectionIndex {
		t.Fatalf("expected webhook controls rendered inside webhook section, got: %s", homeBody)
	}
	if !strings.Contains(homeBody, "postAction('"+daemonAdminWebhookStartPath+"', '', '', false, 'webhook-feedback')") {
		t.Fatalf("expected webhook start action targets webhook feedback area, got: %s", homeBody)
	}
	if !strings.Contains(homeBody, "postAction('"+daemonAdminWebhookStopPath+"', '', 'Stop current webhook process now?', true, 'webhook-feedback')") {
		t.Fatalf("expected webhook stop action targets webhook feedback area, got: %s", homeBody)
	}

	statusResp, err := httpGetWithBasicAuth(baseURL+daemonAdminStatusPath, authConfig.Username, authConfig.Password)
	if err != nil {
		t.Fatalf("GET admin status failed: %v", err)
	}
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /admin/status 200, got %d", statusResp.StatusCode)
	}
	statusBody, err := io.ReadAll(statusResp.Body)
	if err != nil {
		_ = statusResp.Body.Close()
		t.Fatalf("read /admin/status failed: %v", err)
	}
	_ = statusResp.Body.Close()

	var statusPayload daemonAdminStatusResponse
	if err := json.Unmarshal(statusBody, &statusPayload); err != nil {
		t.Fatalf("decode /admin/status failed: %v", err)
	}
	if statusPayload.Daemon.PID != os.Getpid() {
		t.Fatalf("expected daemon pid %d, got %d", os.Getpid(), statusPayload.Daemon.PID)
	}
	if statusPayload.Task.Total != 1 || len(statusPayload.Task.Tasks) != 1 {
		t.Fatalf("expected one task in status, got %+v", statusPayload.Task)
	}
	if statusPayload.Task.Scheduled != 0 {
		t.Fatalf("expected scheduled count 0 for paused task, got %+v", statusPayload.Task)
	}
	if statusPayload.Task.Tasks[0].ID != taskSnapshot.ID {
		t.Fatalf("expected task id %s in status, got %s", taskSnapshot.ID, statusPayload.Task.Tasks[0].ID)
	}
	if statusPayload.Task.Tasks[0].State != "paused" {
		t.Fatalf("expected derived state paused, got %+v", statusPayload.Task.Tasks[0])
	}
	if statusPayload.Task.Tasks[0].LastResult != "unknown" {
		t.Fatalf("expected derived last_result unknown, got %+v", statusPayload.Task.Tasks[0])
	}
	if statusPayload.Monitor.Total != 1 || len(statusPayload.Monitor.Monitors) != 1 {
		t.Fatalf("expected one monitor in status, got %+v", statusPayload.Monitor)
	}
	if statusPayload.Booking.Status != "running" || !statusPayload.Booking.Running {
		t.Fatalf("expected booking service running in status payload, got %+v", statusPayload.Booking)
	}
	if statusPayload.Booking.PID != 12001 {
		t.Fatalf("expected booking service pid=12001, got %+v", statusPayload.Booking)
	}
	if stubBookingStatusCalls == 0 {
		t.Fatalf("expected booking status callback invoked")
	}

	bookingServiceStatusResp, err := httpGetWithBasicAuth(baseURL+daemonAdminBookingServiceStatusPath, authConfig.Username, authConfig.Password)
	if err != nil {
		t.Fatalf("GET /admin/booking/service/status failed: %v", err)
	}
	bookingServiceStatusBody := readAllAndClose(t, bookingServiceStatusResp)
	if bookingServiceStatusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /admin/booking/service/status 200, got %d body=%s", bookingServiceStatusResp.StatusCode, bookingServiceStatusBody)
	}
	if !strings.Contains(bookingServiceStatusBody, "\"running\": true") {
		t.Fatalf("expected running booking service payload, got: %s", bookingServiceStatusBody)
	}

	var statusRaw map[string]any
	if err := json.Unmarshal(statusBody, &statusRaw); err != nil {
		t.Fatalf("decode /admin/status raw failed: %v", err)
	}
	taskRaw, ok := statusRaw["task"].(map[string]any)
	if !ok {
		t.Fatalf("expected task object in /admin/status raw payload")
	}
	if _, exists := taskRaw["scheduled"]; !exists {
		t.Fatalf("expected task.scheduled in /admin/status payload")
	}
	taskListRaw, ok := taskRaw["tasks"].([]any)
	if !ok || len(taskListRaw) == 0 {
		t.Fatalf("expected non-empty task list in /admin/status payload")
	}
	firstTaskRaw, ok := taskListRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected first task object in /admin/status payload")
	}
	for _, key := range []string{"state", "state_text", "last_result", "last_status", "paused", "running", "LastStatus"} {
		if _, exists := firstTaskRaw[key]; !exists {
			t.Fatalf("expected task field %q in /admin/status payload: %+v", key, firstTaskRaw)
		}
	}

	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminTaskResumePath, url.Values{"id": {taskSnapshot.ID}}, authConfig.Username, authConfig.Password, http.StatusOK)
	tasksAfterResume := taskManager.listTasks()
	if len(tasksAfterResume) != 1 || tasksAfterResume[0].Paused {
		t.Fatalf("expected task resumed, got %+v", tasksAfterResume)
	}

	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminTaskPausePath, url.Values{"id": {taskSnapshot.ID}}, authConfig.Username, authConfig.Password, http.StatusOK)
	tasksAfterPause := taskManager.listTasks()
	if len(tasksAfterPause) != 1 || !tasksAfterPause[0].Paused {
		t.Fatalf("expected task paused again, got %+v", tasksAfterPause)
	}

	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminMonitorStartPath, url.Values{"id": {"m-1"}}, authConfig.Username, authConfig.Password, http.StatusOK)
	if startedMonitorID != "m-1" {
		t.Fatalf("expected monitor start callback id m-1, got %s", startedMonitorID)
	}

	cancelCalled := false
	monitorMu.Lock()
	monitors[1] = &daemonMonitorRuntime{
		id:          1,
		recordID:    "m-1",
		key:         "mailbox=inbox|unread=true",
		commandLine: "mail monitor --wait-timeout 1m",
		startedAt:   time.Now(),
		cancel: func() {
			cancelCalled = true
		},
	}
	record := monitorRecords["mailbox=inbox|unread=true"]
	record.Paused = false
	monitorRecords["mailbox=inbox|unread=true"] = record
	monitorMu.Unlock()

	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminMonitorStopPath, url.Values{"id": {"m-1"}}, authConfig.Username, authConfig.Password, http.StatusOK)
	if !cancelCalled {
		t.Fatalf("expected monitor cancel called")
	}
	if saveCalls == 0 {
		t.Fatalf("expected monitor save state called")
	}

	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminMonitorStartAllPath, url.Values{}, authConfig.Username, authConfig.Password, http.StatusOK)
	if startAllCalls == 0 {
		t.Fatalf("expected start-all callback called")
	}

	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminWebhookStartPath, url.Values{}, authConfig.Username, authConfig.Password, http.StatusOK)
	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminWebhookStopPath, url.Values{}, authConfig.Username, authConfig.Password, http.StatusOK)
	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminWebhookKillPortPath, url.Values{}, authConfig.Username, authConfig.Password, http.StatusOK)
	if stubWebhookStartCalls != 1 || stubWebhookStopCalls != 1 || stubWebhookKillCalls != 1 {
		t.Fatalf("expected webhook action stubs called once, got start=%d stop=%d kill=%d", stubWebhookStartCalls, stubWebhookStopCalls, stubWebhookKillCalls)
	}
	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminBookingServiceStartPath, url.Values{}, authConfig.Username, authConfig.Password, http.StatusOK)
	bookingOwnedMu.Lock()
	if _, tracked := bookingOwnedPIDs[12001]; !tracked {
		bookingOwnedMu.Unlock()
		t.Fatalf("expected booking pid tracked after admin booking start, got %v", bookingOwnedPIDs)
	}
	bookingOwnedMu.Unlock()
	mustPOSTFormWithBasicAuth(t, baseURL+daemonAdminBookingServiceStopPath, url.Values{}, authConfig.Username, authConfig.Password, http.StatusOK)
	bookingOwnedMu.Lock()
	if _, tracked := bookingOwnedPIDs[12001]; tracked {
		bookingOwnedMu.Unlock()
		t.Fatalf("expected booking pid untracked after admin booking stop, got %v", bookingOwnedPIDs)
	}
	bookingOwnedMu.Unlock()
	if stubBookingStartCalls != 1 || stubBookingStopCalls != 1 {
		t.Fatalf("expected booking service start/stop callbacks once, got start=%d stop=%d", stubBookingStartCalls, stubBookingStopCalls)
	}
	getBookingActionResp, err := httpGetWithBasicAuth(baseURL+daemonAdminBookingServiceStartPath, authConfig.Username, authConfig.Password)
	if err != nil {
		t.Fatalf("GET booking service action endpoint failed: %v", err)
	}
	if getBookingActionResp.StatusCode != http.StatusMethodNotAllowed {
		body := readAllAndClose(t, getBookingActionResp)
		t.Fatalf("expected GET /admin/booking/service/start 405, got %d body=%s", getBookingActionResp.StatusCode, body)
	}
	_ = getBookingActionResp.Body.Close()

	resp405, err := httpGetWithBasicAuth(baseURL+daemonAdminTaskPausePath, authConfig.Username, authConfig.Password)
	if err != nil {
		t.Fatalf("GET task pause endpoint failed: %v", err)
	}
	if resp405.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected GET action endpoint 405, got %d", resp405.StatusCode)
	}
	_ = resp405.Body.Close()
}

func TestStartDaemonAdminServiceAllPortsBusy(t *testing.T) {
	basePort := pickFreePort(t)
	listener, err := net.Listen("tcp", "127.0.0.1:"+basePort)
	if err != nil {
		t.Fatalf("occupy base admin port failed: %v", err)
	}
	defer listener.Close()
	taskManager := newDaemonTaskManagerWithPaths(func(ctx context.Context, commands [][]string) error { return nil }, filepath.Join(t.TempDir(), "daemon_tasks.json"), filepath.Join(t.TempDir(), "daemon_tasks_history.json"))
	defer taskManager.Close()

	_, err = startDaemonAdminService(daemonAdminServiceOptions{
		PreferredAddr:   "127.0.0.1:" + basePort,
		MaxPortFallback: 0,
		RuntimePath:     filepath.Join(t.TempDir(), "admin_runtime.json"),
		DaemonPID:       os.Getpid(),
		DaemonSessionID: "daemon-test-session",
		DaemonStartedAt: time.Now(),
		TaskManager:     taskManager,
		MonitorMu:       &sync.Mutex{},
		Monitors:        map[int]*daemonMonitorRuntime{},
		MonitorRecords:  map[string]daemonMonitorRecord{},
		AuthConfig: daemonAdminAuthConfig{
			Username: testDaemonAdminUsername,
			Password: testDaemonAdminPassword,
		},
	})
	if err == nil {
		t.Fatalf("expected daemon admin start failure when all ports busy")
	}
}

func TestStartDaemonAdminServiceSupportsPasswordHashAuth(t *testing.T) {
	hash, err := hashDaemonAdminPassword("hash-secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword failed: %v", err)
	}
	taskManager := newDaemonTaskManagerWithPaths(func(ctx context.Context, commands [][]string) error { return nil }, filepath.Join(t.TempDir(), "daemon_tasks.json"), filepath.Join(t.TempDir(), "daemon_tasks_history.json"))
	defer taskManager.Close()

	svc, err := startDaemonAdminService(daemonAdminServiceOptions{
		PreferredAddr:   "127.0.0.1:" + pickFreePort(t),
		MaxPortFallback: 0,
		RuntimePath:     filepath.Join(t.TempDir(), "admin_runtime.json"),
		DaemonPID:       os.Getpid(),
		DaemonSessionID: "daemon-hash-auth",
		DaemonStartedAt: time.Now(),
		TaskManager:     taskManager,
		MonitorMu:       &sync.Mutex{},
		Monitors:        map[int]*daemonMonitorRuntime{},
		MonitorRecords:  map[string]daemonMonitorRecord{},
		AuthConfig: daemonAdminAuthConfig{
			Username:     testDaemonAdminUsername,
			PasswordHash: hash,
		},
	})
	if err != nil {
		t.Fatalf("startDaemonAdminService hash auth failed: %v", err)
	}
	defer func() {
		_ = svc.Close()
	}()

	okResp, err := httpGetWithBasicAuth(svc.URL(), testDaemonAdminUsername, "hash-secret")
	if err != nil {
		t.Fatalf("GET admin home with hash auth failed: %v", err)
	}
	if okResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, okResp)
		t.Fatalf("expected /admin 200 with hash auth, got %d body=%s", okResp.StatusCode, body)
	}
	_ = okResp.Body.Close()

	badResp, err := httpGetWithBasicAuth(svc.URL(), testDaemonAdminUsername, "wrong-secret")
	if err != nil {
		t.Fatalf("GET admin home with wrong hash auth failed: %v", err)
	}
	if badResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, badResp)
		t.Fatalf("expected /admin 401 with wrong hash auth, got %d body=%s", badResp.StatusCode, body)
	}
	_ = badResp.Body.Close()
}

func TestStartDaemonAdminServiceReloadsAuthConfigWithoutRestart(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "admin_auth.json")
	initialHash, err := hashDaemonAdminPassword("old-secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword old failed: %v", err)
	}
	if err := writeDaemonAdminAuthConfig(authPath, daemonAdminAuthConfig{
		Username:     testDaemonAdminUsername,
		PasswordHash: initialHash,
	}); err != nil {
		t.Fatalf("write initial admin auth failed: %v", err)
	}
	loadedAuth, err := loadDaemonAdminAuthConfig(authPath)
	if err != nil {
		t.Fatalf("load initial admin auth failed: %v", err)
	}

	taskManager := newDaemonTaskManagerWithPaths(func(ctx context.Context, commands [][]string) error { return nil }, filepath.Join(t.TempDir(), "daemon_tasks.json"), filepath.Join(t.TempDir(), "daemon_tasks_history.json"))
	defer taskManager.Close()

	svc, err := startDaemonAdminService(daemonAdminServiceOptions{
		PreferredAddr:   "127.0.0.1:" + pickFreePort(t),
		MaxPortFallback: 0,
		RuntimePath:     filepath.Join(t.TempDir(), "admin_runtime.json"),
		AuthConfigPath:  authPath,
		DaemonPID:       os.Getpid(),
		DaemonSessionID: "daemon-auth-reload",
		DaemonStartedAt: time.Now(),
		TaskManager:     taskManager,
		MonitorMu:       &sync.Mutex{},
		Monitors:        map[int]*daemonMonitorRuntime{},
		MonitorRecords:  map[string]daemonMonitorRecord{},
		AuthConfig:      loadedAuth,
	})
	if err != nil {
		t.Fatalf("startDaemonAdminService failed: %v", err)
	}
	defer func() {
		_ = svc.Close()
	}()

	oldResp, err := httpGetWithBasicAuth(svc.URL(), testDaemonAdminUsername, "old-secret")
	if err != nil {
		t.Fatalf("GET admin home with old password failed: %v", err)
	}
	if oldResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, oldResp)
		t.Fatalf("expected old password 200 before reload, got %d body=%s", oldResp.StatusCode, body)
	}
	_ = oldResp.Body.Close()

	nextHash, err := hashDaemonAdminPassword("new-secret", 4)
	if err != nil {
		t.Fatalf("hashDaemonAdminPassword new failed: %v", err)
	}
	if err := writeDaemonAdminAuthConfig(authPath, daemonAdminAuthConfig{
		Username:     testDaemonAdminUsername,
		PasswordHash: nextHash,
	}); err != nil {
		t.Fatalf("write updated admin auth failed: %v", err)
	}

	oldAfterResp, err := httpGetWithBasicAuth(svc.URL(), testDaemonAdminUsername, "old-secret")
	if err != nil {
		t.Fatalf("GET admin home old password after reload failed: %v", err)
	}
	if oldAfterResp.StatusCode != http.StatusUnauthorized {
		body := readAllAndClose(t, oldAfterResp)
		t.Fatalf("expected old password 401 after reload, got %d body=%s", oldAfterResp.StatusCode, body)
	}
	_ = oldAfterResp.Body.Close()

	newResp, err := httpGetWithBasicAuth(svc.URL(), testDaemonAdminUsername, "new-secret")
	if err != nil {
		t.Fatalf("GET admin home with new password failed: %v", err)
	}
	if newResp.StatusCode != http.StatusOK {
		body := readAllAndClose(t, newResp)
		t.Fatalf("expected new password 200 after reload, got %d body=%s", newResp.StatusCode, body)
	}
	_ = newResp.Body.Close()
}

func TestBuildDaemonAdminTaskViewState(t *testing.T) {
	cases := []struct {
		name        string
		task        daemonTaskSnapshot
		globalPause bool
		wantState   string
		wantText    string
		wantResult  string
	}{
		{
			name:       "running pause pending",
			task:       daemonTaskSnapshot{Running: true, Paused: true},
			wantState:  "running_pause_pending",
			wantText:   "Running (pause pending)",
			wantResult: "unknown",
		},
		{
			name:       "running",
			task:       daemonTaskSnapshot{Running: true},
			wantState:  "running",
			wantText:   "Running",
			wantResult: "unknown",
		},
		{
			name:       "paused",
			task:       daemonTaskSnapshot{Paused: true},
			wantState:  "paused",
			wantText:   "Paused",
			wantResult: "unknown",
		},
		{
			name:        "globally paused",
			task:        daemonTaskSnapshot{},
			globalPause: true,
			wantState:   "globally_paused",
			wantText:    "Globally Paused",
			wantResult:  "unknown",
		},
		{
			name:       "scheduled last success",
			task:       daemonTaskSnapshot{LastStatus: "success"},
			wantState:  "scheduled",
			wantText:   "Scheduled (last success)",
			wantResult: "success",
		},
		{
			name:       "scheduled last failed by status",
			task:       daemonTaskSnapshot{LastStatus: "failed"},
			wantState:  "scheduled",
			wantText:   "Scheduled (last failed)",
			wantResult: "failed",
		},
		{
			name:       "scheduled last failed by error",
			task:       daemonTaskSnapshot{LastError: "boom"},
			wantState:  "scheduled",
			wantText:   "Scheduled (last failed)",
			wantResult: "failed",
		},
		{
			name:       "scheduled unknown",
			task:       daemonTaskSnapshot{},
			wantState:  "scheduled",
			wantText:   "Scheduled",
			wantResult: "unknown",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			view := buildDaemonAdminTaskView(tc.task, tc.globalPause)
			if view.State != tc.wantState {
				t.Fatalf("expected state %s, got %+v", tc.wantState, view)
			}
			if view.StateText != tc.wantText {
				t.Fatalf("expected state text %q, got %+v", tc.wantText, view)
			}
			if view.LastResult != tc.wantResult {
				t.Fatalf("expected last result %q, got %+v", tc.wantResult, view)
			}
		})
	}
}

func pickFreePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port listen failed: %v", err)
	}
	defer listener.Close()
	addr := listener.Addr().String()
	parts := strings.Split(addr, ":")
	if len(parts) == 0 {
		t.Fatalf("unexpected listener addr %s", addr)
	}
	return parts[len(parts)-1]
}

func mustPOSTForm(t *testing.T, endpoint string, form url.Values, expectedCode int) {
	t.Helper()
	resp, err := http.PostForm(endpoint, form)
	if err != nil {
		t.Fatalf("POST %s failed: %v", endpoint, err)
	}
	if resp.StatusCode != expectedCode {
		body := readAllAndClose(t, resp)
		t.Fatalf("POST %s expected %d got %d body=%s", endpoint, expectedCode, resp.StatusCode, body)
	}
	_ = resp.Body.Close()
}

func mustPOSTFormWithBasicAuth(t *testing.T, endpoint string, form url.Values, username string, password string, expectedCode int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build POST %s failed: %v", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(username, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s failed: %v", endpoint, err)
	}
	if resp.StatusCode != expectedCode {
		body := readAllAndClose(t, resp)
		t.Fatalf("POST %s expected %d got %d body=%s", endpoint, expectedCode, resp.StatusCode, body)
	}
	_ = resp.Body.Close()
}

func mustPostJSON(t *testing.T, endpoint string, body string, username string, password string, expectedCode int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s failed: %v", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(username) != "" || strings.TrimSpace(password) != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s failed: %v", endpoint, err)
	}
	responseBody := readAllAndClose(t, resp)
	if resp.StatusCode != expectedCode {
		t.Fatalf("POST %s expected %d got %d body=%s", endpoint, expectedCode, resp.StatusCode, responseBody)
	}
	return responseBody
}

func httpGetWithBasicAuth(endpoint string, username string, password string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(username, password)
	return http.DefaultClient.Do(req)
}

func readAllAndClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return ""
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)
	return buf.String()
}
