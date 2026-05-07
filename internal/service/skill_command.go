package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultSkillMinConfidence          = 0.55
	defaultSkillMarketCapUSDMin        = 50_000_000_000.0
	defaultSkillPEMax                  = 25.0
	defaultSkillMACDLookbackDays       = 5
	defaultSkillMACDPeriod             = "day"
	defaultSkillKlineMinCount          = 80
	defaultSkillFallbackHKDToUSDFactor = 1.0 / 7.8
	skillExecutorMarketScreenV1        = "market_screen_v1"
	skillExecutorQuoteV1               = "quote_v1"
	skillExecutorActionsV1             = "actions_v1"
)

var (
	skillRouteIntentWithLLM      skillIntentRouter     = routeSkillIntentWithOpenAI
	skillRunLongbridgeJSON       skillLongbridgeRunner = runSkillLongbridgeJSON
	skillSymbolWithMarketPattern                       = regexp.MustCompile(`(?i)\b[A-Z0-9]{1,12}\.(US|HK|SH|SZ|SG|HAS)\b`)
	skillTickerPattern                                 = regexp.MustCompile(`\b[A-Z]{1,5}\b`)
	skillSymbolExactPattern                            = regexp.MustCompile(`^[A-Z0-9]{1,12}\.(US|HK|SH|SZ|SG|HAS)$`)
	skillTickerExactPattern                            = regexp.MustCompile(`^[A-Z]{1,5}$`)
	skillDefaultUSTechUniverse                         = []string{
		"AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "META.US", "NVDA.US", "TSLA.US", "NFLX.US", "AMD.US", "IBM.US",
	}
	skillDefaultHKTechUniverse = []string{
		"700.HK", "9988.HK", "9999.HK", "9618.HK", "1810.HK", "3690.HK", "1024.HK", "285.HK", "992.HK",
	}
)

type skillIntentRouter func(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error)

type skillLongbridgeRunner func(ctx context.Context, args []string) (any, string, error)

type skillRuntimePaths struct {
	LockPath       string
	SkillsRootPath string
	SkillsConfig   string
	LLMConfig      string
}

type skillRunOptions struct {
	Text          string
	Symbols       []string
	DryRun        bool
	Format        string
	MinConfidence float64
	Paths         skillRuntimePaths
}

type skillListOptions struct {
	Format string
	Paths  skillRuntimePaths
}

type skillValidateOptions struct {
	Format string
	Paths  skillRuntimePaths
}

type skillRunResult struct {
	Mode       string              `json:"mode"`
	SkillID    string              `json:"skill_id,omitempty"`
	Intent     string              `json:"intent,omitempty"`
	Confidence float64             `json:"confidence,omitempty"`
	Arguments  map[string]any      `json:"arguments,omitempty"`
	Actions    []skillActionResult `json:"actions,omitempty"`
	Result     any                 `json:"result,omitempty"`
	Errors     []string            `json:"errors,omitempty"`
	DryRun     bool                `json:"dry_run,omitempty"`
}

type skillListResult struct {
	Mode   string           `json:"mode"`
	Skills []skillInstalled `json:"skills"`
}

type skillValidateResult struct {
	Mode      string           `json:"mode"`
	Skills    []skillInstalled `json:"skills"`
	Warnings  []string         `json:"warnings,omitempty"`
	Valid     bool             `json:"valid"`
	CheckedAt string           `json:"checked_at"`
}

type skillActionResult struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
	Error      string `json:"error,omitempty"`
	OutputHint string `json:"output_hint,omitempty"`
}

type skillInstalled struct {
	ID             string   `json:"id"`
	Source         string   `json:"source,omitempty"`
	SourceType     string   `json:"source_type,omitempty"`
	SourceURL      string   `json:"source_url,omitempty"`
	InstalledAt    string   `json:"installed_at,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
	SkillPath      string   `json:"skill_path,omitempty"`
	SkillFile      string   `json:"skill_file,omitempty"`
	Description    string   `json:"description,omitempty"`
	CommandHints   []string `json:"command_hints,omitempty"`
	Enabled        bool     `json:"enabled"`
	Executor       string   `json:"executor,omitempty"`
	AllowedActions []string `json:"allowed_actions,omitempty"`
}

type skillRouteDecision struct {
	SkillID    string         `json:"skill_id"`
	Intent     string         `json:"intent,omitempty"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	Actions    []skillAction  `json:"actions,omitempty"`
	Reason     string         `json:"reason,omitempty"`
}

type skillAction struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
}

type skillExecutableAction struct {
	Type        string
	Command     string
	CLIArgs     []string
	CommandLine string
}

type skillLockFile struct {
	Version int                           `json:"version"`
	Skills  map[string]skillLockFileEntry `json:"skills"`
}

type skillLockFileEntry struct {
	Source          string `json:"source,omitempty"`
	SourceType      string `json:"sourceType,omitempty"`
	SourceURL       string `json:"sourceUrl,omitempty"`
	SkillPath       string `json:"skillPath,omitempty"`
	SkillFolderHash string `json:"skillFolderHash,omitempty"`
	InstalledAt     string `json:"installedAt,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
}

type skillConfigFile struct {
	MinConfidence float64                     `json:"min_confidence,omitempty"`
	SkillRoots    []string                    `json:"skill_roots,omitempty"`
	Skills        map[string]skillConfigEntry `json:"skills,omitempty"`
}

type skillConfigEntry struct {
	Enabled        *bool    `json:"enabled,omitempty"`
	Executor       string   `json:"executor,omitempty"`
	AllowedActions []string `json:"allowed_actions,omitempty"`
	Description    string   `json:"description,omitempty"`
}

type skillLLMConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model,omitempty"`
}

type skillMarketScreenRequest struct {
	Symbols          []string
	MarketCapUSDMin  float64
	PEMax            float64
	MACDLookbackDays int
	MACDPeriod       string
}

type skillMarketScreenResult struct {
	Criteria      map[string]any               `json:"criteria"`
	Matched       []skillMarketScreenMatch     `json:"matched"`
	Rejected      []skillMarketScreenRejection `json:"rejected"`
	Stats         map[string]any               `json:"stats"`
	ExchangeRates map[string]float64           `json:"exchange_rates"`
}

type skillMarketScreenMatch struct {
	Symbol          string  `json:"symbol"`
	PE              float64 `json:"pe"`
	MarketCapUSD    float64 `json:"market_cap_usd"`
	MACDGoldenCross bool    `json:"macd_golden_cross"`
}

type skillMarketScreenRejection struct {
	Symbol  string   `json:"symbol"`
	Reasons []string `json:"reasons"`
}

type skillQuoteResult struct {
	Symbols []string        `json:"symbols"`
	Quotes  []skillQuoteRow `json:"quotes"`
	Stats   map[string]any  `json:"stats"`
}

type skillQuoteRow struct {
	Symbol    string `json:"symbol"`
	Last      any    `json:"last,omitempty"`
	PrevClose any    `json:"prev_close,omitempty"`
	Open      any    `json:"open,omitempty"`
	High      any    `json:"high,omitempty"`
	Low       any    `json:"low,omitempty"`
	Volume    any    `json:"volume,omitempty"`
	Turnover  any    `json:"turnover,omitempty"`
	Status    any    `json:"status,omitempty"`
}

type pdfExtractTextOptions struct {
	InputPath      string
	OutputPath     string
	PreserveLayout bool
	FirstPage      int
	LastPage       int
}

func newSkillCommand(app *AppContext) *cobra.Command {
	var (
		textInput        string
		llmConfigPath    string
		skillsConfigPath string
		skillsRootPath   string
		minConfidence    float64
		outputFormat     string
		symbolsText      string
		dryRun           bool
	)

	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Run natural-language skills from locally installed skill packs",
	}

	runCmd := &cobra.Command{
		Use:   "run --text \"<natural-language>\" [--symbols \"<csv>\"]",
		Short: "Route natural language to a local skill and execute it",
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.TrimSpace(textInput)
			if text == "" {
				text = strings.TrimSpace(strings.Join(args, " "))
			}
			if text == "" {
				return fmt.Errorf("missing skill text; usage: skill run --text \"...\" --symbols \"...\"")
			}
			symbols := parseSkillSymbolsCSV(symbolsText)
			opts := skillRunOptions{
				Text:          text,
				Symbols:       symbols,
				DryRun:        dryRun,
				Format:        outputFormat,
				MinConfidence: minConfidence,
				Paths: skillRuntimePaths{
					LockPath:       defaultSkillLockPath(),
					SkillsRootPath: strings.TrimSpace(skillsRootPath),
					SkillsConfig:   strings.TrimSpace(skillsConfigPath),
					LLMConfig:      strings.TrimSpace(llmConfigPath),
				},
			}
			app.SetExecution("skill", []string{"run"})
			result, err := runSkillCommand(cmd.Context(), opts)
			app.SetResult(result)
			printSkillRunResult(result, outputFormat)
			return err
		},
	}
	runCmd.Flags().StringVar(&textInput, "text", "", "Natural language request to route into an installed skill")
	runCmd.Flags().StringVar(&llmConfigPath, "llm-config", defaultSkillsLLMConfigPath(), "Path to skills LLM config (OpenAI-compatible)")
	runCmd.Flags().StringVar(&skillsConfigPath, "skills-config", defaultSkillsConfigPath(), "Path to optional skills overrides config")
	runCmd.Flags().StringVar(&skillsRootPath, "skills-root", "", "Optional root directory to discover local skills (overrides default roots)")
	runCmd.Flags().Float64Var(&minConfidence, "min-confidence", defaultSkillMinConfidence, "Minimum route confidence to execute")
	runCmd.Flags().StringVar(&symbolsText, "symbols", "", "Comma-separated symbol list, e.g. 700.HK,9988.HK,IBM.US")
	runCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Route and plan only; do not execute downstream actions")
	runCmd.Flags().StringVar(&outputFormat, "format", "table", "Output format: table|json")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List locally installed skills discovered from lock file",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := skillListOptions{
				Format: outputFormat,
				Paths: skillRuntimePaths{
					LockPath:       defaultSkillLockPath(),
					SkillsRootPath: strings.TrimSpace(skillsRootPath),
					SkillsConfig:   strings.TrimSpace(skillsConfigPath),
					LLMConfig:      strings.TrimSpace(llmConfigPath),
				},
			}
			app.SetExecution("skill", []string{"list"})
			result, err := listSkillCommand(cmd.Context(), opts)
			app.SetResult(result)
			printSkillListResult(result, outputFormat)
			return err
		},
	}
	listCmd.Flags().StringVar(&skillsConfigPath, "skills-config", defaultSkillsConfigPath(), "Path to optional skills overrides config")
	listCmd.Flags().StringVar(&llmConfigPath, "llm-config", defaultSkillsLLMConfigPath(), "Path to skills LLM config (for validation only)")
	listCmd.Flags().StringVar(&skillsRootPath, "skills-root", "", "Optional root directory to discover local skills (overrides default roots)")
	listCmd.Flags().StringVar(&outputFormat, "format", "table", "Output format: table|json")

	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate skill lock/config and discoverability",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := skillValidateOptions{
				Format: outputFormat,
				Paths: skillRuntimePaths{
					LockPath:       defaultSkillLockPath(),
					SkillsRootPath: strings.TrimSpace(skillsRootPath),
					SkillsConfig:   strings.TrimSpace(skillsConfigPath),
					LLMConfig:      strings.TrimSpace(llmConfigPath),
				},
			}
			app.SetExecution("skill", []string{"validate"})
			result, err := validateSkillCommand(cmd.Context(), opts)
			app.SetResult(result)
			printSkillValidateResult(result, outputFormat)
			return err
		},
	}
	validateCmd.Flags().StringVar(&skillsConfigPath, "skills-config", defaultSkillsConfigPath(), "Path to optional skills overrides config")
	validateCmd.Flags().StringVar(&llmConfigPath, "llm-config", defaultSkillsLLMConfigPath(), "Path to skills LLM config")
	validateCmd.Flags().StringVar(&skillsRootPath, "skills-root", "", "Optional root directory to discover local skills (overrides default roots)")
	validateCmd.Flags().StringVar(&outputFormat, "format", "table", "Output format: table|json")

	cmd.AddCommand(runCmd, listCmd, validateCmd)
	return cmd
}

func runSkillCommand(ctx context.Context, opts skillRunOptions) (skillRunResult, error) {
	result := skillRunResult{
		Mode:      "run",
		DryRun:    opts.DryRun,
		Arguments: map[string]any{},
	}
	installed, cfg, err := discoverInstalledSkills(opts.Paths)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}
	enabled := make([]skillInstalled, 0, len(installed))
	for _, item := range installed {
		if item.Enabled {
			enabled = append(enabled, item)
		}
	}
	if len(enabled) == 0 {
		err := fmt.Errorf("no enabled skills discovered; install with `npx skills add <source> -g -y` and check %s", opts.Paths.LockPath)
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}

	minConfidence := opts.MinConfidence
	if minConfidence <= 0 {
		minConfidence = cfg.MinConfidence
	}
	if minConfidence <= 0 {
		minConfidence = defaultSkillMinConfidence
	}
	if minConfidence > 1 {
		minConfidence = 1
	}
	result.Arguments["text"] = opts.Text
	result.Arguments["symbols"] = append([]string(nil), opts.Symbols...)

	decision, explicitMatched := routeSkillIntentByExplicitCommand(opts.Text, enabled)
	if !explicitMatched {
		if compositeDecision, compositeMatched := routeSkillIntentByCompositeText(opts.Text, opts.Symbols, enabled); compositeMatched {
			decision = compositeDecision
		} else {
			var routeErr error
			decision, routeErr = skillRouteIntentWithLLM(ctx, opts.Text, enabled, resolveSkillsLLMConfigPath(opts.Paths.LLMConfig))
			if routeErr != nil {
				result.Errors = append(result.Errors, routeErr.Error())
				return result, routeErr
			}
		}
	}
	if decision.Arguments == nil {
		decision.Arguments = map[string]any{}
	}
	result.SkillID = strings.TrimSpace(decision.SkillID)
	result.Intent = strings.TrimSpace(decision.Intent)
	result.Confidence = decision.Confidence
	result.Arguments["route"] = decision.Arguments

	if result.SkillID == "" {
		err := fmt.Errorf("skill router returned empty skill_id")
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}
	if decision.Confidence < minConfidence {
		err := fmt.Errorf("skill route confidence %.2f is below threshold %.2f", decision.Confidence, minConfidence)
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}

	selected, ok := findSkillByID(enabled, result.SkillID)
	if !ok {
		err := fmt.Errorf("routed skill %q is not installed/enabled", result.SkillID)
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}

	if err := validateSkillPlannedActions(selected, decision.Actions); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}

	if shouldExecuteExplicitSkillActions(result.Intent, decision.Actions) {
		actions, planned, planErr := buildSkillExecutableActions(selected, decision.Actions)
		if planErr != nil {
			result.Errors = append(result.Errors, planErr.Error())
			return result, planErr
		}
		if opts.DryRun {
			result.Actions = planned
			result.Result = map[string]any{
				"planned": true,
				"steps":   planned,
			}
			return result, nil
		}
		runResult, executed, runErr := executeSkillExecutableActions(ctx, actions)
		result.Actions = executed
		result.Result = runResult
		if runErr != nil {
			result.Errors = append(result.Errors, runErr.Error())
			return result, runErr
		}
		return result, nil
	}

	selectedExecutor := strings.TrimSpace(selected.Executor)
	if shouldUseSkillQuoteIntent(result.Intent) {
		if !skillAllowsAction(selected, "lb") {
			err := fmt.Errorf("skill %q executor %q requires action %q", selected.ID, skillExecutorQuoteV1, "lb")
			result.Errors = append(result.Errors, err.Error())
			return result, err
		}
		quoteSymbols := resolveSkillSymbols(opts.Symbols, decision.Arguments, opts.Text)
		if len(quoteSymbols) == 0 {
			err := fmt.Errorf("no symbols provided; use --symbols or include symbols in routed arguments")
			result.Errors = append(result.Errors, err.Error())
			return result, err
		}
		planned := planSkillQuoteActions(quoteSymbols)
		if opts.DryRun {
			result.Actions = planned
			result.Result = map[string]any{
				"planned": true,
				"symbols": quoteSymbols,
			}
			return result, nil
		}
		quoteResult, actions, quoteErr := executeSkillQuoteV1(ctx, quoteSymbols)
		result.Actions = actions
		result.Result = quoteResult
		if quoteErr != nil {
			result.Errors = append(result.Errors, quoteErr.Error())
			return result, quoteErr
		}
		return result, nil
	}

	switch selectedExecutor {
	case skillExecutorMarketScreenV1:
		req, reqErr := buildSkillMarketScreenRequest(opts, decision)
		if reqErr != nil {
			result.Errors = append(result.Errors, reqErr.Error())
			return result, reqErr
		}
		planned := planSkillMarketScreenActions(req)
		if opts.DryRun {
			result.Actions = planned
			result.Result = map[string]any{
				"planned": true,
				"criteria": map[string]any{
					"market_cap_usd_min": req.MarketCapUSDMin,
					"pe_max":             req.PEMax,
					"macd_period":        req.MACDPeriod,
					"macd_lookback_days": req.MACDLookbackDays,
					"symbols":            req.Symbols,
				},
			}
			return result, nil
		}
		if !skillAllowsAction(selected, "lb") {
			err := fmt.Errorf("skill %q executor %q requires action %q", selected.ID, selected.Executor, "lb")
			result.Errors = append(result.Errors, err.Error())
			return result, err
		}
		screenResult, actions, runErr := executeSkillMarketScreenV1(ctx, req)
		result.Actions = actions
		result.Result = screenResult
		if runErr != nil {
			result.Errors = append(result.Errors, runErr.Error())
			return result, runErr
		}
		return result, nil
	case "", skillExecutorActionsV1:
		primaryActions := append([]skillAction(nil), decision.Actions...)
		pdfFallbackActions := buildPDFLocalFallbackActions(selected, result.Intent, opts.Text, decision.Arguments)
		usePDFFallback := shouldUsePDFLocalFallback(selected, primaryActions, pdfFallbackActions)
		if usePDFFallback && pdfActionsRequireDaemon(primaryActions) {
			// `task` actions are daemon scheduler operations and cannot run in this execution path.
			primaryActions = append([]skillAction(nil), pdfFallbackActions...)
			usePDFFallback = false
		}

		actions, planned, planErr := buildSkillExecutableActions(selected, primaryActions)
		if planErr != nil && usePDFFallback {
			fallbackActions, fallbackPlanned, fallbackPlanErr := buildSkillExecutableActions(selected, pdfFallbackActions)
			if fallbackPlanErr == nil {
				actions = fallbackActions
				planned = fallbackPlanned
				planErr = nil
				usePDFFallback = false
			}
		}
		if planErr != nil {
			result.Errors = append(result.Errors, planErr.Error())
			return result, planErr
		}
		if opts.DryRun {
			result.Actions = planned
			result.Result = map[string]any{
				"planned": true,
				"steps":   planned,
			}
			return result, nil
		}
		runResult, executed, runErr := executeSkillExecutableActions(ctx, actions)
		if runErr == nil {
			result.Actions = executed
			result.Result = runResult
			return result, nil
		}
		if !usePDFFallback {
			result.Actions = executed
			result.Result = runResult
			result.Errors = append(result.Errors, runErr.Error())
			return result, runErr
		}

		fallbackActions, _, fallbackPlanErr := buildSkillExecutableActions(selected, pdfFallbackActions)
		if fallbackPlanErr != nil {
			result.Actions = executed
			result.Result = runResult
			result.Errors = append(result.Errors, runErr.Error(), fallbackPlanErr.Error())
			return result, fmt.Errorf("primary skill action failed: %v; pdf fallback planning failed: %w", runErr, fallbackPlanErr)
		}
		fallbackResult, fallbackExecuted, fallbackErr := executeSkillExecutableActions(ctx, fallbackActions)
		for i := range executed {
			if strings.TrimSpace(executed[i].Note) == "" {
				executed[i].Note = "primary"
			}
		}
		for i := range fallbackExecuted {
			if strings.TrimSpace(fallbackExecuted[i].Note) == "" {
				fallbackExecuted[i].Note = "pdf_local_fallback"
			}
		}
		result.Actions = append(executed, fallbackExecuted...)
		result.Result = fallbackResult
		if fallbackErr != nil {
			result.Errors = append(result.Errors, runErr.Error(), fallbackErr.Error())
			return result, fmt.Errorf("primary skill action failed: %v; pdf fallback failed: %w", runErr, fallbackErr)
		}
		return result, nil
	default:
		err := fmt.Errorf("skill %q uses unsupported executor %q", selected.ID, selected.Executor)
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}
}

func listSkillCommand(ctx context.Context, opts skillListOptions) (skillListResult, error) {
	_ = ctx
	items, _, err := discoverInstalledSkills(opts.Paths)
	if err != nil {
		return skillListResult{Mode: "list"}, err
	}
	return skillListResult{
		Mode:   "list",
		Skills: items,
	}, nil
}

func validateSkillCommand(ctx context.Context, opts skillValidateOptions) (skillValidateResult, error) {
	_ = ctx
	out := skillValidateResult{
		Mode:      "validate",
		CheckedAt: time.Now().Format(time.RFC3339),
		Valid:     true,
	}

	items, _, err := discoverInstalledSkills(opts.Paths)
	if err != nil {
		out.Valid = false
		out.Warnings = append(out.Warnings, err.Error())
		return out, err
	}
	out.Skills = items
	if len(items) == 0 {
		out.Valid = false
		out.Warnings = append(out.Warnings, "no installed skills discovered from lock file or configured skill roots")
	}

	if llmCfgErr := validateSkillsLLMConfigPath(resolveSkillsLLMConfigPath(opts.Paths.LLMConfig)); llmCfgErr != nil {
		out.Warnings = append(out.Warnings, llmCfgErr.Error())
	}
	return out, nil
}

func discoverInstalledSkills(paths skillRuntimePaths) ([]skillInstalled, skillConfigFile, error) {
	lockPath := strings.TrimSpace(paths.LockPath)
	if lockPath == "" {
		lockPath = defaultSkillLockPath()
	}
	cfgPath := resolveSkillsConfigPath(paths.SkillsConfig)

	cfg, err := loadSkillConfigFile(cfgPath)
	if err != nil {
		return nil, skillConfigFile{}, err
	}
	rootCandidates := resolveSkillRootCandidates(paths, cfg)
	skillMap := make(map[string]skillInstalled)

	lock, lockErr := loadSkillLockFile(lockPath)
	if lockErr != nil {
		if !errors.Is(lockErr, os.ErrNotExist) {
			return nil, cfg, lockErr
		}
	} else {
		agentsRoot := filepath.Clean(filepath.Dir(lockPath))
		lockIDs := make([]string, 0, len(lock.Skills))
		for id := range lock.Skills {
			lockIDs = append(lockIDs, id)
		}
		sort.Strings(lockIDs)

		for _, id := range lockIDs {
			record := lock.Skills[id]
			relativeSkillPath := strings.TrimSpace(record.SkillPath)
			if relativeSkillPath == "" {
				relativeSkillPath = filepath.ToSlash(filepath.Join("skills", id, "SKILL.md"))
			}

			skillFilePath, resolveErr := resolveSkillPathFromLock(agentsRoot, relativeSkillPath)
			if resolveErr != nil {
				return nil, cfg, fmt.Errorf("resolve skill %q path %q failed: %w", id, relativeSkillPath, resolveErr)
			}
			if len(rootCandidates) > 0 && !pathWithinAnyRoot(skillFilePath, rootCandidates) {
				return nil, cfg, fmt.Errorf("skill %q file %s is outside configured skill roots %v", id, filepath.Clean(skillFilePath), rootCandidates)
			}

			item, buildErr := buildSkillInstalledItem(id, relativeSkillPath, skillFilePath, cfg.Skills[id], record.Source, record.SourceType, record.SourceURL, record.InstalledAt, record.UpdatedAt)
			if buildErr != nil {
				return nil, cfg, buildErr
			}
			skillMap[id] = item
		}
	}

	scannedItems, scanErr := scanSkillRoots(rootCandidates, cfg.Skills, skillMap)
	if scanErr != nil {
		return nil, cfg, scanErr
	}
	for id, item := range scannedItems {
		skillMap[id] = item
	}

	if len(skillMap) == 0 {
		if lockErr != nil {
			return nil, cfg, lockErr
		}
		return []skillInstalled{}, cfg, nil
	}

	ids := make([]string, 0, len(skillMap))
	for id := range skillMap {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	items := make([]skillInstalled, 0, len(ids))
	for _, id := range ids {
		items = append(items, skillMap[id])
	}
	return items, cfg, nil
}

func resolveSkillRootCandidates(paths skillRuntimePaths, cfg skillConfigFile) []string {
	candidates := make([]string, 0, 4)
	explicitRoot := strings.TrimSpace(paths.SkillsRootPath)
	if explicitRoot != "" {
		candidates = append(candidates, explicitRoot)
	} else {
		candidates = append(candidates, defaultSkillsRootPath())
		candidates = append(candidates, defaultClawhubSkillsRootPath())
	}
	candidates = append(candidates, cfg.SkillRoots...)

	out := make([]string, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, one := range candidates {
		cleaned := filepath.Clean(strings.TrimSpace(one))
		if cleaned == "" || cleaned == "." {
			continue
		}
		if _, exists := seen[cleaned]; exists {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out
}

func defaultClawhubSkillsRootPath() string {
	home := strings.TrimSpace(getEnvFirst("HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = strings.TrimSpace(h)
		}
	}
	return filepath.Join(home, "skills")
}

func pathWithinAnyRoot(path string, roots []string) bool {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	for _, root := range roots {
		cleanRoot := filepath.Clean(strings.TrimSpace(root))
		if cleanRoot == "" {
			continue
		}
		if cleanPath == cleanRoot {
			return true
		}
		prefix := cleanRoot + string(os.PathSeparator)
		if strings.HasPrefix(cleanPath, prefix) {
			return true
		}
	}
	return false
}

func buildSkillInstalledItem(
	id string,
	skillPath string,
	skillFilePath string,
	override skillConfigEntry,
	source string,
	sourceType string,
	sourceURL string,
	installedAt string,
	updatedAt string,
) (skillInstalled, error) {
	skillFileText, readErr := os.ReadFile(skillFilePath)
	if readErr != nil {
		return skillInstalled{}, fmt.Errorf("read skill %q file %s failed: %w", id, skillFilePath, readErr)
	}

	allowedActions := normalizeSkillAllowedActions(override.AllowedActions)
	if len(allowedActions) == 0 {
		if strings.EqualFold(strings.TrimSpace(id), "longbridge") {
			allowedActions = []string{"email", "lb", "task"}
		} else {
			allowedActions = []string{"email", "lb", "sys", "task"}
		}
	}

	enabled := true
	if override.Enabled != nil {
		enabled = *override.Enabled
	}
	executor := strings.TrimSpace(override.Executor)
	if executor == "" && strings.EqualFold(strings.TrimSpace(id), "longbridge") {
		executor = skillExecutorMarketScreenV1
	}
	description := strings.TrimSpace(override.Description)
	if description == "" {
		description = extractSkillDescription(string(skillFileText))
	}
	commandHints := extractSkillCommandHints(string(skillFileText))

	return skillInstalled{
		ID:             strings.TrimSpace(id),
		Source:         strings.TrimSpace(source),
		SourceType:     strings.TrimSpace(sourceType),
		SourceURL:      strings.TrimSpace(sourceURL),
		InstalledAt:    strings.TrimSpace(installedAt),
		UpdatedAt:      strings.TrimSpace(updatedAt),
		SkillPath:      strings.TrimSpace(skillPath),
		SkillFile:      filepath.Clean(skillFilePath),
		Description:    description,
		CommandHints:   commandHints,
		Enabled:        enabled,
		Executor:       executor,
		AllowedActions: allowedActions,
	}, nil
}

func scanSkillRoots(roots []string, overrides map[string]skillConfigEntry, existing map[string]skillInstalled) (map[string]skillInstalled, error) {
	out := make(map[string]skillInstalled)
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" || root == "." {
			continue
		}

		entries, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read skills root %s failed: %w", root, err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			id := strings.TrimSpace(entry.Name())
			if id == "" {
				continue
			}
			if _, exists := existing[id]; exists {
				continue
			}
			if _, exists := out[id]; exists {
				continue
			}

			skillDir := filepath.Join(root, id)
			skillFilePath, ok := resolveSkillFileInDirectory(skillDir)
			if !ok {
				continue
			}

			relativePath := filepath.ToSlash(filepath.Join(id, filepath.Base(skillFilePath)))
			item, buildErr := buildSkillInstalledItem(id, relativePath, skillFilePath, overrides[id], "local/"+id, "directory", "", "", "")
			if buildErr != nil {
				return nil, buildErr
			}
			out[id] = item
		}
	}
	return out, nil
}

func resolveSkillFileInDirectory(dir string) (string, bool) {
	candidates := []string{
		filepath.Join(dir, "SKILL.md"),
		filepath.Join(dir, "skill.md"),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		return candidate, true
	}
	return "", false
}

func buildSkillMarketScreenRequest(opts skillRunOptions, decision skillRouteDecision) (skillMarketScreenRequest, error) {
	req := skillMarketScreenRequest{
		Symbols:          append([]string(nil), opts.Symbols...),
		MarketCapUSDMin:  defaultSkillMarketCapUSDMin,
		PEMax:            defaultSkillPEMax,
		MACDLookbackDays: defaultSkillMACDLookbackDays,
		MACDPeriod:       defaultSkillMACDPeriod,
	}

	req.Symbols = resolveSkillSymbols(req.Symbols, decision.Arguments, opts.Text)
	if len(req.Symbols) == 0 {
		req.Symbols = inferSkillUniverseSymbols(decision.Arguments, opts.Text)
	}
	if len(req.Symbols) == 0 {
		return req, fmt.Errorf("no symbols provided; use --symbols or include symbols in routed arguments")
	}

	if value, ok := decision.Arguments["market_cap_usd_min"]; ok {
		if parsed, parsedOK := skillAnyToFloat64(value); parsedOK && parsed > 0 {
			req.MarketCapUSDMin = parsed
		}
	}
	if value, ok := decision.Arguments["pe_max"]; ok {
		if parsed, parsedOK := skillAnyToFloat64(value); parsedOK && parsed > 0 {
			req.PEMax = parsed
		}
	}
	if value, ok := decision.Arguments["macd_lookback_days"]; ok {
		if parsed, parsedOK := skillAnyToInt(value); parsedOK && parsed > 0 {
			req.MACDLookbackDays = parsed
		}
	}
	if value, ok := decision.Arguments["macd_period"]; ok {
		if parsed := strings.TrimSpace(skillAnyToString(value)); parsed != "" {
			req.MACDPeriod = parsed
		}
	}
	req.Symbols = uniqueUpperSymbols(req.Symbols)
	return req, nil
}

func shouldUseSkillQuoteIntent(intent string) bool {
	normalized := strings.ToLower(strings.TrimSpace(intent))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "quote") || strings.Contains(normalized, "price") {
		return true
	}
	if strings.Contains(normalized, "stock_price") || strings.Contains(normalized, "行情") || strings.Contains(normalized, "股价") {
		return true
	}
	return false
}

func resolveSkillSymbols(initial []string, routeArguments map[string]any, text string) []string {
	symbols := uniqueUpperSymbols(initial)
	if len(symbols) > 0 {
		return symbols
	}
	symbols = parseSymbolsFromDecision(routeArguments)
	if len(symbols) > 0 {
		return symbols
	}
	return inferSkillSymbolsFromText(text)
}

func inferSkillUniverseSymbols(routeArguments map[string]any, text string) []string {
	normalizedText := strings.ToLower(strings.TrimSpace(text))
	regionUS := false
	regionHK := false
	sectorTech := false

	for _, token := range collectSkillStringTokens(routeArguments) {
		normalized := strings.ToLower(strings.TrimSpace(token))
		switch normalized {
		case "us", "usa", "美股", "美国", "united states":
			regionUS = true
		case "hk", "hkg", "hong kong", "港股", "香港":
			regionHK = true
		case "tech", "technology", "科技", "互联网", "internet":
			sectorTech = true
		}
	}

	if strings.Contains(normalizedText, "us") || strings.Contains(normalizedText, "美股") || strings.Contains(normalizedText, "美国") {
		regionUS = true
	}
	if strings.Contains(normalizedText, "hk") || strings.Contains(normalizedText, "港股") || strings.Contains(normalizedText, "香港") {
		regionHK = true
	}
	if strings.Contains(normalizedText, "tech") || strings.Contains(normalizedText, "technology") || strings.Contains(normalizedText, "科技") || strings.Contains(normalizedText, "互联网") {
		sectorTech = true
	}

	if !regionUS && !regionHK {
		return nil
	}
	if !sectorTech {
		return nil
	}

	out := make([]string, 0, 20)
	if regionUS {
		out = append(out, skillDefaultUSTechUniverse...)
	}
	if regionHK {
		out = append(out, skillDefaultHKTechUniverse...)
	}
	return uniqueUpperSymbols(out)
}

func collectSkillStringTokens(raw any) []string {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return nil
		}
		return []string{text}
	case []any:
		out := make([]string, 0, len(value))
		for _, one := range value {
			out = append(out, collectSkillStringTokens(one)...)
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(value))
		for _, nested := range value {
			out = append(out, collectSkillStringTokens(nested)...)
		}
		return out
	default:
		text := strings.TrimSpace(skillAnyToString(value))
		if text == "" || text == "<nil>" {
			return nil
		}
		return []string{text}
	}
}

func planSkillQuoteActions(symbols []string) []skillActionResult {
	if len(symbols) == 0 {
		return nil
	}
	args := append([]string{"quote"}, append(append([]string(nil), symbols...), "--format", "json")...)
	return []skillActionResult{
		{
			Type:    "lb",
			Command: FormatCommandLine(append([]string{longbridgeCLIProgram}, args...)),
			Status:  "planned",
		},
	}
}

func executeSkillQuoteV1(ctx context.Context, symbols []string) (skillQuoteResult, []skillActionResult, error) {
	out := skillQuoteResult{
		Symbols: append([]string(nil), symbols...),
		Quotes:  []skillQuoteRow{},
	}
	actions := planSkillQuoteActions(symbols)
	if len(actions) == 0 {
		return out, actions, fmt.Errorf("no symbols provided for quote")
	}

	args := append([]string{"quote"}, append(append([]string(nil), symbols...), "--format", "json")...)
	raw, commandLine, runErr := skillRunLongbridgeJSON(ctx, args)
	if runErr != nil {
		actions[0].Status = "failed"
		actions[0].Command = commandLine
		actions[0].Error = runErr.Error()
		return out, actions, fmt.Errorf("quote failed: %w", runErr)
	}
	actions[0].Status = "success"
	actions[0].Command = commandLine

	rows := collectSkillObjectRows(raw)
	for _, row := range rows {
		symbol := strings.ToUpper(strings.TrimSpace(skillAnyToString(firstExistingValue(row, "symbol", "code", "ticker"))))
		if symbol == "" {
			continue
		}
		out.Quotes = append(out.Quotes, skillQuoteRow{
			Symbol:    symbol,
			Last:      firstExistingValue(row, "last", "last_done"),
			PrevClose: firstExistingValue(row, "prev_close"),
			Open:      firstExistingValue(row, "open"),
			High:      firstExistingValue(row, "high"),
			Low:       firstExistingValue(row, "low"),
			Volume:    firstExistingValue(row, "volume"),
			Turnover:  firstExistingValue(row, "turnover"),
			Status:    firstExistingValue(row, "status", "trade_status"),
		})
	}
	out.Stats = map[string]any{
		"requested": len(symbols),
		"returned":  len(out.Quotes),
	}
	return out, actions, nil
}

func planSkillMarketScreenActions(req skillMarketScreenRequest) []skillActionResult {
	actions := make([]skillActionResult, 0, len(req.Symbols)+2)
	calcArgs := append([]string{"calc-index"}, append(append([]string(nil), req.Symbols...), "--fields", "total_market_value,pe", "--format", "json")...)
	actions = append(actions, skillActionResult{
		Type:    "lb",
		Command: FormatCommandLine(append([]string{longbridgeCLIProgram}, calcArgs...)),
		Status:  "planned",
	})
	actions = append(actions, skillActionResult{
		Type:    "lb",
		Command: FormatCommandLine([]string{longbridgeCLIProgram, "exchange-rate", "--format", "json"}),
		Status:  "planned",
	})
	count := req.MACDLookbackDays + 50
	if count < defaultSkillKlineMinCount {
		count = defaultSkillKlineMinCount
	}
	for _, symbol := range req.Symbols {
		klineArgs := []string{"kline", symbol, "--period", req.MACDPeriod, "--count", strconv.Itoa(count), "--format", "json"}
		actions = append(actions, skillActionResult{
			Type:    "lb",
			Command: FormatCommandLine(append([]string{longbridgeCLIProgram}, klineArgs...)),
			Status:  "planned",
		})
	}
	return actions
}

func executeSkillMarketScreenV1(ctx context.Context, req skillMarketScreenRequest) (skillMarketScreenResult, []skillActionResult, error) {
	out := skillMarketScreenResult{
		Criteria: map[string]any{
			"market_cap_usd_min": req.MarketCapUSDMin,
			"pe_max":             req.PEMax,
			"macd_period":        req.MACDPeriod,
			"macd_lookback_days": req.MACDLookbackDays,
			"symbols":            req.Symbols,
		},
		Matched:       []skillMarketScreenMatch{},
		Rejected:      []skillMarketScreenRejection{},
		ExchangeRates: map[string]float64{"USD": 1},
	}
	actions := make([]skillActionResult, 0, len(req.Symbols)+2)

	calcArgs := append([]string{"calc-index"}, append(append([]string(nil), req.Symbols...), "--fields", "total_market_value,pe", "--format", "json")...)
	calcRaw, calcCommand, calcErr := skillRunLongbridgeJSON(ctx, calcArgs)
	calcAction := skillActionResult{
		Type:    "lb",
		Command: calcCommand,
		Status:  "success",
	}
	if calcErr != nil {
		calcAction.Status = "failed"
		calcAction.Error = calcErr.Error()
		actions = append(actions, calcAction)
		return out, actions, fmt.Errorf("calc-index failed: %w", calcErr)
	}
	actions = append(actions, calcAction)

	exchangeRaw, exchangeCommand, exchangeErr := skillRunLongbridgeJSON(ctx, []string{"exchange-rate", "--format", "json"})
	exchangeAction := skillActionResult{
		Type:    "lb",
		Command: exchangeCommand,
		Status:  "success",
	}
	if exchangeErr != nil {
		exchangeAction.Status = "failed"
		exchangeAction.Error = exchangeErr.Error()
		actions = append(actions, exchangeAction)
		return out, actions, fmt.Errorf("exchange-rate failed: %w", exchangeErr)
	}
	actions = append(actions, exchangeAction)

	rates := parseSkillToUSDRates(exchangeRaw)
	if _, ok := rates["USD"]; !ok {
		rates["USD"] = 1
	}
	if _, ok := rates["HKD"]; !ok {
		rates["HKD"] = defaultSkillFallbackHKDToUSDFactor
	}
	out.ExchangeRates = rates

	calcRows := parseSkillCalcIndexRows(calcRaw)
	calcBySymbol := make(map[string]skillCalcIndexRow, len(calcRows))
	for _, row := range calcRows {
		if strings.TrimSpace(row.Symbol) == "" {
			continue
		}
		calcBySymbol[strings.ToUpper(strings.TrimSpace(row.Symbol))] = row
	}

	count := req.MACDLookbackDays + 50
	if count < defaultSkillKlineMinCount {
		count = defaultSkillKlineMinCount
	}

	for _, symbol := range req.Symbols {
		upperSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		reasons := make([]string, 0, 3)

		row, exists := calcBySymbol[upperSymbol]
		if !exists {
			reasons = append(reasons, "calc-index missing symbol")
		}

		marketCapUSD := 0.0
		pe := 0.0
		if exists {
			pe = row.PE
			currency := strings.TrimSpace(row.Currency)
			if currency == "" {
				currency = inferSkillCurrencyFromSymbol(upperSymbol)
			}
			factor, factorOK := rates[strings.ToUpper(currency)]
			if !factorOK || factor <= 0 {
				factor = 1
			}
			marketCapUSD = row.TotalMarketValue * factor
			if marketCapUSD <= req.MarketCapUSDMin {
				reasons = append(reasons, fmt.Sprintf("market_cap_usd %.2f <= %.2f", marketCapUSD, req.MarketCapUSDMin))
			}
			if pe <= 0 {
				reasons = append(reasons, "pe unavailable")
			} else if pe >= req.PEMax {
				reasons = append(reasons, fmt.Sprintf("pe %.2f >= %.2f", pe, req.PEMax))
			}
		}

		klineArgs := []string{"kline", upperSymbol, "--period", req.MACDPeriod, "--count", strconv.Itoa(count), "--format", "json"}
		klineRaw, klineCommand, klineErr := skillRunLongbridgeJSON(ctx, klineArgs)
		klineAction := skillActionResult{
			Type:    "lb",
			Command: klineCommand,
			Status:  "success",
		}
		if klineErr != nil {
			klineAction.Status = "failed"
			klineAction.Error = klineErr.Error()
			actions = append(actions, klineAction)
			reasons = append(reasons, "kline fetch failed")
		} else {
			actions = append(actions, klineAction)
			closes := parseSkillCloseSeries(klineRaw)
			if len(closes) < 35 {
				reasons = append(reasons, "kline data too short")
			} else {
				cross := skillHasRecentMACDGoldenCross(closes, req.MACDLookbackDays, 12, 26, 9)
				if !cross {
					reasons = append(reasons, fmt.Sprintf("no MACD golden cross in last %d bars", req.MACDLookbackDays))
				}
			}
		}

		if len(reasons) == 0 {
			out.Matched = append(out.Matched, skillMarketScreenMatch{
				Symbol:          upperSymbol,
				PE:              pe,
				MarketCapUSD:    marketCapUSD,
				MACDGoldenCross: true,
			})
			continue
		}
		out.Rejected = append(out.Rejected, skillMarketScreenRejection{
			Symbol:  upperSymbol,
			Reasons: reasons,
		})
	}

	out.Stats = map[string]any{
		"total":    len(req.Symbols),
		"matched":  len(out.Matched),
		"rejected": len(out.Rejected),
	}
	return out, actions, nil
}

func buildPDFLocalFallbackActions(skill skillInstalled, intent string, text string, routeArgs map[string]any) []skillAction {
	if !strings.EqualFold(strings.TrimSpace(skill.ID), "pdf") {
		return nil
	}
	normalizedIntent := strings.ToLower(strings.TrimSpace(intent))
	normalizedText := strings.ToLower(strings.TrimSpace(text))
	if !skillAllowsAction(skill, "sys") {
		return nil
	}

	if isPDFExtractTextRequest(normalizedIntent, normalizedText) {
		opts := buildPDFExtractTextOptions(routeArgs, text)
		if strings.TrimSpace(opts.InputPath) == "" {
			return nil
		}
		command := buildLocalPDFExtractTextShellCommand(opts)
		return []skillAction{
			{
				Type:    "sys",
				Command: command,
			},
		}
	}
	if !isPDFGenerateRequest(normalizedIntent, normalizedText) {
		return nil
	}

	content := extractPDFTextContentFromRouteArgs(routeArgs)
	if strings.TrimSpace(content) == "" {
		content = extractPDFTextContentFromInstruction(text)
	}
	output := extractPDFOutputPathFromRouteArgs(routeArgs)
	if strings.TrimSpace(output) == "" {
		output = extractPDFOutputPathFromText(text)
	}
	command := buildLocalPDFReportlabShellCommand(output, content)
	return []skillAction{
		{
			Type:    "sys",
			Command: command,
		},
	}
}

func shouldUsePDFLocalFallback(skill skillInstalled, primaryActions []skillAction, fallbackActions []skillAction) bool {
	if !strings.EqualFold(strings.TrimSpace(skill.ID), "pdf") {
		return false
	}
	if len(fallbackActions) == 0 {
		return false
	}
	return !skillActionsEquivalent(primaryActions, fallbackActions)
}

func pdfActionsRequireDaemon(actions []skillAction) bool {
	for _, one := range actions {
		if normalizeSkillAction(one.Type) == "task" {
			return true
		}
	}
	return false
}

func skillActionsEquivalent(left []skillAction, right []skillAction) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		l := left[idx]
		r := right[idx]
		if normalizeSkillAction(l.Type) != normalizeSkillAction(r.Type) {
			return false
		}
		if strings.TrimSpace(l.Command) != strings.TrimSpace(r.Command) {
			return false
		}
	}
	return true
}

func buildSkillExecutableActions(skill skillInstalled, actions []skillAction) ([]skillExecutableAction, []skillActionResult, error) {
	out := make([]skillExecutableAction, 0, len(actions))
	planned := make([]skillActionResult, 0, len(actions))
	for _, one := range actions {
		actionType := normalizeSkillAction(one.Type)
		if actionType == "" {
			continue
		}
		executable, err := toSkillExecutableAction(skill, actionType, strings.TrimSpace(one.Command))
		if err != nil {
			return nil, nil, err
		}
		out = append(out, executable)
		planned = append(planned, skillActionResult{
			Type:    executable.Type,
			Command: executable.CommandLine,
			Status:  "planned",
		})
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("skill route did not produce executable actions")
	}
	return out, planned, nil
}

func executeSkillExecutableActions(ctx context.Context, actions []skillExecutableAction) (map[string]any, []skillActionResult, error) {
	out := map[string]any{
		"steps": []map[string]any{},
	}
	steps := make([]map[string]any, 0, len(actions))
	statuses := make([]skillActionResult, 0, len(actions))

	var previous *daemonPipelineStage
	var finalResult any

	for _, one := range actions {
		stageCtx := ctx
		if pipelineInput, ok := buildDaemonPipelineInput(previous); ok {
			stageCtx = withDaemonPipelineInput(stageCtx, pipelineInput)
		}

		stageApp := NewAppContext()
		runErr := executeCLICommand(stageCtx, stageApp, nil, one.CLIArgs)
		_, _, stageResult := stageApp.ExecutionSnapshot()
		_ = stageApp.Close()

		status := skillActionResult{
			Type:    one.Type,
			Command: one.CommandLine,
			Status:  "success",
		}
		step := map[string]any{
			"type":    one.Type,
			"command": one.CommandLine,
		}
		if stageResult != nil {
			step["result"] = stageResult
		}

		if runErr != nil {
			status.Status = "failed"
			status.Error = runErr.Error()
			statuses = append(statuses, status)
			step["error"] = runErr.Error()
			steps = append(steps, step)
			out["steps"] = steps
			if stageResult != nil {
				out["final"] = stageResult
			}
			return out, statuses, runErr
		}

		statuses = append(statuses, status)
		steps = append(steps, step)
		finalResult = stageResult
		previous = &daemonPipelineStage{
			commandLine: one.CommandLine,
			result:      stageResult,
		}
	}

	out["steps"] = steps
	if finalResult != nil {
		out["final"] = finalResult
	}
	return out, statuses, nil
}

func toSkillExecutableAction(skill skillInstalled, actionType string, command string) (skillExecutableAction, error) {
	normalized := normalizeSkillAction(actionType)
	if normalized == "" {
		return skillExecutableAction{}, fmt.Errorf("skill action type is empty")
	}
	trimmedCommand := strings.TrimSpace(command)
	if trimmedCommand == "" {
		return skillExecutableAction{}, fmt.Errorf("skill action %q has empty command", normalized)
	}

	var (
		args []string
		err  error
	)
	switch normalized {
	case "lb":
		args, err = buildLBCLIArgsFromSkillCommand(trimmedCommand)
	case "email":
		args, err = buildPrefixedCLIArgsFromSkillCommand(trimmedCommand, "email", []string{"email", "mail"})
	case "task":
		taskArgs, taskErr := buildPrefixedCLIArgsFromSkillCommand(trimmedCommand, "task", []string{"task"})
		if taskErr != nil {
			err = taskErr
			break
		}
		if isTaskSchedulerCommandArgs(taskArgs) {
			args = taskArgs
			break
		}
		if !skillAllowsAction(skill, "sys") {
			err = fmt.Errorf("task action %q is not a scheduler command; enable sys action or use task add/list/pause/resume/global-pause/global-resume/remove", trimmedCommand)
			break
		}
		converted := trimTaskInstructionPrefix(trimmedCommand)
		args, err = buildSystemCLIArgsFromSkillCommand(converted)
		if err == nil {
			normalized = "sys"
		}
	case "sys":
		args, err = buildSystemCLIArgsFromSkillCommand(trimmedCommand)
	default:
		err = fmt.Errorf("skill action %q is not supported", normalized)
	}
	if err != nil {
		return skillExecutableAction{}, err
	}
	return skillExecutableAction{
		Type:        normalized,
		Command:     trimmedCommand,
		CLIArgs:     args,
		CommandLine: FormatCommandLine(args),
	}, nil
}

func isTaskSchedulerCommandArgs(args []string) bool {
	if len(args) < 2 {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(args[0]), "task") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(args[1])) {
	case "add", "list", "ls", "pause", "resume", "global-pause", "global-resume", "remove", "rm", "delete":
		return true
	default:
		return false
	}
}

func trimTaskInstructionPrefix(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return trimmed
	}
	args, err := parseCommandLine(trimmed)
	if err != nil {
		return trimmed
	}
	args = trimLongtradegoPrefix(args)
	if len(args) >= 2 && strings.EqualFold(strings.TrimSpace(args[0]), "task") {
		return strings.TrimSpace(strings.Join(args[1:], " "))
	}
	return trimmed
}

func buildLBCLIArgsFromSkillCommand(command string) ([]string, error) {
	args, err := parseCommandLine(command)
	if err != nil {
		return nil, fmt.Errorf("parse lb action command failed: %w", err)
	}
	args = trimLongtradegoPrefix(args)
	if len(args) == 0 {
		return nil, fmt.Errorf("lb action command is empty")
	}

	first := strings.ToLower(strings.TrimSpace(args[0]))
	switch first {
	case "longbridge", "lb":
		if len(args) == 1 {
			return []string{"longbridge", "--help"}, nil
		}
		return append([]string{"longbridge"}, args[1:]...), nil
	default:
		return append([]string{"longbridge"}, args...), nil
	}
}

func buildPrefixedCLIArgsFromSkillCommand(command string, canonicalPrefix string, aliases []string) ([]string, error) {
	args, err := parseCommandLine(command)
	if err != nil {
		return nil, fmt.Errorf("parse %s action command failed: %w", canonicalPrefix, err)
	}
	args = trimLongtradegoPrefix(args)
	if len(args) == 0 {
		return nil, fmt.Errorf("%s action command is empty", canonicalPrefix)
	}

	first := strings.ToLower(strings.TrimSpace(args[0]))
	for _, one := range aliases {
		if first == strings.ToLower(strings.TrimSpace(one)) {
			args[0] = canonicalPrefix
			return args, nil
		}
	}

	return append([]string{canonicalPrefix}, args...), nil
}

func buildSystemCLIArgsFromSkillCommand(command string) ([]string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil, fmt.Errorf("sys action command is empty")
	}

	args, parseErr := parseCommandLine(trimmed)
	if parseErr == nil && len(args) > 0 {
		args = trimLongtradegoPrefix(args)
		if len(args) > 0 {
			first := strings.ToLower(strings.TrimSpace(args[0]))
			if first == "sys" || first == "shell" {
				args[0] = "sys"
				return args, nil
			}
		}
	}

	if rewritten, ok := rewriteSkillInstructionToShell(trimmed); ok {
		return []string{"sys", "--shell", rewritten}, nil
	}
	return []string{"sys", "--shell", trimmed}, nil
}

func trimLongtradegoPrefix(args []string) []string {
	if len(args) == 0 {
		return args
	}
	if strings.EqualFold(strings.TrimSpace(args[0]), "longtradego") {
		return append([]string(nil), args[1:]...)
	}
	return append([]string(nil), args...)
}

func rewriteSkillInstructionToShell(command string) (string, bool) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", false
	}
	if shouldSkipPDFInstructionRewrite(trimmed) {
		return "", false
	}
	lower := strings.ToLower(trimmed)
	if isPDFExtractTextRequest("", lower) {
		opts := buildPDFExtractTextOptions(nil, trimmed)
		if strings.TrimSpace(opts.InputPath) != "" {
			script := buildLocalPDFExtractTextShellCommand(opts)
			return script, true
		}
	}
	if !isPDFGenerateRequest("", lower) {
		return "", false
	}

	output := extractPDFOutputPathFromText(trimmed)
	content := extractPDFTextContentFromInstruction(trimmed)
	script := buildLocalPDFReportlabShellCommand(output, content)
	return script, true
}

func shouldSkipPDFInstructionRewrite(command string) bool {
	args, err := parseCommandLine(command)
	if err != nil || len(args) == 0 {
		return false
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	if first == "" {
		return false
	}
	if strings.HasPrefix(first, "(") || strings.HasPrefix(first, "{") || strings.HasPrefix(first, "[") {
		return true
	}
	if strings.Contains(first, "=") {
		return true
	}
	if strings.HasPrefix(first, "/") || strings.HasPrefix(first, "./") || strings.HasPrefix(first, "../") {
		return true
	}
	switch first {
	case "if", "then", "fi", "for", "while", "case", "do", "done":
		return true
	case "python", "python3", "bash", "sh", "zsh", "node", "ruby", "perl",
		"pdftotext", "qpdf", "cat", "echo", "sed", "awk", "grep", "rg", "find",
		"ls", "cp", "mv", "rm", "touch", "mkdir", "longbridge", "lb", "sys", "shell":
		return true
	default:
		return false
	}
}

func buildLocalPDFReportlabShellCommand(output string, content string) string {
	pdfBytes := buildSimplePDFBytes(content)
	encoded := base64.StdEncoding.EncodeToString(pdfBytes)
	target := strings.TrimSpace(output)
	if target == "" {
		target = "skill_output.pdf"
	}
	quotedPayload := shellQuoteSingle(encoded)
	quotedTarget := shellQuoteSingle(target)
	return fmt.Sprintf("(printf '%%s' %s | base64 --decode 2>/dev/null || printf '%%s' %s | base64 -D) > %s", quotedPayload, quotedPayload, quotedTarget)
}

func buildLocalPDFExtractTextShellCommand(opts pdfExtractTextOptions) string {
	inputPath := strings.TrimSpace(opts.InputPath)
	outputPath := strings.TrimSpace(opts.OutputPath)
	pdftotextArgs := make([]string, 0, 8)
	if opts.PreserveLayout {
		pdftotextArgs = append(pdftotextArgs, "-layout")
	}
	if opts.FirstPage > 0 {
		pdftotextArgs = append(pdftotextArgs, "-f", strconv.Itoa(opts.FirstPage))
	}
	if opts.LastPage > 0 {
		pdftotextArgs = append(pdftotextArgs, "-l", strconv.Itoa(opts.LastPage))
	}
	pdftotextArgs = append(pdftotextArgs, inputPath)
	if outputPath != "" {
		pdftotextArgs = append(pdftotextArgs, outputPath)
	} else {
		pdftotextArgs = append(pdftotextArgs, "-")
	}
	pdftotextCommand := make([]string, 0, len(pdftotextArgs)+1)
	pdftotextCommand = append(pdftotextCommand, "pdftotext")
	pdftotextCommand = append(pdftotextCommand, pdftotextArgs...)
	pdftotextCommandLine := FormatCommandLine(pdftotextCommand)

	script := strings.Join([]string{
		"import re,sys,zlib",
		"input_path=sys.argv[1]",
		`output_path=sys.argv[2] if len(sys.argv)>2 else ""`,
		`text=""`,
		"reader_cls=None",
		`for mod in ("pypdf","PyPDF2"):`,
		"    try:",
		`        reader_cls=__import__(mod, fromlist=["PdfReader"]).PdfReader`,
		"        break",
		"    except Exception:",
		"        pass",
		"if reader_cls is not None:",
		"    try:",
		"        reader=reader_cls(input_path)",
		"        pages=[]",
		`        for page in getattr(reader,"pages",[]):`,
		`            one=(page.extract_text() or "").strip()`,
		"            if one:",
		"                pages.append(one)",
		`        text="\n\n".join(pages).strip()`,
		"    except Exception:",
		`        text=""`,
		"if not text:",
		`    data=open(input_path,"rb").read()`,
		"    parts=[]",
		`    for match in re.finditer(rb"stream\r?\n(.*?)\r?\nendstream", data, re.S):`,
		"        stream=match.group(1)",
		"        header=data[max(0, match.start()-256):match.start()]",
		`        if b"FlateDecode" in header:`,
		"            try:",
		"                stream=zlib.decompress(stream)",
		"            except Exception:",
		"                pass",
		`        for token in re.findall(rb"\(([^()]*)\)\s*Tj", stream):`,
		`            raw=token.decode("latin-1","ignore").strip()`,
		"            if raw:",
		"                parts.append(raw)",
		`        for arr in re.findall(rb"\[(.*?)\]\s*TJ", stream, re.S):`,
		`            for token in re.findall(rb"\(([^()]*)\)", arr):`,
		`                raw=token.decode("latin-1","ignore").strip()`,
		"                if raw:",
		"                    parts.append(raw)",
		`    text="\n".join(parts).strip()`,
		"if output_path:",
		`    with open(output_path,"w",encoding="utf-8") as handle:`,
		"        handle.write(text)",
		"sys.stdout.write(text)",
	}, "\n")
	quotedScript := shellQuoteSingle(script)
	quotedInput := shellQuoteSingle(inputPath)
	var fallbackCommand string
	if outputPath != "" {
		quotedOutput := shellQuoteSingle(outputPath)
		fallbackCommand = fmt.Sprintf("python3 -c %s %s %s", quotedScript, quotedInput, quotedOutput)
	} else {
		fallbackCommand = fmt.Sprintf("python3 -c %s %s", quotedScript, quotedInput)
	}
	if outputPath != "" {
		quotedOutput := shellQuoteSingle(outputPath)
		return fmt.Sprintf("(if command -v pdftotext >/dev/null 2>&1; then %s && cat %s; else %s; fi)", pdftotextCommandLine, quotedOutput, fallbackCommand)
	}
	return fmt.Sprintf("(if command -v pdftotext >/dev/null 2>&1; then %s; else %s; fi)", pdftotextCommandLine, fallbackCommand)
}

func isPDFGenerateRequest(normalizedIntent string, normalizedText string) bool {
	if strings.Contains(normalizedIntent, "generate") || strings.Contains(normalizedIntent, "create") || strings.Contains(normalizedIntent, "make") {
		return true
	}
	if !(strings.Contains(normalizedText, "pdf") || strings.Contains(normalizedText, ".pdf")) {
		return false
	}
	for _, key := range []string{"generate", "create", "make", "build", "write", "生成", "创建", "制作"} {
		if strings.Contains(normalizedText, key) {
			return true
		}
	}
	return false
}

func isPDFExtractTextRequest(normalizedIntent string, normalizedText string) bool {
	if strings.Contains(normalizedIntent, "extract") && strings.Contains(normalizedIntent, "text") {
		return true
	}
	for _, key := range []string{"pdf_to_text", "to_text", "read_pdf", "extract_text", "ocr"} {
		if strings.Contains(normalizedIntent, key) {
			return true
		}
	}
	if !(strings.Contains(normalizedText, "pdf") || strings.Contains(normalizedText, ".pdf")) {
		return false
	}
	for _, key := range []string{
		"pdf to text",
		"to text",
		"to txt",
		"extract text",
		"text from pdf",
		"read pdf",
		"parse pdf",
		"pdf转txt",
		"pdf 转 text",
		"提取文字",
		"提取文本",
		"转文字",
		"转文本",
		"读取pdf",
		"读pdf",
		"ocr",
	} {
		if strings.Contains(normalizedText, key) {
			return true
		}
	}
	return false
}

func buildSimplePDFBytes(text string) []byte {
	stream := fmt.Sprintf("BT /F1 24 Tf 100 700 Td (%s) Tj ET", escapePDFText(text))
	streamLen := len([]byte(stream))

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", streamLen, stream),
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	offsets[0] = 0
	for index, obj := range objects {
		offsets[index+1] = buf.Len()
		buf.WriteString(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", index+1, obj))
	}

	xrefStart := buf.Len()
	buf.WriteString(fmt.Sprintf("xref\n0 %d\n", len(objects)+1))
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		buf.WriteString(fmt.Sprintf("%010d 00000 n \n", offsets[i]))
	}
	buf.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefStart))
	return buf.Bytes()
}

func escapePDFText(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		text = "hello world"
	}
	replacer := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)", "\r", " ", "\n", " ")
	return replacer.Replace(text)
}

func shellQuoteSingle(raw string) string {
	if raw == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(raw, "'", `'"'"'`) + "'"
}

func extractPDFOutputPathFromText(raw string) string {
	const fallback = "skill_output.pdf"
	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', '|', '"', '\'':
			return true
		default:
			return false
		}
	})
	for _, token := range tokens {
		if cleaned, ok := normalizePDFPathToken(token); ok {
			return cleaned
		}
	}
	return fallback
}

func extractPDFInputPathFromText(raw string) string {
	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', '|', '"', '\'':
			return true
		default:
			return false
		}
	})
	for _, token := range tokens {
		if cleaned, ok := normalizePDFPathToken(token); ok {
			return cleaned
		}
	}
	return ""
}

func normalizePDFPathToken(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if !strings.HasSuffix(strings.ToLower(trimmed), ".pdf") {
		return "", false
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.Contains(cleaned, ".."+string(os.PathSeparator)) || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return cleaned, true
}

func buildPDFExtractTextOptions(routeArgs map[string]any, text string) pdfExtractTextOptions {
	opts := pdfExtractTextOptions{
		InputPath: extractPDFInputPathFromRouteArgs(routeArgs),
	}
	if strings.TrimSpace(opts.InputPath) == "" {
		opts.InputPath = extractPDFInputPathFromText(text)
	}
	opts.OutputPath = extractPDFTextOutputPathFromRouteArgs(routeArgs)
	if strings.TrimSpace(opts.OutputPath) == "" {
		opts.OutputPath = extractPDFTextOutputPathFromText(text)
	}
	opts.PreserveLayout = extractPDFPreserveLayoutFromRouteArgs(routeArgs)
	if !opts.PreserveLayout {
		opts.PreserveLayout = extractPDFPreserveLayoutFromText(text)
	}
	first, last := extractPDFPageRangeFromRouteArgs(routeArgs)
	if first <= 0 && last <= 0 {
		first, last = extractPDFPageRangeFromText(text)
	}
	if first > 0 {
		opts.FirstPage = first
	}
	if last > 0 {
		opts.LastPage = last
	}
	if opts.FirstPage > 0 && opts.LastPage > 0 && opts.FirstPage > opts.LastPage {
		opts.FirstPage, opts.LastPage = opts.LastPage, opts.FirstPage
	}
	return opts
}

func extractPDFPreserveLayoutFromRouteArgs(routeArgs map[string]any) bool {
	if routeArgs == nil {
		return false
	}
	for _, key := range []string{"layout", "preserve_layout", "preserveLayout", "keep_layout", "keepLayout"} {
		raw, exists := routeArgs[key]
		if !exists {
			continue
		}
		if parsed, ok := skillAnyToBool(raw); ok {
			return parsed
		}
	}
	return false
}

func extractPDFPreserveLayoutFromText(raw string) bool {
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "-layout") || strings.Contains(lower, "--layout") {
		return true
	}
	for _, key := range []string{"preserving layout", "preserve layout", "keep layout", "保留版式", "保持版式", "保留布局"} {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}

func extractPDFPageRangeFromRouteArgs(routeArgs map[string]any) (int, int) {
	if routeArgs == nil {
		return 0, 0
	}
	first := extractPDFPageIntFromRouteArgs(routeArgs, []string{"first_page", "page_start", "start_page", "from_page", "f"})
	last := extractPDFPageIntFromRouteArgs(routeArgs, []string{"last_page", "page_end", "end_page", "to_page", "l"})
	return first, last
}

func extractPDFPageIntFromRouteArgs(routeArgs map[string]any, keys []string) int {
	for _, key := range keys {
		if value, exists := routeArgs[key]; exists {
			if parsed, ok := skillAnyToInt(value); ok && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func extractPDFPageRangeFromText(raw string) (int, int) {
	args, err := parseCommandLine(strings.TrimSpace(raw))
	if err != nil || len(args) == 0 {
		return 0, 0
	}
	first := 0
	last := 0
	for i := 0; i < len(args); i++ {
		token := strings.TrimSpace(args[i])
		if token == "" {
			continue
		}
		if token == "-f" || token == "--first-page" {
			if i+1 < len(args) {
				if parsed, err := strconv.Atoi(strings.TrimSpace(args[i+1])); err == nil && parsed > 0 {
					first = parsed
				}
				i++
			}
			continue
		}
		if token == "-l" || token == "--last-page" {
			if i+1 < len(args) {
				if parsed, err := strconv.Atoi(strings.TrimSpace(args[i+1])); err == nil && parsed > 0 {
					last = parsed
				}
				i++
			}
			continue
		}
		if strings.HasPrefix(token, "-f=") || strings.HasPrefix(token, "--first-page=") {
			value := strings.TrimSpace(token[strings.Index(token, "=")+1:])
			if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
				first = parsed
			}
			continue
		}
		if strings.HasPrefix(token, "-l=") || strings.HasPrefix(token, "--last-page=") {
			value := strings.TrimSpace(token[strings.Index(token, "=")+1:])
			if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
				last = parsed
			}
			continue
		}
	}
	return first, last
}

func skillAnyToBool(raw any) (bool, bool) {
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(value))
		switch trimmed {
		case "1", "true", "yes", "y", "on":
			return true, true
		case "0", "false", "no", "n", "off":
			return false, true
		default:
			return false, false
		}
	case json.Number:
		if asInt, err := value.Int64(); err == nil {
			return asInt != 0, true
		}
		if asFloat, err := value.Float64(); err == nil {
			return asFloat != 0, true
		}
		return false, false
	case int:
		return value != 0, true
	case int8:
		return value != 0, true
	case int16:
		return value != 0, true
	case int32:
		return value != 0, true
	case int64:
		return value != 0, true
	case uint:
		return value != 0, true
	case uint8:
		return value != 0, true
	case uint16:
		return value != 0, true
	case uint32:
		return value != 0, true
	case uint64:
		return value != 0, true
	case float32:
		return value != 0, true
	case float64:
		return value != 0, true
	default:
		return false, false
	}
}

func extractPDFOutputPathFromRouteArgs(routeArgs map[string]any) string {
	if routeArgs == nil {
		return ""
	}
	for _, key := range []string{"output", "output_file", "file", "path", "filename"} {
		value := strings.TrimSpace(skillAnyToString(routeArgs[key]))
		if value == "" || value == "<nil>" {
			continue
		}
		if cleaned, ok := normalizePDFPathToken(value); ok {
			return cleaned
		}
	}
	return ""
}

func extractPDFInputPathFromRouteArgs(routeArgs map[string]any) string {
	if routeArgs == nil {
		return ""
	}
	for _, key := range []string{"input", "input_file", "input_path", "pdf", "pdf_file", "pdf_path", "source", "source_file", "source_path", "file", "path"} {
		value := strings.TrimSpace(skillAnyToString(routeArgs[key]))
		if value == "" || value == "<nil>" {
			continue
		}
		if cleaned, ok := normalizePDFPathToken(value); ok {
			return cleaned
		}
	}
	return ""
}

func extractPDFTextOutputPathFromText(raw string) string {
	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', '|', '"', '\'':
			return true
		default:
			return false
		}
	})
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if !strings.HasSuffix(lower, ".txt") && !strings.HasSuffix(lower, ".md") {
			continue
		}
		cleaned := filepath.Clean(trimmed)
		if cleaned == "." || cleaned == ".." || strings.Contains(cleaned, ".."+string(os.PathSeparator)) || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			continue
		}
		return cleaned
	}
	return ""
}

func extractPDFTextOutputPathFromRouteArgs(routeArgs map[string]any) string {
	if routeArgs == nil {
		return ""
	}
	for _, key := range []string{"output_text", "output_txt", "text_output", "text_file", "txt_file", "output"} {
		value := strings.TrimSpace(skillAnyToString(routeArgs[key]))
		if value == "" || value == "<nil>" {
			continue
		}
		lower := strings.ToLower(value)
		if !strings.HasSuffix(lower, ".txt") && !strings.HasSuffix(lower, ".md") {
			continue
		}
		cleaned := filepath.Clean(value)
		if cleaned == "." || cleaned == ".." || strings.Contains(cleaned, ".."+string(os.PathSeparator)) || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			continue
		}
		return cleaned
	}
	return ""
}

func extractPDFTextContentFromInstruction(raw string) string {
	lower := strings.ToLower(raw)
	defaultText := "hello world"

	if idx := strings.Index(lower, "content"); idx >= 0 {
		tail := strings.TrimSpace(raw[idx+len("content"):])
		if candidate := extractQuotedSegment(tail); candidate != "" {
			return candidate
		}
		tail = strings.TrimSpace(strings.TrimLeft(tail, ":= "))
		if tail != "" {
			if parsed := strings.TrimSpace(strings.Trim(tail, `"'`)); parsed != "" {
				return parsed
			}
		}
	}
	if candidate := extractQuotedSegment(raw); candidate != "" {
		return candidate
	}
	if strings.Contains(lower, "hello world") {
		return "hello world"
	}
	return defaultText
}

func extractPDFTextContentFromRouteArgs(routeArgs map[string]any) string {
	if routeArgs == nil {
		return ""
	}
	for _, key := range []string{"content", "text", "message", "title", "body"} {
		value := strings.TrimSpace(skillAnyToString(routeArgs[key]))
		if value == "" || value == "<nil>" {
			continue
		}
		return value
	}
	return ""
}

func extractQuotedSegment(raw string) string {
	type quotePair struct {
		left  string
		right string
	}
	for _, pair := range []quotePair{{"'", "'"}, {"\"", "\""}} {
		start := strings.Index(raw, pair.left)
		if start < 0 {
			continue
		}
		rest := raw[start+len(pair.left):]
		end := strings.Index(rest, pair.right)
		if end < 0 {
			continue
		}
		segment := strings.TrimSpace(rest[:end])
		if segment != "" {
			return segment
		}
	}
	return ""
}

func shouldExecuteExplicitSkillActions(intent string, actions []skillAction) bool {
	if len(actions) == 0 {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(intent))
	if normalized == "" {
		return false
	}
	switch normalized {
	case "explicit_command", "explicit_cli", "command_passthrough", "quote_to_pdf":
		return true
	default:
		return false
	}
}

func routeSkillIntentByExplicitCommand(text string, skills []skillInstalled) (skillRouteDecision, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len(skills) == 0 {
		return skillRouteDecision{}, false
	}
	args, err := parseCommandLine(trimmed)
	if err != nil || len(args) == 0 {
		return skillRouteDecision{}, false
	}
	args = trimLongtradegoPrefix(args)
	if len(args) == 0 {
		return skillRouteDecision{}, false
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	if first == "" {
		return skillRouteDecision{}, false
	}
	second := ""
	if len(args) > 1 {
		second = strings.ToLower(strings.TrimSpace(args[1]))
	}

	if !looksLikeExplicitSkillCommand(first, second, skills) {
		return skillRouteDecision{}, false
	}

	type candidate struct {
		skill      skillInstalled
		actionType string
		score      int
	}
	best := candidate{score: -1}
	for _, skill := range skills {
		if !skill.Enabled {
			continue
		}
		score, actionType, ok := matchExplicitCommandToSkill(skill, first, second)
		if !ok {
			continue
		}
		if score > best.score {
			best = candidate{
				skill:      skill,
				actionType: actionType,
				score:      score,
			}
		}
	}
	if best.score < 0 {
		return skillRouteDecision{}, false
	}

	commandLine := FormatCommandLine(args)
	return skillRouteDecision{
		SkillID:    best.skill.ID,
		Intent:     "explicit_command",
		Confidence: 1,
		Arguments: map[string]any{
			"explicit_command": commandLine,
			"tokens":           append([]string(nil), args...),
		},
		Actions: []skillAction{
			{
				Type:    best.actionType,
				Command: commandLine,
			},
		},
		Reason: "matched explicit command against installed skills",
	}, true
}

func routeSkillIntentByCompositeText(text string, initialSymbols []string, skills []skillInstalled) (skillRouteDecision, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len(skills) == 0 {
		return skillRouteDecision{}, false
	}
	if !isQuoteToPDFCompositeText(trimmed) {
		return skillRouteDecision{}, false
	}
	longbridgeSkill, ok := findSkillByID(skills, "longbridge")
	if !ok || !longbridgeSkill.Enabled {
		return skillRouteDecision{}, false
	}
	if !skillAllowsAction(longbridgeSkill, "lb") || !skillAllowsAction(longbridgeSkill, "sys") {
		return skillRouteDecision{}, false
	}
	symbols := resolveSkillSymbols(initialSymbols, nil, trimmed)
	if len(symbols) == 0 {
		return skillRouteDecision{}, false
	}
	output := extractPDFOutputPathFromText(trimmed)
	if strings.TrimSpace(output) == "" {
		output = "skill_output.pdf"
	}
	actions := buildQuoteToPDFActions(symbols, output)
	return skillRouteDecision{
		SkillID:    longbridgeSkill.ID,
		Intent:     "quote_to_pdf",
		Confidence: 0.99,
		Arguments: map[string]any{
			"symbols":     append([]string(nil), symbols...),
			"output_file": output,
		},
		Actions: actions,
		Reason:  "local composite routing: quote + pdf export",
	}, true
}

func isQuoteToPDFCompositeText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	if isPDFExtractTextRequest("", lower) {
		return false
	}
	hasPDFOutput := strings.Contains(lower, ".pdf") || strings.Contains(lower, " pdf")
	if !hasPDFOutput {
		return false
	}
	quoteKeywords := []string{
		"quote", "stock", "price", "ticker", "行情", "股价", "股票", "报价", "获取", "查询",
	}
	hasQuote := false
	for _, key := range quoteKeywords {
		if strings.Contains(lower, key) {
			hasQuote = true
			break
		}
	}
	if !hasQuote {
		return false
	}
	exportKeywords := []string{
		"保存", "导出", "输出", "export", "save", "write", "生成", "create", "to pdf",
	}
	for _, key := range exportKeywords {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return strings.Contains(lower, ".pdf")
}

func buildQuoteToPDFActions(symbols []string, outputPath string) []skillAction {
	quoteCommand := append([]string{"longbridge", "quote"}, append([]string(nil), symbols...)...)
	quoteCommand = append(quoteCommand, "--format", "json")
	quoteLine := FormatCommandLine(quoteCommand)
	pdfShell := buildQuoteResultToPDFShellCommand(outputPath)
	return []skillAction{
		{
			Type:    "lb",
			Command: quoteLine,
		},
		{
			Type:    "sys",
			Command: pdfShell,
		},
	}
}

func buildQuoteResultToPDFShellCommand(outputPath string) string {
	target := strings.TrimSpace(outputPath)
	if target == "" {
		target = "skill_output.pdf"
	}
	script := strings.Join([]string{
		"import json,os,sys",
		`target=sys.argv[1] if len(sys.argv)>1 else "skill_output.pdf"`,
		`raw=os.environ.get("LONGTRADE_PIPELINE_RESULT_JSON","").strip()`,
		`source=None`,
		"if raw:",
		"    try:",
		"        payload=json.loads(raw)",
		"    except Exception:",
		"        payload={}",
		"    if isinstance(payload, dict):",
		`        source=payload.get("stdout_json")`,
		"        if source is None:",
		`            stdout=payload.get("stdout")`,
		"            if isinstance(stdout, str):",
		"                try:",
		"                    source=json.loads(stdout)",
		"                except Exception:",
		"                    source=None",
		"if source is None:",
		"    try:",
		"        piped=json.load(sys.stdin)",
		"        if isinstance(piped, dict):",
		`            inner=piped.get("result")`,
		"            if isinstance(inner, dict):",
		`                source=inner.get("stdout_json", inner)`,
		"            else:",
		"                source=inner",
		"    except Exception:",
		"        source=None",
		"def pick_rows(obj):",
		"    if isinstance(obj, list):",
		"        return obj",
		"    if isinstance(obj, dict):",
		`        for key in ("data","items","quotes","list","result"):`,
		"            value=obj.get(key)",
		"            if isinstance(value, list):",
		"                return value",
		"    return []",
		"rows=pick_rows(source)",
		`lines=["Longbridge Quote Report"]`,
		"for row in rows:",
		"    if not isinstance(row, dict):",
		"        continue",
		`    symbol=str(row.get("symbol") or row.get("code") or row.get("ticker") or "").strip()`,
		`    last=row.get("last")`,
		`    prev=row.get("prev_close")`,
		`    high=row.get("high")`,
		`    low=row.get("low")`,
		"    parts=[]",
		"    if symbol:",
		"        parts.append(symbol)",
		"    if last is not None:",
		"        parts.append(f'last={last}')",
		"    if prev is not None:",
		"        parts.append(f'prev={prev}')",
		"    if high is not None:",
		"        parts.append(f'high={high}')",
		"    if low is not None:",
		"        parts.append(f'low={low}')",
		"    if parts:",
		"        lines.append(' '.join(parts))",
		"if len(lines)==1:",
		"    lines.append('No quote data returned')",
		"def esc(raw):",
		"    return raw.replace('\\\\','\\\\\\\\').replace('(','\\\\(').replace(')','\\\\)').replace('\\r',' ').replace('\\n',' ')",
		"text=' | '.join([line.strip() for line in lines if line.strip()]).strip()",
		"if not text:",
		"    text='No quote data returned'",
		"stream=f'BT /F1 12 Tf 40 760 Td ({esc(text)}) Tj ET'",
		"objects=[",
		"    '<< /Type /Catalog /Pages 2 0 R >>',",
		"    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',",
		"    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>',",
		"    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',",
		"    f'<< /Length {len(stream.encode())} >>\\nstream\\n{stream}\\nendstream',",
		"]",
		"buf=[]",
		"buf.append('%PDF-1.4\\n')",
		"offsets=[0]",
		"current=len(buf[0].encode())",
		"for i,obj in enumerate(objects, start=1):",
		"    offsets.append(current)",
		"    chunk=f'{i} 0 obj\\n{obj}\\nendobj\\n'",
		"    buf.append(chunk)",
		"    current+=len(chunk.encode())",
		"xref_start=current",
		"xref=[f'xref\\n0 {len(objects)+1}\\n','0000000000 65535 f \\n']",
		"for off in offsets[1:]:",
		"    xref.append(f'{off:010d} 00000 n \\n')",
		"trailer=f'trailer\\n<< /Size {len(objects)+1} /Root 1 0 R >>\\nstartxref\\n{xref_start}\\n%%EOF\\n'",
		"pdf=''.join(buf)+''.join(xref)+trailer",
		"with open(target,'wb') as handle:",
		"    handle.write(pdf.encode('latin-1','ignore'))",
		"print(target)",
	}, "\n")
	quotedScript := shellQuoteSingle(script)
	quotedTarget := shellQuoteSingle(target)
	return fmt.Sprintf("python3 -c %s %s", quotedScript, quotedTarget)
}

func looksLikeExplicitSkillCommand(first string, second string, skills []skillInstalled) bool {
	if first == "" {
		return false
	}
	if first == "longbridge" || first == "lb" || first == "email" || first == "mail" || first == "task" || first == "sys" || first == "shell" ||
		first == "pdftotext" || first == "qpdf" || first == "pdfcpu" || first == "pdfinfo" {
		return true
	}
	if strings.HasPrefix(first, "-") {
		return false
	}
	for _, skill := range skills {
		if explicitSkillHasCommandPrefix(skill, first, second) {
			return true
		}
	}
	if isLikelyLongbridgeCommandPrefix(first) {
		return true
	}
	return false
}

func matchExplicitCommandToSkill(skill skillInstalled, first string, second string) (int, string, bool) {
	score := 0
	if explicitSkillHasCommandPrefix(skill, first, second) {
		score += 80
	}

	skillID := strings.ToLower(strings.TrimSpace(skill.ID))
	switch skillID {
	case "longbridge":
		if isLikelyLongbridgeCommandPrefix(first) {
			score += 70
		}
	case "pdf":
		if first == "pdftotext" || first == "qpdf" || first == "pdfcpu" || first == "pdfinfo" {
			score += 70
		}
	}
	if score == 0 {
		return 0, "", false
	}

	for _, actionType := range explicitActionTypeCandidates(first, skillID) {
		if skillAllowsAction(skill, actionType) {
			return score, actionType, true
		}
	}
	return 0, "", false
}

func explicitSkillHasCommandPrefix(skill skillInstalled, first string, second string) bool {
	for _, hint := range skill.CommandHints {
		args, err := parseCommandLine(strings.TrimSpace(hint))
		if err != nil || len(args) == 0 {
			continue
		}
		args = trimLongtradegoPrefix(args)
		if len(args) == 0 {
			continue
		}
		hintFirst := strings.ToLower(strings.TrimSpace(args[0]))
		if hintFirst == first {
			return true
		}
		if (hintFirst == "longbridge" || hintFirst == "lb") && second != "" {
			if len(args) > 1 {
				hintSecond := strings.ToLower(strings.TrimSpace(args[1]))
				if hintSecond == second {
					return true
				}
			}
		}
	}
	return false
}

func explicitActionTypeCandidates(first string, skillID string) []string {
	switch first {
	case "email", "mail":
		return []string{"email", "sys"}
	case "task":
		return []string{"task", "sys"}
	case "sys", "shell":
		return []string{"sys"}
	}
	if isLikelyLongbridgeCommandPrefix(first) {
		return []string{"lb", "sys"}
	}
	if skillID == "longbridge" {
		return []string{"lb", "sys"}
	}
	return []string{"sys", "task", "email", "lb"}
}

func isLikelyLongbridgeCommandPrefix(first string) bool {
	switch first {
	case "longbridge", "lb",
		"quote", "kline", "calc-index", "exchange-rate",
		"order", "stock", "option", "warrant", "asset",
		"cash-flow", "capital-flow", "watchlist":
		return true
	default:
		return false
	}
}

func routeSkillIntentWithOpenAI(ctx context.Context, text string, skills []skillInstalled, llmConfigPath string) (skillRouteDecision, error) {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return skillRouteDecision{}, fmt.Errorf("route text is empty")
	}
	llm, err := loadSkillLLMConfig(llmConfigPath)
	if err != nil {
		return skillRouteDecision{}, err
	}
	apiURL, err := normalizeBookingOpenAIChatCompletionsURL(llm.BaseURL)
	if err != nil {
		return skillRouteDecision{}, fmt.Errorf("invalid skills llm base_url: %w", err)
	}
	model := strings.TrimSpace(llm.Model)
	if model == "" {
		model = "gpt-4.1-mini"
	}

	routingSkills := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		routingSkills = append(routingSkills, map[string]any{
			"id":              skill.ID,
			"source":          skill.Source,
			"description":     skill.Description,
			"command_hints":   skill.CommandHints,
			"executor":        skill.Executor,
			"allowed_actions": skill.AllowedActions,
		})
	}

	systemPrompt := strings.Join([]string{
		"You are a deterministic multi-skill intent router for local automation.",
		"Return STRICT JSON only (no markdown, no prose).",
		"Schema:",
		`{"skill_id":string,"intent":string,"arguments":object,"confidence":number(0..1),"actions":[{"type":"lb|email|task|sys","command":string}]}`,
		"",
		"Routing rules:",
		"1) Always select one installed skill id from the provided skills list.",
		"2) Route by the user's real task domain, not by popularity of a skill.",
		"3) If text contains explicit CLI command syntax, keep command verbatim in actions.command.",
		"4) Prefer normalized symbols with market suffix when possible, e.g. TSLA.US, 700.HK, 9988.HK.",
		"5) actions.type must be one of: lb, email, task, sys. Never output other action types.",
		"6) Only use actions.type values that are listed in the selected skill allowed_actions.",
		"7) task is ONLY for scheduler operations: task add/list/pause/resume/global-pause/global-resume/remove.",
		"8) For normal file/data processing requests, prefer sys with concrete shell/python command.",
		"9) Use command_hints from the selected skill when possible and emit concrete runnable commands.",
		"10) If the request is clearly market-data related, actions should include lb.",
		"11) If the request is PDF-related, prefer the pdf skill and keep pdftotext/qpdf style commands when user asks for them.",
		"12) Keep confidence realistic: high for explicit commands, lower for ambiguous requests.",
		"13) Always provide at least one action.",
		"",
		"Few-shot examples:",
		`Input: {"text":"给我当前特斯拉的股票价格信息"}`,
		`Output: {"skill_id":"longbridge","intent":"get_stock_price","arguments":{"company":"Tesla","ticker":"TSLA.US"},"confidence":0.95,"actions":[{"type":"lb","command":"quote TSLA.US"}]}`,
		`Input: {"text":"US + HK tech screen: market cap > 50B, PE < 25, recent MACD golden cross","symbols":"700.HK,9988.HK,IBM.US"}`,
		`Output: {"skill_id":"longbridge","intent":"market_screen_v1","arguments":{"symbols":["700.HK","9988.HK","IBM.US"],"market_cap_usd_min":50000000000,"pe_max":25,"macd_lookback_days":5},"confidence":0.95,"actions":[{"type":"lb","command":"calc-index/kline/exchange-rate"}]}`,
		`Input: {"text":"上海今天天气怎么样"}`,
		`Output: {"skill_id":"weather","intent":"get_weather","arguments":{"location":"Shanghai"},"confidence":0.90,"actions":[{"type":"sys","command":"curl -s \"wttr.in/Shanghai?format=3\""}]}`,
		`Input: {"text":"pdftotext -layout -f 1 -l 5 report.pdf report.txt"}`,
		`Output: {"skill_id":"pdf","intent":"pdf_to_text","arguments":{"input_file":"report.pdf","output_text":"report.txt","layout":true,"first_page":1,"last_page":5},"confidence":0.98,"actions":[{"type":"sys","command":"pdftotext -layout -f 1 -l 5 report.pdf report.txt"}]}`,
	}, "\n")
	requestBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": mustMarshalSkillRouterPrompt(map[string]any{
				"text":   trimmedText,
				"skills": routingSkills,
			})},
		},
		"temperature":     0,
		"response_format": map[string]any{"type": "json_object"},
	}
	encoded, err := marshalJSONNoHTMLEscape(requestBody)
	if err != nil {
		return skillRouteDecision{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(encoded))
	if err != nil {
		return skillRouteDecision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(llm.APIKey))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return skillRouteDecision{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return skillRouteDecision{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return skillRouteDecision{}, fmt.Errorf("skills llm routing failed: http=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return skillRouteDecision{}, err
	}
	if len(completion.Choices) == 0 {
		return skillRouteDecision{}, fmt.Errorf("skills llm routing failed: empty choices")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if content == "" {
		return skillRouteDecision{}, fmt.Errorf("skills llm routing failed: empty content")
	}

	decision, err := parseSkillRouteDecision(content)
	if err != nil {
		return skillRouteDecision{}, fmt.Errorf("skills llm routing response is invalid json: %w", err)
	}
	return decision, nil
}

func parseSkillRouteDecision(content string) (skillRouteDecision, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return skillRouteDecision{}, err
	}
	decision := skillRouteDecision{
		SkillID: strings.TrimSpace(skillAnyToString(payload["skill_id"])),
		Intent:  strings.TrimSpace(skillAnyToString(payload["intent"])),
	}
	if score, ok := skillAnyToFloat64(payload["confidence"]); ok {
		decision.Confidence = score
	}
	if decision.Confidence < 0 {
		decision.Confidence = 0
	}
	if decision.Confidence > 1 {
		decision.Confidence = 1
	}
	if args, ok := payload["arguments"].(map[string]any); ok {
		decision.Arguments = args
	} else {
		decision.Arguments = map[string]any{}
	}
	if actionsRaw, ok := payload["actions"].([]any); ok {
		actions := make([]skillAction, 0, len(actionsRaw))
		for _, one := range actionsRaw {
			actionObj, ok := one.(map[string]any)
			if !ok {
				continue
			}
			actions = append(actions, skillAction{
				Type:    strings.ToLower(strings.TrimSpace(skillAnyToString(actionObj["type"]))),
				Command: strings.TrimSpace(skillAnyToString(actionObj["command"])),
			})
		}
		decision.Actions = actions
	}
	decision.Reason = strings.TrimSpace(skillAnyToString(payload["reason"]))
	return decision, nil
}

func runSkillLongbridgeJSON(ctx context.Context, args []string) (any, string, error) {
	command := append([]string{longbridgeCLIProgram}, append([]string(nil), args...)...)
	commandLine := FormatCommandLine(command)
	result, err := runSystemCommand(ctx, command, "")
	if err != nil {
		return nil, commandLine, err
	}
	if result.StdoutJSONValid != nil && *result.StdoutJSONValid {
		return result.StdoutJSON, commandLine, nil
	}
	trimmed := strings.TrimSpace(result.Stdout)
	if trimmed == "" {
		return nil, commandLine, fmt.Errorf("longbridge command returned empty stdout")
	}
	var decoded any
	if decodeErr := json.Unmarshal([]byte(trimmed), &decoded); decodeErr != nil {
		return nil, commandLine, fmt.Errorf("longbridge command stdout is not valid json: %w", decodeErr)
	}
	return decoded, commandLine, nil
}

func parseSkillCalcIndexRows(raw any) []skillCalcIndexRow {
	rows := collectSkillObjectRows(raw)
	out := make([]skillCalcIndexRow, 0, len(rows))
	for _, row := range rows {
		symbol := strings.ToUpper(strings.TrimSpace(skillAnyToString(firstExistingValue(row, "symbol", "code", "ticker", "security", "name"))))
		if symbol == "" {
			continue
		}
		pe, _ := skillAnyToFloat64(firstExistingValue(row, "pe", "pe_ttm"))
		marketValue, _ := skillAnyToFloat64(firstExistingValue(row, "total_market_value", "market_cap", "market_value", "market_capitalization"))
		currency := strings.ToUpper(strings.TrimSpace(skillAnyToString(firstExistingValue(row, "currency", "market_currency", "quote_currency"))))
		out = append(out, skillCalcIndexRow{
			Symbol:           symbol,
			PE:               pe,
			TotalMarketValue: marketValue,
			Currency:         currency,
		})
	}
	return out
}

type skillCalcIndexRow struct {
	Symbol           string
	PE               float64
	TotalMarketValue float64
	Currency         string
}

func parseSkillCloseSeries(raw any) []float64 {
	rows := collectSkillObjectRows(raw)
	out := make([]float64, 0, len(rows))
	for _, row := range rows {
		closePrice, ok := skillAnyToFloat64(firstExistingValue(row, "close", "last_done", "price", "c"))
		if !ok {
			continue
		}
		out = append(out, closePrice)
	}
	return out
}

func collectSkillObjectRows(raw any) []map[string]any {
	switch value := raw.(type) {
	case []any:
		out := make([]map[string]any, 0, len(value))
		for _, item := range value {
			if row, ok := item.(map[string]any); ok {
				out = append(out, row)
			}
		}
		return out
	case map[string]any:
		for _, key := range []string{"data", "items", "list", "rows", "candles", "result"} {
			if nested, exists := value[key]; exists {
				if nestedRows := collectSkillObjectRows(nested); len(nestedRows) > 0 {
					return nestedRows
				}
			}
		}
		return []map[string]any{value}
	default:
		return nil
	}
}

func parseSkillToUSDRates(raw any) map[string]float64 {
	rates := map[string]float64{
		"USD": 1,
	}
	visitSkillRates(raw, rates)
	if _, ok := rates["HKD"]; !ok {
		rates["HKD"] = defaultSkillFallbackHKDToUSDFactor
	}
	return rates
}

func visitSkillRates(raw any, rates map[string]float64) {
	switch value := raw.(type) {
	case map[string]any:
		currency := strings.ToUpper(strings.TrimSpace(skillAnyToString(firstExistingValue(value, "currency", "code", "base", "from"))))
		if currency != "" {
			if toUSD, ok := skillAnyToFloat64(firstExistingValue(value, "to_usd", "usd_rate", "rate_to_usd", "usd")); ok && toUSD > 0 {
				rates[currency] = toUSD
			} else if fromUSD, ok := skillAnyToFloat64(firstExistingValue(value, "from_usd", "usd_to_currency")); ok && fromUSD > 0 {
				rates[currency] = 1 / fromUSD
			} else if rate, ok := skillAnyToFloat64(firstExistingValue(value, "rate", "exchange_rate")); ok && rate > 0 {
				if strings.EqualFold(strings.TrimSpace(skillAnyToString(firstExistingValue(value, "quote", "to"))), "USD") {
					rates[currency] = rate
				}
			}
		}

		for key, nested := range value {
			upperKey := strings.ToUpper(strings.TrimSpace(key))
			if strings.HasSuffix(upperKey, "USD") && len(upperKey) == 6 {
				base := strings.TrimSuffix(upperKey, "USD")
				if val, ok := skillAnyToFloat64(nested); ok && val > 0 {
					rates[base] = val
				}
			}
			visitSkillRates(nested, rates)
		}
	case []any:
		for _, item := range value {
			visitSkillRates(item, rates)
		}
	}
}

func skillHasRecentMACDGoldenCross(closes []float64, lookback int, fast int, slow int, signal int) bool {
	if len(closes) < slow+signal || lookback <= 0 {
		return false
	}
	dif, dea := skillComputeMACD(closes, fast, slow, signal)
	if len(dif) < 2 || len(dea) < 2 {
		return false
	}
	start := len(closes) - lookback
	if start < 1 {
		start = 1
	}
	for i := start; i < len(closes); i++ {
		if dif[i-1] <= dea[i-1] && dif[i] > dea[i] {
			return true
		}
	}
	return false
}

func skillComputeMACD(closes []float64, fast int, slow int, signal int) ([]float64, []float64) {
	emaFast := skillEMA(closes, fast)
	emaSlow := skillEMA(closes, slow)
	dif := make([]float64, len(closes))
	for i := range closes {
		dif[i] = emaFast[i] - emaSlow[i]
	}
	dea := skillEMA(dif, signal)
	return dif, dea
}

func skillEMA(series []float64, period int) []float64 {
	out := make([]float64, len(series))
	if len(series) == 0 {
		return out
	}
	if period <= 1 {
		copy(out, series)
		return out
	}
	alpha := 2.0 / (float64(period) + 1.0)
	out[0] = series[0]
	for i := 1; i < len(series); i++ {
		out[i] = alpha*series[i] + (1-alpha)*out[i-1]
	}
	return out
}

func validateSkillPlannedActions(skill skillInstalled, actions []skillAction) error {
	if len(actions) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(skill.AllowedActions))
	for _, one := range skill.AllowedActions {
		allowedSet[strings.ToLower(strings.TrimSpace(one))] = struct{}{}
	}
	for _, action := range actions {
		actionType := normalizeSkillAction(action.Type)
		if actionType == "" {
			continue
		}
		if _, ok := allowedSet[actionType]; !ok {
			return fmt.Errorf("skill action %q is not allowed; allowed=%v", actionType, skill.AllowedActions)
		}
	}
	return nil
}

func normalizeSkillAction(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "lb", "longbridge":
		return "lb"
	case "query", "quote", "price", "market", "market_data":
		return "lb"
	case "email", "mail":
		return "email"
	case "task":
		return "task"
	case "sys", "shell", "command", "exec", "curl":
		return "sys"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func normalizeSkillAllowedActions(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, one := range raw {
		action := normalizeSkillAction(one)
		if action == "" {
			continue
		}
		switch action {
		case "lb", "email", "task", "sys":
		default:
			continue
		}
		if _, exists := seen[action]; exists {
			continue
		}
		seen[action] = struct{}{}
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}

func skillAllowsAction(skill skillInstalled, action string) bool {
	normalized := normalizeSkillAction(action)
	if normalized == "" {
		return false
	}
	for _, one := range skill.AllowedActions {
		if normalizeSkillAction(one) == normalized {
			return true
		}
	}
	return false
}

func findSkillByID(skills []skillInstalled, id string) (skillInstalled, bool) {
	target := strings.ToLower(strings.TrimSpace(id))
	for _, skill := range skills {
		if strings.EqualFold(strings.TrimSpace(skill.ID), target) {
			return skill, true
		}
	}
	return skillInstalled{}, false
}

func parseSymbolsFromDecision(arguments map[string]any) []string {
	if arguments == nil {
		return nil
	}
	candidateKeys := []string{
		"symbols",
		"symbol",
		"ticker",
		"tickers",
		"code",
		"codes",
		"security",
		"securities",
		"stock",
		"stocks",
		"target",
		"targets",
		"instrument",
		"instruments",
	}
	items := make([]string, 0, len(candidateKeys))
	for _, key := range candidateKeys {
		if raw, ok := arguments[key]; ok {
			items = append(items, parseSymbolsFromDecisionValue(raw)...)
		}
	}
	return uniqueUpperSymbols(items)
}

func parseSymbolsFromDecisionValue(raw any) []string {
	switch value := raw.(type) {
	case string:
		return parseSkillSymbolsCSV(value)
	case []any:
		items := make([]string, 0, len(value))
		for _, one := range value {
			items = append(items, parseSymbolsFromDecisionValue(one)...)
		}
		return uniqueUpperSymbols(items)
	case map[string]any:
		items := make([]string, 0, 3)
		for _, key := range []string{"symbol", "ticker", "code", "security"} {
			if nested, ok := value[key]; ok {
				items = append(items, parseSymbolsFromDecisionValue(nested)...)
			}
		}
		return uniqueUpperSymbols(items)
	default:
		text := strings.TrimSpace(skillAnyToString(raw))
		if text == "" || text == "<nil>" {
			return nil
		}
		return parseSkillSymbolsCSV(text)
	}
}

func parseSkillSymbolsCSV(raw string) []string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	replaced := strings.NewReplacer("，", ",", "、", ",", ";", ",", "|", ",", "\n", ",", "\t", ",").Replace(text)
	parts := strings.Fields(strings.ReplaceAll(replaced, ",", " "))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if symbol, ok := normalizeSkillSymbolCandidate(part); ok {
			out = append(out, symbol)
		}
	}
	return uniqueUpperSymbols(out)
}

func inferSkillSymbolsFromText(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	upperText := strings.ToUpper(trimmed)
	out := make([]string, 0, 4)

	alias := map[string]string{
		"TESLA":     "TSLA.US",
		"特斯拉":       "TSLA.US",
		"NVIDIA":    "NVDA.US",
		"英伟达":       "NVDA.US",
		"APPLE":     "AAPL.US",
		"苹果":        "AAPL.US",
		"微软":        "MSFT.US",
		"MICROSOFT": "MSFT.US",
		"亚马逊":       "AMZN.US",
		"AMAZON":    "AMZN.US",
		"谷歌":        "GOOGL.US",
		"GOOGLE":    "GOOGL.US",
		"阿里巴巴":      "9988.HK",
		"TENCENT":   "700.HK",
		"腾讯":        "700.HK",
		"京东":        "9618.HK",
		"小米":        "1810.HK",
		"网易":        "9999.HK",
	}
	for token, symbol := range alias {
		if strings.Contains(upperText, strings.ToUpper(token)) {
			out = append(out, symbol)
		}
	}

	for _, one := range skillSymbolWithMarketPattern.FindAllString(upperText, -1) {
		if symbol, ok := normalizeSkillSymbolCandidate(one); ok {
			out = append(out, symbol)
		}
	}

	ignoredTickerTokens := map[string]struct{}{
		"US": {}, "HK": {}, "SH": {}, "SZ": {}, "SG": {}, "HAS": {},
		"PE": {}, "PB": {}, "EPS": {}, "MACD": {}, "RSI": {}, "KDJ": {}, "BOLL": {},
		"EMA": {}, "SMA": {}, "USD": {}, "CNY": {}, "HKD": {},
	}
	for _, one := range skillTickerPattern.FindAllString(trimmed, -1) {
		if _, skip := ignoredTickerTokens[one]; skip {
			continue
		}
		if symbol, ok := normalizeSkillSymbolCandidate(one); ok {
			out = append(out, symbol)
		}
	}
	return uniqueUpperSymbols(out)
}

func normalizeSkillSymbolCandidate(raw string) (string, bool) {
	candidate := strings.ToUpper(strings.TrimSpace(raw))
	candidate = strings.Trim(candidate, `"'`)
	if candidate == "" {
		return "", false
	}
	if skillSymbolExactPattern.MatchString(candidate) {
		return candidate, true
	}
	if skillTickerExactPattern.MatchString(candidate) {
		return candidate + ".US", true
	}
	return "", false
}

func uniqueUpperSymbols(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, one := range items {
		symbol := strings.ToUpper(strings.TrimSpace(one))
		if symbol == "" {
			continue
		}
		if _, exists := seen[symbol]; exists {
			continue
		}
		seen[symbol] = struct{}{}
		out = append(out, symbol)
	}
	return out
}

func inferSkillCurrencyFromSymbol(symbol string) string {
	upper := strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case strings.HasSuffix(upper, ".US"):
		return "USD"
	case strings.HasSuffix(upper, ".HK"):
		return "HKD"
	default:
		return "USD"
	}
}

func resolveSkillPathFromLock(root string, relativePath string) (string, error) {
	base := filepath.Clean(strings.TrimSpace(root))
	if base == "" {
		return "", fmt.Errorf("root path is empty")
	}
	relative := filepath.Clean(strings.TrimSpace(relativePath))
	if relative == "." || relative == "" {
		return "", fmt.Errorf("relative path is empty")
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute path is not allowed")
	}
	joined := filepath.Clean(filepath.Join(base, relative))
	prefix := base + string(os.PathSeparator)
	if joined != base && !strings.HasPrefix(joined, prefix) {
		return "", fmt.Errorf("path traversal detected")
	}
	return joined, nil
}

func loadSkillLockFile(path string) (skillLockFile, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return skillLockFile{}, fmt.Errorf("skills lock path is empty")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skillLockFile{}, fmt.Errorf("skills lock file not found: %s: %w", trimmed, os.ErrNotExist)
		}
		return skillLockFile{}, err
	}
	var lock skillLockFile
	if err := json.Unmarshal(raw, &lock); err != nil {
		return skillLockFile{}, fmt.Errorf("parse skills lock %s failed: %w", trimmed, err)
	}
	if lock.Skills == nil {
		lock.Skills = map[string]skillLockFileEntry{}
	}
	return lock, nil
}

func loadSkillConfigFile(path string) (skillConfigFile, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return skillConfigFile{Skills: map[string]skillConfigEntry{}}, nil
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skillConfigFile{Skills: map[string]skillConfigEntry{}}, nil
		}
		return skillConfigFile{}, err
	}
	var cfg skillConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return skillConfigFile{}, fmt.Errorf("parse skills config %s failed: %w", trimmed, err)
	}
	if cfg.Skills == nil {
		cfg.Skills = map[string]skillConfigEntry{}
	}
	if cfg.MinConfidence < 0 {
		cfg.MinConfidence = 0
	}
	if cfg.MinConfidence > 1 {
		cfg.MinConfidence = 1
	}
	return cfg, nil
}

func loadSkillLLMConfig(path string) (skillLLMConfig, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return skillLLMConfig{}, fmt.Errorf("skills llm config path is empty")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skillLLMConfig{}, fmt.Errorf("skills llm config file not found: %s", trimmed)
		}
		return skillLLMConfig{}, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return skillLLMConfig{}, fmt.Errorf("skills llm config %s is empty", trimmed)
	}
	var cfg skillLLMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return skillLLMConfig{}, fmt.Errorf("parse skills llm config %s failed: %w", trimmed, err)
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.APIKey == "" {
		return skillLLMConfig{}, fmt.Errorf("skills llm config %s has empty api_key", trimmed)
	}
	if cfg.BaseURL == "" {
		return skillLLMConfig{}, fmt.Errorf("skills llm config %s has empty base_url", trimmed)
	}
	return cfg, nil
}

func validateSkillsLLMConfigPath(path string) error {
	_, err := loadSkillLLMConfig(path)
	return err
}

func resolveSkillsConfigPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return defaultSkillsConfigPath()
	}
	return trimmed
}

func resolveSkillsLLMConfigPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return defaultSkillsLLMConfigPath()
	}
	return trimmed
}

func defaultSkillLockPath() string {
	home := strings.TrimSpace(getEnvFirst("HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = strings.TrimSpace(h)
		}
	}
	return filepath.Join(home, ".agents", ".skill-lock.json")
}

func defaultSkillsRootPath() string {
	home := strings.TrimSpace(getEnvFirst("HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = strings.TrimSpace(h)
		}
	}
	return filepath.Join(home, ".agents", "skills")
}

func defaultSkillsConfigPath() string {
	return filepath.Join("conf", "skills.json")
}

func defaultSkillsLLMConfigPath() string {
	return filepath.Join("conf", "skills_llm.json")
}

func extractSkillDescription(markdown string) string {
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "description:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(text, "description:")), `"'`)
		}
		if strings.HasPrefix(text, "#") || strings.HasPrefix(text, "---") {
			continue
		}
		return text
	}
	return ""
}

func extractSkillCommandHints(markdown string) []string {
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return nil
	}

	lines := strings.Split(trimmed, "\n")
	hints := make([]string, 0, 8)
	seen := map[string]struct{}{}
	inCodeBlock := false

	for _, one := range lines {
		line := strings.TrimSpace(one)
		if strings.HasPrefix(line, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if !inCodeBlock || line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "Output:") {
			continue
		}
		if strings.HasPrefix(line, "$") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "$"))
		}

		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "curl "):
		case strings.HasPrefix(lower, "longbridge "):
		case strings.HasPrefix(lower, "lb "):
		case strings.HasPrefix(lower, "quote "):
		case strings.HasPrefix(lower, "email "):
		case strings.HasPrefix(lower, "mail "):
		case strings.HasPrefix(lower, "task "):
		case strings.HasPrefix(lower, "sys "):
		default:
			continue
		}

		if _, exists := seen[line]; exists {
			continue
		}
		seen[line] = struct{}{}
		hints = append(hints, line)
		if len(hints) >= 8 {
			break
		}
	}

	return hints
}

func printSkillRunResult(result skillRunResult, format string) {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return
	}
	fmt.Printf("skill run: skill=%s confidence=%.2f dry_run=%t\n", strings.TrimSpace(result.SkillID), result.Confidence, result.DryRun)
	if strings.TrimSpace(result.Intent) != "" {
		fmt.Printf("intent: %s\n", strings.TrimSpace(result.Intent))
	}
	if len(result.Actions) > 0 {
		fmt.Println("actions:")
		for _, action := range result.Actions {
			if strings.TrimSpace(action.Error) != "" {
				fmt.Printf("  - [%s] %s (%s)\n", action.Status, action.Command, action.Error)
			} else {
				fmt.Printf("  - [%s] %s\n", action.Status, action.Command)
			}
		}
	}
	if len(result.Errors) > 0 {
		fmt.Println("errors:")
		for _, item := range result.Errors {
			fmt.Printf("  - %s\n", item)
		}
	}
}

func printSkillListResult(result skillListResult, format string) {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return
	}
	if len(result.Skills) == 0 {
		fmt.Println("no installed skills")
		return
	}
	for _, item := range result.Skills {
		fmt.Printf("id=%s enabled=%t executor=%s source=%s installed_at=%s\n", item.ID, item.Enabled, item.Executor, item.Source, item.InstalledAt)
	}
}

func printSkillValidateResult(result skillValidateResult, format string) {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return
	}
	fmt.Printf("skill validate: valid=%t checked_at=%s skills=%d\n", result.Valid, result.CheckedAt, len(result.Skills))
	for _, warning := range result.Warnings {
		fmt.Printf("warning: %s\n", warning)
	}
}

func mustMarshalSkillRouterPrompt(value any) string {
	encoded, err := marshalJSONNoHTMLEscape(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func firstExistingValue(row map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, exists := row[key]; exists {
			return value
		}
	}
	return nil
}

func skillAnyToString(raw any) string {
	switch value := raw.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case fmt.Stringer:
		return value.String()
	default:
		return fmt.Sprintf("%v", raw)
	}
}

func skillAnyToFloat64(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	case json.Number:
		number, err := value.Float64()
		return number, err == nil
	case string:
		trimmed := strings.TrimSpace(strings.ReplaceAll(value, ",", ""))
		if trimmed == "" {
			return 0, false
		}
		number, err := strconv.ParseFloat(trimmed, 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func skillAnyToInt(raw any) (int, bool) {
	if number, ok := skillAnyToFloat64(raw); ok {
		return int(number), true
	}
	return 0, false
}
