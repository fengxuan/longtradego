package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	"github.com/spf13/cobra"
)

type daemonMonitorRuntime struct {
	id          int
	recordID    string
	key         string
	commandLine string
	startedAt   time.Time
	cancel      context.CancelFunc
}

type daemonMonitorSnapshot struct {
	MonitorID   string `json:"monitor_id,omitempty"`
	RuntimeID   int    `json:"runtime_id,omitempty"`
	Key         string `json:"key"`
	CommandLine string `json:"command_line"`
	CreatedAt   string `json:"created_at,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	Status      string `json:"status"`
}

const (
	daemonMonitorStateVersion = 1
	daemonMonitorStateFile    = "daemon_monitors.json"
)

type daemonMonitorState struct {
	Version  int                   `json:"version"`
	NextID   int64                 `json:"next_id,omitempty"`
	Monitors []daemonMonitorRecord `json:"monitors"`
}

type daemonMonitorRecord struct {
	ID          string `json:"id,omitempty"`
	Key         string `json:"key"`
	CommandLine string `json:"command_line"`
	CreatedAt   string `json:"created_at,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
}

type daemonOwnedWebhookRuntime struct {
	StartToken string
}

func newDaemonCommand(app *appContext, commandLogger *commandFileLogger) *cobra.Command {
	return &cobra.Command{
		Use:     "daemon",
		Aliases: []string{"d"},
		Short:   "Run interactive daemon mode and execute commands continuously",
		RunE: func(cmd *cobra.Command, args []string) error {
			app.SetExecution("daemon", nil)
			app.SetResult(map[string]any{"mode": "daemon", "state": "running"})

			fmt.Println("Daemon mode started. Type commands like: quote AAPL.US TSLA.US")
			fmt.Println("Use 'exit' to stop daemon mode.")

			rl, err := newDaemonReadline()
			if err != nil {
				return err
			}
			defer rl.Close()
			daemonOut := rl.Stdout()
			printDaemonWebhookStartupHint(daemonOut)

			var (
				taskManager       *daemonTaskManager
				executeMu         sync.Mutex
				monitorMu         sync.Mutex
				daemonWebhookMu   sync.Mutex
				daemonStartedAt   = time.Now()
				daemonSessionID   = newDaemonWebhookOwnerSessionID()
				monitorSeq        int
				monitorNextID     int64 = 1
				monitorStatePath        = defaultDaemonMonitorStatePath()
				daemonWebhookPIDs       = make(map[int]daemonOwnedWebhookRuntime)
				monitors                = make(map[int]*daemonMonitorRuntime)
				monitorRecords          = make(map[string]daemonMonitorRecord)
			)
			runSingleParsedCommand := func(runCtx context.Context, parsedArgs []string) (any, error) {
				executeMu.Lock()
				defer executeMu.Unlock()

				effectiveCtx := runCtx
				if claim, ok := buildDaemonWebhookStartOwnerClaim(parsedArgs, daemonSessionID); ok {
					effectiveCtx = withWebhookStartOwnerClaim(runCtx, claim)
				}

				runErr := executeCLICommand(effectiveCtx, app, commandLogger, parsedArgs)
				_, _, result := app.ExecutionSnapshot()

				// Keep daemon status as foreground session state after command execution.
				app.SetExecution("daemon", nil)
				app.SetResult(map[string]any{"mode": "daemon", "state": "running"})
				return result, runErr
			}
			runParsedCommands := func(runCtx context.Context, parsedCommands [][]string) error {
				var runErr error
				if len(parsedCommands) > 1 {
					executeMu.Lock()
					runErr = executeDaemonPipeline(runCtx, app, commandLogger, taskManager, parsedCommands)
					executeMu.Unlock()
				} else if len(parsedCommands) == 1 {
					_, runErr = runSingleParsedCommand(runCtx, parsedCommands[0])
				}

				// Keep daemon status as foreground session state after command execution.
				app.SetExecution("daemon", nil)
				app.SetResult(map[string]any{"mode": "daemon", "state": "running"})
				return runErr
			}
			saveMonitorRecords := func() error {
				monitorMu.Lock()
				records := cloneDaemonMonitorRecords(monitorRecords)
				nextID := monitorNextID
				monitorMu.Unlock()
				return writeDaemonMonitorState(monitorStatePath, records, nextID)
			}
			startDaemonMonitor := func(commands [][]string, persist bool, restored bool, preferredKey string) error {
				if !isMonitorJobCommands(commands) {
					return fmt.Errorf("unsupported monitor command: %s", formatPipelineCommands(commands))
				}
				monitorKey := strings.TrimSpace(preferredKey)
				if monitorKey == "" {
					monitorKey = buildMonitorJobIdentityKey(commands)
				}
				commandLine := formatPipelineCommands(commands)
				now := time.Now()
				var (
					prevRecord daemonMonitorRecord
					prevExists bool
					recordID   string
				)

				monitorMu.Lock()
				if existing := findDaemonMonitorByKeyLocked(monitors, monitorKey); existing != nil {
					id := existing.id
					existingLine := existing.commandLine
					monitorMu.Unlock()
					if restored {
						return nil
					}
					return fmt.Errorf("monitor already running: monitor-%d (%s)", id, existingLine)
				}

				if persist {
					prevRecord, prevExists = monitorRecords[monitorKey]
					record, ok := monitorRecords[monitorKey]
					if !ok {
						record = daemonMonitorRecord{
							ID:        formatDaemonMonitorPersistentID(monitorNextID),
							Key:       monitorKey,
							CreatedAt: formatTaskTimestamp(now),
						}
						monitorNextID++
					}
					if strings.TrimSpace(record.ID) == "" {
						record.ID = formatDaemonMonitorPersistentID(monitorNextID)
						monitorNextID++
					}
					record.Key = monitorKey
					record.CommandLine = commandLine
					if strings.TrimSpace(record.CreatedAt) == "" {
						record.CreatedAt = formatTaskTimestamp(now)
					}
					record.Paused = false
					monitorRecords[monitorKey] = record
					recordID = record.ID
				} else if existingRecord, ok := monitorRecords[monitorKey]; ok {
					recordID = existingRecord.ID
				}

				monitorSeq++
				monitorID := monitorSeq
				monitorCtx, cancel := context.WithCancel(cmd.Context())
				monitors[monitorID] = &daemonMonitorRuntime{
					id:          monitorID,
					recordID:    recordID,
					key:         monitorKey,
					commandLine: commandLine,
					startedAt:   now,
					cancel:      cancel,
				}
				monitorMu.Unlock()

				if persist {
					if err := saveMonitorRecords(); err != nil {
						monitorMu.Lock()
						delete(monitors, monitorID)
						if prevExists {
							monitorRecords[monitorKey] = prevRecord
						} else {
							delete(monitorRecords, monitorKey)
						}
						monitorMu.Unlock()
						return err
					}
				}

				if restored {
					_, _ = fmt.Fprintf(daemonOut, "[monitor-%d] restored: %s\n", monitorID, commandLine)
				} else {
					_, _ = fmt.Fprintf(daemonOut, "[monitor-%d] started: %s\n", monitorID, commandLine)
				}

				go func(id int, runtimeKey string, runtimeRecordID string, runCommands [][]string) {
					backgroundApp := newAppContext()
					defer backgroundApp.Close()

					runErr := runMonitorJobLoop(monitorCtx, backgroundApp, commandLogger, runCommands, runtimeRecordID)

					monitorMu.Lock()
					delete(monitors, id)
					_, shouldKeep := monitorRecords[runtimeKey]
					monitorMu.Unlock()

					if runErr != nil {
						label := fmt.Sprintf("monitor-%d", id)
						if strings.TrimSpace(runtimeRecordID) != "" {
							label = runtimeRecordID
						}
						_, _ = fmt.Fprintf(daemonOut, "\n[%s] stopped with error: %v\n", label, runErr)
						if shouldKeep {
							if saveErr := saveMonitorRecords(); saveErr != nil {
								_, _ = fmt.Fprintf(daemonOut, "monitor state save failed: %v\n", saveErr)
							}
						}
						return
					}
					label := fmt.Sprintf("monitor-%d", id)
					if strings.TrimSpace(runtimeRecordID) != "" {
						label = runtimeRecordID
					}
					_, _ = fmt.Fprintf(daemonOut, "\n[%s] stopped\n", label)
				}(monitorID, monitorKey, recordID, cloneCommandStages(commands))

				return nil
			}

			taskManager = newDaemonTaskManagerWithPaths(
				runParsedCommands,
				defaultDaemonTaskStatePath(),
				defaultDaemonTaskHistoryPath(),
			)
			defer taskManager.Close()
			defer func() {
				daemonWebhookMu.Lock()
				cloned := make(map[int]daemonOwnedWebhookRuntime, len(daemonWebhookPIDs))
				for pid, state := range daemonWebhookPIDs {
					cloned[pid] = state
				}
				daemonWebhookMu.Unlock()
				stopDaemonOwnedWebhookOnDaemonExit(cloned, daemonSessionID, daemonOut)
			}()
			defer func() {
				monitorMu.Lock()
				defer monitorMu.Unlock()
				for id, monitor := range monitors {
					monitor.cancel()
					delete(monitors, id)
				}
			}()
			restoredTaskCount, loadErr := taskManager.Load(cmd.Context())
			if loadErr != nil {
				fmt.Printf("task restore failed: %v\n", loadErr)
			} else if restoredTaskCount > 0 {
				fmt.Printf("restored %d scheduled task(s)\n", restoredTaskCount)
			}
			loadedMonitorRecords, loadedMonitorNextID, monitorLoadErr := readDaemonMonitorState(monitorStatePath)
			if monitorLoadErr != nil {
				if !errors.Is(monitorLoadErr, os.ErrNotExist) {
					fmt.Printf("monitor restore failed: %v\n", monitorLoadErr)
				}
			} else {
				monitorRecords = loadedMonitorRecords
				monitorNextID = loadedMonitorNextID
			}
			if len(monitorRecords) > 0 {
				keys := sortedDaemonMonitorRecordKeys(monitorRecords)
				restoredCount := 0
				stateChanged := false
				for _, key := range keys {
					record := monitorRecords[key]
					if record.Paused {
						continue
					}
					commands, parseErr := parsePipelineCommandLine(record.CommandLine)
					if parseErr != nil || len(commands) == 0 || !isMonitorJobCommands(commands) {
						fmt.Printf("monitor restore skipped invalid command: %s\n", record.CommandLine)
						monitorMu.Lock()
						delete(monitorRecords, key)
						monitorMu.Unlock()
						stateChanged = true
						continue
					}
					if startErr := startDaemonMonitor(commands, false, true, key); startErr != nil {
						fmt.Printf("monitor restore start failed: %v\n", startErr)
						continue
					}
					restoredCount++
				}
				if restoredCount > 0 {
					fmt.Printf("restored %d monitor(s)\n", restoredCount)
				}
				if stateChanged {
					if saveErr := saveMonitorRecords(); saveErr != nil {
						fmt.Printf("monitor state save failed: %v\n", saveErr)
					}
				}
			}

			daemonTaskIDCandidates = taskManager.TaskIDs
			defer func() {
				daemonTaskIDCandidates = emptyDaemonTaskIDs
			}()
			daemonMonitorIDCandidates = func() []string {
				monitorMu.Lock()
				defer monitorMu.Unlock()
				ids := make([]string, 0, len(monitorRecords))
				for _, record := range monitorRecords {
					if strings.TrimSpace(record.ID) != "" {
						ids = append(ids, record.ID)
					}
				}
				sort.Strings(ids)
				return ids
			}
			defer func() {
				daemonMonitorIDCandidates = emptyDaemonMonitorIDs
			}()

			startMonitorByConfigID := func(configID string) error {
				monitorMu.Lock()
				recordKey, record, ok := findDaemonMonitorRecordByIDLocked(monitorRecords, configID)
				monitorMu.Unlock()
				if !ok {
					return fmt.Errorf("monitor config not found: %s", configID)
				}
				commands, parseErr := parsePipelineCommandLine(record.CommandLine)
				if parseErr != nil || len(commands) == 0 || !isMonitorJobCommands(commands) {
					return fmt.Errorf("invalid monitor config command: %s", record.CommandLine)
				}
				return startDaemonMonitor(commands, true, false, recordKey)
			}
			startAllPausedMonitorConfigs := func() (int, error) {
				monitorMu.Lock()
				pausedIDs := make([]string, 0, len(monitorRecords))
				for _, record := range monitorRecords {
					if record.Paused && strings.TrimSpace(record.ID) != "" {
						pausedIDs = append(pausedIDs, record.ID)
					}
				}
				monitorMu.Unlock()
				sort.Strings(pausedIDs)
				started := 0
				for _, configID := range pausedIDs {
					if err := startMonitorByConfigID(configID); err != nil {
						return started, err
					}
					started++
				}
				return started, nil
			}

			adminAuthPath := defaultDaemonAdminAuthConfigPath()
			adminAuthConfig, adminAuthErr := loadDaemonAdminAuthConfig(adminAuthPath)
			if adminAuthErr != nil {
				fmt.Printf("daemon admin service unavailable: load auth config %s failed: %v\n", adminAuthPath, adminAuthErr)
			} else {
				adminSvc, adminErr := startDaemonAdminService(daemonAdminServiceOptions{
					PreferredAddr:       defaultDaemonAdminAddr,
					MaxPortFallback:     defaultDaemonAdminPortFallback,
					RuntimePath:         defaultDaemonAdminRuntimePath(),
					DaemonPID:           os.Getpid(),
					DaemonSessionID:     daemonSessionID,
					DaemonStartedAt:     daemonStartedAt,
					Out:                 daemonOut,
					TaskManager:         taskManager,
					MonitorMu:           &monitorMu,
					Monitors:            monitors,
					MonitorRecords:      monitorRecords,
					SaveMonitorState:    saveMonitorRecords,
					StartMonitorByID:    startMonitorByConfigID,
					StartAllMonitors:    startAllPausedMonitorConfigs,
					WebhookOwnedMu:      &daemonWebhookMu,
					WebhookOwnedPIDs:    daemonWebhookPIDs,
					WebhookRuntime:      defaultWebhookRuntimeStatePath(),
					WebhookLogPath:      defaultWebhookServerLogPath(),
					BookingRuntime:      defaultBookingRuntimeStatePath(),
					BookingLogPath:      defaultBookingServiceLogPath(),
					BookingAddr:         defaultBookingServiceAddr,
					BookingCatalog:      defaultBookingCatalogStatePath(),
					BookingReservations: defaultBookingReservationsStatePath(),
					BookingDrafts:       defaultBookingIntakeDraftsPath(),
					BookingAPIKeys:      defaultBookingAPIKeysConfigPath(),
					BookingLLMConfig:    defaultBookingLLMConfigPath(),
					BookingAdminAuth:    defaultDaemonAdminAuthConfigPath(),
					AuthConfig:          adminAuthConfig,
				})
				if adminErr != nil {
					fmt.Printf("daemon admin service unavailable: %v\n", adminErr)
				} else {
					fmt.Printf("daemon admin service listening on %s (%s)\n", adminSvc.Address(), adminSvc.URL())
					defer func() {
						if closeErr := adminSvc.Close(); closeErr != nil {
							fmt.Printf("daemon admin shutdown failed: %v\n", closeErr)
						}
					}()
				}
			}

			var nextDefault string
			var interruptHintShown bool

			for {
				var line string
				if strings.TrimSpace(nextDefault) != "" {
					line, err = rl.ReadlineWithDefault(nextDefault)
					nextDefault = ""
				} else {
					line, err = rl.Readline()
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						fmt.Println()
						return finalizeDaemonExit(app, "eof")
					}
					if errors.Is(err, readline.ErrInterrupt) {
						if !interruptHintShown {
							fmt.Println("Ctrl+C only clears current input. Use 'exit' to stop daemon.")
							interruptHintShown = true
						}
						continue
					}

					if err != nil {
						if cleanupErr := finalizeDaemonExit(app, "error"); cleanupErr != nil {
							return errors.Join(err, cleanupErr)
						}
						return err
					}
				}
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				_ = rl.SaveHistory(line)

				taskListPipeline := isTaskListPipelineLine(line)
				if !taskListPipeline {
					taskHandled, taskErr := taskManager.HandleInputLine(cmd.Context(), line)
					if taskHandled {
						if taskErr != nil {
							fmt.Printf("task command failed: %v\n", taskErr)
						}
						continue
					}
				}

				pipelineCommands, err := parsePipelineCommandLine(line)
				if err != nil {
					fmt.Printf("invalid command line: %v\n", err)
					continue
				}
				if len(pipelineCommands) == 0 {
					continue
				}

				if len(pipelineCommands) > 1 {
					if isMonitorPipelineCommands(pipelineCommands) {
						if startErr := startDaemonMonitor(pipelineCommands, true, false, ""); startErr != nil {
							fmt.Printf("monitor start failed: %v\n", startErr)
						}
						continue
					}
					if err := runParsedCommands(cmd.Context(), pipelineCommands); err != nil {
						fmt.Printf("pipeline failed: %v\n", err)
					}
					continue
				}

				parsedArgs := pipelineCommands[0]
				if rewritten, ok := rewriteDaemonWebhookServeToStart(parsedArgs); ok {
					fmt.Println("daemon note: webhook serve is rewritten to background mode via webhook start")
					parsedArgs = rewritten
				}
				if rewritten, ok := rewriteDaemonBookingServeToStart(parsedArgs); ok {
					fmt.Println("daemon note: booking service serve is rewritten to background mode via booking service start")
					parsedArgs = rewritten
				}
				first := strings.ToLower(parsedArgs[0])
				if first == "exit" {
					return finalizeDaemonExit(app, "stopped")
				}
				if first == "daemon" || first == "d" {
					fmt.Println("already in daemon mode")
					continue
				}
				if template, ok := commandTemplate(parsedArgs); ok {
					fmt.Printf("template: %s\n", template)
					nextDefault = template
					continue
				}
				if normalized, ok := normalizeEmailMonitorControlCommand(parsedArgs); ok {
					parsedArgs = normalized
				}
				if handled, monitorErr := handleDaemonMonitorControlCommand(parsedArgs, &monitorMu, monitors, monitorRecords, saveMonitorRecords, startMonitorByConfigID, startAllPausedMonitorConfigs); handled {
					if monitorErr != nil {
						fmt.Printf("monitor command failed: %v\n", monitorErr)
					}
					continue
				}

				if isEmailMonitorCommand(parsedArgs) {
					if startErr := startDaemonMonitor([][]string{parsedArgs}, true, false, ""); startErr != nil {
						fmt.Printf("monitor start failed: %v\n", startErr)
					}
					continue
				}

				commandResult, commandErr := runSingleParsedCommand(cmd.Context(), parsedArgs)
				daemonWebhookMu.Lock()
				trackDaemonOwnedWebhookLifecycle(parsedArgs, commandResult, daemonWebhookPIDs, daemonSessionID)
				daemonWebhookMu.Unlock()
				if commandErr != nil {
					fmt.Printf("command failed: %v\n", commandErr)
				}
			}
		},
	}
}

func finalizeDaemonExit(app *appContext, state string) error {
	app.SetExecution("daemon", nil)
	app.SetResult(map[string]any{"mode": "daemon", "state": state})
	if err := app.Close(); err != nil {
		return fmt.Errorf("close daemon context: %w", err)
	}
	return nil
}

func isTaskListPipelineLine(line string) bool {
	args, err := parseCommandLine(line)
	if err != nil {
		return false
	}
	if len(args) < 3 {
		return false
	}
	if !strings.EqualFold(args[0], daemonTaskCommandName) {
		return false
	}
	sub := strings.ToLower(strings.TrimSpace(args[1]))
	if sub != "list" && sub != "ls" {
		return false
	}
	for _, arg := range args[2:] {
		if arg == "|" {
			return true
		}
	}
	return false
}

type daemonPipelineStage struct {
	commandLine string
	result      any
}

type daemonPipelineInput struct {
	SourceCommand string
	ResultJSON    string
	InputJSON     string
}

type daemonPipelineInputContextKey struct{}

const (
	pipelineEnvSourceCommand = "LONGTRADE_PIPELINE_SOURCE_COMMAND"
	pipelineEnvResultJSON    = "LONGTRADE_PIPELINE_RESULT_JSON"
	pipelineEnvInputJSON     = "LONGTRADE_PIPELINE_INPUT_JSON"
)

func executeDaemonPipeline(ctx context.Context, app *appContext, commandLogger *commandFileLogger, taskManager *daemonTaskManager, commands [][]string) error {
	return executeDaemonPipelineWithPrevious(ctx, app, commandLogger, taskManager, commands, nil)
}

func executeDaemonPipelineWithPrevious(
	ctx context.Context,
	app *appContext,
	commandLogger *commandFileLogger,
	taskManager *daemonTaskManager,
	commands [][]string,
	initialPrevious *daemonPipelineStage,
) error {
	previous := initialPrevious

	for index, rawArgs := range commands {
		if len(rawArgs) == 0 {
			return fmt.Errorf("empty command at pipeline stage %d", index+1)
		}

		preparedArgs := preparePipelineArgs(rawArgs, previous)
		first := strings.ToLower(preparedArgs[0])

		if first == "exit" {
			return fmt.Errorf("exit cannot be used inside pipeline")
		}
		if first == "daemon" || first == "d" {
			return fmt.Errorf("daemon command cannot be nested inside pipeline")
		}

		if template, ok := commandTemplate(preparedArgs); ok {
			return fmt.Errorf("pipeline stage %d triggered template (%s); please fill concrete arguments", index+1, template)
		}

		if strings.EqualFold(first, daemonTaskCommandName) {
			taskResult, taskErr := executeTaskPipelineStage(taskManager, preparedArgs)
			if taskErr != nil {
				return fmt.Errorf("stage %d (%s) failed: %w", index+1, formatCommandLine(preparedArgs), taskErr)
			}
			app.SetExecution("task", nil)
			app.SetResult(taskResult)
			previous = &daemonPipelineStage{
				commandLine: formatCommandLine(preparedArgs),
				result:      taskResult,
			}
			continue
		}

		stageCtx := ctx
		if pipelineInput, ok := buildDaemonPipelineInput(previous); ok {
			stageCtx = withDaemonPipelineInput(stageCtx, pipelineInput)
		}

		if err := executeCLICommand(stageCtx, app, commandLogger, preparedArgs); err != nil {
			return fmt.Errorf("stage %d (%s) failed: %w", index+1, formatCommandLine(preparedArgs), err)
		}

		_, _, result := app.ExecutionSnapshot()
		if shouldStopPipelineAfterStage(preparedArgs, result) {
			return nil
		}
		previous = &daemonPipelineStage{
			commandLine: formatCommandLine(preparedArgs),
			result:      result,
		}
	}

	return nil
}

func shouldStopPipelineAfterStage(args []string, result any) bool {
	if !isEmailMonitorCommand(args) {
		return false
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		return false
	}

	stopped, ok := resultMap["stopped"].(bool)
	if !ok {
		return false
	}
	return stopped
}

func executeTaskPipelineStage(taskManager *daemonTaskManager, args []string) (any, error) {
	if taskManager == nil {
		return nil, fmt.Errorf("task manager is not initialized")
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("missing task subcommand")
	}
	sub := strings.ToLower(strings.TrimSpace(args[1]))
	switch sub {
	case "list", "ls":
		if len(args) > 2 {
			return nil, fmt.Errorf("task list in pipeline does not accept extra arguments")
		}
		tasks := taskManager.listTasks()
		globallyPaused := taskManager.isGloballyPaused()
		text := formatTaskListOutput(tasks, globallyPaused)
		if strings.TrimSpace(text) != "" {
			fmt.Print(text)
		}
		return map[string]any{
			"global_paused": globallyPaused,
			"tasks":         tasks,
			"text":          strings.TrimRight(text, "\n"),
		}, nil
	default:
		return nil, fmt.Errorf("task subcommand %q is not supported in pipeline; only task list is supported", sub)
	}
}

func preparePipelineArgs(currentArgs []string, previous *daemonPipelineStage) []string {
	prepared := append([]string(nil), currentArgs...)
	if previous == nil {
		return prepared
	}

	if isEmailSendCommand(prepared) {
		if emailHasBodyInput(prepared) {
			return prepared
		}

		body := buildPipelineEmailBody(previous.commandLine, previous.result)
		if strings.TrimSpace(body) == "" {
			return prepared
		}

		prepared = append(prepared, "--body", body)
		return prepared
	}

	if isEmailAnalyzeCommand(prepared) {
		if analyzeHasInputJSON(prepared) {
			return prepared
		}
		input := buildPipelineAnalyzeInputJSON(previous.commandLine, previous.result)
		if strings.TrimSpace(input) == "" {
			return prepared
		}
		prepared = append(prepared, "--input-json", input)
		return prepared
	}

	return prepared
}

func buildDaemonPipelineInput(previous *daemonPipelineStage) (daemonPipelineInput, bool) {
	if previous == nil {
		return daemonPipelineInput{}, false
	}
	source := strings.TrimSpace(previous.commandLine)
	resultJSON := marshalPipelineJSON(previous.result)
	inputJSON := marshalPipelineInputJSON(source, previous.result)
	return daemonPipelineInput{
		SourceCommand: source,
		ResultJSON:    resultJSON,
		InputJSON:     inputJSON,
	}, true
}

func withDaemonPipelineInput(ctx context.Context, input daemonPipelineInput) context.Context {
	return context.WithValue(ctx, daemonPipelineInputContextKey{}, input)
}

func daemonPipelineInputFromContext(ctx context.Context) (daemonPipelineInput, bool) {
	if ctx == nil {
		return daemonPipelineInput{}, false
	}
	value := ctx.Value(daemonPipelineInputContextKey{})
	input, ok := value.(daemonPipelineInput)
	if !ok {
		return daemonPipelineInput{}, false
	}
	return input, true
}

func marshalPipelineJSON(value any) string {
	encoded, err := marshalJSONNoHTMLEscape(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

func marshalPipelineInputJSON(sourceCommand string, result any) string {
	payload := map[string]any{
		"source_command": sourceCommand,
		"result":         result,
	}
	encoded, err := marshalJSONNoHTMLEscape(payload)
	if err != nil {
		fallback, fallbackErr := marshalJSONNoHTMLEscape(map[string]string{
			"source_command": sourceCommand,
			"result_error":   err.Error(),
		})
		if fallbackErr != nil {
			return `{"source_command":"","result_error":"encode_failed"}`
		}
		return string(fallback)
	}
	return string(encoded)
}

func isEmailSendCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}

	first := strings.ToLower(args[0])
	if first != "email" && first != "mail" {
		return false
	}
	return strings.EqualFold(args[1], "send")
}

func isEmailMonitorCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	first := strings.ToLower(args[0])
	if first != "email" && first != "mail" {
		return false
	}
	sub := strings.ToLower(strings.TrimSpace(args[1]))
	if sub != "monitor" && sub != "watch" {
		return false
	}
	if len(args) >= 3 {
		switch strings.ToLower(strings.TrimSpace(args[2])) {
		case "list", "ls", "start", "resume", "stop", "kill", "remove", "delete", "rm", "help":
			return false
		}
	}
	return true
}

func isMonitorPipelineCommands(commands [][]string) bool {
	return len(commands) > 1 && len(commands[0]) > 0 && isEmailMonitorCommand(commands[0])
}

func isMonitorJobCommands(commands [][]string) bool {
	if len(commands) == 0 || len(commands[0]) == 0 {
		return false
	}
	if !isEmailMonitorCommand(commands[0]) {
		return false
	}
	return true
}

func buildMonitorJobIdentityKey(commands [][]string) string {
	if len(commands) == 0 || len(commands[0]) == 0 {
		return ""
	}
	base := buildEmailMonitorIdentityKey(commands[0])
	if len(commands) == 1 {
		return base
	}
	return base + "|pipeline=" + strings.ToLower(formatPipelineCommands(commands))
}

func formatPipelineCommands(commands [][]string) string {
	if len(commands) == 0 {
		return ""
	}
	parts := make([]string, 0, len(commands))
	for _, command := range commands {
		parts = append(parts, formatCommandLine(command))
	}
	return strings.Join(parts, " | ")
}

func cloneCommandStages(commands [][]string) [][]string {
	cloned := make([][]string, 0, len(commands))
	for _, stage := range commands {
		copied := append([]string(nil), stage...)
		cloned = append(cloned, copied)
	}
	return cloned
}

func stripMonitorOnceArgs(args []string) []string {
	next := make([]string, 0, len(args))
	for _, token := range args {
		trimmed := strings.TrimSpace(strings.ToLower(token))
		if trimmed == "--once" || strings.HasPrefix(trimmed, "--once=") {
			continue
		}
		next = append(next, token)
	}
	return next
}

func runMonitorJobLoop(ctx context.Context, app *appContext, commandLogger *commandFileLogger, commands [][]string, monitorConfigID string) error {
	if len(commands) == 0 {
		return fmt.Errorf("empty monitor command")
	}
	baseCommands := cloneCommandStages(commands)
	baseCommands[0] = stripMonitorOnceArgs(baseCommands[0])
	baseCommands[0] = ensureMonitorIDArg(baseCommands[0], monitorConfigID)

	if len(baseCommands) > 1 {
		return runStreamingMonitorPipelineLoop(ctx, app, commandLogger, baseCommands[0], baseCommands[1:])
	}

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if err := executeCLICommand(ctx, app, commandLogger, baseCommands[0]); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

func runStreamingMonitorPipelineLoop(
	ctx context.Context,
	app *appContext,
	commandLogger *commandFileLogger,
	monitorArgs []string,
	downstreamCommands [][]string,
) error {
	monitorLine := formatCommandLine(monitorArgs)
	downstream := cloneCommandStages(downstreamCommands)

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}

		hook := func(result imapReceiveResult) error {
			previous := &daemonPipelineStage{
				commandLine: monitorLine,
				result:      result,
			}
			if err := executeDaemonPipelineWithPrevious(ctx, app, commandLogger, nil, downstream, previous); err != nil {
				return fmt.Errorf("monitor downstream pipeline failed: %w", err)
			}
			return nil
		}

		monitorCtx := withMailMonitorEventHook(ctx, hook)
		if err := executeCLICommand(monitorCtx, app, commandLogger, monitorArgs); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

func ensureMonitorIDArg(args []string, monitorConfigID string) []string {
	trimmedID := strings.TrimSpace(monitorConfigID)
	if trimmedID == "" {
		return append([]string(nil), args...)
	}
	next := make([]string, 0, len(args)+2)
	skipNext := false
	for index, token := range args {
		if skipNext {
			skipNext = false
			continue
		}
		trimmed := strings.TrimSpace(strings.ToLower(token))
		if trimmed == "--monitor-id" {
			// Drop old value if provided as separate arg pair.
			if index+1 < len(args) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "--monitor-id=") {
			continue
		}
		next = append(next, token)
	}
	next = append(next, "--monitor-id", trimmedID)
	return next
}

func emailHasBodyInput(args []string) bool {
	for _, token := range args {
		if token == "--body" || token == "--body-file" {
			return true
		}
		if strings.HasPrefix(token, "--body=") || strings.HasPrefix(token, "--body-file=") {
			return true
		}
	}
	return false
}

func isEmailAnalyzeCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	first := strings.ToLower(args[0])
	if first != "email" && first != "mail" {
		return false
	}
	return strings.EqualFold(args[1], "analyze")
}

func analyzeHasInputJSON(args []string) bool {
	for _, token := range args {
		if token == "--input-json" {
			return true
		}
		if strings.HasPrefix(token, "--input-json=") {
			return true
		}
	}
	return false
}

func buildPipelineEmailBody(previousCommandLine string, previousResult any) string {
	if rawBody, ok := extractWebhookRawBodyForEmail(previousResult); ok {
		return rawBody
	}

	resultText := "null"
	if previousResult != nil {
		if encoded, err := marshalJSONIndentNoHTMLEscape(previousResult, "", "  "); err == nil {
			resultText = string(encoded)
		} else {
			resultText = fmt.Sprintf("%v", previousResult)
		}
	}

	var builder strings.Builder
	builder.WriteString("Longtradego pipeline result\n")
	builder.WriteString("source_command: ")
	builder.WriteString(previousCommandLine)
	builder.WriteString("\n")
	builder.WriteString("result:\n")
	builder.WriteString(resultText)
	return builder.String()
}

func extractWebhookRawBodyForEmail(previousResult any) (string, bool) {
	resultMap, ok := previousResult.(map[string]any)
	if !ok {
		return "", false
	}
	rawValue, exists := resultMap["raw_body"]
	if !exists {
		return "", false
	}
	rawBody, ok := rawValue.(string)
	if !ok {
		return "", false
	}
	if strings.TrimSpace(rawBody) == "" {
		return "", false
	}
	return rawBody, true
}

func buildPipelineAnalyzeInputJSON(previousCommandLine string, previousResult any) string {
	payload := map[string]any{
		"source_command": previousCommandLine,
		"result":         previousResult,
	}
	encoded, err := marshalJSONNoHTMLEscape(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func marshalJSONNoHTMLEscape(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func marshalJSONIndentNoHTMLEscape(value any, prefix string, indent string) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, prefix, indent)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return encoded, nil
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent(prefix, indent)
	if err := encoder.Encode(decoded); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func normalizeEmailMonitorControlCommand(args []string) ([]string, bool) {
	if len(args) < 3 {
		return nil, false
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	if first != "email" && first != "mail" {
		return nil, false
	}
	sub := strings.ToLower(strings.TrimSpace(args[1]))
	if sub != "monitor" && sub != "watch" {
		return nil, false
	}
	control := strings.ToLower(strings.TrimSpace(args[2]))
	switch control {
	case "list", "ls", "start", "resume", "stop", "kill", "remove", "delete", "rm", "help":
		normalized := make([]string, 0, len(args)-1)
		normalized = append(normalized, "monitor", control)
		if len(args) > 3 {
			normalized = append(normalized, args[3:]...)
		}
		return normalized, true
	default:
		return nil, false
	}
}

func findDaemonMonitorByKeyLocked(monitors map[int]*daemonMonitorRuntime, key string) *daemonMonitorRuntime {
	for _, monitor := range monitors {
		if monitor.key == key {
			return monitor
		}
	}
	return nil
}

func buildEmailMonitorIdentityKey(args []string) string {
	mailAlias := ""
	mailbox := ""
	unreadOnly := "true"
	subject := ""
	from := ""

	for index := 2; index < len(args); index++ {
		token := strings.TrimSpace(args[index])
		if token == "" {
			continue
		}
		if strings.HasPrefix(token, "--mailbox=") {
			mailbox = strings.TrimSpace(strings.TrimPrefix(token, "--mailbox="))
			continue
		}
		if strings.HasPrefix(token, "--mail-alias=") {
			mailAlias = strings.TrimSpace(strings.TrimPrefix(token, "--mail-alias="))
			continue
		}
		if strings.HasPrefix(token, "--subject-contains=") {
			subject = strings.TrimSpace(strings.TrimPrefix(token, "--subject-contains="))
			continue
		}
		if strings.HasPrefix(token, "--from-contains=") {
			from = strings.TrimSpace(strings.TrimPrefix(token, "--from-contains="))
			continue
		}
		if strings.HasPrefix(token, "--unread-only=") {
			unreadOnly = strings.TrimSpace(strings.TrimPrefix(token, "--unread-only="))
			continue
		}
		switch token {
		case "--mail-alias":
			if index+1 < len(args) {
				index++
				mailAlias = strings.TrimSpace(args[index])
			}
		case "--mailbox":
			if index+1 < len(args) {
				index++
				mailbox = strings.TrimSpace(args[index])
			}
		case "--subject-contains":
			if index+1 < len(args) {
				index++
				subject = strings.TrimSpace(args[index])
			}
		case "--from-contains":
			if index+1 < len(args) {
				index++
				from = strings.TrimSpace(args[index])
			}
		case "--unread-only":
			if index+1 < len(args) {
				index++
				unreadOnly = strings.TrimSpace(args[index])
			} else {
				unreadOnly = "true"
			}
		}
	}

	return strings.ToLower(strings.Join([]string{
		"mail_alias=" + mailAlias,
		"mailbox=" + mailbox,
		"unread=" + unreadOnly,
		"subject=" + subject,
		"from=" + from,
	}, "|"))
}

func handleDaemonMonitorControlCommand(
	args []string,
	monitorMu *sync.Mutex,
	monitors map[int]*daemonMonitorRuntime,
	monitorRecords map[string]daemonMonitorRecord,
	saveMonitorRecords func() error,
	startMonitorByConfigID func(string) error,
	startAllPausedMonitorConfigs func() (int, error),
) (bool, error) {
	if len(args) == 0 || !strings.EqualFold(args[0], "monitor") {
		return false, nil
	}
	if len(args) < 2 {
		printDaemonMonitorHelp()
		return true, nil
	}

	sub := strings.ToLower(strings.TrimSpace(args[1]))
	switch sub {
	case "list", "ls":
		if len(args) > 2 {
			return true, fmt.Errorf("monitor list does not accept extra arguments")
		}
		snapshots := listDaemonMonitors(monitorMu, monitors, monitorRecords)
		fmt.Print(formatDaemonMonitorListOutput(snapshots))
		return true, nil
	case "start", "resume":
		if len(args) < 3 {
			return true, fmt.Errorf("usage: monitor start <id|all>")
		}
		target := strings.TrimSpace(args[2])
		if strings.EqualFold(target, "all") {
			count, err := startAllPausedMonitorConfigs()
			if err != nil {
				return true, err
			}
			if count == 0 {
				fmt.Println("no paused monitor")
				return true, nil
			}
			fmt.Printf("started %d monitor(s)\n", count)
			return true, nil
		}
		if err := startMonitorByConfigID(target); err != nil {
			return true, err
		}
		fmt.Printf("started monitor config: %s\n", target)
		return true, nil
	case "stop", "kill":
		if len(args) < 3 {
			return true, fmt.Errorf("usage: monitor stop <id|all>")
		}
		target := strings.ToLower(strings.TrimSpace(args[2]))
		if target == "all" {
			stopped := stopAllDaemonMonitors(monitorMu, monitors, monitorRecords)
			if stopped == 0 {
				fmt.Println("no running monitor")
				return true, nil
			}
			if err := saveMonitorRecords(); err != nil {
				return true, fmt.Errorf("save monitor state failed: %w", err)
			}
			fmt.Printf("stopped %d monitor(s)\n", stopped)
			return true, nil
		}
		if !stopDaemonMonitorByTarget(monitorMu, monitors, monitorRecords, args[2]) {
			return true, fmt.Errorf("monitor not found: %s", args[2])
		}
		if err := saveMonitorRecords(); err != nil {
			return true, fmt.Errorf("save monitor state failed: %w", err)
		}
		fmt.Printf("stopped monitor: %s\n", args[2])
		return true, nil
	case "remove", "delete", "rm":
		if len(args) < 4 {
			return true, fmt.Errorf("usage: monitor remove <id> <confirm-id>")
		}
		targetID := strings.TrimSpace(args[2])
		confirmID := strings.TrimSpace(args[3])
		if targetID == "" || confirmID == "" {
			return true, fmt.Errorf("monitor id confirmation is required")
		}
		if !strings.EqualFold(targetID, confirmID) {
			return true, fmt.Errorf("monitor id confirmation mismatch: expected %q, got %q", targetID, confirmID)
		}
		removed := removeDaemonMonitorByConfigID(monitorMu, monitors, monitorRecords, targetID)
		if !removed {
			return true, fmt.Errorf("monitor config not found: %s", targetID)
		}
		if err := saveMonitorRecords(); err != nil {
			return true, fmt.Errorf("save monitor state failed: %w", err)
		}
		fmt.Printf("removed monitor config: %s\n", targetID)
		return true, nil
	case "help":
		printDaemonMonitorHelp()
		return true, nil
	default:
		return true, fmt.Errorf("unknown monitor subcommand %q, allowed: list|start|stop|remove|help", sub)
	}
}

func stopDaemonMonitorByTarget(
	monitorMu *sync.Mutex,
	monitors map[int]*daemonMonitorRuntime,
	monitorRecords map[string]daemonMonitorRecord,
	target string,
) bool {
	monitorMu.Lock()
	var (
		runtimeID int
		monitor   *daemonMonitorRuntime
		ok        bool
	)

	trimmedTarget := strings.TrimSpace(target)
	for id, candidate := range monitors {
		if strings.EqualFold(candidate.recordID, trimmedTarget) {
			runtimeID = id
			monitor = candidate
			ok = true
			break
		}
	}
	if !ok {
		if parsedRuntimeID, parseOK := parseDaemonMonitorRuntimeID(trimmedTarget); parseOK {
			if candidate, exists := monitors[parsedRuntimeID]; exists {
				runtimeID = parsedRuntimeID
				monitor = candidate
				ok = true
			}
		}
	}
	if ok {
		delete(monitors, runtimeID)
		if record, exists := monitorRecords[monitor.key]; exists {
			record.Paused = true
			monitorRecords[monitor.key] = record
		}
		monitorMu.Unlock()
		monitor.cancel()
		return true
	}

	if recordKey, record, exists := findDaemonMonitorRecordByIDLocked(monitorRecords, trimmedTarget); exists {
		if !record.Paused {
			record.Paused = true
			monitorRecords[recordKey] = record
		}
		monitorMu.Unlock()
		return true
	}
	monitorMu.Unlock()
	return false
}

func removeDaemonMonitorByConfigID(
	monitorMu *sync.Mutex,
	monitors map[int]*daemonMonitorRuntime,
	monitorRecords map[string]daemonMonitorRecord,
	configID string,
) bool {
	monitorMu.Lock()
	recordKey, _, exists := findDaemonMonitorRecordByIDLocked(monitorRecords, configID)
	if !exists {
		monitorMu.Unlock()
		return false
	}
	delete(monitorRecords, recordKey)

	running := make([]*daemonMonitorRuntime, 0, 1)
	for id, runtime := range monitors {
		if runtime.key == recordKey || strings.EqualFold(strings.TrimSpace(runtime.recordID), strings.TrimSpace(configID)) {
			running = append(running, runtime)
			delete(monitors, id)
		}
	}
	monitorMu.Unlock()
	for _, runtime := range running {
		runtime.cancel()
	}
	return true
}

func formatDaemonMonitorPersistentID(id int64) string {
	if id <= 0 {
		id = 1
	}
	return fmt.Sprintf("m-%d", id)
}

func parseDaemonMonitorPersistentID(raw string) (int64, bool) {
	text := strings.TrimSpace(strings.ToLower(raw))
	if !strings.HasPrefix(text, "m-") {
		return 0, false
	}
	value := strings.TrimSpace(strings.TrimPrefix(text, "m-"))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func parseDaemonMonitorRuntimeID(raw string) (int, bool) {
	text := strings.TrimSpace(strings.ToLower(raw))
	if strings.HasPrefix(text, "run-") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "run-"))
	}
	parsed, err := strconv.Atoi(text)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func findDaemonMonitorRecordByIDLocked(records map[string]daemonMonitorRecord, monitorID string) (string, daemonMonitorRecord, bool) {
	target := strings.TrimSpace(strings.ToLower(monitorID))
	if target == "" {
		return "", daemonMonitorRecord{}, false
	}
	for key, record := range records {
		if strings.ToLower(strings.TrimSpace(record.ID)) == target {
			return key, record, true
		}
	}
	return "", daemonMonitorRecord{}, false
}

func stopAllDaemonMonitors(
	monitorMu *sync.Mutex,
	monitors map[int]*daemonMonitorRuntime,
	monitorRecords map[string]daemonMonitorRecord,
) int {
	monitorMu.Lock()
	running := make([]*daemonMonitorRuntime, 0, len(monitors))
	for id, monitor := range monitors {
		running = append(running, monitor)
		delete(monitors, id)
		if record, exists := monitorRecords[monitor.key]; exists {
			record.Paused = true
			monitorRecords[monitor.key] = record
		}
	}
	monitorMu.Unlock()
	for _, monitor := range running {
		monitor.cancel()
	}
	return len(running)
}

func listDaemonMonitors(
	monitorMu *sync.Mutex,
	monitors map[int]*daemonMonitorRuntime,
	monitorRecords map[string]daemonMonitorRecord,
) []daemonMonitorSnapshot {
	monitorMu.Lock()
	defer monitorMu.Unlock()

	snapshots := make([]daemonMonitorSnapshot, 0, len(monitorRecords)+len(monitors))
	snapshotByKey := make(map[string]*daemonMonitorSnapshot, len(monitorRecords))

	for key, record := range monitorRecords {
		status := "scheduled"
		if record.Paused {
			status = "paused"
		}
		snapshots = append(snapshots, daemonMonitorSnapshot{
			MonitorID:   record.ID,
			Key:         key,
			CommandLine: record.CommandLine,
			CreatedAt:   record.CreatedAt,
			Status:      status,
		})
		snapshotByKey[key] = &snapshots[len(snapshots)-1]
	}

	for _, monitor := range monitors {
		if existing := snapshotByKey[monitor.key]; existing != nil {
			existing.RuntimeID = monitor.id
			existing.StartedAt = monitor.startedAt.Format(time.RFC3339)
			existing.Status = "running"
			if strings.TrimSpace(existing.MonitorID) == "" {
				existing.MonitorID = monitor.recordID
			}
			continue
		}
		snapshots = append(snapshots, daemonMonitorSnapshot{
			MonitorID:   monitor.recordID,
			RuntimeID:   monitor.id,
			Key:         monitor.key,
			CommandLine: monitor.commandLine,
			StartedAt:   monitor.startedAt.Format(time.RFC3339),
			Status:      "running",
		})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		// running first, then paused/scheduled, then by key
		wi := monitorStatusWeight(snapshots[i].Status)
		wj := monitorStatusWeight(snapshots[j].Status)
		if wi != wj {
			return wi < wj
		}
		idi := strings.TrimSpace(snapshots[i].MonitorID)
		idj := strings.TrimSpace(snapshots[j].MonitorID)
		if idi != "" && idj != "" && idi != idj {
			return idi < idj
		}
		return snapshots[i].CommandLine < snapshots[j].CommandLine
	})
	return snapshots
}

func formatDaemonMonitorListOutput(snapshots []daemonMonitorSnapshot) string {
	if len(snapshots) == 0 {
		return "no monitor configured\n"
	}
	var builder strings.Builder
	builder.WriteString("MONITOR_ID\tSTATUS\tCREATED_AT\tSTARTED_AT\tCOMMAND\n")
	for _, item := range snapshots {
		monitorID := "-"
		if strings.TrimSpace(item.MonitorID) != "" {
			monitorID = item.MonitorID
		}
		createdAt := "-"
		if strings.TrimSpace(item.CreatedAt) != "" {
			createdAt = item.CreatedAt
		}
		startedAt := "-"
		if strings.TrimSpace(item.StartedAt) != "" {
			startedAt = item.StartedAt
		}
		builder.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n", monitorID, item.Status, createdAt, startedAt, item.CommandLine))
	}
	return builder.String()
}

func monitorStatusWeight(status string) int {
	switch status {
	case "running":
		return 0
	case "scheduled":
		return 1
	case "paused":
		return 2
	default:
		return 3
	}
}

func printDaemonMonitorHelp() {
	fmt.Println("monitor commands:")
	fmt.Println("  monitor list")
	fmt.Println("  monitor start <monitor_id|all>")
	fmt.Println("  monitor stop <monitor_id|all>")
	fmt.Println("  monitor remove <monitor_id> <confirm_monitor_id>")
}

func defaultDaemonMonitorStatePath() string {
	return filepath.Join(daemonConfigDir, daemonMonitorStateFile)
}

func cloneDaemonMonitorRecords(records map[string]daemonMonitorRecord) map[string]daemonMonitorRecord {
	cloned := make(map[string]daemonMonitorRecord, len(records))
	for key, record := range records {
		cloned[key] = record
	}
	return cloned
}

func sortedDaemonMonitorRecordKeys(records map[string]daemonMonitorRecord) []string {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readDaemonMonitorState(path string) (map[string]daemonMonitorRecord, int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 1, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]daemonMonitorRecord{}, 1, nil
	}

	var state daemonMonitorState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, 1, err
	}

	records := make(map[string]daemonMonitorRecord, len(state.Monitors))
	nextID := state.NextID
	if nextID <= 0 {
		nextID = 1
	}
	for _, record := range state.Monitors {
		key := strings.TrimSpace(record.Key)
		commandLine := strings.TrimSpace(record.CommandLine)
		if key == "" || commandLine == "" {
			continue
		}
		if strings.TrimSpace(record.ID) == "" {
			record.ID = formatDaemonMonitorPersistentID(nextID)
			nextID++
		}
		if parsedID, ok := parseDaemonMonitorPersistentID(record.ID); ok && parsedID >= nextID {
			nextID = parsedID + 1
		}
		record.Key = key
		record.CommandLine = commandLine
		records[key] = record
	}
	return records, nextID, nil
}

func writeDaemonMonitorState(path string, records map[string]daemonMonitorRecord, nextID int64) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("monitor state path is empty")
	}
	if nextID <= 0 {
		nextID = 1
	}

	keys := sortedDaemonMonitorRecordKeys(records)
	state := daemonMonitorState{
		Version:  daemonMonitorStateVersion,
		NextID:   nextID,
		Monitors: make([]daemonMonitorRecord, 0, len(keys)),
	}
	for _, key := range keys {
		record := records[key]
		record.Key = key
		record.CommandLine = strings.TrimSpace(record.CommandLine)
		if record.CommandLine == "" {
			continue
		}
		state.Monitors = append(state.Monitors, record)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func newDaemonReadline() (*readline.Instance, error) {
	if err := os.MkdirAll(commandLogDir, 0o755); err != nil {
		return nil, err
	}

	historyPath := filepath.Join(commandLogDir, "daemon.history")
	return readline.NewEx(&readline.Config{
		Prompt:          "longtradego> ",
		HistoryFile:     historyPath,
		HistoryLimit:    5000,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		AutoComplete:    newDaemonCompleter(),
	})
}

var (
	daemonTaskIDCandidates    = emptyDaemonTaskIDs
	daemonMonitorIDCandidates = emptyDaemonMonitorIDs

	daemonRootCommandCandidates = []string{
		"quote",
		"q",
		"email",
		"mail",
		"sys",
		"shell",
		"admin",
		"task",
		"webhook",
		"booking",
		"monitor",
		"version",
		"upgrade",
		"help",
		"exit",
	}
	daemonQuoteSymbolCandidates = []string{
		"AAPL.US",
		"TSLA.US",
		"NVDA.US",
		"700.HK",
		"9988.HK",
	}
	daemonEmailFlagCandidates = []string{
		"--to",
		"--subject",
		"--body",
		"--body-file",
	}
	daemonEmailSubcommandCandidates = []string{
		"send",
		"receive",
		"monitor",
		"analyze",
	}
	daemonEmailFlagValueCandidates = map[string][]string{
		"--to":        {"recipient@example.com"},
		"--subject":   {"Notification"},
		"--body":      {"message"},
		"--body-file": {"./message.txt"},
	}
	daemonEmailReceiveFlagCandidates = []string{
		"--mail-alias",
		"--mailbox",
		"--limit",
		"--unread-only",
		"--subject-contains",
		"--from-contains",
		"--mark-seen",
		"--connect-timeout",
		"--action-cmd",
		"--action-timeout",
		"--with-body",
		"--with-files",
		"--files-dir",
		"--body-max-bytes",
	}
	daemonEmailMonitorFlagCandidates = []string{
		"--mail-alias",
		"--mailbox",
		"--limit",
		"--unread-only",
		"--subject-contains",
		"--from-contains",
		"--connect-timeout",
		"--mode",
		"--poll-interval",
		"--fallback-poll-interval",
		"--once",
		"--wait-timeout",
		"--with-body",
		"--with-files",
		"--files-dir",
		"--body-max-bytes",
		"--since-uid",
	}
	daemonEmailAnalyzeFlagCandidates = []string{
		"--mail-alias",
		"--mailbox",
		"--limit",
		"--unread-only",
		"--subject-contains",
		"--from-contains",
		"--connect-timeout",
		"--with-files",
		"--files-dir",
		"--body-max-bytes",
	}
	daemonWebhookSubcommandCandidates = []string{
		"start",
		"status",
		"stop",
		"kill-port",
		"route",
		"token",
		"sign",
		"send",
		"debug-pipeline",
		"help",
	}
	daemonWebhookServeFlagCandidates = []string{
		"--addr",
		"--path",
		"--routes-file",
		"--token-store",
		"--event-log",
		"--audit-log",
		"--dispatch-queue",
		"--dispatch-history",
		"--dead-letter",
		"--timestamp-skew",
		"--max-body-bytes",
		"--allow-sys-downstream",
		"--dispatch-workers",
		"--dispatch-poll-interval",
		"--async-max-attempts",
		"--async-retry-backoff",
		"--downstream-timeout",
	}
	daemonWebhookStartFlagCandidates = []string{
		"--addr",
		"--path",
		"--routes-file",
		"--token-store",
		"--event-log",
		"--audit-log",
		"--dispatch-queue",
		"--dispatch-history",
		"--dead-letter",
		"--timestamp-skew",
		"--max-body-bytes",
		"--allow-sys-downstream",
		"--dispatch-workers",
		"--dispatch-poll-interval",
		"--async-max-attempts",
		"--async-retry-backoff",
		"--downstream-timeout",
		"--runtime",
		"--log-file",
	}
	daemonWebhookStatusFlagCandidates = []string{
		"--runtime",
	}
	daemonWebhookStopFlagCandidates = []string{
		"--runtime",
		"--timeout",
	}
	daemonWebhookKillPortFlagCandidates = []string{
		"--addr",
		"--timeout",
		"--runtime",
		"--dry-run",
	}
	daemonWebhookSignFlagCandidates = []string{
		"--third-party-id",
		"--token",
		"--data",
		"--data-file",
		"--timestamp",
		"--path",
		"--url",
	}
	daemonWebhookSendFlagCandidates = []string{
		"--third-party-id",
		"--token",
		"--data",
		"--data-file",
		"--timestamp",
		"--url",
		"--timeout",
	}
	daemonWebhookDebugPipelineFlagCandidates = []string{
		"--audit-log",
		"--event-id",
		"--route-id",
	}
	daemonWebhookTokenSubcommandCandidates = []string{
		"generate",
		"query",
		"reset",
	}
	daemonWebhookRouteSubcommandCandidates = []string{
		"add",
		"list",
		"update",
		"remove",
	}
	daemonWebhookRouteFlagCandidates = []string{
		"--routes-file",
		"--path",
		"--mode",
		"--pipeline",
		"--enabled",
		"--max-attempts",
		"--retry-backoff",
		"--timeout",
	}
	daemonWebhookTokenFlagCandidates = []string{
		"--token-store",
	}
	daemonUpgradeFlagCandidates = []string{
		"--version",
		"--yes",
		"--dry-run",
	}
	daemonUpgradeSubcommandCandidates = []string{
		"check",
		"help",
	}
	daemonBookingSubcommandCandidates = []string{
		"product",
		"slot",
		"reservation",
		"query",
		"service",
		"help",
	}
	daemonBookingGlobalFlagCandidates = []string{
		"--catalog",
		"--reservations",
	}
	daemonBookingProductSubcommandCandidates = []string{
		"add",
		"update",
		"list",
		"remove",
	}
	daemonBookingProductFlagCandidates = []string{
		"--id",
		"--name",
		"--description",
		"--enabled",
	}
	daemonBookingSlotSubcommandCandidates = []string{
		"add",
		"update",
		"list",
		"remove",
	}
	daemonBookingSlotUpsertFlagCandidates = []string{
		"--id",
		"--product-id",
		"--start",
		"--end",
		"--capacity",
		"--enabled",
	}
	daemonBookingSlotListFlagCandidates = []string{
		"--product-id",
		"--from",
		"--to",
		"--include-full",
		"--include-disabled",
	}
	daemonBookingReservationSubcommandCandidates = []string{
		"create",
		"list",
		"confirm",
		"reject",
		"cancel",
	}
	daemonBookingReservationCreateFlagCandidates = []string{
		"--product-id",
		"--slot-id",
		"--user-id",
		"--party-size",
		"--contact-name",
		"--contact-phone",
		"--member",
		"--special-requirements",
	}
	daemonBookingReservationListFlagCandidates = []string{
		"--user-id",
		"--status",
	}
	daemonBookingReservationActionFlagCandidates = []string{
		"--note",
	}
	daemonBookingQueryFlagCandidates = []string{
		"--product-id",
		"--from",
		"--to",
		"--include-full",
	}
	daemonBookingServiceSubcommandCandidates = []string{
		"start",
		"stop",
		"status",
		"help",
	}
	daemonBookingServiceStartFlagCandidates = []string{
		"--addr",
		"--runtime",
		"--log-file",
		"--drafts",
		"--api-keys",
		"--llm-config",
		"--admin-auth",
		"--max-port-fallback",
	}
	daemonBookingServiceStopFlagCandidates = []string{
		"--runtime",
		"--timeout",
	}
	daemonBookingServiceStatusFlagCandidates = []string{
		"--runtime",
	}
)

func newDaemonCompleter() readline.AutoCompleter {
	return readline.SegmentFunc(func(segments [][]rune, _ int) [][]rune {
		return daemonCompletionCandidates(segments)
	})
}

func daemonCompletionCandidates(segments [][]rune) [][]rune {
	stageParts := pipelineStageCompletionParts(segments)
	if len(stageParts) == 0 {
		return stringCandidatesToRunes(daemonRootCommandCandidates)
	}

	segmentIndex := len(stageParts) - 1
	if segmentIndex == 0 {
		return stringCandidatesToRunes(daemonRootCommandCandidates)
	}

	switch strings.ToLower(stageParts[0]) {
	case "quote", "q":
		return stringCandidatesToRunes(daemonQuoteSymbolCandidates)
	case "email", "mail":
		return emailCompletionCandidates(stageParts)
	case "sys", "shell":
		return systemCompletionCandidates(stageParts)
	case "admin":
		return adminCompletionCandidates(stageParts)
	case "task":
		return taskCompletionCandidates(stageParts)
	case "monitor":
		return monitorCompletionCandidates(stageParts)
	case "webhook":
		return webhookCompletionCandidates(stageParts)
	case "booking":
		return bookingCompletionCandidates(stageParts)
	case "version":
		return nil
	case "upgrade":
		return upgradeCompletionCandidates(stageParts)
	default:
		return nil
	}
}

func emptyDaemonTaskIDs() []string {
	return nil
}

func emptyDaemonMonitorIDs() []string {
	return nil
}

func pipelineStageCompletionParts(segments [][]rune) []string {
	if len(segments) == 0 {
		return []string{""}
	}

	stageParts := make([]string, 0, len(segments))
	for _, segment := range segments {
		for _, token := range splitCompletionTokenByPipe(string(segment)) {
			if token == "|" {
				stageParts = stageParts[:0]
				continue
			}
			stageParts = append(stageParts, token)
		}
	}

	if len(stageParts) == 0 {
		return []string{""}
	}
	return normalizeCompletionStageParts(stageParts)
}

func normalizeCompletionStageParts(parts []string) []string {
	if len(parts) == 0 {
		return []string{""}
	}

	keepTrailingEmpty := parts[len(parts)-1] == ""
	normalized := make([]string, 0, len(parts))
	for index, part := range parts {
		if part == "" {
			if keepTrailingEmpty && index == len(parts)-1 {
				normalized = append(normalized, part)
			}
			continue
		}
		normalized = append(normalized, part)
	}

	if len(normalized) == 0 {
		return []string{""}
	}
	return normalized
}

func splitCompletionTokenByPipe(token string) []string {
	parts := make([]string, 0, 3)
	var current strings.Builder
	for _, r := range token {
		if r == '|' {
			parts = append(parts, current.String())
			current.Reset()
			parts = append(parts, "|")
			continue
		}
		current.WriteRune(r)
	}
	parts = append(parts, current.String())
	return parts
}

func emailCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		return stringCandidatesToRunes(daemonEmailSubcommandCandidates)
	}

	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "send":
		argsAfterSend := parts[2:]
		current := argsAfterSend[len(argsAfterSend)-1]
		completed := argsAfterSend[:len(argsAfterSend)-1]

		usedFlags, awaitingValueFor := parseEmailCompletionState(completed)
		if awaitingValueFor != "" {
			return stringCandidatesToRunes(daemonEmailFlagValueCandidates[awaitingValueFor])
		}

		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(remainingEmailFlags(usedFlags))
		}
		return nil
	case "receive", "recv", "inbox":
		return emailReceiveCompletionCandidates(parts[2:])
	case "monitor", "watch":
		return emailMonitorCompletionCandidates(parts[2:])
	case "analyze":
		return emailAnalyzeCompletionCandidates(parts[2:])
	default:
		return stringCandidatesToRunes(daemonEmailSubcommandCandidates)
	}
}

func emailReceiveCompletionCandidates(args []string) [][]rune {
	if len(args) == 0 {
		return stringCandidatesToRunes(daemonEmailReceiveFlagCandidates)
	}
	current := args[len(args)-1]
	if current == "" || strings.HasPrefix(current, "--") {
		return stringCandidatesToRunes(daemonEmailReceiveFlagCandidates)
	}
	return nil
}

func emailMonitorCompletionCandidates(args []string) [][]rune {
	controlCandidates := []string{"list", "start", "stop", "remove", "help"}
	if len(args) == 0 {
		candidates := append([]string{}, controlCandidates...)
		candidates = append(candidates, daemonEmailMonitorFlagCandidates...)
		return stringCandidatesToRunes(candidates)
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	switch sub {
	case "list", "ls", "help":
		return nil
	case "start", "resume":
		if len(args) <= 1 || strings.TrimSpace(args[1]) == "" {
			candidates := append([]string{"all"}, daemonMonitorIDCandidates()...)
			return stringCandidatesToRunes(candidates)
		}
		return nil
	case "stop", "kill":
		if len(args) <= 1 || strings.TrimSpace(args[1]) == "" {
			candidates := append([]string{"all"}, daemonMonitorIDCandidates()...)
			return stringCandidatesToRunes(candidates)
		}
		return nil
	case "remove", "delete", "rm":
		if len(args) <= 1 || strings.TrimSpace(args[1]) == "" {
			return stringCandidatesToRunes(daemonMonitorIDCandidates())
		}
		if len(args) == 2 || strings.TrimSpace(args[2]) == "" {
			target := strings.TrimSpace(args[1])
			if target == "" {
				return nil
			}
			return stringCandidatesToRunes([]string{target})
		}
		return nil
	}
	current := args[len(args)-1]
	if current == "" {
		candidates := append([]string{}, controlCandidates...)
		candidates = append(candidates, daemonEmailMonitorFlagCandidates...)
		return stringCandidatesToRunes(candidates)
	}
	if strings.HasPrefix(current, "--") {
		return stringCandidatesToRunes(daemonEmailMonitorFlagCandidates)
	}
	return nil
}

func emailAnalyzeCompletionCandidates(args []string) [][]rune {
	if len(args) == 0 {
		return stringCandidatesToRunes(daemonEmailAnalyzeFlagCandidates)
	}
	current := args[len(args)-1]
	if current == "" || strings.HasPrefix(current, "--") {
		return stringCandidatesToRunes(daemonEmailAnalyzeFlagCandidates)
	}
	return nil
}

func webhookCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		return stringCandidatesToRunes(daemonWebhookSubcommandCandidates)
	}

	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	args := parts[2:]
	current := args[len(args)-1]

	switch sub {
	case "start":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookStartFlagCandidates)
		}
		return nil
	case "serve":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookServeFlagCandidates)
		}
		return nil
	case "status":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookStatusFlagCandidates)
		}
		return nil
	case "stop":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookStopFlagCandidates)
		}
		return nil
	case "kill-port":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookKillPortFlagCandidates)
		}
		return nil
	case "sign":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookSignFlagCandidates)
		}
		return nil
	case "send":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookSendFlagCandidates)
		}
		return nil
	case "debug-pipeline":
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes(daemonWebhookDebugPipelineFlagCandidates)
		}
		return nil
	case "route":
		if len(parts) <= 3 {
			candidates := append([]string{}, daemonWebhookRouteSubcommandCandidates...)
			candidates = append(candidates, daemonWebhookRouteFlagCandidates...)
			return stringCandidatesToRunes(candidates)
		}
		routeSub := strings.ToLower(strings.TrimSpace(parts[2]))
		if routeSub == "" || strings.HasPrefix(routeSub, "--") {
			candidates := append([]string{}, daemonWebhookRouteSubcommandCandidates...)
			candidates = append(candidates, daemonWebhookRouteFlagCandidates...)
			return stringCandidatesToRunes(candidates)
		}
		switch routeSub {
		case "add", "update", "list":
			if current == "" || strings.HasPrefix(current, "--") {
				return stringCandidatesToRunes(daemonWebhookRouteFlagCandidates)
			}
		case "remove":
			if current == "" || strings.HasPrefix(current, "--") {
				return stringCandidatesToRunes([]string{"<route-id>"})
			}
		}
		return nil
	case "token":
		if len(parts) <= 3 {
			candidates := append([]string{}, daemonWebhookTokenSubcommandCandidates...)
			candidates = append(candidates, daemonWebhookTokenFlagCandidates...)
			return stringCandidatesToRunes(candidates)
		}
		tokenSub := strings.ToLower(strings.TrimSpace(parts[2]))
		if tokenSub == "" || strings.HasPrefix(tokenSub, "--") {
			candidates := append([]string{}, daemonWebhookTokenSubcommandCandidates...)
			candidates = append(candidates, daemonWebhookTokenFlagCandidates...)
			return stringCandidatesToRunes(candidates)
		}
		switch tokenSub {
		case "generate", "query", "reset":
			if current == "" || strings.HasPrefix(current, "--") {
				return stringCandidatesToRunes(daemonWebhookTokenFlagCandidates)
			}
		}
		return nil
	case "help":
		return nil
	default:
		return stringCandidatesToRunes(daemonWebhookSubcommandCandidates)
	}
}

func taskCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		return stringCandidatesToRunes([]string{"add", "list", "pause", "global-pause", "resume", "global-resume", "remove", "help"})
	}

	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	switch sub {
	case "add":
		// task add --every <duration> [--auto-resume] -- <command line>
		// task add --cron "<expr>" [--auto-resume] -- <command line>
		if len(parts) <= 3 {
			return stringCandidatesToRunes([]string{"--every", "--cron", "--auto-resume", "1m", "5m", "*/5 * * * *"})
		}
		return nil
	case "pause", "resume":
		if len(parts) <= 3 || strings.TrimSpace(parts[2]) == "" {
			return stringCandidatesToRunes(daemonTaskIDCandidates())
		}
		return nil
	case "remove", "delete", "rm":
		if len(parts) <= 3 || strings.TrimSpace(parts[2]) == "" {
			return stringCandidatesToRunes(daemonTaskIDCandidates())
		}
		if len(parts) == 4 {
			taskID := strings.TrimSpace(parts[2])
			if taskID == "" {
				return nil
			}
			return stringCandidatesToRunes([]string{taskID})
		}
		return nil
	case "global-pause", "global-resume":
		// These commands don't take arguments
		return nil
	default:
		return nil
	}
}

func monitorCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		return stringCandidatesToRunes([]string{"list", "start", "stop", "remove", "help"})
	}
	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	switch sub {
	case "start", "resume":
		if len(parts) <= 3 || strings.TrimSpace(parts[2]) == "" {
			candidates := append([]string{"all"}, daemonMonitorIDCandidates()...)
			return stringCandidatesToRunes(candidates)
		}
		return nil
	case "stop", "kill":
		if len(parts) <= 3 || strings.TrimSpace(parts[2]) == "" {
			candidates := append([]string{"all"}, daemonMonitorIDCandidates()...)
			return stringCandidatesToRunes(candidates)
		}
		return nil
	case "remove", "delete", "rm":
		if len(parts) <= 3 || strings.TrimSpace(parts[2]) == "" {
			return stringCandidatesToRunes(daemonMonitorIDCandidates())
		}
		if len(parts) == 4 || strings.TrimSpace(parts[3]) == "" {
			target := strings.TrimSpace(parts[2])
			if target == "" {
				return nil
			}
			return stringCandidatesToRunes([]string{target})
		}
		return nil
	case "list", "ls", "help":
		return nil
	default:
		return nil
	}
}

func systemCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		return stringCandidatesToRunes([]string{"--shell", "--timeout", "--"})
	}
	return nil
}

func adminCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		return stringCandidatesToRunes([]string{"status", "--runtime", "help"})
	}
	current := strings.TrimSpace(parts[len(parts)-1])
	if current == "" || strings.HasPrefix(current, "--") {
		return stringCandidatesToRunes([]string{"--runtime"})
	}
	return stringCandidatesToRunes([]string{"status", "help"})
}

func bookingCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		candidates := append([]string{}, daemonBookingSubcommandCandidates...)
		candidates = append(candidates, daemonBookingGlobalFlagCandidates...)
		return stringCandidatesToRunes(candidates)
	}

	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	current := strings.TrimSpace(parts[len(parts)-1])
	candidateWithGlobals := func(values []string) [][]rune {
		candidates := append([]string{}, values...)
		candidates = append(candidates, daemonBookingGlobalFlagCandidates...)
		return stringCandidatesToRunes(candidates)
	}

	switch sub {
	case "product":
		if len(parts) <= 3 {
			candidates := append([]string{}, daemonBookingProductSubcommandCandidates...)
			candidates = append(candidates, daemonBookingProductFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		productSub := strings.ToLower(strings.TrimSpace(parts[2]))
		if productSub == "" || strings.HasPrefix(productSub, "--") {
			candidates := append([]string{}, daemonBookingProductSubcommandCandidates...)
			candidates = append(candidates, daemonBookingProductFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		switch productSub {
		case "add", "update":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingProductFlagCandidates)
			}
		}
		return nil
	case "slot":
		if len(parts) <= 3 {
			candidates := append([]string{}, daemonBookingSlotSubcommandCandidates...)
			candidates = append(candidates, daemonBookingSlotUpsertFlagCandidates...)
			candidates = append(candidates, daemonBookingSlotListFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		slotSub := strings.ToLower(strings.TrimSpace(parts[2]))
		if slotSub == "" || strings.HasPrefix(slotSub, "--") {
			candidates := append([]string{}, daemonBookingSlotSubcommandCandidates...)
			candidates = append(candidates, daemonBookingSlotUpsertFlagCandidates...)
			candidates = append(candidates, daemonBookingSlotListFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		switch slotSub {
		case "add", "update":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingSlotUpsertFlagCandidates)
			}
		case "list":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingSlotListFlagCandidates)
			}
		case "remove":
			if current == "" || strings.HasPrefix(current, "--") {
				return stringCandidatesToRunes([]string{"<slot-id>"})
			}
		}
		return nil
	case "reservation":
		if len(parts) <= 3 {
			candidates := append([]string{}, daemonBookingReservationSubcommandCandidates...)
			candidates = append(candidates, daemonBookingReservationCreateFlagCandidates...)
			candidates = append(candidates, daemonBookingReservationListFlagCandidates...)
			candidates = append(candidates, daemonBookingReservationActionFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		reservationSub := strings.ToLower(strings.TrimSpace(parts[2]))
		if reservationSub == "" || strings.HasPrefix(reservationSub, "--") {
			candidates := append([]string{}, daemonBookingReservationSubcommandCandidates...)
			candidates = append(candidates, daemonBookingReservationCreateFlagCandidates...)
			candidates = append(candidates, daemonBookingReservationListFlagCandidates...)
			candidates = append(candidates, daemonBookingReservationActionFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		switch reservationSub {
		case "create":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingReservationCreateFlagCandidates)
			}
		case "list":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingReservationListFlagCandidates)
			}
		case "confirm", "reject", "cancel":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingReservationActionFlagCandidates)
			}
			if len(parts) <= 4 {
				return stringCandidatesToRunes([]string{"<reservation-id>"})
			}
		}
		return nil
	case "query":
		if current == "" || strings.HasPrefix(current, "--") {
			return candidateWithGlobals(daemonBookingQueryFlagCandidates)
		}
		return nil
	case "service":
		if len(parts) <= 3 {
			candidates := append([]string{}, daemonBookingServiceSubcommandCandidates...)
			candidates = append(candidates, daemonBookingServiceStartFlagCandidates...)
			candidates = append(candidates, daemonBookingServiceStopFlagCandidates...)
			candidates = append(candidates, daemonBookingServiceStatusFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		serviceSub := strings.ToLower(strings.TrimSpace(parts[2]))
		if serviceSub == "" || strings.HasPrefix(serviceSub, "--") {
			candidates := append([]string{}, daemonBookingServiceSubcommandCandidates...)
			candidates = append(candidates, daemonBookingServiceStartFlagCandidates...)
			candidates = append(candidates, daemonBookingServiceStopFlagCandidates...)
			candidates = append(candidates, daemonBookingServiceStatusFlagCandidates...)
			return candidateWithGlobals(candidates)
		}
		switch serviceSub {
		case "start":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingServiceStartFlagCandidates)
			}
		case "stop":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingServiceStopFlagCandidates)
			}
		case "status":
			if current == "" || strings.HasPrefix(current, "--") {
				return candidateWithGlobals(daemonBookingServiceStatusFlagCandidates)
			}
		}
		return nil
	case "help":
		return nil
	default:
		candidates := append([]string{}, daemonBookingSubcommandCandidates...)
		candidates = append(candidates, daemonBookingGlobalFlagCandidates...)
		return stringCandidatesToRunes(candidates)
	}
}

func upgradeCompletionCandidates(parts []string) [][]rune {
	if len(parts) <= 2 {
		candidates := append([]string{}, daemonUpgradeSubcommandCandidates...)
		candidates = append(candidates, daemonUpgradeFlagCandidates...)
		return stringCandidatesToRunes(candidates)
	}
	current := strings.TrimSpace(parts[len(parts)-1])
	sub := strings.ToLower(strings.TrimSpace(parts[1]))

	if sub == "check" {
		if current == "" || strings.HasPrefix(current, "--") {
			return stringCandidatesToRunes([]string{"--version"})
		}
		return nil
	}
	if current == "" || strings.HasPrefix(current, "--") {
		return stringCandidatesToRunes(daemonUpgradeFlagCandidates)
	}
	return nil
}

func rewriteDaemonWebhookServeToStart(args []string) ([]string, bool) {
	if len(args) < 2 {
		return nil, false
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	second := strings.ToLower(strings.TrimSpace(args[1]))
	if first != "webhook" || second != "serve" {
		return nil, false
	}
	rewritten := make([]string, 0, len(args))
	rewritten = append(rewritten, "webhook", "start")
	if len(args) > 2 {
		rewritten = append(rewritten, args[2:]...)
	}
	return rewritten, true
}

func rewriteDaemonBookingServeToStart(args []string) ([]string, bool) {
	if len(args) < 3 {
		return nil, false
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	second := strings.ToLower(strings.TrimSpace(args[1]))
	third := strings.ToLower(strings.TrimSpace(args[2]))
	if first != "booking" || second != "service" || third != "serve" {
		return nil, false
	}
	rewritten := make([]string, 0, len(args))
	rewritten = append(rewritten, "booking", "service", "start")
	if len(args) > 3 {
		rewritten = append(rewritten, args[3:]...)
	}
	return rewritten, true
}

func newDaemonWebhookOwnerSessionID() string {
	return newDaemonOpaqueToken("daemon")
}

func newDaemonWebhookOwnerStartToken() string {
	return newDaemonOpaqueToken("owner")
}

func newDaemonOpaqueToken(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), hex.EncodeToString(buf))
}

func isDaemonWebhookStartArgs(args []string) bool {
	if len(args) < 2 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(args[0]), "webhook") && strings.EqualFold(strings.TrimSpace(args[1]), "start")
}

func buildDaemonWebhookStartOwnerClaim(args []string, daemonSessionID string) (webhookStartOwnerClaim, bool) {
	if !isDaemonWebhookStartArgs(args) {
		return webhookStartOwnerClaim{}, false
	}
	sessionID := strings.TrimSpace(daemonSessionID)
	if sessionID == "" {
		return webhookStartOwnerClaim{}, false
	}
	return webhookStartOwnerClaim{
		SessionID:  sessionID,
		DaemonPID:  os.Getpid(),
		ClaimedAt:  time.Now(),
		StartToken: newDaemonWebhookOwnerStartToken(),
	}, true
}

func printDaemonWebhookStartupHint(out io.Writer) {
	printDaemonWebhookStartupHintWithPath(defaultWebhookRuntimeStatePath(), out)
}

func printDaemonWebhookStartupHintWithPath(runtimePath string, out io.Writer) {
	if out == nil {
		return
	}
	status, runtime, err := webhookRuntimeStatus(runtimePath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daemon webhook runtime check failed: %v\n", err)
		return
	}
	switch status {
	case "running":
		if runtime == nil {
			return
		}
		ownerSummary := buildDaemonWebhookRuntimeOwnerSummary(runtime)
		_, _ = fmt.Fprintf(out, "daemon webhook runtime: running pid=%d addr=%s path=%s owner=%s\n", runtime.PID, runtime.Address, runtime.Path, ownerSummary)
	case "stale":
		pid := 0
		addr := ""
		path := ""
		if runtime != nil {
			pid = runtime.PID
			addr = runtime.Address
			path = runtime.Path
		}
		_, _ = fmt.Fprintf(out, "daemon webhook runtime: stale pid=%d addr=%s path=%s file=%s; run `webhook stop` to clean up\n", pid, addr, path, runtimePath)
	}
}

func buildDaemonWebhookRuntimeOwnerSummary(runtime *webhookRuntimeInfo) string {
	if runtime == nil {
		return "none"
	}
	sessionID := strings.TrimSpace(runtime.OwnerSessionID)
	startToken := strings.TrimSpace(runtime.OwnerStartToken)
	daemonPID := runtime.OwnerDaemonPID
	claimedAt := strings.TrimSpace(runtime.OwnerClaimedAt)
	if sessionID == "" && startToken == "" && daemonPID == 0 && claimedAt == "" {
		return "none"
	}
	parts := make([]string, 0, 4)
	if sessionID != "" {
		parts = append(parts, "session="+sessionID)
	}
	if daemonPID > 0 {
		parts = append(parts, "daemon_pid="+strconv.Itoa(daemonPID))
	}
	if claimedAt != "" {
		parts = append(parts, "claimed_at="+claimedAt)
	}
	if startToken != "" {
		parts = append(parts, "start_token="+startToken)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func trackDaemonOwnedWebhookLifecycle(args []string, result any, ownedPIDs map[int]daemonOwnedWebhookRuntime, daemonSessionID string) {
	if len(args) < 2 {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(args[0]), "webhook") {
		return
	}

	sub := strings.ToLower(strings.TrimSpace(args[1]))
	switch sub {
	case "start":
		startResult, ok := result.(webhookStartResult)
		if !ok {
			return
		}
		status := strings.ToLower(strings.TrimSpace(startResult.Status))
		if status != "started" {
			return
		}
		if startResult.Runtime == nil || startResult.Runtime.PID <= 0 {
			return
		}
		if strings.TrimSpace(startResult.Runtime.OwnerSessionID) != strings.TrimSpace(daemonSessionID) {
			return
		}
		startToken := strings.TrimSpace(startResult.Runtime.OwnerStartToken)
		if startToken == "" {
			return
		}
		ownedPIDs[startResult.Runtime.PID] = daemonOwnedWebhookRuntime{StartToken: startToken}
	case "stop":
		stopResult, ok := result.(webhookStopResult)
		if !ok {
			return
		}
		if stopResult.PID > 0 {
			delete(ownedPIDs, stopResult.PID)
		}
		switch strings.ToLower(strings.TrimSpace(stopResult.Status)) {
		case "stopped", "stale_removed", "not_running":
			for pid := range ownedPIDs {
				delete(ownedPIDs, pid)
			}
		}
	}
}

func stopDaemonOwnedWebhookOnDaemonExit(ownedPIDs map[int]daemonOwnedWebhookRuntime, daemonSessionID string, out io.Writer) {
	stopDaemonOwnedWebhookOnDaemonExitWithPath(ownedPIDs, defaultWebhookRuntimeStatePath(), daemonSessionID, out)
}

func stopDaemonOwnedWebhookOnDaemonExitWithPath(ownedPIDs map[int]daemonOwnedWebhookRuntime, runtimePath string, daemonSessionID string, out io.Writer) {
	if len(ownedPIDs) == 0 {
		return
	}
	runtime, exists, err := readWebhookRuntimeState(runtimePath)
	if err != nil {
		if out != nil {
			_, _ = fmt.Fprintf(out, "daemon webhook cleanup skipped: %v\n", err)
		}
		return
	}
	if !exists || runtime == nil || runtime.PID <= 0 {
		return
	}
	owned, ok := ownedPIDs[runtime.PID]
	if !ok {
		return
	}
	if strings.TrimSpace(runtime.OwnerSessionID) != strings.TrimSpace(daemonSessionID) || strings.TrimSpace(runtime.OwnerStartToken) != strings.TrimSpace(owned.StartToken) {
		if out != nil {
			_, _ = fmt.Fprintf(out, "daemon exit webhook cleanup skip: not owned by current daemon (pid=%d)\n", runtime.PID)
		}
		return
	}
	stopResult, stopErr := stopWebhook(runtimePath, defaultWebhookStopTimeout)
	if stopErr != nil {
		if out != nil {
			_, _ = fmt.Fprintf(out, "daemon webhook cleanup failed: %v\n", stopErr)
		}
		return
	}
	if out == nil {
		return
	}
	if stopResult.PID > 0 {
		_, _ = fmt.Fprintf(out, "daemon exit stopped webhook pid=%d\n", stopResult.PID)
		return
	}
	_, _ = fmt.Fprintf(out, "daemon exit webhook cleanup status=%s\n", stopResult.Status)
}

func parseEmailCompletionState(args []string) (map[string]struct{}, string) {
	usedFlags := make(map[string]struct{}, len(daemonEmailFlagCandidates))
	var awaitingValueFor string

	for _, token := range args {
		if awaitingValueFor != "" {
			awaitingValueFor = ""
			continue
		}

		if _, ok := daemonEmailFlagValueCandidates[token]; ok {
			usedFlags[token] = struct{}{}
			awaitingValueFor = token
		}
	}

	return usedFlags, awaitingValueFor
}

func remainingEmailFlags(usedFlags map[string]struct{}) []string {
	remaining := make([]string, 0, len(daemonEmailFlagCandidates))
	for _, flag := range daemonEmailFlagCandidates {
		if _, exists := usedFlags[flag]; exists {
			continue
		}
		remaining = append(remaining, flag)
	}
	if len(remaining) == 0 {
		return daemonEmailFlagCandidates
	}
	return remaining
}

func stringCandidatesToRunes(candidates []string) [][]rune {
	out := make([][]rune, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, []rune(candidate))
	}
	return out
}

func commandTemplate(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}

	first := strings.ToLower(strings.TrimSpace(args[0]))
	switch first {
	case "quote", "q":
		if len(args) == 1 {
			return "quote AAPL.US TSLA.US 700.HK", true
		}
	case "email", "mail":
		if len(args) == 1 {
			return `email send --to recipient@example.com --subject "Notification" --body "message"`, true
		}
		if len(args) == 2 && strings.EqualFold(args[1], "send") {
			return `email send --to recipient@example.com --subject "Notification" --body "message"`, true
		}
	case "task":
		if len(args) == 1 {
			return `task add --every 1m -- quote AAPL.US`, true
		}
	case "admin":
		if len(args) == 1 {
			return `admin status`, true
		}
	case "sys", "shell":
		if len(args) == 1 {
			return `sys --shell "uname -a"`, true
		}
	}
	return "", false
}

func parsePipelineCommandLine(input string) ([][]string, error) {
	segments, err := splitPipelineSegments(input)
	if err != nil {
		return nil, err
	}

	commands := make([][]string, 0, len(segments))
	for index, segment := range segments {
		args, parseErr := parseCommandLine(segment)
		if parseErr != nil {
			return nil, fmt.Errorf("pipeline stage %d: %w", index+1, parseErr)
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("pipeline stage %d is empty", index+1)
		}
		commands = append(commands, args)
	}
	return commands, nil
}

func splitPipelineSegments(input string) ([]string, error) {
	var (
		segments []string
		current  strings.Builder
		inSingle bool
		inDouble bool
		escaped  bool
	)

	pushCurrent := func() error {
		segment := strings.TrimSpace(current.String())
		current.Reset()
		if segment == "" {
			return fmt.Errorf("empty pipeline stage")
		}
		segments = append(segments, segment)
		return nil
	}

	for _, ch := range input {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}

		if ch == '\\' && !inSingle {
			escaped = true
			current.WriteRune(ch)
			continue
		}

		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			current.WriteRune(ch)
			continue
		}

		if inDouble {
			if ch == '"' {
				inDouble = false
			}
			current.WriteRune(ch)
			continue
		}

		switch ch {
		case '\'':
			inSingle = true
			current.WriteRune(ch)
		case '"':
			inDouble = true
			current.WriteRune(ch)
		case '|':
			if err := pushCurrent(); err != nil {
				return nil, err
			}
		default:
			current.WriteRune(ch)
		}
	}

	if escaped {
		return nil, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unclosed quote")
	}
	if err := pushCurrent(); err != nil {
		return nil, err
	}

	return segments, nil
}

func parseCommandLine(input string) ([]string, error) {
	var (
		args     []string
		current  strings.Builder
		inSingle bool
		inDouble bool
		escaped  bool
	)

	pushCurrent := func() {
		if current.Len() == 0 {
			return
		}
		args = append(args, current.String())
		current.Reset()
	}

	for _, ch := range input {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}

		if ch == '\\' && !inSingle {
			escaped = true
			continue
		}

		if inSingle {
			if ch == '\'' {
				inSingle = false
			} else {
				current.WriteRune(ch)
			}
			continue
		}

		if inDouble {
			if ch == '"' {
				inDouble = false
			} else {
				current.WriteRune(ch)
			}
			continue
		}

		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ' ', '\t', '\n', '\r':
			pushCurrent()
		default:
			current.WriteRune(ch)
		}
	}

	if escaped {
		return nil, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unclosed quote")
	}

	pushCurrent()
	return args, nil
}
