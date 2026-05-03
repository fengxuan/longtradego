package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
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

	result, err := performUpgradeInstall(context.Background(), "", true, true, strings.NewReader(""), io.Discard)
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

	_, err := performUpgradeInstall(context.Background(), "", true, false, strings.NewReader(""), io.Discard)
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

	_, err := performUpgradeInstall(context.Background(), "", true, false, strings.NewReader(""), io.Discard)
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

	result, err := performUpgradeInstall(context.Background(), "", true, false, strings.NewReader(""), io.Discard)
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

	_, err := performUpgradeInstall(context.Background(), "", true, false, strings.NewReader(""), io.Discard)
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

func TestShouldSkipAutomaticUpgradeCheck(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{args: []string{"upgrade"}, want: true},
		{args: []string{"version"}, want: true},
		{args: []string{"help"}, want: true},
		{args: []string{"quote", "AAPL.US"}, want: false},
	}
	for _, tc := range cases {
		if got := shouldSkipAutomaticUpgradeCheck(tc.args); got != tc.want {
			t.Fatalf("shouldSkipAutomaticUpgradeCheck(%v)=%t want=%t", tc.args, got, tc.want)
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
