package conformance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

const root = "../.."

var schemas *hintconf.Schemas

func TestMain(m *testing.M) {
	var err error
	schemas, err = hintconf.LoadSchemas(filepath.Join(root, "schemas", "v0.1"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func load(t *testing.T, path string) (any, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := hintconf.Decode(b)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	doc, err := hintconf.Plain(dec)
	if err != nil {
		t.Fatal(err)
	}
	return dec, doc
}

func policy(t *testing.T) map[string]any {
	_, p := load(t, filepath.Join(root, "examples", "v0.1", "export-policy.json"))
	return p["payload"].(map[string]any)
}

func validFiles(t *testing.T) []string {
	a, _ := filepath.Glob(filepath.Join(root, "examples", "v0.1", "*.json"))
	b, _ := filepath.Glob(filepath.Join("valid", "*.json"))
	if len(a) < 9 || len(b) < 4 {
		t.Fatalf("missing fixtures: %d examples, %d valid", len(a), len(b))
	}
	return append(a, b...)
}

// --- schema properties -------------------------------------------------------

// TestNoFreeFormStringSlot walks every schema: each string slot must be closed by an
// enum, a const or a pattern, and each object definition must forbid other fields.
func TestNoFreeFormStringSlot(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(root, "schemas", "v0.1", "*.schema.json"))
	for _, f := range files {
		var s any
		readJSON(t, f, &s)
		var walk func(v any, path string)
		walk = func(v any, path string) {
			switch n := v.(type) {
			case map[string]any:
				isString := n["type"] == "string"
				if l, ok := n["type"].([]any); ok {
					for _, x := range l {
						isString = isString || x == "string"
					}
				}
				if isString && n["enum"] == nil && n["const"] == nil && n["pattern"] == nil {
					t.Errorf("%s %s: free-form string", filepath.Base(f), path)
				}
				// An object definition (type object) must be closed; refinements inside
				// if/then/allOf only narrow an already closed object.
				if n["type"] == "object" && n["additionalProperties"] != false && n["propertyNames"] == nil &&
					!strings.HasSuffix(path, "/properties/payload") {
					t.Errorf("%s %s: object without additionalProperties=false", filepath.Base(f), path)
				}
				for k, x := range n {
					walk(x, path+"/"+k)
				}
			case []any:
				for i, x := range n {
					walk(x, fmt.Sprintf("%s/%d", path, i))
				}
			}
		}
		walk(s, "")
	}
}

func TestValidDocuments(t *testing.T) {
	pol := policy(t)
	for _, f := range validFiles(t) {
		dec, doc := load(t, f)
		if err := schemas.Validate(dec); err != nil {
			t.Errorf("%s: schema: %v", f, err)
			continue
		}
		var p map[string]any
		if red, _ := doc["redaction"].(map[string]any); red["profile"] == "detailed" {
			p = pol // the default policy covers the detailed examples
		}
		if fs := hintconf.Check(doc, p); len(fs) > 0 {
			t.Errorf("%s: semantic: %v", f, fs)
		}
	}
}

func TestInvalidDocuments(t *testing.T) {
	var expected map[string]struct{ Level, Code, Base string }
	readJSON(t, filepath.Join("invalid", "expected.json"), &expected)
	files, _ := filepath.Glob(filepath.Join("invalid", "*.json"))
	if len(files) != len(expected)+1 {
		t.Fatalf("%d fixture files, %d expectations", len(files)-1, len(expected))
	}
	for name, e := range expected {
		dec, doc := load(t, filepath.Join("invalid", name))
		err := schemas.Validate(dec)
		switch e.Level {
		case "schema":
			if err == nil {
				t.Errorf("%s: schema accepted an invalid document", name)
			}
		case "semantic":
			if err != nil {
				t.Errorf("%s: must be schema-valid (only the semantic rule fails): %v", name, err)
				continue
			}
			found := false
			for _, f := range hintconf.Check(doc, nil) {
				found = found || f.Code == e.Code
			}
			if !found {
				t.Errorf("%s: semantic code %s not reported: %v", name, e.Code, hintconf.Check(doc, nil))
			}
		}
	}
}

// TestSignedDeltasStayValid guards the other side of the non-negative quantity rule:
// forecast intervals, forecast points and effect estimates are signed differences, so
// negative values must stay valid there.
func TestSignedDeltasStayValid(t *testing.T) {
	_, rec := load(t, filepath.Join(root, "examples", "v0.1", "recommendation.json"))
	_, eff := load(t, filepath.Join(root, "examples", "v0.1", "effect-report.json"))
	plan := rec["payload"].(map[string]any)["plans"].([]any)[0].(map[string]any)
	pred := plan["predictions"].([]any)[0].(map[string]any)
	pred["interval"] = map[string]any{"lower": -9000.0, "upper": -1000.0, "level": 0.9}
	pred["point"] = -3000.0
	e := eff["payload"].(map[string]any)["effects"].([]any)[0].(map[string]any)
	e["estimate"] = map[string]any{"lower": -5200.0, "upper": -100.0, "level": 0.9}
	for _, doc := range []map[string]any{rec, eff} {
		d, err := hintconf.Digest(doc["payload"])
		if err != nil {
			t.Fatal(err)
		}
		doc["content_digest"] = d
		if err := schemas.Validate(doc); err != nil {
			t.Errorf("%s: a negative delta must stay schema-valid: %v", doc["document_type"], err)
		}
		if fs := hintconf.Check(doc, nil); len(fs) > 0 {
			t.Errorf("%s: semantic: %v", doc["document_type"], fs)
		}
	}
}

// TestForbiddenContentInEverySlot replaces every string of every valid document with a
// sample of each forbidden content class, and adds each sample as an extra field to every
// object. The schema must reject every variant (privacy contract, hint-data §8).
func TestForbiddenContentInEverySlot(t *testing.T) {
	samples := []string{
		"func canarySymbolQ7() { return }", "canarySymbolQ7", "src/canary_dir_e1d2/main.go", "/home/dev/shopkit",
		"C:\\work\\shopkit", "// HINTCANARY_REPO_7f3a91", "\"HINTCANARY_REPO_7f3a91\"", "HINTCANARY_PROMPT_9d1e07 fix it",
		"cat x | grep HINTCANARY_ARG_2b8c44", "HINTCANARY_TOOLOUT_5a6f32", "HINT_CANARY_ENV=HINTCANARY_ENV_c4e819",
		"AKIAHINTCANARY000000", "https://git.example.invalid/org/repo.git", "build01.example.invalid", "origin",
		"jdoe", "dev@example.invalid", "feature/canary", "Fix canary bug", "@private-org/billing-core",
	}
	for _, f := range validFiles(t) {
		_, doc := load(t, f)
		var paths [][]any
		var objs [][]any
		collect(doc, nil, &paths, &objs)
		checked := 0
		for _, p := range paths {
			for _, s := range samples {
				variant := set(clone(doc), p, s)
				if schemas.Validate(variant) == nil {
					t.Errorf("%s: %v accepted %q", f, p, s)
				}
				checked++
			}
		}
		for _, p := range objs {
			variant := clone(doc)
			at(variant, p).(map[string]any)["note"] = samples[7]
			if schemas.Validate(variant) == nil {
				t.Errorf("%s: extra field accepted at %v", f, p)
			}
		}
		if checked == 0 {
			t.Errorf("%s: no string slot checked", f)
		}
	}
}

func collect(v any, path []any, strs, objs *[][]any) {
	switch n := v.(type) {
	case map[string]any:
		*objs = append(*objs, append([]any{}, path...))
		for k, x := range n {
			collect(x, append(append([]any{}, path...), k), strs, objs)
		}
	case []any:
		for i, x := range n {
			collect(x, append(append([]any{}, path...), i), strs, objs)
		}
	case string:
		*strs = append(*strs, append([]any{}, path...))
	}
}

func at(v any, path []any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

func set(doc map[string]any, path []any, val any) map[string]any {
	parent := at(doc, path[:len(path)-1])
	switch k := path[len(path)-1].(type) {
	case string:
		parent.(map[string]any)[k] = val
	case int:
		parent.([]any)[k] = val
	}
	return doc
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// --- identifiers and digests ----------------------------------------------------

func TestIDVectors(t *testing.T) {
	var v struct {
		Keys    map[string]string
		Vectors []struct {
			Key, Domain, ID string
			Parts           []string
			Bytes           int
		}
	}
	readJSON(t, filepath.Join("vectors", "ids-v0.1.json"), &v)
	keys := map[string][]byte{}
	for k, h := range v.Keys {
		b, _ := hexDecode(h)
		keys[k] = b
	}
	got := map[string]string{}
	for _, x := range v.Vectors {
		var id string
		switch x.Domain {
		case "trace":
			id, _ = hintconf.TraceID(keys[x.Key], x.Parts[0])
		case "span":
			id, _ = hintconf.SpanID(keys[x.Key], x.Parts[0], x.Parts[1])
		default:
			id = hintconf.DeriveID(keys[x.Key], x.Domain, x.Parts...)
		}
		if id != x.ID {
			t.Errorf("%s %s %v: %s != %s", x.Key, x.Domain, x.Parts, id, x.ID)
		}
		got[x.Key+"/"+x.Domain+"/"+strings.Join(x.Parts, "|")] = id
	}
	r1 := hintconf.RepoID(keys["K1"], "shopkit")
	pairs := [][2]string{
		{"K1/namespace/", "K2/namespace/"},
		{"K1/repo/shopkit", "K2/repo/shopkit"},  // same name, unrelated namespaces
		{"K1/repo/shopkit", "K1/repo/shop-kit"}, // similar names
		{"K1/file/" + r1 + "|billing/ledger.go", "K1/file/" + r1 + "|billing/ledger_core.go"},
		{"K1/contract/idl_service|shopkit.billing.v1.BillingService", "K2/contract/idl_service|shopkit.billing.v1.BillingService"},
		{"K1/trace/4bf92f3577b34da6a3ce929d0e0e4736", "K2/trace/4bf92f3577b34da6a3ce929d0e0e4736"},
		{"K1/span/4bf92f3577b34da6a3ce929d0e0e4736|00f067aa0ba902b7", "K2/span/4bf92f3577b34da6a3ce929d0e0e4736|00f067aa0ba902b7"},
		{"K1/span/4bf92f3577b34da6a3ce929d0e0e4736|00f067aa0ba902b7", "K1/span/5bf92f3577b34da6a3ce929d0e0e4736|00f067aa0ba902b7"},
	}
	for _, p := range pairs {
		if got[p[0]] == "" || got[p[0]] == got[p[1]] {
			t.Errorf("identifiers must differ: %s vs %s", p[0], p[1])
		}
	}
}

func TestDigestVectors(t *testing.T) {
	var v struct {
		Vectors []struct {
			Value     any
			Canonical string
			Digest    string
		}
	}
	readJSON(t, filepath.Join("vectors", "digests-v0.1.json"), &v)
	for _, x := range v.Vectors {
		c, _ := hintconf.Canonical(x.Value)
		d, _ := hintconf.Digest(x.Value)
		if string(c) != x.Canonical || d != x.Digest {
			t.Errorf("%v: %s %s", x.Value, c, d)
		}
	}
}

// TestStableIDsAcrossRescanAndRename: an unchanged rescan gives the same files (the
// generator is deterministic and the committed files are current); after a file rename,
// symbol IDs stay, the file ID changes, and the change set links old and new file IDs.
func TestStableIDsAcrossRescanAndRename(t *testing.T) {
	_, before := load(t, filepath.Join(root, "examples", "v0.1", "single-repo.json"))
	_, after := load(t, filepath.Join(root, "examples", "v0.1", "single-repo-after-rename.json"))
	_, changes := load(t, filepath.Join(root, "examples", "v0.1", "change-set.json"))
	ids := func(doc map[string]any) map[string]string {
		m := map[string]string{}
		for _, e := range doc["payload"].(map[string]any)["entities"].([]any) {
			em := e.(map[string]any)
			m[em["entity_id"].(string)] = em["kind"].(string)
		}
		return m
	}
	b, a := ids(before), ids(after)
	var stable, onlyBefore, onlyAfter []string
	for id, k := range b {
		if _, ok := a[id]; ok && k == "function" {
			stable = append(stable, id)
		} else if !ok {
			onlyBefore = append(onlyBefore, k)
		}
	}
	for id, k := range a {
		if _, ok := b[id]; !ok {
			onlyAfter = append(onlyAfter, k)
		}
	}
	sort.Strings(onlyBefore)
	sort.Strings(onlyAfter)
	if len(stable) != 3 { // Apply, Rate (moved to another file) and Format
		t.Errorf("stable function IDs: %d", len(stable))
	}
	if strings.Join(onlyBefore, ",") != "block,file,file,function" || strings.Join(onlyAfter, ",") != "block,file,function,function" {
		t.Errorf("changed IDs: before %v, after %v", onlyBefore, onlyAfter)
	}
	for _, c := range changes["payload"].(map[string]any)["changes"].([]any) {
		cm := c.(map[string]any)
		bef, aft := cm["before"].([]any), cm["after"].([]any)
		switch cm["operation"] {
		case "RENAME":
			if b[bef[0].(string)] != "file" || a[aft[0].(string)] != "file" || bef[0] == aft[0] {
				t.Errorf("RENAME must link two different file IDs")
			}
		case "MOVE":
			if bef[0] != aft[0] {
				t.Errorf("a moved symbol keeps its ID")
			}
		}
	}
}

func TestGeneratedFilesAreCurrent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.CopyFS(filepath.Join(tmp, "schemas"), os.DirFS(filepath.Join(root, "schemas"))); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./cmd/genexamples", "-root", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	cmp := func(gen, committed string) {
		files, _ := filepath.Glob(filepath.Join(gen, "*.json"))
		for _, f := range files {
			want, _ := os.ReadFile(filepath.Join(committed, filepath.Base(f)))
			got, _ := os.ReadFile(f)
			if !bytes.Equal(want, got) {
				t.Errorf("%s is stale; run go run ./cmd/genexamples -root ../..", filepath.Join(committed, filepath.Base(f)))
			}
		}
		if len(files) == 0 {
			t.Errorf("nothing generated in %s", gen)
		}
	}
	cmp(filepath.Join(tmp, "examples", "v0.1"), filepath.Join(root, "examples", "v0.1"))
	for _, d := range []string{"valid", "invalid", "resolve", "resolve/cases", "vectors"} {
		cmp(filepath.Join(tmp, "tests", "conformance", d), d)
	}
}

// TestCrossRepoContract: two repositories share an opaque contract ID only through a
// contract entity with contract evidence; their repository IDs differ.
func TestCrossRepoContract(t *testing.T) {
	_, doc := load(t, filepath.Join(root, "examples", "v0.1", "multi-repo.json"))
	p := doc["payload"].(map[string]any)
	contracts := map[string]map[string]bool{}
	for _, e := range p["entities"].([]any) {
		em := e.(map[string]any)
		if em["kind"] == "contract" {
			id := em["entity_id"].(string)
			if contracts[id] == nil {
				contracts[id] = map[string]bool{}
			}
			contracts[id][em["repo_id"].(string)] = true
		}
	}
	if len(contracts) != 1 {
		t.Fatalf("contracts: %v", contracts)
	}
	for _, repos := range contracts {
		if len(repos) != 2 {
			t.Errorf("the contract must appear in both repositories: %v", repos)
		}
	}
	refs := 0
	for _, r := range p["relations"].([]any) {
		rm := r.(map[string]any)
		if rm["kind"] == "contract_ref" {
			refs++
			if rm["contract_evidence"] == nil {
				t.Errorf("contract_ref without evidence")
			}
		}
	}
	if refs != 2 {
		t.Errorf("contract_ref relations: %d", refs)
	}
}

func hexDecode(s string) ([]byte, error) {
	b := make([]byte, len(s)/2)
	_, err := fmt.Sscanf(s, "%x", &b)
	return b, err
}
