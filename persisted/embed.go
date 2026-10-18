// Package persisted holds the trusted documents served in allowlist mode
// (PERSISTED_QUERIES=allowlist): documents/*.graphql are the sources, and
// queries.json (SHA-256 → canonical document) is generated from them with
// `make persisted-queries` and compiled into the binary.
package persisted

import _ "embed"

// Manifest is queries.json: the allowlist of trusted documents.
//
//go:embed queries.json
var Manifest []byte
