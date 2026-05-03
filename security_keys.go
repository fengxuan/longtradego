package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	securityKeysStateVersion = 1
	securityKeysConfigFile   = "security_keys.json"

	securityScopeBooking = "booking"
	securityScopeWebhook = "webhook"

	securityScopeBoth = "both"
)

type securityKeysState struct {
	Version int                  `json:"version"`
	Tokens  []webhookTokenRecord `json:"tokens"`
}

type legacyBookingAPIKeysConfig struct {
	Keys    []string `json:"keys"`
	APIKeys []string `json:"api_keys"`
}

func defaultSecurityKeysPath() string {
	return filepath.Join(daemonConfigDir, securityKeysConfigFile)
}

func normalizeSecurityScope(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case securityScopeBooking:
		return securityScopeBooking
	case securityScopeWebhook:
		return securityScopeWebhook
	default:
		return ""
	}
}

func normalizeSecurityScopes(scopes []string, defaultScopes []string) []string {
	merged := make([]string, 0, len(scopes)+len(defaultScopes))
	merged = append(merged, scopes...)
	if len(merged) == 0 {
		merged = append(merged, defaultScopes...)
	}
	seen := make(map[string]struct{}, len(merged))
	clean := make([]string, 0, len(merged))
	for _, scope := range merged {
		normalized := normalizeSecurityScope(scope)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		clean = append(clean, normalized)
	}
	if len(clean) == 0 {
		return []string{securityScopeBooking, securityScopeWebhook}
	}
	sort.Strings(clean)
	return clean
}

func validateSecurityScopes(scopes []string) error {
	for _, scope := range scopes {
		if normalizeSecurityScope(scope) == "" {
			return fmt.Errorf("invalid scope %q", strings.TrimSpace(scope))
		}
	}
	return nil
}

func mergeSecurityScopes(left []string, right []string) []string {
	merged := make([]string, 0, len(left)+len(right))
	merged = append(merged, left...)
	merged = append(merged, right...)
	return normalizeSecurityScopes(merged, nil)
}

func securityRecordHasScope(record webhookTokenRecord, scope string) bool {
	target := normalizeSecurityScope(scope)
	if target == "" {
		return false
	}
	scopes := normalizeSecurityScopes(record.Scopes, nil)
	for _, item := range scopes {
		if item == target {
			return true
		}
	}
	return false
}

func loadSecurityTokenRecords(path string, allowMissing bool) (map[string]webhookTokenRecord, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("security keys path is empty")
	}

	if err := migrateSecurityKeysFromLegacyIfNeeded(trimmedPath); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if allowMissing {
				return map[string]webhookTokenRecord{}, nil
			}
			return nil, fmt.Errorf("security keys config %s not found; copy conf-example/security_keys.json to %s and set at least one booking/webhook token", trimmedPath, trimmedPath)
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		if allowMissing {
			return map[string]webhookTokenRecord{}, nil
		}
		return nil, fmt.Errorf("security keys config %s is empty", trimmedPath)
	}

	records, parseErr := parseSecurityTokenRecords(raw)
	if parseErr == nil {
		return records, nil
	}

	legacyRecords, legacyErr := parseLegacySecurityTokenRecords(raw)
	if legacyErr != nil {
		return nil, fmt.Errorf("parse security keys config %s: %w", trimmedPath, parseErr)
	}
	if err := backupAndRewriteSecurityKeys(trimmedPath, raw, legacyRecords); err != nil {
		return nil, err
	}
	return legacyRecords, nil
}

func parseSecurityTokenRecords(raw []byte) (map[string]webhookTokenRecord, error) {
	var rawState map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawState); err != nil {
		return nil, err
	}
	if _, hasTokens := rawState["tokens"]; !hasTokens {
		return nil, fmt.Errorf("tokens field is required")
	}

	var state securityKeysState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}

	records := make(map[string]webhookTokenRecord, len(state.Tokens))
	tokenOwner := make(map[string]string, len(state.Tokens))
	for _, item := range state.Tokens {
		if len(item.Scopes) == 0 {
			return nil, fmt.Errorf("third_party_id %q: scopes is required", strings.TrimSpace(item.ThirdPartyID))
		}
		if len(item.Scopes) > 0 {
			if err := validateSecurityScopes(item.Scopes); err != nil {
				return nil, fmt.Errorf("third_party_id %q: %w", strings.TrimSpace(item.ThirdPartyID), err)
			}
		}
		record := webhookTokenRecord{
			ThirdPartyID: normalizeThirdPartyID(item.ThirdPartyID),
			Token:        strings.TrimSpace(item.Token),
			Scopes:       normalizeSecurityScopes(item.Scopes, nil),
			CreatedAt:    strings.TrimSpace(item.CreatedAt),
			UpdatedAt:    strings.TrimSpace(item.UpdatedAt),
		}
		if record.ThirdPartyID == "" || record.Token == "" {
			continue
		}
		if len(record.Scopes) == 0 {
			record.Scopes = []string{securityScopeBooking, securityScopeWebhook}
		}

		if ownerID, exists := tokenOwner[record.Token]; exists && ownerID != record.ThirdPartyID {
			return nil, fmt.Errorf("token %q is duplicated across third_party_id %q and %q", record.Token, ownerID, record.ThirdPartyID)
		}
		tokenOwner[record.Token] = record.ThirdPartyID

		existing, exists := records[record.ThirdPartyID]
		if exists {
			if existing.Token != record.Token {
				return nil, fmt.Errorf("third_party_id %q maps to multiple token values", record.ThirdPartyID)
			}
			existing.Scopes = mergeSecurityScopes(existing.Scopes, record.Scopes)
			if existing.CreatedAt == "" {
				existing.CreatedAt = record.CreatedAt
			}
			if record.UpdatedAt != "" {
				existing.UpdatedAt = record.UpdatedAt
			}
			records[record.ThirdPartyID] = existing
			continue
		}
		records[record.ThirdPartyID] = record
	}
	return records, nil
}

func parseLegacySecurityTokenRecords(raw []byte) (map[string]webhookTokenRecord, error) {
	if records, err := parseLegacyWebhookTokenRecords(raw); err == nil && len(records) > 0 {
		return records, nil
	}
	keys, err := parseLegacyBookingAPIKeyValues(raw)
	if err != nil {
		return nil, err
	}
	records := make(map[string]webhookTokenRecord, len(keys))
	nowText := time.Now().Format(time.RFC3339Nano)
	for index, token := range keys {
		records[fmt.Sprintf("booking-client-%d", index+1)] = webhookTokenRecord{
			ThirdPartyID: fmt.Sprintf("booking-client-%d", index+1),
			Token:        token,
			Scopes:       []string{securityScopeBooking},
			CreatedAt:    nowText,
			UpdatedAt:    nowText,
		}
	}
	return records, nil
}

func parseLegacyWebhookTokenRecords(raw []byte) (map[string]webhookTokenRecord, error) {
	var state struct {
		Tokens []webhookTokenRecord `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	records := make(map[string]webhookTokenRecord, len(state.Tokens))
	for _, item := range state.Tokens {
		thirdPartyID := normalizeThirdPartyID(item.ThirdPartyID)
		token := strings.TrimSpace(item.Token)
		if thirdPartyID == "" || token == "" {
			continue
		}
		records[thirdPartyID] = webhookTokenRecord{
			ThirdPartyID: thirdPartyID,
			Token:        token,
			Scopes:       normalizeSecurityScopes(item.Scopes, []string{securityScopeWebhook}),
			CreatedAt:    strings.TrimSpace(item.CreatedAt),
			UpdatedAt:    strings.TrimSpace(item.UpdatedAt),
		}
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("legacy webhook token config has no valid records")
	}
	return records, nil
}

func parseLegacyBookingAPIKeyValues(raw []byte) ([]string, error) {
	var cfg legacyBookingAPIKeysConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(cfg.Keys)+len(cfg.APIKeys))
	keys = append(keys, cfg.Keys...)
	keys = append(keys, cfg.APIKeys...)
	seen := make(map[string]struct{}, len(keys))
	clean := make([]string, 0, len(keys))
	for _, item := range keys {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		clean = append(clean, trimmed)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("legacy booking api keys config has no valid keys")
	}
	sort.Strings(clean)
	return clean, nil
}

func backupAndRewriteSecurityKeys(path string, oldRaw []byte, records map[string]webhookTokenRecord) error {
	suffix := strconv.FormatInt(time.Now().Unix(), 10)
	backupPath := fmt.Sprintf("%s.bak.%s", path, suffix)
	if err := writeFileAtomic(backupPath, oldRaw, 0o644); err != nil {
		return err
	}
	if err := writeSecurityTokenRecords(path, records); err != nil {
		return err
	}
	return nil
}

func writeSecurityTokenRecords(path string, records map[string]webhookTokenRecord) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("security keys path is empty")
	}

	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	state := securityKeysState{
		Version: securityKeysStateVersion,
		Tokens:  make([]webhookTokenRecord, 0, len(keys)),
	}
	tokenOwner := make(map[string]string, len(keys))
	for _, key := range keys {
		record := records[key]
		record.ThirdPartyID = normalizeThirdPartyID(key)
		record.Token = strings.TrimSpace(record.Token)
		if len(record.Scopes) > 0 {
			if err := validateSecurityScopes(record.Scopes); err != nil {
				return fmt.Errorf("third_party_id %q: %w", strings.TrimSpace(record.ThirdPartyID), err)
			}
		}
		record.Scopes = normalizeSecurityScopes(record.Scopes, nil)
		record.CreatedAt = strings.TrimSpace(record.CreatedAt)
		record.UpdatedAt = strings.TrimSpace(record.UpdatedAt)
		if record.ThirdPartyID == "" || record.Token == "" {
			continue
		}
		if len(record.Scopes) == 0 {
			record.Scopes = []string{securityScopeBooking, securityScopeWebhook}
		}
		if ownerID, exists := tokenOwner[record.Token]; exists && ownerID != record.ThirdPartyID {
			return fmt.Errorf("token %q is duplicated across third_party_id %q and %q", record.Token, ownerID, record.ThirdPartyID)
		}
		tokenOwner[record.Token] = record.ThirdPartyID
		state.Tokens = append(state.Tokens, record)
	}

	data, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	return writeFileAtomic(trimmedPath, append(data, '\n'), 0o644)
}

func migrateSecurityKeysFromLegacyIfNeeded(path string) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("security keys path is empty")
	}
	if _, err := os.Stat(trimmedPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dir := filepath.Dir(trimmedPath)
	legacyWebhookPath := filepath.Join(dir, webhookTokenStateFile)
	legacyBookingPath := filepath.Join(dir, bookingAPIKeysConfigFile)

	records := map[string]webhookTokenRecord{}
	tokenOwner := map[string]string{}
	hasLegacy := false
	nowText := time.Now().Format(time.RFC3339Nano)
	bookingCounter := 1

	if raw, err := os.ReadFile(legacyWebhookPath); err == nil {
		legacyRecords, parseErr := parseLegacyWebhookTokenRecords(raw)
		if parseErr != nil {
			return fmt.Errorf("parse legacy webhook tokens %s: %w", legacyWebhookPath, parseErr)
		}
		hasLegacy = true
		webhookIDs := make([]string, 0, len(legacyRecords))
		for id := range legacyRecords {
			webhookIDs = append(webhookIDs, id)
		}
		sort.Strings(webhookIDs)
		for _, id := range webhookIDs {
			record := legacyRecords[id]
			if err := mergeSecurityRecord(records, tokenOwner, record, []string{securityScopeWebhook}, nowText); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if raw, err := os.ReadFile(legacyBookingPath); err == nil {
		legacyKeys, parseErr := parseLegacyBookingAPIKeyValues(raw)
		if parseErr != nil {
			return fmt.Errorf("parse legacy booking api keys %s: %w", legacyBookingPath, parseErr)
		}
		hasLegacy = true
		for _, token := range legacyKeys {
			thirdPartyID := nextBookingLegacyThirdPartyID(records, &bookingCounter)
			record := webhookTokenRecord{
				ThirdPartyID: thirdPartyID,
				Token:        token,
				Scopes:       []string{securityScopeBooking},
				CreatedAt:    nowText,
				UpdatedAt:    nowText,
			}
			if err := mergeSecurityRecord(records, tokenOwner, record, []string{securityScopeBooking}, nowText); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if !hasLegacy {
		return nil
	}
	if len(records) == 0 {
		return nil
	}
	if err := writeSecurityTokenRecords(trimmedPath, records); err != nil {
		return err
	}

	suffix := strconv.FormatInt(time.Now().Unix(), 10)
	for _, legacyPath := range []string{legacyWebhookPath, legacyBookingPath} {
		if filepath.Clean(legacyPath) == filepath.Clean(trimmedPath) {
			continue
		}
		if _, err := os.Stat(legacyPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		backupPath := fmt.Sprintf("%s.bak.%s", legacyPath, suffix)
		if err := os.Rename(legacyPath, backupPath); err != nil {
			raw, readErr := os.ReadFile(legacyPath)
			if readErr != nil {
				return readErr
			}
			if writeErr := writeFileAtomic(backupPath, raw, 0o644); writeErr != nil {
				return writeErr
			}
			if removeErr := os.Remove(legacyPath); removeErr != nil {
				return removeErr
			}
		}
	}
	return nil
}

func mergeSecurityRecord(
	records map[string]webhookTokenRecord,
	tokenOwner map[string]string,
	record webhookTokenRecord,
	defaultScopes []string,
	nowText string,
) error {
	record.ThirdPartyID = normalizeThirdPartyID(record.ThirdPartyID)
	record.Token = strings.TrimSpace(record.Token)
	if len(record.Scopes) > 0 {
		if err := validateSecurityScopes(record.Scopes); err != nil {
			return fmt.Errorf("third_party_id %q: %w", strings.TrimSpace(record.ThirdPartyID), err)
		}
	}
	record.Scopes = normalizeSecurityScopes(record.Scopes, defaultScopes)
	record.CreatedAt = strings.TrimSpace(record.CreatedAt)
	record.UpdatedAt = strings.TrimSpace(record.UpdatedAt)
	if record.ThirdPartyID == "" || record.Token == "" {
		return nil
	}
	if len(record.Scopes) == 0 {
		record.Scopes = normalizeSecurityScopes(nil, defaultScopes)
	}
	if record.CreatedAt == "" {
		record.CreatedAt = nowText
	}
	if record.UpdatedAt == "" {
		record.UpdatedAt = nowText
	}

	if ownerID, exists := tokenOwner[record.Token]; exists {
		existing := records[ownerID]
		existing.Scopes = mergeSecurityScopes(existing.Scopes, record.Scopes)
		existing.UpdatedAt = record.UpdatedAt
		records[ownerID] = existing
		return nil
	}

	if existing, exists := records[record.ThirdPartyID]; exists {
		if existing.Token != record.Token {
			return fmt.Errorf("third_party_id %q maps to multiple token values", record.ThirdPartyID)
		}
		existing.Scopes = mergeSecurityScopes(existing.Scopes, record.Scopes)
		existing.UpdatedAt = record.UpdatedAt
		records[record.ThirdPartyID] = existing
		tokenOwner[existing.Token] = existing.ThirdPartyID
		return nil
	}

	records[record.ThirdPartyID] = record
	tokenOwner[record.Token] = record.ThirdPartyID
	return nil
}

func nextBookingLegacyThirdPartyID(records map[string]webhookTokenRecord, counter *int) string {
	if counter == nil {
		now := time.Now().Unix()
		return fmt.Sprintf("booking-client-%d", now)
	}
	for {
		candidate := fmt.Sprintf("booking-client-%d", *counter)
		*counter = *counter + 1
		if _, exists := records[candidate]; !exists {
			return candidate
		}
	}
}

func resolveSecurityKeysPath(securityKeysPath string, legacyPath string, legacyFlag string) (string, error) {
	preferred := strings.TrimSpace(securityKeysPath)
	legacy := strings.TrimSpace(legacyPath)
	switch {
	case preferred != "" && legacy != "":
		if filepath.Clean(preferred) != filepath.Clean(legacy) {
			return "", fmt.Errorf("security-keys and %s must point to the same path when both are set", legacyFlag)
		}
		return preferred, nil
	case preferred != "":
		return preferred, nil
	case legacy != "":
		return legacy, nil
	default:
		return defaultSecurityKeysPath(), nil
	}
}

func parseTokenScopeArgument(value string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", securityScopeBoth:
		return []string{securityScopeBooking, securityScopeWebhook}, nil
	case securityScopeBooking:
		return []string{securityScopeBooking}, nil
	case securityScopeWebhook:
		return []string{securityScopeWebhook}, nil
	default:
		return nil, fmt.Errorf("scope must be one of: both, booking, webhook")
	}
}
