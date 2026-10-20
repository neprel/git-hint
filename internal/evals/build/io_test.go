package build

import (
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
)

// ioInput is a declared cli-file-v1 eval: a Go module whose tool.sh must upper-case stdin.
func ioInput(t *testing.T, l *library.Library) InputIO {
	base := mkTree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "cat\n"})
	result := mkTree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
	return InputIO{EpisodeID: "ep-0123456789abcdef", Family: "bug_fix", Repo: "shop", Base: base, Result: result,
		Request: "tool.sh must print its input in upper case.", RequestAt: map[string]any{"source": library.Digest([]byte("src")), "record": "u1"},
		Entry: []string{"sh", "tool.sh"}, Cases: []IOCase{{"lower", []byte("hello\n")}, {"mixed", []byte("Ab c\nx")}}, CaseSeconds: 5,
		Now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

// 07B-J §17.2: a declared cli-file-v1 eval has exactly the B1 check, a verifier archive with
// only the declaration and the inputs, and no input in the agent's base archive.
func TestBuildIO(t *testing.T) {
	l := lib(t)
	b, err := BuildIO(l, ioInput(t, l))
	if err != nil {
		t.Fatal(err)
	}
	rec := b.Record
	if err := evals.ValidateValue("eval", rec); err != nil {
		t.Fatalf("schema: %v", err)
	}
	v := rec["verifier"].(map[string]any)
	cs := v["checks"].([]any)
	if v["kind"] != "cli-file-v1" || len(cs) != 1 || cs[0].(map[string]any)["check_id"] != "B1" || cs[0].(map[string]any)["origin"] != "declared_io" {
		t.Fatalf("verifier %v", v)
	}
	arc := untar(t, l, v["archive"].(string))
	if len(arc) != 3 || arc["io/inputs/mixed.in"] != "Ab c\nx" || !strings.Contains(arc["io/spec.json"], `"cli-file-v1"`) {
		t.Fatalf("verifier archive %v", keysOf(arc))
	}
	agent := untar(t, l, rec["repositories"].([]any)[0].(map[string]any)["base_archive"].(string))
	for name, c := range agent {
		if strings.Contains(c, "Ab c") || strings.Contains(name, "io/") {
			t.Fatalf("an input in the agent archive: %s", name)
		}
	}
	if ref := untar(t, l, rec["reference"].(string)); ref["files/tool.sh"] != "tr a-z A-Z\n" {
		t.Fatalf("reference %v", keysOf(ref))
	}
	// declarations the runner would refuse are refused here too
	for name, edit := range map[string]func(in *InputIO){
		"no cases":       func(in *InputIO) { in.Cases = nil },
		"unsafe ID":      func(in *InputIO) { in.Cases[0].ID = "../x" },
		"duplicate ID":   func(in *InputIO) { in.Cases[1].ID = "lower" },
		"empty entry":    func(in *InputIO) { in.Entry = nil },
		"long timeout":   func(in *InputIO) { in.CaseSeconds = 31 },
		"no change":      func(in *InputIO) { in.Result = in.Base },
		"no request":     func(in *InputIO) { in.Request = " " },
		"oversize input": func(in *InputIO) { in.Cases[0].Input = make([]byte, 1<<20+1) },
	} {
		in := ioInput(t, l)
		edit(&in)
		if _, err := BuildIO(l, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The schema binds the kind to its check and command: B checks only with cli-file-v1,
// exactly one B1 with origin declared_io, C checks only with the generated kind.
func TestVerifierKindSchema(t *testing.T) {
	l := lib(t)
	b, err := BuildIO(l, ioInput(t, l))
	if err != nil {
		t.Fatal(err)
	}
	gen, err := Build(l, input(t, l))
	if err != nil {
		t.Fatal(err)
	}
	clone := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	for name, mk := range map[string]func() map[string]any{
		"cli kind with C1": func() map[string]any {
			r := clone(b.Record)
			r["verifier"] = map[string]any{"archive": b.Record["verifier"].(map[string]any)["archive"], "kind": "cli-file-v1",
				"checks": []any{map[string]any{"check_id": "C1", "origin": "declared_io", "tests": []any{"cases/lower::output"}}}}
			return r
		},
		"cli kind with two checks": func() map[string]any {
			r := clone(b.Record)
			v := clone(b.Record["verifier"].(map[string]any))
			v["checks"] = append(append([]any{}, v["checks"].([]any)...), map[string]any{"check_id": "B2", "origin": "declared_io", "tests": []any{}})
			r["verifier"] = v
			return r
		},
		"generated kind with B1": func() map[string]any {
			r := clone(gen.Record)
			v := clone(gen.Record["verifier"].(map[string]any))
			v["checks"] = []any{map[string]any{"check_id": "B1", "origin": "declared_io"}}
			r["verifier"] = v
			return r
		},
		"cli kind with tests/test.sh": func() map[string]any {
			r := clone(b.Record)
			e := clone(b.Record["environment"].(map[string]any))
			e["test_command"] = "tests/test.sh"
			r["environment"] = e
			return r
		},
		"generated kind with the io command": func() map[string]any {
			r := clone(gen.Record)
			e := clone(gen.Record["environment"].(map[string]any))
			e["test_command"] = "tests/io/spec.json"
			r["environment"] = e
			return r
		},
	} {
		if evals.ValidateValue("eval", mk()) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := evals.ValidateValue("eval", gen.Record); err != nil {
		t.Fatalf("a generated eval without kind: %v", err)
	}
}
