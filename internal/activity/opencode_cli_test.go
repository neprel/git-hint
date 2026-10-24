package activity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

func cliOptions(t *testing.T) CLIRootOptions {
	t.Helper()
	var cfg Configuration
	if err := json.Unmarshal([]byte(ledgerConfigJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	return CLIRootOptions{Key: testKey, AgentVersion: CLIRootAgentVersion, Configuration: &cfg,
		TaskID: hint.DeriveID(testKey, "task", "synthetic"), TaskFamilyID: FamilyRef(testKey, "synthetic"), RunRef: hint.DeriveID(testKey, "run", "synthetic"),
		Cwd: "/app", Resolve: func(abs string) (string, bool) {
			if strings.HasPrefix(abs, "/app/src/") {
				return hint.FileID(testKey, "synthetic", strings.TrimPrefix(abs, "/app/")), true
			}
			return "", false
		}, CaptureComplete: true, CapturedAt: time.Unix(1760100000, 0)}
}

func cliFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cli-root", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCLIRootQualificationShapes(t *testing.T) {
	for _, c := range []struct {
		name                     string
		records, tools, failures int
	}{{"p1", 26, 7, 0}, {"p2", 24, 6, 0}, {"p3", 26, 8, 2}} {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeCLIRoot(strings.NewReader(cliFixture(t, c.name)), cliOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Observations) != c.tools || got.Diagnostics.Records != c.records || got.Diagnostics.FailedTools != c.failures || !got.Diagnostics.MissingStepFinish {
				t.Fatalf("%+v", got.Diagnostics)
			}
			if hint.SourceCLIRootProblem(cliPlain(got.Source)) {
				t.Fatal("source violates its scope")
			}
			for _, o := range got.Observations {
				if o.Source != "log" || hint.ObservationCLIRootProblem(cliPlain(o)) {
					t.Fatal("observation violates its scope")
				}
				if o.Provenance.CapturedAt == Timestamp(cliOptions(t).CapturedAt) {
					t.Fatal("import time replaced event time")
				}
			}
			b, _ := json.Marshal(got)
			for _, canary := range []string{"HINTCANARY", "synthetic-session", "synthetic-call", "/app", "providerResult", "synthetic argument"} {
				if strings.Contains(string(b), canary) {
					t.Fatalf("raw capture content escaped: %s", canary)
				}
			}
		})
	}
}

func cliTool(tool, status string) map[string]any {
	return map[string]any{"type": "tool_use", "sessionID": "synthetic-session", "timestamp": float64(1760000000100), "part": map[string]any{
		"type": "tool", "id": "synthetic-call", "messageID": "synthetic-message", "partID": "synthetic-part", "sessionID": "synthetic-session", "tool": tool,
		"state": map[string]any{"status": status, "input": map[string]any{"path": "/app/src/example.go"}, "output": "HINTCANARY output", "metadata": map[string]any{"providerCall": map[string]any{"executed": false}, "providerResult": map[string]any{"executed": false}},
			"time": map[string]any{"start": float64(1760000000000), "end": float64(1760000000100)}}}}
}

func cliState(record map[string]any) map[string]any {
	return record["part"].(map[string]any)["state"].(map[string]any)
}

func TestCLIRootTerminalStatusAndUnknownScope(t *testing.T) {
	for _, c := range []struct {
		tool, status, op string
		size             bool
	}{{"read", "completed", "READ", true}, {"read", "error", "READ", false}, {"task", "completed", "DELEGATE", true}, {"unrecognized-private-tool", "completed", "TOOL", true}} {
		record := cliTool(c.tool, c.status)
		got, err := DecodeCLIRoot(strings.NewReader(events(record)), cliOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		o := got.Observations[0]
		if o.Op != c.op || (o.Size != nil) != c.size || (o.Outcome.Status == "failure") != (c.status == "error") {
			t.Fatalf("%+v", o)
		}
		if c.tool == "task" && (!slices.Contains(o.OpsSecondary, "UNKNOWN") || o.Coordination != nil) {
			t.Fatal("delegate pretends child coverage")
		}
		if c.tool == "read" && c.status == "error" && !slices.Contains(o.OpsSecondary, "UNKNOWN") {
			t.Fatal("failed read claims complete navigation")
		}
		if c.tool == "unrecognized-private-tool" && !slices.Contains(o.OpsSecondary, "UNKNOWN") {
			t.Fatal("unknown tool lost uncertainty")
		}
	}
}

func TestCLIRootReplayIdentityAndSourceBinding(t *testing.T) {
	input := events(cliTool("read", "completed"))
	a := cliOptions(t)
	first, err := DecodeCLIRoot(strings.NewReader(input), a)
	if err != nil {
		t.Fatal(err)
	}
	a.CapturedAt = a.CapturedAt.Add(time.Hour)
	again, err := DecodeCLIRoot(strings.NewReader(input), a)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Observations, again.Observations) || first.Source.SourceID != again.Source.SourceID {
		t.Fatal("capture time changes replay")
	}
	a.RunRef = hint.DeriveID(testKey, "run", "another")
	other, err := DecodeCLIRoot(strings.NewReader(input), a)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.SourceID == other.Source.SourceID || first.Observations[0].ObservationID != other.Observations[0].ObservationID {
		t.Fatal("run does not bind source, or changed body evades observation conflict")
	}
	changed := cliTool("read", "completed")
	cliState(changed)["output"] = "different output length"
	body, err := DecodeCLIRoot(strings.NewReader(events(changed)), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if body.Observations[0].ObservationID != first.Observations[0].ObservationID || reflect.DeepEqual(body.Observations, first.Observations) {
		t.Fatal("changed body changes identity or disappears")
	}
}

func cliPlain(v any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func TestCLIRootAtomicIntegrityFailures(t *testing.T) {
	cases := []struct {
		name, code string
		mutate     func(map[string]any)
		suffix     string
	}{
		{"malformed", "record_json_invalid", nil, "{HINTCANARY\n"},
		{"unknown record", "record_type_unsupported", func(r map[string]any) { r["type"] = "future-private-record" }, ""},
		{"missing timestamp", "timestamp_invalid", func(r map[string]any) { delete(r, "timestamp") }, ""},
		{"fractional timestamp", "timestamp_invalid", func(r map[string]any) { r["timestamp"] = 1.5 }, ""},
		{"overflow timestamp", "timestamp_invalid", func(r map[string]any) { r["timestamp"] = 1e100 }, ""},
		{"missing start", "tool_time_invalid", func(r map[string]any) { delete(cliState(r)["time"].(map[string]any), "start") }, ""},
		{"end before start", "tool_time_invalid", func(r map[string]any) { cliState(r)["time"].(map[string]any)["end"] = float64(1759999999000) }, ""},
		{"record before end", "tool_time_invalid", func(r map[string]any) { r["timestamp"] = float64(1759999999000) }, ""},
		{"missing call", "tool_identity_invalid", func(r map[string]any) { delete(r["part"].(map[string]any), "id") }, ""},
		{"missing message", "tool_identity_invalid", func(r map[string]any) { delete(r["part"].(map[string]any), "messageID") }, ""},
		{"part session", "tool_binding_invalid", func(r map[string]any) { r["part"].(map[string]any)["sessionID"] = "another-session" }, ""},
		{"different root", "multiple_root_sessions", func(r map[string]any) { r["sessionID"] = "another-session" }, ""},
		{"running tool", "tool_not_terminal", func(r map[string]any) { cliState(r)["status"] = "running" }, ""},
		{"input wrong type", "tool_input_invalid", func(r map[string]any) { cliState(r)["input"] = "HINTCANARY raw" }, ""},
		{"duplicate conflict", "tool_duplicate_conflict", func(r map[string]any) {
			r["part"].(map[string]any)["id"] = "synthetic-call"
			cliState(r)["output"] = "changed body"
		}, ""},
		{"part conflict", "tool_duplicate_conflict", func(r map[string]any) { r["part"].(map[string]any)["partID"] = "synthetic-part" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := cliTool("read", "completed")
			second := cliTool("read", "completed")
			second["part"].(map[string]any)["id"] = "second-call"
			second["part"].(map[string]any)["partID"] = "second-part"
			input := events(first)
			if c.mutate != nil {
				c.mutate(second)
				input += events(second)
			} else {
				input += c.suffix
			}
			got, err := DecodeCLIRoot(strings.NewReader(input), cliOptions(t))
			var reason *CLIRootError
			if !errors.As(err, &reason) || reason.Code != c.code || !reflect.DeepEqual(got, CLIRootImport{}) {
				t.Fatalf("result is not atomic: %+v %v", got, err)
			}
			if strings.Contains(err.Error(), "HINTCANARY") {
				t.Fatal("raw error content escaped")
			}
		})
	}
}

func TestCLIRootDuplicateAndStrictJSON(t *testing.T) {
	record := cliTool("read", "completed")
	input := events(record)
	got, err := DecodeCLIRoot(strings.NewReader(input+input), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != 1 || got.Diagnostics.DuplicateTools != 1 {
		t.Fatal("identical terminal replay was not deduplicated")
	}
	other := cliTool("read", "completed")
	other["part"].(map[string]any)["messageID"] = "second-message"
	other["part"].(map[string]any)["partID"] = "second-part"
	independent, err := DecodeCLIRoot(strings.NewReader(input+events(other)), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(independent.Observations) != 2 || independent.Observations[0].ObservationID == independent.Observations[1].ObservationID || independent.Observations[0].Correlation["tool_call_id"] == independent.Observations[1].Correlation["tool_call_id"] {
		t.Fatal("call identity is not scoped to its message")
	}
	duplicateField := strings.Replace(input, `"type":"tool_use"`, `"type":"tool_use","type":"tool_use"`, 1)
	if got, err := DecodeCLIRoot(strings.NewReader(duplicateField), cliOptions(t)); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
		t.Fatal("duplicate JSON fields accepted")
	}
	for _, in := range []string{"", strings.TrimSuffix(input, "\n"), input + "\n"} {
		if got, err := DecodeCLIRoot(strings.NewReader(in), cliOptions(t)); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
			t.Fatal("empty, truncated or malformed capture accepted")
		}
	}
}

func TestCLIRootOptionsAndCaptureFact(t *testing.T) {
	for _, mutate := range []func(*CLIRootOptions){
		func(o *CLIRootOptions) { o.Key = nil }, func(o *CLIRootOptions) { o.AgentVersion = "2.0.27" }, func(o *CLIRootOptions) { o.Configuration = nil },
		func(o *CLIRootOptions) { o.TaskID = "" }, func(o *CLIRootOptions) { o.TaskFamilyID = "raw-family" }, func(o *CLIRootOptions) { o.RunRef = "" },
		func(o *CLIRootOptions) { o.Cwd = "/host/private" }, func(o *CLIRootOptions) { o.Resolve = nil }, func(o *CLIRootOptions) { o.CaptureComplete = false },
		func(o *CLIRootOptions) { o.Configuration.Agent.Version.Value = "2.0.27" }, func(o *CLIRootOptions) { o.Configuration.Model.Ref = "HINTCANARY" },
	} {
		o := cliOptions(t)
		mutate(&o)
		if got, err := DecodeCLIRoot(strings.NewReader(events(cliTool("read", "completed"))), o); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
			t.Fatal("invalid binding or capture assertion accepted")
		}
	}
	o := cliOptions(t)
	o.Resolve = func(string) (string, bool) { return "HINTCANARY raw path", true }
	if got, err := DecodeCLIRoot(strings.NewReader(events(cliTool("read", "completed"))), o); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
		t.Fatal("resolver can export raw values")
	}
}

type cliBrokenReader struct{}

func (cliBrokenReader) Read([]byte) (int, error) { return 0, errors.New("HINTCANARY read error") }

func TestCLIRootReadErrorIsAtomicAndPrivate(t *testing.T) {
	got, err := DecodeCLIRoot(cliBrokenReader{}, cliOptions(t))
	if err == nil || strings.Contains(err.Error(), "HINTCANARY") || !reflect.DeepEqual(got, CLIRootImport{}) {
		t.Fatal("read failure exposed content or observations")
	}
}

func TestCLIRootShellExitAndMetadataReduction(t *testing.T) {
	for _, exit := range []any{float64(0), float64(7), nil} {
		r := cliTool("shell", "completed")
		state := cliState(r)
		state["input"] = map[string]any{"command": "cat example.go", "workdir": "src"}
		state["metadata"] = map[string]any{"metadata": map[string]any{"exit": exit}}
		got, err := DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		o := got.Observations[0]
		if o.Source != "log" || o.CommandClass != "READ" || len(o.Targets) != 1 || o.Targets[0].EntityID != hint.FileID(testKey, "synthetic", "src/example.go") {
			t.Fatalf("%+v", o)
		}
		if exit == nil && (o.Outcome.ExitCode != nil || !slices.Contains(o.Missing, "outcome.exit_code")) {
			t.Fatal("unknown exit converted to zero")
		}
		if exit == float64(7) && (o.Outcome.Status != "failure" || o.Size != nil) {
			t.Fatal("nonzero shell exit became successful read")
		}
	}
	for _, meta := range []map[string]any{{"exit": 1.5}, {"exit": "HINTCANARY"}, {"exit": float64(0), "metadata": map[string]any{"exit": float64(1)}}} {
		r := cliTool("shell", "completed")
		cliState(r)["input"] = map[string]any{"command": "go test ./..."}
		cliState(r)["metadata"] = meta
		if got, err := DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t)); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
			t.Fatal("invalid/conflicting exit accepted")
		}
	}
}

func TestCLIRootSharedClassifierAndNormalizerParity(t *testing.T) {
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"read", map[string]any{"path": "/app/src/example.go"}}, {"grep", map[string]any{"path": "/app/src/example.go"}},
		{"glob", nil}, {"edit", map[string]any{"filePath": "/app/src/example.go"}}, {"shell", map[string]any{"command": "git show HEAD"}},
		{"shell", map[string]any{"command": "cat src/example.go"}}, {"todowrite", nil}, {"unknown", nil},
	} {
		args := c.args
		if args == nil {
			args = map[string]any{}
		}
		r := cliTool(c.tool, "completed")
		cliState(r)["input"] = args
		decoded, err := DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		n := norm()
		n.Cwd = "/app"
		n.Resolve = cliOptions(t).Resolve
		var legacy []Observation
		_, err = n.Stream(strings.NewReader(events(ev("tool.execute.before", c.tool, "parity", map[string]any{"args": args}))), func(o Observation) error { legacy = append(legacy, o); return nil })
		if err != nil {
			t.Fatal(err)
		}
		a, b := decoded.Observations[0], legacy[0]
		if a.Op != b.Op || a.Precision != b.Precision || a.CommandClass != b.CommandClass || !slices.Equal(a.OpsSecondary, b.OpsSecondary) || !reflect.DeepEqual(a.Targets, b.Targets) {
			t.Fatalf("classifier parity changed: %+v / %+v", a, b)
		}
	}
	classified := ClassifyTool("read", map[string]any{"path": "/app/HINTCANARY"})
	b, _ := json.Marshal(classified)
	if strings.Contains(string(b), "HINTCANARY") {
		t.Fatal("classifier serialized transient paths")
	}
}

func TestCLIRootPatchTargetsAndFailedReadsStayConservative(t *testing.T) {
	r := cliTool("apply_patch", "completed")
	state := cliState(r)
	state["input"] = map[string]any{"patch": "HINTCANARY raw patch"}
	state["metadata"] = map[string]any{"metadata": map[string]any{"files": []any{map[string]any{"file": "/app/src/example.go", "patch": "HINTCANARY"}}}}
	got, err := DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations[0].Targets) != 1 || slices.Contains(got.Observations[0].Missing, "targets") {
		t.Fatal("observed edit target was lost")
	}
	r = cliTool("read", "error")
	cliState(r)["input"] = map[string]any{"path": "/app/../../private/HINTCANARY"}
	got, err = DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	o := got.Observations[0]
	if len(o.Targets) != 0 || o.Size != nil || !slices.Contains(o.Missing, "targets") || !slices.Contains(o.Missing, "size") {
		t.Fatal("failed/outside read invented evidence")
	}
}

func TestCLIRootSchemaAndSemanticConformance(t *testing.T) {
	got, err := DecodeCLIRoot(strings.NewReader(cliFixture(t, "p3")), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got.Observations, func(i, j int) bool { return got.Observations[i].ObservationID < got.Observations[j].ObservationID })
	payload := map[string]any{"observations": got.Observations, "metrics": []any{}}
	digest, err := hint.Digest(payload)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"hint_version": "0.2.0", "document_type": "activity_stream", "document_id": hint.DeriveID(testKey, "doc", "cli"),
		"project_namespace": hint.DeriveID(testKey, "ns"), "repositories": []any{hint.DeriveID(testKey, "repo")},
		"producer": map[string]any{"tool": "hint-cli", "version": "0.1.0"}, "created_at": "2026-10-19T12:00:00Z", "sources": []any{got.Source},
		"redaction":      map[string]any{"policy_id": hint.DeriveID(testKey, "policy"), "profile": "detailed", "record_types": []any{"Observation"}, "free_text": "denied", "time_resolution_seconds": 0},
		"content_digest": digest, "payload": payload}
	b, _ := json.Marshal(doc)
	decoded, err := hint.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := hint.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(decoded); err != nil {
		t.Fatal(err)
	}
	if findings := hint.Check(decoded.(map[string]any), nil); len(findings) > 0 {
		t.Fatal(findings)
	}
}

func TestCLIRootSourceIdentityAndConfigurationSnapshot(t *testing.T) {
	o := cliOptions(t)
	a, err := CLIRootSource(o)
	if err != nil {
		t.Fatal(err)
	}
	o.Configuration.ConfigRefs.Refs[0] = strings.Repeat("c", 32)
	b, err := CLIRootSource(o)
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceID == b.SourceID || a.Configuration.ConfigRefs.Refs[0] == b.Configuration.ConfigRefs.Refs[0] {
		t.Fatal("configuration identity or clone is missing")
	}
	for _, mutate := range []func(*CLIRootOptions){func(o *CLIRootOptions) { o.TaskID = hint.DeriveID(testKey, "task", "another") }, func(o *CLIRootOptions) { o.TaskFamilyID = FamilyRef(testKey, "another") }} {
		o := cliOptions(t)
		mutate(&o)
		c, err := CLIRootSource(o)
		if err != nil {
			t.Fatal(err)
		}
		if a.SourceID == c.SourceID {
			t.Fatal("source identity omits declaration")
		}
	}
}

func TestCLIRootOutputBytesAreObservedNotInvented(t *testing.T) {
	for _, value := range []string{"é\n", "[redacted:len=999]", ""} {
		r := cliTool("read", "completed")
		cliState(r)["output"] = value
		got, err := DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		o := got.Observations[0]
		if o.Size == nil || *o.Size.Value != float64(len(value)) || o.Size.Method != "scanner" || o.ContentDelivery != "unknown" {
			t.Fatal("read size/delivery exceeds observed output")
		}
	}
	r := cliTool("read", "completed")
	delete(cliState(r), "output")
	got, err := DecodeCLIRoot(strings.NewReader(events(r)), cliOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.Observations[0].Size != nil || got.Diagnostics.UnknownReadSizes != 1 {
		t.Fatal("absent read output became zero")
	}
}

func TestCLIRootCaptureBoundsAndNestedDuplicateKeys(t *testing.T) {
	input := events(cliTool("read", "completed"))
	bad := strings.Replace(input, `"status":"completed"`, `"status":"completed","status":"error"`, 1)
	if got, err := DecodeCLIRoot(strings.NewReader(bad), cliOptions(t)); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
		t.Fatal("nested duplicate key accepted")
	}
	bad = strings.Repeat(" ", maxCLILineBytes+1) + "\n"
	got, err := DecodeCLIRoot(strings.NewReader(bad), cliOptions(t))
	var reason *CLIRootError
	if !errors.As(err, &reason) || reason.Code != "record_size_invalid" || !reflect.DeepEqual(got, CLIRootImport{}) {
		t.Fatal("record bound missing")
	}
	for _, bad := range []string{input + "{}{}\n", input + "\xff\n"} {
		if got, err := DecodeCLIRoot(strings.NewReader(bad), cliOptions(t)); err == nil || !reflect.DeepEqual(got, CLIRootImport{}) {
			t.Fatal("trailing data or invalid UTF-8 accepted")
		}
	}
}
