// Package githint holds the files that the git-hint binary embeds.
package githint

import "embed"

// SchemasV01 holds the HINT v0.1 JSON Schemas (schemas/v0.1/*.schema.json).
//
//go:embed schemas/v0.1/*.schema.json
var SchemasV01 embed.FS

// SchemasV02 holds the HINT v0.2 JSON Schemas (schemas/v0.2/*.schema.json): v0.1 plus the
// optional context measurement of an observation.
//
//go:embed schemas/v0.2/*.schema.json
var SchemasV02 embed.FS

// SchemasEvalsV1 holds the schemas of the local eval library (schemas/evals-v1/*.schema.json).
// These records stay on the user's machine; they are not part of the HINT wire standard.
//
//go:embed schemas/evals-v1/*.schema.json
var SchemasEvalsV1 embed.FS

// SchemaEvalReportV01 is the eval result report schema (schemas/evalreport-v0.1).
//
//go:embed schemas/evalreport-v0.1/report.schema.json
var SchemaEvalReportV01 []byte

// OpenCodePlugin is the OpenCode adapter plugin that `git hint adapter install opencode`
// writes to .opencode/plugins/.
//
//go:embed adapters/opencode/hint-adapter.ts
var OpenCodePlugin []byte
