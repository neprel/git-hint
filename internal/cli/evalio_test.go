package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// declareRepo commits a base (tool.sh prints its input) and a reference (upper case) in the
// fixture repository and returns both revisions.
func declareRepo(t *testing.T, shop string) (string, string) {
	t.Helper()
	os.WriteFile(filepath.Join(shop, "go.mod"), []byte("module shop\n\ngo 1.26\n"), 0o644)
	os.WriteFile(filepath.Join(shop, "tool.sh"), []byte("cat\n"), 0o644)
	os.WriteFile(filepath.Join(shop, "sample.txt"), []byte("in the base\n"), 0o644)
	testkit.Git(t, shop, "add", "go.mod", "tool.sh", "sample.txt")
	testkit.Git(t, shop, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(testkit.Git(t, shop, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(shop, "tool.sh"), []byte("tr a-z A-Z\n"), 0o644)
	os.WriteFile(filepath.Join(shop, "ref-only.txt"), []byte("only in the reference\n"), 0o644)
	testkit.Git(t, shop, "add", "tool.sh", "ref-only.txt")
	testkit.Git(t, shop, "commit", "-q", "-m", "reference")
	return base, strings.TrimSpace(testkit.Git(t, shop, "rev-parse", "HEAD"))
}

// 07B-J §19: eval declare-io packages a cli-file-v1 eval from read-only revisions, a JSON argv
// entry and local case files; it never changes the work tree.
func TestEvalDeclareIO(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	base, refRev := declareRepo(t, shop)
	head := testkit.Git(t, shop, "status", "--porcelain")
	dir := t.TempDir()
	w := func(name, s string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	req := w("request.txt", "tool.sh must print its input in upper case.\n")
	lower, mixed := w("lower.in", "hello\n"), w("mixed.in", "Ab c\nx")
	link := filepath.Join(dir, "link.in")
	os.Symlink(lower, link)
	r := must(t, runEval(t, shop, &spawned, "eval", "declare-io", "--base", base, "--reference", refRev, "--entry", `["sh","tool.sh"]`,
		"--case", "lower="+link, "--case", "mixed="+mixed, "--request", req))
	for _, want := range []string{"cli-file-v1, 2 cases", "finite reference-equivalence", "git hint eval map", "runner_unsupported"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	if testkit.Git(t, shop, "status", "--porcelain") != head {
		t.Fatal("the work tree changed")
	}
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	var found map[string]any
	for _, e := range latestEvals(l) {
		var rec map[string]any
		if l.Manifest("eval", e.Ref, &rec) == nil {
			if v, _ := rec["verifier"].(map[string]any); v["kind"] == "cli-file-v1" {
				found = rec
			}
		}
	}
	if found == nil {
		t.Fatal("no cli-file-v1 eval stored")
	}
	// eval map offers the declared cases and links the request to B1
	out := must(t, runEval(t, shop, &spawned, "eval", "map", found["eval_id"].(string))).stdout
	var f pipeline.MapFile
	if err := json.Unmarshal([]byte(out), &f); err != nil || strings.Join(f.Tests, ",") != "cases/lower::output,cases/mixed::output" {
		t.Fatalf("map tests: %v %s", err, out)
	}
	f.Requirements[0].Quote, f.Requirements[0].Tests = "tool.sh must print its input in upper case", []string{"cases/lower::output", "cases/mixed::output"}
	mf := filepath.Join(t.TempDir(), "map.json")
	mb, _ := json.Marshal(f)
	os.WriteFile(mf, mb, 0o600)
	must(t, runEval(t, shop, &spawned, "eval", "map", found["eval_id"].(string), "--apply", mf))
	var v2 map[string]any
	if l.Manifest("eval", found["eval_id"].(string)+"@2", &v2) != nil {
		t.Fatal("no mapped version")
	}
	linked := false
	for _, r := range v2["requirements"].([]any) {
		m := r.(map[string]any)
		if m["origin"] == "request" && fmt.Sprint(m["checks"]) == "[B1]" && m["mapping"] == "user" {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("requirements %v", v2["requirements"])
	}
	show := must(t, runEval(t, shop, &spawned, "eval", "show", found["eval_id"].(string))).stdout
	if !strings.Contains(show, "finite reference-equivalence") {
		t.Fatalf("show: %s", show)
	}
	os.WriteFile(filepath.Join(shop, "ignored-case.in"), []byte("Ab c\nx"), 0o644) // a local file no snapshot holds
	big := w("big.in", strings.Repeat("x", 1<<20+1))
	for name, args := range map[string][]string{
		"entry not JSON":        {"--entry", "sh tool.sh"},
		"entry object":          {"--entry", `{"a":1}`},
		"input in the base":     {"--case", "c=" + filepath.Join(shop, "sample.txt")},
		"input via a link":      {"--case", "c=" + mustLink(t, filepath.Join(shop, "sample.txt"))},
		"input only in the ref": {"--case", "c=" + filepath.Join(shop, "ref-only.txt")},
		"oversize input":        {"--case", "c=" + big},
		"missing input":         {"--case", "c=" + filepath.Join(dir, "absent.in")},
		"no ID":                 {"--case", "=" + lower},
		"unsafe ID":             {"--case", "../x=" + lower},
		"option as revision":    {"--base", "--all"},
		"unknown revision":      {"--base", "no-such-rev"},
		"long timeout":          {"--case-seconds", "31"},
		"empty request":         {"--request", w("empty.txt", " \n")},
	} {
		full := map[string]string{"--base": base, "--reference": refRev, "--entry": `["sh","tool.sh"]`, "--request": req, "--case-seconds": "5"}
		cs := []string{"x=" + lower}
		for i := 0; i < len(args); i += 2 {
			if args[i] == "--case" {
				cs = []string{args[i+1]}
			} else {
				full[args[i]] = args[i+1]
			}
		}
		a := []string{"eval", "declare-io"}
		for k, v := range full {
			a = append(a, k, v)
		}
		for _, c := range cs {
			a = append(a, "--case", c)
		}
		if r := runEval(t, shop, &spawned, a...); r.code == 0 {
			t.Errorf("%s: accepted:\n%s", name, r.stdout)
		}
	}
	must(t, runEval(t, shop, &spawned, "eval", "declare-io", "--base", base, "--reference", refRev, "--entry", `["sh","tool.sh"]`,
		"--case", "c="+filepath.Join(shop, "ignored-case.in"), "--request", req))
}

func mustLink(t *testing.T, target string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "l.in")
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// C7BJ-10: the identity of a declaration is canonical: an identical retry at another time,
// also with other JSON formatting of the entry, finds the existing eval and writes nothing;
// after eval map the retry keeps the mapped version and does not claim R1 is unmapped; a
// changed limit, request or family is another declaration; the old record stays unchanged.
func TestEvalDeclareIORetryAndIdentity(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	base, refRev := declareRepo(t, shop)
	dir := t.TempDir()
	w := func(name, s string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	req, other := w("request.txt", "tool.sh must print its input in upper case.\n"), w("other.txt", "tool.sh must shout its input.\n")
	in := w("c.in", "hi\n")
	at := func(d time.Duration, entry, reqFile string, extra ...string) result {
		a := append([]string{"eval", "declare-io", "--base", base, "--reference", refRev, "--entry", entry, "--case", "c=" + in, "--request", reqFile}, extra...)
		var out, errb bytes.Buffer
		home := filepath.Join(shop, ".test-home")
		env := &Env{Dir: shop, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Now: func() time.Time { return time.Now().Add(d) },
			Spawn: func(a ...string) error { spawned = append(spawned, strings.Join(a, " ")); return nil },
			EvalEnv: func() pipeline.Env {
				return pipeline.Env{Home: home, DataHome: filepath.Join(home, "data"), Cmd: &noAgents{}, Now: time.Now}
			}}
		return result{Run(a, env), out.String(), errb.String()}
	}
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	manifests := func() []string { n, _ := l.Manifests("eval"); return n }
	must(t, at(0, `["sh","tool.sh"]`, req))
	names := manifests()
	if len(names) != 1 {
		t.Fatalf("evals %v", names)
	}
	first := names[0]
	var firstRec map[string]any
	l.Manifest("eval", first, &firstRec)
	firstBytes, _ := json.Marshal(firstRec)
	r := must(t, at(time.Hour, ` [ "sh" ,  "tool.sh" ] `, req))
	if !strings.Contains(r.stdout, "exists already: eval "+first) || len(manifests()) != 1 {
		t.Fatalf("retry: %s %v", r.stdout, manifests())
	}
	// map R1, then retry: the mapped version stays the latest, and R1 is not called unmapped
	id, _, _ := strings.Cut(first, "@")
	out := must(t, runEval(t, shop, &spawned, "eval", "map", id)).stdout
	var f pipeline.MapFile
	json.Unmarshal([]byte(out), &f)
	f.Requirements[0].Quote, f.Requirements[0].Tests = "tool.sh must print its input in upper case", []string{"cases/c::output"}
	mf := filepath.Join(t.TempDir(), "map.json")
	mb, _ := json.Marshal(f)
	os.WriteFile(mf, mb, 0o600)
	must(t, runEval(t, shop, &spawned, "eval", "map", id, "--apply", mf))
	r = must(t, at(2*time.Hour, `["sh","tool.sh"]`, req))
	if !strings.Contains(r.stdout, "exists already: eval "+id+"@2") || strings.Contains(r.stdout, "not linked") || len(manifests()) != 2 {
		t.Fatalf("retry after map: %s %v", r.stdout, manifests())
	}
	// semantic changes are other declarations
	ids := map[string]bool{id: true}
	for name, rr := range map[string]result{
		"limit":   at(0, `["sh","tool.sh"]`, req, "--case-seconds", "11"),
		"request": at(0, `["sh","tool.sh"]`, other),
		"family":  at(0, `["sh","tool.sh"]`, req, "--family", "feature"),
		"argv":    at(0, `["sh","./tool.sh"]`, req),
	} {
		if rr.code != 0 || strings.Contains(rr.stdout, "exists already") {
			t.Fatalf("%s: %+v", name, rr)
		}
		for _, n := range manifests() {
			ids[strings.SplitN(n, "@", 2)[0]] = true
		}
	}
	if len(ids) != 5 {
		t.Fatalf("eval IDs %v", ids)
	}
	var now map[string]any
	l.Manifest("eval", first, &now)
	if b, _ := json.Marshal(now); !bytes.Equal(b, firstBytes) {
		t.Fatal("the first record changed")
	}
}
