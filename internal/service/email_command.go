package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type smtpConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

type emailRequest struct {
	To          []string
	Subject     string
	Body        string
	Attachments []emailAttachment
}

type emailAttachment struct {
	FileName    string
	ContentType string
	Content     []byte
	SourcePath  string
}

type emailCommandResult struct {
	Provider    string   `json:"provider"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Subject     string   `json:"subject"`
	Attachments []string `json:"attachments,omitempty"`
	SentAt      string   `json:"sent_at"`
	MessageID   string   `json:"message_id,omitempty"`
}

type emailSendConfig struct {
	Provider string
	SMTP     *smtpConfig
	MailsCLI *mailsCLIConfig
}

type mailsCLIConfig struct {
	Path string
}

type emailSendResult struct {
	Provider  string
	From      string
	MessageID string
}

type mailSender interface {
	Send(ctx context.Context, req emailRequest) (emailSendResult, error)
}

type smtpSender struct {
	config smtpConfig
}

type mailsCLISender struct {
	config mailsCLIConfig
}

type execCmd interface {
	CombinedOutput() ([]byte, error)
}

var (
	mailsLookPath       = exec.LookPath
	mailsCommandContext = func(ctx context.Context, name string, args ...string) execCmd {
		return exec.CommandContext(ctx, name, args...)
	}
)

type emailAliasConfig struct {
	Aliases map[string][]string `json:"aliases"`
}

func newEmailCommand(app *AppContext) *cobra.Command {
	emailCmd := &cobra.Command{
		Use:     "email",
		Aliases: []string{"mail"},
		Short:   "Send email notifications via SMTP",
	}

	var (
		to          string
		subject     string
		body        string
		bodyFile    string
		attachments []string
	)

	sendCmd := &cobra.Command{
		Use:   "send --to <email[,email...]> --subject <text> [--body <text> | --body-file <path>] [--attach <path>]...",
		Short: "Send one email message",
		RunE: func(cmd *cobra.Command, args []string) error {
			recipients, err := parseRecipients(to)
			if err != nil {
				return err
			}
			if len(recipients) == 0 {
				return fmt.Errorf("at least one recipient is required")
			}

			content, err := resolveEmailBody(body, bodyFile)
			if err != nil {
				return err
			}
			attachmentItems, err := resolveEmailAttachments(attachments)
			if err != nil {
				return err
			}
			if strings.TrimSpace(content) == "" && len(attachmentItems) == 0 {
				pipedContent, pipeErr := readPipedText(os.Stdin)
				if pipeErr != nil {
					return pipeErr
				}
				if strings.TrimSpace(pipedContent) != "" {
					content = pipedContent
				}
			}
			if strings.TrimSpace(content) == "" && len(attachmentItems) > 0 {
				content = "Please see attached file(s)."
			}
			if strings.TrimSpace(content) == "" && len(attachmentItems) == 0 {
				return fmt.Errorf("email body is empty")
			}

			app.SetExecution("email", recipients)

			req := emailRequest{
				To:          recipients,
				Subject:     subject,
				Body:        content,
				Attachments: attachmentItems,
			}
			sender, err := buildMailSenderFromEnv()
			if err != nil {
				return err
			}
			sendResult, err := sender.Send(cmd.Context(), req)
			if err != nil {
				return err
			}

			attachmentNames := make([]string, 0, len(attachmentItems))
			for _, item := range attachmentItems {
				attachmentNames = append(attachmentNames, item.FileName)
			}
			result := emailCommandResult{
				Provider:    sendResult.Provider,
				From:        sendResult.From,
				To:          append([]string(nil), recipients...),
				Subject:     subject,
				Attachments: attachmentNames,
				SentAt:      time.Now().Format(time.RFC3339),
				MessageID:   sendResult.MessageID,
			}
			app.SetResult(result)

			fmt.Printf("Email sent to %s\n", strings.Join(recipients, ", "))
			return nil
		},
	}

	sendCmd.Flags().StringVar(&to, "to", "", "Recipient email addresses, comma separated")
	sendCmd.Flags().StringVar(&subject, "subject", "", "Email subject")
	sendCmd.Flags().StringVar(&body, "body", "", "Email body text")
	sendCmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to a file used as email body")
	sendCmd.Flags().StringArrayVar(&attachments, "attach", nil, "Attachment file path (repeatable)")
	_ = sendCmd.MarkFlagRequired("to")
	_ = sendCmd.MarkFlagRequired("subject")

	emailCmd.AddCommand(sendCmd)
	emailCmd.AddCommand(newEmailReceiveCommand(app))
	emailCmd.AddCommand(newEmailMonitorCommand(app))
	emailCmd.AddCommand(newEmailAnalyzeCommand(app))
	return emailCmd
}

func parseRecipients(raw string) ([]string, error) {
	return parseRecipientsWithAliasPath(raw, defaultEmailAliasConfigPath())
}

func parseRecipientsWithAliasPath(raw string, aliasPath string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	aliasMap, err := loadEmailAliasMap(aliasPath)
	if err != nil {
		return nil, err
	}

	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})

	recipients := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}

		if parsed, parseErr := mail.ParseAddress(token); parseErr == nil {
			recipient := parsed.Address
			if _, exists := seen[recipient]; !exists {
				recipients = append(recipients, recipient)
				seen[recipient] = struct{}{}
			}
			continue
		}

		aliasKey := normalizeRecipientAlias(token)
		resolved, exists := aliasMap[aliasKey]
		if !exists {
			return nil, fmt.Errorf("invalid recipient or alias %q (configure alias in %s)", token, aliasPath)
		}
		for _, recipient := range resolved {
			if _, exists := seen[recipient]; exists {
				continue
			}
			recipients = append(recipients, recipient)
			seen[recipient] = struct{}{}
		}
	}
	return recipients, nil
}

func defaultEmailAliasConfigPath() string {
	return resolveConfigPath("email_aliases.json")
}

func normalizeRecipientAlias(alias string) string {
	return strings.ToLower(strings.TrimSpace(alias))
}

func loadEmailAliasMap(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("read email alias config %s: %w", path, err)
	}

	var config emailAliasConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse email alias config %s: %w", path, err)
	}

	normalized := make(map[string][]string, len(config.Aliases))
	for rawAlias, targets := range config.Aliases {
		alias := normalizeRecipientAlias(rawAlias)
		if alias == "" {
			continue
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("email alias %q has no target addresses", rawAlias)
		}

		resolved := make([]string, 0, len(targets))
		for _, target := range targets {
			trimmed := strings.TrimSpace(target)
			if trimmed == "" {
				continue
			}
			parsed, parseErr := mail.ParseAddress(trimmed)
			if parseErr != nil {
				return nil, fmt.Errorf("email alias %q has invalid address %q: %w", rawAlias, target, parseErr)
			}
			resolved = append(resolved, parsed.Address)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("email alias %q has no valid target addresses", rawAlias)
		}
		normalized[alias] = resolved
	}

	return normalized, nil
}

func resolveEmailBody(body string, bodyFile string) (string, error) {
	if strings.TrimSpace(bodyFile) == "" {
		return body, nil
	}

	data, err := os.ReadFile(bodyFile)
	if err != nil {
		return "", fmt.Errorf("read body file %s: %w", bodyFile, err)
	}
	if strings.TrimSpace(body) == "" {
		return string(data), nil
	}
	return body + "\n" + string(data), nil
}

func readPipedText(stdin *os.File) (string, error) {
	if stdin == nil {
		return "", nil
	}
	info, err := stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect stdin failed: %w", err)
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin failed: %w", err)
	}
	return string(data), nil
}

func resolveEmailAttachments(paths []string) ([]emailAttachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	items := make([]emailAttachment, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read attachment file %s: %w", path, err)
		}
		fileName := filepath.Base(path)
		if strings.TrimSpace(fileName) == "" || fileName == "." || fileName == string(os.PathSeparator) {
			return nil, fmt.Errorf("invalid attachment file name from path: %s", path)
		}
		contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
		if strings.TrimSpace(contentType) == "" {
			contentType = "application/octet-stream"
		}

		items = append(items, emailAttachment{
			FileName:    fileName,
			ContentType: contentType,
			Content:     data,
			SourcePath:  path,
		})
	}
	return items, nil
}

func loadSMTPConfigFromEnv() (smtpConfig, error) {
	host := getEnvFirst("SMTP_HOST", "MAIL_HOST")
	portText := getEnvFirst("SMTP_PORT", "MAIL_PORT")
	username := getEnvFirst("SMTP_USERNAME", "MAIL_USERNAME", "SMTP_USER", "MAIL_USER")
	password := getEnvFirst("SMTP_PASSWORD", "MAIL_PASSWORD", "SMTP_PASS", "MAIL_PASS")
	from := getEnvFirst("SMTP_FROM", "MAIL_FROM")

	if host == "" {
		return smtpConfig{}, fmt.Errorf("missing SMTP_HOST")
	}

	port := 587
	if strings.TrimSpace(portText) != "" {
		parsedPort, err := strconv.Atoi(portText)
		if err != nil {
			return smtpConfig{}, fmt.Errorf("invalid SMTP_PORT: %w", err)
		}
		port = parsedPort
	}

	if from == "" {
		from = username
	}
	if from == "" {
		return smtpConfig{}, fmt.Errorf("missing SMTP_FROM (or SMTP_USERNAME as fallback)")
	}

	parsedFrom, err := mail.ParseAddress(from)
	if err != nil {
		return smtpConfig{}, fmt.Errorf("invalid SMTP_FROM: %w", err)
	}

	hasUser := strings.TrimSpace(username) != ""
	hasPass := strings.TrimSpace(password) != ""
	if hasUser != hasPass {
		return smtpConfig{}, fmt.Errorf("SMTP_USERNAME and SMTP_PASSWORD must be both set or both empty")
	}

	return smtpConfig{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		From:     parsedFrom.Address,
	}, nil
}

func loadEmailSendConfigFromEnv() (emailSendConfig, error) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("MAIL_SEND_PROVIDER")))
	if provider == "" || provider == "smtp" {
		cfg, err := loadSMTPConfigFromEnv()
		if err != nil {
			return emailSendConfig{}, err
		}
		return emailSendConfig{
			Provider: "smtp",
			SMTP:     &cfg,
		}, nil
	}

	switch provider {
	case "mails_cli":
		path := strings.TrimSpace(os.Getenv("MAILS_CLI_PATH"))
		if path == "" {
			resolved, err := mailsLookPath("mails")
			if err != nil {
				return emailSendConfig{}, fmt.Errorf("resolve mails cli: %w", err)
			}
			path = resolved
		}
		return emailSendConfig{
			Provider: "mails_cli",
			MailsCLI: &mailsCLIConfig{Path: path},
		}, nil
	default:
		return emailSendConfig{}, fmt.Errorf("unsupported mail send provider %q", provider)
	}
}

func buildMailSenderFromEnv() (mailSender, error) {
	cfg, err := loadEmailSendConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return buildMailSender(cfg)
}

func buildMailSender(cfg emailSendConfig) (mailSender, error) {
	switch cfg.Provider {
	case "smtp":
		if cfg.SMTP == nil {
			return nil, fmt.Errorf("smtp provider requires smtp config")
		}
		return smtpSender{config: *cfg.SMTP}, nil
	case "mails_cli":
		if cfg.MailsCLI == nil {
			return nil, fmt.Errorf("mails_cli provider requires mails cli config")
		}
		if strings.TrimSpace(cfg.MailsCLI.Path) == "" {
			return nil, fmt.Errorf("mails cli path is empty")
		}
		return mailsCLISender{config: *cfg.MailsCLI}, nil
	default:
		return nil, fmt.Errorf("unsupported mail send provider %q", cfg.Provider)
	}
}

func (s smtpSender) Send(ctx context.Context, req emailRequest) (emailSendResult, error) {
	if err := sendSMTPMail(s.config, req); err != nil {
		return emailSendResult{}, err
	}
	return emailSendResult{
		Provider: fmt.Sprintf("smtp:%s:%d", s.config.Host, s.config.Port),
		From:     s.config.From,
	}, nil
}

func (s mailsCLISender) Send(ctx context.Context, req emailRequest) (emailSendResult, error) {
	args := []string{"send"}
	for _, recipient := range req.To {
		args = append(args, "--to", recipient)
	}
	args = append(args, "--subject", req.Subject, "--body", req.Body)
	for _, attachment := range req.Attachments {
		if strings.TrimSpace(attachment.SourcePath) == "" {
			return emailSendResult{}, fmt.Errorf("attachment %q missing source path for mails cli provider", attachment.FileName)
		}
		args = append(args, "--attach", attachment.SourcePath)
	}

	output, err := mailsCommandContext(ctx, s.config.Path, args...).CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return emailSendResult{}, fmt.Errorf("mails send failed: %w", err)
		}
		return emailSendResult{}, fmt.Errorf("mails send failed: %w: %s", err, trimmed)
	}

	return emailSendResult{
		Provider:  "mails_cli",
		From:      "",
		MessageID: extractMessageIDFromOutput(string(output)),
	}, nil
}

func extractMessageIDFromOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "message id:") {
			return strings.TrimSpace(line[len("message id:"):])
		}
	}
	return ""
}

func sendSMTPMail(cfg smtpConfig, req emailRequest) error {
	message := buildEmailMessage(cfg.From, req)

	switch cfg.Port {
	case 465:
		return sendSMTPWithImplicitTLS(cfg, req.To, message)
	default:
		addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
		return smtp.SendMail(addr, smtpAuth(cfg), cfg.From, req.To, message)
	}
}

func sendSMTPWithImplicitTLS(cfg smtpConfig, recipients []string, message []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return err
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()

	if auth := smtpAuth(cfg); auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}

	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}

	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(message); err != nil {
		_ = wc.Close()
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}

	return client.Quit()
}

func smtpAuth(cfg smtpConfig) smtp.Auth {
	if cfg.Username == "" || cfg.Password == "" {
		return nil
	}
	return smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
}

func buildEmailMessage(from string, req emailRequest) []byte {
	subject := sanitizeHeader(req.Subject)
	toHeader := make([]string, 0, len(req.To))
	for _, r := range req.To {
		toHeader = append(toHeader, sanitizeHeader(r))
	}

	encodedSubject := fmt.Sprintf("=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(subject)))
	encodedBody := wrapBase64(base64.StdEncoding.EncodeToString([]byte(req.Body)))

	var builder strings.Builder
	builder.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	builder.WriteString("To: " + strings.Join(toHeader, ", ") + "\r\n")
	builder.WriteString("Subject: " + encodedSubject + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")

	buildPlain := func() []byte {
		var plainBuilder strings.Builder
		plainBuilder.WriteString(builder.String())
		plainBuilder.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		plainBuilder.WriteString("Content-Transfer-Encoding: base64\r\n")
		plainBuilder.WriteString("\r\n")
		plainBuilder.WriteString(encodedBody)
		plainBuilder.WriteString("\r\n")
		return []byte(plainBuilder.String())
	}

	if len(req.Attachments) == 0 {
		return buildPlain()
	}

	var body bytes.Buffer
	mixedWriter := multipart.NewWriter(&body)

	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", `text/plain; charset="UTF-8"`)
	textHeader.Set("Content-Transfer-Encoding", "base64")
	textPart, err := mixedWriter.CreatePart(textHeader)
	if err != nil {
		return buildPlain()
	}
	if _, err := io.WriteString(textPart, encodedBody); err != nil {
		return buildPlain()
	}
	if _, err := io.WriteString(textPart, "\r\n"); err != nil {
		return buildPlain()
	}

	for _, attachment := range req.Attachments {
		fileName := sanitizeHeader(attachment.FileName)
		contentType := sanitizeHeader(attachment.ContentType)
		if strings.TrimSpace(contentType) == "" {
			contentType = "application/octet-stream"
		}
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Type", fmt.Sprintf(`%s; name="%s"`, contentType, fileName))
		partHeader.Set("Content-Transfer-Encoding", "base64")
		partHeader.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))

		part, err := mixedWriter.CreatePart(partHeader)
		if err != nil {
			return buildPlain()
		}
		encodedAttachment := wrapBase64(base64.StdEncoding.EncodeToString(attachment.Content))
		if _, err := io.WriteString(part, encodedAttachment); err != nil {
			return buildPlain()
		}
		if _, err := io.WriteString(part, "\r\n"); err != nil {
			return buildPlain()
		}
	}

	if err := mixedWriter.Close(); err != nil {
		return buildPlain()
	}
	builder.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n", mixedWriter.Boundary()))
	builder.WriteString("\r\n")
	builder.Write(body.Bytes())
	return []byte(builder.String())
}

func wrapBase64(input string) string {
	if len(input) <= 76 {
		return input
	}

	var builder strings.Builder
	for len(input) > 76 {
		builder.WriteString(input[:76])
		builder.WriteString("\r\n")
		input = input[76:]
	}
	builder.WriteString(input)
	return builder.String()
}

func sanitizeHeader(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}
