package service

import (
	"strings"
	"testing"
)

func TestBuildVersionResult(t *testing.T) {
	oldVersion := buildVersion
	oldCommit := buildCommit
	oldDate := buildDate
	buildVersion = "v1.2.3"
	buildCommit = "abc123"
	buildDate = "2026-05-01T10:20:30Z"
	defer func() {
		buildVersion = oldVersion
		buildCommit = oldCommit
		buildDate = oldDate
	}()

	result := buildVersionResult()
	if result.Version != "v1.2.3" {
		t.Fatalf("unexpected version: %+v", result)
	}
	if result.Commit != "abc123" {
		t.Fatalf("unexpected commit: %+v", result)
	}
	if result.BuildDate != "2026-05-01T10:20:30Z" {
		t.Fatalf("unexpected build date: %+v", result)
	}
	if strings.TrimSpace(result.Platform) == "" {
		t.Fatalf("expected platform, got %+v", result)
	}
	if strings.TrimSpace(result.GoVersion) == "" {
		t.Fatalf("expected go version, got %+v", result)
	}
	if result.IsDevBuild {
		t.Fatalf("expected release build flag false, got %+v", result)
	}
}

func TestVersionCommandSetsExecutionResult(t *testing.T) {
	app := NewAppContext()
	cmd := newVersionCommand(app)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command execute failed: %v", err)
	}

	command, symbols, result := app.ExecutionSnapshot()
	if command != "version" {
		t.Fatalf("expected version command, got %q", command)
	}
	if len(symbols) != 1 || symbols[0] != "version" {
		t.Fatalf("unexpected symbols: %v", symbols)
	}
	versionResult, ok := result.(versionResult)
	if !ok {
		t.Fatalf("expected versionResult, got %T", result)
	}
	if strings.TrimSpace(versionResult.Version) == "" {
		t.Fatalf("expected non-empty version result, got %+v", versionResult)
	}
}

func TestIsDevBuildVersion(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{version: "", want: true},
		{version: "dev", want: true},
		{version: "v1.0.0-dev", want: true},
		{version: "v1.0.0", want: false},
	}
	for _, tc := range cases {
		if got := isDevBuildVersion(tc.version); got != tc.want {
			t.Fatalf("isDevBuildVersion(%q)=%t want=%t", tc.version, got, tc.want)
		}
	}
}
