package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	id "github.com/emersion/go-imap-id"
	imapclient "github.com/emersion/go-imap/client"
	_ "github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"
	"github.com/spf13/cobra"
)

type imapConfig struct {
	Host               string
	Port               int
	Username           string
	Password           string
	Mailbox            string
	UseTLS             bool
	InsecureSkipVerify bool
	ClientIDName       string
	ClientIDVersion    string
	ClientIDVendor     string
	ClientIDAddress    string
}

type imapReceiveOptions struct {
	Limit           int
	UnreadOnly      bool
	SubjectContains string
	FromContains    string
	MarkSeen        bool
	ConnectTimeout  time.Duration
	ActionCommand   string
	ActionTimeout   time.Duration
	WithBody        bool
	WithFiles       bool
	FilesDir        string
	BodyMaxBytes    int
}

type imapAttachment struct {
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	SavedPath   string `json:"saved_path,omitempty"`
}

type imapMessageSummary struct {
	UID         uint32           `json:"uid"`
	Subject     string           `json:"subject"`
	From        []string         `json:"from,omitempty"`
	Date        string           `json:"date,omitempty"`
	Seen        bool             `json:"seen"`
	MessageID   string           `json:"message_id,omitempty"`
	InReplyTo   string           `json:"in_reply_to,omitempty"`
	BodyText    string           `json:"body_text,omitempty"`
	Attachments []imapAttachment `json:"attachments,omitempty"`
}

type imapReceiveResult struct {
	Provider      string               `json:"provider"`
	Mailbox       string               `json:"mailbox"`
	UnreadOnly    bool                 `json:"unread_only"`
	Limit         int                  `json:"limit"`
	MatchedCount  int                  `json:"matched_count"`
	Messages      []imapMessageSummary `json:"messages"`
	ActionRun     bool                 `json:"action_run,omitempty"`
	ActionCommand string               `json:"action_command,omitempty"`
	ActionResult  *systemCommandResult `json:"action_result,omitempty"`
}

type imapMailSettingsFile struct {
	DefaultAlias string                            `json:"default_alias"`
	Aliases      map[string]imapMailAccountSetting `json:"aliases"`
}

type imapMailAccountSetting struct {
	Host               string `json:"IMAP_HOST"`
	Port               int    `json:"IMAP_PORT"`
	Username           string `json:"IMAP_USERNAME"`
	Password           string `json:"IMAP_PASSWORD"`
	Mailbox            string `json:"IMAP_MAILBOX"`
	TLS                *bool  `json:"IMAP_TLS"`
	InsecureSkipVerify *bool  `json:"IMAP_INSECURE_SKIP_VERIFY"`
	ClientIDName       string `json:"IMAP_ID_NAME"`
	ClientIDVersion    string `json:"IMAP_ID_VERSION"`
	ClientIDVendor     string `json:"IMAP_ID_VENDOR"`
	ClientIDAddress    string `json:"IMAP_ID_ADDRESS"`
}

const (
	monitorModePoll   = "poll"
	monitorModeIdle   = "idle"
	monitorModeHybrid = "hybrid"

	defaultMonitorLimit          = 20
	defaultMonitorConnectTimeout = 10 * time.Second
	defaultMonitorPollInterval   = 15 * time.Second
	defaultMonitorFallbackPoll   = 2 * time.Minute
	defaultMonitorBodyMaxBytes   = 20000
	defaultMonitorMode           = monitorModeHybrid
	defaultMonitorPollStep       = 5 * time.Second
	defaultMonitorPollMax        = 300 * time.Second
	defaultMonitorPollIncreaseN  = 3
	mailMonitorCursorVersion     = 1
	mailMonitorCursorFile        = "mail_monitor_cursors.json"
)

type mailMonitorCursorState struct {
	Version int               `json:"version"`
	Cursors map[string]uint32 `json:"cursors"`
}

type mailMonitorEventHook func(imapReceiveResult) error
type mailMonitorEventHookContextKey struct{}

func withMailMonitorEventHook(ctx context.Context, hook mailMonitorEventHook) context.Context {
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, mailMonitorEventHookContextKey{}, hook)
}

func mailMonitorEventHookFromContext(ctx context.Context) (mailMonitorEventHook, bool) {
	if ctx == nil {
		return nil, false
	}
	value := ctx.Value(mailMonitorEventHookContextKey{})
	hook, ok := value.(mailMonitorEventHook)
	if !ok || hook == nil {
		return nil, false
	}
	return hook, true
}

func newEmailReceiveCommand(app *appContext) *cobra.Command {
	var (
		mailAlias       string
		mailbox         string
		limit           int
		unreadOnly      bool
		subjectContains string
		fromContains    string
		markSeen        bool
		connectTimeout  time.Duration
		actionCommand   string
		actionTimeout   time.Duration
		withBody        bool
		withFiles       bool
		filesDir        string
		bodyMaxBytes    int
	)

	receiveCmd := &cobra.Command{
		Use:     "receive",
		Aliases: []string{"recv", "inbox"},
		Short:   "Query messages from IMAP mailbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit <= 0 {
				return fmt.Errorf("limit must be > 0")
			}
			if connectTimeout <= 0 {
				return fmt.Errorf("connect-timeout must be > 0")
			}
			if actionTimeout < 0 {
				return fmt.Errorf("action-timeout must be >= 0")
			}
			if bodyMaxBytes <= 0 {
				return fmt.Errorf("body-max-bytes must be > 0")
			}
			if withFiles && strings.TrimSpace(filesDir) == "" {
				return fmt.Errorf("files-dir is required when --with-files is enabled")
			}

			cfg, err := loadIMAPConfig(mailAlias)
			if err != nil {
				return err
			}
			if strings.TrimSpace(mailbox) != "" {
				cfg.Mailbox = strings.TrimSpace(mailbox)
			}

			opts := buildIMAPReceiveOptions(
				limit,
				unreadOnly,
				subjectContains,
				fromContains,
				markSeen,
				connectTimeout,
				actionCommand,
				actionTimeout,
				withBody,
				withFiles,
				filesDir,
				bodyMaxBytes,
			)

			app.SetExecution("email", []string{"receive", cfg.Mailbox})

			result, err := receiveIMAPMessages(cmd.Context(), cfg, opts)
			app.SetResult(result)
			if err != nil {
				return err
			}

			if result.MatchedCount > 0 {
				fmt.Printf("Received %d message(s) from %s/%s\n", result.MatchedCount, cfg.Host, cfg.Mailbox)
			} else {
				fmt.Printf("No matching message from %s/%s\n", cfg.Host, cfg.Mailbox)
			}
			return nil
		},
	}

	receiveCmd.Flags().StringVar(&mailAlias, "mail-alias", "", "IMAP alias from conf/mail_receive_setting.json, defaults to default_alias")
	receiveCmd.Flags().StringVar(&mailbox, "mailbox", "", "Mailbox name override, defaults to alias IMAP_MAILBOX or INBOX")
	receiveCmd.Flags().IntVar(&limit, "limit", 20, "Max number of recent messages to inspect")
	receiveCmd.Flags().BoolVar(&unreadOnly, "unread-only", true, "Only inspect unread messages")
	receiveCmd.Flags().StringVar(&subjectContains, "subject-contains", "", "Filter subject containing text (case-insensitive)")
	receiveCmd.Flags().StringVar(&fromContains, "from-contains", "", "Filter sender containing text (case-insensitive)")
	receiveCmd.Flags().BoolVar(&markSeen, "mark-seen", false, "Mark matched messages as seen")
	receiveCmd.Flags().DurationVar(&connectTimeout, "connect-timeout", 10*time.Second, "IMAP connect timeout")
	receiveCmd.Flags().StringVar(&actionCommand, "action-cmd", "", "Optional shell command to run when matched_count > 0")
	receiveCmd.Flags().DurationVar(&actionTimeout, "action-timeout", 0, "Timeout for action command, e.g. 5s")
	receiveCmd.Flags().BoolVar(&withBody, "with-body", false, "Fetch and include message text content")
	receiveCmd.Flags().BoolVar(&withFiles, "with-files", false, "Fetch and save attachment files")
	receiveCmd.Flags().StringVar(&filesDir, "files-dir", filepath.Join(commandLogDir, "mail_files"), "Directory used to save fetched attachment files")
	receiveCmd.Flags().IntVar(&bodyMaxBytes, "body-max-bytes", 20000, "Max bytes stored in body_text for each message")

	return receiveCmd
}

func newEmailMonitorCommand(app *appContext) *cobra.Command {
	var (
		mailAlias       string
		mailbox         string
		limit           int
		unreadOnly      bool
		subjectContains string
		fromContains    string
		connectTimeout  time.Duration
		pollInterval    time.Duration
		waitTimeout     time.Duration
		mode            string
		fallbackPoll    time.Duration
		once            bool
		withBody        bool
		withFiles       bool
		filesDir        string
		bodyMaxBytes    int
		sinceUID        uint32
		monitorID       string
	)

	cmd := &cobra.Command{
		Use:     "monitor",
		Aliases: []string{"watch"},
		Short:   "Monitor IMAP and wait for new messages",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedLimit := resolveMonitorLimit(limit)
			resolvedConnectTimeout := resolveMonitorDuration("IMAP_MONITOR_CONNECT_TIMEOUT", connectTimeout, defaultMonitorConnectTimeout)
			resolvedPollInterval := resolveMonitorDuration("IMAP_MONITOR_POLL_INTERVAL", pollInterval, defaultMonitorPollInterval)
			resolvedFallbackPoll := resolveMonitorDuration("IMAP_MONITOR_FALLBACK_POLL_INTERVAL", fallbackPoll, defaultMonitorFallbackPoll)
			resolvedBodyMaxBytes := resolveMonitorLimitWithKey("IMAP_MONITOR_BODY_MAX_BYTES", bodyMaxBytes, defaultMonitorBodyMaxBytes)
			resolvedMode := resolveMonitorMode(mode)

			if resolvedLimit <= 0 {
				return fmt.Errorf("limit must be > 0")
			}
			if resolvedConnectTimeout <= 0 {
				return fmt.Errorf("connect-timeout must be > 0")
			}
			if resolvedPollInterval <= 0 {
				return fmt.Errorf("poll-interval must be > 0")
			}
			mode = strings.ToLower(strings.TrimSpace(resolvedMode))
			switch mode {
			case monitorModePoll, monitorModeIdle, monitorModeHybrid:
			default:
				return fmt.Errorf("unsupported monitor mode %q, allowed: poll|idle|hybrid", mode)
			}
			if resolvedFallbackPoll <= 0 {
				return fmt.Errorf("fallback-poll-interval must be > 0")
			}
			if waitTimeout < 0 {
				return fmt.Errorf("wait-timeout must be >= 0")
			}
			if resolvedBodyMaxBytes <= 0 {
				return fmt.Errorf("body-max-bytes must be > 0")
			}
			if withFiles && strings.TrimSpace(filesDir) == "" {
				return fmt.Errorf("files-dir is required when --with-files is enabled")
			}

			cfg, err := loadIMAPConfig(mailAlias)
			if err != nil {
				return err
			}
			if strings.TrimSpace(mailbox) != "" {
				cfg.Mailbox = strings.TrimSpace(mailbox)
			}

			idleSupported, idleErr := detectIMAPIDLESupport(cfg, resolvedConnectTimeout)
			if idleErr != nil {
				appendMailMonitorLogf("monitor startup: mode=%s, idle_supported=unknown (%v)", mode, idleErr)
			} else {
				appendMailMonitorLogf("monitor startup: mode=%s, idle_supported=%t", mode, idleSupported)
			}

			opts := buildIMAPReceiveOptions(
				resolvedLimit,
				unreadOnly,
				subjectContains,
				fromContains,
				false,
				resolvedConnectTimeout,
				"",
				0,
				withBody,
				withFiles,
				filesDir,
				resolvedBodyMaxBytes,
			)

			runCtx := cmd.Context()
			var cancel context.CancelFunc
			if waitTimeout > 0 {
				runCtx, cancel = context.WithTimeout(runCtx, waitTimeout)
				defer cancel()
			}

			app.SetExecution("email", []string{"monitor", cfg.Mailbox})
			cursorPath := defaultMailMonitorCursorPath()
			cursorKey := buildMailMonitorCursorKey(cfg, opts, monitorID)
			currentSinceUID := sinceUID
			if currentSinceUID == 0 {
				savedSinceUID, found, cursorErr := loadMailMonitorCursor(cursorPath, cursorKey)
				if cursorErr != nil {
					appendMailMonitorLogf("monitor cursor load failed: %v", cursorErr)
				} else if found {
					currentSinceUID = savedSinceUID
					appendMailMonitorLogf("monitor cursor: loaded since_uid=%d", currentSinceUID)
				}
			}
			totalDetected := 0
			for {
				result, err := monitorIMAPMessages(runCtx, cfg, opts, resolvedPollInterval, currentSinceUID, mode, resolvedFallbackPoll)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						app.SetResult(map[string]any{
							"mode":           mode,
							"mailbox":        cfg.Mailbox,
							"total_detected": totalDetected,
							"stopped":        true,
						})
						return nil
					}
					return err
				}

				app.SetResult(result)
				totalDetected += result.MatchedCount
				appendMailMonitorLogf("Detected %d new message(s) from %s/%s", result.MatchedCount, cfg.Host, cfg.Mailbox)
				latestUID := maxUIDFromMessages(result.Messages)
				if latestUID > currentSinceUID {
					currentSinceUID = latestUID
					if cursorErr := saveMailMonitorCursor(cursorPath, cursorKey, currentSinceUID); cursorErr != nil {
						appendMailMonitorLogf("monitor cursor save failed: %v", cursorErr)
					}
				}
				if hook, ok := mailMonitorEventHookFromContext(runCtx); ok {
					if hookErr := hook(result); hookErr != nil {
						return hookErr
					}
				}
				if once {
					return nil
				}
			}
		},
	}

	cmd.Flags().StringVar(&mailAlias, "mail-alias", "", "IMAP alias from conf/mail_receive_setting.json, defaults to default_alias")
	cmd.Flags().StringVar(&mailbox, "mailbox", "", "Mailbox name override, defaults to alias IMAP_MAILBOX or INBOX")
	cmd.Flags().IntVar(&limit, "limit", 0, "Max number of recent messages to inspect when polling")
	cmd.Flags().BoolVar(&unreadOnly, "unread-only", true, "Only monitor unread messages")
	cmd.Flags().StringVar(&subjectContains, "subject-contains", "", "Filter subject containing text (case-insensitive)")
	cmd.Flags().StringVar(&fromContains, "from-contains", "", "Filter sender containing text (case-insensitive)")
	cmd.Flags().DurationVar(&connectTimeout, "connect-timeout", 0, "IMAP connect timeout")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 0, "Polling interval for new message checks")
	cmd.Flags().StringVar(&mode, "mode", "", "Monitor mode: poll, idle, or hybrid")
	cmd.Flags().DurationVar(&fallbackPoll, "fallback-poll-interval", 0, "Fallback polling interval used in hybrid mode")
	cmd.Flags().BoolVar(&once, "once", false, "Stop after first detected new-message batch")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 0, "Max monitoring duration, e.g. 10m (0 means no timeout)")
	cmd.Flags().BoolVar(&withBody, "with-body", true, "Fetch and include message text content for new emails")
	cmd.Flags().BoolVar(&withFiles, "with-files", false, "Fetch and save attachment files for new emails")
	cmd.Flags().StringVar(&filesDir, "files-dir", filepath.Join(commandLogDir, "mail_files"), "Directory used to save fetched attachment files")
	cmd.Flags().IntVar(&bodyMaxBytes, "body-max-bytes", 0, "Max bytes stored in body_text for each message")
	cmd.Flags().Uint32Var(&sinceUID, "since-uid", 0, "Only notify messages with UID greater than this value")
	cmd.Flags().StringVar(&monitorID, "monitor-id", "", "Internal monitor config id for cursor isolation")
	_ = cmd.Flags().MarkHidden("monitor-id")
	return cmd
}

func newEmailAnalyzeCommand(app *appContext) *cobra.Command {
	var (
		mailAlias       string
		mailbox         string
		limit           int
		unreadOnly      bool
		subjectContains string
		fromContains    string
		connectTimeout  time.Duration
		withFiles       bool
		filesDir        string
		bodyMaxBytes    int
		inputJSON       string
	)

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze email elements and print current fields",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit <= 0 {
				return fmt.Errorf("limit must be > 0")
			}
			if connectTimeout <= 0 {
				return fmt.Errorf("connect-timeout must be > 0")
			}
			if bodyMaxBytes <= 0 {
				return fmt.Errorf("body-max-bytes must be > 0")
			}
			if withFiles && strings.TrimSpace(filesDir) == "" {
				return fmt.Errorf("files-dir is required when --with-files is enabled")
			}

			app.SetExecution("email", []string{"analyze"})

			var (
				result imapReceiveResult
				err    error
			)
			if strings.TrimSpace(inputJSON) != "" {
				result, err = parseAnalyzeInputJSON(inputJSON)
				if err != nil {
					return err
				}
			} else {
				cfg, cfgErr := loadIMAPConfig(mailAlias)
				if cfgErr != nil {
					return cfgErr
				}
				if strings.TrimSpace(mailbox) != "" {
					cfg.Mailbox = strings.TrimSpace(mailbox)
				}
				opts := buildIMAPReceiveOptions(
					limit,
					unreadOnly,
					subjectContains,
					fromContains,
					false,
					connectTimeout,
					"",
					0,
					true,
					withFiles,
					filesDir,
					bodyMaxBytes,
				)
				result, err = receiveIMAPMessages(cmd.Context(), cfg, opts)
				if err != nil {
					return err
				}
			}

			printAnalyzeResultElements(result)
			app.SetResult(result)
			return nil
		},
	}

	cmd.Flags().StringVar(&mailAlias, "mail-alias", "", "IMAP alias from conf/mail_receive_setting.json, defaults to default_alias")
	cmd.Flags().StringVar(&mailbox, "mailbox", "", "Mailbox name override, defaults to alias IMAP_MAILBOX or INBOX")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max number of recent messages to inspect")
	cmd.Flags().BoolVar(&unreadOnly, "unread-only", true, "Only inspect unread messages")
	cmd.Flags().StringVar(&subjectContains, "subject-contains", "", "Filter subject containing text (case-insensitive)")
	cmd.Flags().StringVar(&fromContains, "from-contains", "", "Filter sender containing text (case-insensitive)")
	cmd.Flags().DurationVar(&connectTimeout, "connect-timeout", 10*time.Second, "IMAP connect timeout")
	cmd.Flags().BoolVar(&withFiles, "with-files", false, "Fetch and save attachment files during analysis")
	cmd.Flags().StringVar(&filesDir, "files-dir", filepath.Join(commandLogDir, "mail_files"), "Directory used to save fetched attachment files")
	cmd.Flags().IntVar(&bodyMaxBytes, "body-max-bytes", 20000, "Max bytes stored in body_text for each message")
	cmd.Flags().StringVar(&inputJSON, "input-json", "", "Internal pipeline input JSON payload")
	_ = cmd.Flags().MarkHidden("input-json")
	return cmd
}

func buildIMAPReceiveOptions(
	limit int,
	unreadOnly bool,
	subjectContains string,
	fromContains string,
	markSeen bool,
	connectTimeout time.Duration,
	actionCommand string,
	actionTimeout time.Duration,
	withBody bool,
	withFiles bool,
	filesDir string,
	bodyMaxBytes int,
) imapReceiveOptions {
	return imapReceiveOptions{
		Limit:           limit,
		UnreadOnly:      unreadOnly,
		SubjectContains: strings.TrimSpace(subjectContains),
		FromContains:    strings.TrimSpace(fromContains),
		MarkSeen:        markSeen,
		ConnectTimeout:  connectTimeout,
		ActionCommand:   strings.TrimSpace(actionCommand),
		ActionTimeout:   actionTimeout,
		WithBody:        withBody,
		WithFiles:       withFiles,
		FilesDir:        strings.TrimSpace(filesDir),
		BodyMaxBytes:    bodyMaxBytes,
	}
}

func monitorIMAPMessages(
	ctx context.Context,
	cfg imapConfig,
	opts imapReceiveOptions,
	interval time.Duration,
	sinceUID uint32,
	mode string,
	fallbackPollInterval time.Duration,
) (imapReceiveResult, error) {
	switch mode {
	case monitorModePoll:
		return monitorIMAPMessagesByPolling(ctx, cfg, opts, interval, sinceUID)
	case monitorModeIdle:
		return monitorIMAPMessagesByIdle(ctx, cfg, opts, interval, sinceUID, 0)
	case monitorModeHybrid:
		return monitorIMAPMessagesByIdle(ctx, cfg, opts, interval, sinceUID, fallbackPollInterval)
	default:
		return imapReceiveResult{}, fmt.Errorf("unsupported monitor mode %q", mode)
	}
}

func monitorIMAPMessagesByPolling(ctx context.Context, cfg imapConfig, opts imapReceiveOptions, interval time.Duration, sinceUID uint32) (imapReceiveResult, error) {
	watermark := sinceUID
	if watermark == 0 {
		initialOpts := opts
		initialOpts.WithBody = false
		initialOpts.WithFiles = false
		initialOpts.ActionCommand = ""
		initial, err := receiveIMAPMessages(ctx, cfg, initialOpts)
		if err != nil {
			return imapReceiveResult{}, err
		}
		watermark = maxUIDFromMessages(initial.Messages)
	}

	backoff := newAdaptivePollInterval(interval, defaultMonitorPollStep, defaultMonitorPollMax, defaultMonitorPollIncreaseN)
	timer := time.NewTimer(backoff.Current())
	timerActive := true
	defer stopTaskTimer(timer, &timerActive)

	for {
		select {
		case <-ctx.Done():
			return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
		case <-timer.C:
			timerActive = false
			pollOpts := opts
			pollOpts.WithBody = false
			pollOpts.WithFiles = false
			pollOpts.ActionCommand = ""
			polled, err := receiveIMAPMessages(ctx, cfg, pollOpts)
			if err != nil {
				return imapReceiveResult{}, err
			}

			newUIDs := collectNewUIDs(polled.Messages, watermark)
			if len(newUIDs) == 0 {
				latest := maxUIDFromMessages(polled.Messages)
				if latest > watermark {
					watermark = latest
				}
				resetTaskTimer(timer, &timerActive, backoff.OnNoNew())
				continue
			}

			filtered, err := fetchMessagesByUIDs(ctx, cfg, opts, newUIDs)
			if err != nil {
				return imapReceiveResult{}, err
			}
			backoff.OnDetected()
			latest := maxUIDFromMessages(filtered.Messages)
			if latest > watermark {
				watermark = latest
			}
			return filtered, nil
		}
	}
}

func monitorIMAPMessagesByIdle(
	ctx context.Context,
	cfg imapConfig,
	opts imapReceiveOptions,
	idlePollInterval time.Duration,
	sinceUID uint32,
	fallbackPollInterval time.Duration,
) (imapReceiveResult, error) {
	watermark := sinceUID
	if watermark == 0 {
		initialOpts := opts
		initialOpts.WithBody = false
		initialOpts.WithFiles = false
		initialOpts.ActionCommand = ""
		initial, err := receiveIMAPMessages(ctx, cfg, initialOpts)
		if err != nil {
			return imapReceiveResult{}, err
		}
		watermark = maxUIDFromMessages(initial.Messages)
	}
	for {
		c, err := dialIMAP(cfg, opts.ConnectTimeout)
		if err != nil {
			if !waitMonitorRetry(ctx, 2*time.Second) {
				return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
			}
			continue
		}

		if err := c.Login(cfg.Username, cfg.Password); err != nil {
			_ = c.Logout()
			if !waitMonitorRetry(ctx, 2*time.Second) {
				return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
			}
			continue
		}
		if err := sendIMAPClientID(c, cfg); err != nil {
			_ = c.Logout()
			if !waitMonitorRetry(ctx, 2*time.Second) {
				return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
			}
			continue
		}
		if _, err := c.Select(cfg.Mailbox, false); err != nil {
			_ = c.Logout()
			if !waitMonitorRetry(ctx, 2*time.Second) {
				return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
			}
			continue
		}

		updates := make(chan imapclient.Update, 64)
		c.Updates = updates

		var fallbackBackoff *adaptivePollInterval
		var fallbackTimer *time.Timer
		fallbackTimerActive := false
		if fallbackPollInterval > 0 {
			fallbackBackoff = newAdaptivePollInterval(fallbackPollInterval, defaultMonitorPollStep, defaultMonitorPollMax, defaultMonitorPollIncreaseN)
			fallbackTimer = time.NewTimer(fallbackBackoff.Current())
			fallbackTimerActive = true
		}

		needReconnect := false
		usedFallbackTick := false
		for {
			stop := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- c.Idle(stop, &imapclient.IdleOptions{
					LogoutTimeout: 25 * time.Minute,
					PollInterval:  idlePollInterval,
				})
			}()

			needCheck := false
			idleFinished := false
			select {
			case <-ctx.Done():
				close(stop)
				_ = <-done
				stopTaskTimerIfNeeded(fallbackTimer, &fallbackTimerActive)
				_ = c.Logout()
				return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
			case idleErr := <-done:
				idleFinished = true
				if idleErr != nil {
					if shouldRetryMonitorIdleError(idleErr) {
						needReconnect = true
						break
					}
					stopTaskTimerIfNeeded(fallbackTimer, &fallbackTimerActive)
					_ = c.Logout()
					return imapReceiveResult{}, fmt.Errorf("mail idle failed: %w", idleErr)
				}
				needCheck = true
			case <-updates:
				needCheck = true
			case <-fallbackTimerChannel(fallbackTimer):
				fallbackTimerActive = false
				usedFallbackTick = true
				needCheck = true
			}

			close(stop)
			if !idleFinished {
				if idleErr := <-done; idleErr != nil {
					if shouldRetryMonitorIdleError(idleErr) {
						needReconnect = true
						break
					}
					stopTaskTimerIfNeeded(fallbackTimer, &fallbackTimerActive)
					_ = c.Logout()
					return imapReceiveResult{}, fmt.Errorf("mail idle stopped with error: %w", idleErr)
				}
			}
			drainIMAPUpdates(updates)

			if needReconnect {
				break
			}
			if !needCheck {
				continue
			}

			pollOpts := opts
			pollOpts.WithBody = false
			pollOpts.WithFiles = false
			pollOpts.ActionCommand = ""
			polled, err := receiveIMAPMessages(ctx, cfg, pollOpts)
			if err != nil {
				if shouldRetryMonitorIdleError(err) {
					needReconnect = true
					break
				}
				stopTaskTimerIfNeeded(fallbackTimer, &fallbackTimerActive)
				_ = c.Logout()
				return imapReceiveResult{}, err
			}
			newUIDs := collectNewUIDs(polled.Messages, watermark)
			if len(newUIDs) == 0 {
				latest := maxUIDFromMessages(polled.Messages)
				if latest > watermark {
					watermark = latest
				}
				if usedFallbackTick && fallbackBackoff != nil {
					resetTaskTimer(fallbackTimer, &fallbackTimerActive, fallbackBackoff.OnNoNew())
				}
				usedFallbackTick = false
				continue
			}
			filtered, err := fetchMessagesByUIDs(ctx, cfg, opts, newUIDs)
			stopTaskTimerIfNeeded(fallbackTimer, &fallbackTimerActive)
			_ = c.Logout()
			if err != nil {
				return imapReceiveResult{}, err
			}
			if fallbackBackoff != nil {
				fallbackBackoff.OnDetected()
			}
			latest := maxUIDFromMessages(filtered.Messages)
			if latest > watermark {
				watermark = latest
			}
			return filtered, nil
		}

		stopTaskTimerIfNeeded(fallbackTimer, &fallbackTimerActive)
		_ = c.Logout()
		if !waitMonitorRetry(ctx, 2*time.Second) {
			return imapReceiveResult{}, fmt.Errorf("mail monitor stopped: %w", ctx.Err())
		}
	}
}

func fallbackTimerChannel(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func stopTaskTimerIfNeeded(timer *time.Timer, active *bool) {
	if timer == nil || active == nil {
		return
	}
	stopTaskTimer(timer, active)
}

func drainIMAPUpdates(updates <-chan imapclient.Update) {
	for {
		select {
		case <-updates:
		default:
			return
		}
	}
}

func shouldRetryMonitorIdleError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "disconnected while idling") {
		return true
	}
	if strings.Contains(lower, "connection reset by peer") {
		return true
	}
	if strings.Contains(lower, "broken pipe") {
		return true
	}
	if strings.Contains(lower, "use of closed network connection") {
		return true
	}
	return false
}

func waitMonitorRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type adaptivePollInterval struct {
	base           time.Duration
	step           time.Duration
	max            time.Duration
	increaseEveryN int
	current        time.Duration
	emptyCount     int
}

func newAdaptivePollInterval(base time.Duration, step time.Duration, max time.Duration, increaseEveryN int) *adaptivePollInterval {
	if base <= 0 {
		base = defaultMonitorPollInterval
	}
	if step <= 0 {
		step = defaultMonitorPollStep
	}
	if max <= 0 {
		max = defaultMonitorPollMax
	}
	if increaseEveryN <= 0 {
		increaseEveryN = defaultMonitorPollIncreaseN
	}
	if base > max {
		base = max
	}
	return &adaptivePollInterval{
		base:           base,
		step:           step,
		max:            max,
		increaseEveryN: increaseEveryN,
		current:        base,
	}
}

func (a *adaptivePollInterval) Current() time.Duration {
	return a.current
}

func (a *adaptivePollInterval) OnNoNew() time.Duration {
	a.emptyCount++
	if a.emptyCount%a.increaseEveryN == 0 && a.current < a.max {
		next := a.current + a.step
		if next > a.max {
			next = a.max
		}
		a.current = next
	}
	return a.current
}

func (a *adaptivePollInterval) OnDetected() time.Duration {
	a.emptyCount = 0
	a.current = a.base
	return a.current
}

func collectNewUIDs(messages []imapMessageSummary, watermark uint32) []uint32 {
	uids := make([]uint32, 0, len(messages))
	for _, message := range messages {
		if message.UID > watermark {
			uids = append(uids, message.UID)
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids
}

func maxUIDFromMessages(messages []imapMessageSummary) uint32 {
	var maxUID uint32
	for _, message := range messages {
		if message.UID > maxUID {
			maxUID = message.UID
		}
	}
	return maxUID
}

func fetchMessagesByUIDs(ctx context.Context, cfg imapConfig, opts imapReceiveOptions, uids []uint32) (imapReceiveResult, error) {
	result := imapReceiveResult{
		Provider:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Mailbox:    cfg.Mailbox,
		UnreadOnly: opts.UnreadOnly,
		Limit:      len(uids),
		Messages:   []imapMessageSummary{},
	}
	if len(uids) == 0 {
		return result, nil
	}

	c, err := dialIMAP(cfg, opts.ConnectTimeout)
	if err != nil {
		return result, err
	}
	defer c.Logout()

	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		return result, fmt.Errorf("imap login failed: %w", err)
	}
	if err := sendIMAPClientID(c, cfg); err != nil {
		return result, err
	}
	if _, err := c.Select(cfg.Mailbox, false); err != nil {
		return result, fmt.Errorf("imap select mailbox failed: %w", err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, imap.FetchUid}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, items, messages)
	}()

	collected := make([]imapMessageSummary, 0, len(uids))
	for msg := range messages {
		if msg == nil {
			continue
		}
		summary := imapMessageSummary{
			UID:       msg.Uid,
			From:      envelopeAddressList(msg.Envelope),
			Seen:      hasIMAPFlag(msg.Flags, imap.SeenFlag),
			MessageID: "",
		}
		if msg.Envelope != nil {
			summary.Subject = msg.Envelope.Subject
			summary.MessageID = strings.TrimSpace(msg.Envelope.MessageId)
			summary.InReplyTo = strings.TrimSpace(msg.Envelope.InReplyTo)
		}
		if !msg.InternalDate.IsZero() {
			summary.Date = msg.InternalDate.Format(time.RFC3339)
		}
		collected = append(collected, summary)
	}
	if fetchErr := <-done; fetchErr != nil {
		return result, fmt.Errorf("imap fetch failed: %w", fetchErr)
	}

	sort.Slice(collected, func(i, j int) bool { return collected[i].UID > collected[j].UID })
	if opts.WithBody || opts.WithFiles {
		enriched, enrichErr := enrichIMAPMessages(c, collected, opts)
		if enrichErr != nil {
			return result, enrichErr
		}
		collected = enriched
	}

	result.Messages = collected
	result.MatchedCount = len(collected)
	return result, nil
}

func parseAnalyzeInputJSON(raw string) (imapReceiveResult, error) {
	var wrapper struct {
		Messages []imapMessageSummary `json:"messages"`
		Result   imapReceiveResult    `json:"result"`
	}
	var direct imapReceiveResult
	if err := json.Unmarshal([]byte(raw), &direct); err == nil && len(direct.Messages) > 0 {
		if direct.MatchedCount == 0 {
			direct.MatchedCount = len(direct.Messages)
		}
		return direct, nil
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return imapReceiveResult{}, fmt.Errorf("parse input-json failed: %w", err)
	}
	if len(wrapper.Result.Messages) > 0 {
		if wrapper.Result.MatchedCount == 0 {
			wrapper.Result.MatchedCount = len(wrapper.Result.Messages)
		}
		return wrapper.Result, nil
	}
	if len(wrapper.Messages) > 0 {
		return imapReceiveResult{
			Messages:     wrapper.Messages,
			MatchedCount: len(wrapper.Messages),
		}, nil
	}
	return imapReceiveResult{}, fmt.Errorf("input-json does not contain analyzable messages")
}

func printAnalyzeResultElements(result imapReceiveResult) {
	fmt.Printf("analyze: mailbox=%s matched=%d\n", result.Mailbox, result.MatchedCount)
	for index, message := range result.Messages {
		fmt.Printf("message[%d].uid=%d\n", index, message.UID)
		fmt.Printf("message[%d].subject=%s\n", index, message.Subject)
		fmt.Printf("message[%d].date=%s\n", index, message.Date)
		fmt.Printf("message[%d].seen=%t\n", index, message.Seen)
		for fromIndex, from := range message.From {
			fmt.Printf("message[%d].from[%d]=%s\n", index, fromIndex, from)
		}
		if strings.TrimSpace(message.BodyText) != "" {
			fmt.Printf("message[%d].body_text=%s\n", index, message.BodyText)
		}
		for attachIndex, attachment := range message.Attachments {
			fmt.Printf("message[%d].attachments[%d].file_name=%s\n", index, attachIndex, attachment.FileName)
			fmt.Printf("message[%d].attachments[%d].size=%d\n", index, attachIndex, attachment.Size)
			if strings.TrimSpace(attachment.ContentType) != "" {
				fmt.Printf("message[%d].attachments[%d].content_type=%s\n", index, attachIndex, attachment.ContentType)
			}
			if strings.TrimSpace(attachment.SavedPath) != "" {
				fmt.Printf("message[%d].attachments[%d].saved_path=%s\n", index, attachIndex, attachment.SavedPath)
			}
		}
	}
}

func defaultIMAPSettingsConfigPath() string {
	return filepath.Join("conf", "mail_receive_setting.json")
}

func loadIMAPConfig(mailAlias string) (imapConfig, error) {
	return loadIMAPConfigFromSettings(defaultIMAPSettingsConfigPath(), mailAlias)
}

func loadIMAPConfigFromSettings(path string, mailAlias string) (imapConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return imapConfig{}, err
	}

	var settings imapMailSettingsFile
	if err := json.Unmarshal(raw, &settings); err != nil {
		return imapConfig{}, fmt.Errorf("parse imap settings %s: %w", path, err)
	}

	if len(settings.Aliases) == 0 {
		return imapConfig{}, fmt.Errorf("imap settings %s has no aliases", path)
	}

	normalizedAliases := make(map[string]imapMailAccountSetting, len(settings.Aliases))
	for rawAlias, account := range settings.Aliases {
		alias := strings.ToLower(strings.TrimSpace(rawAlias))
		if alias == "" {
			continue
		}
		normalizedAliases[alias] = account
	}
	if len(normalizedAliases) == 0 {
		return imapConfig{}, fmt.Errorf("imap settings %s has no valid aliases", path)
	}

	selectedAlias := strings.ToLower(strings.TrimSpace(mailAlias))
	if selectedAlias == "" {
		selectedAlias = strings.ToLower(strings.TrimSpace(settings.DefaultAlias))
	}
	if selectedAlias == "" && len(normalizedAliases) == 1 {
		for alias := range normalizedAliases {
			selectedAlias = alias
		}
	}
	if selectedAlias == "" {
		return imapConfig{}, fmt.Errorf("missing IMAP alias: set --mail-alias or default_alias in %s", path)
	}

	account, ok := normalizedAliases[selectedAlias]
	if !ok {
		return imapConfig{}, fmt.Errorf("imap alias %q not found in %s", selectedAlias, path)
	}

	host := strings.TrimSpace(account.Host)
	username := strings.TrimSpace(account.Username)
	password := strings.TrimSpace(account.Password)
	mailbox := strings.TrimSpace(account.Mailbox)
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if host == "" {
		return imapConfig{}, fmt.Errorf("imap alias %q missing host in %s", selectedAlias, path)
	}
	if username == "" {
		return imapConfig{}, fmt.Errorf("imap alias %q missing username in %s", selectedAlias, path)
	}
	if password == "" {
		return imapConfig{}, fmt.Errorf("imap alias %q missing password in %s", selectedAlias, path)
	}

	port := account.Port
	if port <= 0 {
		port = 993
	}

	useTLS := true
	if account.TLS != nil {
		useTLS = *account.TLS
	}

	insecure := false
	if account.InsecureSkipVerify != nil {
		insecure = *account.InsecureSkipVerify
	}

	return imapConfig{
		Host:               host,
		Port:               port,
		Username:           username,
		Password:           password,
		Mailbox:            mailbox,
		UseTLS:             useTLS,
		InsecureSkipVerify: insecure,
		ClientIDName:       firstNonEmptyTrimmed(account.ClientIDName, "longtradego"),
		ClientIDVersion:    firstNonEmptyTrimmed(account.ClientIDVersion, "1.0.0"),
		ClientIDVendor:     firstNonEmptyTrimmed(account.ClientIDVendor, "longtradego"),
		ClientIDAddress:    firstNonEmptyTrimmed(account.ClientIDAddress, username),
	}, nil
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func resolveMonitorMode(mode string) string {
	trimmed := strings.ToLower(strings.TrimSpace(mode))
	if trimmed != "" {
		return trimmed
	}
	envMode := strings.ToLower(strings.TrimSpace(os.Getenv("IMAP_MONITOR_MODE")))
	if envMode != "" {
		return envMode
	}
	return defaultMonitorMode
}

func resolveMonitorDuration(envKey string, flagValue time.Duration, fallback time.Duration) time.Duration {
	if flagValue > 0 {
		return flagValue
	}
	if envRaw := strings.TrimSpace(os.Getenv(envKey)); envRaw != "" {
		if parsed, err := time.ParseDuration(envRaw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func resolveMonitorLimit(flagValue int) int {
	return resolveMonitorLimitWithKey("IMAP_MONITOR_LIMIT", flagValue, defaultMonitorLimit)
}

func resolveMonitorLimitWithKey(envKey string, flagValue int, fallback int) int {
	if flagValue > 0 {
		return flagValue
	}
	if envRaw := strings.TrimSpace(os.Getenv(envKey)); envRaw != "" {
		if parsed, err := strconv.Atoi(envRaw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func defaultMailMonitorCursorPath() string {
	return filepath.Join(daemonDataDir, mailMonitorCursorFile)
}

func buildMailMonitorCursorKey(cfg imapConfig, opts imapReceiveOptions, monitorID string) string {
	parts := []string{
		"host=" + strings.TrimSpace(cfg.Host),
		"port=" + strconv.Itoa(cfg.Port),
		"user=" + strings.TrimSpace(cfg.Username),
		"mailbox=" + strings.TrimSpace(cfg.Mailbox),
		"unread=" + strconv.FormatBool(opts.UnreadOnly),
		"subject=" + strings.TrimSpace(opts.SubjectContains),
		"from=" + strings.TrimSpace(opts.FromContains),
	}
	if trimmedID := strings.TrimSpace(monitorID); trimmedID != "" {
		parts = append(parts, "monitor_id="+trimmedID)
	}
	return strings.ToLower(strings.Join(parts, "|"))
}

func loadMailMonitorCursor(path string, key string) (uint32, bool, error) {
	state, err := readMailMonitorCursorState(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if state.Cursors == nil {
		return 0, false, nil
	}
	value, ok := state.Cursors[key]
	if !ok {
		return 0, false, nil
	}
	return value, true, nil
}

func saveMailMonitorCursor(path string, key string, sinceUID uint32) error {
	state, err := readMailMonitorCursorState(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.Version == 0 {
		state.Version = mailMonitorCursorVersion
	}
	if state.Cursors == nil {
		state.Cursors = make(map[string]uint32)
	}
	state.Cursors[key] = sinceUID
	return writeMailMonitorCursorState(path, state)
}

func readMailMonitorCursorState(path string) (mailMonitorCursorState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return mailMonitorCursorState{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return mailMonitorCursorState{}, nil
	}
	var state mailMonitorCursorState
	if err := json.Unmarshal(raw, &state); err != nil {
		return mailMonitorCursorState{}, err
	}
	return state, nil
}

func writeMailMonitorCursorState(path string, state mailMonitorCursorState) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("cursor path is empty")
	}
	if state.Version == 0 {
		state.Version = mailMonitorCursorVersion
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func appendMailMonitorLogf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	path := filepath.Join(commandLogDir, "mail_monitor.log")
	line := []byte(fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), message))
	_ = appendLogLineWithRotation(path, line, commandLogMaxSize, "mail_monitor")
}

func receiveIMAPMessages(ctx context.Context, cfg imapConfig, opts imapReceiveOptions) (imapReceiveResult, error) {
	result := imapReceiveResult{
		Provider:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Mailbox:    cfg.Mailbox,
		UnreadOnly: opts.UnreadOnly,
		Limit:      opts.Limit,
		Messages:   []imapMessageSummary{},
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	c, err := dialIMAP(cfg, opts.ConnectTimeout)
	if err != nil {
		return result, err
	}
	defer c.Logout()

	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		return result, fmt.Errorf("imap login failed: %w", err)
	}
	if err := sendIMAPClientID(c, cfg); err != nil {
		return result, err
	}
	if _, err := c.Select(cfg.Mailbox, false); err != nil {
		return result, fmt.Errorf("imap select mailbox failed: %w", err)
	}

	criteria := imap.NewSearchCriteria()
	if opts.UnreadOnly {
		criteria.WithoutFlags = []string{imap.SeenFlag}
	}
	if opts.SubjectContains != "" {
		if criteria.Header == nil {
			criteria.Header = textproto.MIMEHeader{}
		}
		criteria.Header.Add("Subject", opts.SubjectContains)
	}
	if opts.FromContains != "" {
		if criteria.Header == nil {
			criteria.Header = textproto.MIMEHeader{}
		}
		criteria.Header.Add("From", opts.FromContains)
	}

	uids, err := c.UidSearch(criteria)
	if err != nil {
		return result, fmt.Errorf("imap search failed: %w", err)
	}
	if len(uids) == 0 {
		return result, nil
	}

	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > opts.Limit {
		uids = uids[:opts.Limit]
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)

	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, imap.FetchUid}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, items, messages)
	}()

	collected := make([]imapMessageSummary, 0, len(uids))
	for msg := range messages {
		if msg == nil {
			continue
		}
		summary := imapMessageSummary{
			UID:       msg.Uid,
			Subject:   "",
			From:      envelopeAddressList(msg.Envelope),
			Seen:      hasIMAPFlag(msg.Flags, imap.SeenFlag),
			MessageID: "",
		}
		if msg.Envelope != nil {
			summary.Subject = msg.Envelope.Subject
			summary.MessageID = strings.TrimSpace(msg.Envelope.MessageId)
			summary.InReplyTo = strings.TrimSpace(msg.Envelope.InReplyTo)
		}
		if !msg.InternalDate.IsZero() {
			summary.Date = msg.InternalDate.Format(time.RFC3339)
		}
		collected = append(collected, summary)
	}

	if fetchErr := <-done; fetchErr != nil {
		return result, fmt.Errorf("imap fetch failed: %w", fetchErr)
	}

	sort.Slice(collected, func(i, j int) bool { return collected[i].UID > collected[j].UID })
	if opts.WithBody || opts.WithFiles {
		enriched, enrichErr := enrichIMAPMessages(c, collected, opts)
		if enrichErr != nil {
			return result, enrichErr
		}
		collected = enriched
	}

	result.Messages = collected
	result.MatchedCount = len(collected)

	if opts.MarkSeen && len(uids) > 0 {
		if err := markIMAPMessagesSeen(c, uids); err != nil {
			return result, err
		}
	}

	if strings.TrimSpace(opts.ActionCommand) != "" && result.MatchedCount > 0 {
		actionCommand := expandIMAPActionCommand(opts.ActionCommand, result)
		actionCtx := ctx
		if opts.ActionTimeout > 0 {
			var cancel context.CancelFunc
			actionCtx, cancel = context.WithTimeout(ctx, opts.ActionTimeout)
			defer cancel()
		}

		actionResult, actionErr := runSystemCommand(actionCtx, nil, actionCommand)
		result.ActionRun = true
		result.ActionCommand = actionCommand
		result.ActionResult = &actionResult
		if actionErr != nil {
			return result, fmt.Errorf("action command failed: %w", actionErr)
		}
	}

	return result, nil
}

func enrichIMAPMessages(client *imapclient.Client, summaries []imapMessageSummary, opts imapReceiveOptions) ([]imapMessageSummary, error) {
	if len(summaries) == 0 {
		return summaries, nil
	}
	if opts.WithFiles {
		if err := os.MkdirAll(opts.FilesDir, 0o755); err != nil {
			return nil, fmt.Errorf("create files dir %s: %w", opts.FilesDir, err)
		}
	}

	for index := range summaries {
		raw, err := fetchRawMessageByUID(client, summaries[index].UID)
		if err != nil {
			return nil, err
		}
		enrichSummaryFromRawHeader(raw, &summaries[index])
		bodyText, attachments, err := extractMailContentAndFiles(raw, summaries[index].UID, opts)
		if err != nil {
			return nil, err
		}
		if opts.WithBody {
			summaries[index].BodyText = bodyText
		}
		if opts.WithFiles {
			summaries[index].Attachments = attachments
		}
	}

	return summaries, nil
}

func enrichSummaryFromRawHeader(raw []byte, summary *imapMessageSummary) {
	if summary == nil || len(raw) == 0 {
		return
	}
	reader, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && reader == nil {
		return
	}
	if reader == nil {
		return
	}
	defer reader.Close()

	if subject, _ := reader.Header.Subject(); strings.TrimSpace(subject) != "" {
		summary.Subject = strings.TrimSpace(subject)
	}

	if fromAddrs, fromErr := reader.Header.AddressList("From"); fromErr == nil {
		formatted := formatMailAddressList(fromAddrs)
		if len(formatted) > 0 {
			summary.From = formatted
		}
	}

	if messageID, messageIDErr := reader.Header.MessageID(); messageIDErr == nil {
		formattedID := formatMessageID(strings.TrimSpace(messageID))
		if formattedID != "" {
			summary.MessageID = formattedID
		}
	}

	if inReplyToIDs, inReplyErr := reader.Header.MsgIDList("In-Reply-To"); inReplyErr == nil && len(inReplyToIDs) > 0 {
		formattedReply := formatMessageID(strings.TrimSpace(inReplyToIDs[0]))
		if formattedReply != "" {
			summary.InReplyTo = formattedReply
		}
	}
}

func fetchRawMessageByUID(client *imapclient.Client, uid uint32) ([]byte, error) {
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, section.FetchItem()}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- client.UidFetch(seqSet, items, messages)
	}()

	var (
		raw     []byte
		readErr error
	)

	for msg := range messages {
		if msg == nil {
			continue
		}
		body := msg.GetBody(section)
		if body == nil {
			continue
		}
		raw, readErr = io.ReadAll(body)
		if readErr != nil {
			break
		}
	}

	if fetchErr := <-done; fetchErr != nil {
		return nil, fmt.Errorf("imap raw fetch failed for uid %d: %w", uid, fetchErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("read raw message failed for uid %d: %w", uid, readErr)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("raw message body is empty for uid %d", uid)
	}
	return raw, nil
}

func extractMailContentAndFiles(raw []byte, uid uint32, opts imapReceiveOptions) (string, []imapAttachment, error) {
	reader, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("parse raw message failed for uid %d: %w", uid, err)
	}
	defer reader.Close()

	plainBuilder := strings.Builder{}
	htmlBuilder := strings.Builder{}
	attachments := make([]imapAttachment, 0, 2)
	remainingBodyBytes := opts.BodyMaxBytes
	attachmentIndex := 0

	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return "", nil, fmt.Errorf("read message part failed for uid %d: %w", uid, nextErr)
		}

		switch header := part.Header.(type) {
		case *gomail.InlineHeader:
			if !opts.WithBody {
				if _, err := io.Copy(io.Discard, part.Body); err != nil {
					return "", nil, fmt.Errorf("discard inline part failed for uid %d: %w", uid, err)
				}
				continue
			}
			contentType, _, _ := header.ContentType()
			data, err := io.ReadAll(part.Body)
			if err != nil {
				return "", nil, fmt.Errorf("read inline part failed for uid %d: %w", uid, err)
			}
			text := strings.TrimSpace(string(data))
			if text == "" || remainingBodyBytes <= 0 {
				continue
			}
			trimmed := truncateByBytes(text, remainingBodyBytes)
			if trimmed == "" {
				continue
			}
			if plainBuilder.Len() > 0 || htmlBuilder.Len() > 0 {
				trimmed = "\n\n" + trimmed
			}
			if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
				htmlBuilder.WriteString(trimmed)
			} else {
				plainBuilder.WriteString(trimmed)
			}
			remainingBodyBytes -= len(trimmed)
		case *gomail.AttachmentHeader:
			if !opts.WithFiles {
				if _, err := io.Copy(io.Discard, part.Body); err != nil {
					return "", nil, fmt.Errorf("discard attachment part failed for uid %d: %w", uid, err)
				}
				continue
			}
			filename, _ := header.Filename()
			contentType, _, _ := header.ContentType()
			content, err := io.ReadAll(part.Body)
			if err != nil {
				return "", nil, fmt.Errorf("read attachment failed for uid %d: %w", uid, err)
			}
			attachmentIndex++
			savedPath, saveErr := saveAttachmentFile(opts.FilesDir, uid, attachmentIndex, filename, content)
			if saveErr != nil {
				return "", nil, saveErr
			}
			attachments = append(attachments, imapAttachment{
				FileName:    filepath.Base(savedPath),
				ContentType: contentType,
				Size:        int64(len(content)),
				SavedPath:   savedPath,
			})
		default:
			if _, err := io.Copy(io.Discard, part.Body); err != nil {
				return "", nil, fmt.Errorf("discard unknown part failed for uid %d: %w", uid, err)
			}
		}
	}

	if opts.WithBody {
		if plainBuilder.Len() > 0 {
			return plainBuilder.String(), attachments, nil
		}
		if htmlBuilder.Len() > 0 {
			return htmlBuilder.String(), attachments, nil
		}
	}
	return "", attachments, nil
}

func truncateByBytes(input string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(input) <= limit {
		return input
	}
	if limit <= 3 {
		return input[:limit]
	}
	return input[:limit-3] + "..."
}

func saveAttachmentFile(baseDir string, uid uint32, attachmentIndex int, fileName string, content []byte) (string, error) {
	uidDir := filepath.Join(baseDir, fmt.Sprintf("uid-%d", uid))
	if err := os.MkdirAll(uidDir, 0o755); err != nil {
		return "", fmt.Errorf("create attachment dir %s: %w", uidDir, err)
	}

	safeName := sanitizeAttachmentFileName(fileName)
	if safeName == "" {
		safeName = fmt.Sprintf("attachment-%d.bin", attachmentIndex)
	}
	path := filepath.Join(uidDir, safeName)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", fmt.Errorf("save attachment file %s: %w", path, err)
	}
	return path, nil
}

func sanitizeAttachmentFileName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	cleaned := replacer.Replace(trimmed)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return ""
	}
	return cleaned
}

func dialIMAP(cfg imapConfig, timeout time.Duration) (*imapclient.Client, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: timeout}

	if cfg.UseTLS {
		client, err := imapclient.DialWithDialerTLS(dialer, addr, &tls.Config{
			ServerName:         cfg.Host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		})
		if err != nil {
			return nil, fmt.Errorf("imap tls connect failed: %w", err)
		}
		return client, nil
	}

	client, err := imapclient.DialWithDialer(dialer, addr)
	if err != nil {
		return nil, fmt.Errorf("imap connect failed: %w", err)
	}
	return client, nil
}

func hasIMAPFlag(flags []string, target string) bool {
	for _, flag := range flags {
		if strings.EqualFold(strings.TrimSpace(flag), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func envelopeAddressList(envelope *imap.Envelope) []string {
	if envelope == nil || len(envelope.From) == 0 {
		return nil
	}

	addresses := make([]string, 0, len(envelope.From))
	for _, addr := range envelope.From {
		formatted := formatEnvelopeAddress(addr)
		if formatted != "" {
			addresses = append(addresses, formatted)
		}
	}
	if len(addresses) == 0 {
		return nil
	}
	return addresses
}

func formatEnvelopeAddress(addr *imap.Address) string {
	if addr == nil {
		return ""
	}
	mailbox := strings.TrimSpace(addr.MailboxName)
	host := strings.TrimSpace(addr.HostName)
	address := strings.TrimSpace(mailbox + "@" + host)
	if mailbox == "" || host == "" {
		address = strings.TrimSpace(mailbox + host)
	}
	personal := strings.TrimSpace(addr.PersonalName)
	if personal != "" && address != "" {
		return personal + " <" + address + ">"
	}
	if personal != "" {
		return personal
	}
	return address
}

func formatMailAddressList(addresses []*gomail.Address) []string {
	if len(addresses) == 0 {
		return nil
	}
	out := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		if addr == nil {
			continue
		}
		parsedAddress := strings.TrimSpace(addr.Address)
		parsedName := strings.TrimSpace(addr.Name)
		if parsedName != "" && parsedAddress != "" {
			out = append(out, parsedName+" <"+parsedAddress+">")
			continue
		}
		if parsedAddress != "" {
			out = append(out, parsedAddress)
			continue
		}
		if parsedName != "" {
			out = append(out, parsedName)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func formatMessageID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">") {
		return trimmed
	}
	return "<" + trimmed + ">"
}

func markIMAPMessagesSeen(client *imapclient.Client, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.SeenFlag}
	if err := client.UidStore(seqSet, item, flags, nil); err != nil {
		return fmt.Errorf("mark messages as seen failed: %w", err)
	}
	return nil
}

func expandIMAPActionCommand(command string, result imapReceiveResult) string {
	replaced := strings.ReplaceAll(command, "${MAIL_COUNT}", strconv.Itoa(result.MatchedCount))
	replaced = strings.ReplaceAll(replaced, "${MAILBOX}", result.Mailbox)
	replaced = strings.ReplaceAll(replaced, "${IMAP_PROVIDER}", result.Provider)
	return replaced
}

func sendIMAPClientID(client *imapclient.Client, cfg imapConfig) error {
	idClient := id.NewClient(client)
	supported, err := idClient.SupportID()
	if err != nil {
		return fmt.Errorf("imap ID capability check failed: %w", err)
	}
	if !supported {
		return nil
	}

	clientID := id.ID{
		id.FieldName:    cfg.ClientIDName,
		id.FieldVersion: cfg.ClientIDVersion,
		id.FieldVendor:  cfg.ClientIDVendor,
		id.FieldAddress: cfg.ClientIDAddress,
	}
	if _, err := idClient.ID(clientID); err != nil {
		return fmt.Errorf("imap ID command failed: %w", err)
	}
	return nil
}

func detectIMAPIDLESupport(cfg imapConfig, connectTimeout time.Duration) (bool, error) {
	c, err := dialIMAP(cfg, connectTimeout)
	if err != nil {
		return false, err
	}
	defer c.Logout()

	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		return false, fmt.Errorf("imap login failed: %w", err)
	}
	if err := sendIMAPClientID(c, cfg); err != nil {
		return false, err
	}
	supported, err := c.Support("IDLE")
	if err != nil {
		return false, err
	}
	return supported, nil
}
