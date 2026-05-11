package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDiscoverInstalledSkillsMissingLock(t *testing.T) {
	root := t.TempDir()
	paths := skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: filepath.Join(root, ".agents", "skills"),
		SkillsConfig:   filepath.Join(root, "skills.json"),
	}
	_, _, err := discoverInstalledSkills(paths)
	if err == nil {
		t.Fatalf("expected missing lock error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Fatalf("expected missing lock error text, got: %v", err)
	}
}

func TestDiscoverInstalledSkillsScansRootWithoutLock(t *testing.T) {
	root := t.TempDir()
	skillsRoot := filepath.Join(root, "skills")
	if err := os.MkdirAll(filepath.Join(skillsRoot, "weather"), 0o755); err != nil {
		t.Fatalf("mkdir weather skill dir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsRoot, "weather", "SKILL.md"), []byte("# weather\nGet current weather"), 0o644); err != nil {
		t.Fatalf("write weather SKILL.md failed: %v", err)
	}

	items, _, err := discoverInstalledSkills(skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: skillsRoot,
		SkillsConfig:   filepath.Join(root, "skills.json"),
	})
	if err != nil {
		t.Fatalf("expected discovery from skill root without lock, got: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one discovered skill, got %d (%+v)", len(items), items)
	}
	if items[0].ID != "weather" {
		t.Fatalf("expected weather skill discovered, got %+v", items[0])
	}
	if items[0].SourceType != "directory" {
		t.Fatalf("expected source_type=directory, got %+v", items[0])
	}
	if !slices.Contains(items[0].AllowedActions, "sys") {
		t.Fatalf("expected discovered non-longbridge skill to allow sys action by default, got %+v", items[0].AllowedActions)
	}
}

func TestDiscoverInstalledSkillsSupportsConfiguredSkillRoots(t *testing.T) {
	root := t.TempDir()
	altRoot := filepath.Join(root, "clawhub-skills")
	if err := os.MkdirAll(filepath.Join(altRoot, "weather"), 0o755); err != nil {
		t.Fatalf("mkdir alt weather skill dir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(altRoot, "weather", "SKILL.md"), []byte("# weather\nGet current weather"), 0o644); err != nil {
		t.Fatalf("write alt weather SKILL.md failed: %v", err)
	}

	cfgPath := filepath.Join(root, "skills.json")
	writeSkillTestJSON(t, cfgPath, map[string]any{
		"skill_roots": []string{altRoot},
	})

	items, _, err := discoverInstalledSkills(skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: filepath.Join(root, "empty"),
		SkillsConfig:   cfgPath,
	})
	if err != nil {
		t.Fatalf("expected discovery from configured skill_roots, got: %v", err)
	}
	if len(items) != 1 || items[0].ID != "weather" {
		t.Fatalf("expected configured root skill weather discovered, got %+v", items)
	}
}

func TestDiscoverInstalledSkillsRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	agentsRoot := filepath.Join(root, ".agents")
	if err := os.MkdirAll(agentsRoot, 0o755); err != nil {
		t.Fatalf("mkdir agents root failed: %v", err)
	}
	lock := skillLockFile{
		Version: 1,
		Skills: map[string]skillLockFileEntry{
			"longbridge": {
				Source:    "longbridge/developers",
				SkillPath: "../outside/SKILL.md",
			},
		},
	}
	lockPath := filepath.Join(agentsRoot, ".skill-lock.json")
	writeSkillTestJSON(t, lockPath, lock)

	_, _, err := discoverInstalledSkills(skillRuntimePaths{
		LockPath:       lockPath,
		SkillsRootPath: filepath.Join(agentsRoot, "skills"),
		SkillsConfig:   filepath.Join(root, "skills.json"),
	})
	if err == nil {
		t.Fatalf("expected path traversal error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "path traversal") {
		t.Fatalf("expected path traversal error, got: %v", err)
	}
}

func TestRunSkillCommandRejectsLowConfidence(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "market_screen_v1",
			Confidence: 0.2,
			Arguments: map[string]any{
				"symbols": []any{"700.HK"},
			},
		}, nil
	})
	defer restoreRouter()

	_, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:          "strategy screener request",
		Symbols:       []string{"700.HK"},
		DryRun:        true,
		MinConfidence: 0.6,
		Paths:         paths,
	})
	if err == nil {
		t.Fatalf("expected low confidence rejection")
	}
	if !strings.Contains(err.Error(), "below threshold") {
		t.Fatalf("expected low confidence error, got: %v", err)
	}
}

func TestRouteSkillIntentByExplicitCommandLongbridge(t *testing.T) {
	decision, ok := routeSkillIntentByExplicitCommand("longbridge quote TSLA.US --format json", []skillInstalled{
		{
			ID:             "longbridge",
			Enabled:        true,
			AllowedActions: []string{"lb", "email", "task"},
		},
	})
	if !ok {
		t.Fatalf("expected explicit command routing success")
	}
	if decision.SkillID != "longbridge" {
		t.Fatalf("expected longbridge skill, got %q", decision.SkillID)
	}
	if decision.Intent != "explicit_command" {
		t.Fatalf("expected explicit_command intent, got %q", decision.Intent)
	}
	if decision.Confidence != 1 {
		t.Fatalf("expected confidence=1, got %v", decision.Confidence)
	}
	if len(decision.Actions) != 1 || decision.Actions[0].Type != "lb" {
		t.Fatalf("expected one lb action, got %+v", decision.Actions)
	}
	if !strings.Contains(decision.Actions[0].Command, "longbridge quote TSLA.US") {
		t.Fatalf("unexpected action command: %q", decision.Actions[0].Command)
	}
}

func TestRouteSkillIntentByExplicitCommandPDF(t *testing.T) {
	decision, ok := routeSkillIntentByExplicitCommand("pdftotext -layout -f 1 -l 5 input.pdf output.txt", []skillInstalled{
		{
			ID:             "pdf",
			Enabled:        true,
			AllowedActions: []string{"sys", "task"},
		},
	})
	if !ok {
		t.Fatalf("expected explicit command routing success")
	}
	if decision.SkillID != "pdf" {
		t.Fatalf("expected pdf skill, got %q", decision.SkillID)
	}
	if len(decision.Actions) != 1 || decision.Actions[0].Type != "sys" {
		t.Fatalf("expected one sys action, got %+v", decision.Actions)
	}
	if !strings.Contains(decision.Actions[0].Command, "pdftotext -layout -f 1 -l 5 input.pdf output.txt") {
		t.Fatalf("unexpected action command: %q", decision.Actions[0].Command)
	}
}

func TestRouteSkillIntentByExplicitCommandIgnoresNaturalSentence(t *testing.T) {
	_, ok := routeSkillIntentByExplicitCommand("/tmp/sample.pdf pdf to text", []skillInstalled{
		{
			ID:             "pdf",
			Enabled:        true,
			AllowedActions: []string{"sys", "task"},
		},
	})
	if ok {
		t.Fatalf("expected natural sentence not to be treated as explicit command")
	}
}

func TestRunSkillCommandExplicitCommandBypassesLLM(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{}, fmt.Errorf("router should not be called for explicit command")
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "longbridge quote TSLA.US --format json",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand explicit dry-run failed: %v", err)
	}
	if result.SkillID != "longbridge" {
		t.Fatalf("expected longbridge skill, got %q", result.SkillID)
	}
	if result.Intent != "explicit_command" {
		t.Fatalf("expected explicit_command intent, got %q", result.Intent)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected one planned action, got %+v", result.Actions)
	}
	if result.Actions[0].Status != "planned" || result.Actions[0].Type != "lb" {
		t.Fatalf("expected planned lb action, got %+v", result.Actions[0])
	}
	if !strings.Contains(result.Actions[0].Command, "longbridge quote TSLA.US") {
		t.Fatalf("unexpected planned command: %q", result.Actions[0].Command)
	}
}

func TestRunSkillCommandQuoteToPDFUsesRouterMultiSkill(t *testing.T) {
	paths := writeLongbridgeAndPDFSkillFixture(t)
	routerCalled := false
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		routerCalled = true
		return skillRouteDecision{
			SkillID:    "multi-skill",
			Intent:     "multi_skill_workflow",
			Confidence: 0.93,
			Steps: []skillRouteStep{
				{
					SkillID:   "longbridge",
					Intent:    "get_stock_price",
					Arguments: map[string]any{"symbols": []string{"TSLA.US"}},
					Actions: []skillAction{
						{Type: "lb", Command: "quote TSLA.US --format json"},
					},
				},
				{
					SkillID:   "pdf",
					Intent:    "generate_pdf",
					Arguments: map[string]any{"output_file": "tesla_1111.pdf"},
					Actions: []skillAction{
						{Type: "sys", Command: "sys -- python3 -c 'print(\"tesla_1111.pdf\")'"},
					},
				},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "获取特斯拉股票的信息 保存到pdf file filename is tesla_1111.pdf",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand quote->pdf via router dry-run failed: %v", err)
	}
	if !routerCalled {
		t.Fatalf("expected router to be called for quote->pdf request")
	}
	if result.SkillID != "multi-skill" {
		t.Fatalf("expected multi-skill route, got %q", result.SkillID)
	}
	if result.Intent != "multi_skill_workflow" {
		t.Fatalf("expected multi_skill_workflow intent, got %q", result.Intent)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("expected two planned actions, got %+v", result.Actions)
	}
	if !strings.Contains(result.Actions[0].Note, "step_1:longbridge") {
		t.Fatalf("expected first action from longbridge step, got %+v", result.Actions[0])
	}
	if !strings.Contains(result.Actions[1].Note, "step_2:pdf") {
		t.Fatalf("expected second action from pdf step, got %+v", result.Actions[1])
	}
}

func TestRunSkillCommandWeatherToPDFUsesRouterMultiSkill(t *testing.T) {
	paths := writeWeatherAndPDFSkillFixture(t)
	routerCalled := false
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		routerCalled = true
		return skillRouteDecision{
			SkillID:    "multi-skill",
			Intent:     "multi_skill_workflow",
			Confidence: 0.95,
			Steps: []skillRouteStep{
				{
					SkillID:   "weather",
					Intent:    "get_weather",
					Arguments: map[string]any{"location": "New York"},
					Actions: []skillAction{
						{Type: "sys", Command: "sys -- /bin/sh -lc 'echo New York: 24C'"},
					},
				},
				{
					SkillID:   "pdf",
					Intent:    "generate_pdf",
					Arguments: map[string]any{"output_file": "skill_output.pdf"},
					Actions: []skillAction{
						{Type: "sys", Command: "sys -- python3 -c 'print(\"skill_output.pdf\")'"},
					},
				},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "纽约当前天气 保存到pdf文件",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand weather->pdf via router dry-run failed: %v", err)
	}
	if !routerCalled {
		t.Fatalf("expected router to be called for weather->pdf request")
	}
	if result.SkillID != "multi-skill" {
		t.Fatalf("expected multi-skill route, got %q", result.SkillID)
	}
	if result.Intent != "multi_skill_workflow" {
		t.Fatalf("expected multi_skill_workflow intent, got %q", result.Intent)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("expected two planned actions, got %+v", result.Actions)
	}
	if !strings.Contains(result.Actions[0].Note, "step_1:weather") || !strings.Contains(result.Actions[1].Note, "step_2:pdf") {
		t.Fatalf("expected step notes for planned actions, got %+v", result.Actions)
	}
}

func TestRunSkillCommandQuoteToPDFMultiStepPrefersLocalFallbackForNarrativeSysAction(t *testing.T) {
	paths := writeLongbridgeAndPDFSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "multi-skill",
			Intent:     "multi_skill_workflow",
			Confidence: 0.95,
			Steps: []skillRouteStep{
				{
					SkillID:   "longbridge",
					Intent:    "quote",
					Arguments: map[string]any{"symbol": "TSLA.US"},
					Actions: []skillAction{
						{Type: "lb", Command: "longbridge quote TSLA.US"},
					},
				},
				{
					SkillID:   "pdf",
					Intent:    "generate_pdf",
					Arguments: map[string]any{"output_file": "xxxy.pdf"},
					Actions: []skillAction{
						{Type: "sys", Command: "echo 'Generate xxxy.pdf from the retrieved TSLA.US quote'"},
					},
				},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "特斯拉的股票价格 保存 to file xxxy.pdf",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand quote->pdf narrative action dry-run failed: %v", err)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("expected two planned actions, got %+v", result.Actions)
	}
	if strings.Contains(result.Actions[1].Command, "echo 'Generate xxxy.pdf") {
		t.Fatalf("expected pdf step to avoid narrative echo action, got %q", result.Actions[1].Command)
	}
	if !strings.Contains(result.Actions[1].Command, "python3 -c") {
		t.Fatalf("expected pdf step to use local python fallback command, got %q", result.Actions[1].Command)
	}
	if !strings.Contains(result.Actions[1].Command, pipelineEnvResultJSON) {
		t.Fatalf("expected pdf fallback action to include pipeline env var, got %q", result.Actions[1].Command)
	}
}

func TestRunSkillCommandDryRunSupportsMultiSkillSteps(t *testing.T) {
	paths := writeWeatherAndPDFSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "multi-skill",
			Intent:     "multi_skill_workflow",
			Confidence: 0.92,
			Steps: []skillRouteStep{
				{
					SkillID:   "weather",
					Intent:    "get_weather",
					Arguments: map[string]any{"location": "Shanghai"},
					Actions: []skillAction{
						{Type: "sys", Command: "sys -- /bin/sh -lc 'echo Shanghai: 25C'"},
					},
				},
				{
					SkillID:   "pdf",
					Intent:    "generate_pdf",
					Arguments: map[string]any{"output_file": "skill_output.pdf"},
					Actions: []skillAction{
						{Type: "sys", Command: "sys -- python3 -c 'print(\"skill_output.pdf\")'"},
					},
				},
			},
			Reason: "unit_test_multi_skill",
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "组合任务",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand multi-skill dry-run failed: %v", err)
	}
	if result.SkillID != "multi-skill" {
		t.Fatalf("expected multi-skill route, got %q", result.SkillID)
	}
	if result.Intent != "multi_skill_workflow" {
		t.Fatalf("expected multi_skill_workflow intent, got %q", result.Intent)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("expected two planned actions, got %+v", result.Actions)
	}
	if result.Actions[0].Status != "planned" || !strings.Contains(result.Actions[0].Note, "step_1:weather") {
		t.Fatalf("unexpected first action: %+v", result.Actions[0])
	}
	if result.Actions[1].Status != "planned" || !strings.Contains(result.Actions[1].Note, "step_2:pdf") {
		t.Fatalf("unexpected second action: %+v", result.Actions[1])
	}
	if len(result.Route.Steps) != 2 {
		t.Fatalf("expected route steps in v2 route payload, got %+v", result.Route.Steps)
	}
}

func TestRunSkillCommandRejectsDisallowedAction(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "market_screen_v1",
			Confidence: 0.9,
			Arguments: map[string]any{
				"symbols": []any{"700.HK"},
			},
			Actions: []skillAction{
				{Type: "sys", Command: "uname -a"},
			},
		}, nil
	})
	defer restoreRouter()

	_, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:    "strategy screener request",
		Symbols: []string{"700.HK"},
		DryRun:  true,
		Paths:   paths,
	})
	if err == nil {
		t.Fatalf("expected disallowed action rejection")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected disallowed action error, got: %v", err)
	}
}

func TestRunSkillCommandAcceptsQueryActionAsLB(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "get_stock_price",
			Confidence: 0.9,
			Arguments: map[string]any{
				"ticker": "TSLA",
			},
			Actions: []skillAction{
				{Type: "query"},
			},
		}, nil
	})
	defer restoreRouter()

	restoreRunner := setSkillLongbridgeRunnerForTest(func(ctx context.Context, args []string) (any, string, error) {
		commandLine := FormatCommandLine(append([]string{longbridgeCLIProgram}, args...))
		if len(args) == 0 || strings.ToLower(strings.TrimSpace(args[0])) != "quote" {
			return nil, commandLine, fmt.Errorf("expected quote command, got %v", args)
		}
		return []any{
			map[string]any{
				"symbol": "TSLA.US",
				"last":   "345.620",
			},
		}, commandLine, nil
	})
	defer restoreRunner()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "给我当前特斯拉的股票价格信息",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand failed: %v", err)
	}
	quote, ok := result.Result.(skillQuoteResult)
	if !ok {
		t.Fatalf("expected quote result, got %T", result.Result)
	}
	if len(quote.Quotes) != 1 || quote.Quotes[0].Symbol != "TSLA.US" {
		t.Fatalf("expected TSLA.US quote row, got %+v", quote.Quotes)
	}
}

func TestRunSkillCommandDryRunPlansActions(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "market_screen_v1",
			Confidence: 0.95,
			Arguments:  map[string]any{},
			Actions: []skillAction{
				{Type: "lb", Command: "calc-index"},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:    "factor screener request",
		Symbols: []string{"700.HK", "9988.HK"},
		DryRun:  true,
		Paths:   paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand dry-run failed: %v", err)
	}
	if !result.DryRun {
		t.Fatalf("expected dry-run=true")
	}
	if len(result.Actions) < 4 {
		t.Fatalf("expected planned actions, got %v", result.Actions)
	}
	if result.Actions[0].Status != "planned" || !strings.Contains(result.Actions[0].Command, "calc-index") {
		t.Fatalf("unexpected first planned action: %+v", result.Actions[0])
	}
}

func TestRunSkillCommandMarketScreenUsesUniverseFallbackWithoutSymbols(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "market_screen_v1",
			Confidence: 0.9,
			Arguments: map[string]any{
				"regions": []any{"US", "HK"},
				"sector":  "tech",
				"pe_max":  25,
			},
			Actions: []skillAction{
				{Type: "lb"},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "筛选 US+HK 科技股，条件：市值>500亿USD、PE<25",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("expected universe fallback to avoid symbol error, got: %v", err)
	}
	if len(result.Actions) == 0 {
		t.Fatalf("expected planned actions from fallback universe")
	}
	if !strings.Contains(result.Actions[0].Command, "calc-index") {
		t.Fatalf("expected first planned action to be calc-index, got %+v", result.Actions[0])
	}
	payload, ok := result.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected dry-run map payload, got %T", result.Result)
	}
	criteria, ok := payload["criteria"].(map[string]any)
	if !ok {
		t.Fatalf("expected criteria map in dry-run payload, got %#v", payload["criteria"])
	}
	symbolsRaw, ok := criteria["symbols"].([]string)
	if !ok {
		t.Fatalf("expected criteria symbols []string, got %#v", criteria["symbols"])
	}
	if len(symbolsRaw) < 5 {
		t.Fatalf("expected fallback symbol set, got %v", symbolsRaw)
	}
	if !slices.Contains(symbolsRaw, "TSLA.US") || !slices.Contains(symbolsRaw, "700.HK") {
		t.Fatalf("expected fallback to include US/HK tech symbols, got %v", symbolsRaw)
	}
}

func TestRunSkillCommandQuoteIntentInfersSymbolFromText(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "get_stock_price",
			Confidence: 0.95,
			Arguments:  map[string]any{},
			Actions: []skillAction{
				{Type: "lb"},
			},
		}, nil
	})
	defer restoreRouter()

	restoreRunner := setSkillLongbridgeRunnerForTest(func(ctx context.Context, args []string) (any, string, error) {
		commandLine := FormatCommandLine(append([]string{longbridgeCLIProgram}, args...))
		if len(args) == 0 || strings.ToLower(strings.TrimSpace(args[0])) != "quote" {
			return nil, commandLine, fmt.Errorf("expected quote command, got %v", args)
		}
		if !slices.Contains(args, "TSLA.US") {
			return nil, commandLine, fmt.Errorf("expected inferred symbol TSLA.US in args, got %v", args)
		}
		return []any{
			map[string]any{
				"symbol":     "TSLA.US",
				"last":       "345.620",
				"prev_close": "343.250",
				"open":       "343.150",
				"high":       "348.880",
				"low":        "337.250",
				"volume":     "62164016",
				"turnover":   "21375312140.000",
				"status":     "Normal",
			},
		}, commandLine, nil
	})
	defer restoreRunner()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "给我当前特斯拉的股票价格信息",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand quote intent failed: %v", err)
	}
	quote, ok := result.Result.(skillQuoteResult)
	if !ok {
		t.Fatalf("expected skillQuoteResult, got %T", result.Result)
	}
	if len(quote.Symbols) != 1 || quote.Symbols[0] != "TSLA.US" {
		t.Fatalf("expected inferred symbols [TSLA.US], got %v", quote.Symbols)
	}
	if len(quote.Quotes) != 1 || quote.Quotes[0].Symbol != "TSLA.US" {
		t.Fatalf("expected TSLA.US quote row, got %+v", quote.Quotes)
	}
	if len(result.Actions) != 1 || result.Actions[0].Status != "success" {
		t.Fatalf("expected single successful quote action, got %+v", result.Actions)
	}
}

func TestParseSymbolsFromDecisionSupportsTickerKey(t *testing.T) {
	symbols := parseSymbolsFromDecision(map[string]any{
		"ticker": "TSLA",
	})
	if !slices.Equal(symbols, []string{"TSLA.US"}) {
		t.Fatalf("expected [TSLA.US], got %v", symbols)
	}
}

func TestRunSkillCommandExecutesMarketScreenV1(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "market_screen_v1",
			Confidence: 0.95,
			Arguments: map[string]any{
				"symbols": []any{"700.HK", "IBM.US"},
			},
			Actions: []skillAction{
				{Type: "lb"},
			},
		}, nil
	})
	defer restoreRouter()

	crossSeries := buildSkillCrossSeriesForTest(t)
	noCrossSeries := buildSkillNoCrossSeriesForTest()
	restoreRunner := setSkillLongbridgeRunnerForTest(func(ctx context.Context, args []string) (any, string, error) {
		if len(args) == 0 {
			return nil, "longbridge", fmt.Errorf("missing args")
		}
		commandLine := FormatCommandLine(append([]string{longbridgeCLIProgram}, args...))
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "calc-index":
			return []any{
				map[string]any{"symbol": "700.HK", "total_market_value": 600_000_000_000.0, "pe": 20.0, "currency": "HKD"},
				map[string]any{"symbol": "IBM.US", "total_market_value": 200_000_000_000.0, "pe": 30.0, "currency": "USD"},
			}, commandLine, nil
		case "exchange-rate":
			return map[string]any{"HKDUSD": 0.128205128}, commandLine, nil
		case "kline":
			if len(args) < 2 {
				return nil, commandLine, fmt.Errorf("missing symbol for kline")
			}
			symbol := strings.ToUpper(strings.TrimSpace(args[1]))
			switch symbol {
			case "700.HK":
				return map[string]any{"data": buildKlineRowsForTest(crossSeries)}, commandLine, nil
			case "IBM.US":
				return map[string]any{"data": buildKlineRowsForTest(noCrossSeries)}, commandLine, nil
			default:
				return nil, commandLine, fmt.Errorf("unexpected symbol %s", symbol)
			}
		default:
			return nil, commandLine, fmt.Errorf("unexpected command %q", args[0])
		}
	})
	defer restoreRunner()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:    "US and HK tech screener",
		Symbols: []string{"700.HK", "IBM.US"},
		DryRun:  false,
		Paths:   paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand execute failed: %v", err)
	}
	screen, ok := result.Result.(skillMarketScreenResult)
	if !ok {
		t.Fatalf("expected skillMarketScreenResult, got %T", result.Result)
	}
	if len(screen.Matched) != 1 || screen.Matched[0].Symbol != "700.HK" {
		t.Fatalf("expected 700.HK matched, got matched=%+v rejected=%+v", screen.Matched, screen.Rejected)
	}
	if len(screen.Rejected) != 1 || screen.Rejected[0].Symbol != "IBM.US" {
		t.Fatalf("expected IBM.US rejected, got %+v", screen.Rejected)
	}
	if !containsAnyReason(screen.Rejected[0].Reasons, "pe", "MACD") {
		t.Fatalf("expected reject reason to include PE/MACD, got %v", screen.Rejected[0].Reasons)
	}
	if len(result.Actions) < 4 {
		t.Fatalf("expected executed actions, got %v", result.Actions)
	}
	if result.Actions[0].Status != "success" {
		t.Fatalf("expected first action success, got %+v", result.Actions[0])
	}
}

func TestRunSkillCommandDryRunPlansGenericWeatherActions(t *testing.T) {
	paths := writeWeatherSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "weather",
			Intent:     "get_weather",
			Confidence: 0.9,
			Arguments: map[string]any{
				"location": "Shanghai",
			},
			Actions: []skillAction{
				{Type: "sys", Command: `curl -s "wttr.in/Shanghai?format=3"`},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "上海今天天气怎么样",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand dry-run failed: %v", err)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected one planned action, got %+v", result.Actions)
	}
	if result.Actions[0].Type != "sys" || result.Actions[0].Status != "planned" {
		t.Fatalf("expected planned sys action, got %+v", result.Actions[0])
	}
	if !strings.Contains(result.Actions[0].Command, "sys --shell") {
		t.Fatalf("expected sys --shell command, got %q", result.Actions[0].Command)
	}
}

func TestRunSkillCommandExecutesGenericActionsWithPipelineInput(t *testing.T) {
	paths := writeWeatherSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "weather",
			Intent:     "notify_weather",
			Confidence: 0.92,
			Arguments: map[string]any{
				"location": "Shanghai",
			},
			Actions: []skillAction{
				{Type: "sys", Command: `curl -s "wttr.in/Shanghai?format=3"`},
				{Type: "email", Command: `send --to bot@example.com --subject "Weather"`},
			},
		}, nil
	})
	defer restoreRouter()

	restoreCLI := setCLIExecutorForTest(func(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error {
		app.SetExecution(strings.ToLower(strings.TrimSpace(rawArgs[0])), rawArgs[1:])
		switch strings.ToLower(strings.TrimSpace(rawArgs[0])) {
		case "sys":
			app.SetResult(map[string]any{
				"stdout": "Shanghai: +26C",
			})
			return nil
		case "email":
			input, ok := daemonPipelineInputFromContext(ctx)
			if !ok {
				return fmt.Errorf("expected pipeline input for email stage")
			}
			if !strings.Contains(input.SourceCommand, "sys --shell") {
				return fmt.Errorf("unexpected pipeline source command: %q", input.SourceCommand)
			}
			if !strings.Contains(input.ResultJSON, "Shanghai") {
				return fmt.Errorf("unexpected pipeline result json: %q", input.ResultJSON)
			}
			app.SetResult(map[string]any{
				"sent": true,
			})
			return nil
		default:
			return fmt.Errorf("unexpected command: %v", rawArgs)
		}
	})
	defer restoreCLI()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "把上海天气发邮件给我",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand execute generic actions failed: %v", err)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("expected 2 executed actions, got %+v", result.Actions)
	}
	if result.Actions[0].Status != "success" || result.Actions[1].Status != "success" {
		t.Fatalf("expected successful action statuses, got %+v", result.Actions)
	}
	payload, ok := result.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result payload, got %T", result.Result)
	}
	steps, ok := payload["steps"].([]map[string]any)
	if !ok {
		t.Fatalf("expected steps payload, got %#v", payload["steps"])
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %+v", steps)
	}
}

func TestRunSkillCommandConvertsNonSchedulerTaskActionToSysForPDF(t *testing.T) {
	paths := writePDFSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "pdf",
			Intent:     "generate_pdf",
			Confidence: 0.9,
			Actions: []skillAction{
				{Type: "task", Command: `task generate pdf with content 'hello world'`},
			},
		}, nil
	})
	defer restoreRouter()

	restoreCLI := setCLIExecutorForTest(func(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error {
		app.SetExecution(strings.ToLower(strings.TrimSpace(rawArgs[0])), rawArgs[1:])
		if len(rawArgs) < 3 || strings.ToLower(strings.TrimSpace(rawArgs[0])) != "sys" || rawArgs[1] != "--shell" {
			return fmt.Errorf("expected sys --shell action, got %v", rawArgs)
		}
		if !strings.Contains(rawArgs[2], "base64") || !strings.Contains(rawArgs[2], "skill_output.pdf") {
			return fmt.Errorf("unexpected rewritten shell command: %s", rawArgs[2])
		}
		app.SetResult(map[string]any{"stdout": "skill_output.pdf"})
		return nil
	})
	defer restoreCLI()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "可以帮我生成一个pdf文件 里面写一个hello world文字",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand pdf fallback failed: %v", err)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected one action, got %+v", result.Actions)
	}
	if result.Actions[0].Type != "sys" || result.Actions[0].Status != "success" {
		t.Fatalf("expected converted sys success action, got %+v", result.Actions[0])
	}
}

func TestRunSkillCommandFallsBackToLocalPDFActionWhenPrimaryFails(t *testing.T) {
	paths := writePDFSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "pdf",
			Intent:     "generate_pdf",
			Confidence: 0.91,
			Arguments: map[string]any{
				"content": "hello world",
			},
			Actions: []skillAction{
				{Type: "sys", Command: `echo 'hello world' > hello.txt && pandoc hello.txt -o hello.pdf`},
			},
		}, nil
	})
	defer restoreRouter()

	callCount := 0
	restoreCLI := setCLIExecutorForTest(func(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error {
		app.SetExecution(strings.ToLower(strings.TrimSpace(rawArgs[0])), rawArgs[1:])
		if len(rawArgs) < 3 || strings.ToLower(strings.TrimSpace(rawArgs[0])) != "sys" || rawArgs[1] != "--shell" {
			return fmt.Errorf("expected sys --shell action, got %v", rawArgs)
		}
		callCount++
		if callCount == 1 {
			if !strings.Contains(rawArgs[2], "pandoc") {
				return fmt.Errorf("expected first primary command to keep pandoc, got %s", rawArgs[2])
			}
			return fmt.Errorf("pandoc: command not found")
		}
		if callCount != 2 {
			return fmt.Errorf("unexpected extra sys invocation #%d: %v", callCount, rawArgs)
		}
		if strings.Contains(rawArgs[2], "pandoc") {
			return fmt.Errorf("expected local pdf fallback action without pandoc, got %s", rawArgs[2])
		}
		if !strings.Contains(rawArgs[2], "base64") {
			return fmt.Errorf("expected local base64 pdf fallback shell command, got %s", rawArgs[2])
		}
		app.SetResult(map[string]any{"stdout": "skill_output.pdf"})
		return nil
	})
	defer restoreCLI()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "可以帮我生成一个pdf文件 里面写一个hello world文字",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand pdf force-local failed: %v", err)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("expected two actions (primary+fallback), got %+v", result.Actions)
	}
	if result.Actions[0].Status != "failed" || result.Actions[0].Note != "primary" {
		t.Fatalf("expected first action failed primary, got %+v", result.Actions[0])
	}
	if result.Actions[1].Status != "success" || result.Actions[1].Note != "pdf_local_fallback" {
		t.Fatalf("expected second action successful local fallback, got %+v", result.Actions[1])
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no terminal errors after fallback success, got %+v", result.Errors)
	}
}

func TestRunSkillCommandPrefersLocalPDFActionWhenPrimaryInstructionLacksContent(t *testing.T) {
	paths := writePDFSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "pdf",
			Intent:     "generate_pdf",
			Confidence: 0.95,
			Arguments: map[string]any{
				"content": "我们明天到公园玩",
			},
			Actions: []skillAction{
				{Type: "sys", Command: "generate_pdf_from_text"},
			},
		}, nil
	})
	defer restoreRouter()

	callCount := 0
	restoreCLI := setCLIExecutorForTest(func(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error {
		app.SetExecution(strings.ToLower(strings.TrimSpace(rawArgs[0])), rawArgs[1:])
		if len(rawArgs) < 3 || strings.ToLower(strings.TrimSpace(rawArgs[0])) != "sys" || rawArgs[1] != "--shell" {
			return fmt.Errorf("expected sys --shell action, got %v", rawArgs)
		}
		callCount++
		if callCount != 1 {
			return fmt.Errorf("expected a single local fallback invocation, got #%d: %v", callCount, rawArgs)
		}
		if !strings.Contains(rawArgs[2], "'skill_output.pdf' '我们明天到公园玩'") {
			return fmt.Errorf("expected rewritten shell command to use route content, got %s", rawArgs[2])
		}
		if strings.Contains(rawArgs[2], "'skill_output.pdf' 'hello world'") {
			return fmt.Errorf("expected rewritten shell command not to use default hello world, got %s", rawArgs[2])
		}
		app.SetResult(map[string]any{"stdout": "skill_output.pdf"})
		return nil
	})
	defer restoreCLI()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "写入 我们明天到公园玩 到pdf文件",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand pdf local preference failed: %v", err)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected one action, got %+v", result.Actions)
	}
	if result.Actions[0].Type != "sys" || result.Actions[0].Status != "success" {
		t.Fatalf("expected converted sys success action, got %+v", result.Actions[0])
	}
}

func TestRunSkillCommandForcesLocalPDFExtractAction(t *testing.T) {
	paths := writePDFSkillFixture(t)
	inputPDF := filepath.Join(t.TempDir(), "sample.pdf")
	if err := os.WriteFile(inputPDF, buildSimplePDFBytes("hello world"), 0o644); err != nil {
		t.Fatalf("write sample pdf failed: %v", err)
	}

	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "pdf",
			Intent:     "pdf_to_text",
			Confidence: 0.92,
			Arguments: map[string]any{
				"input_file": inputPDF,
			},
			Actions: []skillAction{
				{Type: "task", Command: "task extract text from " + inputPDF},
			},
		}, nil
	})
	defer restoreRouter()

	restoreCLI := setCLIExecutorForTest(func(ctx context.Context, app *AppContext, commandLogger *CommandFileLogger, rawArgs []string) error {
		app.SetExecution(strings.ToLower(strings.TrimSpace(rawArgs[0])), rawArgs[1:])
		if len(rawArgs) < 3 || strings.ToLower(strings.TrimSpace(rawArgs[0])) != "sys" || rawArgs[1] != "--shell" {
			return fmt.Errorf("expected sys --shell action, got %v", rawArgs)
		}
		if strings.Contains(rawArgs[2], "base64 --decode") {
			return fmt.Errorf("expected extract-text command, got pdf-generate command: %s", rawArgs[2])
		}
		if !strings.Contains(rawArgs[2], inputPDF) {
			return fmt.Errorf("expected extract-text command to include input pdf path, got %s", rawArgs[2])
		}
		app.SetResult(map[string]any{"stdout": "hello world"})
		return nil
	})
	defer restoreCLI()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   inputPDF + " pdf to text",
		DryRun: false,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand pdf extract failed: %v", err)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected one action, got %+v", result.Actions)
	}
	if result.Actions[0].Type != "sys" || result.Actions[0].Status != "success" {
		t.Fatalf("expected converted sys success action, got %+v", result.Actions[0])
	}
}

func TestBuildPDFExtractTextOptionsFromTextAndRouteArgs(t *testing.T) {
	inputPath := "/tmp/input.pdf"
	opts := buildPDFExtractTextOptions(map[string]any{
		"input_file":      inputPath,
		"output_text":     "out.txt",
		"preserve_layout": true,
		"first_page":      1,
		"last_page":       5,
	}, "ignored")
	if opts.InputPath != inputPath {
		t.Fatalf("unexpected input path: %q", opts.InputPath)
	}
	if opts.OutputPath != "out.txt" {
		t.Fatalf("unexpected output path: %q", opts.OutputPath)
	}
	if !opts.PreserveLayout {
		t.Fatalf("expected preserve layout enabled")
	}
	if opts.FirstPage != 1 || opts.LastPage != 5 {
		t.Fatalf("unexpected page range: %d-%d", opts.FirstPage, opts.LastPage)
	}

	parsed := buildPDFExtractTextOptions(nil, "pdftotext -layout -f 2 -l 6 /tmp/a.pdf /tmp/a.txt")
	if parsed.InputPath != "/tmp/a.pdf" {
		t.Fatalf("unexpected parsed input path: %q", parsed.InputPath)
	}
	if parsed.OutputPath != "/tmp/a.txt" {
		t.Fatalf("unexpected parsed output path: %q", parsed.OutputPath)
	}
	if !parsed.PreserveLayout {
		t.Fatalf("expected parsed preserve layout enabled")
	}
	if parsed.FirstPage != 2 || parsed.LastPage != 6 {
		t.Fatalf("unexpected parsed page range: %d-%d", parsed.FirstPage, parsed.LastPage)
	}
}

func TestBuildLocalPDFExtractTextShellCommandPrefersPdftotext(t *testing.T) {
	cmd := buildLocalPDFExtractTextShellCommand(pdfExtractTextOptions{
		InputPath:      "/tmp/input.pdf",
		OutputPath:     "/tmp/output.txt",
		PreserveLayout: true,
		FirstPage:      1,
		LastPage:       5,
	})
	if !strings.Contains(cmd, "command -v pdftotext") {
		t.Fatalf("expected pdftotext availability check, got %q", cmd)
	}
	if !strings.Contains(cmd, "pdftotext -layout -f 1 -l 5 /tmp/input.pdf /tmp/output.txt") {
		t.Fatalf("expected pdftotext extraction command, got %q", cmd)
	}
	if !strings.Contains(cmd, "python3 -c") {
		t.Fatalf("expected python fallback command, got %q", cmd)
	}
	if !strings.Contains(cmd, "cat '/tmp/output.txt'") {
		t.Fatalf("expected output cat for file output mode, got %q", cmd)
	}
}

func TestExtractPDFTextContentFromInstructionSupportsWriteToPDFPattern(t *testing.T) {
	got := extractPDFTextContentFromInstruction("写入 我们明天到公园玩 到pdf文件")
	if got != "我们明天到公园玩" {
		t.Fatalf("expected extracted content from write-to-pdf pattern, got %q", got)
	}
}

func TestExtractPDFTextContentFromInstructionSupportsFromTextPattern(t *testing.T) {
	got := extractPDFTextContentFromInstruction("create pdf from text: 我们明天到公园玩")
	if got != "我们明天到公园玩" {
		t.Fatalf("expected extracted content from from-text pattern, got %q", got)
	}
}

func TestBuildPDFLocalFallbackActionsUsesInstructionContentWhenRouteDefaultsHelloWorld(t *testing.T) {
	actions := buildPDFLocalFallbackActions(skillInstalled{
		ID:             "pdf",
		AllowedActions: []string{"sys"},
	}, "generate_pdf", "写入 我们明天到公园玩 到pdf文件", map[string]any{
		"content": "hello world",
	})
	if len(actions) != 1 {
		t.Fatalf("expected one fallback action, got %+v", actions)
	}
	if !strings.Contains(actions[0].Command, "我们明天到公园玩") {
		t.Fatalf("expected fallback command to include extracted instruction content, got %q", actions[0].Command)
	}
	if !strings.Contains(actions[0].Command, "'skill_output.pdf' '我们明天到公园玩'") {
		t.Fatalf("expected fallback command args to pass extracted content, got %q", actions[0].Command)
	}
	if strings.Contains(actions[0].Command, "'skill_output.pdf' 'hello world'") {
		t.Fatalf("expected fallback command args to avoid default hello world content, got %q", actions[0].Command)
	}
}

func TestBuildPDFLocalFallbackActionsUsesPipelineForWeatherContentSource(t *testing.T) {
	actions := buildPDFLocalFallbackActions(skillInstalled{
		ID:             "pdf",
		AllowedActions: []string{"sys"},
	}, "create_pdf", "北京天气 保存到file zzz.pdf", map[string]any{
		"content_source": "weather_result",
		"filename":       "zzz.pdf",
	})
	if len(actions) != 1 {
		t.Fatalf("expected one fallback action, got %+v", actions)
	}
	if !strings.Contains(actions[0].Command, pipelineEnvResultJSON) {
		t.Fatalf("expected weather fallback command to include pipeline result env var, got %q", actions[0].Command)
	}
	if !strings.Contains(actions[0].Command, "'zzz.pdf'") {
		t.Fatalf("expected weather fallback command to keep requested output file, got %q", actions[0].Command)
	}
}

func TestRewriteSkillInstructionToShellUsesPipelineQuoteResultForPDF(t *testing.T) {
	script, ok := rewriteSkillInstructionToShell("save quote result to PDF file xxxy.pdf")
	if !ok {
		t.Fatalf("expected rewrite to produce shell script")
	}
	if !strings.Contains(script, "xxxy.pdf") {
		t.Fatalf("expected rewritten script to include output file, got %q", script)
	}
	if !strings.Contains(script, pipelineEnvResultJSON) {
		t.Fatalf("expected rewritten script to include pipeline result env var, got %q", script)
	}
	if !strings.Contains(script, pipelineEnvInputJSON) {
		t.Fatalf("expected rewritten script to include pipeline input env var, got %q", script)
	}

	scriptFallback, ok := rewriteSkillInstructionToShell("generate_pdf xxxy.pdf")
	if !ok {
		t.Fatalf("expected fallback rewrite to produce shell script")
	}
	if !strings.Contains(scriptFallback, pipelineEnvResultJSON) {
		t.Fatalf("expected fallback rewrite script to include pipeline result env var, got %q", scriptFallback)
	}

	scriptSaveOutput, ok := rewriteSkillInstructionToShell("save output to xxxy.pdf")
	if !ok {
		t.Fatalf("expected save-output rewrite to produce shell script")
	}
	if !strings.Contains(scriptSaveOutput, pipelineEnvResultJSON) {
		t.Fatalf("expected save-output rewrite script to include pipeline result env var, got %q", scriptSaveOutput)
	}
	if !strings.Contains(scriptSaveOutput, "json.loads") {
		t.Fatalf("expected save-output rewrite script to parse pipeline json, got %q", scriptSaveOutput)
	}
}

func TestShouldUsePipelineResultForPDFContent(t *testing.T) {
	if !shouldUsePipelineResultForPDFContent("quote result to") {
		t.Fatalf("expected quote-result placeholder to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("-") {
		t.Fatalf("expected placeholder '-' to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("a") {
		t.Fatalf("expected placeholder 'a' to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("output to xxxy.") {
		t.Fatalf("expected output placeholder to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("retrieved tesla stock quote") {
		t.Fatalf("expected retrieved quote placeholder to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("特斯拉的股票价格 保存 to file xxxy.") {
		t.Fatalf("expected stock-price request text to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("weather_result") {
		t.Fatalf("expected weather result placeholder to use pipeline content")
	}
	if !shouldUsePipelineResultForPDFContent("北京天气 保存到file zzz.") {
		t.Fatalf("expected weather save-to-file request text to use pipeline content")
	}
	if shouldUsePipelineResultForPDFContent("我们明天到公园玩") {
		t.Fatalf("did not expect natural explicit text content to use pipeline content")
	}
}

func TestNewSkillCommandRunSupportsTextFlag(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "longbridge",
			Intent:     "market_screen_v1",
			Confidence: 0.95,
			Arguments: map[string]any{
				"symbols": []any{"700.HK"},
			},
		}, nil
	})
	defer restoreRouter()

	app := NewAppContext()
	defer app.Close()

	cmd := newSkillCommand(app)
	cmd.SetArgs([]string{
		"run",
		"--text", "strategy screener request",
		"--symbols", "700.HK",
		"--dry-run",
		"--format", "json",
		"--skills-config", paths.SkillsConfig,
		"--llm-config", paths.LLMConfig,
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute skill run command failed: %v", err)
	}

	command, symbols, resultAny := app.ExecutionSnapshot()
	if command != "skill" {
		t.Fatalf("expected execution command skill, got %q", command)
	}
	if !slices.Equal(symbols, []string{"run"}) {
		t.Fatalf("expected execution symbols [run], got %v", symbols)
	}
	result, ok := resultAny.(skillRunResult)
	if !ok {
		t.Fatalf("expected skillRunResult, got %T", resultAny)
	}
	if !result.DryRun {
		t.Fatalf("expected dry_run=true")
	}
}

func writeLongbridgeSkillFixture(t *testing.T) skillRuntimePaths {
	t.Helper()

	root := t.TempDir()
	agentsRoot := filepath.Join(root, ".agents")
	skillFile := filepath.Join(agentsRoot, "skills", "longbridge", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatalf("mkdir skills root failed: %v", err)
	}
	if err := os.WriteFile(skillFile, []byte("# longbridge\nLongbridge developers skill"), 0o644); err != nil {
		t.Fatalf("write skill file failed: %v", err)
	}

	lockPath := filepath.Join(agentsRoot, ".skill-lock.json")
	lock := skillLockFile{
		Version: 1,
		Skills: map[string]skillLockFileEntry{
			"longbridge": {
				Source:      "longbridge/developers",
				SourceType:  "npx",
				SkillPath:   "skills/longbridge/SKILL.md",
				InstalledAt: "2026-04-15T00:00:00Z",
			},
		},
	}
	writeSkillTestJSON(t, lockPath, lock)

	return skillRuntimePaths{
		LockPath:       lockPath,
		SkillsRootPath: filepath.Join(agentsRoot, "skills"),
		SkillsConfig:   filepath.Join(root, "skills.json"),
		LLMConfig:      filepath.Join(root, "skills_llm.json"),
	}
}

func writeWeatherSkillFixture(t *testing.T) skillRuntimePaths {
	t.Helper()

	root := t.TempDir()
	skillsRoot := filepath.Join(root, "skills")
	skillFile := filepath.Join(skillsRoot, "weather", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatalf("mkdir weather skills root failed: %v", err)
	}
	content := strings.Join([]string{
		"---",
		"name: weather",
		"description: Get current weather and forecasts (no API key required).",
		"---",
		"",
		"# Weather",
		"```bash",
		`curl -s "wttr.in/London?format=3"`,
		"```",
	}, "\n")
	if err := os.WriteFile(skillFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write weather skill file failed: %v", err)
	}

	return skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: skillsRoot,
		SkillsConfig:   filepath.Join(root, "skills.json"),
		LLMConfig:      filepath.Join(root, "skills_llm.json"),
	}
}

func writeWeatherAndPDFSkillFixture(t *testing.T) skillRuntimePaths {
	t.Helper()

	root := t.TempDir()
	skillsRoot := filepath.Join(root, "skills")

	weatherFile := filepath.Join(skillsRoot, "weather", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(weatherFile), 0o755); err != nil {
		t.Fatalf("mkdir weather skills root failed: %v", err)
	}
	weatherContent := strings.Join([]string{
		"---",
		"name: weather",
		"description: Get current weather and forecasts.",
		"---",
		"",
		"# Weather",
		"```bash",
		`curl -s "wttr.in/Shanghai?format=3"`,
		"```",
	}, "\n")
	if err := os.WriteFile(weatherFile, []byte(weatherContent), 0o644); err != nil {
		t.Fatalf("write weather skill file failed: %v", err)
	}

	pdfFile := filepath.Join(skillsRoot, "pdf", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(pdfFile), 0o755); err != nil {
		t.Fatalf("mkdir pdf skills root failed: %v", err)
	}
	pdfContent := strings.Join([]string{
		"---",
		"name: pdf",
		"description: Create and edit PDF files.",
		"---",
		"",
		"# PDF",
		"```python",
		"from reportlab.pdfgen import canvas",
		"```",
	}, "\n")
	if err := os.WriteFile(pdfFile, []byte(pdfContent), 0o644); err != nil {
		t.Fatalf("write pdf skill file failed: %v", err)
	}

	return skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: skillsRoot,
		SkillsConfig:   filepath.Join(root, "skills.json"),
		LLMConfig:      filepath.Join(root, "skills_llm.json"),
	}
}

func writeLongbridgeAndPDFSkillFixture(t *testing.T) skillRuntimePaths {
	t.Helper()

	root := t.TempDir()
	skillsRoot := filepath.Join(root, "skills")

	longbridgeFile := filepath.Join(skillsRoot, "longbridge", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(longbridgeFile), 0o755); err != nil {
		t.Fatalf("mkdir longbridge skills root failed: %v", err)
	}
	longbridgeContent := strings.Join([]string{
		"---",
		"name: longbridge",
		"description: Longbridge market data and trading skill.",
		"---",
		"",
		"# Longbridge",
		"```bash",
		"longbridge quote TSLA.US --format json",
		"```",
	}, "\n")
	if err := os.WriteFile(longbridgeFile, []byte(longbridgeContent), 0o644); err != nil {
		t.Fatalf("write longbridge skill file failed: %v", err)
	}

	pdfFile := filepath.Join(skillsRoot, "pdf", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(pdfFile), 0o755); err != nil {
		t.Fatalf("mkdir pdf skills root failed: %v", err)
	}
	pdfContent := strings.Join([]string{
		"---",
		"name: pdf",
		"description: Create and edit PDF files.",
		"---",
		"",
		"# PDF",
		"```python",
		"from reportlab.pdfgen import canvas",
		"```",
	}, "\n")
	if err := os.WriteFile(pdfFile, []byte(pdfContent), 0o644); err != nil {
		t.Fatalf("write pdf skill file failed: %v", err)
	}

	writeSkillTestJSON(t, filepath.Join(root, "skills.json"), map[string]any{
		"skills": map[string]any{
			"longbridge": map[string]any{
				"allowed_actions": []string{"lb", "sys", "email", "task"},
			},
			"pdf": map[string]any{
				"allowed_actions": []string{"sys", "task"},
			},
		},
	})

	return skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: skillsRoot,
		SkillsConfig:   filepath.Join(root, "skills.json"),
		LLMConfig:      filepath.Join(root, "skills_llm.json"),
	}
}

func writePDFSkillFixture(t *testing.T) skillRuntimePaths {
	t.Helper()

	root := t.TempDir()
	skillsRoot := filepath.Join(root, "skills")
	skillFile := filepath.Join(skillsRoot, "pdf", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatalf("mkdir pdf skills root failed: %v", err)
	}
	content := strings.Join([]string{
		"---",
		"name: pdf",
		"description: Create and edit PDF files.",
		"---",
		"",
		"# PDF",
		"```python",
		"from reportlab.pdfgen import canvas",
		"```",
	}, "\n")
	if err := os.WriteFile(skillFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write pdf skill file failed: %v", err)
	}

	return skillRuntimePaths{
		LockPath:       filepath.Join(root, ".agents", ".skill-lock.json"),
		SkillsRootPath: skillsRoot,
		SkillsConfig:   filepath.Join(root, "skills.json"),
		LLMConfig:      filepath.Join(root, "skills_llm.json"),
	}
}

func writeSkillTestJSON(t *testing.T, path string, payload any) {
	t.Helper()
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal json failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir path failed: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write json file failed: %v", err)
	}
}

func setSkillRouteIntentRouterForTest(router skillIntentRouter) func() {
	old := skillRouteIntentWithLLM
	skillRouteIntentWithLLM = router
	return func() {
		skillRouteIntentWithLLM = old
	}
}

func setSkillRouterStartForRouteForTest(starter func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error)) func() {
	previous := startSkillRouterInBackgroundForRoute
	startSkillRouterInBackgroundForRoute = starter
	return func() {
		startSkillRouterInBackgroundForRoute = previous
	}
}

func setSkillLongbridgeRunnerForTest(runner skillLongbridgeRunner) func() {
	old := skillRunLongbridgeJSON
	skillRunLongbridgeJSON = runner
	return func() {
		skillRunLongbridgeJSON = old
	}
}

func setCLIExecutorForTest(executor CLIExecutor) func() {
	old := cliExecutor
	cliExecutor = executor
	return func() {
		cliExecutor = old
	}
}

func buildSkillCrossSeriesForTest(t *testing.T) []float64 {
	t.Helper()
	series := []float64{
		99.572, 99.383, 98.963, 98.994, 98.868, 98.785, 98.532, 98.163, 98.192, 97.931,
		97.736, 97.736, 97.722, 97.364, 97.099, 96.613, 96.173, 96.147, 95.878, 95.889,
		95.810, 95.845, 95.550, 95.185, 95.044, 94.700, 94.563, 94.066, 93.767, 93.293,
		93.369, 93.079, 93.060, 92.850, 92.533, 92.632, 92.444, 92.282, 92.244, 92.114,
		92.100, 91.817, 91.875, 91.571, 91.483, 91.562, 91.601, 91.118, 90.938, 90.869,
		90.573, 90.651, 90.665, 90.554, 90.331, 89.905, 89.448, 89.361, 88.890, 88.952,
		88.465, 88.474, 88.415, 88.277, 87.945, 87.873, 87.457, 87.463, 87.543, 87.584,
		87.219, 86.732, 86.763, 86.486, 86.191, 85.747, 85.737, 85.280, 84.788, 84.393,
		84.024, 83.549, 83.152, 84.072, 84.645, 85.543, 86.357, 86.999, 87.996, 88.917,
	}
	if !skillHasRecentMACDGoldenCross(series, defaultSkillMACDLookbackDays, 12, 26, 9) {
		t.Fatalf("test setup error: expected recent MACD golden cross")
	}
	return series
}

func buildSkillNoCrossSeriesForTest() []float64 {
	series := make([]float64, 0, 120)
	price := 120.0
	for i := 0; i < 120; i++ {
		price -= 0.25
		series = append(series, price)
	}
	return series
}

func buildKlineRowsForTest(closes []float64) []any {
	rows := make([]any, 0, len(closes))
	for _, closePrice := range closes {
		rows = append(rows, map[string]any{
			"close": closePrice,
		})
	}
	return rows
}

func containsAnyReason(reasons []string, needles ...string) bool {
	for _, reason := range reasons {
		for _, needle := range needles {
			if strings.Contains(strings.ToLower(reason), strings.ToLower(needle)) {
				return true
			}
		}
	}
	return false
}

func TestRouteSkillIntentWithSidecarSuccess(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "skills_catalog.json")
	if err := os.WriteFile(catalogPath, []byte(strings.Join([]string{
		`{`,
		`  "skills": [`,
		`    {`,
		`      "id": "pdf-pro",`,
		`      "capabilities": ["pdf"],`,
		`      "priority": 90,`,
		`      "source": "sample/pdf-pro",`,
		`      "source_url": "https://example.com/pdf-pro",`,
		`      "install_command_template": "npx skills add {source} -g -y"`,
		`    }`,
		`  ]`,
		`}`,
	}, "\n")), 0o644); err != nil {
		t.Fatalf("write skills catalog failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/route" {
			t.Fatalf("expected /v1/route, got %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if strings.TrimSpace(skillAnyToString(payload["text"])) == "" {
			t.Fatalf("expected non-empty text")
		}
		availableSkills, ok := payload["available_skills"].([]any)
		if !ok || len(availableSkills) != 1 {
			t.Fatalf("expected one available skill in request payload, got %#v", payload["available_skills"])
		}
		first, ok := availableSkills[0].(map[string]any)
		if !ok {
			t.Fatalf("expected available skill map payload, got %#v", availableSkills[0])
		}
		if strings.TrimSpace(skillAnyToString(first["id"])) != "pdf-pro" {
			t.Fatalf("unexpected available skill id payload: %#v", first)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill_id":   "longbridge",
			"intent":     "get_stock_price",
			"arguments":  map[string]any{"symbols": []string{"TSLA.US"}},
			"confidence": 0.96,
			"actions": []map[string]any{
				{"type": "lb", "command": "quote TSLA.US"},
			},
			"recommendations": []map[string]any{
				{
					"skill_id":        "pdf-pro",
					"reason":          "better_match_available",
					"capabilities":    []string{"pdf"},
					"install_source":  "sample/pdf-pro",
					"install_command": "npx skills add sample/pdf-pro -g -y",
				},
			},
			"reason": "unit_test",
			"trace": map[string]any{
				"candidate_scores": []map[string]any{
					{"skill_id": "longbridge", "score": 0.96},
				},
			},
		})
	}))
	defer server.Close()

	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        server.URL + "/v1/route",
			"timeout_seconds": 2,
		},
		"sidecar": map[string]any{
			"skills_catalog": catalogPath,
		},
	})

	restoreStarter := setSkillRouterStartForRouteForTest(func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		return skillRouterStartResult{Mode: "start", Status: "already_running", RuntimePath: "data/skill_router_runtime.json"}, nil
	})
	defer restoreStarter()

	decision, err := routeSkillIntentWithSidecar(context.Background(), "quote tsla", []skillInstalled{
		{ID: "longbridge", Enabled: true, AllowedActions: []string{"lb", "email", "task"}},
	}, cfgPath)
	if err != nil {
		t.Fatalf("routeSkillIntentWithSidecar failed: %v", err)
	}
	if decision.SkillID != "longbridge" || decision.Intent != "get_stock_price" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if len(decision.Actions) != 1 || decision.Actions[0].Type != "lb" {
		t.Fatalf("unexpected actions: %+v", decision.Actions)
	}
	if len(decision.Trace) == 0 {
		t.Fatalf("expected trace data in decision")
	}
	if len(decision.Recommendations) != 1 {
		t.Fatalf("expected one recommendation, got %+v", decision.Recommendations)
	}
	if decision.Recommendations[0].SkillID != "pdf-pro" || decision.Recommendations[0].Reason != "better_match_available" {
		t.Fatalf("unexpected recommendation payload: %+v", decision.Recommendations[0])
	}
}

func TestRouteSkillIntentWithSidecarMultiStepSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill_id":   "multi-skill",
			"intent":     "multi_skill_workflow",
			"arguments":  map[string]any{"location": "New York", "output_file": "skill_output.pdf"},
			"confidence": 0.95,
			"steps": []map[string]any{
				{
					"skill_id":  "weather",
					"intent":    "get_weather",
					"arguments": map[string]any{"location": "New York"},
					"actions": []map[string]any{
						{"type": "sys", "command": "sys -- /bin/sh -lc 'curl -s https://wttr.in/New+York?format=3 || true'"},
					},
				},
				{
					"skill_id":  "pdf",
					"intent":    "generate_pdf",
					"arguments": map[string]any{"output_file": "skill_output.pdf"},
					"actions": []map[string]any{
						{"type": "sys", "command": "sys -- python3 -c 'print(\"ok\")'"},
					},
				},
			},
			"reason": "multi_skill_weather_pdf",
		})
	}))
	defer server.Close()

	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        server.URL + "/v1/route",
			"timeout_seconds": 2,
		},
	})

	restoreStarter := setSkillRouterStartForRouteForTest(func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		return skillRouterStartResult{Mode: "start", Status: "already_running", RuntimePath: "data/skill_router_runtime.json"}, nil
	})
	defer restoreStarter()

	decision, err := routeSkillIntentWithSidecar(context.Background(), "纽约当前天气 保存到pdf文件", []skillInstalled{
		{ID: "weather", Enabled: true, AllowedActions: []string{"sys"}},
		{ID: "pdf", Enabled: true, AllowedActions: []string{"sys", "task"}},
	}, cfgPath)
	if err != nil {
		t.Fatalf("routeSkillIntentWithSidecar failed: %v", err)
	}
	if decision.SkillID != "multi-skill" {
		t.Fatalf("expected multi-skill id, got %+v", decision)
	}
	if len(decision.Steps) != 2 {
		t.Fatalf("expected two route steps, got %+v", decision.Steps)
	}
	if decision.Steps[0].SkillID != "weather" || decision.Steps[1].SkillID != "pdf" {
		t.Fatalf("unexpected route steps: %+v", decision.Steps)
	}
}

func TestRouteSkillIntentWithSidecarTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"skill_id": "longbridge"})
	}))
	defer server.Close()

	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        server.URL + "/v1/route",
			"timeout_seconds": 1,
		},
	})
	restoreStarter := setSkillRouterStartForRouteForTest(func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		return skillRouterStartResult{Mode: "start", Status: "already_running", RuntimePath: "data/skill_router_runtime.json"}, nil
	})
	defer restoreStarter()

	_, err := routeSkillIntentWithSidecar(context.Background(), "quote tsla", []skillInstalled{
		{ID: "longbridge", Enabled: true, AllowedActions: []string{"lb"}},
	}, cfgPath)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "failed") {
		t.Fatalf("expected sidecar request failure, got: %v", err)
	}
}

func TestRouteSkillIntentWithSidecarInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer server.Close()

	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        server.URL + "/v1/route",
			"timeout_seconds": 2,
		},
	})
	restoreStarter := setSkillRouterStartForRouteForTest(func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		return skillRouterStartResult{Mode: "start", Status: "already_running", RuntimePath: "data/skill_router_runtime.json"}, nil
	})
	defer restoreStarter()

	_, err := routeSkillIntentWithSidecar(context.Background(), "quote tsla", []skillInstalled{
		{ID: "longbridge", Enabled: true, AllowedActions: []string{"lb"}},
	}, cfgPath)
	if err == nil {
		t.Fatalf("expected invalid json error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "invalid json") {
		t.Fatalf("expected invalid json error, got: %v", err)
	}
}

func TestRouteSkillIntentWithSidecarUnreachable(t *testing.T) {
	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        "http://127.0.0.1:1/v1/route",
			"timeout_seconds": 1,
		},
	})
	_, err := routeSkillIntentWithSidecar(context.Background(), "quote tsla", []skillInstalled{
		{ID: "longbridge", Enabled: true, AllowedActions: []string{"lb"}},
	}, cfgPath)
	if err == nil {
		t.Fatalf("expected unreachable error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "failed") {
		t.Fatalf("expected request failed error, got: %v", err)
	}
}

func TestRouteSkillIntentWithSidecarAutoStartsLocalSidecar(t *testing.T) {
	startCalls := 0
	restoreStarter := setSkillRouterStartForRouteForTest(func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		startCalls++
		return skillRouterStartResult{
			Mode:        "start",
			Status:      "started",
			RuntimePath: "data/skill_router_runtime.json",
		}, nil
	})
	defer restoreStarter()

	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        "http://127.0.0.1:19090/v1/route",
			"timeout_seconds": 2,
		},
		"sidecar": map[string]any{
			"host":       "127.0.0.1",
			"port":       19090,
			"runtime":    "data/skill_router_runtime.json",
			"log":        "logs/skill_router.log",
			"script":     "scripts/skill_router_sidecar.py",
			"python_bin": "python3",
		},
	})

	_, err := routeSkillIntentWithSidecar(context.Background(), "quote tsla", []skillInstalled{
		{ID: "longbridge", Enabled: true, AllowedActions: []string{"lb"}},
	}, cfgPath)
	if err == nil {
		t.Fatalf("expected sidecar route request failure in test sandbox")
	}
	if startCalls != 1 {
		t.Fatalf("expected one ensure-sidecar start call, got %d", startCalls)
	}
}

func TestRouteSkillIntentWithOpenAIDelegatesToSidecar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/route" {
			t.Fatalf("expected /v1/route, got %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill_id":   "longbridge",
			"intent":     "get_stock_price",
			"arguments":  map[string]any{"symbols": []string{"TSLA.US"}},
			"confidence": 0.96,
			"actions": []map[string]any{
				{"type": "lb", "command": "quote TSLA.US"},
			},
			"reason": "unit_test_openai_delegate",
		})
	}))
	defer server.Close()

	cfgPath := writeSkillLLMV2ConfigForTest(t, map[string]any{
		"router": map[string]any{
			"endpoint":        server.URL + "/v1/route",
			"timeout_seconds": 2,
		},
	})
	restoreStarter := setSkillRouterStartForRouteForTest(func(llmConfigPath string, runtimePath string, logPath string, pythonBin string) (skillRouterStartResult, error) {
		return skillRouterStartResult{Mode: "start", Status: "already_running", RuntimePath: "data/skill_router_runtime.json"}, nil
	})
	defer restoreStarter()

	decision, err := routeSkillIntentWithOpenAI(context.Background(), "quote tsla", []skillInstalled{
		{ID: "longbridge", Enabled: true, AllowedActions: []string{"lb", "sys"}},
	}, cfgPath)
	if err != nil {
		t.Fatalf("routeSkillIntentWithOpenAI failed: %v", err)
	}
	if decision.SkillID != "longbridge" || decision.Intent != "get_stock_price" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if len(decision.Actions) != 1 || decision.Actions[0].Type != "lb" {
		t.Fatalf("unexpected actions: %+v", decision.Actions)
	}
}

func TestRunSkillCommandRejectsUnsupportedActionType(t *testing.T) {
	paths := writeWeatherSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "weather",
			Intent:     "get_weather",
			Confidence: 0.9,
			Actions: []skillAction{
				{Type: "unknown_action", Command: "echo hello"},
			},
		}, nil
	})
	defer restoreRouter()

	_, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "weather in shanghai",
		DryRun: true,
		Paths:  paths,
	})
	if err == nil {
		t.Fatalf("expected unsupported action type error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not allowed") {
		t.Fatalf("expected not allowed error, got: %v", err)
	}
}

func TestRunSkillCommandOutputUsesV2Schema(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{}, fmt.Errorf("router should not be called for explicit command")
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "longbridge quote TSLA.US --format json",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand failed: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal result failed: %v", err)
	}
	for _, key := range []string{"version", "mode", "route", "execution", "trace"} {
		if _, exists := payload[key]; !exists {
			t.Fatalf("expected v2 key %q in payload: %v", key, payload)
		}
	}
	if _, exists := payload["skill_id"]; exists {
		t.Fatalf("expected v1 field skill_id removed from top-level payload")
	}
}

func TestRunSkillCommandOutputIncludesRouteRecommendations(t *testing.T) {
	paths := writeWeatherSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "weather",
			Intent:     "get_weather",
			Confidence: 0.92,
			Actions: []skillAction{
				{Type: "sys", Command: "sys -- /bin/sh -lc 'echo weather-ok'"},
			},
			Recommendations: []skillRouteRecommendation{
				{
					SkillID:        "weather-pro",
					Reason:         "better_match_available",
					Capabilities:   []string{"weather"},
					InstallSource:  "sample/weather-pro",
					InstallCommand: "npx skills add sample/weather-pro -g -y",
				},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "上海天气",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand with recommendations failed: %v", err)
	}
	if len(result.Route.Recommendations) != 1 {
		t.Fatalf("expected one route recommendation, got %+v", result.Route.Recommendations)
	}
	if result.Route.Recommendations[0].SkillID != "weather-pro" {
		t.Fatalf("unexpected route recommendation payload: %+v", result.Route.Recommendations[0])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result failed: %v", err)
	}
	if !strings.Contains(string(encoded), "\"recommendations\"") {
		t.Fatalf("expected serialized payload to include recommendations, got: %s", string(encoded))
	}
}

func TestRunSkillCommandSkillInventoryQueryUsesRouter(t *testing.T) {
	paths := writeWeatherSkillFixture(t)
	routerCalled := false
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		routerCalled = true
		return skillRouteDecision{
			SkillID:    "weather",
			Intent:     "get_weather",
			Confidence: 0.9,
			Actions: []skillAction{
				{Type: "sys", Command: "sys -- /bin/sh -lc 'echo skills-summary'"},
			},
		}, nil
	})
	defer restoreRouter()

	result, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "当前支持的skills有几个",
		DryRun: true,
		Paths:  paths,
	})
	if err != nil {
		t.Fatalf("runSkillCommand skill inventory via router failed: %v", err)
	}
	if !routerCalled {
		t.Fatalf("expected router to be called for skill inventory query")
	}
	if result.Intent != "get_weather" {
		t.Fatalf("expected router intent to be used, got %q", result.Intent)
	}
}

func TestRunSkillCommandRejectsLowConfidenceWithoutLocalFallback(t *testing.T) {
	paths := writeLongbridgeSkillFixture(t)
	restoreRouter := setSkillRouteIntentRouterForTest(func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
		return skillRouteDecision{
			SkillID:    "find-skills",
			Intent:     "unknown",
			Confidence: 0.01,
			Arguments:  map[string]any{},
			Actions:    nil,
		}, nil
	})
	defer restoreRouter()

	_, err := runSkillCommand(context.Background(), skillRunOptions{
		Text:   "特斯拉股票价格 使用longbridge获取",
		DryRun: false,
		Paths:  paths,
	})
	if err == nil {
		t.Fatalf("expected low confidence rejection without local fallback")
	}
	if !strings.Contains(err.Error(), "below threshold") {
		t.Fatalf("expected low confidence error, got %v", err)
	}
}

func TestLoadSkillLLMConfigRejectsLegacySchema(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, "legacy_skills_llm.json")
	writeSkillTestJSON(t, legacyPath, map[string]any{
		"api_key":  "legacy-key",
		"base_url": "https://api.openai.com/v1",
		"model":    "gpt-4.1-mini",
	})
	_, err := loadSkillLLMConfig(legacyPath)
	if err == nil {
		t.Fatalf("expected legacy schema rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "legacy schema") {
		t.Fatalf("expected legacy schema message, got: %v", err)
	}
}

func writeSkillLLMV2ConfigForTest(t *testing.T, overrides map[string]any) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "skills_llm.json")
	payload := map[string]any{
		"router": map[string]any{
			"endpoint":        "http://127.0.0.1:19090/v1/route",
			"timeout_seconds": 2,
			"auth_token":      "",
		},
		"llm": map[string]any{
			"api_key":  "test-key",
			"base_url": "https://api.openai.com/v1",
			"model":    "gpt-4.1-mini",
		},
		"sidecar": map[string]any{
			"host":       "127.0.0.1",
			"port":       19090,
			"runtime":    "data/skill_router_runtime.json",
			"log":        "logs/skill_router.log",
			"script":     "scripts/skill_router_sidecar.py",
			"python_bin": "python3",
		},
	}
	for key, value := range overrides {
		payload[key] = value
	}
	writeSkillTestJSON(t, path, payload)
	return path
}
