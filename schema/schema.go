// Package schema embeds the normative configuration schema so the binary
// validates against exactly the document that ships in the repository, and the
// two can never drift.
package schema

import _ "embed"

// Config is config.schema.json embedded at build time. internal/config validates
// against these bytes; the file on disk stays the normative document.
//
//go:embed config.schema.json
var Config []byte
