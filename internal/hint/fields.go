package hint

import (
	"encoding/json"
	"io/fs"
	"sort"

	githint "github.com/neprel/git-hint"
)

// recordDefs names the schema file and definition of each record type.
var recordDefs = map[string][2]string{
	"Snapshot": {"repository", "Snapshot"}, "Entity": {"repository", "Entity"}, "Relation": {"repository", "Relation"},
	"PolicyConstraint": {"repository", "PolicyConstraint"}, "Change": {"repository", "Change"},
	"Observation": {"activity", "Observation"}, "Metric": {"activity", "Metric"},
	"Plan": {"recommendation", "Plan"}, "Effect": {"recommendation", "Effect"},
}

// RecordFields returns the top-level fields of every record type, from the embedded schemas.
func RecordFields() (map[string][]string, error) {
	out := map[string][]string{}
	for rt, d := range recordDefs {
		b, err := fs.ReadFile(githint.SchemasV01, "schemas/v0.1/"+d[0]+".schema.json")
		if err != nil {
			return nil, err
		}
		var s struct {
			Defs map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"$defs"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, err
		}
		for k := range s.Defs[d[1]].Properties {
			out[rt] = append(out[rt], k)
		}
		sort.Strings(out[rt])
	}
	return out, nil
}

// RequiredFields returns the required top-level fields of a record type.
func RequiredFields(recordType string) ([]string, error) {
	d, ok := recordDefs[recordType]
	if !ok {
		return nil, nil
	}
	b, err := fs.ReadFile(githint.SchemasV01, "schemas/v0.1/"+d[0]+".schema.json")
	if err != nil {
		return nil, err
	}
	var s struct {
		Defs map[string]struct {
			Required []string `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return s.Defs[d[1]].Required, nil
}
