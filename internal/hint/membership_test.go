package hint

import (
	"fmt"
	"testing"
)

// epoch builds a chained, sealed membership epoch with n launch-less refused executions.
func epoch(n int) map[string]any {
	d := func(s string) string { v, _ := Digest(s); return v }
	evs := []any{map[string]any{"seq": float64(1), "kind": "epoch_open", "epoch": float64(1), "original_scope_digest": d("s"),
		"enrollment_start_revision_digest": d("o"), "policy_digest": d("p"), "coverage": "hint_orchestrated_executions", "predecessor_epoch_digest": nil}}
	for i := 0; i < n; i++ {
		x := fmt.Sprintf("%032x", i)
		evs = append(evs, map[string]any{"kind": "enroll", "execution_ref": x, "run_ref": x, "attempt_ref": x, "task_id": x, "planned_configuration_digest": d("c")},
			map[string]any{"kind": "finish", "execution_ref": x, "finish_state": "refused_before_launch", "finish_audit_ref": x, "ledger_ref": nil})
	}
	evs = append(evs, map[string]any{"kind": "epoch_seal", "frozen_open_revision_digest": d("f"), "executions": float64(n)})
	var prev any
	for i, e := range evs {
		m := e.(map[string]any)
		m["seq"], m["prev_digest"] = float64(i+1), prev
		prev, _ = Digest(m)
	}
	return map[string]any{"unit_ref": fmt.Sprintf("%032x", 1), "events": evs, "head_digest": prev}
}

// The finite bounds: an epoch at the bounds is consistent; one execution more than the bound, or
// more events than the bound, is refused (never truncated).
func TestMembershipBounds(t *testing.T) {
	if MembershipProblem(epoch(3)) {
		t.Fatal("a valid epoch refused")
	}
	if MembershipProblem(epoch(1024)) {
		t.Fatal("an epoch at the execution bound refused")
	}
	if !MembershipProblem(epoch(1025)) {
		t.Fatal("an epoch over the execution bound accepted")
	}
	over := epoch(1024)
	evs := over["events"].([]any)
	if len(evs) > MembershipMaxEvents {
		t.Fatal("test setup")
	}
	for len(evs) <= MembershipMaxEvents { // pad with extra events past the bound
		evs = append(evs, evs[len(evs)-1])
	}
	over["events"] = evs
	if !MembershipProblem(over) {
		t.Fatal("an epoch over the event bound accepted")
	}
}
