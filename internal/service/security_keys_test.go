package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndLoadSecurityTokenRecordsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "security_keys.json")
	input := map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "tok-a",
			Scopes:       []string{securityScopeWebhook, securityScopeBooking},
			CreatedAt:    "2026-05-03T00:00:00Z",
			UpdatedAt:    "2026-05-03T00:00:00Z",
		},
		"partner-b": {
			ThirdPartyID: "partner-b",
			Token:        "tok-b",
			Scopes:       []string{securityScopeBooking},
			CreatedAt:    "2026-05-03T00:00:00Z",
			UpdatedAt:    "2026-05-03T00:00:00Z",
		},
	}
	if err := writeSecurityTokenRecords(path, input); err != nil {
		t.Fatalf("writeSecurityTokenRecords failed: %v", err)
	}
	loaded, err := loadSecurityTokenRecords(path, false)
	if err != nil {
		t.Fatalf("loadSecurityTokenRecords failed: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 records, got %d", len(loaded))
	}
	if !securityRecordHasScope(loaded["partner-a"], securityScopeBooking) || !securityRecordHasScope(loaded["partner-a"], securityScopeWebhook) {
		t.Fatalf("expected partner-a includes booking+webhook scopes, got %+v", loaded["partner-a"])
	}
	if !securityRecordHasScope(loaded["partner-b"], securityScopeBooking) || securityRecordHasScope(loaded["partner-b"], securityScopeWebhook) {
		t.Fatalf("expected partner-b booking-only scope, got %+v", loaded["partner-b"])
	}
}

func TestWriteSecurityTokenRecordsRejectsDuplicateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "security_keys.json")
	input := map[string]webhookTokenRecord{
		"partner-a": {
			ThirdPartyID: "partner-a",
			Token:        "dup-token",
			Scopes:       []string{securityScopeBooking},
		},
		"partner-b": {
			ThirdPartyID: "partner-b",
			Token:        "dup-token",
			Scopes:       []string{securityScopeWebhook},
		},
	}
	err := writeSecurityTokenRecords(path, input)
	if err == nil {
		t.Fatalf("expected duplicate token validation error")
	}
	if !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate token error, got %v", err)
	}
}

func TestLoadSecurityTokenRecordsDoesNotMigrateLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, securityKeysConfigFile)
	legacyWebhookPath := filepath.Join(dir, webhookTokenStateFile)
	legacyBookingPath := filepath.Join(dir, bookingAPIKeysConfigFile)

	if err := os.WriteFile(legacyWebhookPath, []byte(`{
  "version": 1,
  "tokens": [
    {"third_party_id":"partner-a","token":"shared-token","created_at":"2026-05-03T00:00:00Z","updated_at":"2026-05-03T00:00:00Z"}
  ]
}`), 0o644); err != nil {
		t.Fatalf("write legacy webhook token file failed: %v", err)
	}
	if err := os.WriteFile(legacyBookingPath, []byte(`{"keys":["shared-token","booking-only-token"]}`), 0o644); err != nil {
		t.Fatalf("write legacy booking key file failed: %v", err)
	}

	if _, err := loadSecurityTokenRecords(target, false); err == nil {
		t.Fatalf("expected missing security_keys.json error when only legacy files exist")
	}
}

func TestResolveSecurityKeysPath(t *testing.T) {
	path, err := resolveSecurityKeysPath("", "", "token-store")
	if err != nil {
		t.Fatalf("resolveSecurityKeysPath default failed: %v", err)
	}
	if path != defaultSecurityKeysPath() {
		t.Fatalf("expected default security key path %s, got %s", defaultSecurityKeysPath(), path)
	}

	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	if _, err = resolveSecurityKeysPath("", legacyPath, "token-store"); err == nil {
		t.Fatalf("expected legacy token-store rejection")
	}

	if _, err := resolveSecurityKeysPath("/tmp/a.json", "/tmp/b.json", "token-store"); err == nil {
		t.Fatalf("expected token-store rejection")
	}
}
