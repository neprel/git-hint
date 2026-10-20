package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/testkit"
)

// chainCanary is a frozen local name of the task-bound chain; it must never reach an export.
const chainCanary = "HintChainCanaryRate"

// TestTaskBoundChainArtifacts builds the task-bound journey with the real binary, offline:
// agent runs declared with HINT_TASK_ID go through the real OpenCode adapter (10 of them call
// the real `git-hint mcp` hint_context, seed and expansion), hint_collect joins one task by the
// same raw ID, one run declares nothing; the eval's episode names the session of task 01; a
// context variant of that eval runs; `git hint eval report` binds the objective to the task
// from the spool; `git hint export` writes the activity. With HINT_CHAIN_OUT set the export
// and the report are written for the server's chain test.
func TestTaskBoundChainArtifacts(t *testing.T) {
	s, l := validatedProject(t)
	dir := s.Root
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	p.Export = "results"
	s.Cfg.Evals = &p
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, dir, "go.mod", "module example.invalid/chain\n\ngo 1.22\n")
	testkit.WriteFile(t, dir, "pay/rate.go", "package pay\n\n// "+chainCanary+" gives a rate. HINTCANARY_CHAIN_COMMENT\nfunc "+chainCanary+"(a, b int) int { return a + b }\n")
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "base")
	bin := filepath.Join(t.TempDir(), "git-hint")
	if o, err := exec.Command("go", "build", "-o", bin, "../../../cmd/git-hint").CombinedOutput(); err != nil {
		t.Fatal(string(o))
	}
	hintRun := func(env []string, stdin string, args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir, c.Env, c.Stdin = dir, append(os.Environ(), env...), strings.NewReader(stdin)
		o, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git-hint %v: %v\n%s", args, err, o)
		}
		return string(o)
	}
	hintRun(nil, "", "init", "--context-limits", "depth=1,items=10")
	hintRun(nil, "", "collect", "--full")

	// two real hint_context answers (seed, then the expansion of the start item's ref)
	// the MCP server over stdio, as an orchestrator drives it: input stays open until every
	// answer arrived
	mcpCall := func(tool string, calls ...map[string]any) []map[string]any {
		t.Helper()
		c := exec.Command(bin, "mcp")
		c.Dir, c.Env = dir, os.Environ()
		in, _ := c.StdinPipe()
		outp, _ := c.StdoutPipe()
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		send := func(m map[string]any) { b, _ := json.Marshal(m); in.Write(append(b, '\n')) }
		send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "chain", "version": "0"}}})
		send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		for i, a := range calls {
			send(map[string]any{"jsonrpc": "2.0", "id": i + 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": a}})
		}
		var res []map[string]any
		sc := bufio.NewScanner(outp)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for len(res) < len(calls) && sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil && m["id"] != nil && m["id"].(float64) >= 2 {
				r, _ := m["result"].(map[string]any)
				if r == nil || r["isError"] == true {
					t.Fatalf("%s: %s", tool, sc.Bytes())
				}
				res = append(res, r)
			}
		}
		in.Close()
		c.Wait()
		return res
	}
	mcp := func(calls ...map[string]any) []map[string]any { return mcpCall("hint_context", calls...) }
	seed := mcp(map[string]any{"seed": chainCanary})
	if len(seed) != 1 {
		t.Fatalf("hint_context: %v", seed)
	}
	ref := seed[0]["structuredContent"].(map[string]any)["items"].([]any)[0].(map[string]any)["ref"].(string)
	expand := mcp(map[string]any{"ref": ref})
	reduce := func(res map[string]any) (int, map[string]any) { // what the plugin passes on
		n := 0
		var meta map[string]any
		for _, c := range res["content"].([]any) {
			text := c.(map[string]any)["text"].(string)
			n += len(text)
			if strings.HasPrefix(text, "hint-context-measurement/1 ") {
				json.Unmarshal([]byte(strings.TrimPrefix(text, "hint-context-measurement/1 ")), &meta)
			}
		}
		return n, meta
	}
	file := filepath.Join(dir, "pay", "rate.go")
	runs := 0
	events := func(session string, withContext bool, reads int) string {
		runs++
		run := fmt.Sprintf("run%02d", runs) // call IDs are unique per agent run
		var b strings.Builder
		ts := int64(1792000000000)
		ev := func(call, tool string, before, after map[string]any) {
			for _, e := range []map[string]any{
				{"hook": "tool.execute.before", "t_wall_ms": ts, "input": map[string]any{"tool": tool, "sessionID": session, "callID": run + "-" + call}, "output": before},
				{"hook": "tool.execute.after", "t_wall_ms": ts + 5, "input": map[string]any{"tool": tool, "sessionID": session, "callID": run + "-" + call}, "output": after}} {
				x, _ := json.Marshal(e)
				b.Write(append(x, '\n'))
			}
			ts += 10
		}
		if withContext {
			for i, res := range [][]map[string]any{seed, expand} {
				n, meta := reduce(res[0])
				ev(fmt.Sprintf("%s-ctx%d", session, i), "githint_hint_context", map[string]any{"args": map[string]any{}},
					map[string]any{"metadata": map[string]any{}, "status": "completed", "output": fmt.Sprintf("[redacted:len=%d]", n), "context": meta})
			}
		}
		for i := range reads {
			ev(fmt.Sprintf("%s-r%d", session, i), "read", map[string]any{"args": map[string]any{"filePath": file}},
				map[string]any{"metadata": map[string]any{}, "status": "completed", "output": "[redacted:len=120]"})
		}
		ev(session+"-e", "edit", map[string]any{"args": map[string]any{"filePath": file}},
			map[string]any{"metadata": map[string]any{"files": []any{map[string]any{"filePath": file}}}, "status": "completed", "output": "[redacted:len=0]"})
		ev(session+"-t", "bash", map[string]any{"args": map[string]any{"command": "go test ./...", "workdir": dir}},
			map[string]any{"metadata": map[string]any{"exit": 0}, "status": "completed", "output": "[redacted:len=40]"})
		return b.String()
	}
	for i := 1; i <= 20; i++ {
		// tasks 1-10 get context and need fewer reads; tasks 11-20 read more
		reads := 6
		if i <= 10 {
			reads = 3
		}
		hintRun([]string{fmt.Sprintf("HINT_TASK_ID=chain-task-%02d", i)}, events(fmt.Sprintf("ses_chain_t%02d", i), i <= 10, reads), "adapter", "opencode", "--project", dir)
	}
	hintRun([]string{"HINT_TASK_ID="}, events("ses_chain_free", false, 4), "adapter", "opencode", "--project", dir) // undeclared
	// hint_collect joins task 01 by the same raw ID
	testkit.WriteFile(t, dir, "pay/extra.go", "package pay\n\nfunc Extra() int { return 1 }\n")
	mcpCall("hint_collect", map[string]any{"task_id": "chain-task-01"})

	// the eval came from the session of task 01: its episode names that session
	putChainEpisode(t, l, "ep-00000000000000e1", "ses_chain_t01")
	base := baselineRef(t, l)
	v, err := MakeVariant(l, base, VariantInput{Arm: "context_only", Context: []ContextFile{{Path: "AGENTS.md", Data: []byte("Run go test ./... before you finish.\n")}}}, variantNow)
	if err != nil {
		t.Fatal(err)
	}
	if q := Qualify(context.Background(), s, &e2eRunner{}, variantNow); q.States["validated"] != 1 {
		t.Fatalf("variant: %+v", q)
	}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "baseline", Evals: []string{base}}, variantNow); err != nil {
		t.Fatal(err)
	}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "context_only", Evals: []string{strings.Split(v.Ref, "@")[0] + "@2"}}, variantNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reportFile := filepath.Join(t.TempDir(), "report.json")
	hintRun(nil, "", "eval", "report", "-o", reportFile)
	rep, _ := os.ReadFile(reportFile)
	var r map[string]any
	json.Unmarshal(rep, &r)
	tasks := map[any]int{}
	for _, x := range r["trials"].([]any) {
		tasks[x.(map[string]any)["source_task_id"]]++
	}
	if len(tasks) != 1 || tasks[nil] != 0 {
		t.Fatalf("source tasks of the trials: %v", tasks)
	}
	exp := t.TempDir()
	hintRun(nil, "", "export", "--out", exp)
	files, _ := filepath.Glob(filepath.Join(exp, "*.hint.json"))
	var all strings.Builder
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all.Write(b)
	}
	all.Write(rep)
	for _, c := range []string{chainCanary, "HINTCANARY_", "chain-task-", "ses_chain", "rate.go", "pay/"} {
		if strings.Contains(all.String(), c) {
			t.Fatalf("local name %q left the machine", c)
		}
	}
	// negative cases through the same builder and the real spool: a changed declaration in
	// the same session, then an undeclared run in it, leave the objective unbound
	trialNames, _ := l.Manifests("trial")
	bound := func() map[any]int {
		sessions, agents, err := project.SessionTasks(s)
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := report.Builder{Key: s.Key(), Namespace: s.Cfg.ProjectNamespace, Sessions: sessions, AgentSources: agents}.Build(l, trialNames, variantNow.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		got := map[any]int{}
		for _, x := range m["trials"].([]any) {
			got[x.(map[string]any)["source_task_id"]]++
		}
		return got
	}
	if got := bound(); len(got) != 1 || got[nil] != 0 {
		t.Fatalf("builder before the negative cases: %v", got)
	}
	hintRun([]string{"HINT_TASK_ID=chain-task-99"}, events("ses_chain_t01", false, 1), "adapter", "opencode", "--project", dir)
	if got := bound(); len(got) != 1 || got[nil] == 0 {
		t.Fatalf("changed declaration in the same session: %v", got)
	}
	hintRun([]string{"HINT_TASK_ID="}, events("ses_chain_t02", false, 1), "adapter", "opencode", "--project", dir)
	if out := os.Getenv("HINT_CHAIN_OUT"); out != "" {
		os.MkdirAll(filepath.Join(out, "export"), 0o755)
		for _, f := range files {
			b, _ := os.ReadFile(f)
			os.WriteFile(filepath.Join(out, "export", filepath.Base(f)), b, 0o644)
		}
		os.WriteFile(filepath.Join(out, "report.json"), rep, 0o644)
	}
}

func putChainEpisode(t *testing.T, l *library.Library, id, session string) {
	t.Helper()
	src := library.Digest([]byte("chain"))
	ep := map[string]any{"record": "episode", "manifest_version": 1, "episode_id": id, "created_at": "2026-10-17T00:00:00Z", "rules_version": "0.1.0",
		"sources":      []any{map[string]any{"agent": "opencode", "parser_version": "0.1.0", "source": src, "session": session, "role": "primary"}},
		"boundary":     map[string]any{"basis": "inferred", "start": map[string]any{"source": src, "record": "u"}, "rule": "r", "support": "high"},
		"requests":     []any{map[string]any{"at": map[string]any{"source": src, "record": "u"}}},
		"repositories": []any{map[string]any{"repo": "shop", "base_evidence": "unavailable"}}, "links": []any{}, "attribution": "exclusive", "reasons": []any{}}
	if _, err := l.PutManifest("episode", ep); err != nil {
		t.Fatal(err)
	}
}
