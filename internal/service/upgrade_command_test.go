package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPerformUpgradeCheckUpdateAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	withUpgradeTestWorkingDir(t, tmpDir)

	oldVersion := buildVersion
	oldFetch := upgradeFetchRelease
	oldNow := upgradeNow
	buildVersion = "v1.0.0"
	upgradeNow = func() time.Time { return time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC) }
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return githubRelease{TagName: "v1.2.0", HTMLURL: "https://example.com/release/v1.2.0", PublishedAt: "2026-05-01T09:00:00Z"}, nil
	}
	defer func() {
		buildVersion = oldVersion
		upgradeFetchRelease = oldFetch
		upgradeNow = oldNow
	}()

	result, err := performUpgradeCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("performUpgradeCheck failed: %v", err)
	}
	if result.Status != "update_available" || !result.UpdateAvailable {
		t.Fatalf("expected update_available result, got %+v", result)
	}
	statePath := defaultUpdateStatePath()
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected update state file created, err=%v", err)
	}
	state, exists, err := readUpgradeUpdateState(statePath)
	if err != nil {
		t.Fatalf("readUpgradeUpdateState failed: %v", err)
	}
	if !exists {
		t.Fatalf("expected update state exists")
	}
	if state.LastAvailableVersion != "v1.2.0" {
		t.Fatalf("unexpected state last_available_version: %+v", state)
	}
}

func TestPerformUpgradeCheckFetchError(t *testing.T) {
	oldFetch := upgradeFetchRelease
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return githubRelease{}, errors.New("network down")
	}
	defer func() { upgradeFetchRelease = oldFetch }()

	_, err := performUpgradeCheck(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("expected network error, got %v", err)
	}
}

func TestFetchGitHubReleaseExplicitVersionSkipsAPI(t *testing.T) {
	oldBaseURL := upgradeGitHubAPIBaseURL
	oldClient := upgradeHTTPClient
	defer func() {
		upgradeGitHubAPIBaseURL = oldBaseURL
		upgradeHTTPClient = oldClient
	}()

	upgradeGitHubAPIBaseURL = "http://127.0.0.1:1"
	upgradeHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("did not expect HTTP request for explicit version: %s", req.URL.String())
		return nil, nil
	})}

	release, err := fetchGitHubRelease(context.Background(), "fengxuan/longtradego", "v1.0.5")
	if err != nil {
		t.Fatalf("fetchGitHubRelease explicit version failed: %v", err)
	}
	if release.TagName != "v1.0.5" {
		t.Fatalf("unexpected tag: %+v", release)
	}
	if len(release.Assets) != 2 {
		t.Fatalf("expected direct release assets, got %+v", release.Assets)
	}
	if !strings.Contains(release.Assets[0].BrowserDownloadURL, "/releases/download/v1.0.5/") {
		t.Fatalf("expected direct download url, got %+v", release.Assets[0])
	}
}

func TestFetchGitHubReleaseLatestUsesGitHubToken(t *testing.T) {
	oldBaseURL := upgradeGitHubAPIBaseURL
	oldClient := upgradeHTTPClient
	oldToken := upgradeGitHubToken
	defer func() {
		upgradeGitHubAPIBaseURL = oldBaseURL
		upgradeHTTPClient = oldClient
		upgradeGitHubToken = oldToken
	}()

	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/fengxuan/longtradego/releases/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v1.0.5", HTMLURL: "https://example.com/r/v1.0.5"})
	}))
	defer server.Close()

	upgradeGitHubAPIBaseURL = server.URL
	upgradeHTTPClient = server.Client()
	upgradeGitHubToken = func() string { return "token-123" }

	release, err := fetchGitHubRelease(context.Background(), "fengxuan/longtradego", "")
	if err != nil {
		t.Fatalf("fetchGitHubRelease latest failed: %v", err)
	}
	if authHeader != "Bearer token-123" {
		t.Fatalf("expected bearer token header, got %q", authHeader)
	}
	if release.TagName != "v1.0.5" {
		t.Fatalf("unexpected release: %+v", release)
	}
}

func TestBuildDirectGitHubRelease(t *testing.T) {
	release := buildDirectGitHubRelease("fengxuan/longtradego", "v1.0.5", "darwin", "arm64")
	if release.TagName != "v1.0.5" {
		t.Fatalf("unexpected tag: %+v", release)
	}
	if len(release.Assets) != 2 {
		t.Fatalf("expected 2 assets, got %+v", release.Assets)
	}
	if release.Assets[0].Name != "longtradego_1.0.5_darwin_arm64.tar.gz" {
		t.Fatalf("unexpected binary asset: %+v", release.Assets[0])
	}
	if release.Assets[1].BrowserDownloadURL != "https://github.com/fengxuan/longtradego/releases/download/v1.0.5/checksums.txt" {
		t.Fatalf("unexpected checksum asset: %+v", release.Assets[1])
	}
}

func TestRunAutomaticUpgradeCheckInteractiveReminder(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "update_state.json")

	oldVersion := buildVersion
	oldFetch := upgradeFetchRelease
	oldNow := upgradeNow
	buildVersion = "v1.0.0"
	upgradeNow = func() time.Time { return time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC) }
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return githubRelease{TagName: "v1.3.0"}, nil
	}
	defer func() {
		buildVersion = oldVersion
		upgradeFetchRelease = oldFetch
		upgradeNow = oldNow
	}()

	var nonInteractiveOut bytes.Buffer
	state, err := runAutomaticUpgradeCheck(context.Background(), statePath, &nonInteractiveOut, false)
	if err != nil {
		t.Fatalf("runAutomaticUpgradeCheck non-interactive failed: %v", err)
	}
	if strings.TrimSpace(nonInteractiveOut.String()) != "" {
		t.Fatalf("expected no reminder in non-interactive mode, got %q", nonInteractiveOut.String())
	}
	if state.LastAvailableVersion != "v1.3.0" {
		t.Fatalf("expected cached available version, got %+v", state)
	}

	state.LastCheckedAt = upgradeNow().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	state.LastNotifiedVersion = ""
	if err := writeUpgradeUpdateState(statePath, state); err != nil {
		t.Fatalf("writeUpgradeUpdateState failed: %v", err)
	}

	var interactiveOut bytes.Buffer
	updated, err := runAutomaticUpgradeCheck(context.Background(), statePath, &interactiveOut, true)
	if err != nil {
		t.Fatalf("runAutomaticUpgradeCheck interactive failed: %v", err)
	}
	if !strings.Contains(interactiveOut.String(), "update available") {
		t.Fatalf("expected reminder output, got %q", interactiveOut.String())
	}
	if updated.LastNotifiedVersion != "v1.3.0" {
		t.Fatalf("expected last_notified_version updated, got %+v", updated)
	}
}

func TestResolveUpgradeTargetVersion(t *testing.T) {
	cases := []struct {
		name string
		flag string
		args []string
		want string
	}{
		{name: "flag wins", flag: "v1.0.9", args: []string{"v1.0.8"}, want: "v1.0.9"},
		{name: "positional fallback", flag: "", args: []string{"v1.0.9"}, want: "v1.0.9"},
		{name: "trim positional", flag: "", args: []string{"  v1.0.9  "}, want: "v1.0.9"},
		{name: "empty", flag: "", args: nil, want: ""},
	}
	for _, tc := range cases {
		if got := resolveUpgradeTargetVersion(tc.flag, tc.args); got != tc.want {
			t.Fatalf("%s: resolveUpgradeTargetVersion(%q, %v)=%q want=%q", tc.name, tc.flag, tc.args, got, tc.want)
		}
	}
}

func TestNewUpgradeCommandAcceptsPositionalVersion(t *testing.T) {
	oldVersion := buildVersion
	oldFetch := upgradeFetchRelease
	oldInteractive := upgradeInteractiveTerminal
	defer func() {
		buildVersion = oldVersion
		upgradeFetchRelease = oldFetch
		upgradeInteractiveTerminal = oldInteractive
	}()

	buildVersion = "v1.0.0"
	upgradeInteractiveTerminal = func() bool { return false }
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		if targetVersion != "v1.0.9" {
			t.Fatalf("expected positional version v1.0.9, got %q", targetVersion)
		}
		return testUpgradeRelease("v1.0.9"), nil
	}

	cmd := newUpgradeCommand(nil)
	cmd.SetArgs([]string{"v1.0.9", "--dry-run", "--yes"})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected positional version dry-run to succeed, got %v", err)
	}
}

func TestPerformUpgradeInstallPromptsForVersion(t *testing.T) {
	tmpDir := t.TempDir()
	withUpgradeTestWorkingDir(t, tmpDir)
	exePath := filepath.Join(tmpDir, "longtradego")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write executable failed: %v", err)
	}

	release := testUpgradeRelease("v1.1.0")
	binaryAsset := release.Assets[0]
	checksumAsset := release.Assets[1]
	archive := mustBuildTarGzBinary(t, "longtradego", []byte("new-binary"))
	checksums := []byte(fmt.Sprintf("%s  %s\n", sha256Hex(archive), binaryAsset.Name))

	oldVersion := buildVersion
	oldFetch := upgradeFetchRelease
	oldDaemonStatus := upgradeDaemonRuntimeStatus
	oldExecutable := upgradeResolveExecutablePath
	oldDownload := upgradeDownloadContent
	oldInteractive := upgradeInteractiveTerminal
	buildVersion = "v1.0.0"
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		if targetVersion != "v1.1.0" {
			t.Fatalf("expected prompted version v1.1.0, got %q", targetVersion)
		}
		return release, nil
	}
	upgradeDaemonRuntimeStatus = func(path string) (string, *daemonAdminRuntimeInfo, error) {
		return "stopped", nil, nil
	}
	upgradeResolveExecutablePath = func() (string, error) { return exePath, nil }
	upgradeDownloadContent = func(ctx context.Context, downloadURL string) ([]byte, error) {
		switch downloadURL {
		case binaryAsset.BrowserDownloadURL:
			return archive, nil
		case checksumAsset.BrowserDownloadURL:
			return checksums, nil
		default:
			return nil, fmt.Errorf("unexpected download url: %s", downloadURL)
		}
	}
	upgradeInteractiveTerminal = func() bool { return true }
	defer func() {
		buildVersion = oldVersion
		upgradeFetchRelease = oldFetch
		upgradeDaemonRuntimeStatus = oldDaemonStatus
		upgradeResolveExecutablePath = oldExecutable
		upgradeDownloadContent = oldDownload
		upgradeInteractiveTerminal = oldInteractive
	}()

	var out bytes.Buffer
	result, err := performUpgradeInstall(context.Background(), "", true, false, strings.NewReader("v1.1.0\n"), &out)
	if err != nil {
		t.Fatalf("performUpgradeInstall prompt flow failed: %v", err)
	}
	if result.Status != "upgraded" {
		t.Fatalf("expected upgraded status, got %+v", result)
	}
	if !strings.Contains(out.String(), "enter target version") {
		t.Fatalf("expected version prompt output, got %q", out.String())
	}
}

func TestPerformUpgradeInstallRequiresVersionInNonInteractiveMode(t *testing.T) {
	oldInteractive := upgradeInteractiveTerminal
	defer func() { upgradeInteractiveTerminal = oldInteractive }()
	upgradeInteractiveTerminal = func() bool { return false }

	_, err := performUpgradeInstall(context.Background(), "", true, true, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires --version") {
		t.Fatalf("expected requires --version error, got %v", err)
	}
}

func TestPrintUpgradeReminderIncludesVersionCommand(t *testing.T) {
	var out bytes.Buffer
	printUpgradeReminder(&out, "v1.0.0", "v1.2.0")
	if !strings.Contains(out.String(), "longtradego upgrade --version v1.2.0") {
		t.Fatalf("expected versioned upgrade hint, got %q", out.String())
	}
}

func TestPerformUpgradeInstallDryRun(t *testing.T) {
	tmpDir := t.TempDir()
	withUpgradeTestWorkingDir(t, tmpDir)
	exePath := filepath.Join(tmpDir, "longtradego")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write executable failed: %v", err)
	}

	oldVersion := buildVersion
	oldFetch := upgradeFetchRelease
	oldDaemonStatus := upgradeDaemonRuntimeStatus
	oldExecutable := upgradeResolveExecutablePath
	buildVersion = "v1.0.0"
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return testUpgradeRelease("v1.1.0"), nil
	}
	upgradeDaemonRuntimeStatus = func(path string) (string, *daemonAdminRuntimeInfo, error) {
		return "stopped", nil, nil
	}
	upgradeResolveExecutablePath = func() (string, error) { return exePath, nil }
	defer func() {
		buildVersion = oldVersion
		upgradeFetchRelease = oldFetch
		upgradeDaemonRuntimeStatus = oldDaemonStatus
		upgradeResolveExecutablePath = oldExecutable
	}()

	result, err := performUpgradeInstall(context.Background(), "v1.1.0", true, true, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("performUpgradeInstall dry-run failed: %v", err)
	}
	if result.Status != "dry_run" {
		t.Fatalf("expected dry_run status, got %+v", result)
	}
	raw, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read executable failed: %v", err)
	}
	if string(raw) != "old-binary" {
		t.Fatalf("expected executable unchanged in dry-run, got %q", string(raw))
	}
}

func TestPerformUpgradeInstallBlocksDaemonRunning(t *testing.T) {
	oldFetch := upgradeFetchRelease
	oldDaemonStatus := upgradeDaemonRuntimeStatus
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return testUpgradeRelease("v1.1.0"), nil
	}
	upgradeDaemonRuntimeStatus = func(path string) (string, *daemonAdminRuntimeInfo, error) {
		return "running", &daemonAdminRuntimeInfo{PID: os.Getpid(), Address: ":18080"}, nil
	}
	defer func() {
		upgradeFetchRelease = oldFetch
		upgradeDaemonRuntimeStatus = oldDaemonStatus
	}()

	_, err := performUpgradeInstall(context.Background(), "v1.1.0", true, false, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "daemon is running") {
		t.Fatalf("expected daemon running error, got %v", err)
	}
}

func TestPerformUpgradeInstallRejectsGoRunBinary(t *testing.T) {
	oldFetch := upgradeFetchRelease
	oldDaemonStatus := upgradeDaemonRuntimeStatus
	oldExecutable := upgradeResolveExecutablePath
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return testUpgradeRelease("v1.1.0"), nil
	}
	upgradeDaemonRuntimeStatus = func(path string) (string, *daemonAdminRuntimeInfo, error) {
		return "stopped", nil, nil
	}
	upgradeResolveExecutablePath = func() (string, error) {
		return "/private/var/folders/x/go-build/abc123/exe/main", nil
	}
	defer func() {
		upgradeFetchRelease = oldFetch
		upgradeDaemonRuntimeStatus = oldDaemonStatus
		upgradeResolveExecutablePath = oldExecutable
	}()

	_, err := performUpgradeInstall(context.Background(), "v1.1.0", true, false, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "go run") {
		t.Fatalf("expected go run rejection error, got %v", err)
	}
}

func TestPerformUpgradeInstallSuccessReplacesBinary(t *testing.T) {
	tmpDir := t.TempDir()
	withUpgradeTestWorkingDir(t, tmpDir)
	exePath := filepath.Join(tmpDir, "longtradego")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write executable failed: %v", err)
	}

	release := testUpgradeRelease("v1.1.0")
	binaryAsset := release.Assets[0]
	checksumAsset := release.Assets[1]
	archive := mustBuildTarGzBinary(t, "longtradego", []byte("new-binary"))
	checksums := []byte(fmt.Sprintf("%s  %s\n", sha256Hex(archive), binaryAsset.Name))

	oldVersion := buildVersion
	oldFetch := upgradeFetchRelease
	oldDaemonStatus := upgradeDaemonRuntimeStatus
	oldExecutable := upgradeResolveExecutablePath
	oldDownload := upgradeDownloadContent
	buildVersion = "v1.0.0"
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return release, nil
	}
	upgradeDaemonRuntimeStatus = func(path string) (string, *daemonAdminRuntimeInfo, error) {
		return "stopped", nil, nil
	}
	upgradeResolveExecutablePath = func() (string, error) { return exePath, nil }
	upgradeDownloadContent = func(ctx context.Context, downloadURL string) ([]byte, error) {
		switch downloadURL {
		case binaryAsset.BrowserDownloadURL:
			return archive, nil
		case checksumAsset.BrowserDownloadURL:
			return checksums, nil
		default:
			return nil, fmt.Errorf("unexpected download url: %s", downloadURL)
		}
	}
	defer func() {
		buildVersion = oldVersion
		upgradeFetchRelease = oldFetch
		upgradeDaemonRuntimeStatus = oldDaemonStatus
		upgradeResolveExecutablePath = oldExecutable
		upgradeDownloadContent = oldDownload
	}()

	result, err := performUpgradeInstall(context.Background(), "v1.1.0", true, false, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("performUpgradeInstall failed: %v", err)
	}
	if result.Status != "upgraded" {
		t.Fatalf("expected upgraded status, got %+v", result)
	}
	raw, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read upgraded executable failed: %v", err)
	}
	if string(raw) != "new-binary" {
		t.Fatalf("expected executable replaced, got %q", string(raw))
	}
}

func TestPerformUpgradeInstallChecksumMismatchDoesNotReplace(t *testing.T) {
	tmpDir := t.TempDir()
	withUpgradeTestWorkingDir(t, tmpDir)
	exePath := filepath.Join(tmpDir, "longtradego")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write executable failed: %v", err)
	}

	release := testUpgradeRelease("v1.1.0")
	binaryAsset := release.Assets[0]
	checksumAsset := release.Assets[1]
	archive := mustBuildTarGzBinary(t, "longtradego", []byte("new-binary"))
	checksums := []byte(fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), binaryAsset.Name))

	oldFetch := upgradeFetchRelease
	oldDaemonStatus := upgradeDaemonRuntimeStatus
	oldExecutable := upgradeResolveExecutablePath
	oldDownload := upgradeDownloadContent
	upgradeFetchRelease = func(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
		return release, nil
	}
	upgradeDaemonRuntimeStatus = func(path string) (string, *daemonAdminRuntimeInfo, error) {
		return "stopped", nil, nil
	}
	upgradeResolveExecutablePath = func() (string, error) { return exePath, nil }
	upgradeDownloadContent = func(ctx context.Context, downloadURL string) ([]byte, error) {
		switch downloadURL {
		case binaryAsset.BrowserDownloadURL:
			return archive, nil
		case checksumAsset.BrowserDownloadURL:
			return checksums, nil
		default:
			return nil, fmt.Errorf("unexpected download url: %s", downloadURL)
		}
	}
	defer func() {
		upgradeFetchRelease = oldFetch
		upgradeDaemonRuntimeStatus = oldDaemonStatus
		upgradeResolveExecutablePath = oldExecutable
		upgradeDownloadContent = oldDownload
	}()

	_, err := performUpgradeInstall(context.Background(), "v1.1.0", true, false, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
	raw, readErr := os.ReadFile(exePath)
	if readErr != nil {
		t.Fatalf("read executable failed: %v", readErr)
	}
	if string(raw) != "old-binary" {
		t.Fatalf("expected executable unchanged on checksum mismatch, got %q", string(raw))
	}
}

func TestMaybeNotifyUpgradeAvailableRunsForInteractiveQuote(t *testing.T) {
	oldInteractive := upgradeInteractiveTerminal
	oldRunAutomatic := upgradeRunAutomaticCheck
	defer func() {
		upgradeInteractiveTerminal = oldInteractive
		upgradeRunAutomaticCheck = oldRunAutomatic
	}()

	upgradeInteractiveTerminal = func() bool { return true }
	called := false
	upgradeRunAutomaticCheck = func(ctx context.Context, statePath string, out io.Writer, interactive bool) (upgradeUpdateState, error) {
		called = true
		if !interactive {
			t.Fatalf("expected interactive automatic check")
		}
		if strings.TrimSpace(statePath) == "" {
			t.Fatalf("expected non-empty state path")
		}
		return upgradeUpdateState{}, nil
	}

	MaybeNotifyUpgradeAvailable(context.Background(), []string{"quote", "AAPL.US"}, io.Discard)
	if !called {
		t.Fatalf("expected automatic upgrade check to run for interactive quote")
	}
}

func TestMaybeNotifyUpgradeAvailableSkipsUpgradeCommand(t *testing.T) {
	oldInteractive := upgradeInteractiveTerminal
	oldRunAutomatic := upgradeRunAutomaticCheck
	defer func() {
		upgradeInteractiveTerminal = oldInteractive
		upgradeRunAutomaticCheck = oldRunAutomatic
	}()

	upgradeInteractiveTerminal = func() bool { return true }
	upgradeRunAutomaticCheck = func(ctx context.Context, statePath string, out io.Writer, interactive bool) (upgradeUpdateState, error) {
		t.Fatalf("did not expect automatic check for upgrade command")
		return upgradeUpdateState{}, nil
	}

	MaybeNotifyUpgradeAvailable(context.Background(), []string{"upgrade", "--version", "v1.0.5"}, io.Discard)
}

func TestMaybeNotifyUpgradeAvailableSkipsNonInteractiveCommands(t *testing.T) {
	oldInteractive := upgradeInteractiveTerminal
	oldRunAutomatic := upgradeRunAutomaticCheck
	defer func() {
		upgradeInteractiveTerminal = oldInteractive
		upgradeRunAutomaticCheck = oldRunAutomatic
	}()

	upgradeInteractiveTerminal = func() bool { return false }
	upgradeRunAutomaticCheck = func(ctx context.Context, statePath string, out io.Writer, interactive bool) (upgradeUpdateState, error) {
		t.Fatalf("did not expect automatic check for non-interactive command")
		return upgradeUpdateState{}, nil
	}

	MaybeNotifyUpgradeAvailable(context.Background(), []string{"quote", "AAPL.US"}, io.Discard)
}

func TestShouldSkipAutomaticUpgradeCheck(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		interactive bool
		want        bool
	}{
		{name: "empty interactive", args: nil, interactive: true, want: false},
		{name: "empty non interactive", args: nil, interactive: false, want: true},
		{name: "upgrade skipped", args: []string{"upgrade"}, interactive: true, want: true},
		{name: "help skipped", args: []string{"help"}, interactive: true, want: true},
		{name: "completion skipped", args: []string{"completion"}, interactive: true, want: true},
		{name: "quote runs", args: []string{"quote", "AAPL.US"}, interactive: true, want: false},
		{name: "longbridge runs", args: []string{"longbridge", "quote", "AAPL.US"}, interactive: true, want: false},
		{name: "daemon runs", args: []string{"daemon"}, interactive: true, want: false},
	}
	for _, tc := range cases {
		if got := shouldSkipAutomaticUpgradeCheck(tc.args, tc.interactive); got != tc.want {
			t.Fatalf("%s: shouldSkipAutomaticUpgradeCheck(%v, %t)=%t want=%t", tc.name, tc.args, tc.interactive, got, tc.want)
		}
	}
}

func testUpgradeRelease(tag string) githubRelease {
	version := strings.TrimPrefix(normalizeVersionTag(tag), "v")
	assetName := fmt.Sprintf("longtradego_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	return githubRelease{
		TagName:     normalizeVersionTag(tag),
		HTMLURL:     "https://example.com/release/" + normalizeVersionTag(tag),
		PublishedAt: "2026-05-01T09:00:00Z",
		Assets: []githubReleaseAsset{
			{Name: assetName, BrowserDownloadURL: "https://example.com/download/" + assetName},
			{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/download/checksums.txt"},
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mustBuildTarGzBinary(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gz)
	header := &tar.Header{
		Name: filepath.ToSlash(filepath.Join("longtradego", name)),
		Mode: 0o755,
		Size: int64(len(payload)),
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatalf("write tar header failed: %v", err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatalf("write tar payload failed: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer failed: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer failed: %v", err)
	}
	return buf.Bytes()
}

func withUpgradeTestWorkingDir(t *testing.T, dir string) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir to %s failed: %v", dir, err)
	}
	t.Cleanup(func() {
		if chdirErr := os.Chdir(oldDir); chdirErr != nil {
			t.Fatalf("restore working dir failed: %v", chdirErr)
		}
	})
}
