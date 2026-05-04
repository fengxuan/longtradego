package service

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPublicIdempotencyStoreSaveLookupAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotency.json")
	store, err := newPublicIdempotencyStore(path, time.Hour)
	if err != nil {
		t.Fatalf("newPublicIdempotencyStore failed: %v", err)
	}

	route := "/booking/intents/parse"
	thirdPartyID := "partner-a"
	key := "idem-1"
	body := []byte(`{"a":1}`)
	responseBody := []byte(`{"status":"ok","code":"ok"}`)
	headers := map[string]string{
		"Content-Type":           "application/json",
		publicAPIHeaderRequestID: "req-1",
	}

	if err := store.Save(route, thirdPartyID, key, body, 200, headers, responseBody); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	replay, found, conflict, err := store.Lookup(route, thirdPartyID, key, body)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !found || conflict {
		t.Fatalf("expected found replay without conflict, got found=%v conflict=%v", found, conflict)
	}
	if replay.StatusCode != 200 || string(replay.ResponseBody) != string(responseBody) {
		t.Fatalf("unexpected replay payload: %+v", replay)
	}

	_, found, conflict, err = store.Lookup(route, thirdPartyID, key, []byte(`{"a":2}`))
	if err != nil {
		t.Fatalf("Lookup conflict failed: %v", err)
	}
	if found || !conflict {
		t.Fatalf("expected conflict for different payload with same key, got found=%v conflict=%v", found, conflict)
	}
}

func TestPublicIdempotencyStoreExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotency_expiry.json")
	store, err := newPublicIdempotencyStore(path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("newPublicIdempotencyStore failed: %v", err)
	}

	if err := store.Save("/webhook/events", "partner-a", "idem-expire", []byte(`{"x":1}`), 200, nil, []byte(`{"status":"ok"}`)); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	time.Sleep(80 * time.Millisecond)

	_, found, conflict, err := store.Lookup("/webhook/events", "partner-a", "idem-expire", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if found || conflict {
		t.Fatalf("expected expired record not found, got found=%v conflict=%v", found, conflict)
	}
}
