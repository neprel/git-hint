package mcpserver

import (
	"context"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// add registers a tool with an output schema without nullable types (portable to clients
// that map schemas to a single-type dialect) and fills nil slices and maps in the output,
// so that every result matches that schema.
func add[In, Out any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	sch, err := jsonschema.For[Out](nil)
	if err != nil {
		panic(err)
	}
	stripNull(sch)
	t.OutputSchema = sch
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		res, out, err := h(ctx, req, in)
		if err == nil {
			fill(reflect.ValueOf(&out).Elem())
		}
		return res, out, err
	})
}

// stripNull turns {"type": ["null", X]} into {"type": X} everywhere.
func stripNull(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) == 2 && (s.Types[0] == "null" || s.Types[1] == "null") {
		if s.Types[0] == "null" {
			s.Type = s.Types[1]
		} else {
			s.Type = s.Types[0]
		}
		s.Types = nil
	}
	for _, p := range s.Properties {
		stripNull(p)
	}
	stripNull(s.Items)
	stripNull(s.AdditionalProperties)
	for _, p := range s.PrefixItems {
		stripNull(p)
	}
	for _, p := range s.AnyOf {
		stripNull(p)
	}
	for _, p := range s.Defs {
		stripNull(p)
	}
}

// fill replaces nil slices and maps by empty ones, recursively.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			fill(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Slice:
		if v.IsNil() && v.CanSet() {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
		for i := 0; i < v.Len(); i++ {
			fill(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() && v.CanSet() {
			v.Set(reflect.MakeMap(v.Type()))
		}
	}
}
