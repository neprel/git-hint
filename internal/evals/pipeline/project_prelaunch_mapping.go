package pipeline

// Local factual correspondence only. No new source identifier, issuer, scanner,
// ordinary wire payload, original-start or completion authority is defined here.
import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

var ErrSelectedProjectMapping = errors.New("selected project mapping unavailable")

type TaskInputIdentity struct {
	Namespace, Eval, ManifestDigest, Suite, Run string
	Attempt                                     int
	Arm, BaseTree                               string
	Inputs                                      runner.FrozenIOInputsIdentity
}
type RetainedTaskInput struct {
	observed  *observedStartResult
	store     *store.Store // actual owner, private; getters/serialization expose no key/path
	identity  TaskInputIdentity
	base      []byte
	inventory runner.PrelaunchInventory
}

func (t *RetainedTaskInput) Identity() TaskInputIdentity {
	if t == nil {
		return TaskInputIdentity{}
	}
	raw, _ := json.Marshal(t.identity)
	var c TaskInputIdentity
	_ = json.Unmarshal(raw, &c)
	return c
}
func (*RetainedTaskInput) MarshalJSON() ([]byte, error) { return nil, ErrSelectedProjectMapping }

// Only the actual RunSuite bounded-export return enters this constructor. The
// exact immutable manifest digest is checked against the frozen suite member.
func retainActualTaskInput(ctx context.Context, s *store.Store, l *library.Library, ref, digest, suite, run, arm string, attempt int, rec map[string]any, task *runner.Task) (*RetainedTaskInput, error) {
	if ctx == nil || ctx.Err() != nil || s == nil || l == nil || task == nil || attempt < 1 || ref == "" || digest == "" || suite == "" || run == "" {
		return nil, ErrSelectedProjectMapping
	}
	raw, e := l.ResolveBoundedRead(ctx, "eval", ref, qualify.ResolveMetadataMax)
	if e != nil || library.Digest(raw) != digest {
		return nil, ErrSelectedProjectMapping
	}
	var actual map[string]any
	if json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(actual, rec) {
		return nil, ErrSelectedProjectMapping
	}
	frozen, e := task.FrozenIOInputs()
	if e != nil {
		return nil, e
	}
	b, e := frozen.BaseArchive()
	if e != nil {
		return nil, e
	}
	inv, e := frozen.BaseInventory()
	if e != nil {
		return nil, e
	}
	repos, ok := actual["repositories"].([]any)
	if !ok || len(repos) != 1 {
		return nil, ErrSelectedProjectMapping
	}
	repo, ok := repos[0].(map[string]any)
	if !ok {
		return nil, ErrSelectedProjectMapping
	}
	id := frozen.Identity()
	expected := map[string]string{"instruction": str(actual["instruction"]), "base_archive": str(repo["base_archive"]), "verifier": str(mappingMap(actual["verifier"])["archive"]), "reference": str(actual["reference"])}
	if setup := str(mappingMap(actual["environment"])["setup"]); setup != "" {
		expected["setup"] = setup
	}
	if !reflect.DeepEqual(id.SourceInputs, expected) {
		return nil, mappingProblem("actual_export_objects")
	}
	if id.SourceInputs["base_archive"] != str(repo["base_archive"]) || inv.Digest != str(repo["base_archive"]) || id.DeclaredImage != str(mappingMap(actual["environment"])["image"]) {
		return nil, ErrSelectedProjectMapping
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &RetainedTaskInput{store: s, identity: TaskInputIdentity{s.Cfg.ProjectNamespace, ref, digest, suite, run, attempt, arm, str(repo["base_tree"]), id}, base: b, inventory: inv}, nil
}

// obj is intentionally not a record parser; immutable manifests already went
// through the existing library/qualified loader. This helper safely inspects maps.
func mappingMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

type SelectedMappingIdentity struct {
	Population  string
	Baseline    project.BaselineBinding
	Source      project.CaptureSourceRef
	Definition  project.WorkDefinitionRef
	Task        TaskInputIdentity
	Capture     runner.PrelaunchTreeCapture
	BaseArchive string
	Compared    []runner.PrelaunchEntry
	Absent      []string
}
type SelectedProjectMapping struct {
	identity SelectedMappingIdentity
	unmapped runner.PrelaunchInventory
}

func (m *SelectedProjectMapping) Identity() SelectedMappingIdentity {
	if m == nil {
		return SelectedMappingIdentity{}
	}
	raw, _ := json.Marshal(m.identity)
	var c SelectedMappingIdentity
	_ = json.Unmarshal(raw, &c)
	return c
}
func (m *SelectedProjectMapping) UnmappedInventory() runner.PrelaunchInventory {
	if m == nil {
		return runner.PrelaunchInventory{}
	}
	return m.unmapped.Copy()
}
func (*SelectedProjectMapping) MarshalJSON() ([]byte, error) { return nil, ErrSelectedProjectMapping }
func mappingProblem(code string) error                       { return fmt.Errorf("%w: %s", ErrSelectedProjectMapping, code) }
func selectedBaselinePair(b *project.BaselineInputs) (*trees.DecodedSelectedSourcePair, error) {
	v := b.Identity()
	if len(v.Receipts) != 4 {
		return nil, mappingProblem("source_receipts")
	}
	raw := make([][]byte, 4)
	for i, o := range v.Receipts {
		var ok bool
		raw[i], ok = b.Object(o.Digest)
		if !ok || int64(len(raw[i])) != o.Bytes {
			return nil, mappingProblem("source_receipts")
		}
	}
	p, e := trees.DecodeSelectedSourcePair(v.Limits, trees.SelectedSourceWire{SelectedA: raw[0], SelectedB: raw[1], SourceA: raw[2], SourceB: raw[3]})
	if e != nil {
		return nil, e
	}
	objects := map[string][]byte{}
	for _, r := range p.ObjectRefs() {
		v, ok := b.Object(r.Digest)
		if !ok {
			return nil, mappingProblem("source_object")
		}
		objects[r.Digest] = v
	}
	if _, e = p.ValidateObjects(objects); e != nil {
		return nil, e
	}
	return p, nil
}
func inventoryNodes(inv runner.PrelaunchInventory) (map[string]runner.PrelaunchEntry, error) {
	if len(inv.Entries) > 200000 {
		return nil, mappingProblem("inventory_bound")
	}
	out := map[string]runner.PrelaunchEntry{}
	aliases := map[string]bool{}
	for _, e := range inv.Entries {
		if e.Path == "" && e.Type == tar.TypeDir && !e.Unresolved && e.Mode >= 0 && e.Mode&^0777 == 0 {
			if aliases[""] {
				return nil, mappingProblem("root_alias")
			}
			aliases[""] = true
			out[""] = e
			continue
		}
		if e.Path == "" || path.Clean(e.Path) != e.Path || strings.HasPrefix(e.Path, "/") || e.Path == ".." || strings.HasPrefix(e.Path, "../") || strings.ContainsAny(e.Path, "\\\x00") || e.Mode < 0 || e.Mode&^0777 != 0 {
			return nil, mappingProblem("path_or_mode")
		}
		key := runner.ArchivePathAlias(e.Path)
		if aliases[key] {
			return nil, mappingProblem("alias")
		}
		aliases[key] = true
		out[e.Path] = e
	}
	return out, nil
}
func logicalMode(v string) int64 {
	if v == "100644" {
		return 0100644
	}
	if v == "100755" {
		return 0100755
	}
	return 0
}
func sameSelected(f trees.SelectedPathFact, e runner.PrelaunchEntry) bool {
	return !e.Unresolved && e.Type == tar.TypeReg && e.LogicalMode == logicalMode(f.Mode) && e.Size == f.Size && e.Digest == f.ObjectDigest
}
func selectedInventoryFacts(selected trees.SelectedReceipt, inv runner.PrelaunchInventory, exact bool) ([]runner.PrelaunchEntry, []string, runner.PrelaunchInventory, error) {
	nodes, e := inventoryNodes(inv)
	if e != nil {
		return nil, nil, runner.PrelaunchInventory{}, e
	}
	expected := map[string]bool{}
	parents := map[string]bool{}
	parentAliases := map[string]string{}
	aliases := map[string]string{}
	compared := []runner.PrelaunchEntry{}
	absent := []string{}
	for _, f := range selected.Selected {
		aliases[runner.ArchivePathAlias(f.Path)] = f.Path
		expected[f.Path] = true
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			parents[d] = true
			parentAliases[runner.ArchivePathAlias(d)] = d
		}
	}
	for _, f := range selected.Selected {
		n, exists := nodes[f.Path]
		if f.State == trees.SelectedAbsent {
			if exists {
				return nil, nil, runner.PrelaunchInventory{}, mappingProblem("selected_absence")
			}
			absent = append(absent, f.Path)
		} else if !exists || !sameSelected(f, n) {
			return nil, nil, runner.PrelaunchInventory{}, mappingProblem("selected_content_mode_or_type")
		} else {
			compared = append(compared, n)
		}
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if n, ok := nodes[d]; ok && (n.Type != tar.TypeDir || n.Unresolved) {
				return nil, nil, runner.PrelaunchInventory{}, mappingProblem("ancestor_type")
			}
		}
	}
	unmapped := runner.PrelaunchInventory{Digest: inv.Digest, Entries: []runner.PrelaunchEntry{}}
	for _, n := range inv.Entries {
		if target, ok := parentAliases[runner.ArchivePathAlias(n.Path)]; ok && target != n.Path {
			return nil, nil, unmapped, mappingProblem("selected_ancestor_alias")
		}
		if target, ok := aliases[runner.ArchivePathAlias(n.Path)]; ok && target != n.Path {
			return nil, nil, unmapped, mappingProblem("selected_alias")
		}
		if expected[n.Path] {
			continue
		}
		// A link to/through a selected node cannot be treated as unrelated extra.
		if n.LinkTarget != "" {
			target := path.Clean(path.Join(path.Dir(n.Path), n.LinkTarget))
			if n.Type == tar.TypeLink {
				target = path.Clean(strings.TrimPrefix(n.LinkTarget, "app/"))
			}
			if strings.HasPrefix(n.LinkTarget, "/app/") {
				target = strings.TrimPrefix(n.LinkTarget, "/app/")
			}
			if n.LinkTarget == "/app" {
				target = "."
			}
			a := runner.ArchivePathAlias(target)
			if _, ok := aliases[a]; ok {
				return nil, nil, unmapped, mappingProblem("selected_link_alias")
			}
			for parent := range parents {
				if runner.ArchivePathAlias(parent) == a {
					return nil, nil, unmapped, mappingProblem("selected_link_alias")
				}
			}
			if target == "." {
				return nil, nil, unmapped, mappingProblem("selected_link_alias")
			}
		}

		if exact && !(n.Type == tar.TypeDir && !n.Unresolved && parents[n.Path]) {
			return nil, nil, unmapped, mappingProblem("unexpected_base_node")
		}
		unmapped.Entries = append(unmapped.Entries, n)
	}
	return compared, absent, unmapped, nil
}
func CheckSelectedProjectPrelaunch(ctx context.Context, b *project.BaselineInputs, source *project.CaptureSourceInputs, v *ObservedPrelaunch) (*SelectedProjectMapping, error) {
	if ctx == nil || ctx.Err() != nil || b == nil || source == nil || v == nil || v.result == nil || v.input == nil || v.input.store == nil || v.input.observed != v.result {
		return nil, mappingProblem("missing_actual_input")
	}
	input := v.input
	// Reuse the sole loaders at the ACTUAL export owner, not a caller store or map.
	baseline, e := project.LoadBaselineObservation(ctx, input.store, b.Binding().Observation)
	if e != nil || !reflect.DeepEqual(baseline.Identity(), b.Identity()) {
		return nil, mappingProblem("baseline_owner")
	}
	src, e := project.LoadCaptureSource(ctx, input.store, source.Ref())
	if e != nil || !reflect.DeepEqual(src.Identity(), source.Identity()) {
		return nil, mappingProblem("source_owner")
	}
	sid := src.Identity()
	binding := baseline.Binding()
	def := sid.Definition
	qi := def.QualifiedInputIdentity
	id := input.identity
	if sid.Kind != project.CaptureSourceV2 || def.Kind != project.WorkDefinitionInputsV2 || def.Baseline == nil || *def.Baseline != binding || id.Namespace != baseline.Identity().Namespace || v.result.repoID != binding.RepoID || id.Eval != qi.EvalName || id.ManifestDigest != qi.ManifestDigest || id.BaseTree != binding.Physical.TreeDigest || qi.NegativeControlTree != id.BaseTree {
		return nil, mappingProblem("manifest_definition_binding")
	}
	parts := id.Inputs.SourceInputs
	if parts["base_archive"] != qi.NegativeControlArchive || parts["verifier"] != qi.VerifierArchive || parts["reference"] != qi.ReferencePatchArchive || parts["setup"] != qi.SetupDigest || id.Inputs.SpecDigest != qi.SpecDigest || id.Inputs.InputsDigest != qi.InputsDigest || id.Inputs.DeclaredImage != qi.Image || qi.Workdir != "/app" {
		return nil, mappingProblem("qualified_input_binding")
	}
	if v.inventory.Digest != v.result.capture.Digest || v.result.capture.State != "complete" || v.result.capture.Clock != "ledger_anchor_monotonic_ms" || v.result.capture.ExecutionDigest == "" {
		return nil, mappingProblem("capture_binding")
	}
	pair, e := selectedBaselinePair(baseline)
	if e != nil {
		return nil, e
	}
	selected, _, _, _ := pair.Receipts()
	if _, _, _, e = selectedInventoryFacts(selected, input.inventory, true); e != nil {
		return nil, e
	}
	compared, absent, unmapped, e := selectedInventoryFacts(selected, v.inventory, false)
	if e != nil {
		return nil, e
	}
	// The existing scan/resolver is the only structural mapper; retain its full graph.
	files := map[string]struct{ path, hash string }{}
	for _, ent := range v.result.graph.Entities {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if ent.Kind == "file" {
			files[ent.ID] = struct{ path, hash string }{ent.Path, ent.Hash}
		}
	}
	for _, n := range compared {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		id, ok := v.result.resolve("/app/" + n.Path)
		f, found := files[id]
		if !ok || !found || f.path != n.Path || "sha256:"+f.hash != n.Digest {
			return nil, mappingProblem("selected_graph_content")
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &SelectedProjectMapping{SelectedMappingIdentity{"project_selected_prelaunch_v1", binding, src.Ref(), sid.DefinitionRef, input.Identity(), v.result.capture, input.inventory.Digest, compared, absent}, unmapped.Copy()}, nil
}
