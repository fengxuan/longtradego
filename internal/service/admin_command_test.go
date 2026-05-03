package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewAdminCommandHasStatusAndAuthSubcommands(t *testing.T) {
	app := NewAppContext()
	cmd := newAdminCommand(app)
	foundStatus := false
	foundAuth := false
	for _, sub := range cmd.Commands() {
		switch sub.Name() {
		case "status":
			foundStatus = true
		case "auth":
			foundAuth = true
		}
	}
	if !foundStatus || !foundAuth {
		t.Fatalf("expected admin command includes status/auth subcommands")
	}

	authCmd, _, err := cmd.Find([]string{"auth"})
	if err != nil {
		t.Fatalf("find auth command failed: %v", err)
	}
	if authCmd == nil {
		t.Fatalf("expected auth command exists")
	}
	needed := map[string]bool{"set-password": false, "migrate": false, "reset-password": false, "verify": false}
	for _, sub := range authCmd.Commands() {
		if _, ok := needed[sub.Name()]; ok {
			needed[sub.Name()] = true
		}
	}
	for name, ok := range needed {
		if !ok {
			t.Fatalf("expected admin auth command includes subcommand %s", name)
		}
	}
}

func TestRunAdminStatusSetsExecutionResult(t *testing.T) {
	app := NewAppContext()
	runtimePath := filepath.Join(t.TempDir(), "admin_runtime.json")

	if err := runAdminStatus(app, runtimePath); err != nil {
		t.Fatalf("runAdminStatus stopped failed: %v", err)
	}
	command, symbols, result := app.ExecutionSnapshot()
	if command != "admin" {
		t.Fatalf("expected admin command, got %q", command)
	}
	if len(symbols) != 1 || symbols[0] != "status" {
		t.Fatalf("expected admin status symbols, got %v", symbols)
	}
	statusResult, ok := result.(adminStatusResult)
	if !ok {
		t.Fatalf("expected adminStatusResult, got %T", result)
	}
	if statusResult.Status != "stopped" {
		t.Fatalf("expected stopped status, got %+v", statusResult)
	}

	now := time.Now().Format(time.RFC3339Nano)
	if err := writeDaemonAdminRuntimeState(runtimePath, daemonAdminRuntimeInfo{
		PID:       os.Getpid(),
		Address:   ":18080",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write runtime failed: %v", err)
	}
	if err := runAdminStatus(app, runtimePath); err != nil {
		t.Fatalf("runAdminStatus running failed: %v", err)
	}
	_, _, result = app.ExecutionSnapshot()
	statusResult, ok = result.(adminStatusResult)
	if !ok {
		t.Fatalf("expected adminStatusResult after running, got %T", result)
	}
	if statusResult.Status != "running" || statusResult.Runtime == nil || statusResult.Runtime.PID != os.Getpid() {
		t.Fatalf("expected running result with current pid, got %+v", statusResult)
	}
}

func TestAdminAuthSetPasswordAndVerify(t *testing.T) {
	app := NewAppContext()
	cfgPath := filepath.Join(t.TempDir(), "admin_auth.json")
	password := "S3cret-pass"

	if err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "set-password",
		"--config", cfgPath,
		"--username", "admin",
		"--password", password,
		"--cost", "4",
	}); err != nil {
		t.Fatalf("set-password failed: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read auth config failed: %v", err)
	}
	var rawCfg map[string]any
	if err := json.Unmarshal(raw, &rawCfg); err != nil {
		t.Fatalf("decode auth config failed: %v", err)
	}
	if _, exists := rawCfg["password"]; exists {
		t.Fatalf("expected hash-only auth config, got %s", string(raw))
	}
	if _, exists := rawCfg["password_hash"]; !exists {
		t.Fatalf("expected password_hash in auth config, got %s", string(raw))
	}

	loaded, err := loadDaemonAdminAuthConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadDaemonAdminAuthConfig failed: %v", err)
	}
	if loaded.Username != "admin" || loaded.Password != "" || strings.TrimSpace(loaded.PasswordHash) == "" {
		t.Fatalf("unexpected config after set-password: %+v", loaded)
	}

	if err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "verify",
		"--config", cfgPath,
		"--username", "admin",
		"--password", password,
	}); err != nil {
		t.Fatalf("verify with correct password failed: %v", err)
	}

	if err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "verify",
		"--config", cfgPath,
		"--username", "admin",
		"--password", "wrong-password",
	}); err == nil {
		t.Fatalf("expected verify with wrong password to fail")
	}
}

func TestAdminAuthMigrateFromLegacyPlaintext(t *testing.T) {
	app := NewAppContext()
	cfgPath := filepath.Join(t.TempDir(), "admin_auth.json")
	if err := os.WriteFile(cfgPath, []byte(`{"username":"admin","password":"legacy-pass"}`), 0o644); err != nil {
		t.Fatalf("write legacy config failed: %v", err)
	}

	if err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "migrate",
		"--config", cfgPath,
		"--cost", "4",
	}); err == nil {
		t.Fatalf("expected migrate from legacy plaintext to fail after plaintext compatibility removal")
	}
}

func TestAdminAuthMigrateNoOpWhenHashExists(t *testing.T) {
	app := NewAppContext()
	cfgPath := filepath.Join(t.TempDir(), "admin_auth.json")
	hash, err := hashDaemonAdminPassword("secret", 4)
	if err != nil {
		t.Fatalf("hash password failed: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"username":"admin","password_hash":"`+hash+`"}`), 0o644); err != nil {
		t.Fatalf("write hash config failed: %v", err)
	}

	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read before config failed: %v", err)
	}

	if err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "migrate",
		"--config", cfgPath,
		"--cost", "4",
	}); err != nil {
		t.Fatalf("migrate no-op failed: %v", err)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read after config failed: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("expected hash config unchanged on no-op migrate")
	}
}

func TestAdminAuthResetPasswordGenerateWritesHashOnly(t *testing.T) {
	app := NewAppContext()
	cfgPath := filepath.Join(t.TempDir(), "admin_auth.json")

	if err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "reset-password",
		"--generate",
		"--config", cfgPath,
		"--username", "admin",
		"--length", "16",
		"--cost", "4",
	}); err != nil {
		t.Fatalf("reset-password failed: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read reset config failed: %v", err)
	}
	var rawCfg map[string]any
	if err := json.Unmarshal(raw, &rawCfg); err != nil {
		t.Fatalf("decode reset config failed: %v", err)
	}
	if _, exists := rawCfg["password"]; exists {
		t.Fatalf("expected reset writes hash-only config, got %s", string(raw))
	}
	if _, exists := rawCfg["password_hash"]; !exists {
		t.Fatalf("expected reset config includes password_hash, got %s", string(raw))
	}
}

func TestAdminAuthSetPasswordFailsWithoutInteractiveInput(t *testing.T) {
	app := NewAppContext()
	cfgPath := filepath.Join(t.TempDir(), "admin_auth.json")
	if err := os.WriteFile(cfgPath, []byte(`{"username":"admin","password":"legacy-pass"}`), 0o644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	oldReader := adminAuthPasswordReader
	adminAuthPasswordReader = func(prompt string) (string, error) {
		return "", os.ErrInvalid
	}
	defer func() {
		adminAuthPasswordReader = oldReader
	}()

	err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "set-password",
		"--config", cfgPath,
		"--cost", "4",
	})
	if err == nil {
		t.Fatalf("expected set-password to fail when no password prompt available")
	}
}

func TestAdminAuthVerifyFailsWithoutInteractiveInput(t *testing.T) {
	app := NewAppContext()
	cfgPath := filepath.Join(t.TempDir(), "admin_auth.json")
	if err := os.WriteFile(cfgPath, []byte(`{"username":"admin","password":"legacy-pass"}`), 0o644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	oldReader := adminAuthPasswordReader
	adminAuthPasswordReader = func(prompt string) (string, error) {
		return "", os.ErrInvalid
	}
	defer func() {
		adminAuthPasswordReader = oldReader
	}()

	err := executeCLICommand(context.Background(), app, nil, []string{
		"admin", "auth", "verify",
		"--config", cfgPath,
	})
	if err == nil {
		t.Fatalf("expected verify to fail when no password prompt available")
	}
}
