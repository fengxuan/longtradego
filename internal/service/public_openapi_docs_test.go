package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadPublicOpenAPISpecIncludesCoreContract(t *testing.T) {
	spec, err := loadPublicOpenAPISpec()
	if err != nil {
		t.Fatalf("loadPublicOpenAPISpec failed: %v", err)
	}
	if !strings.Contains(string(spec.yaml), "openapi: 3.0.3") {
		t.Fatalf("expected yaml spec contains openapi version, got %s", string(spec.yaml))
	}

	var payload map[string]any
	if err := json.Unmarshal(spec.json, &payload); err != nil {
		t.Fatalf("decode json spec failed: %v", err)
	}

	openapiVersion, _ := payload["openapi"].(string)
	if strings.TrimSpace(openapiVersion) != "3.0.3" {
		t.Fatalf("expected json spec openapi=3.0.3, got %#v", payload["openapi"])
	}
	if strings.TrimSpace(spec.version) != "3.0.3" {
		t.Fatalf("expected cached openapi version=3.0.3, got %q", spec.version)
	}

	paths, ok := payload["paths"].(map[string]any)
	if !ok {
		t.Fatalf("expected paths section in public openapi spec")
	}
	for _, path := range []string{
		bookingPublicCatalogPath,
		bookingPublicReservationsPath,
		bookingPublicIntentParsePath,
		bookingPublicIntentConfirmPath,
		"/webhook/{routePath}",
	} {
		if _, exists := paths[path]; !exists {
			t.Fatalf("expected openapi paths contains %q", path)
		}
	}

	components, ok := payload["components"].(map[string]any)
	if !ok {
		t.Fatalf("expected components section in public openapi spec")
	}
	securitySchemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("expected securitySchemes section in public openapi spec")
	}
	if !testOpenAPISecurityHeaderName(t, securitySchemes, "ThirdPartyID", webhookHeaderThirdPartyID) {
		t.Fatalf("expected ThirdPartyID security scheme header name %q", webhookHeaderThirdPartyID)
	}
	if !testOpenAPISecurityHeaderName(t, securitySchemes, "WebhookTimestamp", webhookHeaderTimestamp) {
		t.Fatalf("expected WebhookTimestamp security scheme header name %q", webhookHeaderTimestamp)
	}
	if !testOpenAPISecurityHeaderName(t, securitySchemes, "WebhookToken", webhookHeaderSignature) {
		t.Fatalf("expected WebhookToken security scheme header name %q", webhookHeaderSignature)
	}
	if !testOpenAPISecurityHeaderName(t, securitySchemes, "IdempotencyKey", publicAPIHeaderIdempotencyKey) {
		t.Fatalf("expected IdempotencyKey security scheme header name %q", publicAPIHeaderIdempotencyKey)
	}
}

func testOpenAPISecurityHeaderName(t *testing.T, schemes map[string]any, schemeName string, expectedHeader string) bool {
	t.Helper()
	rawScheme, exists := schemes[schemeName]
	if !exists {
		return false
	}
	schemeObj, ok := rawScheme.(map[string]any)
	if !ok {
		return false
	}
	actualName, _ := schemeObj["name"].(string)
	return strings.TrimSpace(actualName) == expectedHeader
}

func TestRegisterPublicOpenAPIDocsRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerPublicOpenAPIDocsRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	homeResp, err := http.Get(server.URL + publicOpenAPIHomePath)
	if err != nil {
		t.Fatalf("GET portal home failed: %v", err)
	}
	if homeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(homeResp.Body)
		t.Fatalf("expected portal home 200, got %d body=%s", homeResp.StatusCode, string(body))
	}
	homeBody, _ := io.ReadAll(homeResp.Body)
	_ = homeResp.Body.Close()
	for _, snippet := range []string{
		"Longtradego Public API Portal",
		publicOpenAPIReferencePath,
		publicOpenAPISpecYAMLPath,
		publicOpenAPIGuideJSPath,
	} {
		if !strings.Contains(string(homeBody), snippet) {
			t.Fatalf("expected portal home contains %q", snippet)
		}
	}

	homeSlashResp, err := http.Get(server.URL + publicOpenAPIHomeSlashPath + "?from=test")
	if err != nil {
		t.Fatalf("GET portal home slash failed: %v", err)
	}
	if homeSlashResp.StatusCode != http.StatusOK {
		t.Fatalf("expected portal home slash final status 200, got %d", homeSlashResp.StatusCode)
	}
	if homeSlashResp.Request == nil || homeSlashResp.Request.URL == nil || homeSlashResp.Request.URL.Path != publicOpenAPIHomePath {
		t.Fatalf("expected portal home slash redirect target %q", publicOpenAPIHomePath)
	}
	if homeSlashResp.Request.URL.RawQuery != "from=test" {
		t.Fatalf("expected portal home slash redirect keeps query, got %q", homeSlashResp.Request.URL.RawQuery)
	}
	_ = homeSlashResp.Body.Close()

	yamlResp, err := http.Get(server.URL + publicOpenAPISpecYAMLPath)
	if err != nil {
		t.Fatalf("GET spec yaml failed: %v", err)
	}
	if yamlResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(yamlResp.Body)
		t.Fatalf("expected spec yaml 200, got %d body=%s", yamlResp.StatusCode, string(body))
	}
	if !strings.Contains(strings.ToLower(yamlResp.Header.Get("Content-Type")), "yaml") {
		t.Fatalf("expected spec yaml content-type includes yaml, got %q", yamlResp.Header.Get("Content-Type"))
	}
	_ = yamlResp.Body.Close()

	jsonResp, err := http.Get(server.URL + publicOpenAPISpecJSONPath)
	if err != nil {
		t.Fatalf("GET spec json failed: %v", err)
	}
	if jsonResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(jsonResp.Body)
		t.Fatalf("expected spec json 200, got %d body=%s", jsonResp.StatusCode, string(body))
	}
	var specPayload map[string]any
	if err := json.NewDecoder(jsonResp.Body).Decode(&specPayload); err != nil {
		t.Fatalf("decode spec json failed: %v", err)
	}
	_ = jsonResp.Body.Close()
	if strings.TrimSpace(specPayload["openapi"].(string)) != "3.0.3" {
		t.Fatalf("expected spec json openapi=3.0.3, got %#v", specPayload["openapi"])
	}

	referenceResp, err := http.Get(server.URL + publicOpenAPIReferencePath)
	if err != nil {
		t.Fatalf("GET reference page failed: %v", err)
	}
	if referenceResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(referenceResp.Body)
		t.Fatalf("expected reference page 200, got %d body=%s", referenceResp.StatusCode, string(body))
	}
	referenceBody, _ := io.ReadAll(referenceResp.Body)
	_ = referenceResp.Body.Close()
	for _, snippet := range []string{
		"Longtradego Public API Reference",
		"SwaggerUIBundle",
		publicOpenAPISpecYAMLPath,
	} {
		if !strings.Contains(string(referenceBody), snippet) {
			t.Fatalf("expected reference page contains %q", snippet)
		}
	}

	assetResp, err := http.Get(server.URL + publicOpenAPIReferencePrefix + "swagger-ui-bundle.js")
	if err != nil {
		t.Fatalf("GET reference asset failed: %v", err)
	}
	if assetResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(assetResp.Body)
		t.Fatalf("expected reference asset 200, got %d body=%s", assetResp.StatusCode, string(body))
	}
	_ = assetResp.Body.Close()

	guideResp, err := http.Get(server.URL + publicOpenAPIGuideJSPath)
	if err != nil {
		t.Fatalf("GET guide html failed: %v", err)
	}
	if guideResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(guideResp.Body)
		t.Fatalf("expected guide html 200, got %d body=%s", guideResp.StatusCode, string(body))
	}
	guideBody, _ := io.ReadAll(guideResp.Body)
	_ = guideResp.Body.Close()
	if !strings.Contains(string(guideBody), "Public API JS Integration Guide") {
		t.Fatalf("expected guide html contains title")
	}

	guideMDResp, err := http.Get(server.URL + publicOpenAPIGuideJSMDPath)
	if err != nil {
		t.Fatalf("GET guide markdown failed: %v", err)
	}
	if guideMDResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(guideMDResp.Body)
		t.Fatalf("expected guide markdown 200, got %d body=%s", guideMDResp.StatusCode, string(body))
	}
	if !strings.Contains(strings.ToLower(guideMDResp.Header.Get("Content-Type")), "markdown") {
		t.Fatalf("expected guide markdown content-type includes markdown, got %q", guideMDResp.Header.Get("Content-Type"))
	}
	guideMD, _ := io.ReadAll(guideMDResp.Body)
	_ = guideMDResp.Body.Close()
	if !strings.Contains(string(guideMD), "Public API JS Integration Guide") {
		t.Fatalf("expected guide markdown contains title")
	}

	legacyPaths := []string{
		legacyPublicOpenAPIDocsPath,
		legacyPublicOpenAPIYAMLPath,
		legacyPublicOpenAPIJSONPath,
		legacyPublicOpenAPIJSGuidPath,
		legacyPublicOpenAPIGuideJPath,
		legacyPublicDocsPath,
		legacyPublicDocsPrefix,
	}
	for _, path := range legacyPaths {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET legacy path %q failed: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected legacy path %q returns 404, got %d body=%s", path, resp.StatusCode, string(body))
		}
		_ = resp.Body.Close()
	}

	postReq, err := http.NewRequest(http.MethodPost, server.URL+publicOpenAPISpecYAMLPath, nil)
	if err != nil {
		t.Fatalf("build POST spec yaml request failed: %v", err)
	}
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST spec yaml endpoint failed: %v", err)
	}
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		body, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected spec yaml POST 405, got %d body=%s", postResp.StatusCode, string(body))
	}
	_ = postResp.Body.Close()
}
