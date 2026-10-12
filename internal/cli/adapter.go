package cli

import (
	"bytes"
	"encoding/json"

	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

func snapshotDiff(s *store.Store, a, b *snapshot.Local) (snapshot.ChangeSet, []snapshot.LocalChange) {
	return snapshot.Diff(s.Key(), a, b)
}

func cmdAdapter(e *Env, args []string) int {
	switch {
	case len(args) >= 2 && args[0] == "install" && args[1] == "opencode":
		return adapterInstall(e, args[2:])
	case len(args) >= 1 && args[0] == "opencode":
		return adapterOpenCode(e, args[1:])
	}
	fmt.Fprintln(e.Stderr, "usage: git hint adapter install opencode [--force]   [writes .opencode/plugins/hint-adapter.ts]")
	fmt.Fprintln(e.Stderr, "       git hint adapter opencode --project <dir>      [internal: started by the plugin]")
	return 2
}

func adapterInstall(e *Env, args []string) int {
	fs := flags(e, "adapter install opencode", "writes .opencode/plugins/hint-adapter.ts; no network")
	force := fs.Bool("force", false, "replace a different existing file")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	dir := filepath.Join(s.Root, ".opencode", "plugins")
	p := filepath.Join(dir, "hint-adapter.ts")
	if old, err := os.ReadFile(p); err == nil {
		if bytes.Equal(old, githint.OpenCodePlugin) {
			fmt.Fprintf(e.Stdout, "The OpenCode adapter is already installed: %s\n", p)
			return 0
		}
		if !*force {
			return fail(e, fmt.Errorf("%s exists and differs; use --force to replace it", p))
		}
	}
	others, _ := filepath.Glob(filepath.Join(dir, "*.ts"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(e, err)
	}
	if err := os.WriteFile(p, githint.OpenCodePlugin, 0o644); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Installed the OpenCode adapter: %s\n", p)
	fmt.Fprintln(e.Stdout, "OpenCode starts `git-hint adapter opencode` from PATH (or HINT_BIN). Tested with OpenCode 2.0.26.")
	for _, o := range others {
		if filepath.Base(o) != "hint-adapter.ts" {
			fmt.Fprintf(e.Stdout, "Note: OpenCode also loads %s from this directory.\n", o)
		}
	}
	return 0
}

// adapterOpenCode is the long-lived child of the OpenCode plugin. It reads reduced plugin
// events from stdin and appends one observation per finished tool call to the spool.
// Exit codes: 0 done, 1 setup error, 3 spool write failure (the plugin counts drops).
func adapterOpenCode(e *Env, args []string) int {
	fs := flags(e, "adapter opencode", "reads stdin; writes the activity spool; no network")
	dir := fs.String("project", e.Dir, "project directory of the OpenCode session")
	agentVersion := fs.String("agent-version", os.Getenv("HINT_AGENT_VERSION"), "OpenCode version, if known")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := store.Open(*dir)
	if err != nil {
		return fail(e, err)
	}
	if err := s.CheckExcluded(); err != nil {
		return fail(e, err)
	}
	src := activity.OpenCodeSource(s.Key(), *agentVersion)
	if err := project.RegisterSource(s, src); err != nil {
		return fail(e, err)
	}
	n := &activity.Normalizer{Key: s.Key(), SourceID: src.SourceID, Cwd: cleanAbs(*dir), Resolve: resolver(s), Now: e.Now}
	// On SIGTERM or SIGINT (OpenCode exit), end the input so that open calls are written
	// with the end time missing instead of being lost.
	in := e.Stdin
	if f, ok := e.Stdin.(*os.File); ok {
		pr, pw := io.Pipe()
		go func() { _, err := io.Copy(pw, f); pw.CloseWithError(err) }()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		go func() { <-sig; pw.Close() }()
		in = pr
	}
	spool, err := project.OpenSpool(s)
	if err != nil {
		return fail(e, err)
	}
	defer spool.Close()
	written := 0
	skipped, err := n.Stream(in, func(o activity.Observation) error {
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		if err := spool.Append(b); err != nil {
			return err
		}
		written++
		return nil
	})
	fmt.Fprintf(e.Stderr, "adapter opencode: written=%d skipped=%d\n", written, skipped)
	if err != nil {
		fmt.Fprintf(e.Stderr, "adapter opencode: %v\n", err)
		if strings.HasPrefix(err.Error(), "emit:") {
			return 3
		}
		return 1
	}
	return 0
}

func cleanAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return filepath.ToSlash(filepath.Clean(p))
	}
	return filepath.ToSlash(a)
}

// resolver maps an absolute path to a file ID when the path lies in a configured
// repository. Both the configured and the symlink-resolved roots are checked.
func resolver(s *store.Store) activity.Resolver {
	type root struct{ dir, id string }
	var roots []root
	for _, r := range s.Cfg.Repos {
		id := s.RepoID(r)
		d := cleanAbs(s.RepoRoot(r))
		roots = append(roots, root{d, id})
		if ev, err := filepath.EvalSymlinks(d); err == nil && filepath.ToSlash(ev) != d {
			roots = append(roots, root{filepath.ToSlash(ev), id})
		}
	}
	return func(abs string) (string, bool) {
		for _, r := range roots {
			if rel, ok := strings.CutPrefix(abs, r.dir+"/"); ok && rel != "" && !strings.HasPrefix(rel, ".hint/") {
				return hint.FileID(s.Key(), r.id, rel), true
			}
		}
		return "", false
	}
}
