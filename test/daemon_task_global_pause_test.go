package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGlobalPauseResumeAllTasks(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "test_tasks.json")

	// Track execution count
	executionCount := 0
	runner := func(ctx context.Context, commands [][]string) error {
		executionCount++
		return nil
	}

	manager := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer manager.Close()

	// Add a couple of tasks
	task1, err := manager.addTask(context.Background(), 100*time.Millisecond, "echo test1", [][]string{{"echo", "test1"}})
	if err != nil {
		t.Fatalf("Failed to add task 1: %v", err)
	}

	task2, err := manager.addTask(context.Background(), 150*time.Millisecond, "echo test2", [][]string{{"echo", "test2"}})
	if err != nil {
		t.Fatalf("Failed to add task 2: %v", err)
	}
	if resumed, resumeErr := manager.resumeTask(task1.ID); resumeErr != nil || !resumed {
		t.Fatalf("Failed to resume task1: resumed=%v err=%v", resumed, resumeErr)
	}
	if resumed, resumeErr := manager.resumeTask(task2.ID); resumeErr != nil || !resumed {
		t.Fatalf("Failed to resume task2: resumed=%v err=%v", resumed, resumeErr)
	}

	// Allow some time for tasks to run
	time.Sleep(200 * time.Millisecond)
	initialExecutions := executionCount

	if initialExecutions == 0 {
		t.Fatal("Tasks should have executed at least once")
	}

	// Pause all tasks globally
	err = manager.pauseAll()
	if err != nil {
		t.Fatalf("Failed to pause all: %v", err)
	}

	if !manager.isGloballyPaused() {
		t.Fatal("Manager should be globally paused after pauseAll()")
	}

	// Wait a bit while paused
	executionCount = 0
	time.Sleep(300 * time.Millisecond)
	pausedExecutions := executionCount

	if pausedExecutions > 0 {
		t.Fatalf("Tasks should not execute when globally paused, but got %d executions", pausedExecutions)
	}

	// Resume all tasks
	err = manager.resumeAll()
	if err != nil {
		t.Fatalf("Failed to resume all: %v", err)
	}

	if manager.isGloballyPaused() {
		t.Fatal("Manager should not be globally paused after resumeAll()")
	}

	// Allow tasks to run again
	executionCount = 0
	time.Sleep(250 * time.Millisecond)
	resumedExecutions := executionCount

	if resumedExecutions == 0 {
		t.Fatal("Tasks should execute after resuming")
	}
}

func TestGlobalPauseWithIndividualPause(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "test_tasks.json")

	executionCount := 0
	runner := func(ctx context.Context, commands [][]string) error {
		executionCount++
		return nil
	}

	manager := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer manager.Close()

	// Add tasks
	task1, err := manager.addTask(context.Background(), 100*time.Millisecond, "echo test1", [][]string{{"echo", "test1"}})
	if err != nil {
		t.Fatalf("Failed to add task 1: %v", err)
	}

	task2, err := manager.addTask(context.Background(), 100*time.Millisecond, "echo test2", [][]string{{"echo", "test2"}})
	if err != nil {
		t.Fatalf("Failed to add task 2: %v", err)
	}
	if resumed, resumeErr := manager.resumeTask(task1.ID); resumeErr != nil || !resumed {
		t.Fatalf("Failed to resume task1: resumed=%v err=%v", resumed, resumeErr)
	}
	if resumed, resumeErr := manager.resumeTask(task2.ID); resumeErr != nil || !resumed {
		t.Fatalf("Failed to resume task2: resumed=%v err=%v", resumed, resumeErr)
	}

	// Let them run
	time.Sleep(150 * time.Millisecond)

	// Pause one task individually
	manager.pauseTask(task1.ID)

	// Pause all tasks globally
	manager.pauseAll()

	// Reset counter
	executionCount = 0
	time.Sleep(200 * time.Millisecond)

	// Both should be paused (global pause overrides)
	if executionCount > 0 {
		t.Fatalf("No tasks should execute with global pause, but got %d", executionCount)
	}

	// Resume globally
	manager.resumeAll()

	// Reset counter
	executionCount = 0
	time.Sleep(150 * time.Millisecond)

	// Task2 should run (was not individually paused), Task1 should not (was individually paused)
	// We can't easily count per-task, but we should have some executions
	if executionCount == 0 {
		t.Fatal("At least task2 should execute after global resume")
	}

	_ = task2 // Use task2 to avoid unused variable error
}

func TestGlobalPauseStatePeristence(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "test_tasks.json")

	runner := func(ctx context.Context, commands [][]string) error {
		return nil
	}

	// Create and pause
	manager1 := newDaemonTaskManagerWithStatePath(runner, statePath)
	manager1.addTask(context.Background(), 100*time.Millisecond, "echo test", [][]string{{"echo", "test"}})
	manager1.pauseAll()
	manager1.Close()

	// Load and check
	manager2 := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer manager2.Close()
	manager2.Load(context.Background())

	if !manager2.isGloballyPaused() {
		t.Fatal("Global pause state should be persisted")
	}

	// Resume and check
	manager2.resumeAll()
	manager2.Close()

	// Load again
	manager3 := newDaemonTaskManagerWithStatePath(runner, statePath)
	defer manager3.Close()
	manager3.Load(context.Background())

	if manager3.isGloballyPaused() {
		t.Fatal("Global pause state should be persisted as false after resume")
	}
}
