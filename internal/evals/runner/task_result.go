package runner

// LOCAL factual custody only. No constructor accepts a caller path, digest or
// JSON. The result artifact is the FULL candidate TAR consumed by B1; the fixed
// privacy role selected-result-artifact never denotes the selected tree digest.
import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/neprel/git-hint/internal/evals/trees"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var ErrTaskResult = errors.New("task_result_unavailable")

type TaskResultIdentity struct {
	RunnerBundleDigest string
	Artifact           FrozenIOArtifactIdentity
	Inputs             FrozenIOInputsIdentity
	Preparation        ReferenceInputIdentity
	Capture            PrelaunchTreeCapture
	Ledger             LedgerReport
}
type TaskResultArtifact struct {
	occurrence *PrelaunchArtifact
	identity   TaskResultIdentity
	artifact   *FrozenIOArtifact
	delivery   atomic.Uint32 // pending, complete, refused; never deserialized
}

func (*TaskResultArtifact) MarshalJSON() ([]byte, error) { return nil, ErrTaskResult }
func (*TaskResultArtifact) UnmarshalJSON([]byte) error   { return ErrTaskResult }
func (a *TaskResultArtifact) Identity() TaskResultIdentity {
	if a == nil {
		return TaskResultIdentity{}
	}
	b, _ := json.Marshal(a.identity)
	var out TaskResultIdentity
	_ = json.Unmarshal(b, &out)
	return out
}
func (a *TaskResultArtifact) Ready() bool {
	return a != nil && a.delivery.Load() == 1 && a.artifact.valid()
}
func (a *TaskResultArtifact) MatchesPrelaunch(p *PrelaunchArtifact) bool {
	return a != nil && p != nil && a.occurrence == p && a.delivery.Load() != 2
}
func (a *TaskResultArtifact) FrozenArtifact() (*FrozenIOArtifact, error) {
	if !a.Ready() {
		return nil, ErrTaskResult
	}
	return &FrozenIOArtifact{a.artifact.identity, bytes.Clone(a.artifact.data)}, nil
}
func (a *TaskResultArtifact) Materialize(ctx context.Context, destination string) (PrelaunchInventory, error) {
	if ctx == nil || ctx.Err() != nil || !a.Ready() {
		return PrelaunchInventory{}, ErrTaskResult
	}
	return materializePrelaunch(ctx, bytes.NewReader(a.artifact.data), a.artifact.identity.Digest, destination)
}
func newTaskResult(data []byte, digest string, p *PrelaunchArtifact, in *FrozenIOInputs, l *LedgerReport, capture PrelaunchTreeCapture) *TaskResultArtifact {
	a := &TaskResultArtifact{occurrence: p, artifact: &FrozenIOArtifact{FrozenIOArtifactIdentity{referenceTARDomain, digest, int64(len(data))}, bytes.Clone(data)}}
	bundle, _, _, _ := referenceBundleIdentity()
	a.identity = TaskResultIdentity{RunnerBundleDigest: bundle, Artifact: a.artifact.identity, Inputs: in.Identity(), Preparation: in.PreparationIdentity(), Capture: capture, Ledger: *l}
	// Deep copy the ledger once; no Counter/report pointer survives the return.
	a.identity = a.Identity()
	return a
}
func taskResultInputsMatch(ctx context.Context, task string, in *FrozenIOInputs) bool {
	if !taskResultCurrent(ctx) || !in.valid() || in.actual.SpecDigest == "" {
		return false
	}
	instruction, ie := taskInstruction(task)
	if ie != nil || referenceDigest([]byte(instruction)) != in.sources["instruction"] {
		return false
	}
	id, e := InspectReferencePreparationInputs(ctx, task)
	b, ce := os.ReadFile(filepath.Join(task, "task.toml"))
	return e == nil && ce == nil && taskResultCurrent(ctx) && id == in.actual && referenceDigest(b) == in.configDigest
}
func taskResultCurrent(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	d, ok := ctx.Deadline()
	return !ok || time.Now().Before(d)
}
func deliverTaskResult(ctx context.Context, a *TaskResultArtifact, fn func(context.Context, *TaskResultArtifact) error) (err error) {
	if !taskResultCurrent(ctx) || a == nil || fn == nil || !a.artifact.valid() {
		return ErrTaskResult
	}
	returned := false
	defer func() {
		if recover() != nil || !returned {
			err = ErrTaskResult
		}
		if err != nil || !taskResultCurrent(ctx) {
			a.delivery.Store(2)
			err = ErrTaskResult
		} else {
			a.delivery.Store(1)
		}
	}()
	run := a.identity.Ledger.AgentRun
	if run == nil || run.Outcome != AgentExited || run.ExitCode == nil || *run.ExitCode != 0 || run.EnclosureStartMs == nil || run.EnclosureEndMs == nil || *run.EnclosureEndMs < *run.EnclosureStartMs || run.ExecutionDigest != a.identity.Capture.ExecutionDigest || a.identity.Capture.State != "complete" || a.identity.Capture.CheckState != "complete" || a.identity.Capture.CheckFinishedMs > *run.EnclosureStartMs {
		return ErrTaskResult
	}
	callbackError := fn(ctx, a)
	returned = true
	if callbackError != nil {
		return ErrTaskResult
	}
	return nil
}

func selectedInventoryProblem(code string) error { return fmt.Errorf("%w: %s", ErrTaskResult, code) }
func inventoryNodes(inv PrelaunchInventory, folded map[string]string) (map[string]PrelaunchEntry, error) {
	if len(inv.Entries) > 200000 {
		return nil, selectedInventoryProblem("inventory_bound")
	}
	out := map[string]PrelaunchEntry{}
	aliases := map[string]bool{}
	for _, e := range inv.Entries {
		if e.Path == "" && e.Type == tar.TypeDir && !e.Unresolved && e.Mode >= 0 && e.Mode&^0777 == 0 {
			if aliases[""] {
				return nil, selectedInventoryProblem("root_alias")
			}
			aliases[""] = true
			out[""] = e
			continue
		}
		if e.Path == "" || path.Clean(e.Path) != e.Path || strings.HasPrefix(e.Path, "/") || e.Path == ".." || strings.HasPrefix(e.Path, "../") || strings.ContainsAny(e.Path, "\\\x00") || e.Mode < 0 || e.Mode&^0777 != 0 {
			return nil, selectedInventoryProblem("path_or_mode")
		}
		key := folded[e.Path]
		if aliases[key] {
			return nil, selectedInventoryProblem("alias")
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
func sameSelected(f trees.SelectedPathFact, e PrelaunchEntry) bool {
	return !e.Unresolved && e.Type == tar.TypeReg && e.LogicalMode == logicalMode(f.Mode) && e.Size == f.Size && e.Digest == f.ObjectDigest
}
func MatchSelectedInventory(ctx context.Context, selected trees.SelectedReceipt, inv PrelaunchInventory, exact bool) ([]PrelaunchEntry, []string, PrelaunchInventory, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, PrelaunchInventory{}, ErrTaskResult
	}
	// Bound total ancestor traversal and retained index entries independently of cancellation.
	work := 0
	step := func() bool { work++; return work <= 1000000 && ctx.Err() == nil }
	var pathBytes int
	for _, n := range inv.Entries {
		if len(n.Path) > 16<<10 || len(n.LinkTarget) > 16<<10 {
			return nil, nil, PrelaunchInventory{}, ErrTaskResult
		}
		pathBytes += len(n.Path) + len(n.LinkTarget)
		if pathBytes > 32<<20 {
			return nil, nil, PrelaunchInventory{}, ErrTaskResult
		}
	}
	folded, e := boundedInventoryAliases(ctx, selected, inv)
	if e != nil {
		return nil, nil, PrelaunchInventory{}, e
	}
	nodes, e := inventoryNodes(inv, folded)
	if e != nil {
		return nil, nil, PrelaunchInventory{}, e
	}
	// Occupancy is a path fact, not a fabricated TAR entry. Materialization
	// creates required ancestor directories even when the archive omits their
	// headers. Preserve all original inventory metadata and unmapped entries.
	occupancy := map[string]map[string]bool{}
	for actual := range nodes {
		if !step() {
			return nil, nil, PrelaunchInventory{}, ErrTaskResult
		}
		for component := actual; component != "."; component = path.Dir(component) {
			if !step() {
				return nil, nil, PrelaunchInventory{}, ErrTaskResult
			}
			alias := folded[component]
			if occupancy[alias] == nil {
				occupancy[alias] = map[string]bool{}
			}
			occupancy[alias][component] = true
		}
	}
	expected := map[string]bool{}
	parents := map[string]bool{}
	parentAliases := map[string]string{}
	aliases := map[string]string{}
	compared := []PrelaunchEntry{}
	absent := []string{}
	for _, f := range selected.Selected {
		if !step() || len(f.Path) > 16<<10 {
			return nil, nil, PrelaunchInventory{}, ErrTaskResult
		}
		aliases[folded[f.Path]] = f.Path
		expected[f.Path] = true
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if !step() {
				return nil, nil, PrelaunchInventory{}, ErrTaskResult
			}
			parents[d] = true
			parentAliases[folded[d]] = d
		}
	}
	for _, f := range selected.Selected {
		if !step() || len(f.Path) > 16<<10 {
			return nil, nil, PrelaunchInventory{}, ErrTaskResult
		}
		n, exists := nodes[f.Path]
		if f.State == trees.SelectedAbsent {
			if len(occupancy[folded[f.Path]]) != 0 {
				return nil, nil, PrelaunchInventory{}, selectedInventoryProblem("selected_absence")
			}
			absent = append(absent, f.Path)
		} else if !exists || !sameSelected(f, n) {
			return nil, nil, PrelaunchInventory{}, selectedInventoryProblem("selected_content_mode_or_type")
		} else {
			compared = append(compared, n)
		}
		for component := f.Path; component != "."; component = path.Dir(component) {
			if !step() {
				return nil, nil, PrelaunchInventory{}, ErrTaskResult
			}
			for actual := range occupancy[folded[component]] {
				if actual != component {
					return nil, nil, PrelaunchInventory{}, selectedInventoryProblem("selected_component_alias")
				}
			}
		}
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if !step() {
				return nil, nil, PrelaunchInventory{}, ErrTaskResult
			}
			if n, ok := nodes[d]; ok && (n.Type != tar.TypeDir || n.Unresolved) {
				return nil, nil, PrelaunchInventory{}, selectedInventoryProblem("ancestor_type")
			}
		}
	}
	unmapped := PrelaunchInventory{Digest: inv.Digest, Entries: []PrelaunchEntry{}}
	for _, n := range inv.Entries {
		if !step() {
			return nil, nil, unmapped, ErrTaskResult
		}
		if target, ok := parentAliases[folded[n.Path]]; ok && target != n.Path {
			return nil, nil, unmapped, selectedInventoryProblem("selected_ancestor_alias")
		}
		if target, ok := aliases[folded[n.Path]]; ok && target != n.Path {
			return nil, nil, unmapped, selectedInventoryProblem("selected_alias")
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
			a := folded[target]
			if _, ok := aliases[a]; ok {
				return nil, nil, unmapped, selectedInventoryProblem("selected_link_alias")
			}
			if _, ok := parentAliases[a]; ok {
				return nil, nil, unmapped, selectedInventoryProblem("selected_link_alias")
			}
			if target == "." {
				return nil, nil, unmapped, selectedInventoryProblem("selected_link_alias")
			}
		}

		if exact && !(n.Type == tar.TypeDir && !n.Unresolved && parents[n.Path]) {
			return nil, nil, unmapped, selectedInventoryProblem("unexpected_base_node")
		}
		unmapped.Entries = append(unmapped.Entries, n)
	}
	return compared, absent, unmapped, nil
}

// Admission bounds the work BEFORE folding/index allocation. Repeated ancestor
// visits count even when their folded key is already cached. Limits narrow this
// factual relation only; the archive validator/full inventory remain unchanged.
type inventoryRelationBounds struct{ Steps, VisitedBytes, RetainedBytes, Keys int }

func inventoryRelationDefaultBounds() inventoryRelationBounds {
	return inventoryRelationBounds{1000000, 64 << 20, 32 << 20, 200000}
}

func boundedInventoryAliases(ctx context.Context, selected trees.SelectedReceipt, inv PrelaunchInventory) (map[string]string, error) {
	return boundedInventoryAliasesWithBounds(ctx, selected, inv, inventoryRelationDefaultBounds())
}
func boundedInventoryAliasesWithBounds(ctx context.Context, selected trees.SelectedReceipt, inv PrelaunchInventory, bounds inventoryRelationBounds) (map[string]string, error) {
	if len(inv.Entries) > 200000 || len(selected.Selected) > 10000 {
		return nil, ErrTaskResult
	}
	folded := map[string]string{}
	steps, visited, retained := 0, 0, 0
	add := func(v string) error {
		steps++
		visited += len(v)
		if ctx.Err() != nil || steps > bounds.Steps || visited > bounds.VisitedBytes || len(v) > 16<<10 {
			return ErrTaskResult
		}
		if _, ok := folded[v]; ok {
			return nil
		}
		// Unicode folding can expand input. Reserve a conservative 4x source-byte
		// bound before computing it, then charge actual key+value before insertion.
		if len(folded) >= bounds.Keys || retained+4*len(v) > bounds.RetainedBytes {
			return ErrTaskResult
		}
		alias := ArchivePathAlias(v)
		if retained+len(v)+len(alias) > bounds.RetainedBytes {
			return ErrTaskResult
		}
		retained += len(v) + len(alias)
		folded[v] = alias
		return nil
	}
	ancestors := func(v string) error {
		for c := v; c != "."; c = path.Dir(c) {
			if e := add(c); e != nil {
				return e
			}
		}
		return nil
	}
	for _, n := range inv.Entries {
		if n.Path != "" && (path.Clean(n.Path) != n.Path || strings.HasPrefix(n.Path, "/") || n.Path == ".." || strings.HasPrefix(n.Path, "../")) {
			return nil, ErrTaskResult
		}
		if len(n.LinkTarget) > 16<<10 {
			return nil, ErrTaskResult
		}
		if e := ancestors(n.Path); e != nil {
			return nil, e
		}
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
			if e := add(target); e != nil {
				return nil, e
			}
		}
	}
	for _, f := range selected.Selected {
		if e := ancestors(f.Path); e != nil {
			return nil, e
		}
	}
	return folded, nil
}
