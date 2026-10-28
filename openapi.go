// Package vodlocal menyediakan artefak tingkat repo yang perlu di-embed ke
// binary, saat ini spesifikasi OpenAPI.
package vodlocal

import _ "embed"

// OpenAPISpec adalah isi openapi.json di akar repo.
//
//go:embed openapi.json
var OpenAPISpec []byte
