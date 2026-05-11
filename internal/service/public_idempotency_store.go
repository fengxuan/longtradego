package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	publicIdempotencyStateVersion = 1

	defaultPublicIdempotencyTTL = 24 * time.Hour

	bookingIdempotencyStateFile = "booking_idempotency.json"
	webhookIdempotencyStateFile = "webhook_idempotency.json"
)

type publicIdempotencyRecord struct {
	Key            string            `json:"key"`
	Route          string            `json:"route"`
	ThirdPartyID   string            `json:"third_party_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	BodyHash       string            `json:"body_hash"`
	StatusCode     int               `json:"status_code"`
	Headers        map[string]string `json:"headers,omitempty"`
	ResponseBody   string            `json:"response_body"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	ExpiresAt      string            `json:"expires_at"`
}

type publicIdempotencyState struct {
	Version int                                `json:"version"`
	Records map[string]publicIdempotencyRecord `json:"records"`
}

type publicIdempotencyReplay struct {
	StatusCode   int
	Headers      map[string]string
	ResponseBody []byte
}

type publicIdempotencyStore struct {
	path  string
	ttl   time.Duration
	nowFn func() time.Time

	mu sync.Mutex
}

func defaultBookingIdempotencyStatePath(runtimePath string) string {
	trimmedRuntime := strings.TrimSpace(runtimePath)
	if trimmedRuntime == "" {
		return resolveDataPath(bookingIdempotencyStateFile)
	}
	return filepath.Join(filepath.Dir(trimmedRuntime), bookingIdempotencyStateFile)
}

func defaultWebhookIdempotencyStatePath(runtimePath string) string {
	trimmedRuntime := strings.TrimSpace(runtimePath)
	if trimmedRuntime == "" {
		return resolveDataPath(webhookIdempotencyStateFile)
	}
	return filepath.Join(filepath.Dir(trimmedRuntime), webhookIdempotencyStateFile)
}

func newPublicIdempotencyStore(path string, ttl time.Duration) (*publicIdempotencyStore, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return nil, fmt.Errorf("idempotency store path is empty")
	}
	if ttl <= 0 {
		ttl = defaultPublicIdempotencyTTL
	}
	store := &publicIdempotencyStore{
		path:  trimmedPath,
		ttl:   ttl,
		nowFn: time.Now,
	}
	if err := store.ensureState(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *publicIdempotencyStore) ensureState() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readStateLocked()
	if err != nil {
		return err
	}
	changed := s.pruneExpiredLocked(&state, s.now())
	if !changed {
		return nil
	}
	return s.writeStateLocked(state)
}

func (s *publicIdempotencyStore) now() time.Time {
	if s == nil || s.nowFn == nil {
		return time.Now()
	}
	return s.nowFn()
}

func (s *publicIdempotencyStore) makeRecordKey(route string, thirdPartyID string, idempotencyKey string) string {
	return strings.ToLower(strings.TrimSpace(route)) + "\n" + normalizeThirdPartyID(thirdPartyID) + "\n" + strings.TrimSpace(idempotencyKey)
}

func (s *publicIdempotencyStore) bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *publicIdempotencyStore) Lookup(route string, thirdPartyID string, idempotencyKey string, body []byte) (publicIdempotencyReplay, bool, bool, error) {
	if s == nil {
		return publicIdempotencyReplay{}, false, false, nil
	}
	key := s.makeRecordKey(route, thirdPartyID, idempotencyKey)
	hash := s.bodyHash(body)

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readStateLocked()
	if err != nil {
		return publicIdempotencyReplay{}, false, false, err
	}
	changed := s.pruneExpiredLocked(&state, s.now())
	record, exists := state.Records[key]
	if changed {
		if writeErr := s.writeStateLocked(state); writeErr != nil {
			return publicIdempotencyReplay{}, false, false, writeErr
		}
	}
	if !exists {
		return publicIdempotencyReplay{}, false, false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(record.BodyHash), hash) {
		return publicIdempotencyReplay{}, false, true, nil
	}
	headers := make(map[string]string, len(record.Headers))
	for headerKey, headerValue := range record.Headers {
		headers[headerKey] = headerValue
	}
	return publicIdempotencyReplay{
		StatusCode:   record.StatusCode,
		Headers:      headers,
		ResponseBody: []byte(record.ResponseBody),
	}, true, false, nil
}

func (s *publicIdempotencyStore) Save(
	route string,
	thirdPartyID string,
	idempotencyKey string,
	body []byte,
	statusCode int,
	headers map[string]string,
	responseBody []byte,
) error {
	if s == nil {
		return nil
	}
	key := s.makeRecordKey(route, thirdPartyID, idempotencyKey)
	hash := s.bodyHash(body)
	now := s.now()
	expiresAt := now.Add(s.ttl)

	headerCopy := make(map[string]string, len(headers))
	for headerKey, headerValue := range headers {
		headerCopy[strings.TrimSpace(headerKey)] = strings.TrimSpace(headerValue)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readStateLocked()
	if err != nil {
		return err
	}
	s.pruneExpiredLocked(&state, now)
	if existing, exists := state.Records[key]; exists {
		if strings.EqualFold(strings.TrimSpace(existing.BodyHash), hash) {
			return nil
		}
	}
	if state.Records == nil {
		state.Records = make(map[string]publicIdempotencyRecord)
	}
	state.Records[key] = publicIdempotencyRecord{
		Key:            key,
		Route:          strings.TrimSpace(route),
		ThirdPartyID:   normalizeThirdPartyID(thirdPartyID),
		IdempotencyKey: strings.TrimSpace(idempotencyKey),
		BodyHash:       hash,
		StatusCode:     statusCode,
		Headers:        headerCopy,
		ResponseBody:   string(responseBody),
		CreatedAt:      now.Format(time.RFC3339Nano),
		UpdatedAt:      now.Format(time.RFC3339Nano),
		ExpiresAt:      expiresAt.Format(time.RFC3339Nano),
	}
	return s.writeStateLocked(state)
}

func (s *publicIdempotencyStore) readStateLocked() (publicIdempotencyState, error) {
	if s == nil {
		return publicIdempotencyState{}, fmt.Errorf("idempotency store is nil")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return publicIdempotencyState{
				Version: publicIdempotencyStateVersion,
				Records: map[string]publicIdempotencyRecord{},
			}, nil
		}
		return publicIdempotencyState{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return publicIdempotencyState{
			Version: publicIdempotencyStateVersion,
			Records: map[string]publicIdempotencyRecord{},
		}, nil
	}
	var state publicIdempotencyState
	if err := json.Unmarshal(raw, &state); err != nil {
		return publicIdempotencyState{}, err
	}
	if state.Version == 0 {
		state.Version = publicIdempotencyStateVersion
	}
	if state.Records == nil {
		state.Records = map[string]publicIdempotencyRecord{}
	}
	return state, nil
}

func (s *publicIdempotencyStore) writeStateLocked(state publicIdempotencyState) error {
	if s == nil {
		return fmt.Errorf("idempotency store is nil")
	}
	state.Version = publicIdempotencyStateVersion
	if state.Records == nil {
		state.Records = map[string]publicIdempotencyRecord{}
	}
	encoded, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(encoded, '\n'), 0o644)
}

func (s *publicIdempotencyStore) pruneExpiredLocked(state *publicIdempotencyState, now time.Time) bool {
	if state == nil || len(state.Records) == 0 {
		return false
	}
	changed := false
	for key, record := range state.Records {
		expiresAtText := strings.TrimSpace(record.ExpiresAt)
		if expiresAtText == "" {
			delete(state.Records, key)
			changed = true
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtText)
		if err != nil || !expiresAt.After(now) {
			delete(state.Records, key)
			changed = true
		}
	}
	return changed
}

func applyPublicIdempotencyMiddleware(
	handler http.HandlerFunc,
	store *publicIdempotencyStore,
	routePath string,
) http.HandlerFunc {
	if handler == nil {
		return nil
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r == nil {
			requestID := nextPublicRequestID(time.Now())
			_ = writePublicAPIError(w, requestID, newPublicAPIError(http.StatusBadRequest, publicAPIErrorCodeValidationError, "request is required", nil))
			return
		}
		if strings.ToUpper(strings.TrimSpace(r.Method)) != http.MethodPost || store == nil {
			handler(w, r)
			return
		}

		requestID := strings.TrimSpace(r.Header.Get(publicAPIHeaderRequestID))
		if requestID == "" {
			requestID = nextPublicRequestID(time.Now())
		}
		idempotencyKey := strings.TrimSpace(r.Header.Get(publicAPIHeaderIdempotencyKey))
		if idempotencyKey == "" {
			_ = writePublicAPIError(w, requestID, newPublicAPIError(
				http.StatusBadRequest,
				publicAPIErrorCodeIdempotencyKeyRequired,
				"idempotency key is required for POST requests",
				map[string]any{"header": publicAPIHeaderIdempotencyKey},
			))
			return
		}

		body, bodyErr := readWebhookBody(r.Body, defaultWebhookMaxBodyBytes)
		if bodyErr != nil {
			_ = writePublicAPIError(w, requestID, newPublicAPIError(
				http.StatusBadRequest,
				publicAPIErrorCodeInvalidJSONBody,
				strings.TrimSpace(bodyErr.Error()),
				nil,
			))
			return
		}
		r.Body = cloneReadCloserFromBytes(body)
		thirdPartyID := normalizeThirdPartyID(r.Header.Get(webhookHeaderThirdPartyID))

		replay, found, conflict, lookupErr := store.Lookup(routePath, thirdPartyID, idempotencyKey, body)
		if lookupErr != nil {
			_ = writePublicAPIError(w, requestID, newPublicAPIError(
				http.StatusInternalServerError,
				publicAPIErrorCodeIdempotencyStoreInternal,
				"idempotency lookup failed",
				map[string]any{"error": lookupErr.Error()},
			))
			return
		}
		if conflict {
			_ = writePublicAPIError(w, requestID, newPublicAPIError(
				http.StatusConflict,
				publicAPIErrorCodeIdempotencyKeyConflict,
				"idempotency key was already used with a different payload",
				nil,
			))
			return
		}
		if found {
			for headerKey, headerValue := range replay.Headers {
				if strings.TrimSpace(headerKey) == "" {
					continue
				}
				w.Header().Set(headerKey, headerValue)
			}
			if strings.TrimSpace(w.Header().Get(publicAPIHeaderRequestID)) == "" {
				w.Header().Set(publicAPIHeaderRequestID, requestID)
			}
			statusCode := replay.StatusCode
			if statusCode <= 0 {
				statusCode = http.StatusOK
			}
			w.WriteHeader(statusCode)
			_, _ = w.Write(replay.ResponseBody)
			return
		}

		capture := newPublicResponseCapture()
		handler(capture, r)
		if capture.status == 0 {
			capture.status = http.StatusOK
		}
		storeHeaders := map[string]string{
			"Content-Type":           strings.TrimSpace(capture.Header().Get("Content-Type")),
			publicAPIHeaderRequestID: strings.TrimSpace(capture.Header().Get(publicAPIHeaderRequestID)),
		}
		if saveErr := store.Save(routePath, thirdPartyID, idempotencyKey, body, capture.status, storeHeaders, capture.body); saveErr != nil {
			_ = writePublicAPIError(w, requestID, newPublicAPIError(
				http.StatusInternalServerError,
				publicAPIErrorCodeIdempotencyStoreInternal,
				"idempotency persist failed",
				map[string]any{"error": saveErr.Error()},
			))
			return
		}
		capture.FlushTo(w)
	}
}
