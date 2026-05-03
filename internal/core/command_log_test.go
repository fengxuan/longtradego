package core

import (
	"os"
	"strings"
	"testing"
)

func TestMarshalCommandLogEntryKeepsHTMLChars(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "command-log-*.jsonl")
	if err != nil {
		t.Fatalf("create temp log file failed: %v", err)
	}
	defer file.Close()

	logger := &CommandFileLogger{
		path: file.Name(),
		file: file,
		size: 0,
	}

	logger.LogExecution(
		"run-test",
		[]string{"mail", "monitor"},
		"email",
		[]string{"monitor"},
		"success",
		map[string]any{
			"body_text": "<p>Hello & welcome</p>",
		},
		0,
		nil,
	)

	if err := file.Sync(); err != nil {
		t.Fatalf("sync temp log file failed: %v", err)
	}

	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("read temp log file failed: %v", err)
	}

	text := string(data)
	if !strings.Contains(text, "<p>Hello & welcome</p>") {
		t.Fatalf("expected html chars kept in log, got: %s", text)
	}
	if strings.Contains(text, `\u003c`) || strings.Contains(text, `\u003e`) || strings.Contains(text, `\u0026`) {
		t.Fatalf("expected no html unicode escaping in log, got: %s", text)
	}
}
