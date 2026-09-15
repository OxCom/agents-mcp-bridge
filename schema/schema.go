// Package schema embeds the normative configuration schema so the binary
// validates against exactly the document that ships in the repository, and the
// two can never drift.
package schema

import _ "embed"

//go:embed config.schema.json
var Config []byte
