package project

// LOCAL correspondence only. Raw native policy/gate/A/B digests must never enter
// Package documents or a public JSON sidecar. These rows confer no authority.
import (
	"errors"
	"reflect"
	"slices"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/snapshot"
)

const maxMembershipProjectionEvents = 16384
const maxMembershipProjectionNativeBytes = 16 << 20

// MembershipExecutionBinding joins an actual launch to its same-epoch enrollment.
// These are existing producer references, never newly derived identities.
type MembershipExecutionBinding struct {
	ExecutionRef, RunRef, AttemptRef, TaskID, PlannedConfigurationDigest string
	EnrollmentSequence                                                   int
}
type MembershipEventProjection struct {
	Sequence                                                         int
	Kind                                                             string
	NativeEventDigest, EventDigest                                   string
	NativePreviousDigest, PreviousDigest                             *string
	NativePolicyDigest, MembershipPolicyRef                          string
	NativeGatePolicyDigest, GatePolicyRef                            string
	NativeLaunchDigest, LaunchConfigurationRef                       string
	NativeExecutionDigest, ExecutionProvenanceRef                    string
	NativeInstalledTarget, InstalledTarget, NativeModelRef, ModelRef string
	NativeSourceConfigurationDigest, SourceConfigurationDigest       string
	NativeBinding, Binding                                           MembershipExecutionBinding
}

// MembershipProjection owns one native/final sealed epoch correspondence.
// Complete means correspondence only, never trusted origin, complete work or cost.
type MembershipProjection struct {
	State, Reason                                                                                            string
	NativeObservationID, NativeObservationDigest, NativeSourceID, NativeSourceDigest, NativeMembershipDigest string
	ObservationID, ObservationDigest, SourceID, SourceDigest, MembershipDigest                               string
	DocumentID, DocumentDigest, DocumentBytesSHA256                                                          string
	NativeUnitRef, UnitRef                                                                                   string
	Epoch                                                                                                    int
	NativeHeadDigest, HeadDigest                                                                             string
	NativePredecessorEpochDigest, PredecessorEpochDigest                                                     *string
	Events                                                                                                   []MembershipEventProjection
}

func (MembershipProjection) MarshalJSON() ([]byte, error) {
	return nil, errors.New("local membership correspondence has no JSON wire form")
}
func (MembershipEventProjection) MarshalJSON() ([]byte, error) {
	return nil, errors.New("local membership event correspondence has no JSON wire form")
}

// MembershipState reports only membership correspondence. State retains its
// existing ledger aggregate meaning, even for a membership-only receipt.
func (r *LocalProjectionReceipt) MembershipState() (string, string) {
	if r == nil || r.membershipState == "" {
		return "unavailable", "receipt_missing"
	}
	return r.membershipState, r.membershipReason
}

// Memberships returns deep copies of bounded LOCAL rows. No key, path or source
// content is exposed; native policy/gate/A/B values remain private metadata.
func (r *LocalProjectionReceipt) Memberships() []MembershipProjection {
	if r == nil {
		return nil
	}
	out := slices.Clone(r.memberships)
	cp := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	for i := range out {
		out[i].NativePredecessorEpochDigest = cp(out[i].NativePredecessorEpochDigest)
		out[i].PredecessorEpochDigest = cp(out[i].PredecessorEpochDigest)
		out[i].Events = slices.Clone(out[i].Events)
		for j := range out[i].Events {
			out[i].Events[j].NativePreviousDigest = cp(out[i].Events[j].NativePreviousDigest)
			out[i].Events[j].PreviousDigest = cp(out[i].Events[j].PreviousDigest)
		}
	}
	return out
}
func (c *projectionCollector) failMemberships(reason string) {
	c.membershipFailed = reason
	c.membershipRows = nil
	c.membershipBodies = nil
}
func membershipNumber(v any) int {
	f, ok := v.(float64)
	if !ok || f < 1 || f > 4096 || f != float64(int(f)) {
		return 0
	}
	return int(f)
}
func membershipNullableDigest(v any) (*string, bool) {
	if v == nil {
		return nil, true
	}
	s, ok := v.(string)
	if !ok || !digestPattern.MatchString(s) {
		return nil, false
	}
	return &s, true
}
func (c *projectionCollector) captureMembershipNative(o activity.Observation, src activity.Source, coarsened bool) {
	if c == nil || o.Membership == nil || c.membershipFailed != "" {
		return
	}
	if len(c.membershipRows) >= maxProjectionRows || len(o.Membership.Events) > 4096 || len(o.Membership.Events) > maxMembershipProjectionEvents-c.membershipEventCount {
		c.failMemberships("membership_receipt_limit")
		return
	}
	raw, e := hint.Canonical(o.Membership)
	if e != nil {
		c.failMemberships("native_membership_unavailable")
		return
	}
	if len(c.membershipRows) >= maxProjectionRows || len(raw) > maxMembershipProjectionNativeBytes-c.membershipNativeBytes || len(o.Membership.Events) > 4096 || len(o.Membership.Events) > maxMembershipProjectionEvents-c.membershipEventCount {
		c.failMemberships("membership_receipt_limit")
		return
	}
	if c.membershipBodies == nil {
		c.membershipBodies = map[string]map[string]any{}
	}
	if _, seen := c.membershipBodies[o.ObservationID]; seen {
		c.failMemberships("native_membership_ambiguous")
		return
	}
	body, e := snapshot.Plain(o.Membership)
	if e != nil {
		c.failMemberships("native_membership_unavailable")
		return
	}
	r := MembershipProjection{State: "incomplete", Reason: "fields_omitted_or_unavailable", NativeObservationID: o.ObservationID, NativeSourceID: o.SourceID, NativeUnitRef: o.Membership.UnitRef, NativeHeadDigest: o.Membership.HeadDigest}
	if coarsened {
		r.Reason = "coarsened_profile"
	}
	r.NativeObservationDigest, e = hint.Digest(o)
	if e != nil {
		c.failMemberships("native_membership_unavailable")
		return
	}
	r.NativeSourceDigest, e = hint.Digest(src)
	if e != nil {
		c.failMemberships("native_membership_unavailable")
		return
	}
	r.NativeMembershipDigest, e = hint.Digest(o.Membership)
	if e != nil {
		c.failMemberships("native_membership_unavailable")
		return
	}
	events, _ := body["events"].([]any)
	if len(events) > 0 {
		first, _ := events[0].(map[string]any)
		r.Epoch = membershipNumber(first["epoch"])
		var valid bool
		r.NativePredecessorEpochDigest, valid = membershipNullableDigest(first["predecessor_epoch_digest"])
		if _, present := first["predecessor_epoch_digest"]; !present || !valid {
			c.failMemberships("native_membership_unavailable")
			return
		}
	}
	c.membershipBodies[o.ObservationID] = body
	c.membershipRows = append(c.membershipRows, r)
	c.membershipNativeBytes += len(raw)
	c.membershipEventCount += len(events)
}
func (c *projectionCollector) bindMembershipFinal(o activity.Observation, final map[string]any) {
	if c == nil || o.Membership == nil || c.membershipFailed != "" {
		return
	}
	matches := 0
	for i := range c.membershipRows {
		if c.membershipRows[i].NativeObservationID == o.ObservationID {
			c.membershipRows[i].ObservationID = str(final["observation_id"])
			matches++
		}
	}
	if matches != 1 {
		c.failMemberships("native_membership_ambiguous")
	}
}
func membershipEnrollments(events []any) (map[string]MembershipExecutionBinding, bool) {
	out := map[string]MembershipExecutionBinding{}
	for _, v := range events {
		e, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if e["kind"] != "enroll" {
			continue
		}
		x := MembershipExecutionBinding{ExecutionRef: str(e["execution_ref"]), RunRef: str(e["run_ref"]), AttemptRef: str(e["attempt_ref"]), TaskID: str(e["task_id"]), PlannedConfigurationDigest: str(e["planned_configuration_digest"]), EnrollmentSequence: membershipNumber(e["seq"])}
		if !projectionRef.MatchString(x.ExecutionRef) || !projectionRef.MatchString(x.RunRef) || !projectionRef.MatchString(x.AttemptRef) || !projectionRef.MatchString(x.TaskID) || !digestPattern.MatchString(x.PlannedConfigurationDigest) || x.EnrollmentSequence == 0 {
			return nil, false
		}
		if _, exists := out[x.ExecutionRef]; exists {
			return nil, false
		}
		out[x.ExecutionRef] = x
	}
	return out, true
}

// Only compare preserved fields using the existing closed event table. Private
// identity rewrites and event/predecessor seals come from ACTUAL final bytes.
func membershipPreserved(native, final map[string]any, kind string) bool {
	fields, ok := eventFields[kind]
	if !ok {
		return false
	}
	for _, field := range fields {
		if _, present := native[field]; !present {
			return false
		}
		switch field {
		case "policy_digest", "gate_policy_digest", "launch_digest", "execution_digest":
			// Their actual final references are checked in the typed event extraction.
			continue
		}
		if _, present := final[field]; !present {
			return false
		}
		if field == "prev_digest" || field == "predecessor_epoch_digest" {
			continue
		}
		a, e := hint.Digest([]any{native[field]})
		if e != nil {
			return false
		}
		b, e := hint.Digest([]any{final[field]})
		if e != nil || a != b {
			return false
		}
	}
	return true
}
func (c *projectionCollector) sealMemberships(doc Doc, body map[string]any, documentSHA string) {
	if c.membershipFailed != "" {
		return
	}
	payload, _ := body["payload"].(map[string]any)
	observations, _ := payload["observations"].([]any)
	sources, _ := body["sources"].([]any)
	for i := range c.membershipRows {
		r := &c.membershipRows[i]
		r.DocumentID, r.DocumentDigest, r.DocumentBytesSHA256 = doc.ID, doc.Digest, documentSHA
		var found map[string]any
		matches := 0
		for _, v := range observations {
			m, ok := v.(map[string]any)
			if ok && r.ObservationID != "" && str(m["observation_id"]) == r.ObservationID {
				found = m
				matches++
			}
		}
		if matches != 1 {
			r.Reason = "final_membership_ambiguous_or_missing"
			continue
		}
		r.ObservationDigest, _ = hint.Digest(found)
		r.SourceID = str(found["source_id"])
		sourceMatches := 0
		for _, v := range sources {
			m, ok := v.(map[string]any)
			if ok && str(m["source_id"]) == r.SourceID {
				r.SourceDigest, _ = hint.Digest(m)
				sourceMatches++
			}
		}
		m, ok := found["membership"].(map[string]any)
		if !ok {
			continue
		}
		r.MembershipDigest, _ = hint.Digest(m)
		r.UnitRef = str(m["unit_ref"])
		r.HeadDigest = str(m["head_digest"])
		native := c.membershipBodies[r.NativeObservationID]
		ne, _ := native["events"].([]any)
		events, ok := m["events"].([]any)
		if !ok || len(events) == 0 || len(events) != len(ne) || body["hint_version"] != "0.3.0" || body["project_namespace"] != c.namespace || r.UnitRef != r.NativeUnitRef || sourceMatches != 1 || r.Epoch == 0 || r.ObservationDigest == "" || r.SourceDigest == "" {
			continue
		}
		nativeEnroll, ok := membershipEnrollments(ne)
		if !ok {
			continue
		}
		finalEnroll, ok := membershipEnrollments(events)
		if !ok {
			continue
		}
		complete := true
		r.Events = nil
		for j, v := range events {
			ev, ok := v.(map[string]any)
			original, nok := ne[j].(map[string]any)
			if !ok || !nok {
				complete = false
				break
			}
			kind := str(ev["kind"])
			if membershipNumber(ev["seq"]) != j+1 || membershipNumber(original["seq"]) != j+1 || original["kind"] != kind || !membershipPreserved(original, ev, kind) {
				complete = false
				break
			}
			x := MembershipEventProjection{Sequence: j + 1, Kind: kind}
			x.NativeEventDigest, _ = hint.Digest(original)
			x.EventDigest, _ = hint.Digest(ev)
			var valid bool
			x.NativePreviousDigest, valid = membershipNullableDigest(original["prev_digest"])
			if !valid {
				complete = false
				break
			}
			x.PreviousDigest, valid = membershipNullableDigest(ev["prev_digest"])
			if !valid {
				complete = false
				break
			}
			if j == 0 {
				if x.NativePreviousDigest != nil || x.PreviousDigest != nil || kind != "epoch_open" {
					complete = false
					break
				}
			} else if x.NativePreviousDigest == nil || x.PreviousDigest == nil || *x.NativePreviousDigest != r.Events[j-1].NativeEventDigest || *x.PreviousDigest != r.Events[j-1].EventDigest {
				complete = false
				break
			}
			if kind == "epoch_open" {
				x.NativePolicyDigest = str(original["policy_digest"])
				x.MembershipPolicyRef = str(ev["membership_policy_ref"])
				r.PredecessorEpochDigest, valid = membershipNullableDigest(ev["predecessor_epoch_digest"])
				if !valid || membershipNumber(ev["epoch"]) != r.Epoch || !digestPattern.MatchString(x.NativePolicyDigest) || !projectionRef.MatchString(x.MembershipPolicyRef) {
					complete = false
					break
				}
			}
			if kind == "enroll" || kind == "launch" {
				execution := str(ev["execution_ref"])
				x.NativeBinding = nativeEnroll[execution]
				x.Binding = finalEnroll[execution]
				if x.Binding.ExecutionRef == "" || !reflect.DeepEqual(x.NativeBinding, x.Binding) {
					complete = false
					break
				}
			}
			if kind == "launch" {
				x.NativeGatePolicyDigest = str(original["gate_policy_digest"])
				x.GatePolicyRef = str(ev["gate_policy_ref"])
				x.NativeLaunchDigest = str(original["launch_digest"])
				x.LaunchConfigurationRef = str(ev["launch_configuration_ref"])
				x.NativeExecutionDigest = str(original["execution_digest"])
				x.ExecutionProvenanceRef = str(ev["execution_provenance_ref"])
				x.NativeInstalledTarget = str(original["installed_target"])
				x.InstalledTarget = str(ev["installed_target"])
				x.NativeModelRef = str(original["model_ref"])
				x.ModelRef = str(ev["model_ref"])
				x.NativeSourceConfigurationDigest = str(original["source_configuration_digest"])
				x.SourceConfigurationDigest = str(ev["source_configuration_digest"])
				if x.SourceConfigurationDigest != x.Binding.PlannedConfigurationDigest || x.NativeSourceConfigurationDigest != x.NativeBinding.PlannedConfigurationDigest || !digestPattern.MatchString(x.NativeGatePolicyDigest) || !digestPattern.MatchString(x.NativeLaunchDigest) || !digestPattern.MatchString(x.NativeExecutionDigest) || !projectionRef.MatchString(x.GatePolicyRef) || !projectionRef.MatchString(x.LaunchConfigurationRef) || !projectionRef.MatchString(x.ExecutionProvenanceRef) {
					complete = false
					break
				}
			}
			r.Events = append(r.Events, x)
		}
		if complete && len(r.Events) == len(events) && r.Events[len(r.Events)-1].Kind == "epoch_seal" && r.HeadDigest == r.Events[len(r.Events)-1].EventDigest && r.NativeHeadDigest == r.Events[len(r.Events)-1].NativeEventDigest && r.Reason != "coarsened_profile" {
			r.State, r.Reason = "complete", ""
		}
	}
}
func (c *projectionCollector) finishMemberships() ([]MembershipProjection, string, string) {
	if c.failed != "" {
		return nil, "unavailable", c.failed
	}
	if c.membershipFailed != "" {
		return nil, "unavailable", c.membershipFailed
	}
	if !c.sawActivity {
		return nil, "unavailable", "activity_not_selected"
	}
	if len(c.membershipRows) == 0 {
		return nil, "unavailable", "no_membership_occurrences"
	}
	state := "complete"
	for _, r := range c.membershipRows {
		if r.State != "complete" {
			state = "incomplete"
		}
	}
	return c.membershipRows, state, ""
}
