package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRecipientsWithAliasPathDirectEmails(t *testing.T) {
	aliasPath := filepath.Join(t.TempDir(), "missing_aliases.json")

	recipients, err := parseRecipientsWithAliasPath("alice@example.com,bob@example.com", aliasPath)
	if err != nil {
		t.Fatalf("parseRecipientsWithAliasPath failed: %v", err)
	}

	expected := []string{"alice@example.com", "bob@example.com"}
	if !reflect.DeepEqual(recipients, expected) {
		t.Fatalf("unexpected recipients: got %v want %v", recipients, expected)
	}
}

func TestParseRecipientsWithAliasPathAliasLookup(t *testing.T) {
	tmpDir := t.TempDir()
	aliasPath := filepath.Join(tmpDir, "email_aliases.json")
	data := `{
	  "aliases": {
	    "qa": ["qa1@example.com", "QA Team <qa2@example.com>"],
	    "risk": ["risk@example.com"]
	  }
	}`
	if err := os.WriteFile(aliasPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write alias config: %v", err)
	}

	recipients, err := parseRecipientsWithAliasPath("qa,risk,qa2@example.com", aliasPath)
	if err != nil {
		t.Fatalf("parseRecipientsWithAliasPath failed: %v", err)
	}

	expected := []string{"qa1@example.com", "qa2@example.com", "risk@example.com"}
	if !reflect.DeepEqual(recipients, expected) {
		t.Fatalf("unexpected recipients: got %v want %v", recipients, expected)
	}
}

func TestParseRecipientsWithAliasPathUnknownAlias(t *testing.T) {
	tmpDir := t.TempDir()
	aliasPath := filepath.Join(tmpDir, "email_aliases.json")
	data := `{"aliases": {"qa": ["qa@example.com"]}}`
	if err := os.WriteFile(aliasPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write alias config: %v", err)
	}

	_, err := parseRecipientsWithAliasPath("ops", aliasPath)
	if err == nil {
		t.Fatalf("expected error for unknown alias")
	}
	if !strings.Contains(err.Error(), "invalid recipient or alias") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadEmailAliasMapInvalidAddress(t *testing.T) {
	tmpDir := t.TempDir()
	aliasPath := filepath.Join(tmpDir, "email_aliases.json")
	data := `{"aliases": {"qa": ["not-an-email"]}}`
	if err := os.WriteFile(aliasPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write alias config: %v", err)
	}

	_, err := loadEmailAliasMap(aliasPath)
	if err == nil {
		t.Fatalf("expected invalid alias config error")
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveEmailAttachments(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "report.pdf")
	content := []byte("%PDF-1.4\nsample")
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}

	attachments, err := resolveEmailAttachments([]string{filePath})
	if err != nil {
		t.Fatalf("resolveEmailAttachments failed: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected one attachment, got %d", len(attachments))
	}
	if attachments[0].FileName != "report.pdf" {
		t.Fatalf("unexpected attachment name: %q", attachments[0].FileName)
	}
	if attachments[0].ContentType != "application/pdf" {
		t.Fatalf("unexpected attachment content type: %q", attachments[0].ContentType)
	}
	if !bytes.Equal(attachments[0].Content, content) {
		t.Fatalf("unexpected attachment content")
	}
	if attachments[0].SourcePath != filePath {
		t.Fatalf("unexpected attachment source path: got %q want %q", attachments[0].SourcePath, filePath)
	}
}

func TestLoadEmailSendConfigFromEnvDefaultsToSMTP(t *testing.T) {
	t.Setenv("MAIL_SEND_PROVIDER", "")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("SMTP_USERNAME", "user@example.com")
	t.Setenv("SMTP_PASSWORD", "secret")
	t.Setenv("SMTP_FROM", "sender@example.com")

	cfg, err := loadEmailSendConfigFromEnv()
	if err != nil {
		t.Fatalf("loadEmailSendConfigFromEnv failed: %v", err)
	}
	if cfg.Provider != "smtp" {
		t.Fatalf("unexpected provider: %q", cfg.Provider)
	}
	if cfg.SMTP == nil || cfg.SMTP.Host != "smtp.example.com" {
		t.Fatalf("unexpected smtp config: %#v", cfg.SMTP)
	}
}

func TestLoadEmailSendConfigFromEnvMailsCLI(t *testing.T) {
	t.Setenv("MAIL_SEND_PROVIDER", "mails_cli")
	t.Setenv("MAILS_CLI_PATH", "/usr/local/bin/mails")

	cfg, err := loadEmailSendConfigFromEnv()
	if err != nil {
		t.Fatalf("loadEmailSendConfigFromEnv failed: %v", err)
	}
	if cfg.Provider != "mails_cli" {
		t.Fatalf("unexpected provider: %q", cfg.Provider)
	}
	if cfg.MailsCLI == nil || cfg.MailsCLI.Path != "/usr/local/bin/mails" {
		t.Fatalf("unexpected mails cli config: %#v", cfg.MailsCLI)
	}
}

func TestBuildMailSenderUnsupportedProvider(t *testing.T) {
	_, err := buildMailSender(emailSendConfig{Provider: "unknown"})
	if err == nil {
		t.Fatalf("expected unsupported provider error")
	}
	if !strings.Contains(err.Error(), "unsupported mail send provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMailsCLISenderSend(t *testing.T) {
	origCommandContext := mailsCommandContext
	defer func() { mailsCommandContext = origCommandContext }()

	var gotName string
	var gotArgs []string
	mailsCommandContext = func(ctx context.Context, name string, args ...string) execCmd {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return fakeExecCmd{output: []byte("Message ID: msg-123\n")}
	}

	sender := mailsCLISender{config: mailsCLIConfig{Path: "/opt/homebrew/bin/mails"}}
	result, err := sender.Send(context.Background(), emailRequest{
		To:      []string{"alice@example.com", "bob@example.com"},
		Subject: "hello",
		Body:    "world",
		Attachments: []emailAttachment{{
			FileName:   "report.pdf",
			SourcePath: "/tmp/report.pdf",
		}},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if gotName != "/opt/homebrew/bin/mails" {
		t.Fatalf("unexpected command name: %q", gotName)
	}
	expectedArgs := []string{
		"send",
		"--to", "alice@example.com",
		"--to", "bob@example.com",
		"--subject", "hello",
		"--body", "world",
		"--attach", "/tmp/report.pdf",
	}
	if !reflect.DeepEqual(gotArgs, expectedArgs) {
		t.Fatalf("unexpected args: got %v want %v", gotArgs, expectedArgs)
	}
	if result.Provider != "mails_cli" {
		t.Fatalf("unexpected provider: %q", result.Provider)
	}
	if result.MessageID != "msg-123" {
		t.Fatalf("unexpected message id: %q", result.MessageID)
	}
}

func TestMailsCLISenderSendRequiresAttachmentSourcePath(t *testing.T) {
	sender := mailsCLISender{config: mailsCLIConfig{Path: "/usr/local/bin/mails"}}
	_, err := sender.Send(context.Background(), emailRequest{
		To:      []string{"alice@example.com"},
		Subject: "hello",
		Body:    "world",
		Attachments: []emailAttachment{{
			FileName: "report.pdf",
		}},
	})
	if err == nil {
		t.Fatalf("expected attachment source path error")
	}
	if !strings.Contains(err.Error(), "missing source path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMailsCLISenderSendCommandFailure(t *testing.T) {
	origCommandContext := mailsCommandContext
	defer func() { mailsCommandContext = origCommandContext }()

	mailsCommandContext = func(ctx context.Context, name string, args ...string) execCmd {
		return fakeExecCmd{output: []byte("boom"), err: fmt.Errorf("exit status 1")}
	}

	sender := mailsCLISender{config: mailsCLIConfig{Path: "/usr/local/bin/mails"}}
	_, err := sender.Send(context.Background(), emailRequest{
		To:      []string{"alice@example.com"},
		Subject: "hello",
		Body:    "world",
	})
	if err == nil {
		t.Fatalf("expected mails cli failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadPipedText(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	if _, err := writer.WriteString("hello from pipe"); err != nil {
		t.Fatalf("write pipe failed: %v", err)
	}
	_ = writer.Close()

	content, err := readPipedText(reader)
	if err != nil {
		t.Fatalf("readPipedText failed: %v", err)
	}
	if content != "hello from pipe" {
		t.Fatalf("unexpected piped content: %q", content)
	}
}

func TestBuildEmailMessageWithAttachmentMultipart(t *testing.T) {
	attachmentRaw := []byte("%PDF-1.4\nhello")
	req := emailRequest{
		To:      []string{"alice@example.com"},
		Subject: "Report",
		Body:    "See attached report",
		Attachments: []emailAttachment{
			{
				FileName:    "report.pdf",
				ContentType: "application/pdf",
				Content:     attachmentRaw,
			},
		},
	}

	message := buildEmailMessage("sender@example.com", req)
	parsed, err := mail.ReadMessage(bytes.NewReader(message))
	if err != nil {
		t.Fatalf("mail.ReadMessage failed: %v", err)
	}

	contentType := parsed.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("mime.ParseMediaType failed: %v", err)
	}
	if mediaType != "multipart/mixed" {
		t.Fatalf("expected multipart/mixed, got %q", mediaType)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		t.Fatalf("expected multipart boundary")
	}

	reader := multipart.NewReader(parsed.Body, boundary)
	textPart, err := reader.NextPart()
	if err != nil {
		t.Fatalf("read text part failed: %v", err)
	}
	textEncoded, err := io.ReadAll(textPart)
	if err != nil {
		t.Fatalf("read text part body failed: %v", err)
	}
	textDecoded := decodeBase64ForTest(t, string(textEncoded))
	if textDecoded != req.Body {
		t.Fatalf("unexpected decoded text body: %q", textDecoded)
	}

	attachmentPart, err := reader.NextPart()
	if err != nil {
		t.Fatalf("read attachment part failed: %v", err)
	}
	if got := attachmentPart.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, `filename="report.pdf"`) {
		t.Fatalf("unexpected Content-Disposition: %q", got)
	}
	if got := attachmentPart.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/pdf") {
		t.Fatalf("unexpected Content-Type: %q", got)
	}
	attachmentEncoded, err := io.ReadAll(attachmentPart)
	if err != nil {
		t.Fatalf("read attachment body failed: %v", err)
	}
	attachmentDecoded := []byte(decodeBase64ForTest(t, string(attachmentEncoded)))
	if !bytes.Equal(attachmentDecoded, attachmentRaw) {
		t.Fatalf("unexpected decoded attachment body")
	}
}

func decodeBase64ForTest(t *testing.T, raw string) string {
	t.Helper()
	cleaned := strings.ReplaceAll(raw, "\r", "")
	cleaned = strings.ReplaceAll(cleaned, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	return string(decoded)
}

type fakeExecCmd struct {
	output []byte
	err    error
}

func (c fakeExecCmd) CombinedOutput() ([]byte, error) {
	return c.output, c.err
}
