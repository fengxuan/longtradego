package service

import (
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
