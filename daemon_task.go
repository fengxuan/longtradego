package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	cronv3 "github.com/robfig/cron/v3"
)

const (
	daemonTaskCommandName    = "task"
	daemonTaskStateVersion   = 1
	daemonTaskHistoryVersion = 1
	daemonDataDir            = "data"
	daemonConfigDir          = "conf"
	daemonTaskStateFile      = "daemon_tasks.json"
	daemonTaskHistoryFile    = "daemon_tasks_history.json"
)

type daemonTaskRunner func(context.Context, [][]string) error

type daemonTaskManager struct {
	mu             sync.Mutex
	runner         daemonTaskRunner
	tasks          map[string]*daemonTask
	nextID         int64
	statePath      string
	historyPath    string
	wg             sync.WaitGroup
	globallyPaused bool
}

type daemonTask struct {
	id          string
	schedule    daemonTaskSchedule
	every       time.Duration
	commandLine string
	commands    [][]string
	createdAt   time.Time
	nextRunAt   time.Time
	lastRunAt   time.Time
	lastStatus  string
	lastError   string
	runCount    int64
	paused      bool
	running     bool
	cancel      context.CancelFunc
	wake        chan struct{}
}

type daemonTaskSnapshot struct {
	ID          string
	Schedule    string
	Every       time.Duration
	CommandLine string
	CreatedAt   time.Time
	NextRunAt   time.Time
	LastRunAt   time.Time
	LastStatus  string
	LastError   string
	RunCount    int64
	Paused      bool
	Running     bool
}

func newDaemonTaskManager(runner daemonTaskRunner) *daemonTaskManager {
	return newDaemonTaskManagerWithPaths(runner, "", "")
}

func newDaemonTaskManagerWithStatePath(runner daemonTaskRunner, statePath string) *daemonTaskManager {
	return newDaemonTaskManagerWithPaths(runner, statePath, deriveDaemonTaskHistoryPath(statePath))
}

func newDaemonTaskManagerWithPaths(runner daemonTaskRunner, statePath string, historyPath string) *daemonTaskManager {
	return &daemonTaskManager{
		runner:      runner,
		tasks:       make(map[string]*daemonTask),
		statePath:   strings.TrimSpace(statePath),
		historyPath: strings.TrimSpace(historyPath),
	}
}

type daemonTaskState struct {
	Version        int                `json:"version"`
	NextID         int64              `json:"next_id"`
	Tasks          []daemonTaskRecord `json:"tasks"`
	GloballyPaused bool               `json:"globally_paused,omitempty"`
}

type daemonTaskRecord struct {
	ID          string `json:"id"`
	Schedule    string `json:"schedule,omitempty"`
	Every       string `json:"every"`
	CommandLine string `json:"command_line"`
	CreatedAt   string `json:"created_at,omitempty"`
	LastRunAt   string `json:"last_run_at,omitempty"`
	LastStatus  string `json:"last_status,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	RunCount    int64  `json:"run_count,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
}

type daemonTaskHistory struct {
	Version int                      `json:"version"`
	Entries []daemonTaskHistoryEntry `json:"entries"`
}

type daemonTaskHistoryEntry struct {
	ID          string `json:"id"`
	Schedule    string `json:"schedule,omitempty"`
	Every       string `json:"every"`
	CommandLine string `json:"command_line"`
	CreatedAt   string `json:"created_at,omitempty"`
	LastRunAt   string `json:"last_run_at,omitempty"`
	LastStatus  string `json:"last_status,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	RunCount    int64  `json:"run_count,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
	RemovedAt   string `json:"removed_at"`
}

type daemonTaskScheduleKind string

const (
	daemonTaskScheduleEvery daemonTaskScheduleKind = "every"
	daemonTaskScheduleCron  daemonTaskScheduleKind = "cron"
)

var daemonTaskCronParser = cronv3.NewParser(
	cronv3.SecondOptional | cronv3.Minute | cronv3.Hour | cronv3.Dom | cronv3.Month | cronv3.Dow | cronv3.Descriptor,
)

type daemonTaskSchedule struct {
	Kind     daemonTaskScheduleKind
	Every    time.Duration
	CronExpr string
	Cron     cronv3.Schedule
}

func (s daemonTaskSchedule) Display() string {
	switch s.Kind {
	case daemonTaskScheduleCron:
		return "cron:" + strings.TrimSpace(s.CronExpr)
	default:
		if s.Every > 0 {
			return "every:" + s.Every.String()
		}
		return "every:0s"
	}
}

func (s daemonTaskSchedule) Persist() string {
	return s.Display()
}

func (s daemonTaskSchedule) LegacyEveryText() string {
	if s.Kind == daemonTaskScheduleEvery && s.Every > 0 {
		return s.Every.String()
	}
	return ""
}

func (s daemonTaskSchedule) Next(from time.Time) time.Time {
	switch s.Kind {
	case daemonTaskScheduleCron:
		if s.Cron == nil {
			return time.Time{}
		}
		return s.Cron.Next(from)
	default:
		if s.Every <= 0 {
			return time.Time{}
		}
		return from.Add(s.Every)
	}
}

func parseCronSchedule(spec string) (cronv3.Schedule, error) {
	return daemonTaskCronParser.Parse(spec)
}

func newEveryTaskSchedule(every time.Duration) (daemonTaskSchedule, error) {
	if every <= 0 {
		return daemonTaskSchedule{}, fmt.Errorf("task interval must be greater than 0")
	}
	return daemonTaskSchedule{
		Kind:  daemonTaskScheduleEvery,
		Every: every,
	}, nil
}

func newCronTaskSchedule(spec string) (daemonTaskSchedule, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return daemonTaskSchedule{}, fmt.Errorf("missing cron expression")
	}
	schedule, err := parseCronSchedule(trimmed)
	if err != nil {
		return daemonTaskSchedule{}, fmt.Errorf("invalid cron expression %q: %w", trimmed, err)
	}
	return daemonTaskSchedule{
		Kind:     daemonTaskScheduleCron,
		CronExpr: trimmed,
		Cron:     schedule,
	}, nil
}

func parsePersistedTaskSchedule(scheduleText string, everyText string) (daemonTaskSchedule, error) {
	scheduleRaw := strings.TrimSpace(scheduleText)
	if scheduleRaw != "" {
		lower := strings.ToLower(scheduleRaw)
		if strings.HasPrefix(lower, "every:") {
			everyValue := strings.TrimSpace(scheduleRaw[len("every:"):])
			every, err := time.ParseDuration(everyValue)
			if err != nil {
				return daemonTaskSchedule{}, fmt.Errorf("invalid persisted every value %q: %w", scheduleRaw, err)
			}
			return newEveryTaskSchedule(every)
		}
		if strings.HasPrefix(lower, "cron:") {
			return newCronTaskSchedule(strings.TrimSpace(scheduleRaw[len("cron:"):]))
		}
		if every, err := time.ParseDuration(scheduleRaw); err == nil {
			return newEveryTaskSchedule(every)
		}
		return newCronTaskSchedule(scheduleRaw)
	}

	legacyEvery := strings.TrimSpace(everyText)
	if legacyEvery == "" {
		return daemonTaskSchedule{}, fmt.Errorf("missing task schedule")
	}
	every, err := time.ParseDuration(legacyEvery)
	if err != nil {
		return daemonTaskSchedule{}, fmt.Errorf("invalid task interval %q: %w", legacyEvery, err)
	}
	return newEveryTaskSchedule(every)
}

func buildTaskTimerDelay(now time.Time, nextRunAt time.Time) (time.Duration, bool) {
	if nextRunAt.IsZero() {
		return 0, false
	}
	delay := nextRunAt.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func defaultDaemonTaskStatePath() string {
	return filepath.Join(daemonConfigDir, daemonTaskStateFile)
}

func defaultDaemonTaskHistoryPath() string {
	return filepath.Join(daemonDataDir, daemonTaskHistoryFile)
}

func deriveDaemonTaskHistoryPath(statePath string) string {
	path := strings.TrimSpace(statePath)
	if path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(path), daemonTaskHistoryFile)
}

func (m *daemonTaskManager) Close() {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.tasks))
	for _, task := range m.tasks {
		cancels = append(cancels, task.cancel)
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	m.wg.Wait()
}

func (m *daemonTaskManager) Load(ctx context.Context) (int, error) {
	if strings.TrimSpace(m.statePath) == "" {
		return 0, nil
	}

	state, err := readDaemonTaskState(m.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	now := time.Now()
	restored := make([]loadedDaemonTask, 0, len(state.Tasks))

	for _, record := range state.Tasks {
		id := strings.TrimSpace(record.ID)
		if id == "" {
			continue
		}
		commandLine := strings.TrimSpace(record.CommandLine)
		if commandLine == "" {
			continue
		}

		schedule, parseErr := parsePersistedTaskSchedule(record.Schedule, record.Every)
		if parseErr != nil {
			continue
		}

		commands, parseErr := parsePipelineCommandLine(commandLine)
		if parseErr != nil {
			continue
		}
		if parseErr := validateTaskCommands(commands); parseErr != nil {
			continue
		}

		taskCtx, cancel := context.WithCancel(ctx)
		task := &daemonTask{
			id:          id,
			schedule:    schedule,
			every:       schedule.Every,
			commandLine: commandLine,
			commands:    clonePipelineCommands(commands),
			createdAt:   parseTaskTimestampOr(record.CreatedAt, now),
			lastRunAt:   parseTaskTimestampOr(record.LastRunAt, time.Time{}),
			lastStatus:  strings.TrimSpace(record.LastStatus),
			lastError:   strings.TrimSpace(record.LastError),
			runCount:    record.RunCount,
			paused:      record.Paused,
			cancel:      cancel,
			wake:        make(chan struct{}, 1),
		}

		if task.lastStatus == "" {
			if task.paused {
				task.lastStatus = "paused"
			} else {
				task.lastStatus = "scheduled"
			}
		}

		if task.paused {
			task.nextRunAt = time.Time{}
		} else {
			task.nextRunAt = task.schedule.Next(now)
		}

		restored = append(restored, loadedDaemonTask{
			taskCtx: taskCtx,
			task:    task,
		})
	}

	maxID := int64(0)
	if state.NextID > 0 {
		maxID = state.NextID
	}
	for _, loaded := range restored {
		if parsedID := parseTaskIDNumber(loaded.task.id); parsedID > maxID {
			maxID = parsedID
		}
	}

	m.mu.Lock()
	if len(m.tasks) > 0 {
		m.mu.Unlock()
		for _, loaded := range restored {
			loaded.task.cancel()
		}
		return 0, fmt.Errorf("cannot load daemon tasks into a non-empty manager")
	}
	for _, loaded := range restored {
		m.tasks[loaded.task.id] = loaded.task
	}
	m.nextID = maxID
	m.globallyPaused = state.GloballyPaused
	m.mu.Unlock()

	for _, loaded := range restored {
		m.wg.Add(1)
		go m.runTask(loaded.taskCtx, loaded.task)
	}

	return len(restored), nil
}

type loadedDaemonTask struct {
	taskCtx context.Context
	task    *daemonTask
}

func (m *daemonTaskManager) HandleInputLine(ctx context.Context, line string) (bool, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 || !strings.EqualFold(fields[0], daemonTaskCommandName) {
		return false, nil
	}

	args, err := parseCommandLine(line)
	if err != nil {
		return true, err
	}
	if len(args) == 0 {
		return true, nil
	}

	first := strings.ToLower(strings.TrimSpace(args[0]))
	if first != daemonTaskCommandName {
		return false, nil
	}

	if len(args) == 1 {
		m.printHelp()
		return true, nil
	}

	sub := strings.ToLower(strings.TrimSpace(args[1]))
	switch sub {
	case "add":
		if err := m.handleAdd(ctx, line, args); err != nil {
			return true, err
		}
		return true, nil
	case "list", "ls":
		m.handleList()
		return true, nil
	case "remove", "rm", "delete":
		if err := m.handleRemove(args); err != nil {
			return true, err
		}
		return true, nil
	case "pause":
		if err := m.handlePause(args); err != nil {
			return true, err
		}
		return true, nil
	case "global-pause":
		if err := m.handlePauseAll(); err != nil {
			return true, err
		}
		return true, nil
	case "resume":
		if err := m.handleResume(args); err != nil {
			return true, err
		}
		return true, nil
	case "global-resume":
		if err := m.handleResumeAll(); err != nil {
			return true, err
		}
		return true, nil
	case "help", "-h", "--help":
		m.printHelp()
		return true, nil
	default:
		return true, fmt.Errorf("unknown task subcommand %q", sub)
	}
}

func (m *daemonTaskManager) TaskIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.tasks))
	for id := range m.tasks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (m *daemonTaskManager) handleAdd(ctx context.Context, line string, args []string) error {
	addOpts, err := parseTaskAddOptions(args)
	if err != nil {
		return err
	}

	commandLine, err := extractTaskCommandLine(line)
	if err != nil {
		return err
	}

	commands, err := parsePipelineCommandLine(commandLine)
	if err != nil {
		return fmt.Errorf("invalid scheduled command: %w", err)
	}
	if len(commands) == 0 {
		return fmt.Errorf("scheduled command is empty")
	}
	if err := validateTaskCommands(commands); err != nil {
		return err
	}

	snapshot, err := m.addTaskWithSchedule(ctx, addOpts.Schedule, commandLine, commands)
	if err != nil {
		return err
	}
	fmt.Printf("task created: id=%s schedule=%s status=paused command=%s\n", snapshot.ID, snapshot.Schedule, snapshot.CommandLine)
	if addOpts.AutoResume {
		resumed, err := m.resumeTask(snapshot.ID)
		if err != nil {
			return err
		}
		if !resumed {
			return fmt.Errorf("task %q not found during auto-resume", snapshot.ID)
		}
		fmt.Printf("task auto-resumed: id=%s\n", snapshot.ID)
	}
	if m.isGloballyPaused() {
		fmt.Println("note: global pause is enabled; run `task global-resume` to allow resumed tasks to execute")
	}
	return nil
}

func (m *daemonTaskManager) handleList() {
	tasks := m.listTasks()
	globallyPaused := m.isGloballyPaused()
	fmt.Print(formatTaskListOutput(tasks, globallyPaused))
}

func formatTaskListOutput(tasks []daemonTaskSnapshot, globallyPaused bool) string {
	var builder strings.Builder
	if globallyPaused {
		builder.WriteString("*** ALL TASKS GLOBALLY PAUSED ***\n")
	}

	if len(tasks) == 0 {
		builder.WriteString("no scheduled task\n")
		return builder.String()
	}

	tw := tabwriter.NewWriter(&builder, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSCHEDULE\tNEXT_RUN\tLAST_RUN\tRUNS\tSTATUS\tCOMMAND")
	for _, task := range tasks {
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			task.ID,
			task.Schedule,
			formatTaskTime(task.NextRunAt),
			formatTaskTime(task.LastRunAt),
			task.RunCount,
			buildTaskStatusText(task),
			task.CommandLine,
		)
	}
	_ = tw.Flush()
	return builder.String()
}

func (m *daemonTaskManager) handleRemove(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("missing task id, usage: task remove <task-id> <confirm-task-id>")
	}
	taskID := strings.TrimSpace(args[2])
	if taskID == "" {
		return fmt.Errorf("missing task id, usage: task remove <task-id> <confirm-task-id>")
	}
	if len(args) < 4 {
		return fmt.Errorf("missing confirmation task id, usage: task remove %s %s", taskID, taskID)
	}
	confirmTaskID := strings.TrimSpace(args[3])
	if confirmTaskID == "" {
		return fmt.Errorf("missing confirmation task id, usage: task remove %s %s", taskID, taskID)
	}
	if taskID != confirmTaskID {
		return fmt.Errorf("task id confirmation mismatch: expected %q, got %q", taskID, confirmTaskID)
	}

	removed, err := m.cancelTask(taskID)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("task %q not found", taskID)
	}
	fmt.Printf("task removed: id=%s\n", taskID)
	return nil
}

func (m *daemonTaskManager) handlePause(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("missing task id, usage: task pause <task-id>")
	}
	taskID := strings.TrimSpace(args[2])
	if taskID == "" {
		return fmt.Errorf("missing task id, usage: task pause <task-id>")
	}

	paused, err := m.pauseTask(taskID)
	if err != nil {
		return err
	}
	if !paused {
		return fmt.Errorf("task %q not found", taskID)
	}
	fmt.Printf("task paused: id=%s\n", taskID)
	return nil
}

func (m *daemonTaskManager) handleResume(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("missing task id, usage: task resume <task-id>")
	}
	taskID := strings.TrimSpace(args[2])
	if taskID == "" {
		return fmt.Errorf("missing task id, usage: task resume <task-id>")
	}

	resumed, err := m.resumeTask(taskID)
	if err != nil {
		return err
	}
	if !resumed {
		return fmt.Errorf("task %q not found", taskID)
	}
	fmt.Printf("task resumed: id=%s\n", taskID)
	return nil
}

func (m *daemonTaskManager) handlePauseAll() error {
	err := m.pauseAll()
	if err != nil {
		return err
	}
	fmt.Println("all tasks paused globally")
	return nil
}

func (m *daemonTaskManager) handleResumeAll() error {
	err := m.resumeAll()
	if err != nil {
		return err
	}
	fmt.Println("all tasks resumed globally")
	return nil
}

func (m *daemonTaskManager) printHelp() {
	fmt.Println("task commands:")
	fmt.Println(`  task add --every <duration> [--auto-resume] -- <command line>`)
	fmt.Println(`  task add --cron "<cron expr>" [--auto-resume] -- <command line>`)
	fmt.Println("  task list")
	fmt.Println("  task pause <task-id>")
	fmt.Println("  task global-pause               # pause all tasks (master switch)")
	fmt.Println("  task resume <task-id>")
	fmt.Println("  task global-resume              # resume all tasks (master switch)")
	fmt.Println("  task remove <task-id> <task-id> # remove task (id confirmation required)")
}

type taskAddOptions struct {
	Schedule   daemonTaskSchedule
	AutoResume bool
}

func parseTaskAddOptions(args []string) (taskAddOptions, error) {
	if len(args) < 3 {
		return taskAddOptions{}, fmt.Errorf("missing task schedule, usage: task add --every <duration> [--auto-resume] -- <command line> OR task add --cron \"<expr>\" [--auto-resume] -- <command line>")
	}

	sepIndex := slices.Index(args, "--")
	if sepIndex < 0 {
		return taskAddOptions{}, fmt.Errorf("missing command separator \"--\", usage: task add --every <duration> [--auto-resume] -- <command line> OR task add --cron \"<expr>\" [--auto-resume] -- <command line>")
	}
	if sepIndex <= 2 {
		return taskAddOptions{}, fmt.Errorf("missing task schedule before separator")
	}

	var (
		everyText string
		cronText  string
		seenEvery bool
		seenCron  bool
		seenAuto  bool
		auto      bool
	)
	for i := 2; i < sepIndex; i++ {
		token := strings.TrimSpace(args[i])
		if token == "" {
			continue
		}

		if strings.HasPrefix(token, "--every=") {
			if seenEvery {
				return taskAddOptions{}, fmt.Errorf("duplicate --every")
			}
			seenEvery = true
			everyText = strings.TrimSpace(strings.TrimPrefix(token, "--every="))
			continue
		}
		if strings.HasPrefix(token, "--cron=") {
			if seenCron {
				return taskAddOptions{}, fmt.Errorf("duplicate --cron")
			}
			seenCron = true
			cronText = strings.TrimSpace(strings.TrimPrefix(token, "--cron="))
			continue
		}
		if strings.HasPrefix(token, "--auto-resume=") {
			if seenAuto {
				return taskAddOptions{}, fmt.Errorf("duplicate --auto-resume")
			}
			seenAuto = true
			autoText := strings.TrimSpace(strings.TrimPrefix(token, "--auto-resume="))
			if autoText == "" {
				return taskAddOptions{}, fmt.Errorf("missing value for --auto-resume")
			}
			autoValue, err := strconv.ParseBool(autoText)
			if err != nil {
				return taskAddOptions{}, fmt.Errorf("invalid --auto-resume value %q: %w", autoText, err)
			}
			auto = autoValue
			continue
		}
		if token == "--every" || token == "-e" {
			if seenEvery {
				return taskAddOptions{}, fmt.Errorf("duplicate --every")
			}
			seenEvery = true
			i++
			if i >= sepIndex {
				return taskAddOptions{}, fmt.Errorf("missing value for %s", token)
			}
			everyText = strings.TrimSpace(args[i])
			continue
		}
		if token == "--cron" || token == "-c" {
			if seenCron {
				return taskAddOptions{}, fmt.Errorf("duplicate --cron")
			}
			seenCron = true
			i++
			if i >= sepIndex {
				return taskAddOptions{}, fmt.Errorf("missing value for %s", token)
			}
			cronText = strings.TrimSpace(args[i])
			continue
		}
		if token == "--auto-resume" {
			if seenAuto {
				return taskAddOptions{}, fmt.Errorf("duplicate --auto-resume")
			}
			seenAuto = true
			auto = true
			continue
		}

		if !seenEvery && !seenCron && !strings.HasPrefix(token, "-") {
			seenEvery = true
			everyText = token
			continue
		}
		return taskAddOptions{}, fmt.Errorf("unsupported option %q in task add", token)
	}

	if strings.TrimSpace(everyText) != "" && strings.TrimSpace(cronText) != "" {
		return taskAddOptions{}, fmt.Errorf("task add supports either --every or --cron, not both")
	}

	if strings.TrimSpace(everyText) != "" {
		every, err := time.ParseDuration(everyText)
		if err != nil {
			return taskAddOptions{}, fmt.Errorf("invalid task interval %q: %w", everyText, err)
		}
		schedule, err := newEveryTaskSchedule(every)
		if err != nil {
			return taskAddOptions{}, err
		}
		return taskAddOptions{
			Schedule:   schedule,
			AutoResume: auto,
		}, nil
	}

	if strings.TrimSpace(cronText) != "" {
		schedule, err := newCronTaskSchedule(cronText)
		if err != nil {
			return taskAddOptions{}, err
		}
		return taskAddOptions{
			Schedule:   schedule,
			AutoResume: auto,
		}, nil
	}

	return taskAddOptions{}, fmt.Errorf("missing task schedule, usage: task add --every <duration> [--auto-resume] -- <command line> OR task add --cron \"<expr>\" [--auto-resume] -- <command line>")
}

func parseTaskSchedule(args []string) (daemonTaskSchedule, error) {
	addOpts, err := parseTaskAddOptions(args)
	if err != nil {
		return daemonTaskSchedule{}, err
	}
	return addOpts.Schedule, nil
}

func extractTaskCommandLine(line string) (string, error) {
	separatorEnd, found, err := findTaskCommandSeparator(line)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("missing command separator \"--\", usage: task add --every <duration> [--auto-resume] -- <command line> OR task add --cron \"<expr>\" [--auto-resume] -- <command line>")
	}

	commandLine := strings.TrimSpace(line[separatorEnd:])
	if commandLine == "" {
		return "", fmt.Errorf("scheduled command is empty")
	}
	return commandLine, nil
}

func findTaskCommandSeparator(line string) (separatorEnd int, found bool, err error) {
	var (
		inSingle   bool
		inDouble   bool
		escaped    bool
		tokenStart = -1
	)

	for index, ch := range line {
		if escaped {
			escaped = false
			continue
		}

		if ch == '\\' && !inSingle {
			escaped = true
			if tokenStart == -1 {
				tokenStart = index
			}
			continue
		}

		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '"' {
				inDouble = false
			}
			continue
		}

		if ch == '\'' {
			if tokenStart == -1 {
				tokenStart = index
			}
			inSingle = true
			continue
		}
		if ch == '"' {
			if tokenStart == -1 {
				tokenStart = index
			}
			inDouble = true
			continue
		}

		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			if tokenStart >= 0 {
				if line[tokenStart:index] == "--" {
					return index, true, nil
				}
				tokenStart = -1
			}
			continue
		}

		if tokenStart == -1 {
			tokenStart = index
		}
	}

	if escaped {
		return 0, false, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		return 0, false, fmt.Errorf("unclosed quote")
	}
	if tokenStart >= 0 && line[tokenStart:] == "--" {
		return len(line), true, nil
	}
	return 0, false, nil
}

func validateTaskCommands(commands [][]string) error {
	for index, command := range commands {
		if len(command) == 0 {
			return fmt.Errorf("task stage %d is empty", index+1)
		}

		first := strings.ToLower(strings.TrimSpace(command[0]))
		switch first {
		case "exit", "quit":
			return fmt.Errorf("task command cannot include %q", first)
		case "daemon", "d":
			return fmt.Errorf("task command cannot include daemon command")
		case daemonTaskCommandName:
			return fmt.Errorf("task command cannot schedule task command recursively")
		}
	}
	return nil
}

func (m *daemonTaskManager) addTask(ctx context.Context, every time.Duration, commandLine string, commands [][]string) (daemonTaskSnapshot, error) {
	schedule, err := newEveryTaskSchedule(every)
	if err != nil {
		return daemonTaskSnapshot{}, err
	}
	return m.addTaskWithSchedule(ctx, schedule, commandLine, commands)
}

func (m *daemonTaskManager) addTaskWithSchedule(ctx context.Context, schedule daemonTaskSchedule, commandLine string, commands [][]string) (daemonTaskSnapshot, error) {
	taskCtx, cancel := context.WithCancel(ctx)

	m.mu.Lock()
	m.nextID++
	id := "task-" + strconv.FormatInt(m.nextID, 10)
	task := &daemonTask{
		id:          id,
		schedule:    schedule,
		every:       schedule.Every,
		commandLine: commandLine,
		commands:    clonePipelineCommands(commands),
		createdAt:   time.Now(),
		nextRunAt:   time.Time{},
		lastStatus:  "paused",
		paused:      true,
		cancel:      cancel,
		wake:        make(chan struct{}, 1),
	}
	m.tasks[id] = task
	if err := m.saveStateLocked(); err != nil {
		delete(m.tasks, id)
		m.mu.Unlock()
		cancel()
		return daemonTaskSnapshot{}, err
	}
	snapshot := m.buildSnapshot(task)
	m.wg.Add(1)
	m.mu.Unlock()

	go m.runTask(taskCtx, task)
	return snapshot, nil
}

func (m *daemonTaskManager) runTask(ctx context.Context, task *daemonTask) {
	defer m.wg.Done()

	timer := time.NewTimer(time.Hour)
	timerActive := true
	initialDelay, initialActive := m.nextTaskDelay(task.id)
	if initialActive {
		resetTaskTimer(timer, &timerActive, initialDelay)
	} else {
		stopTaskTimer(timer, &timerActive)
	}
	defer stopTaskTimer(timer, &timerActive)

	for {
		select {
		case <-ctx.Done():
			return
		case <-task.wake:
			delay, active := m.nextTaskDelay(task.id)
			if !active {
				stopTaskTimer(timer, &timerActive)
				continue
			}
			resetTaskTimer(timer, &timerActive, delay)
		case tickAt := <-timer.C:
			timerActive = false
			if !m.markTaskRunning(task.id) {
				delay, active := m.nextTaskDelay(task.id)
				if active {
					resetTaskTimer(timer, &timerActive, delay)
				}
				continue
			}
			err := m.runner(ctx, task.commands)
			delay, active, stateErr := m.markTaskFinished(task.id, tickAt, err)
			if stateErr != nil {
				fmt.Printf("task state save failed for %s: %v\n", task.id, stateErr)
			}
			if active {
				resetTaskTimer(timer, &timerActive, delay)
			}
		}
	}
}

func (m *daemonTaskManager) markTaskRunning(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[taskID]
	if !ok || task.paused || m.globallyPaused {
		return false
	}
	task.running = true
	task.lastStatus = "running"
	task.lastError = ""
	task.nextRunAt = time.Time{}
	return true
}

func (m *daemonTaskManager) markTaskFinished(taskID string, runAt time.Time, runErr error) (time.Duration, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = runAt

	task, ok := m.tasks[taskID]
	if !ok {
		return 0, false, nil
	}
	task.running = false
	task.lastRunAt = runAt
	task.runCount++
	if runErr != nil {
		task.lastStatus = "failed"
		task.lastError = runErr.Error()
	} else {
		task.lastStatus = "success"
		task.lastError = ""
	}

	if task.paused {
		task.nextRunAt = time.Time{}
		return 0, false, m.saveStateLocked()
	}

	now := time.Now()
	nextRun := task.schedule.Next(now)
	task.nextRunAt = nextRun
	delay, active := buildTaskTimerDelay(now, nextRun)
	return delay, active, m.saveStateLocked()
}

func (m *daemonTaskManager) nextTaskDelay(taskID string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[taskID]
	if !ok || task.paused || task.running || m.globallyPaused {
		return 0, false
	}
	return buildTaskTimerDelay(time.Now(), task.nextRunAt)
}

func (m *daemonTaskManager) cancelTask(taskID string) (bool, error) {
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return false, nil
	}

	delete(m.tasks, taskID)
	if err := m.saveStateLocked(); err != nil {
		m.tasks[taskID] = task
		m.mu.Unlock()
		return true, err
	}

	if err := m.appendTaskHistoryLocked(task, time.Now()); err != nil {
		m.tasks[taskID] = task
		if rollbackErr := m.saveStateLocked(); rollbackErr != nil {
			m.mu.Unlock()
			return true, fmt.Errorf("backup removed task failed: %v; rollback state failed: %w", err, rollbackErr)
		}
		m.mu.Unlock()
		return true, fmt.Errorf("backup removed task failed: %w", err)
	}

	cancel := task.cancel
	m.mu.Unlock()

	cancel()
	return true, nil
}

func (m *daemonTaskManager) pauseTask(taskID string) (bool, error) {
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return false, nil
	}
	prevPaused := task.paused
	prevNextRunAt := task.nextRunAt
	task.paused = true
	task.nextRunAt = time.Time{}
	if err := m.saveStateLocked(); err != nil {
		task.paused = prevPaused
		task.nextRunAt = prevNextRunAt
		m.mu.Unlock()
		return true, err
	}
	wake := task.wake
	m.mu.Unlock()

	signalTaskWake(wake)
	return true, nil
}

func (m *daemonTaskManager) resumeTask(taskID string) (bool, error) {
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return false, nil
	}
	wasPaused := task.paused
	prevNextRunAt := task.nextRunAt
	task.paused = false
	if wasPaused && !task.running {
		task.nextRunAt = task.schedule.Next(time.Now())
	}
	if err := m.saveStateLocked(); err != nil {
		task.paused = wasPaused
		task.nextRunAt = prevNextRunAt
		m.mu.Unlock()
		return true, err
	}
	wake := task.wake
	m.mu.Unlock()

	if wasPaused {
		signalTaskWake(wake)
	}
	return true, nil
}

func (m *daemonTaskManager) pauseAll() error {
	m.mu.Lock()
	if m.globallyPaused {
		m.mu.Unlock()
		return nil
	}
	m.globallyPaused = true

	// Collect all task wake channels
	wakes := make([]chan struct{}, 0, len(m.tasks))
	for _, task := range m.tasks {
		wakes = append(wakes, task.wake)
	}

	if err := m.saveStateLocked(); err != nil {
		m.globallyPaused = false
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// Signal all tasks to wake and check the new state
	for _, wake := range wakes {
		signalTaskWake(wake)
	}
	return nil
}

func (m *daemonTaskManager) resumeAll() error {
	m.mu.Lock()
	if !m.globallyPaused {
		m.mu.Unlock()
		return nil
	}
	m.globallyPaused = false

	now := time.Now()
	// Collect all task wake channels and update next run times
	wakes := make([]chan struct{}, 0, len(m.tasks))
	for _, task := range m.tasks {
		if !task.paused && !task.running {
			task.nextRunAt = task.schedule.Next(now)
		}
		wakes = append(wakes, task.wake)
	}

	if err := m.saveStateLocked(); err != nil {
		m.globallyPaused = true
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// Signal all tasks to wake
	for _, wake := range wakes {
		signalTaskWake(wake)
	}
	return nil
}

func (m *daemonTaskManager) listTasks() []daemonTaskSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshots := make([]daemonTaskSnapshot, 0, len(m.tasks))
	for _, task := range m.tasks {
		snapshots = append(snapshots, m.buildSnapshot(task))
	}
	slices.SortFunc(snapshots, func(a, b daemonTaskSnapshot) int {
		return strings.Compare(a.ID, b.ID)
	})
	return snapshots
}

func (m *daemonTaskManager) isGloballyPaused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.globallyPaused
}

func (m *daemonTaskManager) buildSnapshot(task *daemonTask) daemonTaskSnapshot {
	return daemonTaskSnapshot{
		ID:          task.id,
		Schedule:    task.schedule.Display(),
		Every:       task.every,
		CommandLine: task.commandLine,
		CreatedAt:   task.createdAt,
		NextRunAt:   task.nextRunAt,
		LastRunAt:   task.lastRunAt,
		LastStatus:  task.lastStatus,
		LastError:   task.lastError,
		RunCount:    task.runCount,
		Paused:      task.paused,
		Running:     task.running,
	}
}

func (m *daemonTaskManager) saveStateLocked() error {
	if strings.TrimSpace(m.statePath) == "" {
		return nil
	}

	state := daemonTaskState{
		Version:        daemonTaskStateVersion,
		NextID:         m.nextID,
		Tasks:          make([]daemonTaskRecord, 0, len(m.tasks)),
		GloballyPaused: m.globallyPaused,
	}
	for _, task := range m.tasks {
		state.Tasks = append(state.Tasks, daemonTaskRecord{
			ID:          task.id,
			Schedule:    task.schedule.Persist(),
			Every:       task.schedule.LegacyEveryText(),
			CommandLine: task.commandLine,
			CreatedAt:   formatTaskTimestamp(task.createdAt),
			LastRunAt:   formatTaskTimestamp(task.lastRunAt),
			LastStatus:  task.lastStatus,
			LastError:   task.lastError,
			RunCount:    task.runCount,
			Paused:      task.paused,
		})
	}
	slices.SortFunc(state.Tasks, func(a, b daemonTaskRecord) int {
		return strings.Compare(a.ID, b.ID)
	})

	return writeDaemonTaskState(m.statePath, state)
}

func (m *daemonTaskManager) appendTaskHistoryLocked(task *daemonTask, removedAt time.Time) error {
	if strings.TrimSpace(m.historyPath) == "" {
		return nil
	}

	history, err := readDaemonTaskHistory(m.historyPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		history = daemonTaskHistory{}
	}
	if history.Version == 0 {
		history.Version = daemonTaskHistoryVersion
	}
	history.Entries = append(history.Entries, daemonTaskHistoryEntry{
		ID:          task.id,
		Schedule:    task.schedule.Persist(),
		Every:       task.schedule.LegacyEveryText(),
		CommandLine: task.commandLine,
		CreatedAt:   formatTaskTimestamp(task.createdAt),
		LastRunAt:   formatTaskTimestamp(task.lastRunAt),
		LastStatus:  task.lastStatus,
		LastError:   task.lastError,
		RunCount:    task.runCount,
		Paused:      task.paused,
		RemovedAt:   formatTaskTimestamp(removedAt),
	})
	return writeDaemonTaskHistory(m.historyPath, history)
}

func readDaemonTaskState(path string) (daemonTaskState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return daemonTaskState{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return daemonTaskState{}, nil
	}

	var state daemonTaskState
	if err := json.Unmarshal(raw, &state); err != nil {
		return daemonTaskState{}, err
	}
	return state, nil
}

func writeDaemonTaskState(path string, state daemonTaskState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func readDaemonTaskHistory(path string) (daemonTaskHistory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return daemonTaskHistory{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return daemonTaskHistory{}, nil
	}

	var history daemonTaskHistory
	if err := json.Unmarshal(raw, &history); err != nil {
		return daemonTaskHistory{}, err
	}
	return history, nil
}

func writeDaemonTaskHistory(path string, history daemonTaskHistory) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func clonePipelineCommands(commands [][]string) [][]string {
	out := make([][]string, 0, len(commands))
	for _, command := range commands {
		out = append(out, append([]string(nil), command...))
	}
	return out
}

func formatTaskTime(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	return ts.Format("2006-01-02 15:04:05")
}

func formatTaskTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Format(time.RFC3339Nano)
}

func parseTaskTimestampOr(text string, fallback time.Time) time.Time {
	value := strings.TrimSpace(text)
	if value == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseTaskIDNumber(taskID string) int64 {
	if !strings.HasPrefix(taskID, "task-") {
		return 0
	}
	numeric := strings.TrimPrefix(taskID, "task-")
	id, err := strconv.ParseInt(numeric, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

func buildTaskStatusText(task daemonTaskSnapshot) string {
	if task.Running && task.Paused {
		return "running (pause pending)"
	}
	if task.Running {
		return "running"
	}
	if task.Paused {
		return "paused"
	}
	if strings.TrimSpace(task.LastError) != "" {
		return "scheduled (last failed: " + task.LastError + ")"
	}
	if task.LastStatus == "success" {
		return "scheduled (last success)"
	}
	return "scheduled"
}

func signalTaskWake(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func resetTaskTimer(timer *time.Timer, active *bool, delay time.Duration) {
	stopTaskTimer(timer, active)
	if delay < 0 {
		delay = 0
	}
	timer.Reset(delay)
	*active = true
}

func stopTaskTimer(timer *time.Timer, active *bool) {
	if !*active {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	*active = false
}
