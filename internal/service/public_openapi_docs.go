package service

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strings"
	"sync"

	swaggerFiles "github.com/swaggo/files/v2"
	"gopkg.in/yaml.v3"
	docsasset "longtradego/docs"
	publicopenapi "longtradego/docs/openapi"
)

const (
	publicOpenAPIRootPath        = "/openapi"
	publicOpenAPIVersionPath     = "/openapi/v1"
	publicOpenAPIHomePath        = publicOpenAPIVersionPath
	publicOpenAPIHomeSlashPath   = publicOpenAPIVersionPath + "/"
	publicOpenAPIReferencePath   = publicOpenAPIVersionPath + "/reference"
	publicOpenAPIReferencePrefix = publicOpenAPIVersionPath + "/reference/"
	publicOpenAPISpecYAMLPath    = publicOpenAPIVersionPath + "/spec.yaml"
	publicOpenAPISpecJSONPath    = publicOpenAPIVersionPath + "/spec.json"
	publicOpenAPIGuideJSPath     = publicOpenAPIVersionPath + "/guide/js"
	publicOpenAPIGuideJSMDPath   = publicOpenAPIVersionPath + "/guide/js.md"

	legacyPublicOpenAPIDocsPath   = publicOpenAPIVersionPath + "/docs"
	legacyPublicOpenAPIDocsPrefix = publicOpenAPIVersionPath + "/docs/"
	legacyPublicOpenAPIYAMLPath   = publicOpenAPIVersionPath + "/public_api.yaml"
	legacyPublicOpenAPIJSONPath   = publicOpenAPIVersionPath + "/public_api.json"
	legacyPublicOpenAPIJSGuidPath = publicOpenAPIVersionPath + "/public_api_js_integration.md"
	legacyPublicOpenAPIGuideJPath = legacyPublicOpenAPIDocsPath + "/public_api_js_integration.md"
	legacyPublicDocsPath          = "/docs"
	legacyPublicDocsPrefix        = "/docs/"
)

type publicOpenAPISpec struct {
	yaml    []byte
	json    []byte
	version string
}

type publicOpenAPIPortalView struct {
	Version      string
	SpecYAMLPath string
	SpecJSONPath string
	ReferenceURL string
	GuideJSURL   string
	GuideJSMDURL string
}

type publicOpenAPIReferenceView struct {
	HomeURL      string
	SpecYAMLPath string
	SpecJSONPath string
}

type publicOpenAPIGuideView struct {
	HomeURL      string
	ReferenceURL string
	GuideHTML    template.HTML
}

var (
	publicOpenAPISpecCache struct {
		once sync.Once
		spec publicOpenAPISpec
		err  error
	}

	publicOpenAPIPortalTemplate = template.Must(template.New("public_openapi_portal").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Longtradego Public API Portal</title>
  <style>
    :root {
      --bg-main: #f4fbff;
      --bg-glow: radial-gradient(circle at 20% 20%, rgba(14, 116, 144, 0.22), transparent 40%), radial-gradient(circle at 80% 5%, rgba(3, 105, 161, 0.2), transparent 36%), linear-gradient(165deg, #f6fbff 0%, #edf7ff 42%, #f7fbff 100%);
      --panel: #ffffff;
      --panel-alt: #f1f9ff;
      --text-main: #0f172a;
      --text-muted: #4b5563;
      --line: #c7e0f1;
      --brand: #0f766e;
      --brand-strong: #0369a1;
      --shadow: 0 24px 50px rgba(10, 37, 64, 0.14);
      --radius: 20px;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      background: var(--bg-main);
      background-image: var(--bg-glow);
      color: var(--text-main);
      font-family: "Space Grotesk", "Avenir Next", "Segoe UI", sans-serif;
      line-height: 1.55;
    }
    .shell {
      width: min(1100px, 94vw);
      margin: 0 auto;
      padding: 28px 0 46px;
    }
    .hero {
      border: 1px solid var(--line);
      background: linear-gradient(135deg, rgba(15, 118, 110, 0.09), rgba(3, 105, 161, 0.09));
      border-radius: calc(var(--radius) + 4px);
      box-shadow: var(--shadow);
      padding: 30px 32px;
      animation: rise 360ms ease-out both;
    }
    .hero h1 {
      margin: 0;
      font-size: clamp(1.75rem, 3.2vw, 2.5rem);
      letter-spacing: 0.015em;
    }
    .hero p {
      margin: 12px 0 0;
      color: var(--text-muted);
      max-width: 68ch;
    }
    .hero-meta {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
      margin-top: 18px;
    }
    .chip {
      border: 1px solid rgba(12, 74, 110, 0.22);
      background: rgba(255, 255, 255, 0.76);
      color: #0c4a6e;
      border-radius: 999px;
      padding: 5px 10px;
      font-size: 0.82rem;
      font-weight: 620;
    }
    .grid {
      margin-top: 20px;
      display: grid;
      gap: 14px;
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .card {
      border: 1px solid var(--line);
      border-radius: var(--radius);
      background: var(--panel);
      padding: 18px 18px 16px;
      box-shadow: 0 12px 22px rgba(9, 49, 77, 0.08);
      animation: rise 420ms ease-out both;
    }
    .card h2 {
      margin: 0;
      font-size: 1.08rem;
      letter-spacing: 0.02em;
    }
    .card p {
      margin: 9px 0 0;
      color: var(--text-muted);
      font-size: 0.95rem;
    }
    .links {
      margin: 11px 0 0;
      display: grid;
      gap: 7px;
    }
    .links a {
      color: var(--brand-strong);
      text-decoration: none;
      font-weight: 620;
      word-break: break-all;
    }
    .links a:hover { text-decoration: underline; }
    pre {
      margin: 11px 0 0;
      padding: 11px 12px;
      border-radius: 12px;
      border: 1px solid #d5e8f6;
      background: var(--panel-alt);
      overflow-x: auto;
      font: 0.82rem/1.45 "IBM Plex Mono", "SFMono-Regular", Menlo, monospace;
      color: #0f172a;
    }
    ul {
      margin: 10px 0 0;
      padding-left: 18px;
      color: var(--text-muted);
    }
    code {
      font-family: "IBM Plex Mono", "SFMono-Regular", Menlo, monospace;
      font-size: 0.88em;
      background: rgba(12, 74, 110, 0.09);
      padding: 1px 5px;
      border-radius: 6px;
    }
    @keyframes rise {
      from { opacity: 0; transform: translateY(8px); }
      to { opacity: 1; transform: translateY(0); }
    }
    @media (max-width: 880px) {
      .hero { padding: 22px; }
      .grid { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <main class="shell">
    <section class="hero">
      <h1>Longtradego Public API Portal</h1>
      <p>
        Third-party integration hub for Booking and Webhook public APIs.
        Use this portal to start quickly, review signing rules, and access machine-readable specifications.
      </p>
      <div class="hero-meta">
        <span class="chip">OpenAPI {{ .Version }}</span>
        <span class="chip">Audience: Third-party backend integrators</span>
        <span class="chip">Admin APIs excluded</span>
      </div>
    </section>

    <section class="grid">
      <article class="card">
        <h2>Quick Start: Booking</h2>
        <p>Two-step reservation flow for AI or application clients.</p>
        <pre>POST /booking/intents/parse
POST /booking/intents/confirm</pre>
      </article>
      <article class="card">
        <h2>Quick Start: Webhook</h2>
        <p>Send signed events to your configured route path.</p>
        <pre>POST /webhook/{routePath}
status: 200 (sync) / 202 (async)</pre>
      </article>
      <article class="card">
        <h2>Docs Hub</h2>
        <p>Canonical entry points for contract and implementation guidance.</p>
        <div class="links">
          <a href="{{ .ReferenceURL }}">API Reference (Swagger UI)</a>
          <a href="{{ .SpecYAMLPath }}">OpenAPI Spec (YAML)</a>
          <a href="{{ .SpecJSONPath }}">OpenAPI Spec (JSON)</a>
          <a href="{{ .GuideJSURL }}">JS Integration Guide (HTML)</a>
          <a href="{{ .GuideJSMDURL }}">JS Integration Guide (Markdown)</a>
        </div>
      </article>
      <article class="card">
        <h2>Auth & Signing</h2>
        <ul>
          <li>Required headers: <code>X-Third-Party-ID</code>, <code>X-Webhook-Timestamp</code>, <code>X-Webhook-Token</code></li>
          <li>POST requires: <code>Idempotency-Key</code></li>
          <li>Timestamp window: ±5 minutes</li>
          <li>Sign exact outgoing body bytes</li>
        </ul>
      </article>
      <article class="card">
        <h2>Error Playbook</h2>
        <ul>
          <li><code>timestamp_outside_allowed_window</code>: sync clock and re-sign</li>
          <li><code>signature_verification_failed</code>: verify exact payload bytes</li>
          <li><code>idempotency_key_conflict</code>: use a new key for changed payload</li>
          <li><code>token_scope_not_allowed</code>: ensure scope matches booking/webhook target</li>
        </ul>
      </article>
      <article class="card">
        <h2>Support Trace</h2>
        <p>Always capture <code>X-Request-ID</code> and response <code>code</code> for fast provider-side troubleshooting.</p>
      </article>
    </section>
  </main>
</body>
</html>`))

	publicOpenAPIReferenceTemplate = template.Must(template.New("public_openapi_reference").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Longtradego Public API Reference</title>
  <link rel="stylesheet" type="text/css" href="` + publicOpenAPIReferencePrefix + `swagger-ui.css">
  <link rel="icon" type="image/png" href="` + publicOpenAPIReferencePrefix + `favicon-32x32.png" sizes="32x32">
  <link rel="icon" type="image/png" href="` + publicOpenAPIReferencePrefix + `favicon-16x16.png" sizes="16x16">
  <style>
    body {
      margin: 0;
      background: #f7fbff;
      font-family: "Space Grotesk", "Avenir Next", "Segoe UI", sans-serif;
    }
    .topbar {
      position: sticky;
      top: 0;
      z-index: 3;
      display: flex;
      gap: 12px;
      align-items: center;
      padding: 11px 14px;
      border-bottom: 1px solid #c8dfef;
      background: #ffffff;
      color: #0f172a;
      box-shadow: 0 8px 18px rgba(12, 74, 110, 0.09);
    }
    .topbar a {
      color: #0369a1;
      text-decoration: none;
      font-weight: 620;
    }
    .topbar a:hover { text-decoration: underline; }
  </style>
</head>
<body>
  <div class="topbar">
    <a href="{{ .HomeURL }}">Portal Home</a>
    <a href="{{ .SpecYAMLPath }}">Spec YAML</a>
    <a href="{{ .SpecJSONPath }}">Spec JSON</a>
  </div>
  <div id="swagger-ui"></div>
  <script src="` + publicOpenAPIReferencePrefix + `swagger-ui-bundle.js" charset="UTF-8"></script>
  <script src="` + publicOpenAPIReferencePrefix + `swagger-ui-standalone-preset.js" charset="UTF-8"></script>
  <script>
    window.onload = function () {
      window.ui = SwaggerUIBundle({
        url: "{{ .SpecYAMLPath }}",
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        plugins: [
          SwaggerUIBundle.plugins.DownloadUrl
        ],
        layout: "StandaloneLayout"
      });
    };
  </script>
</body>
</html>`))

	publicOpenAPIGuideTemplate = template.Must(template.New("public_openapi_guide").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Public API JS Integration Guide</title>
  <style>
    :root {
      --bg: #f5fbff;
      --panel: #ffffff;
      --line: #c9e1f2;
      --text: #0f172a;
      --muted: #526173;
      --link: #0369a1;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: radial-gradient(circle at top right, rgba(14, 116, 144, 0.16), transparent 30%), var(--bg);
      color: var(--text);
      font-family: "Space Grotesk", "Avenir Next", "Segoe UI", sans-serif;
      line-height: 1.6;
    }
    .shell {
      width: min(1000px, 94vw);
      margin: 0 auto;
      padding: 26px 0 34px;
    }
    .topbar {
      display: flex;
      gap: 12px;
      align-items: center;
      flex-wrap: wrap;
      border: 1px solid var(--line);
      border-radius: 14px;
      background: var(--panel);
      padding: 10px 12px;
      box-shadow: 0 8px 16px rgba(15, 23, 42, 0.08);
    }
    .topbar a {
      color: var(--link);
      text-decoration: none;
      font-weight: 620;
    }
    .topbar a:hover { text-decoration: underline; }
    .article {
      margin-top: 16px;
      border: 1px solid var(--line);
      border-radius: 20px;
      background: var(--panel);
      padding: 22px;
      box-shadow: 0 16px 34px rgba(15, 23, 42, 0.11);
    }
    .article h1, .article h2, .article h3 {
      margin: 18px 0 8px;
      letter-spacing: 0.01em;
    }
    .article h1 { margin-top: 0; font-size: 2rem; }
    .article h2 { font-size: 1.35rem; }
    .article h3 { font-size: 1.1rem; color: #155e75; }
    .article p { margin: 9px 0; color: var(--muted); }
    .article ul { margin: 8px 0 12px; padding-left: 18px; color: var(--muted); }
    .article li { margin: 4px 0; }
    .article code {
      font-family: "IBM Plex Mono", "SFMono-Regular", Menlo, monospace;
      font-size: 0.88em;
      background: rgba(12, 74, 110, 0.1);
      border-radius: 6px;
      padding: 1px 5px;
      color: #155e75;
    }
    .article pre {
      margin: 10px 0;
      padding: 12px;
      border-radius: 12px;
      border: 1px solid #d8e8f6;
      background: #eef7ff;
      overflow-x: auto;
      font: 0.84rem/1.5 "IBM Plex Mono", "SFMono-Regular", Menlo, monospace;
      color: #0f172a;
    }
  </style>
</head>
<body>
  <main class="shell">
    <nav class="topbar">
      <a href="{{ .HomeURL }}">Portal Home</a>
      <a href="{{ .ReferenceURL }}">API Reference</a>
      <a href="` + publicOpenAPIGuideJSMDPath + `">Download Markdown</a>
    </nav>
    <article class="article">{{ .GuideHTML }}</article>
  </main>
</body>
</html>`))
)

func loadPublicOpenAPISpec() (publicOpenAPISpec, error) {
	publicOpenAPISpecCache.once.Do(func() {
		rawYAML := bytes.TrimSpace(publicopenapi.PublicAPISpecYAML())
		if len(rawYAML) == 0 {
			publicOpenAPISpecCache.err = fmt.Errorf("public openapi yaml is empty")
			return
		}

		normalizedYAML := append([]byte(nil), rawYAML...)
		if normalizedYAML[len(normalizedYAML)-1] != '\n' {
			normalizedYAML = append(normalizedYAML, '\n')
		}

		var payload map[string]any
		if err := yaml.Unmarshal(rawYAML, &payload); err != nil {
			publicOpenAPISpecCache.err = fmt.Errorf("parse public openapi yaml: %w", err)
			return
		}

		encodedJSON, err := marshalJSONIndentNoHTMLEscape(payload, "", defaultWebhookResponseIndent)
		if err != nil {
			publicOpenAPISpecCache.err = fmt.Errorf("encode public openapi json: %w", err)
			return
		}
		encodedJSON = append(encodedJSON, '\n')

		versionText := "3.0.3"
		if rawVersion, ok := payload["openapi"].(string); ok && strings.TrimSpace(rawVersion) != "" {
			versionText = strings.TrimSpace(rawVersion)
		}

		publicOpenAPISpecCache.spec = publicOpenAPISpec{
			yaml:    normalizedYAML,
			json:    encodedJSON,
			version: versionText,
		}
	})
	return publicOpenAPISpecCache.spec, publicOpenAPISpecCache.err
}

func isReservedPublicDocsPath(path string) bool {
	candidate := strings.TrimSpace(path)
	if candidate == "" {
		return false
	}
	return pathEqualsOrHasPrefix(candidate, publicOpenAPIRootPath) || pathEqualsOrHasPrefix(candidate, legacyPublicDocsPath)
}

func pathEqualsOrHasPrefix(path string, prefix string) bool {
	trimmedPath := strings.TrimSpace(path)
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPath == "" || trimmedPrefix == "" {
		return false
	}
	if trimmedPath == trimmedPrefix {
		return true
	}
	return strings.HasPrefix(trimmedPath, trimmedPrefix+"/")
}

func registerPublicOpenAPIDocsRoutes(mux *http.ServeMux) {
	if mux == nil {
		return
	}

	writeMethodNotAllowed := func(w http.ResponseWriter) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	writeNotFound := func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}
	redirectToHome := func(w http.ResponseWriter, r *http.Request) {
		target := publicOpenAPIHomePath
		if rawQuery := strings.TrimSpace(r.URL.RawQuery); rawQuery != "" {
			target += "?" + rawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	}
	serveSpec := func(w http.ResponseWriter, r *http.Request, contentType string, getter func(publicOpenAPISpec) []byte) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		spec, err := loadPublicOpenAPISpec()
		if err != nil {
			http.Error(w, fmt.Sprintf("load openapi document failed: %v", err), http.StatusInternalServerError)
			return
		}
		payload := getter(spec)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
	renderPortal := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		spec, err := loadPublicOpenAPISpec()
		if err != nil {
			http.Error(w, fmt.Sprintf("load openapi document failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = publicOpenAPIPortalTemplate.Execute(w, publicOpenAPIPortalView{
			Version:      spec.version,
			SpecYAMLPath: publicOpenAPISpecYAMLPath,
			SpecJSONPath: publicOpenAPISpecJSONPath,
			ReferenceURL: publicOpenAPIReferencePath,
			GuideJSURL:   publicOpenAPIGuideJSPath,
			GuideJSMDURL: publicOpenAPIGuideJSMDPath,
		})
	}
	renderReference := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = publicOpenAPIReferenceTemplate.Execute(w, publicOpenAPIReferenceView{
			HomeURL:      publicOpenAPIHomePath,
			SpecYAMLPath: publicOpenAPISpecYAMLPath,
			SpecJSONPath: publicOpenAPISpecJSONPath,
		})
	}
	serveGuideMarkdown := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		payload := docsasset.PublicAPIJSIntegrationMarkdown()
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
	renderGuideHTML := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		payload := docsasset.PublicAPIJSIntegrationMarkdown()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = publicOpenAPIGuideTemplate.Execute(w, publicOpenAPIGuideView{
			HomeURL:      publicOpenAPIHomePath,
			ReferenceURL: publicOpenAPIReferencePath,
			GuideHTML:    renderGuideMarkdown(payload),
		})
	}

	staticReferenceAssets := http.StripPrefix(publicOpenAPIReferencePrefix, http.FileServer(http.FS(swaggerFiles.FS)))

	mux.HandleFunc(publicOpenAPIHomePath, renderPortal)
	mux.HandleFunc(publicOpenAPIHomeSlashPath, redirectToHome)
	mux.HandleFunc(publicOpenAPISpecYAMLPath, func(w http.ResponseWriter, r *http.Request) {
		serveSpec(w, r, "application/yaml; charset=utf-8", func(spec publicOpenAPISpec) []byte {
			return spec.yaml
		})
	})
	mux.HandleFunc(publicOpenAPISpecJSONPath, func(w http.ResponseWriter, r *http.Request) {
		serveSpec(w, r, "application/json; charset=utf-8", func(spec publicOpenAPISpec) []byte {
			return spec.json
		})
	})
	mux.HandleFunc(publicOpenAPIReferencePath, renderReference)
	mux.HandleFunc(publicOpenAPIReferencePrefix, func(w http.ResponseWriter, r *http.Request) {
		trimmedPath := strings.TrimSpace(r.URL.Path)
		if trimmedPath == publicOpenAPIReferencePrefix || trimmedPath == publicOpenAPIReferencePrefix+"index.html" {
			renderReference(w, r)
			return
		}
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		staticReferenceAssets.ServeHTTP(w, r)
	})
	mux.HandleFunc(publicOpenAPIGuideJSPath, renderGuideHTML)
	mux.HandleFunc(publicOpenAPIGuideJSMDPath, serveGuideMarkdown)

	legacy404Exact := []string{
		legacyPublicOpenAPIDocsPath,
		legacyPublicOpenAPIYAMLPath,
		legacyPublicOpenAPIJSONPath,
		legacyPublicOpenAPIJSGuidPath,
		legacyPublicOpenAPIGuideJPath,
		legacyPublicDocsPath,
	}
	for _, path := range legacy404Exact {
		legacyPath := path
		mux.HandleFunc(legacyPath, writeNotFound)
	}
	legacy404Prefixes := []string{
		legacyPublicOpenAPIDocsPrefix,
		legacyPublicDocsPrefix,
	}
	for _, prefix := range legacy404Prefixes {
		legacyPrefix := prefix
		mux.HandleFunc(legacyPrefix, writeNotFound)
	}
}

func renderGuideMarkdown(raw []byte) template.HTML {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var builder strings.Builder
	builder.WriteString(`<div class="guide-markdown">`)

	inCodeBlock := false
	inList := false
	inTableBlock := false

	closeList := func() {
		if inList {
			builder.WriteString(`</ul>`)
			inList = false
		}
	}
	closeTableBlock := func() {
		if inTableBlock {
			builder.WriteString(`</pre>`)
			inTableBlock = false
		}
	}

	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\t ")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			closeTableBlock()
			closeList()
			if inCodeBlock {
				builder.WriteString(`</code></pre>`)
				inCodeBlock = false
			} else {
				builder.WriteString(`<pre><code>`)
				inCodeBlock = true
			}
			continue
		}
		if inCodeBlock {
			builder.WriteString(html.EscapeString(rawLine))
			builder.WriteByte('\n')
			continue
		}

		if strings.HasPrefix(trimmed, "|") {
			closeList()
			if !inTableBlock {
				builder.WriteString(`<pre>`)
				inTableBlock = true
			}
			builder.WriteString(html.EscapeString(trimmed))
			builder.WriteByte('\n')
			continue
		}
		closeTableBlock()

		if trimmed == "" {
			closeList()
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "# "):
			closeList()
			builder.WriteString(`<h1>`)
			builder.WriteString(renderGuideInlineText(strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))))
			builder.WriteString(`</h1>`)
		case strings.HasPrefix(trimmed, "## "):
			closeList()
			builder.WriteString(`<h2>`)
			builder.WriteString(renderGuideInlineText(strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))))
			builder.WriteString(`</h2>`)
		case strings.HasPrefix(trimmed, "### "):
			closeList()
			builder.WriteString(`<h3>`)
			builder.WriteString(renderGuideInlineText(strings.TrimSpace(strings.TrimPrefix(trimmed, "### "))))
			builder.WriteString(`</h3>`)
		case strings.HasPrefix(trimmed, "- "):
			if !inList {
				builder.WriteString(`<ul>`)
				inList = true
			}
			builder.WriteString(`<li>`)
			builder.WriteString(renderGuideInlineText(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
			builder.WriteString(`</li>`)
		default:
			closeList()
			builder.WriteString(`<p>`)
			builder.WriteString(renderGuideInlineText(trimmed))
			builder.WriteString(`</p>`)
		}
	}

	closeTableBlock()
	closeList()
	if inCodeBlock {
		builder.WriteString(`</code></pre>`)
	}
	builder.WriteString(`</div>`)
	return template.HTML(builder.String())
}

func renderGuideInlineText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	var builder strings.Builder
	var segment strings.Builder
	inCode := false

	flushSegment := func() {
		if segment.Len() == 0 {
			return
		}
		builder.WriteString(html.EscapeString(segment.String()))
		segment.Reset()
	}

	for _, r := range trimmed {
		if r != '`' {
			segment.WriteRune(r)
			continue
		}
		flushSegment()
		if inCode {
			builder.WriteString(`</code>`)
			inCode = false
		} else {
			builder.WriteString(`<code>`)
			inCode = true
		}
	}
	flushSegment()
	if inCode {
		builder.WriteString(`</code>`)
	}

	return builder.String()
}
