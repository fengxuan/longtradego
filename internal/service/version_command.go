package service

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

type versionResult struct {
	Mode       string `json:"mode"`
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	BuildDate  string `json:"build_date"`
	Platform   string `json:"platform"`
	GoVersion  string `json:"go_version"`
	IsDevBuild bool   `json:"is_dev_build"`
}

func newVersionCommand(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show build version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			result := buildVersionResult()
			if app != nil {
				app.SetExecution("version", []string{"version"})
				app.SetResult(result)
			}
			fmt.Printf("version=%s commit=%s build_date=%s platform=%s go=%s dev=%t\n",
				result.Version,
				result.Commit,
				result.BuildDate,
				result.Platform,
				result.GoVersion,
				result.IsDevBuild,
			)
			return nil
		},
	}
}

func buildVersionResult() versionResult {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = "dev"
	}
	commit := strings.TrimSpace(buildCommit)
	if commit == "" {
		commit = "unknown"
	}
	date := strings.TrimSpace(buildDate)
	if date == "" {
		date = "unknown"
	}
	return versionResult{
		Mode:       "version",
		Version:    version,
		Commit:     commit,
		BuildDate:  date,
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion:  runtime.Version(),
		IsDevBuild: isDevBuildVersion(version),
	}
}

func isDevBuildVersion(version string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(version))
	if trimmed == "" {
		return true
	}
	return trimmed == "dev" || strings.Contains(trimmed, "-dev")
}

func currentBuildVersion() string {
	return buildVersionResult().Version
}
