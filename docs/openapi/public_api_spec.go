package openapi

import _ "embed"

//go:embed public_api.yaml
var publicAPISpecYAML []byte

// PublicAPISpecYAML returns a copy of embedded public OpenAPI YAML bytes.
func PublicAPISpecYAML() []byte {
	cloned := make([]byte, len(publicAPISpecYAML))
	copy(cloned, publicAPISpecYAML)
	return cloned
}
