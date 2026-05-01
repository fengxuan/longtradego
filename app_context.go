package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/longbridge/openapi-go/config"
	"github.com/longbridge/openapi-go/oauth"
	"github.com/longbridge/openapi-go/quote"
	protocol "github.com/longbridge/openapi-protocol/go"
)

type appContext struct {
	mu       sync.Mutex
	cfg      *config.Config
	quoteCtx *quote.QuoteContext
	command  string
	symbols  []string
	result   any
}

func newAppContext() *appContext {
	return &appContext{}
}

func (a *appContext) Config(ctx context.Context) (*config.Config, error) {
	a.mu.Lock()
	if a.cfg != nil {
		cfg := a.cfg
		a.mu.Unlock()
		return cfg, nil
	}
	a.mu.Unlock()

	cfg, err := buildConfig(ctx)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		a.cfg = cfg
	}
	return a.cfg, nil
}

func (a *appContext) QuoteContext(ctx context.Context) (*quote.QuoteContext, error) {
	a.mu.Lock()
	if a.quoteCtx != nil {
		qctx := a.quoteCtx
		a.mu.Unlock()
		return qctx, nil
	}
	a.mu.Unlock()

	cfg, err := a.Config(ctx)
	if err != nil {
		return nil, err
	}

	qctx, err := quote.NewFromCfg(cfg)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quoteCtx == nil {
		a.quoteCtx = qctx
		return a.quoteCtx, nil
	}

	// Another goroutine already initialized; close the extra context.
	_ = qctx.Close()
	return a.quoteCtx, nil
}

func (a *appContext) SetExecution(command string, symbols []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.command = command
	a.symbols = append([]string(nil), symbols...)
	a.result = nil
}

func (a *appContext) ResetExecution() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.command = ""
	a.symbols = nil
	a.result = nil
}

func (a *appContext) SetResult(result any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.result = result
}

func (a *appContext) ExecutionSnapshot() (command string, symbols []string, result any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.command, append([]string(nil), a.symbols...), a.result
}

func (a *appContext) Close() error {
	a.mu.Lock()
	qctx := a.quoteCtx
	a.quoteCtx = nil
	a.mu.Unlock()

	if qctx != nil {
		return qctx.Close()
	}
	return nil
}

func (a *appContext) ResetQuoteContext() error {
	a.mu.Lock()
	qctx := a.quoteCtx
	a.quoteCtx = nil
	a.mu.Unlock()

	if qctx != nil {
		return qctx.Close()
	}
	return nil
}

func buildConfig(ctx context.Context) (*config.Config, error) {
	clientID := strings.TrimSpace(getEnvFirst("LONGBRIDGE_CLIENT_ID", "LONGPORT_CLIENT_ID"))
	if clientID == "" {
		cfg, err := config.New()
		if err != nil {
			return nil, err
		}
		cfg.SetLogger(newSDKLogger())
		return cfg, nil
	}

	o := oauth.New(clientID).OnOpenURL(func(url string) {
		fmt.Printf("Open this URL to authorize:\n%s\n", url)
	})

	if portText := strings.TrimSpace(os.Getenv("LONGBRIDGE_CALLBACK_PORT")); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, fmt.Errorf("invalid LONGBRIDGE_CALLBACK_PORT: %w", err)
		}
		o.WithCallbackPort(port)
	}

	if err := o.Build(ctx); err != nil {
		return nil, err
	}

	cfg, err := config.New(config.WithOAuthClient(o))
	if err != nil {
		return nil, err
	}
	cfg.SetLogger(newSDKLogger())
	return cfg, nil
}

func getEnvFirst(keys ...string) string {
	for _, key := range keys {
		if key == "" {
			continue
		}
		value := strings.TrimSpace(os.Getenv(key))
		if value != "" {
			return value
		}
	}
	return ""
}

type sdkLogger struct {
	inner *protocol.DefaultLogger
}

func newSDKLogger() *sdkLogger {
	return &sdkLogger{inner: &protocol.DefaultLogger{}}
}

func (l *sdkLogger) SetLevel(level string) {
	l.inner.SetLevel(level)
}

func (l *sdkLogger) Info(msg string) {
	l.inner.Info(msg)
}

func (l *sdkLogger) Error(msg string) {
	if isIgnorableSDKMessage(msg) {
		return
	}
	l.inner.Error(msg)
}

func (l *sdkLogger) Warn(msg string) {
	l.inner.Warn(msg)
}

func (l *sdkLogger) Debug(msg string) {
	l.inner.Debug(msg)
}

func (l *sdkLogger) Infof(msg string, args ...interface{}) {
	l.inner.Infof(msg, args...)
}

func (l *sdkLogger) Errorf(msg string, args ...interface{}) {
	formatted := fmt.Sprintf(msg, args...)
	if isIgnorableSDKMessage(formatted) {
		return
	}
	l.inner.Error(formatted)
}

func (l *sdkLogger) Warnf(msg string, args ...interface{}) {
	l.inner.Warnf(msg, args...)
}

func (l *sdkLogger) Debugf(msg string, args ...interface{}) {
	l.inner.Debugf(msg, args...)
}

func isIgnorableSDKMessage(msg string) bool {
	return strings.Contains(msg, "close conn, err: close by client")
}
