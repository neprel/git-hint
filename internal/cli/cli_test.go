package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
