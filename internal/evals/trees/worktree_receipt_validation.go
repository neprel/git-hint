package trees

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/readgit"
)

// ResolveStrictCaptureLimits uses the sole strict normalizer. Zero values select
// its defaults, positive values tighten or clamp, and negative values refuse.
func ResolveStrictCaptureLimits(limits Limits) (Limits, error) { return strictEffectiveLimits(limits) }

// ValidateSelectedSourcePair checks bounded receipt consistency only. It performs
// no IO and establishes no capture origin, authority, clock origin, or raw-object
// validity. ConfigObject/StateObject/content/tree bytes need separate bounded
// object reads and a reviewed closed receipt/state/config codec before a producer.
// This consumer supports at most 8 MiB of raw strings per receipt; source capture
// itself has no new projection-byte cap. The existing per-role/read caps also apply.
func ValidateSelectedSourcePair(limits Limits, a, b SelectedReceipt, x, y SourceViewReceipt) error {
	lim, e := ResolveStrictCaptureLimits(limits)
	if e != nil {
		return pairProblem()
	}
	if validateSelected(lim, a, 1) != nil || validateSelected(lim, b, 2) != nil {
		return pairProblem()
	}
	if validateSource(a, x, 1) != nil || validateSource(b, y, 2) != nil {
		return pairProblem()
	}
	if a.RootIdentity != b.RootIdentity || a.TreeDigest != b.TreeDigest || a.SelectedInventoryDigest != b.SelectedInventoryDigest || a.StageInventoryDigest != b.StageInventoryDigest || !reflect.DeepEqual(a.Selected, b.Selected) || !reflect.DeepEqual(a.Stages, b.Stages) {
		return pairProblem()
	}
	if x.InvocationIdentity != y.InvocationIdentity || x.ExecutableDigest != y.ExecutableDigest || x.ExecutableNode != y.ExecutableNode || x.ConfigObject != y.ConfigObject || x.StateObject != y.StateObject || x.SourceDigest != y.SourceDigest || !reflect.DeepEqual(x.CandidatePaths, y.CandidatePaths) || !reflect.DeepEqual(x.Facts, y.Facts) {
		return pairProblem()
	}
	if x.EndOffsetNS > b.BeginOffsetNS || y.TotalDispatchAttempts <= x.TotalDispatchAttempts || y.PersistenceGuardAttempts <= x.PersistenceGuardAttempts || y.SourceStdoutBytes < x.SourceStdoutBytes || y.GuardStdoutBytes < x.GuardStdoutBytes {
		return pairProblem()
	}
	if x.ExecutableReadBytes/int64(x.ExecutableReads) != y.ExecutableReadBytes/int64(y.ExecutableReads) {
		return pairProblem()
	}
	puts := selectedPutCount(a, x)
	guards := y.PersistenceGuardAttempts - x.PersistenceGuardAttempts
	if guards < 2*puts || guards > 4*puts {
		return pairProblem()
	}
	return nil
}
func pairProblem() error {
	return unsupported("source_receipt_pair_invalid", "the selected-source receipt pair is inconsistent")
}
func objectDigest(s string) bool {
	return len(s) == 71 && strings.HasPrefix(s, "sha256:") && strings.Trim(s[7:], "0123456789abcdef") == ""
}
func receiptSpan(begin, end int64) bool {
	return begin >= 0 && end >= begin && end <= int64(strictCaptureSeconds)
}

const receiptStringLimit int64 = 8 << 20

func stringBudget(left *int64, values ...string) bool {
	for _, v := range values {
		*left -= int64(len(v))
		if *left < 0 {
			return false
		}
	}
	return true
}

func validateSelected(lim Limits, r SelectedReceipt, pass uint8) error {
	if r.Version != SelectedReceiptV1 || r.Pass != pass || r.EffectiveLimits != lim || r.Selected == nil || r.Stages == nil || len(r.Selected) > lim.MaxFiles || len(r.Stages) > 10000 || !receiptSpan(r.BeginOffsetNS, r.EndOffsetNS) {
		return pairProblem()
	}
	for _, d := range []string{r.RootIdentity, r.TreeDigest, r.SelectedInventoryDigest, r.StageInventoryDigest} {
		if !objectDigest(d) {
			return pairProblem()
		}
	}
	indexed := map[string]bool{}
	left := receiptStringLimit
	var stageBytes int64
	for i, s := range r.Stages {
		if !stringBudget(&left, s.Path, s.Mode, s.ObjectID) || !validPath(s.Path) || strictProtectedPath(s.Path) || !utf8.ValidString(s.Path) || s.Stage != 0 || (s.Mode != "100644" && s.Mode != "100755") || !gitHash.MatchString(s.ObjectID) || i > 0 && r.Stages[i-1].Path >= s.Path {
			return pairProblem()
		}
		raw := fmt.Sprintf("%s %s %d\t%s", s.Mode, s.ObjectID, s.Stage, s.Path)
		if !validStageRecord([]byte(raw)) {
			return pairProblem()
		}
		stageBytes += int64(len(raw)) + 1
		if stageBytes > strictInventoryBytes {
			return pairProblem()
		}
		indexed[s.Path] = true
	}
	var present, absent uint32
	var total, selectionBytes int64
	var files []File
	for i, f := range r.Selected {
		if !stringBudget(&left, f.Path, f.Mode, f.ObjectDigest, f.PathIdentity, f.AbsenceParentIdentity) || !validPath(f.Path) || strictProtectedPath(f.Path) || !utf8.ValidString(f.Path) || i > 0 && r.Selected[i-1].Path >= f.Path || f.Indexed != indexed[f.Path] {
			return pairProblem()
		}
		selectionBytes += int64(len(f.Path)) + 1
		if selectionBytes > strictInventoryBytes {
			return pairProblem()
		}
		delete(indexed, f.Path)
		switch f.State {
		case SelectedRegularPresent:
			if f.Mode != "100644" && f.Mode != "100755" || f.Size < 0 || f.Size > lim.MaxFileBytes || f.Size > lim.MaxTotalBytes-total || !objectDigest(f.ObjectDigest) || !objectDigest(f.PathIdentity) || f.AbsenceParentIdentity != "" {
				return pairProblem()
			}
			present++
			total += f.Size
			files = append(files, File{f.Path, f.Mode, f.Size, f.ObjectDigest})
		case SelectedAbsent:
			if !f.Indexed || f.Mode != "" || f.ObjectDigest != "" || f.PathIdentity != "" || f.Size != 0 || !objectDigest(f.AbsenceParentIdentity) {
				return pairProblem()
			}
			absent++
		default:
			return pairProblem()
		}
	}
	if len(indexed) != 0 || r.PresentCount != present || r.IndexedAbsentCount != absent || r.UnindexedAbsentCount != 0 {
		return pairProblem()
	}
	inv, stage := selectedReceiptDigests(r)
	if r.SelectedInventoryDigest != inv || r.StageInventoryDigest != stage {
		return pairProblem()
	}
	raw, e := treeManifestBytes(files)
	if e != nil || library.Digest(raw) != r.TreeDigest {
		return pairProblem()
	}
	return nil
}

func validateSource(selected SelectedReceipt, r SourceViewReceipt, pass uint8) error {
	timing := "discovered_after_selection"
	queries := 24
	if pass == 2 {
		timing = "observed_before_selection"
		queries = 38
	}
	if r.Version != SourceViewVersion || r.Authority != "unknown" || r.State != "observed_source_inputs" || r.ChainTiming != timing || r.Pass != pass || len(r.CandidatePaths) != 3 || r.Facts == nil || len(r.Facts) > 1024 || r.BeginOffsetNS != selected.BeginOffsetNS || r.EndOffsetNS < selected.EndOffsetNS || !receiptSpan(r.BeginOffsetNS, r.EndOffsetNS) {
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
	for _, p := range r.CandidatePaths {
		if !stringBudget(&left, p) || !safeRolePath(p) || len(p) > 16<<10 {
			return pairProblem()
		}
	}
	if len(r.CandidatePaths[1])+len(r.CandidatePaths[2])+2 > 16<<10 || filepath.Base(r.CandidatePaths[0]) != "gitconfig" || !strings.HasSuffix(r.CandidatePaths[1], "/git/config") || filepath.Base(r.CandidatePaths[2]) != ".gitconfig" {
		return pairProblem()
	}
	facts := map[string]SourceFact{}
	counts := map[string]int{}
	root := ""
	var baseBytes, chainBytes int64
	chainCount := 0
	for i, f := range r.Facts {
		if !stringBudget(&left, f.Role, f.Path, f.State, f.ObjectDigest, f.NodeIdentity, f.AbsenceIdentity) || !safeRolePath(f.Path) || i > 0 && (r.Facts[i-1].Role > f.Role || r.Facts[i-1].Role == f.Role && r.Facts[i-1].Path >= f.Path) {
			return pairProblem()
		}
		switch f.Role {
		case "system_config", "global_config", "repo_config", "global_exclude", "local_exclude", "git_state", "selected_chain_gitignore":
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
		key := f.Role + "\x00" + f.Path
		facts[key] = f
		counts[f.Role]++
		if f.Role == "selected_chain_gitignore" {
			chainCount++
			chainBytes += f.Bytes
		} else {
			baseBytes += f.Bytes
		}
		if f.Role == "repo_config" {
			if !strings.HasSuffix(f.Path, "/.git/config") {
				return pairProblem()
			}
			root = filepath.Dir(filepath.Dir(f.Path))
		}
	}
	if r.SourceDigest != sourceFactsDigest(r.Facts) {
		return pairProblem()
	}
	if counts["system_config"] != 1 || counts["global_config"] != 2 || counts["repo_config"] != 1 || counts["global_exclude"] != 1 || counts["local_exclude"] != 1 || counts["git_state"] < 6 || counts["git_state"] > 7 || root == "" {
		return pairProblem()
	}
	require := func(role, path string) bool { _, ok := facts[role+"\x00"+path]; return ok }
	if !require("system_config", r.CandidatePaths[0]) || !require("global_config", r.CandidatePaths[1]) || !require("global_config", r.CandidatePaths[2]) || !require("local_exclude", filepath.Join(root, ".git/info/exclude")) {
		return pairProblem()
	}
	required := map[string]bool{}
	for _, p := range []string{"HEAD", "index", "packed-refs", "config.worktree", "objects/info/alternates", "objects/info/http-alternates"} {
		required[filepath.Join(root, ".git", filepath.FromSlash(p))] = true
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
		if f.Path == filepath.Join(root, ".git/config.worktree") || f.Path == filepath.Join(root, ".git/objects/info/alternates") || f.Path == filepath.Join(root, ".git/objects/info/http-alternates") {
			if f.State != "observed_absent" {
				return pairProblem()
			}
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
		if !require("selected_chain_gitignore", p) {
			return pairProblem()
		}
	}
	baseCount := len(r.Facts) - chainCount
	reads := 4*baseCount + 2*chainCount
	bytes := 4*baseBytes + 2*chainBytes
	if pass == 2 {
		reads = 3 * len(r.Facts)
		bytes = 3 * (baseBytes + chainBytes)
	}
	if r.MetadataReads != reads || r.MetadataReads > 1024 || r.MetadataBytes != bytes || r.MetadataBytes > sourceMetadataCap {
		return pairProblem()
	}
	if r.SourceQueryAttempts != queries || r.TotalDispatchAttempts < queries || r.TotalDispatchAttempts > readgit.SourceDispatchLimit || r.PersistenceGuardAttempts != r.TotalDispatchAttempts-queries || r.ExecutableReads != r.TotalDispatchAttempts+1 || r.ExecutableReadBytes < 1 || r.ExecutableReadBytes%int64(r.ExecutableReads) != 0 || r.ExecutableReadBytes/int64(r.ExecutableReads) > 64<<20 || r.SourceStdoutBytes < 1 || r.SourceStdoutBytes > sourceQueryCap || r.SourceStderrBytes != 0 || r.GuardStderrBytes != 0 || r.GuardStdoutBytes < 0 || r.GuardStdoutBytes > int64(r.PersistenceGuardAttempts)*readgit.MaxOutput {
		return pairProblem()
	}
	puts := selectedPutCount(selected, r)
	if pass == 1 && (r.PersistenceGuardAttempts < 2*puts || r.PersistenceGuardAttempts > 4*puts) {
		return pairProblem()
	}
	return nil
}
func selectedPutCount(r SelectedReceipt, s SourceViewReceipt) int {
	raw := map[string]bool{}
	for _, f := range s.Facts {
		if f.State == "present_regular" {
			raw[f.ObjectDigest] = true
		}
	}
	return int(r.PresentCount) + len(raw) + 3 // selected tree, config query, state envelope
}
