package docs

import _ "embed"

//go:embed public_api_js_integration.md
var publicAPIJSIntegrationMarkdown []byte

// PublicAPIJSIntegrationMarkdown returns a copy of embedded JS integration guide bytes.
func PublicAPIJSIntegrationMarkdown() []byte {
	cloned := make([]byte, len(publicAPIJSIntegrationMarkdown))
	copy(cloned, publicAPIJSIntegrationMarkdown)
	return cloned
}
