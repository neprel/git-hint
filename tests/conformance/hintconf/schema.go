package hintconf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaBase is the URN prefix of the v0.1 schema identifiers.
const SchemaBase = "urn:hint:schema:v0.1:"

// Schemas holds the compiled v0.1 document schema.
type Schemas struct{ envelope *jsonschema.Schema }

// LoadSchemas compiles every *.schema.json file of dir (schemas/v0.1).
func LoadSchemas(dir string) (*Schemas, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no schemas in %s", dir)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if err := c.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	s, err := c.Compile(SchemaBase + "envelope")
	if err != nil {
		return nil, err
	}
	return &Schemas{envelope: s}, nil
}

// Validate checks one decoded document against the envelope schema.
func (s *Schemas) Validate(doc any) error { return s.envelope.Validate(doc) }

// Decode parses JSON with numbers as json.Number (the form the schema library expects).
func Decode(b []byte) (any, error) { return jsonschema.UnmarshalJSON(bytes.NewReader(b)) }

// Plain converts a decoded document to plain Go values (float64 numbers) for semantic checks.
func Plain(doc any) (map[string]any, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}
