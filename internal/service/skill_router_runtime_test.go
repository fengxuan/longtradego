package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeSkillRouterEndpointDefaultsPath(t *testing.T) {
	endpoint, err := normalizeSkillRouterEndpoint("http://127.0.0.1:19090", "127.0.0.1", 19090)
	if err != nil {
		t.Fatalf("normalizeSkillRouterEndpoint failed: %v", err)
	}
	if endpoint != "http://127.0.0.1:19090/v1/route" {
		t.Fatalf("unexpected endpoint: %s", endpoint)
	}
}

func TestResolveSkillRouterRuntimePathFromConfig(t *testing.T) {
	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"sidecar": map[string]any{
			"runtime": "data/custom_router_runtime.json",
		},
	})
	resolved := resolveSkillRouterRuntimePath("", cfgPath)
	if filepath.Clean(resolved) != filepath.Clean("data/custom_router_runtime.json") {
		t.Fatalf("expected runtime path from config, got %s", resolved)
	}
}

func TestSkillRouterStatusNotRunning(t *testing.T) {
	runtimePath := filepath.Join(t.TempDir(), "skill_router_runtime.json")
	status, err := skillRouterStatus(runtimePath)
	if err != nil {
		t.Fatalf("skillRouterStatus failed: %v", err)
	}
	if status.Status != "stopped" || status.Running {
		t.Fatalf("expected stopped status, got %+v", status)
	}
}

func TestStopSkillRouterRemovesStaleRuntime(t *testing.T) {
	runtimePath := filepath.Join(t.TempDir(), "skill_router_runtime.json")
	if err := writeSkillRouterRuntimeState(runtimePath, skillRouterRuntimeInfo{
		PID:      0,
		Endpoint: "http://127.0.0.1:19090/v1/route",
	}); err != nil {
		t.Fatalf("write runtime failed: %v", err)
	}
	result, err := stopSkillRouter(runtimePath, 0)
	if err != nil {
		t.Fatalf("stopSkillRouter failed: %v", err)
	}
	if !strings.EqualFold(result.Status, "stale_removed") {
		t.Fatalf("expected stale_removed status, got %+v", result)
	}
	if _, exists, err := readSkillRouterRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime after stop failed: %v", err)
	} else if exists {
		t.Fatalf("expected runtime removed after stale stop")
	}
}

func TestShouldAutoStartSkillRouterSidecar(t *testing.T) {
	if !shouldAutoStartSkillRouterSidecar("http://127.0.0.1:19090/v1/route", 19090) {
		t.Fatalf("expected local configured endpoint to auto-start")
	}
	if shouldAutoStartSkillRouterSidecar("http://127.0.0.1:18080/v1/route", 19090) {
		t.Fatalf("expected mismatched port not to auto-start")
	}
	if shouldAutoStartSkillRouterSidecar("https://api.example.com/v1/route", 19090) {
		t.Fatalf("expected remote endpoint not to auto-start")
	}
}

func TestEnsureSkillRouterSidecarRunningSkipsRemoteEndpoint(t *testing.T) {
	called := false
	original := startSkillRouterInBackgroundForRoute
	startSkillRouterInBackgroundForRoute = func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		called = true
		return skillRouterStartResult{}, nil
	}
	defer func() {
		startSkillRouterInBackgroundForRoute = original
	}()

	result, err := ensureSkillRouterSidecarRunning("conf/skills_llm.json", skillLLMConfig{
		Router: skillRouterConfig{
			Endpoint: "https://api.example.com/v1/route",
		},
		Sidecar: skillRouterSidecar{
			Port: 19090,
		},
	})
	if err != nil {
		t.Fatalf("ensureSkillRouterSidecarRunning should skip remote endpoint, got error: %v", err)
	}
	if called {
		t.Fatalf("expected start not called for remote endpoint")
	}
	if !strings.EqualFold(result.Status, "skipped") {
		t.Fatalf("expected skipped status, got %+v", result)
	}
}

func TestLoadSkillCatalogFileMissingIsNoop(t *testing.T) {
	items, err := loadSkillCatalogFile(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("expected missing skills catalog to be ignored, got: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty catalog result for missing file, got %+v", items)
	}
}

func TestLoadSkillCatalogFileNormalizesEntries(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "skills_catalog.json")
	raw := strings.Join([]string{
		`{`,
		`  "skills": [`,
		`    {"id":" PDF-PRO ","capabilities":["PDF","pdf","ocr"],"priority":90,"source":"sample/pdf-pro","source_url":"https://example.com/pdf-pro","install_command_template":"npx skills add {source} -g -y"},`,
		`    {"id":"pdf-pro","capabilities":["pdf"],"priority":10,"source":"duplicate/should-skip"}`,
		`  ]`,
		`}`,
	}, "\n")
	if err := os.WriteFile(catalogPath, []byte(raw), 0o644); err != nil {
		t.Fatalf("write catalog failed: %v", err)
	}
	items, err := loadSkillCatalogFile(catalogPath)
	if err != nil {
		t.Fatalf("loadSkillCatalogFile failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one normalized catalog row, got %+v", items)
	}
	if items[0].ID != "PDF-PRO" {
		t.Fatalf("unexpected normalized id: %+v", items[0])
	}
	if strings.Join(items[0].Capabilities, ",") != "ocr,pdf" {
		t.Fatalf("unexpected normalized capabilities: %+v", items[0].Capabilities)
	}
}
