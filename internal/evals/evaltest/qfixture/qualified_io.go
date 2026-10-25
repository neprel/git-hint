// Synthetic stored-q3 fixture inputs only. No engine runs, qualification
// attestation, project completion or prospective-source authority exists here.
package qfixture

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/iodecl"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"io"
	"sort"
	"strings"
	"testing"
	"time"
)

type QualifiedIOFixture struct {
	L                                                *library.Library
	EvalName, ManifestDigest, PreviousManifestDigest string
	Record                                           map[string]any
	Files                                            map[string][]byte
}

func syntheticArchive(t testing.TB, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	keys := make([]string, 0, len(files))
	for n := range files {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		v := files[n]
		if e := w.WriteHeader(&tar.Header{Name: n, Mode: 0600, Size: int64(len(v)), Typeflag: tar.TypeReg}); e != nil {
			t.Fatal(e)
		}
		if _, e := w.Write(v); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func syntheticObject(t testing.TB, l *library.Library, b []byte) string {
	t.Helper()
	d, e := l.PutObject(b)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func syntheticArchiveFiles(b []byte) (map[string][]byte, error) {
	r := tar.NewReader(bytes.NewReader(b))
	out := map[string][]byte{}
	for {
		h, e := r.Next()
		if e == io.EOF {
			return out, nil
		}
		if e != nil {
			return nil, e
		}
		out[h.Name], e = io.ReadAll(r)
		if e != nil {
			return nil, e
		}
	}
}

// QualifiedIO constructs actual stored manifest/control objects through BuildIO.
// Mutations remain synthetic and resolve through the production input resolver.
func QualifiedIO(t testing.TB, l *library.Library, before func(*library.Library, map[string]any), edit func(*QualifiedIOFixture)) *QualifiedIOFixture {
	t.Helper()
	base := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "cat\n"})
	result := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
	built, e := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Family: "bug_fix", Repo: "example", Base: base, Result: result, Request: "Uppercase stdin.", RequestAt: map[string]any{"source": library.Digest([]byte("synthetic-source")), "record": "request1"}, Entry: []string{"sh", "tool.sh"}, Cases: []build.IOCase{{ID: "lower", Input: []byte("hello\n")}}, CaseSeconds: 5, Now: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)})
	if e != nil {
		t.Fatal(e)
	}
	rec := built.Record
	for _, v := range rec["requirements"].([]any) {
		req := v.(map[string]any)
		req["checks"] = []any{"B1"}
		req["mapping"] = "construction"
	}
	if before != nil {
		before(l, rec)
	}
	name, e := l.PutManifest("eval", rec)
	if e != nil {
		t.Fatal(e)
	}
	pd, e := l.ManifestDigest("eval", name)
	if e != nil {
		t.Fatal(e)
	}
	// Work on the exact JSON types read from the immutable predecessor.
	if e = l.Manifest("eval", name, &rec); e != nil {
		t.Fatal(e)
	}
	vd := rec["verifier"].(map[string]any)["archive"].(string)
	vb, e := l.GetObject(vd)
	if e != nil {
		t.Fatal(e)
	}
	vf, e := syntheticArchiveFiles(vb)
	if e != nil {
		t.Fatal(e)
	}
	decl, e := iodecl.Parse(vf["io/spec.json"], map[string][]byte{"lower": []byte("hello\n")})
	if e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{}
	artifact := syntheticArchive(t, map[string][]byte{"app/tool.sh": []byte("tr a-z A-Z\n")})
	for _, group := range []string{"base", "reference", "probe"} {
		n := 1
		if group == "reference" {
			n = 3
		}
		for i := 1; i <= n; i++ {
			trial := fmt.Sprintf("%s__%d", group, i)
			dir := "jobs/" + group + "/" + trial + "/"
			reward := 0
			check := "FAIL"
			if group == "reference" {
				reward = 1
				check = "PASS"
			}
			raw := map[string]any{"trial_name": trial, "verifier_result": map[string]any{"rewards": map[string]any{"reward": reward}}, "hint_container": map[string]any{"runner": runner.DockerRunnerVersion, "result_trust": runner.TrustProcessSeparated, "check_trust": map[string]string{"B1": runner.TrustProcessSeparated}, "io": map[string]any{"interface": iodecl.Interface, "spec": decl.SpecDigest, "inputs": decl.InputsDigest, "oracle_runs": runner.OracleRuns, "verdict_scope": "equivalence_to_reference_on_declared_cases", "network": "engine_inspect:blocked", "reference_tree": library.Digest(artifact), "oracle_outputs": library.Digest([]byte("HELLO\n")), "candidate_outputs": library.Digest([]byte(group))}}}
			files[dir+"result.json"], _ = json.Marshal(raw)
			files[dir+"verifier/test-stdout.txt"] = []byte("CHECK B1 " + check + "\nTEST B1 cases/lower::output " + check + "\n")
			files[dir+"oracle/final.tar"] = artifact
			if group == "probe" {
				files[dir+"agent/oracle.txt"] = []byte("HINT_PROBE_NET=blocked\n")
				files[dir+"verifier/network.txt"] = []byte("HINT_VERIFIER_NET=blocked\n")
			}
		}
	}
	rec["version"], rec["previous"], rec["state"] = float64(2), name, "validated"
	f := &QualifiedIOFixture{L: l, EvalName: strings.Split(name, "@")[0] + "@2", PreviousManifestDigest: pd, Record: rec, Files: files}
	if edit != nil {
		edit(f)
	}
	archive := syntheticObject(t, l, syntheticArchive(t, f.Files))
	var cs []any
	for _, n := range []string{"leak_probe", "base_fails", "reference_passes", "repeatable", "near_miss_fails", "requirement_coverage", "forgery_probe", "isolation"} {
		runs := 1
		if n == "reference_passes" || n == "repeatable" || n == "requirement_coverage" {
			runs = 3
		}
		cs = append(cs, map[string]any{"name": n, "status": "pass", "runs": runs, "evidence": []any{archive}})
	}
	cs = append(cs, map[string]any{"name": "alternative_passes", "status": "missing", "runs": 0})
	f.Record["qualification"] = map[string]any{"policy": "q3", "result_trust": runner.TrustProcessSeparated, "checks": cs, "requirements": []any{map[string]any{"req_id": "R1", "status": "pass"}, map[string]any{"req_id": "R2", "status": "pass"}}}
	// Tests can edit qualification after source archive creation through rec metadata below.
	if special, ok := f.Record["_edit_qualification"].(func(map[string]any)); ok {
		delete(f.Record, "_edit_qualification")
		special(f.Record["qualification"].(map[string]any))
	}
	if _, e = l.PutManifest("eval", f.Record); e != nil {
		t.Fatal(e)
	}
	f.ManifestDigest, e = l.ManifestDigest("eval", f.EvalName)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
