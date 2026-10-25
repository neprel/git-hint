package hint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/version"
)

// SchemaBase is the URN prefix of the v0.1 schema identifiers.
const SchemaBase = "urn:hint:schema:v0.1:"

// Versions are the supported specification versions (major.minor) and their schema sets.
// v0.3 replaces private ledger/membership commitments with project-keyed references.
var Versions = version.Supported

// Schemas holds the compiled document schema of each supported version.
type Schemas struct {
	envelope     map[string]*jsonschema.Schema
	snapshots    map[string]*jsonschema.Schema
	specVersions map[string]*jsonschema.Schema
}

var (
	once     sync.Once
	compiled *Schemas
	errLoad  error
)

// LoadSchemas compiles the schemas that are embedded in the binary.
func LoadSchemas() (*Schemas, error) {
	once.Do(func() {
		compiled = &Schemas{envelope: map[string]*jsonschema.Schema{}, snapshots: map[string]*jsonschema.Schema{}, specVersions: map[string]*jsonschema.Schema{}}
		for v, src := range map[string]fs.FS{"0.1": githint.SchemasV01, "0.2": githint.SchemasV02, "0.3": githint.SchemasV03} {
			s, snap, spec, err := compile(src, "schemas/v"+v, "urn:hint:schema:v"+v+":")
			if err != nil {
				errLoad = err
				return
			}
			compiled.envelope[v], compiled.snapshots[v], compiled.specVersions[v] = s, snap, spec
		}
	})
	return compiled, errLoad
}

func compile(fsys fs.FS, dir, base string) (*jsonschema.Schema, *jsonschema.Schema, *jsonschema.Schema, error) {
	files, err := fs.Glob(fsys, dir+"/*.schema.json")
	if err != nil || len(files) == 0 {
		return nil, nil, nil, fmt.Errorf("no schemas in %s", dir)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, nil, nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if err := c.AddResource(id, doc); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	env, err := c.Compile(base + "envelope")
	if err != nil {
		return nil, nil, nil, err
	}
	snap, err := c.Compile(base + "repository#/$defs/SnapshotPayload")
	if err != nil {
		return nil, nil, nil, err
	}
	spec, err := c.Compile(base + "common#/$defs/SpecVersion")
	return env, snap, spec, err
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

// ValidateSnapshotPayload validates only the existing pinned repository payload
// resource, with the same supported version family and pinned patch syntax.
// It does not create a document envelope, identity, or origin/capture authority.
func (s *Schemas) ValidateSnapshotPayload(version string, payload any) error {
	parts := strings.SplitN(version, ".", 3)
	if s == nil || len(parts) != 3 {
		return fmt.Errorf("unsupported snapshot payload version")
	}
	family := parts[0] + "." + parts[1]
	schema, ok := s.snapshots[family]
	if !ok || s.specVersions[family].Validate(version) != nil {
		return fmt.Errorf("unsupported snapshot payload version")
	}
	return schema.Validate(payload)
}

// CheckSnapshotPayload reuses the normal snapshot semantic checks without making
// a document envelope. It grants no document identity or capture authority.
func CheckSnapshotPayload(version string, payload map[string]any, repositories []string) []Finding {
	// Only normal checker version context, not a fabricated HINT envelope.
	c := &checker{doc: map[string]any{"hint_version": version}}
	c.scan(payload, "payload")
	c.numbers(payload, "payload")
	repos := map[string]bool{}
	for _, repo := range repositories {
		repos[repo] = true
	}
	c.snapshot(payload, repos)
	sort.SliceStable(c.out, func(i, j int) bool { return c.out[i].Path < c.out[j].Path })
	return c.out
}
