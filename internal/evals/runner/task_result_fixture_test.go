//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Same private test catalogue substitution as existing runner tests. The public
// preparation/project/verification paths execute; no entry enters production.
func SelectedResultTestPolicies(t *testing.T) (ReferencePreparationPolicyRef, FrozenVerificationPolicyRef) {
	t.Helper()
	entry, _, _ := policyUnixFixture(t, "")
	entry.ID = "synthetic.terminal-prepare-v1"
	entry.Procedure.identity = entry.ID
	frozen, _, _ := policyUnixFixture(t, "")
	frozen.ID = "synthetic.terminal-verify-v1"
	frozen.Purpose = frozenPurpose
	frozen.Image = fakeImage
	frozen.Procedure = referencePreparationPolicy{identity: frozen.ID, operation: 300 * time.Second, resolve: 30 * time.Second, commit: 30 * time.Second, overhead: 10 * time.Second, cleanup: 30 * time.Second, archiveCap: 96 << 20, totalCap: 256 << 20}
	old := referencePreparationPolicies
	referencePreparationPolicies = []referencePolicyEntry{entry, frozen}
	t.Cleanup(func() { referencePreparationPolicies = old })
	return ReferencePreparationPolicyRef{entry.ID, referenceEntryDigest(entry)}, FrozenVerificationPolicyRef{frozen.ID, referenceEntryDigest(frozen)}
}

func TestTaskResultActualExportMutationRefuses(t *testing.T) {
	for _, name := range []string{"instruction.md", "environment/setup.sh", "task.toml", "tests/io/inputs/lower.in"} {
		t.Run(name, func(t *testing.T) {
			task, inputs := bridgeInputs(t, 300)
			ctx := context.Background()
			if !taskResultInputsMatch(ctx, task.Dir, inputs) {
				t.Fatal("actual export mismatch")
			}
			p := filepath.Join(task.Dir, filepath.FromSlash(name))
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			b = append(b, []byte("\n# changed\n")...)
			if e = os.WriteFile(p, b, 0600); e != nil {
				t.Fatal(e)
			}
			if taskResultInputsMatch(ctx, task.Dir, inputs) {
				t.Fatal("changed generated input admitted", name)
			}
		})
	}
}
func TestTaskResultFrozenPolicyReadOnlyAndPurpose(t *testing.T) {
	ref, _, count := bridgePolicy(t, "")
	before := count.accepted.Load()
	for _, bad := range []FrozenVerificationPolicyRef{{}, {PolicyID: ref.PolicyID, PolicyDigest: referenceDigest([]byte("other"))}} {
		if _, e := ResolveFrozenVerificationPolicy(bad); e == nil {
			t.Fatal("unknown policy")
		}
	}
	if count.accepted.Load() != before {
		t.Fatal("resolver dialed")
	}
	info, e := ResolveFrozenVerificationPolicy(ref)
	if e != nil {
		t.Fatal(e)
	}
	info.Sources[0].Digest = "changed"
	info.Bounds.Operation = 0
	next, e := ResolveFrozenVerificationPolicy(ref)
	if e != nil || next.Sources[0].Digest == "changed" || next.Bounds.Operation == 0 {
		t.Fatal("policy alias", e)
	}
	if count.accepted.Load() != before {
		t.Fatal("description dialed")
	}
}
