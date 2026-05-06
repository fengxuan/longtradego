package service

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	updateStateFileName        = "update_state.json"
	updateStateVersion         = 1
	defaultUpgradeReleaseRepo  = "jianfengxuan/longtradego"
	defaultUpgradeCheckEvery   = 24 * time.Hour
	defaultUpgradeCheckTimeout = 4 * time.Second
	upgradeGithubAPIBase       = "https://api.github.com"
)

type upgradeUpdateState struct {
	Version              int    `json:"version"`
	LastCheckedAt        string `json:"last_checked_at,omitempty"`
	LastAvailableVersion string `json:"last_available_version,omitempty"`
	LastNotifiedVersion  string `json:"last_notified_version,omitempty"`
	UpdatedAt            string `json:"updated_at,omitempty"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName     string               `json:"tag_name"`
	HTMLURL     string               `json:"html_url"`
	PublishedAt string               `json:"published_at"`
	Draft       bool                 `json:"draft"`
	Prerelease  bool                 `json:"prerelease"`
	Assets      []githubReleaseAsset `json:"assets"`
}

type upgradeCommandResult struct {
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	CurrentVersion  string `json:"current_version"`
	TargetVersion   string `json:"target_version,omitempty"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	AssetName       string `json:"asset_name,omitempty"`
	DownloadURL     string `json:"download_url,omitempty"`
	CheckedAt       string `json:"checked_at,omitempty"`
	StatePath       string `json:"state_path,omitempty"`
	DryRun          bool   `json:"dry_run,omitempty"`
	Message         string `json:"message,omitempty"`
}

var (
	upgradeNow                   = time.Now
	upgradeFetchRelease          = fetchGitHubRelease
	upgradeDownloadContent       = downloadUpgradeContent
	upgradeResolveExecutablePath = os.Executable
	upgradeDaemonRuntimeStatus   = daemonAdminRuntimeStatus
	upgradeReplaceBinary         = replaceExecutableBinary
	upgradePromptConfirm         = promptUpgradeConfirmation
	upgradeInteractiveTerminal   = isInteractiveTerminal
)

func newUpgradeCommand(app *AppContext) *cobra.Command {
	var (
		targetVersion string
		yes           bool
		dryRun        bool
	)

	upgradeCmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Check and install new releases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgradeInstallCommand(cmd.Context(), app, strings.TrimSpace(targetVersion), yes, dryRun, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Check latest release availability",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgradeCheckCommand(cmd.Context(), app, strings.TrimSpace(targetVersion))
		},
	}

	upgradeCmd.PersistentFlags().StringVar(&targetVersion, "version", "", "Target version tag (default latest), e.g. v1.2.3")
	upgradeCmd.PersistentFlags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	upgradeCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Check and print planned action without replacing binary")
	upgradeCmd.AddCommand(checkCmd)
	return upgradeCmd
}

func runUpgradeCheckCommand(ctx context.Context, app *AppContext, targetVersion string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultUpgradeCheckTimeout)
	defer cancel()

	result, err := performUpgradeCheck(ctx, targetVersion)
	if app != nil {
		app.SetExecution("upgrade", []string{"check"})
		app.SetResult(result)
	}
	if err != nil {
		return err
	}

	if result.UpdateAvailable {
		fmt.Printf("update available: current=%s latest=%s release=%s\n", result.CurrentVersion, result.LatestVersion, strings.TrimSpace(result.ReleaseURL))
	} else {
		fmt.Printf("up to date: current=%s\n", result.CurrentVersion)
	}
	return nil
}

func runUpgradeInstallCommand(ctx context.Context, app *AppContext, targetVersion string, yes bool, dryRun bool, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := performUpgradeInstall(ctx, targetVersion, yes, dryRun, in, out)
	if app != nil {
		app.SetExecution("upgrade", []string{"upgrade"})
		app.SetResult(result)
	}
	if err != nil {
		return err
	}

	switch result.Status {
	case "upgraded":
		fmt.Printf("upgrade done: %s -> %s\n", result.CurrentVersion, result.TargetVersion)
	case "dry_run":
		fmt.Printf("upgrade dry-run: current=%s target=%s asset=%s\n", result.CurrentVersion, result.TargetVersion, result.AssetName)
	case "cancelled":
		fmt.Println("upgrade cancelled")
	case "up_to_date":
		fmt.Printf("up to date: current=%s\n", result.CurrentVersion)
	default:
		if strings.TrimSpace(result.Message) != "" {
			fmt.Println(result.Message)
		}
	}
	return nil
}

func defaultUpdateStatePath() string {
	return filepath.Join(daemonDataDir, updateStateFileName)
}

func resolveUpgradeReleaseRepo() string {
	if env := strings.TrimSpace(os.Getenv("LONGTRADEGO_RELEASE_REPO")); env != "" {
		return env
	}
	return defaultUpgradeReleaseRepo
}

func performUpgradeCheck(ctx context.Context, targetVersion string) (upgradeCommandResult, error) {
	repo := resolveUpgradeReleaseRepo()
	if strings.TrimSpace(repo) == "" {
		return upgradeCommandResult{}, fmt.Errorf("release repo is empty")
	}
	currentVersion := currentBuildVersion()
	release, err := upgradeFetchRelease(ctx, repo, targetVersion)
	if err != nil {
		return upgradeCommandResult{}, err
	}
	latestTag := normalizeVersionTag(release.TagName)
	updateAvailable := isReleaseNewerThanCurrent(currentVersion, latestTag)
	checkedAt := upgradeNow().Format(time.RFC3339Nano)

	result := upgradeCommandResult{
		Mode:            "check",
		Status:          "up_to_date",
		CurrentVersion:  currentVersion,
		TargetVersion:   normalizeVersionTag(targetVersion),
		LatestVersion:   latestTag,
		UpdateAvailable: updateAvailable,
		ReleaseURL:      strings.TrimSpace(release.HTMLURL),
		PublishedAt:     strings.TrimSpace(release.PublishedAt),
		CheckedAt:       checkedAt,
		StatePath:       defaultUpdateStatePath(),
	}
	if updateAvailable {
		result.Status = "update_available"
		result.Message = "new release available"
	}

	stateErr := updateUpgradeState(defaultUpdateStatePath(), func(state *upgradeUpdateState) {
		state.LastCheckedAt = checkedAt
		if updateAvailable {
			state.LastAvailableVersion = latestTag
		} else {
			state.LastAvailableVersion = ""
			state.LastNotifiedVersion = ""
		}
	})
	if stateErr != nil {
		return result, stateErr
	}
	return result, nil
}

func performUpgradeInstall(ctx context.Context, targetVersion string, yes bool, dryRun bool, in io.Reader, out io.Writer) (upgradeCommandResult, error) {
	repo := resolveUpgradeReleaseRepo()
	if strings.TrimSpace(repo) == "" {
		return upgradeCommandResult{}, fmt.Errorf("release repo is empty")
	}
	currentVersion := currentBuildVersion()
	release, err := upgradeFetchRelease(ctx, repo, targetVersion)
	if err != nil {
		return upgradeCommandResult{}, err
	}

	targetTag := normalizeVersionTag(release.TagName)
	updateAvailable := isReleaseNewerThanCurrent(currentVersion, targetTag)
	targetSpecified := strings.TrimSpace(targetVersion) != ""

	result := upgradeCommandResult{
		Mode:            "upgrade",
		Status:          "up_to_date",
		CurrentVersion:  currentVersion,
		TargetVersion:   targetTag,
		LatestVersion:   targetTag,
		UpdateAvailable: updateAvailable,
		ReleaseURL:      strings.TrimSpace(release.HTMLURL),
		PublishedAt:     strings.TrimSpace(release.PublishedAt),
		StatePath:       defaultUpdateStatePath(),
		DryRun:          dryRun,
	}

	if !targetSpecified && !updateAvailable {
		result.Message = "already on latest version"
		return result, nil
	}
	if targetSpecified && normalizeVersionTag(currentVersion) == targetTag {
		result.Message = "target version already installed"
		return result, nil
	}

	daemonStatus, _, daemonErr := upgradeDaemonRuntimeStatus(defaultDaemonAdminRuntimePath())
	if daemonErr != nil {
		return result, daemonErr
	}
	if daemonStatus == "running" {
		return result, fmt.Errorf("daemon is running; stop daemon first, then retry upgrade")
	}

	executablePath, err := upgradeResolveExecutablePath()
	if err != nil {
		return result, err
	}
	if looksLikeGoRunExecutable(executablePath) {
		return result, fmt.Errorf("upgrade is unavailable for `go run .`; use released binary installation instead")
	}

	binaryAsset, checksumAsset, err := selectUpgradeAssetsForPlatform(release, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return result, err
	}
	result.AssetName = binaryAsset.Name
	result.DownloadURL = binaryAsset.BrowserDownloadURL

	if dryRun {
		result.Status = "dry_run"
		result.Message = "dry-run completed"
		return result, nil
	}

	if !yes {
		if !upgradeInteractiveTerminal() {
			return result, fmt.Errorf("non-interactive terminal requires --yes to proceed")
		}
		confirmed, confirmErr := upgradePromptConfirm(in, out, currentVersion, targetTag)
		if confirmErr != nil {
			return result, confirmErr
		}
		if !confirmed {
			result.Status = "cancelled"
			result.Message = "upgrade cancelled"
			return result, nil
		}
	}

	checksumsContent, err := upgradeDownloadContent(ctx, checksumAsset.BrowserDownloadURL)
	if err != nil {
		return result, fmt.Errorf("download checksums failed: %w", err)
	}
	assetContent, err := upgradeDownloadContent(ctx, binaryAsset.BrowserDownloadURL)
	if err != nil {
		return result, fmt.Errorf("download release asset failed: %w", err)
	}

	expectedChecksum, err := checksumForAsset(checksumsContent, binaryAsset.Name)
	if err != nil {
		return result, err
	}
	actualChecksum := sha256Hex(assetContent)
	if !strings.EqualFold(expectedChecksum, actualChecksum) {
		return result, fmt.Errorf("checksum mismatch for %s: expected=%s actual=%s", binaryAsset.Name, expectedChecksum, actualChecksum)
	}

	binaryBytes, err := extractBinaryFromReleaseArchive(assetContent, "longtradego")
	if err != nil {
		return result, err
	}
	if err := upgradeReplaceBinary(executablePath, binaryBytes); err != nil {
		return result, err
	}

	result.Status = "upgraded"
	result.Message = "upgrade successful"
	result.UpdateAvailable = false

	stateErr := updateUpgradeState(defaultUpdateStatePath(), func(state *upgradeUpdateState) {
		state.LastCheckedAt = upgradeNow().Format(time.RFC3339Nano)
		state.LastAvailableVersion = ""
		state.LastNotifiedVersion = targetTag
	})
	if stateErr != nil {
		return result, stateErr
	}

	return result, nil
}

func MaybeNotifyUpgradeAvailable(ctx context.Context, rawArgs []string, out io.Writer) {
	normalizedArgs := NormalizeArgs(rawArgs)
	if shouldSkipAutomaticUpgradeCheck(normalizedArgs) {
		return
	}
	if out == nil {
		out = os.Stdout
	}

	statePath := defaultUpdateStatePath()
	_, _ = runAutomaticUpgradeCheck(ctx, statePath, out, upgradeInteractiveTerminal())
}

func shouldSkipAutomaticUpgradeCheck(args []string) bool {
	if len(args) == 0 {
		return true
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	switch first {
	case "daemon", "d":
		return false
	default:
		return true
	}
}

func runAutomaticUpgradeCheck(ctx context.Context, statePath string, out io.Writer, interactive bool) (upgradeUpdateState, error) {
	repo := resolveUpgradeReleaseRepo()
	if strings.TrimSpace(repo) == "" {
		return upgradeUpdateState{}, nil
	}
	state, _, err := readUpgradeUpdateState(statePath)
	if err != nil {
		return upgradeUpdateState{}, err
	}

	now := upgradeNow()
	lastChecked := parseRFC3339Nano(state.LastCheckedAt)
	currentVersion := currentBuildVersion()

	if !lastChecked.IsZero() && now.Sub(lastChecked) < defaultUpgradeCheckEvery {
		if !isReleaseNewerThanCurrent(currentVersion, state.LastAvailableVersion) {
			state.LastAvailableVersion = ""
			state.LastNotifiedVersion = ""
			if err := writeUpgradeUpdateState(statePath, state); err != nil {
				return state, err
			}
			return state, nil
		}
		if interactive && strings.TrimSpace(state.LastAvailableVersion) != "" && !strings.EqualFold(strings.TrimSpace(state.LastNotifiedVersion), strings.TrimSpace(state.LastAvailableVersion)) {
			printUpgradeReminder(out, currentVersion, state.LastAvailableVersion)
			state.LastNotifiedVersion = strings.TrimSpace(state.LastAvailableVersion)
			if err := writeUpgradeUpdateState(statePath, state); err != nil {
				return state, err
			}
		}
		return state, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, defaultUpgradeCheckTimeout)
	defer cancel()
	release, err := upgradeFetchRelease(fetchCtx, repo, "")
	if err != nil {
		return state, err
	}
	latestTag := normalizeVersionTag(release.TagName)
	available := isReleaseNewerThanCurrent(currentVersion, latestTag)

	state.LastCheckedAt = now.Format(time.RFC3339Nano)
	if available {
		state.LastAvailableVersion = latestTag
	} else {
		state.LastAvailableVersion = ""
		state.LastNotifiedVersion = ""
	}
	if interactive && available && !strings.EqualFold(strings.TrimSpace(state.LastNotifiedVersion), latestTag) {
		printUpgradeReminder(out, currentVersion, latestTag)
		state.LastNotifiedVersion = latestTag
	}
	if err := writeUpgradeUpdateState(statePath, state); err != nil {
		return state, err
	}
	return state, nil
}

func printUpgradeReminder(out io.Writer, currentVersion string, latestVersion string) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "\nupdate available: current=%s latest=%s\nrun `longtradego upgrade` to install, or `longtradego upgrade check` for details.\n\n", currentVersion, latestVersion)
}

func updateUpgradeState(statePath string, mutate func(state *upgradeUpdateState)) error {
	state, _, err := readUpgradeUpdateState(statePath)
	if err != nil {
		return err
	}
	if mutate != nil {
		mutate(&state)
	}
	return writeUpgradeUpdateState(statePath, state)
}

func readUpgradeUpdateState(path string) (upgradeUpdateState, bool, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return upgradeUpdateState{}, false, fmt.Errorf("update state path is empty")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return upgradeUpdateState{Version: updateStateVersion}, false, nil
		}
		return upgradeUpdateState{}, false, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return upgradeUpdateState{Version: updateStateVersion}, true, nil
	}
	var state upgradeUpdateState
	if err := json.Unmarshal(raw, &state); err != nil {
		return upgradeUpdateState{}, false, err
	}
	if state.Version == 0 {
		state.Version = updateStateVersion
	}
	return state, true, nil
}

func writeUpgradeUpdateState(path string, state upgradeUpdateState) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return fmt.Errorf("update state path is empty")
	}
	state.Version = updateStateVersion
	state.UpdatedAt = upgradeNow().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmed), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmed, append(data, '\n'), 0o644)
}

func fetchGitHubRelease(ctx context.Context, repo string, targetVersion string) (githubRelease, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return githubRelease{}, fmt.Errorf("release repo is empty")
	}

	path := "/repos/" + repo + "/releases/latest"
	if strings.TrimSpace(targetVersion) != "" {
		path = "/repos/" + repo + "/releases/tags/" + url.PathEscape(strings.TrimSpace(targetVersion))
	}
	endpoint := strings.TrimRight(upgradeGithubAPIBase, "/") + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "longtradego/"+currentBuildVersion())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return githubRelease{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return githubRelease{}, fmt.Errorf("github release api failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return githubRelease{}, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return githubRelease{}, fmt.Errorf("release tag_name is empty")
	}
	if release.Draft {
		return githubRelease{}, fmt.Errorf("release %s is draft and not upgradeable", release.TagName)
	}
	return release, nil
}

func downloadUpgradeContent(ctx context.Context, downloadURL string) ([]byte, error) {
	trimmed := strings.TrimSpace(downloadURL)
	if trimmed == "" {
		return nil, fmt.Errorf("download url is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimmed, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "longtradego/"+currentBuildVersion())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("download failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}

func selectUpgradeAssetsForPlatform(release githubRelease, goos string, goarch string) (githubReleaseAsset, githubReleaseAsset, error) {
	versionNoV := strings.TrimPrefix(normalizeVersionTag(release.TagName), "v")
	expectedName := fmt.Sprintf("longtradego_%s_%s_%s.tar.gz", versionNoV, strings.TrimSpace(goos), strings.TrimSpace(goarch))
	expectedWithV := fmt.Sprintf("longtradego_%s_%s_%s.tar.gz", normalizeVersionTag(release.TagName), strings.TrimSpace(goos), strings.TrimSpace(goarch))

	var binaryAsset githubReleaseAsset
	binaryFound := false
	var checksumAsset githubReleaseAsset
	checksumFound := false

	for _, asset := range release.Assets {
		name := strings.TrimSpace(asset.Name)
		if name == "" {
			continue
		}
		if name == expectedName || name == expectedWithV {
			binaryAsset = asset
			binaryFound = true
		}
		if strings.EqualFold(name, "checksums.txt") {
			checksumAsset = asset
			checksumFound = true
		}
	}
	if !binaryFound {
		for _, asset := range release.Assets {
			name := strings.TrimSpace(asset.Name)
			if strings.HasSuffix(name, fmt.Sprintf("_%s_%s.tar.gz", strings.TrimSpace(goos), strings.TrimSpace(goarch))) && strings.HasPrefix(name, "longtradego_") {
				binaryAsset = asset
				binaryFound = true
				break
			}
		}
	}
	if !binaryFound {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("release %s missing asset for %s/%s", release.TagName, goos, goarch)
	}
	if !checksumFound {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("release %s missing checksums.txt", release.TagName)
	}
	if strings.TrimSpace(binaryAsset.BrowserDownloadURL) == "" {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("release asset %s has empty download url", binaryAsset.Name)
	}
	if strings.TrimSpace(checksumAsset.BrowserDownloadURL) == "" {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("checksums.txt has empty download url")
	}
	return binaryAsset, checksumAsset, nil
}

func checksumForAsset(checksumsContent []byte, assetName string) (string, error) {
	targetName := strings.TrimSpace(assetName)
	if targetName == "" {
		return "", fmt.Errorf("asset name is empty")
	}
	scanner := bufio.NewScanner(bytes.NewReader(checksumsContent))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		checksum := strings.TrimSpace(fields[0])
		fileField := strings.TrimSpace(fields[len(fields)-1])
		fileField = strings.TrimPrefix(fileField, "*")
		if fileField == targetName {
			return strings.ToLower(checksum), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksum not found for asset %s", targetName)
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func extractBinaryFromReleaseArchive(archive []byte, binaryName string) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if header == nil {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if filepath.Base(header.Name) != strings.TrimSpace(binaryName) {
			continue
		}
		payload, readErr := io.ReadAll(tarReader)
		if readErr != nil {
			return nil, readErr
		}
		if len(payload) == 0 {
			return nil, fmt.Errorf("binary %s in archive is empty", binaryName)
		}
		return payload, nil
	}
	return nil, fmt.Errorf("binary %s not found in archive", strings.TrimSpace(binaryName))
}

func replaceExecutableBinary(executablePath string, nextBinary []byte) error {
	trimmedPath := strings.TrimSpace(executablePath)
	if trimmedPath == "" {
		return fmt.Errorf("executable path is empty")
	}
	if len(nextBinary) == 0 {
		return fmt.Errorf("replacement binary is empty")
	}

	dir := filepath.Dir(trimmedPath)
	tmpFile, err := os.CreateTemp(dir, ".longtradego-upgrade-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmpFile.Write(nextBinary); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(0o755); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	backupPath := trimmedPath + ".bak." + strconv.FormatInt(upgradeNow().UnixNano(), 10)
	if err := os.Rename(trimmedPath, backupPath); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, trimmedPath); err != nil {
		rollbackErr := os.Rename(backupPath, trimmedPath)
		if rollbackErr != nil {
			return fmt.Errorf("replace failed: %w (rollback failed: %v)", err, rollbackErr)
		}
		return err
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func looksLikeGoRunExecutable(executablePath string) bool {
	trimmed := strings.TrimSpace(executablePath)
	if trimmed == "" {
		return false
	}
	normalized := filepath.ToSlash(trimmed)
	return strings.Contains(normalized, "/go-build/") && strings.Contains(normalized, "/exe/")
}

func normalizeVersionTag(version string) string {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "v") {
		return "v" + strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	}
	return "v" + trimmed
}

func isReleaseNewerThanCurrent(currentVersion string, releaseVersion string) bool {
	releaseTag := normalizeVersionTag(releaseVersion)
	if releaseTag == "" {
		return false
	}
	currentTag := normalizeVersionTag(currentVersion)
	if currentTag == releaseTag {
		return false
	}
	releaseSemver, releaseValid := parseSemverTag(releaseTag)
	if !releaseValid {
		return false
	}
	currentSemver, currentValid := parseSemverTag(currentTag)
	if !currentValid {
		return true
	}
	return compareParsedSemver(currentSemver, releaseSemver) < 0
}

type parsedSemver struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
}

func parseSemverTag(tag string) (parsedSemver, bool) {
	normalized := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V"))
	if normalized == "" {
		return parsedSemver{}, false
	}
	core := normalized
	prerelease := ""
	if idx := strings.Index(core, "+"); idx >= 0 {
		core = core[:idx]
	}
	if idx := strings.Index(core, "-"); idx >= 0 {
		prerelease = core[idx+1:]
		core = core[:idx]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsedSemver{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return parsedSemver{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return parsedSemver{}, false
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil || patch < 0 {
		return parsedSemver{}, false
	}
	return parsedSemver{Major: major, Minor: minor, Patch: patch, Prerelease: prerelease}, true
}

func compareParsedSemver(left parsedSemver, right parsedSemver) int {
	if left.Major != right.Major {
		if left.Major < right.Major {
			return -1
		}
		return 1
	}
	if left.Minor != right.Minor {
		if left.Minor < right.Minor {
			return -1
		}
		return 1
	}
	if left.Patch != right.Patch {
		if left.Patch < right.Patch {
			return -1
		}
		return 1
	}
	leftPre := strings.TrimSpace(left.Prerelease)
	rightPre := strings.TrimSpace(right.Prerelease)
	if leftPre == rightPre {
		return 0
	}
	if leftPre == "" {
		return 1
	}
	if rightPre == "" {
		return -1
	}
	if leftPre < rightPre {
		return -1
	}
	if leftPre > rightPre {
		return 1
	}
	return 0
}

func parseRFC3339Nano(value string) time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func isInteractiveTerminal() bool {
	inInfo, inErr := os.Stdin.Stat()
	outInfo, outErr := os.Stdout.Stat()
	if inErr != nil || outErr != nil {
		return false
	}
	return (inInfo.Mode()&os.ModeCharDevice) != 0 && (outInfo.Mode()&os.ModeCharDevice) != 0
}

func promptUpgradeConfirmation(in io.Reader, out io.Writer, currentVersion string, targetVersion string) (bool, error) {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	_, _ = fmt.Fprintf(out, "upgrade %s -> %s ? [y/N]: ", currentVersion, targetVersion)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	text := strings.ToLower(strings.TrimSpace(line))
	return text == "y" || text == "yes", nil
}
