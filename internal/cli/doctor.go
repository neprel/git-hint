package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
)

// adapterInfo is what `doctor` reports per agent adapter: evidence class, fields, limits.
var adapterInfo = []struct{ agent, status, evidence, fields, limits string }{
	{"OpenCode", "available", "live check with OpenCode 2.0.26 (macOS); offline replay of recorded events",
		"tool start/end, session and call IDs, targets, outcome, exit code, output size, shell command class",
		"no task, turn or token usage; no event for a call that OpenCode never finishes"},
	{"Claude Code", "not available", "documentation only", "—", "adapter not implemented in this version"},
	{"Codex", "not available", "source code only", "—", "adapter not implemented in this version"},
}

func cmdDoctor(e *Env, args []string) int {
	fs := flags(e, "doctor", "reads local state and tools; no network")
	if fs.Parse(args) != nil {
		return 2
	}
	w := e.Stdout
	bad := 0
	check := func(ok bool, what, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			bad++
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", mark, what, detail)
	}
	info := func(what, detail string) { fmt.Fprintf(w, "[info] %s: %s\n", what, detail) }

	info("platform", runtime.GOOS+"/"+runtime.GOARCH+supportNote())
	gitv, err := exec.Command("git", "--version").Output()
	check(err == nil, "git", strings.TrimSpace(string(gitv)))
	_, err = hint.LoadSchemas()
	check(err == nil, "HINT schemas v0.1 (embedded)", errText(err, "compiled"))

	s, err := open(e)
	if err != nil {
		check(false, "project store", err.Error())
		return 1
	}
	check(s.CheckExcluded() == nil, ".hint/ ignored by Git", errText(s.CheckExcluded(), "yes"))
	kf, _ := s.KeyFile()
	if fi, err := os.Stat(kf); err != nil {
		check(false, "project key", err.Error())
	} else {
		perm := fi.Mode().Perm()
		inTree := strings.HasPrefix(cleanAbs(kf), cleanAbs(s.Root)+"/")
		check(perm&0o077 == 0 || runtime.GOOS == "windows", "project key permissions", fmt.Sprintf("%s %v", kf, perm))
		check(!inTree, "project key outside the work tree", kf)
	}
	for _, r := range s.Cfg.Repos {
		_, err := os.Stat(s.RepoRoot(r))
		check(err == nil, "repository "+r.Name, s.RepoRoot(r))
	}
	if _, err := project.BuildPackage(s, project.ExportOptions{}); err != nil {
		info("export check", err.Error())
	} else {
		check(true, "export check", "a package builds and validates")
	}
	plugin := filepath.Join(s.Root, ".opencode", "plugins", "hint-adapter.ts")
	if b, err := os.ReadFile(plugin); err == nil {
		check(bytes.Equal(b, githint.OpenCodePlugin), "OpenCode adapter plugin", plugin+matchNote(bytes.Equal(b, githint.OpenCodePlugin)))
	} else {
		info("OpenCode adapter plugin", "not installed (git hint adapter install opencode)")
	}
	if p, err := exec.LookPath("git-hint"); err == nil {
		info("git-hint on PATH", p+" (the OpenCode plugin starts it)")
	} else {
		info("git-hint on PATH", "not found; set HINT_BIN for the OpenCode plugin")
	}
	for _, a := range adapterInfo {
		info("adapter "+a.agent, fmt.Sprintf("%s; evidence: %s; fields: %s; limits: %s", a.status, a.evidence, a.fields, a.limits))
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		info("opencode", "found at "+p+" (version not checked: doctor starts no agent)")
	}
	coverage := "Go: declarations and imports (go/parser, syntax level); other languages: file level"
	info("structural coverage", coverage)
	for _, tool := range []string{"scip-go", "scip-typescript", "scip-python", "sem"} {
		if p, err := exec.LookPath(tool); err == nil {
			info("optional "+tool, "found at "+p+"; not used by this version")
		} else {
			info("optional "+tool, "not found; not needed")
		}
	}
	if bad > 0 {
		return 1
	}
	return 0
}

func supportNote() string {
	if runtime.GOOS == "windows" {
		return " (Windows is not tested)"
	}
	return ""
}

func matchNote(ok bool) string {
	if ok {
		return " (matches this version)"
	}
	return " (differs from this version; run git hint adapter install opencode --force)"
}

func errText(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}
