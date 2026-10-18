package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &Env{Dir: dir, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb})
	return result{code, out.String(), errb.String()}
}

// runAt runs a command with the clock moved forward (for time windows).
func runAt(t *testing.T, dir string, d time.Duration, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &Env{Dir: dir, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Now: func() time.Time { return time.Now().Add(d) }})
	return result{code, out.String(), errb.String()}
}

func must(t *testing.T, r result) result {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	return r
}

// setup creates the fixture repositories and an initialized project with both of them.
func setup(t *testing.T) (shop, web string) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", filepath.Join(t.TempDir(), "keys"))
	t.Setenv("HINT_SERVER_URL", "")
	t.Setenv("HINT_SERVER_TOKEN", "")
	shop, web = testkit.Fixture(t)
	must(t, run(t, shop, "", "init", "--repo", web))
	must(t, run(t, shop, "", "collect", "--full"))
	return shop, web
}

// openCodeEvents are reduced plugin events (the form the plugin sends to the child). The
// plugin already drops output text; the command strings and paths are content that the
// child must turn into IDs and classes.
func openCodeEvents(root string) string {
	ev := func(hook, tool, call string, t int64, out map[string]any) string {
		b, _ := json.Marshal(map[string]any{"hook": hook, "t_wall_ms": t,
			"input": map[string]any{"tool": tool, "sessionID": "ses_HINTCANARY_S1", "callID": call}, "output": out})
		return string(b) + "\n"
	}
	ledger := filepath.ToSlash(filepath.Join(root, "canary_dir_e1d2/billing/ledger.go"))
	t0 := time.Now().UnixMilli() // after the last snapshot; the next collect runs later (runAt)
	var b strings.Builder
	b.WriteString(ev("tool.execute.before", "read", "call_HINTCANARY_1", t0+0, map[string]any{"args": map[string]any{"filePath": ledger}}))
	b.WriteString(ev("tool.execute.after", "read", "call_HINTCANARY_1", t0+50, map[string]any{"metadata": map[string]any{}, "status": "completed", "output": "[redacted:len=412]"}))
	b.WriteString(ev("tool.execute.before", "bash", "call_HINTCANARY_2", t0+1000, map[string]any{"args": map[string]any{"command": "cat canary_dir_e1d2/billing/ledger.go | grep HINTCANARY_PATTERN", "workdir": filepath.ToSlash(root)}}))
	b.WriteString(ev("tool.execute.after", "bash", "call_HINTCANARY_2", t0+1200, map[string]any{"metadata": map[string]any{"exit": 0}, "status": "completed", "output": "[redacted:len=31]"}))
	b.WriteString(ev("tool.execute.before", "edit", "call_HINTCANARY_3", t0+2000, map[string]any{"args": map[string]any{"filePath": ledger}}))
	b.WriteString(ev("tool.execute.after", "edit", "call_HINTCANARY_3", t0+2100, map[string]any{"metadata": map[string]any{"files": []any{map[string]any{"filePath": ledger}}}, "status": "completed", "output": "[redacted:len=0]"}))
	b.WriteString(ev("tool.execute.before", "bash", "call_HINTCANARY_4", t0+3000, map[string]any{"args": map[string]any{"command": "hintctl sync --all HINTCANARY_ARG"}}))
	b.WriteString(ev("tool.execute.after", "bash", "call_HINTCANARY_4", t0+3500, map[string]any{"metadata": map[string]any{"exit": 2}, "status": "completed", "output": "[redacted:len=9]"}))
	b.WriteString(ev("tool.execute.before", "subagent", "call_HINTCANARY_5", t0+4000, map[string]any{"args": map[string]any{}}))
	b.WriteString(ev("tool.execute.before", "webfetch", "call_HINTCANARY_6", t0+5000, map[string]any{"args": map[string]any{}}))
	b.WriteString(ev("tool.execute.after", "webfetch", "call_HINTCANARY_6", t0+5100, map[string]any{"metadata": map[string]any{}, "status": "error"}))
	b.WriteString("not json HINTCANARY_GARBAGE\n")
	return b.String()
}

func readAll(t *testing.T, dir string) []byte {
	t.Helper()
	var all []byte
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

func noCanary(t *testing.T, what string, b []byte, extra ...string) {
	t.Helper()
	for _, c := range append(append([]string{}, testkit.Canaries...), extra...) {
		if bytes.Contains(b, []byte(c)) {
			t.Errorf("%s contains %q", what, c)
		}
	}
}

func TestInitExcludesStoreAndKeepsKeyOutside(t *testing.T) {
	shop, _ := setup(t)
	if out := testkit.Git(t, shop, "check-ignore", ".hint/state/project.json"); !strings.Contains(out, ".hint") {
		t.Fatalf(".hint not ignored: %q", out)
	}
	if st := testkit.Git(t, shop, "status", "--porcelain"); st != "" {
		t.Fatalf("init changed the work tree: %q", st)
	}
	s, err := store.Open(shop)
	if err != nil {
		t.Fatal(err)
	}
	kf, _ := s.KeyFile()
	if strings.HasPrefix(kf, shop) {
		t.Fatalf("key inside the work tree: %s", kf)
	}
	if fi, err := os.Stat(kf); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("key file: %v %v", fi, err)
	}
	fi, err := os.Stat(s.Path("state", "project.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file permissions: %v %v", fi.Mode(), err)
	}
	// a second init keeps the key and the namespace
	ns := s.Cfg.ProjectNamespace
	must(t, run(t, shop, "", "init"))
	s2, _ := store.Open(shop)
	if s2.Cfg.ProjectNamespace != ns || len(s2.Cfg.Repos) != 2 {
		t.Fatalf("re-init changed the project: %+v", s2.Cfg)
	}
}

func TestWriteFailsClosedWhenNotExcluded(t *testing.T) {
	shop, _ := setup(t)
	if err := os.WriteFile(filepath.Join(shop, ".git", "info", "exclude"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, shop, "", "collect")
	if r.code == 0 || !strings.Contains(r.stderr, "not ignored by Git") {
		t.Fatalf("collect must fail closed: %+v", r)
	}
	r = run(t, shop, "", "export")
	if r.code == 0 {
		t.Fatalf("export must fail closed: %+v", r)
	}
}

func TestExportHasNoContentOrNames(t *testing.T) {
	shop, _ := setup(t)
	r := run(t, shop, openCodeEvents(shop), "adapter", "opencode", "--project", shop)
	must(t, r)
	if !strings.Contains(r.stderr, "written=6 skipped=1") {
		t.Fatalf("adapter: %s", r.stderr)
	}
	// an edit after the adapter events: one FILE_MUTATION
	testkit.WriteFile(t, shop, "canary_dir_e1d2/billing/ledger.go", "package billing\n\nfunc Rate(a, b int) int { return a + b }\n")
	must(t, runAt(t, shop, time.Minute, "collect"))
	out := t.TempDir()
	must(t, run(t, shop, "", "export", "--out", out))
	all := readAll(t, out)
	if len(all) == 0 {
		t.Fatal("no export files")
	}
	noCanary(t, "export", all, shop, `"ses_`, `"call_`, "hintctl", "ledger.go", "billing", "shop-web", "main.go")
	for _, typ := range []string{"export_policy", "repository_snapshot", "change_set", "activity_stream"} {
		if m, _ := filepath.Glob(filepath.Join(out, typ+"-*.hint.json")); len(m) != 1 {
			t.Errorf("want one %s document, got %v", typ, m)
		}
	}
	// the adapter output: classes and unknowns
	var doc struct {
		Payload struct {
			Observations []map[string]any `json:"observations"`
		} `json:"payload"`
	}
	m, _ := filepath.Glob(filepath.Join(out, "activity_stream-*.hint.json"))
	b, _ := os.ReadFile(m[0])
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[string]int{}
	var unknownShell, attributed bool
	for _, o := range doc.Payload.Observations {
		ops[o["op"].(string)]++
		if o["command_class"] == "UNKNOWN" && o["op"] == "EXECUTE" {
			unknownShell = true
		}
		if o["op"] == "FILE_MUTATION" && o["mutation_attribution"] == "inferred" {
			attributed = true
		}
	}
	if ops["READ"] != 1 || ops["EXECUTE"] != 2 || ops["EDIT_INTENT"] != 1 || ops["DELEGATE"] != 1 || ops["TOOL"] != 1 || ops["FILE_MUTATION"] != 1 {
		t.Errorf("ops: %v", ops)
	}
	if !unknownShell {
		t.Error("a shell command without a rule must stay EXECUTE/UNKNOWN")
	}
	if !attributed {
		t.Error("the mutation after exactly one edit of the file must be inferred")
	}
	// a second export sends no observation twice
	out2 := t.TempDir()
	must(t, run(t, shop, "", "export", "--out", out2))
	if m, _ := filepath.Glob(filepath.Join(out2, "activity_stream-*.hint.json")); len(m) != 0 {
		t.Errorf("observations exported twice: %v", m)
	}
}

func TestWatcherOnlyMutationStaysUnassigned(t *testing.T) {
	shop, _ := setup(t)
	testkit.WriteFile(t, shop, "web/app.ts", "export const changed = 1\n")
	must(t, run(t, shop, "", "collect"))
	s, _ := store.Open(shop)
	obs, _, err := project.ReadSpool(s)
	if err != nil || len(obs) != 1 {
		t.Fatalf("spool: %v %v", obs, err)
	}
	if obs[0].Op != "FILE_MUTATION" || obs[0].MutationAttribution != "none" || len(obs[0].Correlation) != 0 {
		t.Fatalf("watcher-only mutation must stay unassigned: %+v", obs[0])
	}
}

func TestCacheCleanKeepsMappingKeysAndState(t *testing.T) {
	shop, _ := setup(t)
	s, _ := store.Open(shop)
	if _, err := os.Stat(s.Path("cache")); err != nil {
		t.Fatal("no cache after collect")
	}
	before := listFiles(t, s.Path("state"))
	must(t, run(t, shop, "", "cache", "clean"))
	if _, err := os.Stat(s.Path("cache")); !os.IsNotExist(err) {
		t.Fatal("cache still present")
	}
	if after := listFiles(t, s.Path("state")); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("state changed: %v -> %v", before, after)
	}
	kf, _ := s.KeyFile()
	if _, err := os.Stat(kf); err != nil {
		t.Fatal("key removed")
	}
	// the next collect rebuilds the cache and keeps the snapshot
	r := must(t, run(t, shop, "", "collect"))
	if !strings.Contains(r.stdout, "unchanged") {
		t.Fatalf("snapshot changed after cache clean: %s", r.stdout)
	}
}

func listFiles(t *testing.T, dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			out = append(out, strings.TrimPrefix(p, dir))
		}
		return nil
	})
	return out
}

func TestPolicyAllowlistKeepsRecordTypesOut(t *testing.T) {
	shop, _ := setup(t)
	must(t, run(t, shop, openCodeEvents(shop), "adapter", "opencode", "--project", shop))
	s, _ := store.Open(shop)
	s.Cfg.Policy.RecordTypes = []string{"Snapshot", "Entity", "Relation"}
	s.Cfg.Policy.Fields = map[string][]string{"Entity": {"entity_id", "kind", "parent_id", "repo_id", "snapshot_id", "id_stability", "resolution"}}
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	must(t, run(t, shop, "", "export", "--out", out))
	all := readAll(t, out)
	for _, bad := range []string{`"observations"`, `"activity_stream"`, `"change_set"`, `"features"`, `"flags"`} {
		if bytes.Contains(all, []byte(bad)) {
			t.Errorf("export contains %s outside the allowlist", bad)
		}
	}
	srv := testkit.NewServer()
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	for _, body := range srv.Posted() {
		for _, bad := range []string{`"observations"`, `"features"`} {
			if bytes.Contains(body, []byte(bad)) {
				t.Errorf("API request contains %s outside the allowlist", bad)
			}
		}
	}
	// a policy that removes a required field fails closed
	s.Cfg.Policy.Fields = map[string][]string{"Entity": {"entity_id"}}
	_ = s.SaveConfig()
	if r := run(t, shop, "", "export", "--dry-run"); r.code == 0 || !strings.Contains(r.stderr, "required field") {
		t.Fatalf("missing required field must fail: %+v", r)
	}
}

func TestFileAndAPIImportGiveSameResultAndDocument(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	api := must(t, run(t, shop, "", "plan", "--json"))
	s, _ := store.Open(shop)
	saved, err := project.LatestRecommendation(s)
	if err != nil {
		t.Fatal(err)
	}
	// file mode: the same bytes from a file outside the store
	f := filepath.Join(t.TempDir(), filepath.Base(saved))
	b, _ := os.ReadFile(saved)
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	file := must(t, run(t, shop, "", "plan", "--json", "--from", f))
	var ra, rf struct {
		Result   any    `json:"result"`
		Markdown string `json:"markdown"`
	}
	_ = json.Unmarshal([]byte(api.stdout), &ra)
	_ = json.Unmarshal([]byte(file.stdout), &rf)
	ja, _ := json.Marshal(ra.Result)
	jf, _ := json.Marshal(rf.Result)
	if string(ja) != string(jf) || !strings.Contains(string(ja), `"accept"`) {
		t.Fatalf("results differ or not accepted:\napi  %s\nfile %s", ja, jf)
	}
	ma, _ := os.ReadFile(ra.Markdown)
	mf, _ := os.ReadFile(rf.Markdown)
	if !bytes.Equal(ma, mf) {
		t.Fatalf("Markdown differs:\n%s\n---\n%s", ma, mf)
	}
	md := string(ma)
	for _, want := range []string{"shop: ", "EXTRACT", "predictions, not measured effects", "uncalibrated"} {
		if !strings.Contains(md, want) {
			t.Errorf("plan lacks %q:\n%s", want, md)
		}
	}
	// the plan reveals names only locally: the server never got them
	for _, body := range srv.Posted() {
		noCanary(t, "API request", body, "ledger.go")
	}
}

func TestRejectedRecommendationsInBothModes(t *testing.T) {
	shop, _ := setup(t)
	cases := []struct {
		name   string
		mutate func(doc map[string]any)
		docRes string // expected document result
		plan   string // expected first plan code (when the document is accepted)
	}{
		{"path in targets", func(d map[string]any) {
			p := d["payload"].(map[string]any)["plans"].([]any)[0].(map[string]any)
			p["targets"] = []any{"canary_dir_e1d2/billing/ledger.go"}
		}, "schema_invalid", ""},
		{"unknown identifier", func(d map[string]any) {
			p := d["payload"].(map[string]any)["plans"].([]any)[0].(map[string]any)
			p["targets"] = append(p["targets"].([]any), strings.Repeat("ab", 16))
		}, "accepted", "unresolved_id"},
		{"wrong namespace", func(d map[string]any) {
			d["project_namespace"] = strings.Repeat("cd", 16)
			for _, p := range d["payload"].(map[string]any)["plans"].([]any) {
				p.(map[string]any)["project_namespace"] = strings.Repeat("cd", 16)
			}
		}, "namespace_mismatch", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := testkit.NewServer()
			defer srv.Close()
			must(t, run(t, shop, "", "recommend", "--yes", "--all", "--server", srv.URL))
			srv.Mutate = c.mutate
			// API mode
			must(t, run(t, shop, "", "recommend", "--yes", "--all", "--server", srv.URL))
			api := run(t, shop, "", "plan", "--json")
			// file mode
			s, _ := store.Open(shop)
			saved, _ := project.LatestRecommendation(s)
			f := filepath.Join(t.TempDir(), "rec.hint.json")
			b, _ := os.ReadFile(saved)
			_ = os.WriteFile(f, b, 0o600)
			file := run(t, shop, "", "plan", "--json", "--from", f)
			for mode, r := range map[string]result{"api": api, "file": file} {
				var out struct {
					Result struct {
						Document string `json:"document"`
						Plans    []struct {
							Status string   `json:"status"`
							Codes  []string `json:"codes"`
						} `json:"plans"`
					} `json:"result"`
				}
				if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
					t.Fatalf("%s: %v %+v", mode, err, r)
				}
				if out.Result.Document != c.docRes {
					t.Errorf("%s: document %s, want %s", mode, out.Result.Document, c.docRes)
				}
				if c.plan != "" && (len(out.Result.Plans) == 0 || out.Result.Plans[0].Status != "reject" ||
					!strings.Contains(strings.Join(out.Result.Plans[0].Codes, ","), c.plan)) {
					t.Errorf("%s: plans %+v, want reject %s", mode, out.Result.Plans, c.plan)
				}
			}
		})
	}
}

func TestOutdatedPlanIsRejected(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	if r := must(t, run(t, shop, "", "plan", "--json")); !strings.Contains(r.stdout, `"accept"`) {
		t.Fatalf("fresh plan not accepted: %s", r.stdout)
	}
	// the work tree changes after the recommendation: the plan is outdated, also before
	// a new collect
	testkit.WriteFile(t, shop, "canary_dir_e1d2/billing/new.go", "package billing\n\nfunc New() {}\n")
	r := must(t, run(t, shop, "", "plan", "--json"))
	if !strings.Contains(r.stdout, "snapshot_mismatch") {
		t.Fatalf("outdated plan not rejected: %s", r.stdout)
	}
	must(t, run(t, shop, "", "collect"))
	if r := must(t, run(t, shop, "", "plan", "--json")); !strings.Contains(r.stdout, "snapshot_mismatch") {
		t.Fatalf("outdated plan not rejected after collect: %s", r.stdout)
	}
	// file mode: the same document from a file gives the same rejection
	s, _ := store.Open(shop)
	saved, _ := project.LatestRecommendation(s)
	f := filepath.Join(t.TempDir(), "old.hint.json")
	b, _ := os.ReadFile(saved)
	_ = os.WriteFile(f, b, 0o600)
	if r := must(t, run(t, shop, "", "plan", "--json", "--from", f)); !strings.Contains(r.stdout, "snapshot_mismatch") {
		t.Fatalf("outdated plan not rejected in file mode: %s", r.stdout)
	}
}

func TestNetworkFailureKeepsRetryablePackage(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	srv.SetFail(true)
	r := run(t, shop, "", "recommend", "--yes", "--server", srv.URL)
	if r.code == 0 || !strings.Contains(r.stderr, "--retry") {
		t.Fatalf("outage must queue the package: %+v", r)
	}
	s, _ := store.Open(shop)
	q, _ := project.Queued(s)
	if len(q) != 1 {
		t.Fatalf("queued: %v", q)
	}
	// a server that is down: still queued, nothing lost
	srv2 := run(t, shop, "", "recommend", "--yes", "--retry", "--server", "http://127.0.0.1:1")
	if srv2.code == 0 {
		t.Fatalf("retry to a closed port must fail: %+v", srv2)
	}
	if q, _ := project.Queued(s); len(q) != 1 {
		t.Fatalf("queue after failed retry: %v", q)
	}
	srv.SetFail(false)
	must(t, run(t, shop, "", "recommend", "--yes", "--retry", "--server", srv.URL))
	if q, _ := project.Queued(s); len(q) != 0 {
		t.Fatalf("queue not empty after retry: %v", q)
	}
	if len(srv.Docs) == 0 {
		t.Fatal("server got no documents")
	}
}

func TestRecommendNeedsApproval(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	r := run(t, shop, "", "recommend", "--server", srv.URL)
	if r.code == 0 || !strings.Contains(r.stderr, "needs approval") || len(srv.Posted()) != 0 {
		t.Fatalf("send without approval: %+v, posted %d", r, len(srv.Posted()))
	}
	var out, errb bytes.Buffer
	code := Run([]string{"recommend", "--server", srv.URL}, &Env{Dir: shop, Stdin: strings.NewReader("n\n"),
		Stdout: &out, Stderr: &errb, IsTerminal: func() bool { return true }})
	if code == 0 || len(srv.Posted()) != 0 {
		t.Fatalf("declined prompt sent data: %s", out.String())
	}
	if r := run(t, shop, "", "recommend", "--yes", "--server", "http://example.invalid"); r.code == 0 || !strings.Contains(r.stderr, "https") {
		t.Fatalf("plain http to a remote host must be refused: %+v", r)
	}
}

func TestFullAndIncrementalScanAreEquivalent(t *testing.T) {
	shop, _ := setup(t)
	testkit.WriteFile(t, shop, "web/extra.ts", "export const y = 2\n")
	testkit.WriteFile(t, shop, "canary_dir_e1d2/billing/ledger.go", "package billing\n\nfunc Rate(a, b int) int { return b }\n\nfunc Fee() int { return 1 }\n")
	inc := must(t, run(t, shop, "", "collect", "--json"))
	s, _ := store.Open(shop)
	incSnap, _ := project.LoadLatest(s, s.RepoID(s.Cfg.Repos[0]))
	if !strings.Contains(inc.stdout, `"reused"`) {
		t.Fatal(inc.stdout)
	}
	var rows []struct {
		Parsed, Reused int
	}
	_ = json.Unmarshal([]byte(inc.stdout), &rows)
	if rows[0].Reused == 0 || rows[0].Parsed == 0 {
		t.Fatalf("incremental scan did not reuse the cache: %+v", rows)
	}
	// full rescan of the same tree: same snapshot (state key, ID and digest)
	full := must(t, run(t, shop, "", "collect", "--full", "--json"))
	if !strings.Contains(full.stdout, `"changed": false`) {
		t.Fatalf("full scan differs from the incremental one: %s", full.stdout)
	}
	fullSnap, _ := project.LoadLatest(s, s.RepoID(s.Cfg.Repos[0]))
	if fullSnap.Record.Digest != incSnap.Record.Digest || fullSnap.Record.SnapshotID != incSnap.Record.SnapshotID {
		t.Fatal("full and incremental snapshots differ")
	}
	// two unchanged scans keep the snapshot and its IDs
	must(t, run(t, shop, "", "collect"))
	again, _ := project.LoadLatest(s, s.RepoID(s.Cfg.Repos[0]))
	if again.Record.SnapshotID != fullSnap.Record.SnapshotID || again.Record.CapturedAt != fullSnap.Record.CapturedAt {
		t.Fatal("an unchanged scan made a new snapshot")
	}
}

func TestDoctorWithoutOptionalTools(t *testing.T) {
	shop, _ := setup(t)
	gitDir := filepath.Dir(mustLook(t, "git"))
	t.Setenv("PATH", gitDir) // no scip-*, no sem, no opencode
	r := run(t, shop, "", "doctor")
	if r.code != 0 {
		t.Fatalf("doctor failed without optional tools:\n%s%s", r.stdout, r.stderr)
	}
	for _, want := range []string{"optional scip-go: not found", "adapter OpenCode: available", "adapter Codex: not available"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("doctor lacks %q:\n%s", want, r.stdout)
		}
	}
	must(t, run(t, shop, "", "collect", "--full"))
}

func mustLook(t *testing.T, name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skip(name + " not found")
	}
	return p
}

func TestGitSubcommandAndBinaryAreTheSameCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	shop, _ := setup(t)
	bin := t.TempDir()
	exe := "git-hint"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, exe), "../../cmd/git-hint")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	path := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	outOf := func(name string, args ...string) string {
		c := exec.Command(name, args...)
		c.Dir = shop
		c.Env = append(os.Environ(), "PATH="+path)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, b)
		}
		return string(b)
	}
	for _, args := range [][]string{{"version"}, {"status"}} {
		a := outOf("git", append([]string{"hint"}, args...)...)
		b := outOf(filepath.Join(bin, exe), args...)
		if a != b {
			t.Errorf("git hint %v and git-hint %v differ:\n%s\n---\n%s", args, args, a, b)
		}
	}
}

func TestPlanMarksUnsupportedAndRefusals(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	srv.Mutate = func(d map[string]any) {
		p := d["payload"].(map[string]any)["plans"].([]any)[0].(map[string]any)
		p["steps"].([]any)[0].(map[string]any)["support"] = "unsupported"
	}
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	r := must(t, run(t, shop, "", "plan", "--json"))
	if !strings.Contains(r.stdout, `"partial"`) || !strings.Contains(r.stdout, `"render_only"`) {
		t.Fatalf("want partial and render_only: %s", r.stdout)
	}
}

// --- regression tests of review findings -----------------------------------------

func TestSameSizeAndMtimeChangeIsDetected(t *testing.T) {
	shop, _ := setup(t)
	f := filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger.go")
	// make the work tree dirty first, so that only the content can reveal the next edit
	// (the work tree state stays "dirty")
	old, _ := os.ReadFile(f)
	if err := os.WriteFile(f, append(old, []byte("\n// edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, run(t, shop, "", "collect"))
	srv := testkit.NewServer()
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	fi, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	nb := bytes.Replace(b, []byte("return a\n"), []byte("return b\n"), 1) // same length
	if bytes.Equal(b, nb) || len(b) != len(nb) {
		t.Fatal("fixture edit")
	}
	if err := os.WriteFile(f, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(f); fi2.Size() != fi.Size() || !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatal("size or mtime changed")
	}
	// the plan check sees the change before a collect
	if r := must(t, run(t, shop, "", "plan", "--json")); !strings.Contains(r.stdout, "snapshot_mismatch") {
		t.Fatalf("plan for the old content accepted: %s", r.stdout)
	}
	r := must(t, run(t, shop, "", "collect", "--json"))
	if !strings.Contains(r.stdout, `"changed": true`) {
		t.Fatalf("collect missed a same-size, same-mtime change: %s", r.stdout)
	}
	full := must(t, run(t, shop, "", "collect", "--full", "--json"))
	if !strings.Contains(full.stdout, `"changed": false`) {
		t.Fatalf("incremental and full snapshots differ: %s", full.stdout)
	}
}

func TestKeyInsideAProjectRepositoryIsRejected(t *testing.T) {
	cases := []struct {
		name string
		key  func(shop, web string) string // HINT_KEY_DIR
		args []string
	}{
		{"absolute inside .hint", func(shop, _ string) string { return filepath.Join(shop, ".hint", "keys") }, nil},
		{"repository root", func(shop, _ string) string { return shop }, nil},
		{"relative inside", func(shop, _ string) string { t.Chdir(shop); return "keys" }, nil},
		{"symlink into the repository", func(shop, _ string) string {
			link := filepath.Join(t.TempDir(), "keys-link")
			if err := os.MkdirAll(filepath.Join(shop, "secret-keys"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(shop, "secret-keys"), link); err != nil {
				t.Fatal(err)
			}
			return link
		}, nil},
		{"second repository", func(_, web string) string { return filepath.Join(web, "keys") }, []string{"--repo", "WEB"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HINT_SERVER_URL", "")
			shop, web := testkit.Fixture(t)
			t.Setenv("HINT_KEY_DIR", c.key(shop, web))
			args := []string{"init"}
			for _, a := range c.args {
				args = append(args, strings.ReplaceAll(a, "WEB", web))
			}
			r := run(t, shop, "", args...)
			if r.code == 0 || !strings.Contains(r.stderr, "must not be inside a repository") {
				t.Fatalf("key inside a repository accepted: %+v", r)
			}
			for _, root := range []string{shop, web} {
				var keys []string
				_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
					if err == nil && strings.HasSuffix(p, ".key") {
						keys = append(keys, p)
					}
					return nil
				})
				if len(keys) > 0 {
					t.Fatalf("key file written inside a repository: %v", keys)
				}
			}
		})
	}
	// a repository added later must not contain the key directory either
	t.Run("added later", func(t *testing.T) {
		t.Setenv("HINT_SERVER_URL", "")
		shop, web := testkit.Fixture(t)
		t.Setenv("HINT_KEY_DIR", filepath.Join(web, "keys"))
		must(t, run(t, shop, "", "init"))
		if r := run(t, shop, "", "init", "--repo", web); r.code == 0 || !strings.Contains(r.stderr, "must not be inside") {
			t.Fatalf("adding a repository that holds the key accepted: %+v", r)
		}
	})
}

func snapshotOfStore(t *testing.T, dir string) map[string]int64 {
	m := map[string]int64{}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			m[p] = fi.ModTime().UnixNano() ^ fi.Size()
		}
		return nil
	})
	return m
}

func TestPartialIgnoreAndTrackedFilesFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		exclude string
		track   string // file below .hint/ to add to the index
	}{
		{"only project.json ignored", "/.hint/state/project.json\n", ""},
		{"negated area", "/.hint/*\n!/.hint/state\n", ""},
		{"only state ignored", "/.hint/state/\n", ""},
		{"tracked file", "/.hint/\n", ".hint/state/project.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shop, _ := setup(t)
			s, _ := store.Open(shop)
			if err := os.WriteFile(filepath.Join(shop, ".git", "info", "exclude"), []byte(c.exclude), 0o644); err != nil {
				t.Fatal(err)
			}
			if c.track != "" {
				testkit.Git(t, shop, "add", "-f", c.track)
			}
			testkit.WriteFile(t, shop, "web/new.ts", "export const n = 1\n")
			before := snapshotOfStore(t, s.Path())
			for _, args := range [][]string{{"collect"}, {"adapter", "opencode", "--project", shop}, {"export"}, {"cache", "clean"}} {
				r := run(t, shop, openCodeEvents(shop), args...)
				if args[0] == "cache" {
					continue // cache clean deletes only; it never writes
				}
				if r.code == 0 || !strings.Contains(r.stderr, "not ignored by Git") {
					t.Errorf("%v must fail closed: %+v", args, r)
				}
			}
			after := snapshotOfStore(t, s.Path())
			for p, v := range after {
				if w, ok := before[p]; !ok || w != v {
					t.Errorf("written although not excluded: %s", p)
				}
			}
		})
	}
}

func TestWrongReceiptDoesNotMarkExport(t *testing.T) {
	cases := map[string]func(d map[string]any){
		"wrong accepted digest": func(d map[string]any) {
			acc := d["payload"].(map[string]any)["accepted"].([]any)
			if len(acc) > 0 {
				acc[0].(map[string]any)["content_digest"] = "sha256:" + strings.Repeat("0", 64)
			}
			testkit.Reseal(d)
		},
		"wrong document ID": func(d map[string]any) {
			acc := d["payload"].(map[string]any)["accepted"].([]any)
			if len(acc) > 0 {
				acc[0].(map[string]any)["document_id"] = strings.Repeat("ab", 16)
			}
			testkit.Reseal(d)
		},
		"receipt digest wrong": func(d map[string]any) {
			d["content_digest"] = "sha256:" + strings.Repeat("1", 64)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			shop, _ := setup(t)
			must(t, run(t, shop, openCodeEvents(shop), "adapter", "opencode", "--project", shop))
			srv := testkit.NewServer()
			defer srv.Close()
			srv.Receipt = mutate
			r := run(t, shop, "", "recommend", "--yes", "--server", srv.URL)
			if r.code == 0 || !strings.Contains(r.stderr, "receipt") {
				t.Fatalf("wrong receipt accepted: %+v", r)
			}
			s, _ := store.Open(shop)
			if _, err := os.Stat(s.Path("state", "exports", "history.jsonl")); !os.IsNotExist(err) {
				t.Fatal("export marked as done")
			}
			// the observations are still pending: a dry run still includes them
			if d := must(t, run(t, shop, "", "export", "--dry-run")); !strings.Contains(d.stdout, "activity_stream") {
				t.Fatalf("observations marked as exported: %s", d.stdout)
			}
		})
	}
}

// TestExclusionChangedDuringAdapterSession: the adapter writes the first call, then the
// Git rules change (exclusion removed, or a .hint/ file enters the index), then a second
// call arrives. The second record must not reach the spool; the child reports the stop
// on stdout, counts the discarded record, keeps reading stdin and exits with 4.
func TestExclusionChangedDuringAdapterSession(t *testing.T) {
	cases := map[string]func(t *testing.T, shop string){
		"rule removed": func(t *testing.T, shop string) {
			if err := os.WriteFile(filepath.Join(shop, ".git", "info", "exclude"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"file added to the index": func(t *testing.T, shop string) {
			testkit.Git(t, shop, "add", "-f", ".hint/state/project.json")
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			shop, _ := setup(t)
			s, _ := store.Open(shop)
			spoolPath := s.Path("state", "activity", "spool.jsonl")
			statsPath := s.Path("state", "activity", "adapter-stats.json")
			pr, pw := io.Pipe()
			var out, errb syncBuffer
			done := make(chan int, 1)
			go func() {
				done <- Run([]string{"adapter", "opencode", "--project", shop},
					&Env{Dir: shop, Stdin: pr, Stdout: &out, Stderr: &errb})
			}()
			call := func(id string) {
				for _, hook := range []string{"tool.execute.before", "tool.execute.after"} {
					b, _ := json.Marshal(map[string]any{"hook": hook, "t_wall_ms": time.Now().UnixMilli(),
						"input":  map[string]any{"tool": "read", "sessionID": "s", "callID": id},
						"output": map[string]any{"args": map[string]any{"filePath": filepath.Join(shop, "web/app.ts")}, "metadata": map[string]any{}, "output": "x"}})
					if _, err := pw.Write(append(b, '\n')); err != nil {
						t.Fatal(err)
					}
				}
				stats, _ := json.Marshal(map[string]any{"hook": "hint.stats", "stats": map[string]any{"sent": 2, "dropped": 0}})
				pw.Write(append(stats, '\n'))
			}
			lines := func() int {
				b, _ := os.ReadFile(spoolPath)
				return bytes.Count(b, []byte("\n"))
			}
			waitFor := func(cond func() bool) {
				for i := 0; i < 200 && !cond(); i++ {
					time.Sleep(10 * time.Millisecond)
				}
			}
			call("c1")
			waitFor(func() bool { _, err := os.Stat(statsPath); return lines() == 1 && err == nil })
			if lines() != 1 {
				t.Fatalf("first call not written: %d", lines())
			}
			statsBefore, _ := os.ReadFile(statsPath)
			change(t, shop)
			call("c2")
			call("c3")
			waitFor(func() bool { return strings.Contains(out.String(), `"stopped"`) })
			pw.Close()
			code := <-done
			if code != 4 {
				t.Fatalf("exit %d, want 4\n%s", code, errb.String())
			}
			if lines() != 1 {
				t.Fatalf("spool grew after the exclusion broke: %d lines", lines())
			}
			if statsAfter, _ := os.ReadFile(statsPath); !bytes.Equal(statsBefore, statsAfter) {
				t.Fatal("adapter-stats.json written after the exclusion broke")
			}
			if !strings.Contains(out.String(), `{"hint":"stopped","reason":"not_excluded"}`) ||
				!strings.Contains(errb.String(), "written=1") || !strings.Contains(errb.String(), "discarded=2") {
				t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
			}
		})
	}
}

// syncBuffer is a bytes.Buffer that a running command and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// TestMCPStdoutCarriesOnlyProtocolMessages runs the built binary as an MCP server over
// stdio, drives it with raw JSON-RPC, and requires that every stdout line is a JSON-RPC
// 2.0 message. Diagnostics may go to stderr only.
func TestMCPStdoutCarriesOnlyProtocolMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	shop, _ := setup(t)
	bin := filepath.Join(t.TempDir(), "git-hint")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/git-hint").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Dir = shop
	cmd.Env = os.Environ()
	stdin, _ := cmd.StdinPipe()
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		stdin.Write(append(b, '\n'))
	}
	waitID := func(id int) {
		for i := 0; i < 500; i++ {
			if strings.Contains(stdout.String(), fmt.Sprintf(`"id":%d`, id)) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no response %d\nstdout:%s\nstderr:%s", id, stdout.String(), stderr.String())
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw", "version": "0"}}})
	waitID(1)
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	waitID(2)
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "hint_collect", "arguments": map[string]any{"full": true}}})
	waitID(3)
	send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "hint_export_preview", "arguments": map[string]any{}}})
	waitID(4)
	send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "hint_recommend", "arguments": map[string]any{}}})
	waitID(5)
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("server did not exit after stdin closed")
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 5 {
		t.Fatalf("too few messages: %d", len(lines))
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("non-protocol stdout line: %q", l)
		}
	}
	out := stdout.String()
	if !strings.Contains(out, `"protocolVersion":"2025-11-25"`) || !strings.Contains(out, "approval_required") {
		t.Fatalf("unexpected answers: %s", out)
	}
	if strings.Contains(out, "HINTCANARY_KEY") || strings.Contains(out, "HINTCANARY_LITERAL") {
		t.Fatal("source content in MCP output")
	}
}

// D06: a hint_context call with a measurement makes the export package v0.2: the call gives two
// stages (one tool call ID), a damaged measurement leaves "context" missing, and every
// document passes the v0.2 schema, the semantic rules and the policy.
func TestExportWithContextMeasurementIsV02(t *testing.T) {
	shop, _ := setup(t)
	must(t, run(t, shop, "", "collect"))
	ev := func(hook, call string, t0 int64, out map[string]any) string {
		b, _ := json.Marshal(map[string]any{"hook": hook, "t_wall_ms": t0,
			"input": map[string]any{"tool": "githint_hint_context", "sessionID": "ses_HINTCANARY_C", "callID": call}, "output": out})
		return string(b) + "\n"
	}
	id := func(c string) string { return strings.Repeat(c, 32) }
	part := map[string]any{"targets": []any{}, "context": map[string]any{"action": "deliver", "stage": "raw_tool_output", "artifact_id": id("a"),
		"snapshot_id": id("b"), "policy_state": "not_applicable", "freshness": "current", "result": "delivered", "bytes": 9000, "omitted": 2,
		"tokens": map[string]any{"value": 2250, "estimator": "bytes-div-4", "estimator_version": "1", "label": "estimate"}}}
	t0 := time.Now().UnixMilli()
	stream := ev("tool.execute.before", "call_HINTCANARY_c1", t0, map[string]any{"args": map[string]any{}}) +
		ev("tool.execute.after", "call_HINTCANARY_c1", t0+20, map[string]any{"metadata": map[string]any{}, "status": "completed", "output": "[redacted:len=4100]", "context": part}) +
		ev("tool.execute.before", "call_HINTCANARY_c2", t0+30, map[string]any{"args": map[string]any{}}) +
		ev("tool.execute.after", "call_HINTCANARY_c2", t0+40, map[string]any{"metadata": map[string]any{}, "status": "completed", "output": "[redacted:len=10]", "context_invalid": true})
	must(t, run(t, shop, stream, "adapter", "opencode", "--project", shop))
	out := t.TempDir()
	must(t, run(t, shop, "", "export", "--out", out))
	files, _ := filepath.Glob(filepath.Join(out, "*.hint.json"))
	var stages []string
	missing := 0
	for _, f := range files {
		var doc map[string]any
		b, _ := os.ReadFile(f)
		_ = json.Unmarshal(b, &doc)
		if doc["hint_version"] != "0.2.0" {
			t.Fatalf("%s: version %v", filepath.Base(f), doc["hint_version"])
		}
		if doc["document_type"] != "activity_stream" {
			continue
		}
		// the source says that it observes hint_context calls (C11C-2: absence is then known)
		if src := doc["sources"].([]any)[0].(map[string]any); !strings.Contains(fmt.Sprint(src["capabilities"]), "context") {
			t.Errorf("source capabilities: %v", src["capabilities"])
		}
		calls := map[any]bool{}
		for _, o := range doc["payload"].(map[string]any)["observations"].([]any) {
			m := o.(map[string]any)
			if c, ok := m["context"].(map[string]any); ok {
				stages = append(stages, c["stage"].(string))
				calls[m["correlation"].(map[string]any)["tool_call_id"]] = true
				if c["stage"] == "delivered_context" && c["bytes"].(float64) != 4100 {
					t.Errorf("delivered bytes: %v", c)
				}
			}
			for _, x := range m["missing"].([]any) {
				if x == "context" {
					missing++
				}
			}
		}
		if len(calls) != 1 {
			t.Errorf("two stages must share one call: %v", calls)
		}
	}
	sort.Strings(stages)
	if strings.Join(stages, ",") != "delivered_context,raw_tool_output" || missing != 1 {
		t.Fatalf("stages %v, missing context %d", stages, missing)
	}
	noCanary(t, "export", readAll(t, out), shop, `"ses_`, `"call_`)
}
