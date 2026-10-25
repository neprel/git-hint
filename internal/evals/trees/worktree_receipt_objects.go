package trees

import (
	"bytes"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/readgit"
)

type SelectedSourceObjectRef struct {
	Digest   string
	MaxBytes int64
}
type SelectedSourceObject struct {
	Digest string
	Bytes  int64
}
type sourceObjectRequirement struct {
	Digest         string
	MaxBytes, Size int64
	Known          bool
}

const sourceObjectsMax = 22054
const sourceObjectBytesMax int64 = 128 << 20

// ObjectRefs is an exact copied read plan, not a filesystem capability. A minimum
// cap of one permits the existing bounded reader to inspect a known-empty object.
func (p *DecodedSelectedSourcePair) ObjectRefs() []SelectedSourceObjectRef {
	if p == nil {
		return nil
	}
	r := make([]SelectedSourceObjectRef, len(p.refs))
	for i, x := range p.refs {
		r[i] = SelectedSourceObjectRef{x.Digest, x.MaxBytes}
	}
	return r
}
func (p *DecodedSelectedSourcePair) objectRequirements() ([]sourceObjectRequirement, error) {
	refs := map[string]sourceObjectRequirement{}
	add := func(d string, cap, size int64, known bool) bool {
		if !objectDigest(d) || cap < 1 || cap > receiptWireMax || known && (size < 0 || size > cap) {
			return false
		}
		if old, ok := refs[d]; ok {
			if old.Known && known && old.Size != size {
				return false
			}
			if old.MaxBytes < cap {
				cap = old.MaxBytes
			}
			if old.Known {
				size = old.Size
				known = true
			}
		}
		if known && size > cap {
			return false
		}
		refs[d] = sourceObjectRequirement{d, cap, size, known}
		return len(refs) <= sourceObjectsMax
	}
	// The pair validator already requires matching pass projections. Role aliases
	// are intentional (for example selected .gitignore and source metadata).
	for _, f := range p.a.Selected {
		if f.State == SelectedRegularPresent {
			if !add(f.ObjectDigest, max(1, f.Size), f.Size, true) {
				return nil, codecProblem()
			}
		}
	}
	byPath := map[string]SourceFact{}
	for _, f := range p.x.Facts {
		if old, ok := byPath[f.Path]; ok {
			old.Role = f.Role
			if old != f {
				return nil, codecProblem()
			}
		}
		byPath[f.Path] = f
		if f.State == "present_regular" && !add(f.ObjectDigest, max(1, f.Bytes), f.Bytes, true) {
			return nil, codecProblem()
		}
	}
	raw, e := selectedTreeBytes(p.a)
	if e != nil || !add(p.a.TreeDigest, max(1, int64(len(raw))), int64(len(raw)), true) || !add(p.x.ConfigObject, readgit.MaxOutput, 0, false) || !add(p.x.StateObject, receiptWireMax, 0, false) {
		return nil, codecProblem()
	}
	out := make([]sourceObjectRequirement, 0, len(refs))
	for _, r := range refs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}
func selectedTreeBytes(r SelectedReceipt) ([]byte, error) {
	var files []File
	for _, f := range r.Selected {
		if f.State == SelectedRegularPresent {
			files = append(files, File{f.Path, f.Mode, f.Size, f.ObjectDigest})
		}
	}
	return treeManifestBytes(files)
}

// ValidateObjects performs no IO and retains no supplied byte slices. Its result
// is content/reference consistency only. It cannot establish path/node/absence
// authenticity, Git config evaluation, raw index/ref semantics, capture origin,
// private-clock origin, original start, authority, or durable handoff.
func (p *DecodedSelectedSourcePair) ValidateObjects(objects map[string][]byte) ([]SelectedSourceObject, error) {
	if p == nil || ValidateSelectedSourcePair(p.limits, p.a, p.b, p.x, p.y) != nil || len(objects) != len(p.refs) || len(objects) > sourceObjectsMax {
		return nil, codecProblem()
	}
	var total int64
	out := make([]SelectedSourceObject, 0, len(p.refs))
	for _, r := range p.refs {
		b, ok := objects[r.Digest]
		if !ok || int64(len(b)) > r.MaxBytes || r.Known && int64(len(b)) != r.Size || int64(len(b)) > sourceObjectBytesMax-total {
			return nil, codecProblem()
		}
		total += int64(len(b))
		if library.Digest(b) != r.Digest {
			return nil, codecProblem()
		}
		out = append(out, SelectedSourceObject{r.Digest, int64(len(b))})
	}
	tree, e := selectedTreeBytes(p.a)
	if e != nil || !bytes.Equal(tree, objects[p.a.TreeDigest]) {
		return nil, codecProblem()
	}
	var state sourceStateObject
	stateRaw := objects[p.x.StateObject]
	if decodeReceiptJSON(stateRaw, &state, false) != nil || !gitHash.MatchString(state.Head) || !reflect.DeepEqual(state.Facts, p.x.Facts) {
		return nil, codecProblem()
	}
	exact, e := sourceStateBytes(state.Head, state.Branch, state.Facts)
	if e != nil || !bytes.Equal(exact, stateRaw) {
		return nil, codecProblem()
	}
	profile := receiptConfigProfile(p.x)
	optional := []string{}
	for _, f := range p.x.Facts {
		if f.Role == "git_state" {
			rel, e := filepath.Rel(profile.gitdir, f.Path)
			if e != nil {
				return nil, codecProblem()
			}
			if strings.HasPrefix(filepath.ToSlash(rel), "refs/heads/") {
				optional = append(optional, filepath.ToSlash(rel))
			}
		}
	}
	if state.Branch == "" {
		if len(optional) != 0 {
			return nil, codecProblem()
		}
	} else if !strings.HasPrefix(state.Branch, "refs/heads/") || !validPath(state.Branch) || strictProtectedPath(state.Branch) || len(optional) != 1 || optional[0] != state.Branch {
		return nil, codecProblem()
	}
	rows, e := parseConfig(objects[p.x.ConfigObject])
	if e != nil || profile.validateConfig(rows) != nil {
		return nil, codecProblem()
	}
	facts := map[string]SourceFact{}
	for _, f := range p.x.Facts {
		facts[f.Role+"\x00"+f.Path] = f
	}
	for _, row := range rows {
		if row.Scope == "command" {
			continue
		}
		path, e := profile.originPath(row.Origin)
		if e != nil {
			return nil, codecProblem()
		}
		role := ""
		switch row.Scope {
		case "system":
			role = "system_config"
		case "global":
			role = "global_config"
		case "local":
			role = "repo_config"
		default:
			return nil, codecProblem()
		}
		f, ok := facts[role+"\x00"+path]
		if !ok || f.State != "present_regular" {
			return nil, codecProblem()
		}
	}
	return out, nil
}
func receiptConfigProfile(r SourceViewReceipt) sourceConfigProfile {
	for _, f := range r.Facts {
		if f.Role == "repo_config" {
			gitdir := filepath.Dir(f.Path)
			return sourceConfigProfile{filepath.Dir(gitdir), gitdir, r.CandidatePaths}
		}
	}
	return sourceConfigProfile{}
}
