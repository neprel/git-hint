package trees

// Local repeated source observations only. No project/start/export/catalogue authority.
import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/readgit"
)

const SourceViewVersion = "git-selected-source-observations-v1"
const sourceMetadataCap = int64(4 << 20)
const sourceQueryCap = int64(16 << 20)

type SourceViewOptions struct{ Limits Limits }
type SourceFact struct {
	Role, Path, State, ObjectDigest, NodeIdentity, AbsenceIdentity string
	Bytes                                                          int64
}
type SourceViewReceipt struct {
	Version, Authority, State, ChainTiming                                                             string
	Pass                                                                                               uint8
	InvocationIdentity, ExecutableDigest, ExecutableNode                                               string
	CandidatePaths                                                                                     []string
	ConfigObject, StateObject, SourceDigest, SelectedInventoryDigest, StageInventoryDigest, TreeDigest string
	Facts                                                                                              []SourceFact
	SourceQueryAttempts                                                                                int
	SourceStdoutBytes, SourceStderrBytes                                                               int64
	TotalDispatchAttempts, PersistenceGuardAttempts, ExecutableReads                                   int
	ExecutableReadBytes, GuardStdoutBytes, GuardStderrBytes                                            int64
	MetadataReads                                                                                      int
	MetadataBytes                                                                                      int64
	BeginOffsetNS, EndOffsetNS                                                                         int64
}
type SelectedSourceViewCapture struct {
	selected *SelectedCapture
	sources  SourceViewReceipt
}

func (c *SelectedSourceViewCapture) Selected() *SelectedCapture {
	if c == nil {
		return nil
	}
	return &SelectedCapture{receipt: c.selected.Receipt()}
}
func (c *SelectedSourceViewCapture) Sources() SourceViewReceipt {
	if c == nil {
		return SourceViewReceipt{}
	}
	r := c.sources
	r.CandidatePaths = append([]string{}, r.CandidatePaths...)
	r.Facts = append([]SourceFact{}, r.Facts...)
	return r
}

type configTuple struct {
	Scope, Origin, Key, Value string
	HasValue                  bool
}
type sourceSnapshot struct {
	Facts        []SourceFact
	Config       []byte
	Head, Branch string
	raw          map[string][]byte
}
type sourceViewDispatchKey struct{}
type sourceViewDispatcher interface {
	sourceOutput(context.Context, string, int64, ...string) ([]byte, error)
}
type SelectedSourceView struct {
	checkPhase              string
	openSnapshot            sourceSnapshot
	mu                      sync.Mutex
	session                 *SelectedSourceSession
	invocation              *readgit.SourceInvocation
	root, home, xdg, gitdir string
	external, git           *worktreeRoot
	candidates              []string
	queries                 int
	stdout, stderr          int64
	reads                   [2]int
	readBytes               [2]int64
	pass                    int
	before, after           sourceSnapshot
	chain, firstChain       []string
	firstSnapshot           *sourceSnapshot
	firstSelected           *SelectedReceipt
	failed, closed          bool
}

func sourceProblem() error {
	return unsupported("source_resolution_unsupported", "the strict Git source view is unavailable")
}
func cleanAbsolute(p string) bool {
	return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p && utf8.ValidString(p) && !strings.ContainsAny(p, "\x00\r\n\t")
}
func safeRolePath(p string) bool {
	if !cleanAbsolute(p) {
		return false
	}
	for _, c := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.EqualFold(c, ".hint") {
			return false
		}
	}
	return true
}

// OpenSelectedSourceView creates the owned session FIRST. Its private anchor and
// original deadline precede invocation discovery; both receipt offset families use
// that same origin. No caller/backdated anchor or second session allowance exists.
func OpenSelectedSourceView(ctx context.Context, l *library.Library, root string, o SourceViewOptions) (*SelectedSourceView, error) {
	s, e := OpenSelectedSourceSession(ctx, l, root, SourceSessionOptions{Limits: o.Limits})
	if e != nil {
		return nil, e
	}
	v := &SelectedSourceView{session: s, root: s.source.root}
	phase := "invocation"
	fail := func() (*SelectedSourceView, error) {
		v.Close()
		if phase == "initial_snapshot" && v.checkPhase != "" {
			phase = v.checkPhase
		}
		return nil, unsupported("source_resolution_unsupported", "the strict source-view phase refused: %s", phase)
	}
	inv, e := readgit.FreezeSourceInvocation(s.ctx, os.Environ())
	if e != nil {
		return fail()
	}
	v.invocation = inv
	// Overwrite any outer invocation scope with this view's OWN frozen object.
	s.ctx = readgit.WithSourceInvocation(s.ctx, inv)
	s.ctx = context.WithValue(s.ctx, sourceViewDispatchKey{}, sourceViewDispatcher(v))
	for _, x := range inv.Environment() {
		k, value, _ := strings.Cut(x, "=")
		if k == "HOME" {
			v.home = value
		}
		if k == "XDG_CONFIG_HOME" {
			v.xdg = value
		}
	}
	if v.xdg == "" {
		v.xdg = filepath.Join(v.home, ".config")
	}
	phase = "external_anchor"
	v.external, e = openWorktreeRoot(string(filepath.Separator), true)
	if e != nil {
		return fail()
	}
	phase = "version"
	version, e := v.query(s.ctx, 16<<10, "--version")
	if e != nil || string(version) != "git version 2.56.0\n" {
		return fail()
	}
	phase = "system_candidate"
	system, e := v.query(s.ctx, 16<<10, "var", "GIT_CONFIG_SYSTEM")
	if e != nil {
		return fail()
	}
	sys, e := sourceLines(system, 1)
	if e != nil || filepath.Base(sys[0]) != "gitconfig" {
		return fail()
	}
	phase = "global_candidates"
	global, e := v.query(s.ctx, 16<<10, "var", "GIT_CONFIG_GLOBAL")
	if e != nil {
		return fail()
	}
	globals, e := sourceLines(global, 2)
	if e != nil || !reflect.DeepEqual(globals, []string{filepath.Join(v.xdg, "git", "config"), filepath.Join(v.home, ".gitconfig")}) {
		return fail()
	}
	v.candidates = append(sys, globals...)
	phase = "root_layout"
	top, e := v.query(s.ctx, 16<<10, "rev-parse", "--show-toplevel")
	if e != nil || string(top) != v.root+"\n" {
		return fail()
	}
	phase = "gitdir_layout"
	gitdir, e := v.query(s.ctx, 16<<10, "rev-parse", "--absolute-git-dir")
	if e != nil || string(gitdir) != filepath.Join(v.root, ".git")+"\n" {
		return fail()
	}
	v.gitdir = filepath.Join(v.root, ".git")
	phase = "common_layout"
	common, e := v.query(s.ctx, 16<<10, "rev-parse", "--git-common-dir")
	if e != nil || (string(common) != ".git\n" && string(common) != v.gitdir+"\n") {
		return fail()
	}
	phase = "git_anchor"
	v.git, e = openWorktreeRoot(v.gitdir, true)
	if e != nil {
		return fail()
	}
	phase = "initial_snapshot"
	snapshot, e := v.snapshot(s.ctx)
	if e != nil {
		return fail()
	}
	v.before = snapshot
	v.openSnapshot = snapshot
	s.hooks.observer = v
	return v, nil
}
func sourceLines(b []byte, count int) ([]string, error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil, sourceProblem()
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	if len(lines) != count {
		return nil, sourceProblem()
	}
	for _, p := range lines {
		if !safeRolePath(p) {
			return nil, sourceProblem()
		}
	}
	return lines, nil
}
func (v *SelectedSourceView) sourceOutput(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	if v.failed || v.queries >= 38 || root != v.root {
		return nil, sourceProblem()
	}
	left := sourceQueryCap - v.stdout - v.stderr
	if left < 1 {
		return nil, sourceProblem()
	}
	if limit > left {
		limit = left
	}
	v.queries++
	before := v.invocation.Stats()
	out, e := readgit.OutputWithSourceInvocation(ctx, v.invocation, root, limit, args...)
	after := v.invocation.Stats()
	v.stdout += after.StdoutBytes - before.StdoutBytes
	v.stderr += after.StderrBytes - before.StderrBytes
	if v.stdout+v.stderr > sourceQueryCap {
		return nil, sourceProblem()
	}
	return out, e
}
func (v *SelectedSourceView) query(ctx context.Context, limit int64, args ...string) ([]byte, error) {
	return v.session.source.git(ctx, limit, args...)
}
func (v *SelectedSourceView) meta(ctx context.Context, role, path string) (SourceFact, []byte, error) {
	v.checkPhase = "metadata_" + role
	var zero SourceFact
	if !safeRolePath(path) || v.reads[v.pass] >= 1024 {
		return zero, nil, sourceProblem()
	}
	v.reads[v.pass]++
	remaining := sourceMetadataCap - v.readBytes[v.pass]
	if remaining < 0 {
		return zero, nil, sourceProblem()
	}
	limit := int64(1 << 20)
	if remaining < limit {
		limit = remaining
	}
	anchor := v.external
	relative := strings.TrimPrefix(filepath.ToSlash(path), "/")
	if path == v.gitdir || strings.HasPrefix(path, v.gitdir+string(filepath.Separator)) {
		anchor = v.git
		relative = strings.TrimPrefix(path, v.gitdir+string(filepath.Separator))
	}
	if role == "selected_chain_gitignore" {
		anchor = v.session.source
		relative = strings.TrimPrefix(path, v.root+string(filepath.Separator))
	}
	if anchor == nil || !validPath(filepath.ToSlash(relative)) {
		return zero, nil, sourceProblem()
	}
	entry, e := anchor.read(ctx, filepath.ToSlash(relative), limit, true, readHooks{observe: true})
	if e != nil {
		return zero, nil, e
	}
	v.readBytes[v.pass] += int64(len(entry.data))
	fact := SourceFact{Role: role, Path: path}
	if entry.absent {
		fact.State = "observed_absent"
		fact.AbsenceIdentity = entry.absenceIdentity
		return fact, nil, nil
	}
	if entry.skip || entry.pathIdentity == "" {
		return zero, nil, sourceProblem()
	}
	fact.State = "present_regular"
	fact.Bytes = int64(len(entry.data))
	fact.NodeIdentity = entry.pathIdentity
	fact.ObjectDigest = library.Digest(entry.data)
	return fact, entry.data, nil
}
func parseConfig(b []byte) ([]configTuple, error) {
	var rows []configTuple
	for len(b) > 0 {
		if len(rows) >= 8192 {
			return nil, sourceProblem()
		}
		fields := make([]string, 3)
		for i := 0; i < 3; i++ {
			end := bytes.IndexByte(b, 0)
			if end < 0 {
				return nil, sourceProblem()
			}
			fields[i] = string(b[:end])
			b = b[end+1:]
		}
		if len(fields[1]) > 4096 {
			return nil, sourceProblem()
		}
		key, value, has := strings.Cut(fields[2], "\n")
		if key == "" || len(key) > 1024 || len(value) > 1<<20 || strings.ContainsAny(key, "\r\n\x00") || !utf8.ValidString(key) {
			return nil, sourceProblem()
		}
		row := configTuple{fields[0], fields[1], key, value, has}
		rows = append(rows, row)
	}
	return rows, nil
}
func (v *SelectedSourceView) originPath(origin string) (string, error) {
	if !strings.HasPrefix(origin, "file:") {
		return "", sourceProblem()
	}
	p := strings.TrimPrefix(origin, "file:")
	if !filepath.IsAbs(p) {
		if p != ".git/config" {
			return "", sourceProblem()
		}
		p = filepath.Join(v.root, p)
	}
	if !safeRolePath(p) {
		return "", sourceProblem()
	}
	for _, candidate := range append(append([]string{}, v.candidates...), filepath.Join(v.gitdir, "config")) {
		if p == candidate {
			return p, nil
		}
	}
	return "", sourceProblem()
}
func (v *SelectedSourceView) validateConfig(rows []configTuple) error {
	command := []configTuple{}
	for _, r := range rows {
		lower := strings.ToLower(r.Key)
		if strings.HasPrefix(lower, "include.") || strings.HasPrefix(lower, "includeif.") || strings.HasPrefix(lower, "extensions.") || lower == "core.worktree" {
			return sourceProblem()
		}
		if (lower == "core.bare" || lower == "core.sparsecheckout") && (!r.HasValue || r.Value != "false") {
			return sourceProblem()
		}
		if lower == "core.excludesfile" && (!r.HasValue || !(strings.HasPrefix(r.Value, "/") || strings.HasPrefix(r.Value, "~/"))) {
			return sourceProblem()
		}
		switch r.Scope {
		case "system", "global", "local":
			if _, e := v.originPath(r.Origin); e != nil {
				return e
			}
		case "command":
			if r.Origin == "" {
				return sourceProblem()
			}
			command = append(command, r)
		default:
			return sourceProblem()
		}
	}
	if len(command) != 2 || command[0].Key != "core.fsmonitor" || command[0].Value != "false" || !command[0].HasValue || command[1].Key != "core.hookspath" || command[1].Value != "/dev/null" || !command[1].HasValue {
		return sourceProblem()
	}
	return nil
}
func (v *SelectedSourceView) snapshot(ctx context.Context) (sourceSnapshot, error) {
	snap := sourceSnapshot{raw: map[string][]byte{}}
	add := func(role, path string) error {
		fact, data, e := v.meta(ctx, role, path)
		if e != nil {
			return e
		}
		snap.Facts = append(snap.Facts, fact)
		if fact.State == "present_regular" {
			snap.raw[fact.ObjectDigest] = append([]byte(nil), data...)
		}
		return nil
	}
	for i, path := range v.candidates {
		role := "global_config"
		if i == 0 {
			role = "system_config"
		}
		if e := add(role, path); e != nil {
			return snap, e
		}
	}
	if e := add("repo_config", filepath.Join(v.gitdir, "config")); e != nil {
		return snap, e
	}
	v.checkPhase = "config_list"
	raw, e := v.query(ctx, readgit.MaxOutput, "config", "list", "--no-includes", "--null", "--show-origin", "--show-scope")
	if e != nil {
		return snap, e
	}
	v.checkPhase = "config_framing"
	rows, e := parseConfig(raw)
	if e != nil {
		return snap, e
	}
	v.checkPhase = "config_profile"
	if v.validateConfig(rows) != nil {
		return snap, sourceProblem()
	}
	snap.Config = append([]byte(nil), raw...)
	v.checkPhase = "config_path_get"
	expanded, e := v.query(ctx, readgit.MaxOutput, "config", "get", "--no-includes", "--null", "--show-origin", "--show-scope", "--type=path", "core.excludesFile")
	excludes := ""
	if readgit.ExitOne(e) && len(expanded) == 0 {
		for _, r := range rows {
			if strings.EqualFold(r.Key, "core.excludesfile") {
				return snap, sourceProblem()
			}
		}
		excludes = filepath.Join(v.xdg, "git", "ignore")
	} else {
		if e != nil {
			return snap, e
		}
		parts := bytes.Split(expanded, []byte{0})
		if len(parts) != 4 || len(parts[3]) != 0 {
			return snap, sourceProblem()
		}
		if _, e = v.originPath(string(parts[1])); e != nil {
			return snap, e
		}
		if string(parts[0]) != "system" && string(parts[0]) != "global" && string(parts[0]) != "local" {
			return snap, sourceProblem()
		}
		excludes = string(parts[2])
	}
	if !safeRolePath(excludes) {
		return snap, sourceProblem()
	}
	if e = add("global_exclude", excludes); e != nil {
		return snap, e
	}
	if e = add("local_exclude", filepath.Join(v.gitdir, "info", "exclude")); e != nil {
		return snap, e
	}
	v.checkPhase = "head_query"
	head, e := v.query(ctx, 16<<10, "rev-parse", "--verify", "--quiet", "HEAD")
	if e != nil || len(head) != 41 || head[40] != '\n' || !gitHash.Match(head[:40]) {
		return snap, sourceProblem()
	}
	snap.Head = string(head[:40])
	v.checkPhase = "symbolic_head_query"
	branch, e := v.query(ctx, 16<<10, "symbolic-ref", "--quiet", "HEAD")
	if readgit.ExitOne(e) && len(branch) == 0 {
		snap.Branch = ""
	} else {
		if e != nil || len(branch) < 2 || branch[len(branch)-1] != '\n' {
			return snap, sourceProblem()
		}
		snap.Branch = string(branch[:len(branch)-1])
		if !strings.HasPrefix(snap.Branch, "refs/heads/") || !validPath(snap.Branch) || strings.ContainsAny(snap.Branch, "\r\n") {
			return snap, sourceProblem()
		}
	}
	for _, name := range []string{"HEAD", "index", "packed-refs", "config.worktree"} {
		if e = add("git_state", filepath.Join(v.gitdir, name)); e != nil {
			return snap, e
		}
		if name == "config.worktree" && snap.Facts[len(snap.Facts)-1].State != "observed_absent" {
			return snap, sourceProblem()
		}
	}
	if snap.Branch != "" {
		if e = add("git_state", filepath.Join(v.gitdir, filepath.FromSlash(snap.Branch))); e != nil {
			return snap, e
		}
	}
	sortSourceFacts(snap.Facts)
	return snap, nil
}
func sortSourceFacts(f []SourceFact) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Role != f[j].Role {
			return f[i].Role < f[j].Role
		}
		return f[i].Path < f[j].Path
	})
}
func sameSnapshot(a, b sourceSnapshot) bool {
	return a.Head == b.Head && a.Branch == b.Branch && bytes.Equal(a.Config, b.Config) && reflect.DeepEqual(a.Facts, b.Facts)
}
func (v *SelectedSourceView) addChain(ctx context.Context, snap *sourceSnapshot, chain []string) error {
	for _, p := range chain {
		f, b, e := v.meta(ctx, "selected_chain_gitignore", p)
		if e != nil {
			return e
		}
		snap.Facts = append(snap.Facts, f)
		if f.State == "present_regular" {
			snap.raw[f.ObjectDigest] = append([]byte(nil), b...)
		}
	}
	sortSourceFacts(snap.Facts)
	return nil
}
func (v *SelectedSourceView) beforeInventory(ctx context.Context) error {
	snap, e := v.snapshot(ctx)
	if e != nil {
		return e
	}
	if v.pass == 0 && !sameSnapshot(v.openSnapshot, snap) {
		v.checkPhase = "before_inventory_compare"
		return sourceProblem()
	}
	if v.pass == 1 {
		if e = v.addChain(ctx, &snap, v.firstChain); e != nil {
			return e
		}
	}
	v.before = snap
	return nil
}
func (v *SelectedSourceView) afterInventory(ctx context.Context, paths []string, stages []IndexStageFact) error {
	for _, s := range stages {
		if s.Stage != 0 || s.Mode == "120000" {
			return sourceProblem()
		}
	}
	chainSet := map[string]bool{}
	for _, p := range paths {
		if !validPath(p) {
			return sourceProblem()
		}
		dir := filepath.Dir(filepath.FromSlash(p))
		for {
			chainSet[filepath.Join(v.root, dir, ".gitignore")] = true
			if dir == "." {
				break
			}
			dir = filepath.Dir(dir)
		}
	}
	chain := make([]string, 0, len(chainSet))
	for p := range chainSet {
		chain = append(chain, p)
	}
	sort.Strings(chain)
	if len(chain) > 1024 {
		return sourceProblem()
	}
	if v.pass == 1 && !reflect.DeepEqual(chain, v.firstChain) {
		return sourceProblem()
	}
	v.chain = chain
	snap, e := v.snapshot(ctx)
	if e != nil {
		return e
	}
	if v.pass == 0 {
		if !sameSnapshot(v.before, snap) {
			v.checkPhase = "after_inventory_compare"
			return sourceProblem()
		}
		if e = v.addChain(ctx, &snap, chain); e != nil {
			return e
		}
	} else {
		if e = v.addChain(ctx, &snap, chain); e != nil {
			return e
		}
		if !sameSnapshot(v.before, snap) {
			v.checkPhase = "after_inventory_compare"
			return sourceProblem()
		}
	}
	v.after = snap
	return nil
}
func (v *SelectedSourceView) afterContent(ctx context.Context, selected SelectedReceipt) error {
	if selected.UnindexedAbsentCount != 0 {
		return sourceProblem()
	}
	snap, e := v.snapshot(ctx)
	if e != nil {
		return e
	}
	if e = v.addChain(ctx, &snap, v.chain); e != nil {
		return e
	}
	if !sameSnapshot(v.after, snap) {
		v.checkPhase = "after_content_compare"
		return sourceProblem()
	}
	if v.pass == 1 && (v.firstSnapshot == nil || !sameSnapshot(*v.firstSnapshot, snap)) {
		v.checkPhase = "repeat_source_compare"
		return sourceProblem()
	}
	v.after = snap
	return nil
}
func (v *SelectedSourceView) Capture(ctx context.Context) (*SelectedSourceViewCapture, error) {
	if v == nil || v.session == nil || ctx == nil {
		return nil, sourceProblem()
	}
	if !v.mu.TryLock() {
		v.session.cancel()
		return nil, sourceProblem()
	}
	defer v.mu.Unlock()
	if v.failed || v.closed || v.pass >= 2 {
		return nil, sourceProblem()
	}
	// Keep the owned discovery/session origin. Caller cancellation can only
	// tighten it, including metadata persistence after the selected pass returns.
	bounded, cancel := context.WithCancel(v.session.ctx)
	if deadline, ok := ctx.Deadline(); ok {
		if original, _ := bounded.Deadline(); deadline.Before(original) {
			cancel()
			bounded, cancel = context.WithDeadline(v.session.ctx, deadline)
		}
	}
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	fail := func(e error) (*SelectedSourceViewCapture, error) {
		v.failed = true
		v.session.cancel()
		if e == nil {
			e = sourceProblem()
		}
		return nil, e
	}
	if e := ctx.Err(); e != nil {
		return fail(e)
	}
	selected, e := v.session.Capture(bounded)
	if e != nil {
		return fail(e)
	}
	r := selected.Receipt()
	if v.pass == 1 {
		first := v.firstSelected
		if first == nil || first.RootIdentity != r.RootIdentity || first.TreeDigest != r.TreeDigest || first.SelectedInventoryDigest != r.SelectedInventoryDigest || first.StageInventoryDigest != r.StageInventoryDigest || !reflect.DeepEqual(first.Selected, r.Selected) {
			return fail(sourceProblem())
		}
	}
	// Only final unique metadata bytes are persisted; repeated checks do not multiply writes.
	for digest, data := range v.after.raw {
		got, e := v.session.library.PutObjectContext(bounded, data)
		if e != nil || got != digest {
			return fail(e)
		}
	}
	cfg, e := v.session.library.PutObjectContext(bounded, v.after.Config)
	if e != nil {
		return fail(e)
	}
	stateRaw, e := json.Marshal(struct {
		Head, Branch string
		Facts        []SourceFact
	}{v.after.Head, v.after.Branch, v.after.Facts})
	if e != nil {
		return fail(e)
	}
	state, e := v.session.library.PutObjectContext(bounded, stateRaw)
	if e != nil {
		return fail(e)
	}
	if e = bounded.Err(); e != nil {
		return fail(e)
	}
	if e = ctx.Err(); e != nil {
		return fail(e)
	}
	exe, node := v.invocation.ExecutableIdentity()
	stats := v.invocation.Stats()
	timing := "discovered_after_selection"
	if v.pass == 1 {
		timing = "observed_before_selection"
	}
	sourceDigest := receiptDigest(struct {
		Domain string
		Facts  []SourceFact
	}{SourceViewVersion, v.after.Facts})
	receipt := SourceViewReceipt{Version: SourceViewVersion, Authority: "unknown", State: "observed_source_inputs", ChainTiming: timing, Pass: r.Pass, InvocationIdentity: v.invocation.Identity(), ExecutableDigest: exe, ExecutableNode: node, CandidatePaths: append([]string(nil), v.candidates...), ConfigObject: cfg, StateObject: state, SourceDigest: sourceDigest, SelectedInventoryDigest: r.SelectedInventoryDigest, StageInventoryDigest: r.StageInventoryDigest, TreeDigest: r.TreeDigest, Facts: append([]SourceFact(nil), v.after.Facts...), SourceQueryAttempts: v.queries, SourceStdoutBytes: v.stdout, SourceStderrBytes: v.stderr, TotalDispatchAttempts: stats.Dispatches, PersistenceGuardAttempts: stats.Dispatches - v.queries, ExecutableReads: stats.ExecutableReads, ExecutableReadBytes: stats.ExecutableBytes, GuardStdoutBytes: stats.StdoutBytes - v.stdout, GuardStderrBytes: stats.StderrBytes - v.stderr, MetadataReads: v.reads[v.pass], MetadataBytes: v.readBytes[v.pass], BeginOffsetNS: r.BeginOffsetNS, EndOffsetNS: time.Since(v.session.anchor).Nanoseconds()}
	if receipt.EndOffsetNS > int64(strictCaptureSeconds) {
		return fail(context.DeadlineExceeded)
	}
	if v.pass == 0 {
		snapshot := v.after
		v.firstSnapshot = &snapshot
		copy := r
		v.firstSelected = &copy
		v.firstChain = append([]string(nil), v.chain...)
	}
	v.pass++
	return &SelectedSourceViewCapture{selected: selected, sources: receipt}, nil
}
func (v *SelectedSourceView) Close() error {
	if v == nil || v.session == nil {
		return nil
	}
	e := v.session.Close()
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		v.closed = true
		for _, a := range []*worktreeRoot{v.external, v.git} {
			if a != nil {
				if ce := a.closeErr(); ce != nil {
					e = sourceProblem()
				}
			}
		}
	}
	return e
}
