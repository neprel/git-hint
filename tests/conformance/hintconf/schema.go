package hintconf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaBase is the URN prefix of the v0.1 schema identifiers.
const SchemaBase = "urn:hint:schema:v0.1:"

// Schemas holds the compiled document schema of each version found next to dir.
type Schemas struct{ envelope map[string]*jsonschema.Schema }

// LoadSchemas compiles the schema sets of dir (schemas/v0.1) and of its sibling versions
// (schemas/v0.2: v0.1 plus the context measurement of an observation).
func LoadSchemas(dir string) (*Schemas, error) {
	out := &Schemas{envelope: map[string]*jsonschema.Schema{}}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), "v0.*"))
	for _, d := range dirs {
		v := strings.TrimPrefix(filepath.Base(d), "v")
		s, err := compileDir(d, "urn:hint:schema:v"+v+":")
		if err != nil {
			return nil, err
		}
		out.envelope[v] = s
	}
	if len(out.envelope) == 0 {
		return nil, fmt.Errorf("no schemas in %s", dir)
	}
	return out, nil
}

func compileDir(dir, base string) (*jsonschema.Schema, error) {
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
	return c.Compile(base + "envelope")
}

// Validate checks one decoded document against the envelope schema of its hint_version.
func (s *Schemas) Validate(doc any) error {
	m, _ := doc.(map[string]any)
	v, _ := m["hint_version"].(string)
	parts := strings.SplitN(v, ".", 3)
	if len(parts) == 3 {
		if e := s.envelope[parts[0]+"."+parts[1]]; e != nil {
			return e.Validate(doc)
		}
	}
	return fmt.Errorf("unsupported hint_version %q", v)
}

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
