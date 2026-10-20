package iodecl

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func spec(edit func(m map[string]any)) []byte {
	m := map[string]any{"interface": Interface, "entry": []string{"sh", "tool.sh"}, "cases": []string{"lower", "mixed"}, "case_seconds": 5, "compare": "exact"}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func TestParse(t *testing.T) {
	ins := map[string][]byte{"lower": []byte("hello\n"), "mixed": []byte("Ab c\nx")}
	d, err := Parse(spec(nil), ins)
	if err != nil || !strings.HasPrefix(d.SpecDigest, "sha256:") || !strings.HasPrefix(d.InputsDigest, "sha256:") || string(d.Inputs["mixed"]) != "Ab c\nx" {
		t.Fatalf("valid: %+v %v", d, err)
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("c%d", i)
	}
	for name, c := range map[string]struct {
		spec []byte
		ins  map[string][]byte
	}{
		"no cases":         {spec(func(m map[string]any) { m["cases"] = []string{} }), map[string][]byte{}},
		"65 cases":         {spec(func(m map[string]any) { m["cases"] = many }), ins},
		"unsafe ID":        {spec(func(m map[string]any) { m["cases"] = []string{"../x", "mixed"} }), map[string][]byte{"../x": nil, "mixed": nil}},
		"duplicate ID":     {spec(func(m map[string]any) { m["cases"] = []string{"lower", "lower"} }), ins},
		"unknown field":    {spec(func(m map[string]any) { m["shell"] = true }), ins},
		"other interface":  {spec(func(m map[string]any) { m["interface"] = "cli-file-v2" }), ins},
		"other comparison": {spec(func(m map[string]any) { m["compare"] = "trimmed" }), ins},
		"empty entry":      {spec(func(m map[string]any) { m["entry"] = []string{} }), ins},
		"NUL in entry":     {spec(func(m map[string]any) { m["entry"] = []string{"a\x00b"} }), ins},
		"zero timeout":     {spec(func(m map[string]any) { m["case_seconds"] = 0 }), ins},
		"long timeout":     {spec(func(m map[string]any) { m["case_seconds"] = 31 }), ins},
		"missing input":    {spec(nil), map[string][]byte{"lower": nil, "other": nil}},
		"extra input":      {spec(nil), map[string][]byte{"lower": nil, "mixed": nil, "x": nil}},
		"oversize input":   {spec(nil), map[string][]byte{"lower": make([]byte, MaxBytes+1), "mixed": nil}},
		"trailing data":    {append(spec(nil), []byte(" {}")...), ins},
	} {
		if _, err := Parse(c.spec, c.ins); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := Tests([]string{"b", "a"}); !slices.Equal(got, []string{"cases/b::output", "cases/a::output"}) {
		t.Fatalf("tests %v", got)
	}
}
