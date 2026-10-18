package hint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/version"
)

// SchemaBase is the URN prefix of the v0.1 schema identifiers.
const SchemaBase = "urn:hint:schema:v0.1:"

// Versions are the supported specification versions (major.minor) and their schema sets.
// v0.2 is v0.1 plus the optional context measurement of an observation; identifiers and
// digests are the same.
var Versions = version.Supported

// Schemas holds the compiled document schema of each supported version.
type Schemas struct{ envelope map[string]*jsonschema.Schema }

var (
	once     sync.Once
	compiled *Schemas
	errLoad  error
)

// LoadSchemas compiles the schemas that are embedded in the binary.
func LoadSchemas() (*Schemas, error) {
	once.Do(func() {
		compiled = &Schemas{envelope: map[string]*jsonschema.Schema{}}
		for v, src := range map[string]fs.FS{"0.1": githint.SchemasV01, "0.2": githint.SchemasV02} {
			s, err := compile(src, "schemas/v"+v, "urn:hint:schema:v"+v+":")
			if err != nil {
				errLoad = err
				return
			}
			compiled.envelope[v] = s
		}
	})
	return compiled, errLoad
}

func compile(fsys fs.FS, dir, base string) (*jsonschema.Schema, error) {
	files, err := fs.Glob(fsys, dir+"/*.schema.json")
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no schemas in %s", dir)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
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
	return fmt.Errorf("unsupported hint_version %q (supported: %s)", v, strings.Join(Versions, ", "))
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
