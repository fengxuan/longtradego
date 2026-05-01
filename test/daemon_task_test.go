package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func captureStdoutForTest(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("stdout writer close failed: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("stdout reader read failed: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("stdout reader close failed: %v", err)
	}
	return string(output)
}

func TestParseTaskScheduleEvery(t *testing.T) {
	schedule, err := parseTaskSchedule([]string{"task", "add", "--every", "30s", "--", "quote", "AAPL.US"})
	if err != nil {
		t.Fatalf("parseTaskSchedule failed: %v", err)
	}
	if schedule.Kind != daemonTaskScheduleEvery {
		t.Fatalf("expected every schedule kind, got %q", schedule.Kind)
	}
	if schedule.Every != 30*time.Second {
		t.Fatalf("expected 30s, got %s", schedule.Every)
	}
}

func TestParseTaskScheduleEveryPositional(t *testing.T) {
	schedule, err := parseTaskSchedule([]string{"task", "add", "5m", "--", "quote", "AAPL.US"})
	if err != nil {
		t.Fatalf("parseTaskSchedule positional failed: %v", err)
	}
	if schedule.Kind != daemonTaskScheduleEvery {
		t.Fatalf("expected every schedule kind, got %q", schedule.Kind)
	}
	if schedule.Every != 5*time.Minute {
		t.Fatalf("expected 5m, got %s", schedule.Every)
	}
}

func TestParseTaskScheduleCron(t *testing.T) {
	schedule, err := parseTaskSchedule([]string{"task", "add", "--cron", "*/5 * * * *", "--", "quote", "AAPL.US"})
	if err != nil {
		t.Fatalf("parseTaskSchedule cron failed: %v", err)
	}
	if schedule.Kind != daemonTaskScheduleCron {
		t.Fatalf("expected cron schedule kind, got %q", schedule.Kind)
	}
	if schedule.Cron == nil {
		t.Fatalf("expected parsed cron schedule")
	}
	if schedule.CronExpr != "*/5 * * * *" {
		t.Fatalf("unexpected cron expr: %q", schedule.CronExpr)
	}
}

func TestParseTaskAddOptionsAutoResume(t *testing.T) {
	opts, err := parseTaskAddOptions([]string{"task", "add", "--every", "1m", "--auto-resume", "--", "quote", "AAPL.US"})
	if err != nil {
		t.Fatalf("parseTaskAddOptions auto resume failed: %v", err)
	}
	if !opts.AutoResume {
		t.Fatalf("expected auto resume to be enabled")
	}
	if opts.Schedule.Kind != daemonTaskScheduleEvery {
		t.Fatalf("expected every schedule kind, got %q", opts.Schedule.Kind)
	}
}

func TestParseTaskScheduleMissingSeparator(t *testing.T) {
	if _, err := parseTaskSchedule([]string{"task", "add", "--every", "1m", "quote", "AAPL.US"}); err == nil {
		t.Fatalf("expected separator error")
	}
}

func TestParseTaskScheduleRejectBothEveryAndCron(t *testing.T) {
	if _, err := parseTaskSchedule([]string{"task", "add", "--every", "1m", "--cron", "*/5 * * * *", "--", "quote", "AAPL.US"}); err == nil {
		t.Fatalf("expected mutually exclusive schedule options error")
	}
}

func TestParsePersistedTaskScheduleCron(t *testing.T) {
	schedule, err := parsePersistedTaskSchedule("cron:*/10 * * * *", "")
	if err != nil {
		t.Fatalf("parsePersistedTaskSchedule cron failed: %v", err)
	}
	if schedule.Kind != daemonTaskScheduleCron {
		t.Fatalf("expected cron schedule kind, got %q", schedule.Kind)
	}
	if schedule.Cron == nil {
		t.Fatalf("expected non-nil parsed cron schedule")
	}
}

func TestTaskScheduleDisplay(t *testing.T) {
	everySchedule, err := newEveryTaskSchedule(2 * time.Minute)
	if err != nil {
		t.Fatalf("newEveryTaskSchedule failed: %v", err)
	}
	if everySchedule.Display() != "every:2m0s" {
		t.Fatalf("unexpected every display: %q", everySchedule.Display())
	}

	cronSchedule, err := newCronTaskSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatalf("newCronTaskSchedule failed: %v", err)
	}
	if cronSchedule.Display() != "cron:0 9 * * 1-5" {
		t.Fatalf("unexpected cron display: %q", cronSchedule.Display())
	}
}

func TestExtractTaskCommandLine(t *testing.T) {
	line := `task add --every 1m -- quote AAPL.US | email send --to a@example.com --subject "A|B"`
	commandLine, err := extractTaskCommandLine(line)
	if err != nil {
		t.Fatalf("extractTaskCommandLine failed: %v", err)
	}
	if commandLine != `quote AAPL.US | email send --to a@example.com --subject "A|B"` {
		t.Fatalf("unexpected command line: %q", commandLine)
	}
}

func TestValidateTaskCommands(t *testing.T) {
	if err := validateTaskCommands([][]string{{"task", "list"}}); err == nil {
		t.Fatalf("expected recursive task command rejection")
	}
}

func TestDefaultDaemonTaskPaths(t *testing.T) {
	if got, want := defaultDaemonTaskStatePath(), filepath.Join("conf", "daemon_tasks.json"); got != want {
		t.Fatalf("unexpected default state path: got %q want %q", got, want)
	}
	if got, want := defaultDaemonTaskHistoryPath(), filepath.Join("data", "daemon_tasks_history.json"); got != want {
		t.Fatalf("unexpected default history path: got %q want %q", got, want)
	}
}

func TestDaemonTaskManagerAddListCancel(t *testing.T) {
	executed := make(chan struct{}, 2)
	runner := func(ctx context.Context, commands [][]string) error {
		executed <- struct{}{}
		return nil
	}

	manager := newDaemonTaskManager(runner)
	defer manager.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshot, err := manager.addTask(ctx, 20*time.Millisecond, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("addTask failed: %v", err)
	}
	if snapshot.ID == "" {
		t.Fatalf("expected non-empty task id")
	}

	select {
	case <-executed:
		t.Fatalf("expected newly created task to be paused by default")
	case <-time.After(90 * time.Millisecond):
	}

	listed := manager.listTasks()
	if len(listed) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(listed))
	}
	if listed[0].ID != snapshot.ID {
		t.Fatalf("unexpected task id in list: %s", listed[0].ID)
	}
	if !listed[0].Paused {
		t.Fatalf("expected newly created task to be paused")
	}
	if !listed[0].NextRunAt.IsZero() {
		t.Fatalf("expected newly created paused task next run to be empty, got %s", listed[0].NextRunAt)
	}

	resumed, err := manager.resumeTask(snapshot.ID)
	if err != nil {
		t.Fatalf("resumeTask failed: %v", err)
	}
	if !resumed {
		t.Fatalf("expected task resume to succeed")
	}

	select {
	case <-executed:
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("expected resumed task to execute at least once")
	}

	ids := manager.TaskIDs()
	if !slices.Contains(ids, snapshot.ID) {
		t.Fatalf("expected task id in TaskIDs result, got %v", ids)
	}

	canceled, err := manager.cancelTask(snapshot.ID)
	if err != nil {
		t.Fatalf("cancelTask failed: %v", err)
	}
	if !canceled {
		t.Fatalf("expected task cancel to succeed")
	}
	canceled, err = manager.cancelTask(snapshot.ID)
	if err != nil {
		t.Fatalf("cancelTask second call failed: %v", err)
	}
	if canceled {
		t.Fatalf("expected second cancel to fail")
	}
	if remaining := manager.listTasks(); len(remaining) != 0 {
		t.Fatalf("expected no remaining tasks, got %d", len(remaining))
	}
}

func TestDaemonTaskManagerPauseResume(t *testing.T) {
	executed := make(chan struct{}, 4)
	runner := func(ctx context.Context, commands [][]string) error {
		executed <- struct{}{}
		return nil
	}

	manager := newDaemonTaskManager(runner)
	defer manager.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshot, err := manager.addTask(ctx, 40*time.Millisecond, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("addTask failed: %v", err)
	}
	listed := manager.listTasks()
	if len(listed) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(listed))
	}
	if !listed[0].Paused {
		t.Fatalf("expected new task to be paused by default")
	}

	select {
	case <-executed:
		t.Fatalf("expected paused task not to execute before first resume")
	case <-time.After(90 * time.Millisecond):
	}

	resumed, err := manager.resumeTask(snapshot.ID)
	if err != nil {
		t.Fatalf("resumeTask failed: %v", err)
	}
	if !resumed {
		t.Fatalf("expected task resume to succeed")
	}

	select {
	case <-executed:
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("expected resumed task to execute before pause")
	}

	paused, err := manager.pauseTask(snapshot.ID)
	if err != nil {
		t.Fatalf("pauseTask failed: %v", err)
	}
	if !paused {
		t.Fatalf("expected task pause to succeed")
	}

	listed = manager.listTasks()
	if len(listed) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(listed))
	}
	if !listed[0].Paused {
		t.Fatalf("expected task to be paused")
	}
	if got := buildTaskStatusText(listed[0]); got != "paused" {
		t.Fatalf("expected paused status, got %q", got)
	}
	if !listed[0].NextRunAt.IsZero() {
		t.Fatalf("expected paused task next run to be empty, got %s", listed[0].NextRunAt)
	}

	select {
	case <-executed:
		t.Fatalf("expected paused task not to execute")
	case <-time.After(90 * time.Millisecond):
	}

	resumed, err = manager.resumeTask(snapshot.ID)
	if err != nil {
		t.Fatalf("resumeTask failed: %v", err)
	}
	if !resumed {
		t.Fatalf("expected task resume to succeed")
	}

	listed = manager.listTasks()
	if listed[0].Paused {
		t.Fatalf("expected task to be resumed")
	}
	if listed[0].NextRunAt.IsZero() {
		t.Fatalf("expected resumed task to have next run time")
	}

	select {
	case <-executed:
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("expected scheduled task to execute after resume")
	}

	paused, err = manager.pauseTask("missing-task")
	if err != nil {
		t.Fatalf("pauseTask missing task failed: %v", err)
	}
	if paused {
		t.Fatalf("expected missing task pause to fail")
	}
	resumed, err = manager.resumeTask("missing-task")
	if err != nil {
		t.Fatalf("resumeTask missing task failed: %v", err)
	}
	if resumed {
		t.Fatalf("expected missing task resume to fail")
	}
}

func TestDaemonTaskManagerHandleRemoveInputRequiresConfirmTaskID(t *testing.T) {
	runner := func(ctx context.Context, commands [][]string) error {
		return nil
	}
	manager := newDaemonTaskManager(runner)
	defer manager.Close()

	ctx := context.Background()
	task, err := manager.addTask(ctx, time.Hour, "echo test", [][]string{{"echo", "test"}})
	if err != nil {
		t.Fatalf("addTask task failed: %v", err)
	}

	if _, err := manager.HandleInputLine(ctx, "task remove "+task.ID); err == nil {
		t.Fatalf("expected missing confirmation to fail")
	}
	if len(manager.listTasks()) != 1 {
		t.Fatalf("expected task to remain when confirmation is missing")
	}

	if _, err := manager.HandleInputLine(ctx, "task remove "+task.ID+" task-999"); err == nil {
		t.Fatalf("expected confirmation mismatch to fail")
	}
	if len(manager.listTasks()) != 1 {
		t.Fatalf("expected task to remain when confirmation mismatches")
	}
	if _, err := manager.HandleInputLine(ctx, "task cancel "+task.ID+" "+task.ID); err == nil {
		t.Fatalf("expected task cancel subcommand to be unsupported")
	}

	handled, err := manager.HandleInputLine(ctx, "task remove "+task.ID+" "+task.ID)
	if err != nil {
		t.Fatalf("HandleInputLine(remove confirmed) failed: %v", err)
	}
	if !handled {
		t.Fatalf("expected confirmed remove command to be handled")
	}
	if got := len(manager.listTasks()); got != 0 {
		t.Fatalf("expected no tasks after confirmed remove, got %d", got)
	}
}

func TestDaemonTaskManagerHandleAddWarnsWhenGloballyPaused(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "daemon_tasks.json")
	historyPath := filepath.Join(tmpDir, "daemon_tasks_history.json")

	runner := func(ctx context.Context, commands [][]string) error {
		return nil
	}
	manager := newDaemonTaskManagerWithPaths(runner, statePath, historyPath)
	defer manager.Close()

	if err := manager.pauseAll(); err != nil {
		t.Fatalf("pauseAll failed: %v", err)
	}

	var (
		handled bool
		err     error
	)
	output := captureStdoutForTest(t, func() {
		handled, err = manager.HandleInputLine(context.Background(), "task add --every 1m -- quote AAPL.US")
	})
	if err != nil {
		t.Fatalf("HandleInputLine(add) failed: %v", err)
	}
	if !handled {
		t.Fatalf("expected task add command to be handled")
	}
	if !strings.Contains(output, "task created: id=") {
		t.Fatalf("expected task created output, got %q", output)
	}
	if !strings.Contains(output, "note: global pause is enabled") {
		t.Fatalf("expected global pause reminder output, got %q", output)
	}
	if !strings.Contains(output, "task global-resume") {
		t.Fatalf("expected global resume guidance in output, got %q", output)
	}
}

func TestDaemonTaskManagerHandleAddAutoResume(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "daemon_tasks.json")
	historyPath := filepath.Join(tmpDir, "daemon_tasks_history.json")

	runner := func(ctx context.Context, commands [][]string) error {
		return nil
	}
	manager := newDaemonTaskManagerWithPaths(runner, statePath, historyPath)
	defer manager.Close()

	var (
		handled bool
		err     error
	)
	output := captureStdoutForTest(t, func() {
		handled, err = manager.HandleInputLine(context.Background(), "task add --every 1h --auto-resume -- quote AAPL.US")
	})
	if err != nil {
		t.Fatalf("HandleInputLine(add auto-resume) failed: %v", err)
	}
	if !handled {
		t.Fatalf("expected task add command to be handled")
	}
	if !strings.Contains(output, "task auto-resumed: id=") {
		t.Fatalf("expected auto-resume output, got %q", output)
	}

	snapshots := manager.listTasks()
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 task, got %d", len(snapshots))
	}
	if snapshots[0].Paused {
		t.Fatalf("expected task to be resumed automatically")
	}
	if snapshots[0].NextRunAt.IsZero() {
		t.Fatalf("expected auto-resumed task next run time to be set")
	}
}

func TestDaemonTaskManagerRemoveWritesTaskHistory(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "daemon_tasks.json")
	historyPath := filepath.Join(tmpDir, "daemon_tasks_history.json")

	runner := func(ctx context.Context, commands [][]string) error {
		return nil
	}
	manager := newDaemonTaskManagerWithPaths(runner, statePath, historyPath)
	defer manager.Close()

	ctx := context.Background()
	task, err := manager.addTask(ctx, time.Hour, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("addTask failed: %v", err)
	}

	handled, err := manager.HandleInputLine(ctx, "task remove "+task.ID+" "+task.ID)
	if err != nil {
		t.Fatalf("HandleInputLine(remove) failed: %v", err)
	}
	if !handled {
		t.Fatalf("expected remove command to be handled")
	}

	history, err := readDaemonTaskHistory(historyPath)
	if err != nil {
		t.Fatalf("readDaemonTaskHistory failed: %v", err)
	}
	if history.Version != daemonTaskHistoryVersion {
		t.Fatalf("unexpected history version: %d", history.Version)
	}
	if len(history.Entries) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history.Entries))
	}
	entry := history.Entries[0]
	if entry.ID != task.ID {
		t.Fatalf("expected history id %q, got %q", task.ID, entry.ID)
	}
	if strings.TrimSpace(entry.RemovedAt) == "" {
		t.Fatalf("expected removed_at timestamp to be set")
	}
	if _, err := time.Parse(time.RFC3339Nano, entry.RemovedAt); err != nil {
		t.Fatalf("expected removed_at to be RFC3339Nano timestamp, got %q err=%v", entry.RemovedAt, err)
	}

	state, err := readDaemonTaskState(statePath)
	if err != nil {
		t.Fatalf("readDaemonTaskState failed: %v", err)
	}
	if len(state.Tasks) != 0 {
		t.Fatalf("expected no tasks after remove, got %d", len(state.Tasks))
	}
}

func TestDaemonTaskManagerRestoreFromStateFile(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "daemon_tasks.json")

	runner := func(ctx context.Context, commands [][]string) error {
		return nil
	}

	first := newDaemonTaskManagerWithStatePath(runner, statePath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task1, err := first.addTask(ctx, time.Hour, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("addTask task1 failed: %v", err)
	}
	task2, err := first.addTask(ctx, time.Hour, "quote TSLA.US", [][]string{{"quote", "TSLA.US"}})
	if err != nil {
		t.Fatalf("addTask task2 failed: %v", err)
	}
	paused, err := first.pauseTask(task2.ID)
	if err != nil || !paused {
		t.Fatalf("pauseTask task2 failed: paused=%v err=%v", paused, err)
	}
	first.Close()

	second := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer second.Close()

	restoredCount, err := second.Load(ctx)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if restoredCount != 2 {
		t.Fatalf("expected 2 restored tasks, got %d", restoredCount)
	}

	snapshots := second.listTasks()
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 tasks after restore, got %d", len(snapshots))
	}

	ids := second.TaskIDs()
	if !slices.Contains(ids, task1.ID) || !slices.Contains(ids, task2.ID) {
		t.Fatalf("expected restored ids %q and %q, got %v", task1.ID, task2.ID, ids)
	}

	var restoredPaused *daemonTaskSnapshot
	for i := range snapshots {
		if snapshots[i].ID == task2.ID {
			restoredPaused = &snapshots[i]
			break
		}
	}
	if restoredPaused == nil {
		t.Fatalf("expected paused task %q in snapshots", task2.ID)
	}
	if !restoredPaused.Paused {
		t.Fatalf("expected restored task %q to remain paused", task2.ID)
	}

	third := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer third.Close()
	if _, err := third.Load(ctx); err != nil {
		t.Fatalf("Load for third manager failed: %v", err)
	}

	canceled, err := third.cancelTask(task1.ID)
	if err != nil || !canceled {
		t.Fatalf("cancelTask task1 failed: canceled=%v err=%v", canceled, err)
	}

	fourth := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer fourth.Close()
	restoredCount, err = fourth.Load(ctx)
	if err != nil {
		t.Fatalf("Load for fourth manager failed: %v", err)
	}
	if restoredCount != 1 {
		t.Fatalf("expected 1 task after persisted cancel, got %d", restoredCount)
	}
	if slices.Contains(fourth.TaskIDs(), task1.ID) {
		t.Fatalf("expected canceled task %q not to be restored", task1.ID)
	}
}

func TestDaemonTaskManagerPersistsRunCountAfterExecution(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "daemon_tasks.json")

	executed := make(chan struct{}, 8)
	runner := func(ctx context.Context, commands [][]string) error {
		executed <- struct{}{}
		return nil
	}

	manager := newDaemonTaskManagerWithStatePath(runner, statePath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := manager.addTask(ctx, 20*time.Millisecond, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("addTask failed: %v", err)
	}
	resumed, err := manager.resumeTask(task.ID)
	if err != nil {
		t.Fatalf("resumeTask failed: %v", err)
	}
	if !resumed {
		t.Fatalf("expected task resume to succeed")
	}

	select {
	case <-executed:
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("expected scheduled task to execute at least once")
	}

	deadline := time.Now().Add(400 * time.Millisecond)
	for {
		snapshots := manager.listTasks()
		if len(snapshots) == 1 && snapshots[0].ID == task.ID && snapshots[0].RunCount > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected run count to be updated in memory for task %q", task.ID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	manager.Close()

	state, err := readDaemonTaskState(statePath)
	if err != nil {
		t.Fatalf("readDaemonTaskState failed: %v", err)
	}
	if len(state.Tasks) != 1 {
		t.Fatalf("expected 1 task in state, got %d", len(state.Tasks))
	}
	if state.Tasks[0].ID != task.ID {
		t.Fatalf("expected persisted task id %q, got %q", task.ID, state.Tasks[0].ID)
	}
	if state.Tasks[0].RunCount <= 0 {
		t.Fatalf("expected persisted run_count > 0, got %d", state.Tasks[0].RunCount)
	}
}
