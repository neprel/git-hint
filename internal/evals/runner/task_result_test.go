package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/neprel/git-hint/internal/evals/trees"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTaskResult(t *testing.T) *TaskResultArtifact {
	t.Helper()
	raw, e := os.ReadFile(fakeTree(t))
	if e != nil {
		t.Fatal(e)
	}
	z, s, f := 0, int64(2), int64(4)
	return &TaskResultArtifact{occurrence: &PrelaunchArtifact{}, artifact: &FrozenIOArtifact{FrozenIOArtifactIdentity{referenceTARDomain, referenceDigest(raw), int64(len(raw))}, raw}, identity: TaskResultIdentity{Capture: PrelaunchTreeCapture{State: "complete", CheckState: "complete", ExecutionDigest: referenceDigest([]byte("execution")), CheckFinishedMs: 1}, Ledger: LedgerReport{AgentRun: &ReportAgentRun{Outcome: AgentExited, ExecutionDigest: referenceDigest([]byte("execution")), ExitCode: &z, EnclosureStartMs: &s, EnclosureEndMs: &f}}}}
}
func TestTaskResultDelivery(t *testing.T) {
	for _, mode := range []string{"complete", "panic", "panic_after_identity", "error", "cancel", "missing_agent"} {
		t.Run(mode, func(t *testing.T) {
			a := testTaskResult(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "missing_agent" {
				a.identity.Ledger.AgentRun = nil
			}
			e := deliverTaskResult(ctx, a, func(ctx context.Context, v *TaskResultArtifact) error {
				if v.MatchesPrelaunch(&PrelaunchArtifact{}) {
					t.Fatal("foreign token")
				}
				if v.Ready() {
					t.Fatal("pending usable")
				}
				if _, e := v.FrozenArtifact(); e == nil {
					t.Fatal("pending artifact")
				}
				switch mode {
				case "panic":
					panic("private panic body")
				case "panic_after_identity":
					_ = v.Identity()
					panic("private")
				case "error":
					return errors.New("raw private")
				case "cancel":
					cancel()
				}
				return nil
			})
			if (e == nil) != (mode == "complete") || a.Ready() != (mode == "complete") {
				t.Fatal(e, a.Ready())
			}
			if e != nil && strings.Contains(e.Error(), "private") {
				t.Fatal("raw error leak")
			}
			if _, e = json.Marshal(a); e == nil {
				t.Fatal("serialized")
			}
			if mode == "complete" {
				art, e := a.FrozenArtifact()
				if e != nil {
					t.Fatal(e)
				}
				art.data[0] ^= 1
				if !a.Ready() {
					t.Fatal("copy alias")
				}
				id := a.Identity()
				*id.Ledger.AgentRun.ExitCode = 99
				if *a.Identity().Ledger.AgentRun.ExitCode != 0 {
					t.Fatal("identity alias")
				}
				dir := t.TempDir()
				if _, e = a.Materialize(ctx, dir); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestTaskResultSameFreezeBytes(t *testing.T) {
	raw := completeTestTree(t, tar.FormatUSTAR, "actual.go")
	var buf bytes.Buffer
	p := filepath.Join(t.TempDir(), "actual.tar")
	d, e := freezeTreeEmission(bytes.NewReader(raw), p, 96<<20, 96<<20, &buf)
	if e != nil {
		t.Fatal(e)
	}
	stored, e := os.ReadFile(p)
	if e != nil || !bytes.Equal(stored, buf.Bytes()) || referenceDigest(stored) != d {
		t.Fatal(e)
	}
	legacy := filepath.Join(t.TempDir(), "legacy.tar")
	ld, e := freezeTree(bytes.NewReader(raw), legacy)
	lb, _ := os.ReadFile(legacy)
	if e != nil || ld != d || !bytes.Equal(lb, stored) {
		t.Fatal("legacy emission changed", e)
	}
}
func TestTaskResultInventoryBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, e := MatchSelectedInventory(ctx, trees.SelectedReceipt{}, PrelaunchInventory{}, false); e == nil {
		t.Fatal("cancel accepted")
	}
	inv := PrelaunchInventory{Entries: []PrelaunchEntry{{Path: strings.Repeat("a/", 9000) + "b", Type: tar.TypeReg, Mode: 0644}}}
	if _, _, _, e := MatchSelectedInventory(context.Background(), trees.SelectedReceipt{}, inv, false); e == nil {
		t.Fatal("unbounded path accepted")
	}
}

func TestTaskResultInventoryBudgets(t *testing.T) {
	inv := PrelaunchInventory{Entries: []PrelaunchEntry{{Path: "a/b/c", Type: tar.TypeReg, Mode: 0644}, {Path: "a/b/d", Type: tar.TypeReg, Mode: 0644}}}
	for _, b := range []inventoryRelationBounds{{1, 1000, 1000, 100}, {100, 3, 1000, 100}, {100, 1000, 3, 100}, {100, 1000, 1000, 1}} {
		if _, e := boundedInventoryAliasesWithBounds(context.Background(), trees.SelectedReceipt{}, inv, b); e == nil {
			t.Fatal("budget ignored", b)
		}
	}
}
