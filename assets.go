// Package githint holds the files that the git-hint binary embeds.
package githint

import "embed"

// SchemasV01 holds the HINT v0.1 JSON Schemas (schemas/v0.1/*.schema.json).
//
//go:embed schemas/v0.1/*.schema.json
var SchemasV01 embed.FS

// OpenCodePlugin is the OpenCode adapter plugin that `git hint adapter install opencode`
// writes to .opencode/plugins/.
//
//go:embed adapters/opencode/hint-adapter.ts
var OpenCodePlugin []byte
