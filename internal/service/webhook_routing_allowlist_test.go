package service

import (
	"strings"
	"testing"
)

func TestValidateWebhookDispatchCommandsRejectsLongbridge(t *testing.T) {
	err := validateWebhookDispatchCommands([][]string{{"longbridge", "quote", "AAPL.US"}}, false)
	if err == nil {
		t.Fatalf("expected longbridge command to be rejected in webhook downstream allowlist")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist rejection error, got: %v", err)
	}
}
