package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	commandLogDir      = "logs"
	commandLogFileName = "command.log"
	commandLogMaxSize  = int64(5 * 1024 * 1024)
)

type commandFileLogger struct {
	path string
	file *os.File
	size int64
	mu   sync.Mutex
}

type commandLogEntry struct {
	Timestamp  string            `json:"timestamp"`
	RunID      string            `json:"run_id"`
	Status     string            `json:"status"`
	DurationMs int64             `json:"duration_ms,omitempty"`
	Request    commandLogRequest `json:"request"`
	Result     any               `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type commandLogRequest struct {
	RawArgs     []string `json:"raw_args"`
	CommandLine string   `json:"command_line,omitempty"`
	Command     string   `json:"command,omitempty"`
	Symbols     []string `json:"symbols,omitempty"`
}

func newCommandFileLogger() (*commandFileLogger, error) {
	if err := os.MkdirAll(commandLogDir, 0o755); err != nil {
		return nil, err
	}

	activePath := filepath.Join(commandLogDir, commandLogFileName)
	if err := rotateLogIfNeeded(activePath); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(activePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	return &commandFileLogger{
		path: activePath,
		file: file,
		size: info.Size(),
	}, nil
}

func rotateLogIfNeeded(activePath string) error {
	info, err := os.Stat(activePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	if info.Size() <= commandLogMaxSize {
		return nil
	}

	return rotateLogFile(activePath)
}

func rotateLogFile(activePath string) error {
	stamp := time.Now().Format("20060102_150405")
	rotatedName := fmt.Sprintf("command_%s.log", stamp)
	rotatedPath := filepath.Join(filepath.Dir(activePath), rotatedName)
	if _, err := os.Stat(rotatedPath); err == nil {
		rotatedPath = filepath.Join(filepath.Dir(activePath), fmt.Sprintf("command_%s_%d.log", stamp, time.Now().UnixNano()))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return os.Rename(activePath, rotatedPath)
}

func (l *commandFileLogger) Close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}

	err := l.file.Close()
	l.file = nil
	return err
}

func (l *commandFileLogger) rotateIfNeeded(appendSize int64) error {
	if l.file == nil {
		return nil
	}

	// Avoid per-write stat syscall; keep current size in memory.
	if l.size+appendSize <= commandLogMaxSize {
		return nil
	}

	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil

	if err := rotateLogFile(l.path); err != nil {
		// Best effort fallback: reopen current file so logging can continue.
		file, openErr := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr == nil {
			l.file = file
			if info, statErr := file.Stat(); statErr == nil {
				l.size = info.Size()
			}
		}
		return err
	}

	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.file = file
	l.size = 0
	return nil
}

func (l *commandFileLogger) LogExecution(runID string, rawArgs []string, command string, symbols []string, status string, result any, duration time.Duration, runErr error) {
	if l == nil {
		return
	}

	entry := commandLogEntry{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		RunID:     runID,
		Status:    status,
		Request: commandLogRequest{
			RawArgs:     append([]string(nil), rawArgs...),
			CommandLine: formatCommandLine(rawArgs),
			Command:     command,
			Symbols:     append([]string(nil), symbols...),
		},
	}
	if duration > 0 {
		entry.DurationMs = duration.Milliseconds()
	}
	if result != nil {
		entry.Result = result
	}
	if runErr != nil {
		entry.Error = runErr.Error()
	}

	line, err := marshalCommandLogEntry(entry)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return
	}

	n, err := l.file.Write(line)
	if err != nil {
		return
	}
	l.size += int64(n)
}

func marshalCommandLogEntry(entry commandLogEntry) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(entry); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func formatCommandLine(rawArgs []string) string {
	if len(rawArgs) == 0 {
		return ""
	}

	parts := make([]string, 0, len(rawArgs))
	for _, arg := range rawArgs {
		parts = append(parts, shellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !needsShellQuote(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

func needsShellQuote(arg string) bool {
	return strings.ContainsAny(arg, " \t\n\r\"'\\$`!&|;<>*?()[]{}")
}
