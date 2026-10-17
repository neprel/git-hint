// Package evals holds the local eval library: policy, manifests and their validation.
//
// Eval records stay on the user's machine. They are not HINT wire documents and never go
// to a HINT server; a separate minimized result report is the only server-bound output.
package evals

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	githint "github.com/neprel/git-hint"
)

// SchemaBase is the URN prefix of the local eval schemas.
const SchemaBase = "urn:hint:local:evals:v1:"

// Kinds are the record kinds with a schema of their own.
var Kinds = []string{"policy", "episode", "eval", "suite", "trial"}

var (
	once     sync.Once
	compiled map[string]*jsonschema.Schema
	errLoad  error
)

func load() (map[string]*jsonschema.Schema, error) {
	once.Do(func() { compiled, errLoad = compile(githint.SchemasEvalsV1, "schemas/evals-v1") })
	return compiled, errLoad
}

func compile(fsys fs.FS, dir string) (map[string]*jsonschema.Schema, error) {
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
	out := map[string]*jsonschema.Schema{}
	for _, k := range Kinds {
		s, err := c.Compile(SchemaBase + k)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

// Validate checks one record of the given kind against its schema.
func Validate(kind string, data []byte) error {
	schemas, err := load()
	if err != nil {
		return err
	}
	s, ok := schemas[kind]
	if !ok {
		return fmt.Errorf("unknown record kind %q", kind)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if err := s.Validate(doc); err != nil {
		return fmt.Errorf("%s record: %w", kind, err)
	}
	return nil
}

// ValidateValue marshals v and validates it.
func ValidateValue(kind string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return Validate(kind, b)
}
