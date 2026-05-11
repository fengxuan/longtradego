package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunConfigPathsRepoRelative(t *testing.T) {
	oldUseRepoRelativeLayout := useRepoRelativeLayout
	oldDefaultConfigDir := defaultConfigDir
	oldDefaultDataDir := defaultDataDir
	oldDefaultLogDir := defaultLogDir
	defer func() {
		useRepoRelativeLayout = oldUseRepoRelativeLayout
		defaultConfigDir = oldDefaultConfigDir
		defaultDataDir = oldDefaultDataDir
		defaultLogDir = oldDefaultLogDir
	}()

	useRepoRelativeLayout = func() bool { return true }
	defaultConfigDir = func() string { return "conf" }
	defaultDataDir = func() string { return "data" }
	defaultLogDir = func() string { return "logs" }

	cmd := newConfigPathsCommand(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config paths execute failed: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "layout=repo_relative") {
		t.Fatalf("expected repo_relative layout, got %q", output)
	}
	if !strings.Contains(output, "config=conf") {
		t.Fatalf("expected repo config dir, got %q", output)
	}
	if !strings.Contains(output, "data=data") {
		t.Fatalf("expected repo data dir, got %q", output)
	}
	if !strings.Contains(output, "logs=logs") {
		t.Fatalf("expected repo log dir, got %q", output)
	}
}

func TestRunConfigPathsUserHome(t *testing.T) {
	oldUseRepoRelativeLayout := useRepoRelativeLayout
	oldDefaultConfigDir := defaultConfigDir
	oldDefaultDataDir := defaultDataDir
	oldDefaultLogDir := defaultLogDir
	defer func() {
		useRepoRelativeLayout = oldUseRepoRelativeLayout
		defaultConfigDir = oldDefaultConfigDir
		defaultDataDir = oldDefaultDataDir
		defaultLogDir = oldDefaultLogDir
	}()

	useRepoRelativeLayout = func() bool { return false }
	defaultConfigDir = func() string { return "/Users/demo/.config/longtradego/conf" }
	defaultDataDir = func() string { return "/Users/demo/.config/longtradego/data" }
	defaultLogDir = func() string { return "/Users/demo/.config/longtradego/logs" }

	cmd := newConfigPathsCommand(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config paths execute failed: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "layout=user_home") {
		t.Fatalf("expected user_home layout, got %q", output)
	}
	if !strings.Contains(output, "config=/Users/demo/.config/longtradego/conf") {
		t.Fatalf("expected home config dir, got %q", output)
	}
}

func TestRunConfigInitCreatesTemplates(t *testing.T) {
	dir := t.TempDir()
	cmd := newConfigInitCommand(nil)
	cmd.SetArgs([]string{"--dir", dir, "--only", "security_keys", "--only", "email_aliases", "--only", "env"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config init execute failed: %v", err)
	}

	securityPath := filepath.Join(dir, "security_keys.json")
	aliasPath := filepath.Join(dir, "email_aliases.json")
	envPath := filepath.Join(dir, "longtradego.env")
	if _, err := os.Stat(securityPath); err != nil {
		t.Fatalf("expected security_keys.json created: %v", err)
	}
	if _, err := os.Stat(aliasPath); err != nil {
		t.Fatalf("expected email_aliases.json created: %v", err)
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("expected longtradego.env created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin_auth.json")); !os.IsNotExist(err) {
		t.Fatalf("expected admin_auth.json not created, err=%v", err)
	}
	output := out.String()
	if !strings.Contains(output, "created: "+securityPath) {
		t.Fatalf("expected created output for security keys, got %q", output)
	}
	if !strings.Contains(output, "created: "+aliasPath) {
		t.Fatalf("expected created output for email aliases, got %q", output)
	}
	if !strings.Contains(output, "created: "+envPath) {
		t.Fatalf("expected created output for env template, got %q", output)
	}
}

func TestRunConfigInitSkipsExistingWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "security_keys.json")
	envPath := filepath.Join(dir, "longtradego.env")
	original := []byte("original")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatalf("seed security_keys.json failed: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("SMTP_HOST=existing.example.com\n"), 0o644); err != nil {
		t.Fatalf("seed longtradego.env failed: %v", err)
	}

	cmd := newConfigInitCommand(nil)
	cmd.SetArgs([]string{"--dir", dir, "--only", "security_keys", "--only", "env"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config init execute failed: %v", err)
	}
	raw, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read security_keys.json failed: %v", err)
	}
	if string(raw) != string(original) {
		t.Fatalf("expected existing file preserved, got %q", string(raw))
	}
	envRaw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read longtradego.env failed: %v", err)
	}
	if string(envRaw) != "SMTP_HOST=existing.example.com\n" {
		t.Fatalf("expected existing env preserved, got %q", string(envRaw))
	}
	if !strings.Contains(out.String(), "skipped existing: "+targetPath) {
		t.Fatalf("expected skipped output for json, got %q", out.String())
	}
	if !strings.Contains(out.String(), "skipped existing: "+envPath) {
		t.Fatalf("expected skipped output for env, got %q", out.String())
	}
}

func TestRunConfigInitOverwritesWhenRequested(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "security_keys.json")
	envPath := filepath.Join(dir, "longtradego.env")
	if err := os.WriteFile(targetPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed security_keys.json failed: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("SMTP_HOST=old.example.com\n"), 0o644); err != nil {
		t.Fatalf("seed longtradego.env failed: %v", err)
	}

	cmd := newConfigInitCommand(nil)
	cmd.SetArgs([]string{"--dir", dir, "--only", "security_keys", "--only", "env", "--overwrite"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config init execute failed: %v", err)
	}
	raw, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read security_keys.json failed: %v", err)
	}
	if !strings.Contains(string(raw), "replace-with-strong-random-token") {
		t.Fatalf("expected template content after overwrite, got %q", string(raw))
	}
	envRaw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read longtradego.env failed: %v", err)
	}
	if !strings.Contains(string(envRaw), "LONGBRIDGE_CLIENT_ID=your-client-id") {
		t.Fatalf("expected env template content after overwrite, got %q", string(envRaw))
	}
}

func TestRunConfigInitRejectsUnknownTemplate(t *testing.T) {
	cmd := newConfigInitCommand(nil)
	cmd.SetArgs([]string{"--dir", t.TempDir(), "--only", "unknown"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected unknown template error")
	}
}

func TestRunConfigInitOnlyEnv(t *testing.T) {
	dir := t.TempDir()
	cmd := newConfigInitCommand(nil)
	cmd.SetArgs([]string{"--dir", dir, "--only", "env"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config init execute failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "longtradego.env")); err != nil {
		t.Fatalf("expected longtradego.env created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "security_keys.json")); !os.IsNotExist(err) {
		t.Fatalf("expected security_keys.json not created, err=%v", err)
	}
}
