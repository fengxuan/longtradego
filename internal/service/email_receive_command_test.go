package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

func TestDefaultIMAPSettingsConfigPath(t *testing.T) {
	got := defaultIMAPSettingsConfigPath()
	want := filepath.Join("conf", "mail_receive_setting.json")
	if got != want {
		t.Fatalf("unexpected default settings path: got %q want %q", got, want)
	}
}

func TestMailMonitorEventHookContext(t *testing.T) {
	called := false
	hook := func(result imapReceiveResult) error {
		called = result.MatchedCount == 1
		return nil
	}
	ctx := withMailMonitorEventHook(context.Background(), hook)
	got, ok := mailMonitorEventHookFromContext(ctx)
	if !ok {
		t.Fatalf("expected hook in context")
	}
	if err := got(imapReceiveResult{MatchedCount: 1}); err != nil {
		t.Fatalf("hook call failed: %v", err)
	}
	if !called {
		t.Fatalf("expected hook to be called")
	}
	if _, ok := mailMonitorEventHookFromContext(context.Background()); ok {
		t.Fatalf("did not expect hook in empty context")
	}
}

func TestLoadIMAPConfigFromSettingsDefaultAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_receive_setting.json")
	data := `{
	  "default_alias": "work",
	  "aliases": {
	    "work": {
	      "IMAP_HOST": "imap.work.example.com",
	      "IMAP_USERNAME": "ops@example.com",
	      "IMAP_PASSWORD": "secret"
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write settings failed: %v", err)
	}

	cfg, err := loadIMAPConfigFromSettings(path, "")
	if err != nil {
		t.Fatalf("loadIMAPConfigFromSettings failed: %v", err)
	}
	if cfg.Host != "imap.work.example.com" {
		t.Fatalf("unexpected host: %q", cfg.Host)
	}
	if cfg.Username != "ops@example.com" {
		t.Fatalf("unexpected username: %q", cfg.Username)
	}
	if cfg.Port != 993 {
		t.Fatalf("expected default port 993, got %d", cfg.Port)
	}
	if cfg.Mailbox != "INBOX" {
		t.Fatalf("expected default mailbox INBOX, got %q", cfg.Mailbox)
	}
	if !cfg.UseTLS {
		t.Fatalf("expected default tls=true")
	}
	if cfg.InsecureSkipVerify {
		t.Fatalf("expected default insecure=false")
	}
}

func TestLoadIMAPConfigFromSettingsSelectedAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_receive_setting.json")
	data := `{
	  "default_alias": "main",
	  "aliases": {
	    "main": {
	      "IMAP_HOST": "imap.main.example.com",
	      "IMAP_USERNAME": "main@example.com",
	      "IMAP_PASSWORD": "main-pass"
	    },
	    "ops": {
	      "IMAP_HOST": "imap.ops.example.com",
	      "IMAP_PORT": 1993,
	      "IMAP_USERNAME": "ops@example.com",
	      "IMAP_PASSWORD": "ops-pass",
	      "IMAP_MAILBOX": "ALERTS",
	      "IMAP_TLS": false,
	      "IMAP_INSECURE_SKIP_VERIFY": true
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write settings failed: %v", err)
	}

	cfg, err := loadIMAPConfigFromSettings(path, "ops")
	if err != nil {
		t.Fatalf("loadIMAPConfigFromSettings failed: %v", err)
	}
	if cfg.Host != "imap.ops.example.com" {
		t.Fatalf("unexpected host: %q", cfg.Host)
	}
	if cfg.Port != 1993 {
		t.Fatalf("expected port 1993, got %d", cfg.Port)
	}
	if cfg.Mailbox != "ALERTS" {
		t.Fatalf("unexpected mailbox: %q", cfg.Mailbox)
	}
	if cfg.UseTLS {
		t.Fatalf("expected tls=false from alias")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatalf("expected insecure_skip_verify=true from alias")
	}
}

func TestLoadIMAPConfigFromSettingsAliasNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_receive_setting.json")
	data := `{
	  "default_alias": "main",
	  "aliases": {
	    "main": {
	      "IMAP_HOST": "imap.main.example.com",
	      "IMAP_USERNAME": "main@example.com",
	      "IMAP_PASSWORD": "main-pass"
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write settings failed: %v", err)
	}

	_, err := loadIMAPConfigFromSettings(path, "ops")
	if err == nil {
		t.Fatalf("expected alias not found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadIMAPConfigFromSettingsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing_mail_receive_setting.json")
	_, err := loadIMAPConfigFromSettings(path, "")
	if err == nil {
		t.Fatalf("expected missing file error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestExpandIMAPActionCommand(t *testing.T) {
	result := imapReceiveResult{
		Provider:     "imap.example.com:993",
		Mailbox:      "INBOX",
		MatchedCount: 3,
	}
	command := expandIMAPActionCommand(`echo "mail=${MAIL_COUNT} box=${MAILBOX} from=${IMAP_PROVIDER}"`, result)
	if !strings.Contains(command, "mail=3") || !strings.Contains(command, "box=INBOX") || !strings.Contains(command, "from=imap.example.com:993") {
		t.Fatalf("unexpected expanded command: %q", command)
	}
}

func TestSanitizeAttachmentFileName(t *testing.T) {
	got := sanitizeAttachmentFileName(`../my:report?.txt`)
	if got != ".._my_report_.txt" {
		t.Fatalf("unexpected sanitized file name: %q", got)
	}
}

func TestExtractMailContentAndFiles(t *testing.T) {
	raw := strings.Join([]string{
		"From: test@example.com",
		"To: bot@example.com",
		"Subject: demo",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="abc123"`,
		"",
		"--abc123",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"hello-body",
		"--abc123",
		`Content-Type: text/plain; name="note.txt"`,
		`Content-Disposition: attachment; filename="note.txt"`,
		"",
		"attachment-data",
		"--abc123--",
		"",
	}, "\r\n")

	opts := imapReceiveOptions{
		WithBody:     true,
		WithFiles:    true,
		FilesDir:     t.TempDir(),
		BodyMaxBytes: 1024,
	}

	body, files, err := extractMailContentAndFiles([]byte(raw), 99, opts)
	if err != nil {
		t.Fatalf("extractMailContentAndFiles failed: %v", err)
	}
	if !strings.Contains(body, "hello-body") {
		t.Fatalf("unexpected body: %q", body)
	}
	if len(files) != 1 {
		t.Fatalf("expected one attachment, got %d", len(files))
	}
	if files[0].SavedPath == "" {
		t.Fatalf("expected saved file path")
	}
	if filepath.Base(files[0].SavedPath) != "note.txt" {
		t.Fatalf("unexpected saved file name: %s", files[0].SavedPath)
	}
}

func TestEnrichSummaryFromRawHeaderReadableFields(t *testing.T) {
	raw := strings.Join([]string{
		"From: =?US-ASCII?Q?Keith_Moore?= <keith@example.com>",
		"To: bot@example.com",
		"Subject: =?US-ASCII?Q?Hello_=26_Welcome?=",
		"Message-ID: <msg-1@example.com>",
		"In-Reply-To: <ref-1@example.com> <ref-2@example.com>",
		"MIME-Version: 1.0",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"hello",
		"",
	}, "\r\n")

	summary := imapMessageSummary{
		UID:       1,
		Subject:   "raw-subject",
		From:      []string{"raw@example.com"},
		MessageID: "raw-message-id",
		InReplyTo: "raw-in-reply-to",
	}

	enrichSummaryFromRawHeader([]byte(raw), &summary)

	if summary.Subject != "Hello & Welcome" {
		t.Fatalf("expected decoded subject, got %q", summary.Subject)
	}
	if len(summary.From) != 1 || summary.From[0] != "Keith Moore <keith@example.com>" {
		t.Fatalf("expected decoded from address, got %#v", summary.From)
	}
	if summary.MessageID != "<msg-1@example.com>" {
		t.Fatalf("expected message id with angle brackets, got %q", summary.MessageID)
	}
	if summary.InReplyTo != "<ref-1@example.com>" {
		t.Fatalf("expected in-reply-to message id with angle brackets, got %q", summary.InReplyTo)
	}
}

func TestFormatEnvelopeAddressIncludesDisplayName(t *testing.T) {
	addr := &imap.Address{
		PersonalName: "Ops Team",
		MailboxName:  "ops",
		HostName:     "example.com",
	}
	got := formatEnvelopeAddress(addr)
	if got != "Ops Team <ops@example.com>" {
		t.Fatalf("unexpected formatted envelope address: %q", got)
	}
}

func TestCollectNewUIDs(t *testing.T) {
	messages := []imapMessageSummary{
		{UID: 3},
		{UID: 10},
		{UID: 7},
	}
	uids := collectNewUIDs(messages, 6)
	if len(uids) != 2 || uids[0] != 7 || uids[1] != 10 {
		t.Fatalf("unexpected new uids: %v", uids)
	}
}

func TestParseAnalyzeInputJSONFromResultWrapper(t *testing.T) {
	input := `{"source_command":"mail monitor","result":{"messages":[{"uid":101,"subject":"A"}],"matched_count":1}}`
	result, err := parseAnalyzeInputJSON(input)
	if err != nil {
		t.Fatalf("parseAnalyzeInputJSON failed: %v", err)
	}
	if result.MatchedCount != 1 || len(result.Messages) != 1 || result.Messages[0].UID != 101 {
		t.Fatalf("unexpected parsed result: %+v", result)
	}
}

func TestMonitorIMAPMessagesInvalidMode(t *testing.T) {
	_, err := monitorIMAPMessages(
		t.Context(),
		imapConfig{},
		imapReceiveOptions{},
		10*time.Second,
		0,
		"unknown",
		time.Minute,
	)
	if err == nil {
		t.Fatalf("expected invalid monitor mode error")
	}
	if !strings.Contains(err.Error(), "unsupported monitor mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveMonitorModeDefaultsHybrid(t *testing.T) {
	t.Setenv("IMAP_MONITOR_MODE", "")
	if got := resolveMonitorMode(""); got != monitorModeHybrid {
		t.Fatalf("expected hybrid default mode, got %q", got)
	}
}

func TestResolveMonitorDurationFromEnv(t *testing.T) {
	t.Setenv("IMAP_MONITOR_POLL_INTERVAL", "42s")
	got := resolveMonitorDuration("IMAP_MONITOR_POLL_INTERVAL", 0, 10*time.Second)
	if got != 42*time.Second {
		t.Fatalf("expected env duration 42s, got %s", got)
	}
}

func TestShouldRetryMonitorIdleError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "disconnected while idling",
			err:  errors.New("disconnected while idling"),
			want: true,
		},
		{
			name: "connection reset by peer",
			err:  errors.New("read: connection reset by peer"),
			want: true,
		},
		{
			name: "closed network connection",
			err:  errors.New("use of closed network connection"),
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("permission denied"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRetryMonitorIdleError(tc.err)
			if got != tc.want {
				t.Fatalf("shouldRetryMonitorIdleError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAdaptivePollIntervalBackoffAndReset(t *testing.T) {
	adaptive := newAdaptivePollInterval(15*time.Second, 5*time.Second, 300*time.Second, 3)

	if adaptive.Current() != 15*time.Second {
		t.Fatalf("expected initial 15s, got %s", adaptive.Current())
	}
	if got := adaptive.OnNoNew(); got != 15*time.Second {
		t.Fatalf("poll#1 expected 15s, got %s", got)
	}
	if got := adaptive.OnNoNew(); got != 15*time.Second {
		t.Fatalf("poll#2 expected 15s, got %s", got)
	}
	if got := adaptive.OnNoNew(); got != 20*time.Second {
		t.Fatalf("poll#3 expected 20s, got %s", got)
	}
	if got := adaptive.OnNoNew(); got != 20*time.Second {
		t.Fatalf("poll#4 expected 20s, got %s", got)
	}
	if got := adaptive.OnNoNew(); got != 20*time.Second {
		t.Fatalf("poll#5 expected 20s, got %s", got)
	}
	if got := adaptive.OnNoNew(); got != 25*time.Second {
		t.Fatalf("poll#6 expected 25s, got %s", got)
	}
	if got := adaptive.OnDetected(); got != 15*time.Second {
		t.Fatalf("after detect expected reset to 15s, got %s", got)
	}
}

func TestAdaptivePollIntervalCappedAtMax(t *testing.T) {
	adaptive := newAdaptivePollInterval(295*time.Second, 5*time.Second, 300*time.Second, 1)

	if got := adaptive.OnNoNew(); got != 300*time.Second {
		t.Fatalf("expected first increase to 300s, got %s", got)
	}
	if got := adaptive.OnNoNew(); got != 300*time.Second {
		t.Fatalf("expected capped at 300s, got %s", got)
	}
}

func TestMailMonitorCursorSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "mail_monitor_cursors.json")
	key := "host=a|port=993|user=u|mailbox=inbox|unread=true|subject=|from="
	if err := saveMailMonitorCursor(path, key, 1234); err != nil {
		t.Fatalf("saveMailMonitorCursor failed: %v", err)
	}

	got, found, err := loadMailMonitorCursor(path, key)
	if err != nil {
		t.Fatalf("loadMailMonitorCursor failed: %v", err)
	}
	if !found || got != 1234 {
		t.Fatalf("expected cursor 1234 found=true, got %d found=%v", got, found)
	}
}

func TestLoadMailMonitorCursorNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "mail_monitor_cursors.json")
	got, found, err := loadMailMonitorCursor(path, "missing")
	if err != nil {
		t.Fatalf("load missing cursor should not fail, got %v", err)
	}
	if found || got != 0 {
		t.Fatalf("expected missing cursor result, got value=%d found=%v", got, found)
	}
}

func TestBuildMailMonitorCursorKey(t *testing.T) {
	cfg := imapConfig{
		Host:     "IMAP.EXAMPLE.COM",
		Port:     993,
		Username: "BOT@EXAMPLE.COM",
		Mailbox:  "INBOX",
	}
	opts := imapReceiveOptions{
		UnreadOnly:      true,
		SubjectContains: "ALERT",
		FromContains:    "OPS@EXAMPLE.COM",
	}
	key := buildMailMonitorCursorKey(cfg, opts, "")
	if !strings.Contains(key, "host=imap.example.com") || !strings.Contains(key, "subject=alert") {
		t.Fatalf("unexpected cursor key: %q", key)
	}
}

func TestBuildMailMonitorCursorKeyWithMonitorID(t *testing.T) {
	cfg := imapConfig{
		Host:     "imap.example.com",
		Port:     993,
		Username: "bot@example.com",
		Mailbox:  "INBOX",
	}
	opts := imapReceiveOptions{UnreadOnly: true}
	key := buildMailMonitorCursorKey(cfg, opts, "m-9")
	if !strings.Contains(key, "monitor_id=m-9") {
		t.Fatalf("expected monitor id in cursor key, got %q", key)
	}
}

func TestReadMailMonitorCursorStateInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "mail_monitor_cursors.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid json failed: %v", err)
	}
	if _, err := readMailMonitorCursorState(path); err == nil {
		t.Fatalf("expected invalid json parse error")
	}
}
