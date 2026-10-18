package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/mcpserver"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func ctxItems(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, x := range m["items"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func itemBy(t *testing.T, m map[string]any, rel, name string) map[string]any {
	t.Helper()
	for _, it := range ctxItems(m) {
		if it["relation"] == rel && (name == "" || strings.HasSuffix(str(it["qualified_name"]), name) || strings.HasSuffix(str(it["path"]), name)) {
			return it
		}
	}
	t.Fatalf("no %s item %q in %v", rel, name, m["items"])
	return nil
}

func str(v any) string { s, _ := v.(string); return s }

// D05: a small useful answer around a declaration, and the expansion of a chosen detail.
func TestContextAroundADeclarationAndExpansion(t *testing.T) {
	shop, _ := project_(t)
	gh(t, shop, "collect", "--full")
	s, _ := store.Open(shop)
	before := listState(t, s.Path())
	cs := session(t, mcpserver.Options{Dir: shop})
	m := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	if m["side_effect"] != "none" || m["action"] != "deliver" || m["result"] != "delivered" || m["freshness"] != "current" {
		t.Fatalf("answer: %v", m)
	}
	def := ctxItems(m)[0]
	if def["relation"] != "definition" || def["path"] != "canary_dir_e1d2/billing/ledger.go" || def["line"].(float64) != 7 ||
		def["source_state"] != "returned" || !strings.HasPrefix(str(def["source"]), "func Rate(a, b int) int {") || !strings.HasSuffix(str(def["source"]), "}\n") {
		t.Fatalf("definition: %v", def)
	}
	if strings.Contains(str(def["source"]), "HINTCANARY_KEY") {
		t.Fatal("the excerpt holds more than the declaration")
	}
	apply := itemBy(t, m, "same_file", "CanaryLedgerK3.Apply")
	if apply["source_state"] != "not_returned" || apply["source"] != nil {
		t.Fatalf("a neighbor carries source: %v", apply)
	}
	itemBy(t, m, "defined_in", "ledger.go")
	if tst := itemBy(t, m, "test", "TestRate"); tst["source_state"] != "returned" || !strings.Contains(str(tst["source"]), "Rate(1, 2)") {
		t.Fatalf("test: %v", tst)
	}
	itemBy(t, m, "imported_by", "cmd/shop/main.go")
	snap := m["snapshot"].(map[string]any)
	if snap["latest"] != true || str(snap["digest"]) == "" || str(m["artifact_id"]) == "" {
		t.Fatalf("snapshot identity: %v %v", snap, m["artifact_id"])
	}
	used := m["used"].(map[string]any)
	raw, _ := json.Marshal(m["items"])
	if int(used["item_bytes"].(float64)) != len(raw) || used["bytes"].(float64) > 16384 {
		t.Fatalf("bytes: %v, items %d", used, len(raw))
	}
	ctxExact(t, cs, map[string]any{"seed": "Rate"}, 16384)
	ms := m["measurement"].(map[string]any)
	if ms["action"] != "deliver" || ms["stage"] != "delivered_context" || ms["result"] != "delivered" || ms["artifact_id"] != m["artifact_id"] ||
		ms["snapshot_id"] != snap["snapshot_id"] || ms["policy_state"] != "not_applicable" || ms["bytes"] != used["bytes"] ||
		ms["tokens"].(map[string]any)["label"] != "estimate" || ms["ref_id"] != nil {
		t.Fatalf("measurement: %v", ms)
	}
	// the same request gives the same artifact
	if again := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"}); again["artifact_id"] != m["artifact_id"] {
		t.Fatal("artifact ID is not deterministic")
	}
	// expand one detail
	x := mustCall(t, cs, "hint_context", map[string]any{"ref": apply["ref"]})
	xd := ctxItems(x)[0]
	if x["action"] != "expand" || x["result"] != "delivered" || xd["relation"] != "definition" ||
		!strings.Contains(str(xd["source"]), "func (l *CanaryLedgerK3) Apply(x int)") {
		t.Fatalf("expansion: %v", x)
	}
	if xm := x["measurement"].(map[string]any); xm["action"] != "expand" || len(str(xm["ref_id"])) != 32 || strings.Contains(str(xm["ref_id"]), ".") {
		t.Fatalf("expansion measurement: %v", xm)
	}
	// a file and a package directory as the start
	f := mustCall(t, cs, "hint_context", map[string]any{"seed": "canary_dir_e1d2/billing/ledger.go"})
	if it := ctxItems(f)[0]; it["relation"] != "start" || it["kind"] != "file" {
		t.Fatalf("file start: %v", f)
	}
	itemBy(t, f, "declares", "billing.Rate")
	itemBy(t, f, "test_file", "ledger_test.go")
	d := mustCall(t, cs, "hint_context", map[string]any{"seed": "canary_dir_e1d2/billing"})
	itemBy(t, d, "imported_by", "cmd/shop/main.go")
	if after := listState(t, s.Path()); after != before {
		t.Fatal("hint_context changed the store")
	}
}

// D05: limits on items, bytes and depth; a cycle stays bounded and every item appears once.
func TestContextLimitsAndCycles(t *testing.T) {
	shop, _ := project_(t)
	testkit.WriteFile(t, shop, "canary_dir_e1d2/aa/a.go", "package aa\n\nimport _ \"example.invalid/shop/canary_dir_e1d2/bb\"\n\nfunc A() {}\n")
	testkit.WriteFile(t, shop, "canary_dir_e1d2/bb/b.go", "package bb\n\nimport _ \"example.invalid/shop/canary_dir_e1d2/aa\"\n\nfunc B() {}\n")
	gh(t, shop, "collect", "--full")
	cs := session(t, mcpserver.Options{Dir: shop})
	for _, depth := range []int{1, 2} {
		m := mustCall(t, cs, "hint_context", map[string]any{"seed": "aa.A", "depth": depth, "max_entities": 100})
		seen := map[string]bool{}
		for _, it := range ctxItems(m) {
			if seen[str(it["ref"])] {
				t.Fatalf("depth %d: item twice: %v", depth, it)
			}
			seen[str(it["ref"])] = true
		}
		if depth == 1 {
			itemBy(t, m, "imports", "canary_dir_e1d2/bb")
			itemBy(t, m, "imported_by", "canary_dir_e1d2/bb/b.go")
		} else {
			itemBy(t, m, "declares", "bb.B")
		}
	}
	m := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate", "max_entities": 2})
	om := m["omitted"].(map[string]any)
	if len(ctxItems(m)) != 2 || om["entities"].(float64) < 1 || !strings.Contains(strings.Join(anyStrings(om["reasons"]), ","), "entities") {
		t.Fatalf("entity limit: %v", m)
	}
	m = ctxExact(t, cs, map[string]any{"seed": "Rate", "max_bytes": 2048}, 2048)
	used := m["used"].(map[string]any)
	if used["bytes"].(float64) > 2048 {
		t.Fatalf("byte limit: %v", used)
	}
	if len(ctxItems(m)) == 0 || !strings.Contains(strings.Join(anyStrings(m["omitted"].(map[string]any)["reasons"]), ","), "bytes") {
		t.Fatalf("byte limit reasons: %v", m)
	}
	m = mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	if !strings.Contains(strings.Join(anyStrings(m["omitted"].(map[string]any)["reasons"]), ","), "depth") {
		t.Fatalf("depth omission not reported: %v", m["omitted"])
	}
	for _, args := range []map[string]any{
		{}, {"seed": "Rate", "ref": "hc1." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 32)}, {"ref": "hc1.x.y"},
		{"seed": "Rate", "depth": 3}, {"seed": "Rate", "max_entities": 101}, {"seed": "Rate", "max_bytes": 2047}, {"seed": "Rate", "max_ms": 99999},
		{"seed": "../etc/passwd; x"},
	} {
		if _, errText := call(t, cs, "hint_context", args); !strings.Contains(errText, "invalid_argument") {
			t.Errorf("%v: want invalid_argument, got %q", args, errText)
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// D05: an old ref reads its own snapshot, or is refused as stale; never other source.
func TestContextStaleReferences(t *testing.T) {
	shop, _ := project_(t)
	gh(t, shop, "collect", "--full")
	cs := session(t, mcpserver.Options{Dir: shop})
	m := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	rateRef := ctxItems(m)[0]["ref"]
	mainRef := itemBy(t, m, "imported_by", "cmd/shop/main.go")["ref"]
	ledger := filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger.go")
	b, _ := os.ReadFile(ledger)
	if err := os.WriteFile(ledger, []byte(strings.Replace(string(b), "return a", "return a + 1000", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// before a new collect: the structure of the snapshot, no source of the changed file
	m = mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	if def := ctxItems(m)[0]; m["freshness"] != "stale" || def["source_state"] != "stale" || def["source"] != nil {
		t.Fatalf("changed file: %v", m)
	}
	r := mustCall(t, cs, "hint_context", map[string]any{"ref": rateRef})
	if r["result"] != "refused_stale" || r["freshness"] != "stale" || len(ctxItems(r)) != 0 || r["artifact_id"] != nil {
		t.Fatalf("stale ref: %v", r)
	}
	ctxExact(t, cs, map[string]any{"ref": rateRef}, 16384)
	if ms := r["measurement"].(map[string]any); ms["result"] != "refused_stale" || ms["freshness"] != "stale" || ms["bytes"] != r["used"].(map[string]any)["bytes"] {
		t.Fatalf("stale measurement: %v", ms)
	}
	gh(t, shop, "collect")
	// the old ref of an unchanged file still reads its own snapshot
	o := mustCall(t, cs, "hint_context", map[string]any{"ref": mainRef})
	if o["result"] != "delivered" || o["snapshot"].(map[string]any)["latest"] != false || ctxItems(o)[0]["path"] != "cmd/shop/main.go" {
		t.Fatalf("old ref of an unchanged file: %v", o)
	}
	if r := mustCall(t, cs, "hint_context", map[string]any{"ref": rateRef}); r["result"] != "refused_stale" {
		t.Fatalf("old ref after collect: %v", r)
	}
	n := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	if def := ctxItems(n)[0]; n["freshness"] != "current" || !strings.Contains(str(def["source"]), "return a + 1000") {
		t.Fatalf("new snapshot: %v", n)
	}
	// a ref of an unknown snapshot is missing
	if x := mustCall(t, cs, "hint_context", map[string]any{"ref": "hc1." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 32)}); x["result"] != "missing" {
		t.Fatalf("unknown ref: %v", x)
	}
}

// D05: several matches give candidates only; no match is missing; without a snapshot the
// tool fails clearly and the ordinary navigation keeps working on the same terms.
func TestContextAmbiguousMissingAndNoIndex(t *testing.T) {
	shop, _ := project_(t)
	testkit.WriteFile(t, shop, "canary_dir_e1d2/other/rate.go", "package other\n\nfunc Rate() int { return 1 }\n")
	gh(t, shop, "collect", "--full")
	cs := session(t, mcpserver.Options{Dir: shop})
	m := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	if m["result"] != "ambiguous" || len(ctxItems(m)) != 2 {
		t.Fatalf("ambiguous: %v", m)
	}
	for _, it := range ctxItems(m) {
		if it["relation"] != "candidate" || it["source"] != nil {
			t.Fatalf("candidate: %v", it)
		}
	}
	if one := mustCall(t, cs, "hint_context", map[string]any{"ref": ctxItems(m)[1]["ref"]}); one["result"] != "delivered" {
		t.Fatalf("candidate ref: %v", one)
	}
	if x := mustCall(t, cs, "hint_context", map[string]any{"seed": "NoSuchName"}); x["result"] != "missing" || len(ctxItems(x)) != 0 {
		t.Fatalf("missing: %v", x)
	}
	shop2, _ := project_(t)
	cs2 := session(t, mcpserver.Options{Dir: shop2})
	if _, errText := call(t, cs2, "hint_context", map[string]any{"seed": "Rate"}); !strings.Contains(errText, "no_snapshot") {
		t.Fatalf("want no_snapshot: %q", errText)
	}
	gh(t, shop2, "collect")
	if d := mustCall(t, cs2, "hint_find_definition", map[string]any{"name": "Rate"}); len(d["definitions"].([]any)) != 1 {
		t.Fatalf("ordinary navigation: %v", d)
	}
}

// D05: removing the scan cache changes neither the answer nor its artifact ID; the
// snapshot that records the context stays the source of truth.
func TestContextSurvivesCacheCleanup(t *testing.T) {
	shop, _ := project_(t)
	gh(t, shop, "collect")
	s, _ := store.Open(shop)
	cs := session(t, mcpserver.Options{Dir: shop})
	m := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	if err := os.RemoveAll(s.Path("cache")); err != nil {
		t.Fatal(err)
	}
	again := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	x := mustCall(t, cs, "hint_context", map[string]any{"ref": ctxItems(m)[0]["ref"]})
	if again["artifact_id"] != m["artifact_id"] || x["result"] != "delivered" {
		t.Fatalf("after cache cleanup: %v %v", again["artifact_id"], x["result"])
	}
}

// D05/D02: every measurement of hint_context is a valid HINT v0.2 Observation.context; the
// example document carries it in place of its first context record.
func TestContextMeasurementsAreValidV02(t *testing.T) {
	shop, _ := project_(t)
	testkit.WriteFile(t, shop, "canary_dir_e1d2/other/rate.go", "package other\n\nfunc Rate() int { return 1 }\n")
	gh(t, shop, "collect", "--full")
	cs := session(t, mcpserver.Options{Dir: shop})
	amb := mustCall(t, cs, "hint_context", map[string]any{"seed": "Rate"})
	one := mustCall(t, cs, "hint_context", map[string]any{"ref": ctxItems(amb)[0]["ref"]})
	miss := mustCall(t, cs, "hint_context", map[string]any{"seed": "NoSuchName"})
	ledger := filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger.go")
	if err := os.WriteFile(ledger, []byte("package billing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := mustCall(t, cs, "hint_context", map[string]any{"ref": ctxItems(amb)[0]["ref"]})
	if stale["result"] != "refused_stale" {
		t.Fatalf("stale: %v", stale)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "v0.2", "activity-context.json"))
	if err != nil {
		t.Fatal(err)
	}
	sch, err := hint.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]map[string]any{"ambiguous": amb, "expand": one, "missing": miss, "stale": stale} {
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		for _, o := range doc["payload"].(map[string]any)["observations"].([]any) {
			if _, ok := o.(map[string]any)["context"]; ok {
				o.(map[string]any)["context"] = m["measurement"]
				break
			}
		}
		b, _ := json.Marshal(doc)
		d, _ := hint.Decode(b)
		if err := sch.Validate(d); err != nil {
			t.Errorf("%s: measurement %v is not valid v0.2: %v", name, m["measurement"], err)
		}
	}
}

// ctxExact calls hint_context and checks that used.bytes and measurement.bytes equal the
// whole answer as returned (structured result plus text summary) and stay within limit.
func ctxExact(t *testing.T, cs *mcp.ClientSession, args map[string]any, limit int) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "hint_context", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("hint_context %v: %v %v", args, err, res)
	}
	sc, _ := json.Marshal(res.StructuredContent)
	n := len(sc)
	for _, c := range res.Content {
		n += len(c.(*mcp.TextContent).Text)
	}
	var m map[string]any
	_ = json.Unmarshal(sc, &m)
	used := int(m["used"].(map[string]any)["bytes"].(float64))
	if used != n || int(m["measurement"].(map[string]any)["bytes"].(float64)) != n || n > limit {
		t.Fatalf("%v: used %d, measured %v, returned %d, limit %d", args, used, m["measurement"].(map[string]any)["bytes"], n, limit)
	}
	return m
}

// C11B-1: every branch keeps the whole answer within max_bytes and measures it exactly:
// many candidates, long names, escaped source, missing and stale answers.
func TestContextByteLimitOnEveryBranch(t *testing.T) {
	shop, _ := project_(t)
	var b strings.Builder
	b.WriteString("package many\n\ntype T struct{}\n\n")
	long := strings.Repeat("L", 150)
	for i := range 30 {
		fmt.Fprintf(&b, "func (T%02d) RepeatName() {}\n\ntype T%02d struct{}\n\nfunc (T%02d) %s%02d() {}\n\n", i, i, i, long, i)
	}
	b.WriteString("func Escaped() string {\n")
	for range 400 {
		b.WriteString("\treturn \"<&>\\\"\\\\\" + `\u2028`\n")
	}
	b.WriteString("}\n")
	testkit.WriteFile(t, shop, "canary_dir_e1d2/many/many.go", b.String())
	gh(t, shop, "collect", "--full")
	cs := session(t, mcpserver.Options{Dir: shop})
	for _, lim := range []int{2048, 4096} {
		m := ctxExact(t, cs, map[string]any{"seed": "RepeatName", "max_entities": 100, "max_bytes": lim}, lim)
		om := m["omitted"].(map[string]any)
		if m["result"] != "ambiguous" || len(ctxItems(m))+int(om["entities"].(float64)) != 30 || !strings.Contains(fmt.Sprint(om["reasons"]), "bytes") {
			t.Fatalf("ambiguous %d: %d items, omitted %v", lim, len(ctxItems(m)), om)
		}
		m = ctxExact(t, cs, map[string]any{"seed": "many.T03", "max_bytes": lim}, lim) // long sibling names
		if m["result"] != "delivered" || len(ctxItems(m)) == 0 {
			t.Fatalf("long names: %v", m)
		}
		m = ctxExact(t, cs, map[string]any{"seed": "Escaped", "max_bytes": lim}, lim)
		if def := ctxItems(m)[0]; def["source_state"] != "truncated" || !strings.HasPrefix(str(def["source"]), "func Escaped() string {") {
			t.Fatalf("escaped source: %v", def)
		}
	}
	ctxExact(t, cs, map[string]any{"seed": "NoSuchName", "max_bytes": 2048}, 2048)
	ctxExact(t, cs, map[string]any{"ref": "hc1." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 32), "max_bytes": 2048}, 2048)
}

// C11B-1: the time limit on the full tool path: a clock that jumps past the limit stops the
// walk after the start item and says so.
func TestContextTimeLimitOnTheToolPath(t *testing.T) {
	shop, _ := project_(t)
	gh(t, shop, "collect", "--full")
	var mu sync.Mutex
	clock := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	tick := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Second)
		return clock
	}
	cs := session(t, mcpserver.Options{Dir: shop, Now: tick})
	m := ctxExact(t, cs, map[string]any{"seed": "Rate", "max_ms": 100}, 16384)
	om := m["omitted"].(map[string]any)
	if len(ctxItems(m)) != 1 || !strings.Contains(fmt.Sprint(om["reasons"]), "time") || om["entities"].(float64) == 0 {
		t.Fatalf("time limit: %d items, %v", len(ctxItems(m)), om)
	}
	m = ctxExact(t, cs, map[string]any{"seed": "canary_dir_e1d2/billing", "max_ms": 100}, 16384)
	if len(ctxItems(m)) != 1 || !strings.Contains(fmt.Sprint(m["omitted"].(map[string]any)["reasons"]), "time") {
		t.Fatalf("time limit on a package: %v", m["omitted"])
	}
}
