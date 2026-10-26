package project

// LOCAL factual Native-to-artifact selection-policy relation. It reads only the
// loaded SourceRef closure, the artifact receipt pair and the full terminal TAR
// inventory. It is not original, completion, issuer or generic-population authority.
import (
	"archive/tar"
	"errors"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
)

const nativeArtifactRelationVersion = "native-artifact-selection-relation-v1"

// ErrSelectedResultRelation means the retained Native closure does not prove the
// artifact selection policy. The full TAR stays factual; composition is unavailable.
var ErrSelectedResultRelation = errors.New("selected_result_policy_relation_unproved")

// Explicit LIMIT: retained chains cover only Native-selected directories. Paths in
// any other directory are unproved, whatever the terminal archive contains.
const nativeArtifactLimitation = "native_chain_covers_selected_directories_only"

type NativeArtifactKey struct{ Key, Native, Artifact string }
type NativeArtifactChain struct{ Directory, State, ObjectDigest string }
type NativeArtifactRelation struct {
	Version, State, Authority, Limitation         string
	NativeSource                                  CaptureSourceRef
	NativeInvocation, ArtifactInvocation          string
	ExecutableDigest                              string
	NativeSourceDigest, ArtifactSourceDigest      string
	ReplayState, ReplayDigest                     string
	LocalExcludeState, LocalExcludeDigest         string
	Compared                                      []NativeArtifactKey
	Chain                                         []NativeArtifactChain
	InventoryEntries, ProtectedGitMetadataEntries int
}

// Closed Native key classes. Selection keys must be equal in the artifact; the
// global exclude is consumed only through its retained fact. Inert keys are not
// read by the fixed ls-files selection. Every other Native key refuses.
var nativeSelectionKeys = []string{"core.ignorecase", "core.precomposeunicode"}
var nativeInertKeys = map[string]bool{"core.repositoryformatversion": true, "core.filemode": true, "core.bare": true, "core.logallrefupdates": true, "core.symlinks": true, "user.name": true, "user.email": true, "init.defaultbranch": true, "credential.helper": true}

func relationProblem() error { return ErrSelectedResultRelation }

// nativeReplay copies the retained effective global_exclude fact. Absent maps to
// an owned empty input; that is recorded as a replay, never as new Native absence.
func nativeReplay(source *CaptureSourceInputs) (*trees.ArtifactSourceOptions, error) {
	var found []trees.SourceFact
	for _, f := range source.native.Facts {
		if f.Role == "global_exclude" {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		return nil, relationProblem()
	}
	switch f := found[0]; f.State {
	case "present_regular":
		b, ok := source.Object(f.ObjectDigest)
		if !ok || library.Digest(b) != f.ObjectDigest || int64(len(b)) != f.Bytes {
			return nil, relationProblem()
		}
		return &trees.ArtifactSourceOptions{ReplayState: trees.ArtifactReplayNativePresent, ReplayDigest: f.ObjectDigest, Replay: b}, nil
	case "observed_absent":
		return &trees.ArtifactSourceOptions{ReplayState: trees.ArtifactReplayNativeAbsent, ReplayDigest: library.Digest(nil), Replay: []byte{}}, nil
	}
	return nil, relationProblem()
}

func effectiveConfig(rows []trees.SourceConfigRow, key string) (string, bool) {
	value := "unset"
	for _, r := range rows {
		if strings.EqualFold(r.Key, key) {
			if !r.HasValue || r.Value != "true" && r.Value != "false" {
				return "", false
			}
			value = r.Value
		}
	}
	return value, true
}

func nativeArtifactRelation(source *CaptureSourceInputs, replay *trees.ArtifactSourceOptions, artifact trees.SourceViewReceipt, objects map[string][]byte, inv runner.PrelaunchInventory) (NativeArtifactRelation, error) {
	var zero NativeArtifactRelation
	native := source.native
	if native.Version != trees.SourceViewVersion || artifact.Version != trees.SourceViewArtifactVersion || replay == nil {
		return zero, relationProblem()
	}
	// The artifact Git must be the SAME captured executable bytes that parsed and
	// selected for the retained Native closure. Invocation identities differ by
	// profile; the version string is not a substitute. Same host and environment
	// are assumed; this is digest equality, not a binary attestation.
	if native.ExecutableDigest == "" || native.ExecutableDigest != artifact.ExecutableDigest {
		return zero, relationProblem()
	}
	nativeConfig, ok := source.Object(native.ConfigObject)
	if !ok {
		return zero, relationProblem()
	}
	nativeRows, e := trees.ParseSourceConfig(nativeConfig)
	if e != nil {
		return zero, relationProblem()
	}
	artifactRows, e := trees.ParseSourceConfig(objects[artifact.ConfigObject])
	if e != nil {
		return zero, relationProblem()
	}
	// R1: closed Native classification; no generic ignore-unknown rule.
	for _, r := range nativeRows {
		lower := strings.ToLower(r.Key)
		switch {
		case r.Scope == "command" && (lower == "core.fsmonitor" || lower == "core.hookspath"):
		case lower == "core.excludesfile", lower == "core.ignorecase", lower == "core.precomposeunicode":
		case nativeInertKeys[lower]:
		default:
			return zero, relationProblem()
		}
	}
	rel := NativeArtifactRelation{Version: nativeArtifactRelationVersion, State: "native_policy_correspondence_observed", Authority: "unknown", Limitation: nativeArtifactLimitation, NativeSource: source.Ref(), NativeInvocation: native.InvocationIdentity, ArtifactInvocation: artifact.InvocationIdentity, ExecutableDigest: native.ExecutableDigest, NativeSourceDigest: native.SourceDigest, ArtifactSourceDigest: artifact.SourceDigest, ReplayState: replay.ReplayState, ReplayDigest: replay.ReplayDigest, Compared: []NativeArtifactKey{}, Chain: []NativeArtifactChain{}, InventoryEntries: len(inv.Entries)}
	for _, key := range nativeSelectionKeys {
		n, ok1 := effectiveConfig(nativeRows, key)
		a, ok2 := effectiveConfig(artifactRows, key)
		if !ok1 || !ok2 || n != a {
			return zero, relationProblem()
		}
		rel.Compared = append(rel.Compared, NativeArtifactKey{key, n, a})
	}
	nativeRoot, artifactRoot := "", ""
	nativeChain := map[string]trees.SourceFact{}
	var nativeLocal, artifactLocal *trees.SourceFact
	for i, f := range native.Facts {
		switch f.Role {
		case "repo_config":
			nativeRoot = filepath.Dir(filepath.Dir(f.Path))
		case "local_exclude":
			nativeLocal = &native.Facts[i]
		}
	}
	for i, f := range artifact.Facts {
		switch f.Role {
		case "repo_config":
			artifactRoot = filepath.Dir(filepath.Dir(f.Path))
		case "local_exclude":
			artifactLocal = &artifact.Facts[i]
		case "replayed_global_exclude":
			// R2: the artifact Git consumed exactly the retained Native bytes.
			if f.State != "present_regular" || f.ObjectDigest != replay.ReplayDigest {
				return zero, relationProblem()
			}
		}
	}
	if nativeRoot == "" || artifactRoot == "" || nativeLocal == nil || artifactLocal == nil {
		return zero, relationProblem()
	}
	// R3: repository info/exclude bytes or absence correspond exactly.
	if nativeLocal.State != artifactLocal.State || nativeLocal.ObjectDigest != artifactLocal.ObjectDigest {
		return zero, relationProblem()
	}
	rel.LocalExcludeState, rel.LocalExcludeDigest = nativeLocal.State, nativeLocal.ObjectDigest
	relDir := func(root, p string) (string, bool) {
		r, e := filepath.Rel(root, p)
		if e != nil || filepath.Base(r) != ".gitignore" {
			return "", false
		}
		return filepath.ToSlash(filepath.Dir(r)), true
	}
	for _, f := range native.Facts {
		if f.Role == "selected_chain_gitignore" {
			d, ok := relDir(nativeRoot, f.Path)
			if !ok {
				return zero, relationProblem()
			}
			nativeChain[d] = f
		}
	}
	same := func(a, b trees.SourceFact) bool { return a.State == b.State && a.ObjectDigest == b.ObjectDigest }
	for _, f := range artifact.Facts {
		if f.Role == "selected_chain_gitignore" {
			d, ok := relDir(artifactRoot, f.Path)
			n, known := nativeChain[d]
			if !ok || !known || !same(n, f) {
				return zero, relationProblem()
			}
		}
	}
	// R4 path domain: EVERY full-TAR entry except the existing Git-metadata rule
	// needs a retained Native chain fact for each ancestor directory.
	entries := map[string]runner.PrelaunchEntry{}
	folded := map[string]string{}
	for _, n := range inv.Entries {
		entries[n.Path] = n
		folded[strings.ToLower(n.Path)] = n.Path
	}
	dirs := map[string]bool{}
	steps := 0
	for _, n := range inv.Entries {
		if n.Path == "" {
			continue
		}
		if trees.ProtectedGitMetadataPath(n.Path) {
			rel.ProtectedGitMetadataEntries++
			continue
		}
		for d := path.Dir(n.Path); ; d = path.Dir(d) {
			steps++
			if steps > 1000000 {
				return zero, relationProblem()
			}
			if dirs[d] {
				break
			}
			if _, ok := nativeChain[d]; !ok {
				return zero, relationProblem()
			}
			dirs[d] = true
			if d == "." {
				break
			}
		}
	}
	for d := range dirs {
		n := nativeChain[d]
		name := path.Join(d, ".gitignore")
		if actual, ok := folded[strings.ToLower(name)]; ok && actual != name {
			return zero, relationProblem()
		}
		e, present := entries[name]
		switch {
		case present && (e.Type != tar.TypeReg || e.Unresolved || n.State != "present_regular" || e.Digest != n.ObjectDigest || e.Size != n.Bytes):
			return zero, relationProblem()
		case !present && n.State != "observed_absent":
			return zero, relationProblem()
		}
		rel.Chain = append(rel.Chain, NativeArtifactChain{d, n.State, n.ObjectDigest})
	}
	sort.Slice(rel.Chain, func(i, j int) bool { return rel.Chain[i].Directory < rel.Chain[j].Directory })
	return rel, nil
}
