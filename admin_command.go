package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/spf13/cobra"
)

const (
	defaultAdminAuthBcryptCost     = 12
	defaultAdminAuthTempPassLength = 24
)

type adminStatusResult struct {
	Mode    string                  `json:"mode"`
	Status  string                  `json:"status"`
	Runtime *daemonAdminRuntimeInfo `json:"runtime,omitempty"`
	URL     string                  `json:"url,omitempty"`
	Message string                  `json:"message,omitempty"`
}

type adminAuthResult struct {
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	Config   string `json:"config"`
	Username string `json:"username,omitempty"`
	Message  string `json:"message,omitempty"`
}

var adminAuthPasswordReader = readAdminAuthPasswordFromTerminal

func newAdminCommand(app *appContext) *cobra.Command {
	var runtimePath string

	adminCmd := &cobra.Command{
		Use:   "admin",
		Short: "Inspect daemon admin service runtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAdminStatus(app, runtimePath)
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon admin runtime status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAdminStatus(app, runtimePath)
		},
	}

	adminCmd.Flags().StringVar(&runtimePath, "runtime", defaultDaemonAdminRuntimePath(), "Path to daemon admin runtime state JSON")
	statusCmd.Flags().StringVar(&runtimePath, "runtime", defaultDaemonAdminRuntimePath(), "Path to daemon admin runtime state JSON")
	adminCmd.AddCommand(statusCmd)
	adminCmd.AddCommand(newAdminAuthCommand(app))
	return adminCmd
}

func newAdminAuthCommand(app *appContext) *cobra.Command {
	var configPath string
	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage daemon admin auth password",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	authCmd.PersistentFlags().StringVar(&configPath, "config", defaultDaemonAdminAuthConfigPath(), "Path to daemon admin auth config JSON")
	authCmd.AddCommand(newAdminAuthSetPasswordCommand(app, &configPath))
	authCmd.AddCommand(newAdminAuthMigrateCommand(app, &configPath))
	authCmd.AddCommand(newAdminAuthResetPasswordCommand(app, &configPath))
	authCmd.AddCommand(newAdminAuthVerifyCommand(app, &configPath))
	return authCmd
}

func newAdminAuthSetPasswordCommand(app *appContext, configPath *string) *cobra.Command {
	var (
		username string
		password string
		cost     int
	)
	cmd := &cobra.Command{
		Use:   "set-password",
		Short: "Set admin password and persist password_hash",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateAdminAuthBcryptCost(cost); err != nil {
				return err
			}
			path := strings.TrimSpace(*configPath)
			cfg, _, err := loadDaemonAdminAuthConfigOptional(path)
			if err != nil {
				return err
			}
			effectiveUsername, err := resolveDaemonAdminAuthUsername(cfg, username)
			if err != nil {
				return err
			}
			effectivePassword, err := resolveAdminPasswordWithOptionalPrompt(password, true)
			if err != nil {
				return err
			}
			hash, err := hashDaemonAdminPassword(effectivePassword, cost)
			if err != nil {
				return err
			}
			nextCfg := daemonAdminAuthConfig{Username: effectiveUsername, PasswordHash: hash}
			if err := writeDaemonAdminAuthConfig(path, nextCfg); err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("admin", []string{"auth", "set-password"})
				app.SetResult(adminAuthResult{
					Mode:     "admin_auth_set_password",
					Status:   "ok",
					Config:   path,
					Username: effectiveUsername,
				})
			}
			fmt.Printf("admin auth password_hash updated: username=%s config=%s\n", effectiveUsername, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Admin username (defaults to existing config username)")
	cmd.Flags().StringVar(&password, "password", "", "Password in automation mode (avoid shell history leaks)")
	cmd.Flags().IntVar(&cost, "cost", defaultAdminAuthBcryptCost, "bcrypt cost")
	return cmd
}

func newAdminAuthMigrateCommand(app *appContext, configPath *string) *cobra.Command {
	var cost int
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate legacy plaintext password to password_hash",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateAdminAuthBcryptCost(cost); err != nil {
				return err
			}
			path := strings.TrimSpace(*configPath)
			cfg, err := loadDaemonAdminAuthConfig(path)
			if err != nil {
				return err
			}
			cfg = normalizeDaemonAdminAuthConfig(cfg)
			if cfg.PasswordHash != "" {
				if app != nil {
					app.SetExecution("admin", []string{"auth", "migrate"})
					app.SetResult(adminAuthResult{
						Mode:     "admin_auth_migrate",
						Status:   "no_op",
						Config:   path,
						Username: cfg.Username,
						Message:  "password_hash already exists",
					})
				}
				fmt.Println("admin auth migrate no-op: password_hash already exists")
				return nil
			}
			if cfg.Password == "" {
				return fmt.Errorf("legacy password is empty; set a new password with admin auth set-password")
			}
			hash, err := hashDaemonAdminPassword(cfg.Password, cost)
			if err != nil {
				return err
			}
			nextCfg := daemonAdminAuthConfig{Username: cfg.Username, PasswordHash: hash}
			if err := writeDaemonAdminAuthConfig(path, nextCfg); err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("admin", []string{"auth", "migrate"})
				app.SetResult(adminAuthResult{
					Mode:     "admin_auth_migrate",
					Status:   "ok",
					Config:   path,
					Username: cfg.Username,
				})
			}
			fmt.Printf("admin auth migrated to password_hash: username=%s config=%s\n", cfg.Username, path)
			return nil
		},
	}
	cmd.Flags().IntVar(&cost, "cost", defaultAdminAuthBcryptCost, "bcrypt cost")
	return cmd
}

func newAdminAuthResetPasswordCommand(app *appContext, configPath *string) *cobra.Command {
	var (
		username string
		generate bool
		length   int
		cost     int
	)
	cmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Reset admin password and print one-time temporary password",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !generate {
				return fmt.Errorf("reset-password requires --generate")
			}
			if err := validateAdminAuthBcryptCost(cost); err != nil {
				return err
			}
			if length < 8 {
				return fmt.Errorf("length must be >= 8")
			}
			path := strings.TrimSpace(*configPath)
			cfg, _, err := loadDaemonAdminAuthConfigOptional(path)
			if err != nil {
				return err
			}
			effectiveUsername, err := resolveDaemonAdminAuthUsername(cfg, username)
			if err != nil {
				return err
			}
			tempPassword, err := generateDaemonAdminTemporaryPassword(length)
			if err != nil {
				return err
			}
			hash, err := hashDaemonAdminPassword(tempPassword, cost)
			if err != nil {
				return err
			}
			nextCfg := daemonAdminAuthConfig{Username: effectiveUsername, PasswordHash: hash}
			if err := writeDaemonAdminAuthConfig(path, nextCfg); err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("admin", []string{"auth", "reset-password"})
				app.SetResult(adminAuthResult{
					Mode:     "admin_auth_reset_password",
					Status:   "ok",
					Config:   path,
					Username: effectiveUsername,
				})
			}
			fmt.Printf("admin auth temporary password (shown once): %s\n", tempPassword)
			fmt.Printf("admin auth password_hash reset complete: username=%s config=%s\n", effectiveUsername, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Admin username (defaults to existing config username)")
	cmd.Flags().BoolVar(&generate, "generate", false, "Generate a temporary password and print it once")
	cmd.Flags().IntVar(&length, "length", defaultAdminAuthTempPassLength, "Temporary password length")
	cmd.Flags().IntVar(&cost, "cost", defaultAdminAuthBcryptCost, "bcrypt cost")
	return cmd
}

func newAdminAuthVerifyCommand(app *appContext, configPath *string) *cobra.Command {
	var (
		username string
		password string
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify admin password against config without modifying it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := strings.TrimSpace(*configPath)
			cfg, err := loadDaemonAdminAuthConfig(path)
			if err != nil {
				return err
			}
			verifyUsername, err := resolveDaemonAdminAuthUsername(cfg, username)
			if err != nil {
				return err
			}
			verifyPassword, err := resolveAdminPasswordWithOptionalPrompt(password, false)
			if err != nil {
				return err
			}
			if !daemonAdminCredentialsMatch(cfg, verifyUsername, verifyPassword) {
				if app != nil {
					app.SetExecution("admin", []string{"auth", "verify"})
					app.SetResult(adminAuthResult{
						Mode:     "admin_auth_verify",
						Status:   "failed",
						Config:   path,
						Username: verifyUsername,
						Message:  "authentication failed",
					})
				}
				return fmt.Errorf("admin auth verification failed")
			}
			if app != nil {
				app.SetExecution("admin", []string{"auth", "verify"})
				app.SetResult(adminAuthResult{
					Mode:     "admin_auth_verify",
					Status:   "ok",
					Config:   path,
					Username: verifyUsername,
				})
			}
			fmt.Printf("admin auth verification succeeded: username=%s config=%s\n", verifyUsername, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Username to verify (defaults to config username)")
	cmd.Flags().StringVar(&password, "password", "", "Password in automation mode (avoid shell history leaks)")
	return cmd
}

func runAdminStatus(app *appContext, runtimePath string) error {
	status, runtime, err := daemonAdminRuntimeStatus(runtimePath)
	if err != nil {
		return err
	}
	result := adminStatusResult{
		Mode:    "status",
		Status:  status,
		Runtime: runtime,
	}
	if runtime != nil {
		result.URL = defaultDaemonAdminURL(runtime.Address)
	}
	if status == "stale" {
		result.Message = "admin runtime exists but daemon process is not running"
	}
	if status == "stopped" {
		result.Message = "admin runtime state not found"
	}

	if app != nil {
		app.SetExecution("admin", []string{"status"})
		app.SetResult(result)
	}

	switch status {
	case "running":
		if runtime != nil {
			fmt.Printf("admin status=%s pid=%d addr=%s url=%s\n", status, runtime.PID, runtime.Address, strings.TrimSpace(result.URL))
		} else {
			fmt.Printf("admin status=%s\n", status)
		}
	case "stale":
		if runtime != nil {
			fmt.Printf("admin status=%s pid=%d addr=%s\n", status, runtime.PID, runtime.Address)
		} else {
			fmt.Printf("admin status=%s\n", status)
		}
	default:
		fmt.Printf("admin status=%s\n", status)
	}
	if strings.TrimSpace(result.Message) != "" {
		fmt.Println(result.Message)
	}
	return nil
}

func loadDaemonAdminAuthConfigOptional(path string) (daemonAdminAuthConfig, bool, error) {
	cfg, err := loadDaemonAdminAuthConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return daemonAdminAuthConfig{}, false, nil
		}
		return daemonAdminAuthConfig{}, false, err
	}
	return cfg, true, nil
}

func resolveDaemonAdminAuthUsername(cfg daemonAdminAuthConfig, requested string) (string, error) {
	username := strings.TrimSpace(requested)
	if username != "" {
		return username, nil
	}
	fallback := strings.TrimSpace(cfg.Username)
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("username is required (set --username)")
}

func validateAdminAuthBcryptCost(cost int) error {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return fmt.Errorf("cost must be between %d and %d", bcrypt.MinCost, bcrypt.MaxCost)
	}
	return nil
}

func hashDaemonAdminPassword(password string, cost int) (string, error) {
	trimmed := strings.TrimSpace(password)
	if trimmed == "" {
		return "", fmt.Errorf("password is required")
	}
	if err := validateAdminAuthBcryptCost(cost); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(trimmed), cost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func resolveAdminPasswordWithOptionalPrompt(password string, requireConfirm bool) (string, error) {
	trimmed := strings.TrimSpace(password)
	if trimmed != "" {
		return trimmed, nil
	}
	first, err := adminAuthPasswordReader("Enter password: ")
	if err != nil {
		return "", err
	}
	if !requireConfirm {
		return first, nil
	}
	confirm, err := adminAuthPasswordReader("Confirm password: ")
	if err != nil {
		return "", err
	}
	if first != confirm {
		return "", fmt.Errorf("password confirmation does not match")
	}
	return first, nil
}

func readAdminAuthPasswordFromTerminal(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("non-interactive terminal requires --password")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Password: "
	}
	if _, err := fmt.Fprint(os.Stdout, prompt); err != nil {
		return "", err
	}
	raw, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(os.Stdout)
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(string(raw))
	if password == "" {
		return "", fmt.Errorf("password is required")
	}
	return password, nil
}

func generateDaemonAdminTemporaryPassword(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("length must be > 0")
	}
	bytesLen := (length*3 + 3) / 4
	randomBytes := make([]byte, bytesLen)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(randomBytes)
	if len(encoded) < length {
		return "", fmt.Errorf("generated password length is too short")
	}
	return encoded[:length], nil
}
