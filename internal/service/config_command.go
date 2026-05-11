package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type configPathsResult struct {
	Mode      string `json:"mode"`
	Layout    string `json:"layout"`
	ConfigDir string `json:"config_dir"`
	DataDir   string `json:"data_dir"`
	LogDir    string `json:"log_dir"`
}

type configInitResult struct {
	Mode      string   `json:"mode"`
	Dir       string   `json:"dir"`
	Selected  []string `json:"selected"`
	Created   []string `json:"created,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
	Overwrite bool     `json:"overwrite"`
}

type configTemplate struct {
	Name     string
	FileName string
	Content  string
}

const (
	configTemplateAdminAuth = `{
  "username": "admin",
  "password_hash": "$2a$12$jkV7cbbJU1CDJw.GWy2rYOCbjfActfvTzgTnfnjS3Zg1p5UylgkLK"
}
`
	configTemplateBookingLLM = `{
  "api_key": "your-openai-compatible-key",
  "base_url": "https://api.openai.com",
  "model": "gpt-4.1-mini"
}
`
	configTemplateEmailAliases = `{
  "aliases": {
    "qa": ["test@sohu.com"],
    "dev": ["test@163.com", "test@gmail.com"],
    "trading": ["test@gmail.com"]
  }
}
`
	configTemplateMailReceive = `{
  "default_alias": "main",
  "aliases": {
    "main": {
      "IMAP_HOST": "imap.163.com",
      "IMAP_PORT": 993,
      "IMAP_USERNAME": "test@163.com",
      "IMAP_PASSWORD": "test",
      "IMAP_MAILBOX": "INBOX",
      "IMAP_TLS": true,
      "IMAP_INSECURE_SKIP_VERIFY": false,
      "IMAP_ID_NAME": "longtradego",
      "IMAP_ID_VERSION": "1.0.0",
      "IMAP_ID_VENDOR": "longtradego",
      "IMAP_ID_ADDRESS": "test@163.com"
    }
  }
}
`
	configTemplateSecurityKeys = `{
  "version": 1,
  "tokens": [
    {
      "third_party_id": "partner-a",
      "token": "replace-with-strong-random-token",
      "scopes": ["booking", "webhook"],
      "created_at": "2026-05-03T00:00:00Z",
      "updated_at": "2026-05-03T00:00:00Z"
    }
  ]
}
`
)

var configInitTemplates = []configTemplate{
	{Name: "admin_auth", FileName: "admin_auth.json", Content: configTemplateAdminAuth},
	{Name: "booking_llm", FileName: "booking_llm.json", Content: configTemplateBookingLLM},
	{Name: "email_aliases", FileName: "email_aliases.json", Content: configTemplateEmailAliases},
	{Name: "mail_receive_setting", FileName: "mail_receive_setting.json", Content: configTemplateMailReceive},
	{Name: "security_keys", FileName: "security_keys.json", Content: configTemplateSecurityKeys},
}

func newConfigCommand(app *AppContext) *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and initialize default config paths",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigPaths(cmd, app)
		},
	}

	configCmd.AddCommand(newConfigPathsCommand(app))
	configCmd.AddCommand(newConfigInitCommand(app))
	return configCmd
}

func newConfigPathsCommand(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "paths",
		Short: "Show resolved config, data, and log directories",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigPaths(cmd, app)
		},
	}
}

func newConfigInitCommand(app *AppContext) *cobra.Command {
	var (
		dir       string
		overwrite bool
		only      []string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write example config files into the resolved config directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			targetDir := strings.TrimSpace(dir)
			if targetDir == "" {
				targetDir = defaultConfigDir()
			}
			return runConfigInit(cmd, app, targetDir, overwrite, only)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "Target config directory (defaults to resolved config dir)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite existing files")
	cmd.Flags().StringArrayVar(&only, "only", nil, "Only write selected template names (repeatable: admin_auth, booking_llm, email_aliases, mail_receive_setting, security_keys)")
	return cmd
}

func runConfigPaths(cmd *cobra.Command, app *AppContext) error {
	layout := "user_home"
	if useRepoRelativeLayout() {
		layout = "repo_relative"
	}
	result := configPathsResult{
		Mode:      "config_paths",
		Layout:    layout,
		ConfigDir: defaultConfigDir(),
		DataDir:   defaultDataDir(),
		LogDir:    defaultLogDir(),
	}
	if app != nil {
		app.SetExecution("config", []string{"config", "paths"})
		app.SetResult(result)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "layout=%s\nconfig=%s\ndata=%s\nlogs=%s\n", result.Layout, result.ConfigDir, result.DataDir, result.LogDir)
	return nil
}

func runConfigInit(cmd *cobra.Command, app *AppContext, dir string, overwrite bool, only []string) error {
	selected, err := selectConfigTemplates(only)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	created := make([]string, 0, len(selected))
	skipped := make([]string, 0, len(selected))
	selectedNames := make([]string, 0, len(selected))
	for _, template := range selected {
		selectedNames = append(selectedNames, template.Name)
		targetPath := filepath.Join(dir, template.FileName)
		if !overwrite {
			if _, statErr := os.Stat(targetPath); statErr == nil {
				skipped = append(skipped, targetPath)
				continue
			} else if !errorsIsNotExist(statErr) {
				return statErr
			}
		}
		if err := writeFileAtomic(targetPath, []byte(template.Content), 0o644); err != nil {
			return err
		}
		created = append(created, targetPath)
	}

	result := configInitResult{
		Mode:      "config_init",
		Dir:       dir,
		Selected:  selectedNames,
		Created:   created,
		Skipped:   skipped,
		Overwrite: overwrite,
	}
	if app != nil {
		app.SetExecution("config", []string{"config", "init"})
		app.SetResult(result)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "initialized config dir: %s\n", dir)
	for _, path := range created {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "created: %s\n", path)
	}
	for _, path := range skipped {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "skipped existing: %s\n", path)
	}
	if len(created) > 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "edit placeholder values before using booking, webhook, or mail features")
	}
	return nil
}

func selectConfigTemplates(only []string) ([]configTemplate, error) {
	if len(only) == 0 {
		selected := make([]configTemplate, len(configInitTemplates))
		copy(selected, configInitTemplates)
		return selected, nil
	}

	available := make(map[string]configTemplate, len(configInitTemplates))
	for _, template := range configInitTemplates {
		available[template.Name] = template
	}

	selected := make([]configTemplate, 0, len(only))
	seen := make(map[string]struct{}, len(only))
	for _, item := range only {
		name := normalizeConfigTemplateName(item)
		template, exists := available[name]
		if !exists {
			return nil, fmt.Errorf("unknown config template %q", item)
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		selected = append(selected, template)
	}
	return selected, nil
}

func normalizeConfigTemplateName(name string) string {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	return strings.TrimSuffix(trimmed, ".json")
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}
