package trees

// Closed retained-artifact profile of the SAME SourceView: agent-authored Git
// metadata never selects host files. Host system/global config is disabled BY
// PROFILE (never recorded as observed absence). The only global-exclude input is
// an owned copy of retained bytes supplied by the caller's Native closure.
import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/readgit"
)

const SourceViewArtifactVersion = "git-selected-artifact-source-observations-v1"
const artifactReceiptState = "observed_artifact_inputs_host_config_disabled_by_profile"

// Artifact replay states name the retained Native global_exclude fact that the
// owned file copies. They are not new Native observations.
const (
	ArtifactReplayNativePresent = "native_global_exclude_present_replayed"
	ArtifactReplayNativeAbsent  = "native_global_exclude_absent_replayed_empty"
)

// ArtifactSourceOptions selects the artifact profile. ReplayDigest must equal the
// digest of Replay; an absent Native fact replays as an owned empty file.
type ArtifactSourceOptions struct {
	ReplayState, ReplayDigest string
	Replay                    []byte
}

type artifactSourceState struct {
	guard                              *readgit.SourceInvocation
	control, cwd, replayPath, replayID string
	controlRoot                        *worktreeRoot
	closed                             bool
}

// Closed artifact repository config: only ordinary git-init keys with literal values.
var artifactConfigBoolean = map[string]bool{"core.filemode": true, "core.logallrefupdates": true, "core.ignorecase": true, "core.precomposeunicode": true, "core.symlinks": true}

func validateArtifactConfig(rows []configTuple, configPath string) error {
	for _, r := range rows {
		lower := strings.ToLower(r.Key)
		if r.Scope != "command" || r.Origin != "file:"+configPath || !r.HasValue {
			return sourceProblem()
		}
		switch {
		case lower == "core.repositoryformatversion":
			if r.Value != "0" {
				return sourceProblem()
			}
		case lower == "core.bare":
			if r.Value != "false" {
				return sourceProblem()
			}
		case artifactConfigBoolean[lower]:
			if r.Value != "true" && r.Value != "false" {
				return sourceProblem()
			}
		default:
			return sourceProblem()
		}
	}
	return nil
}

func openArtifactSourceView(v *SelectedSourceView, o ArtifactSourceOptions) (*SelectedSourceView, error) {
	s := v.session
	phase := "artifact_options"
	fail := func() (*SelectedSourceView, error) {
		v.Close()
		if phase == "initial_snapshot" && v.checkPhase != "" {
			phase = v.checkPhase
		}
		return nil, unsupported("source_resolution_unsupported", "the strict artifact source-view phase refused: %s", phase)
	}
	switch o.ReplayState {
	case ArtifactReplayNativePresent:
	case ArtifactReplayNativeAbsent:
		if len(o.Replay) != 0 {
			return fail()
		}
	default:
		return fail()
	}
	if int64(len(o.Replay)) > 1<<20 || library.Digest(o.Replay) != o.ReplayDigest {
		return fail()
	}
	a := &artifactSourceState{replayID: o.ReplayDigest}
	v.artifact = a
	phase = "artifact_control"
	dir, e := os.MkdirTemp("", "hint-artifact-control-")
	if e != nil {
		return fail()
	}
	a.control = dir
	if dir, e = filepath.EvalSymlinks(dir); e != nil || !cleanAbsolute(dir) {
		return fail()
	}
	a.control = dir
	a.cwd = filepath.Join(dir, "cwd")
	a.replayPath = filepath.Join(dir, "exclude")
	for _, d := range []string{"home", "xdg", "cwd"} {
		if e = os.Mkdir(filepath.Join(dir, d), 0700); e != nil {
			return fail()
		}
	}
	for name, data := range map[string][]byte{"global": nil, "exclude": o.Replay} {
		if e = os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
			return fail()
		}
	}
	if a.controlRoot, e = openWorktreeRoot(dir, true); e != nil {
		return fail()
	}
	phase = "invocation"
	guard, e := readgit.FreezeSourceInvocation(s.ctx, os.Environ())
	if e != nil {
		return fail()
	}
	inv, e := readgit.FreezeArtifactSourceInvocation(s.ctx, os.Environ(), readgit.ArtifactSourceControl{Home: filepath.Join(dir, "home"), XDG: filepath.Join(dir, "xdg"), Global: filepath.Join(dir, "global"), Ceiling: dir}, guard)
	if e != nil {
		return fail()
	}
	gd, gn := guard.ExecutableIdentity()
	ad, an := inv.ExecutableIdentity()
	if guard.Executable() != inv.Executable() || gd != ad || gn != an {
		return fail()
	}
	a.guard, v.invocation = guard, inv
	// Persistence guards keep the Native invocation and the actual Store roots.
	// Only this view's sourceOutput adds rooted artifact arguments.
	s.ctx = readgit.WithSourceInvocation(s.ctx, guard)
	s.ctx = context.WithValue(s.ctx, sourceViewDispatchKey{}, sourceViewDispatcher(v))
	phase = "version"
	version, e := v.dispatch(s.ctx, a.cwd, 16<<10, "--version")
	if e != nil || string(version) != "git version 2.56.0\n" {
		return fail()
	}
	phase = "git_anchor"
	v.gitdir = filepath.Join(v.root, ".git")
	if v.git, e = openWorktreeRoot(v.gitdir, true); e != nil {
		return fail()
	}
	// Every metadata preflight and the explicit-file config parse precede the
	// first repository-aware Git dispatch, including rev-parse.
	phase = "initial_snapshot"
	snapshot, e := v.snapshot(s.ctx)
	if e != nil {
		return fail()
	}
	phase = "root_layout"
	top, e := v.query(s.ctx, 16<<10, "rev-parse", "--show-toplevel")
	if e != nil || string(top) != v.root+"\n" {
		return fail()
	}
	phase = "gitdir_layout"
	gitdir, e := v.query(s.ctx, 16<<10, "rev-parse", "--absolute-git-dir")
	if e != nil || string(gitdir) != v.gitdir+"\n" {
		return fail()
	}
	phase = "common_layout"
	common, e := v.query(s.ctx, 16<<10, "rev-parse", "--git-common-dir")
	if e != nil || string(common) != v.gitdir+"\n" {
		return fail()
	}
	v.before = snapshot
	v.openSnapshot = snapshot
	s.hooks.observer = v
	return v, nil
}

func (a *artifactSourceState) rootedArgs(v *SelectedSourceView) []string {
	return []string{"--git-dir=" + v.gitdir, "--work-tree=" + v.root, "-c", "core.excludesFile=" + a.replayPath}
}

// anchor constrains every artifact metadata role to the materialized root or the
// owned replay file BEFORE any open. No external/host anchor exists in this profile.
func (a *artifactSourceState) anchor(v *SelectedSourceView, role, path string) (*worktreeRoot, string, bool) {
	if role == "replayed_global_exclude" {
		return a.controlRoot, "exclude", path == a.replayPath && a.controlRoot != nil
	}
	prefix := v.root + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return nil, "", false
	}
	switch role {
	case "selected_chain_gitignore":
		rel := strings.TrimPrefix(path, prefix)
		return v.session.source, rel, filepath.Base(rel) == ".gitignore" && !strictProtectedPath(filepath.ToSlash(rel))
	case "repo_config", "local_exclude", "git_state":
		gitPrefix := v.gitdir + string(filepath.Separator)
		if v.git == nil || !strings.HasPrefix(path, gitPrefix) {
			return nil, "", false
		}
		return v.git, strings.TrimPrefix(path, gitPrefix), true
	}
	return nil, "", false
}

func (a *artifactSourceState) close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	var e error
	if a.controlRoot != nil {
		e = a.controlRoot.closeErr()
	}
	if a.control != "" {
		if re := os.RemoveAll(a.control); re != nil {
			e = re
		}
	}
	return e
}

func artifactHeadShape(b []byte) (head, branch string, ok bool) {
	if len(b) == 41 && b[40] == '\n' && gitHash.Match(b[:40]) {
		return string(b[:40]), "", true
	}
	ref, found := bytes.CutPrefix(b, []byte("ref: "))
	if !found || len(ref) < 2 || ref[len(ref)-1] != '\n' {
		return "", "", false
	}
	branch = string(ref[:len(ref)-1])
	if !strings.HasPrefix(branch, "refs/heads/") || !validPath(branch) || strictProtectedPath(branch) || strings.ContainsAny(branch, "\r\n") {
		return "", "", false
	}
	return "", branch, true
}

func (v *SelectedSourceView) artifactSnapshot(ctx context.Context) (sourceSnapshot, error) {
	a := v.artifact
	snap := sourceSnapshot{raw: map[string][]byte{}}
	add := func(role, path string) (SourceFact, error) {
		fact, data, e := v.meta(ctx, role, path)
		if e != nil {
			return fact, e
		}
		snap.Facts = append(snap.Facts, fact)
		if fact.State == "present_regular" {
			snap.raw[fact.ObjectDigest] = append([]byte(nil), data...)
		}
		return fact, nil
	}
	// Redirection/layout inputs must be absent before Git sees the repository.
	for _, name := range []string{"commondir", "gitdir", "config.worktree", "objects/info/alternates", "objects/info/http-alternates"} {
		f, e := add("git_state", filepath.Join(v.gitdir, filepath.FromSlash(name)))
		if e != nil {
			return snap, e
		}
		if f.State != "observed_absent" {
			v.checkPhase = "artifact_redirection"
			return snap, sourceProblem()
		}
	}
	v.checkPhase = "artifact_head_shape"
	headFact, e := add("git_state", filepath.Join(v.gitdir, "HEAD"))
	if e != nil || headFact.State != "present_regular" {
		return snap, sourceProblem()
	}
	fileHead, fileBranch, ok := artifactHeadShape(snap.raw[headFact.ObjectDigest])
	if !ok {
		return snap, sourceProblem()
	}
	configPath := filepath.Join(v.gitdir, "config")
	cfgFact, e := add("repo_config", configPath)
	if e != nil || cfgFact.State != "present_regular" {
		v.checkPhase = "artifact_config_missing"
		return snap, sourceProblem()
	}
	v.checkPhase = "config_file_list"
	raw, e := v.dispatch(ctx, a.cwd, readgit.MaxOutput, "config", "list", "--file", configPath, "--no-includes", "--null", "--show-origin", "--show-scope")
	if e != nil {
		return snap, sourceProblem()
	}
	v.checkPhase = "config_framing"
	rows, e := parseConfig(raw)
	if e != nil {
		return snap, e
	}
	v.checkPhase = "config_profile"
	if validateArtifactConfig(rows, configPath) != nil {
		return snap, sourceProblem()
	}
	snap.Config = append([]byte(nil), raw...)
	replay, e := add("replayed_global_exclude", a.replayPath)
	if e != nil || replay.State != "present_regular" || replay.ObjectDigest != a.replayID {
		v.checkPhase = "replayed_global_exclude"
		return snap, sourceProblem()
	}
	if _, e = add("local_exclude", filepath.Join(v.gitdir, "info", "exclude")); e != nil {
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
	}
	if snap.Branch != fileBranch || fileBranch == "" && snap.Head != fileHead {
		v.checkPhase = "artifact_head_mismatch"
		return snap, sourceProblem()
	}
	for _, name := range []string{"index", "packed-refs"} {
		if _, e = add("git_state", filepath.Join(v.gitdir, name)); e != nil {
			return snap, e
		}
	}
	if snap.Branch != "" {
		if _, e = add("git_state", filepath.Join(v.gitdir, filepath.FromSlash(snap.Branch))); e != nil {
			return snap, e
		}
	}
	sortSourceFacts(snap.Facts)
	return snap, nil
}

// SourceInvocationIdentity returns the identity of the invocation that served
// this view's source queries (the artifact profile's own, not the caller's).
func (v *SelectedSourceView) SourceInvocationIdentity() string {
	if v == nil {
		return ""
	}
	return v.invocation.Identity()
}

// validateArtifactSource is the separate closed artifact receipt domain. Counters
// are derived from the artifact query plan: open = version + explicit-file config +
// HEAD/symbolic-ref + three layout checks; each later snapshot = three queries.
func validateArtifactSource(selected SelectedReceipt, r SourceViewReceipt, pass uint8) error {
	timing, queries := "discovered_after_selection", 18
	if pass == 2 {
		timing, queries = "observed_before_selection", 29
	}
	if r.Authority != "unknown" || r.State != artifactReceiptState || r.ChainTiming != timing || r.Pass != pass || r.CandidatePaths == nil || len(r.CandidatePaths) != 0 || r.Facts == nil || len(r.Facts) > 1024 || r.BeginOffsetNS != selected.BeginOffsetNS || r.EndOffsetNS < selected.EndOffsetNS || !receiptSpan(r.BeginOffsetNS, r.EndOffsetNS) {
		return pairProblem()
	}
	for _, d := range []string{r.InvocationIdentity, r.ExecutableDigest, r.ExecutableNode, r.ConfigObject, r.StateObject, r.SourceDigest} {
		if !objectDigest(d) {
			return pairProblem()
		}
	}
	if r.SelectedInventoryDigest != selected.SelectedInventoryDigest || r.StageInventoryDigest != selected.StageInventoryDigest || r.TreeDigest != selected.TreeDigest {
		return pairProblem()
	}
	left := receiptStringLimit
	facts := map[string]SourceFact{}
	counts := map[string]int{}
	root, replay := "", ""
	var baseBytes, chainBytes int64
	chainCount := 0
	for i, f := range r.Facts {
		if !stringBudget(&left, f.Role, f.Path, f.State, f.ObjectDigest, f.NodeIdentity, f.AbsenceIdentity) || !safeRolePath(f.Path) || i > 0 && (r.Facts[i-1].Role > f.Role || r.Facts[i-1].Role == f.Role && r.Facts[i-1].Path >= f.Path) {
			return pairProblem()
		}
		switch f.Role {
		case "repo_config", "replayed_global_exclude", "local_exclude", "git_state", "selected_chain_gitignore":
		default:
			return pairProblem()
		}
		switch f.State {
		case "present_regular":
			if f.Bytes < 0 || f.Bytes > 1<<20 || !objectDigest(f.ObjectDigest) || !objectDigest(f.NodeIdentity) || f.AbsenceIdentity != "" {
				return pairProblem()
			}
		case "observed_absent":
			if f.Bytes != 0 || f.ObjectDigest != "" || f.NodeIdentity != "" || !objectDigest(f.AbsenceIdentity) {
				return pairProblem()
			}
		default:
			return pairProblem()
		}
		facts[f.Role+"\x00"+f.Path] = f
		counts[f.Role]++
		if f.Role == "selected_chain_gitignore" {
			chainCount++
			chainBytes += f.Bytes
		} else {
			baseBytes += f.Bytes
		}
		switch f.Role {
		case "repo_config":
			if !strings.HasSuffix(f.Path, "/.git/config") || f.State != "present_regular" {
				return pairProblem()
			}
			root = filepath.Dir(filepath.Dir(f.Path))
		case "replayed_global_exclude":
			if f.State != "present_regular" {
				return pairProblem()
			}
			replay = f.Path
		}
	}
	if r.SourceDigest != sourceFactsDigestFor(SourceViewArtifactVersion, r.Facts) {
		return pairProblem()
	}
	if counts["repo_config"] != 1 || counts["replayed_global_exclude"] != 1 || counts["local_exclude"] != 1 || counts["git_state"] < 8 || counts["git_state"] > 9 || root == "" || replay == "" || replay == root || strings.HasPrefix(replay, root+"/") {
		return pairProblem()
	}
	for _, f := range r.Facts {
		if f.Role != "replayed_global_exclude" && !strings.HasPrefix(f.Path, root+"/") {
			return pairProblem()
		}
	}
	if _, ok := facts["local_exclude\x00"+filepath.Join(root, ".git/info/exclude")]; !ok {
		return pairProblem()
	}
	required := map[string]bool{}
	absent := map[string]bool{}
	for _, p := range []string{"HEAD", "index", "packed-refs", "config.worktree", "objects/info/alternates", "objects/info/http-alternates", "commondir", "gitdir"} {
		required[filepath.Join(root, ".git", filepath.FromSlash(p))] = true
	}
	for _, p := range []string{"config.worktree", "objects/info/alternates", "objects/info/http-alternates", "commondir", "gitdir"} {
		absent[filepath.Join(root, ".git", filepath.FromSlash(p))] = true
	}
	optional := 0
	for _, f := range r.Facts {
		if f.Role != "git_state" {
			continue
		}
		if !required[f.Path] {
			rel, e := filepath.Rel(filepath.Join(root, ".git"), f.Path)
			if e != nil || !strings.HasPrefix(filepath.ToSlash(rel), "refs/heads/") || !validPath(filepath.ToSlash(rel)) || strictProtectedPath(filepath.ToSlash(rel)) {
				return pairProblem()
			}
			optional++
			if optional > 1 {
				return pairProblem()
			}
		} else {
			delete(required, f.Path)
		}
		if absent[f.Path] && f.State != "observed_absent" || f.Path == filepath.Join(root, ".git/HEAD") && f.State != "present_regular" {
			return pairProblem()
		}
	}
	if len(required) != 0 {
		return pairProblem()
	}
	paths := make([]string, len(selected.Selected))
	for i, f := range selected.Selected {
		paths[i] = f.Path
	}
	chain, e := selectedIgnoreChain(root, paths, receiptStringLimit)
	if e != nil || len(chain) != chainCount {
		return pairProblem()
	}
	for _, p := range chain {
		if _, ok := facts["selected_chain_gitignore\x00"+p]; !ok {
			return pairProblem()
		}
	}
	baseCount := len(r.Facts) - chainCount
	reads := 4*baseCount + 2*chainCount
	readBytes := 4*baseBytes + 2*chainBytes
	if pass == 2 {
		reads = 3 * len(r.Facts)
		readBytes = 3 * (baseBytes + chainBytes)
	}
	if r.MetadataReads != reads || r.MetadataReads > 1024 || r.MetadataBytes != readBytes || r.MetadataBytes > sourceMetadataCap {
		return pairProblem()
	}
	// Two frozen invocations (artifact source + Native guard) each inspect once;
	// their dispatches share ONE aggregate SourceDispatchLimit budget.
	if r.SourceQueryAttempts != queries || r.TotalDispatchAttempts < queries || r.TotalDispatchAttempts > readgit.SourceDispatchLimit || r.PersistenceGuardAttempts != r.TotalDispatchAttempts-queries || r.ExecutableReads != r.TotalDispatchAttempts+2 || r.ExecutableReadBytes < 1 || r.ExecutableReadBytes%int64(r.ExecutableReads) != 0 || r.ExecutableReadBytes/int64(r.ExecutableReads) > 64<<20 || r.SourceStdoutBytes < 1 || r.SourceStdoutBytes > sourceQueryCap || r.SourceStderrBytes != 0 || r.GuardStderrBytes != 0 || r.GuardStdoutBytes < 0 || r.GuardStdoutBytes > int64(r.PersistenceGuardAttempts)*readgit.MaxOutput {
		return pairProblem()
	}
	puts := selectedPutCount(selected, r)
	if pass == 1 && (r.PersistenceGuardAttempts < 2*puts || r.PersistenceGuardAttempts > 4*puts) {
		return pairProblem()
	}
	return nil
}

func validateArtifactObjects(x SourceViewReceipt, state sourceStateObject, objects map[string][]byte) error {
	configPath := ""
	for _, f := range x.Facts {
		if f.Role == "repo_config" {
			configPath = f.Path
		}
	}
	gitdir := filepath.Dir(configPath)
	optional := []string{}
	for _, f := range x.Facts {
		if f.Role == "git_state" {
			rel, e := filepath.Rel(gitdir, f.Path)
			if e != nil {
				return codecProblem()
			}
			if strings.HasPrefix(filepath.ToSlash(rel), "refs/heads/") {
				optional = append(optional, filepath.ToSlash(rel))
			}
		}
	}
	if state.Branch == "" {
		if len(optional) != 0 {
			return codecProblem()
		}
	} else if !strings.HasPrefix(state.Branch, "refs/heads/") || !validPath(state.Branch) || strictProtectedPath(state.Branch) || len(optional) != 1 || optional[0] != state.Branch {
		return codecProblem()
	}
	rows, e := parseConfig(objects[x.ConfigObject])
	if e != nil || configPath == "" || validateArtifactConfig(rows, configPath) != nil {
		return codecProblem()
	}
	return nil
}

// SourceConfigRow is one row of the SAME config-list framing (parseConfig).
type SourceConfigRow struct {
	Scope, Origin, Key, Value string
	HasValue                  bool
}

// ParseSourceConfig exposes the existing framing for retained ConfigObject bytes.
// It evaluates nothing; callers own any closed key classification.
func ParseSourceConfig(raw []byte) ([]SourceConfigRow, error) {
	rows, e := parseConfig(raw)
	if e != nil {
		return nil, e
	}
	out := make([]SourceConfigRow, len(rows))
	for i, r := range rows {
		out[i] = SourceConfigRow(r)
	}
	return out, nil
}
