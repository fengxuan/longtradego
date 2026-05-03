package main

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

func TestLoadSecurityTokenRecordsMigratesLegacyFiles(t *testing.T) {
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

	records, err := loadSecurityTokenRecords(target, false)
	if err != nil {
		t.Fatalf("loadSecurityTokenRecords migrate failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 merged records after migration, got %d", len(records))
	}
	shared, ok := records["partner-a"]
	if !ok {
		t.Fatalf("expected migrated shared token owned by partner-a, got %+v", records)
	}
	if !securityRecordHasScope(shared, securityScopeWebhook) || !securityRecordHasScope(shared, securityScopeBooking) {
		t.Fatalf("expected merged scopes for shared token, got %+v", shared)
	}
	bookingOnly, ok := records["booking-client-1"]
	if !ok {
		t.Fatalf("expected booking-client-1 migrated token, got %+v", records)
	}
	if !securityRecordHasScope(bookingOnly, securityScopeBooking) || securityRecordHasScope(bookingOnly, securityScopeWebhook) {
		t.Fatalf("expected booking-client-1 booking-only scope, got %+v", bookingOnly)
	}

	if _, err := os.Stat(legacyWebhookPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy webhook file moved to backup, stat err=%v", err)
	}
	if _, err := os.Stat(legacyBookingPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy booking file moved to backup, stat err=%v", err)
	}
	webhookBaks, err := filepath.Glob(legacyWebhookPath + ".bak.*")
	if err != nil {
		t.Fatalf("glob webhook backup failed: %v", err)
	}
	if len(webhookBaks) == 0 {
		t.Fatalf("expected webhook legacy backup file created")
	}
	bookingBaks, err := filepath.Glob(legacyBookingPath + ".bak.*")
	if err != nil {
		t.Fatalf("glob booking backup failed: %v", err)
	}
	if len(bookingBaks) == 0 {
		t.Fatalf("expected booking legacy backup file created")
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
	path, err = resolveSecurityKeysPath("", legacyPath, "token-store")
	if err != nil {
		t.Fatalf("resolveSecurityKeysPath legacy failed: %v", err)
	}
	if path != legacyPath {
		t.Fatalf("expected legacy path %s, got %s", legacyPath, path)
	}

	if _, err := resolveSecurityKeysPath("/tmp/a.json", "/tmp/b.json", "token-store"); err == nil {
		t.Fatalf("expected conflict error when security-keys and token-store differ")
	}
}
