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
		DaemonSessionID: "daemon-test-session",
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
		!strings.Contains(homeBody, daemonAdminTaskGlobalPausePath) ||
		!strings.Contains(homeBody, "State</th>") ||
		!strings.Contains(homeBody, "Last Result</th>") ||
		!strings.Contains(homeBody, "window.setTimeout(function() { window.location.reload(); }, 400);") {
		t.Fatalf("unexpected admin home content: %s", homeBody)
	}
	if strings.Contains(homeBody, "Kill Port :8080") || strings.Contains(homeBody, daemonAdminWebhookKillPortPath) {
		t.Fatalf("expected admin home to hide kill-port button, got: %s", homeBody)
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
