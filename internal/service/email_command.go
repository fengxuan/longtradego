package service

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"os"
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
	To      []string
	Subject string
	Body    string
}

type emailCommandResult struct {
	Provider string   `json:"provider"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Subject  string   `json:"subject"`
	SentAt   string   `json:"sent_at"`
}

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
		to       string
		subject  string
		body     string
		bodyFile string
	)

	sendCmd := &cobra.Command{
		Use:   "send --to <email[,email...]> --subject <text> [--body <text> | --body-file <path>]",
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
			if strings.TrimSpace(content) == "" {
				return fmt.Errorf("email body is empty")
			}

			app.SetExecution("email", recipients)

			cfg, err := loadSMTPConfigFromEnv()
			if err != nil {
				return err
			}

			req := emailRequest{
				To:      recipients,
				Subject: subject,
				Body:    content,
			}
			if err := sendSMTPMail(cfg, req); err != nil {
				return err
			}

			result := emailCommandResult{
				Provider: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
				From:     cfg.From,
				To:       append([]string(nil), recipients...),
				Subject:  subject,
				SentAt:   time.Now().Format(time.RFC3339),
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
	return filepath.Join("conf", "email_aliases.json")
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
	builder.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	builder.WriteString("Content-Transfer-Encoding: base64\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(encodedBody)
	builder.WriteString("\r\n")

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
