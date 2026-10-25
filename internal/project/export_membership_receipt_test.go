package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
)

func TestMembershipReceiptTwoEpochsActualProjectionAndNoWire(t *testing.T) {
	refs := []string{}
	for _, keyName := range []string{"first", "second"} {
		t.Run(keyName, func(t *testing.T) {
			f := membershipFixture(t)
			privacyActualEpoch(t, f, "one")
			privacyActualEpoch(t, f, "two")
			opt := ExportOptions{All: true, Now: memNow}
			ordinary, e := BuildPackage(f.s, opt)
			if e != nil {
				t.Fatal(e)
			}
			p, r, e := BuildPackageWithReceipt(f.s, opt)
			if e != nil || !reflect.DeepEqual(ordinary, p) {
				t.Fatal("package bytes changed", e)
			}
			state, reason := r.MembershipState()
			if state != "complete" || reason != "" {
				t.Fatal("membership correspondence incomplete", state, reason)
			}
			rows := r.Memberships()
			if len(rows) != 2 {
				t.Fatal(len(rows))
			}
			native, _, _ := ReadSpoolChecked(f.s)
			srcs, _ := Sources(f.s)
			for _, row := range rows {
				if row.State != "complete" || row.Epoch < 1 || row.Epoch > 2 || row.NativeUnitRef != row.UnitRef || len(row.Events) != 5 {
					t.Fatal("wrong epoch", row.State, row.Reason, row.Epoch, len(row.Events))
				}
				var local activity.Observation
				for _, o := range native {
					if o.ObservationID == row.NativeObservationID {
						local = o
					}
				}
				if local.Membership == nil {
					t.Fatal("native missing")
				}
				nd, _ := hint.Digest(local)
				sd, _ := hint.Digest(srcs[local.SourceID])
				md, _ := hint.Digest(local.Membership)
				if nd != row.NativeObservationDigest || sd != row.NativeSourceDigest || md != row.NativeMembershipDigest || local.Membership.HeadDigest != row.NativeHeadDigest {
					t.Fatal("native binding")
				}
				foundDoc := false
				for _, doc := range p.Docs {
					if doc.ID != row.DocumentID {
						continue
					}
					foundDoc = true
					sum := sha256.Sum256(doc.Raw)
					if doc.Digest != row.DocumentDigest || "sha256:"+hex.EncodeToString(sum[:]) != row.DocumentBytesSHA256 {
						t.Fatal("document seal")
					}
					var wire map[string]any
					json.Unmarshal(doc.Raw, &wire)
					matched := false
					for _, v := range wire["payload"].(map[string]any)["observations"].([]any) {
						o := v.(map[string]any)
						if o["observation_id"] != row.ObservationID {
							continue
						}
						matched = true
						od, _ := hint.Digest(o)
						m := o["membership"].(map[string]any)
						md, _ := hint.Digest(m)
						if od != row.ObservationDigest || md != row.MembershipDigest || m["head_digest"] != row.HeadDigest {
							t.Fatal("final body seals")
						}
						events := m["events"].([]any)
						for i, event := range row.Events {
							ev := events[i].(map[string]any)
							ed, _ := hint.Digest(ev)
							nativeED, _ := hint.Digest(local.Membership.Events[i])
							if event.Sequence != i+1 || ev["kind"] != event.Kind || ed != event.EventDigest || nativeED != event.NativeEventDigest {
								t.Fatal("event correspondence")
							}
						}
					}
					if !matched {
						t.Fatal("final observation missing")
					}
					for _, bad := range []string{memExec, memLaunch, row.NativeMembershipDigest, row.NativeSourceDigest} {
						if bytes.Contains(doc.Raw, []byte(bad)) {
							t.Fatal("local metadata entered wire")
						}
					}
				}
				if !foundDoc {
					t.Fatal("document missing")
				}
				open, launch := row.Events[0], row.Events[2]
				if open.NativePolicyDigest == "" || open.MembershipPolicyRef != hint.DeriveID(f.s.Key(), "membership-policy", open.NativePolicyDigest) {
					t.Fatal("policy role")
				}
				if launch.NativeLaunchDigest != memLaunch || launch.LaunchConfigurationRef != activity.ConfigRef(f.s.Key(), "setting_override", memLaunch) || launch.NativeExecutionDigest != memExec || launch.ExecutionProvenanceRef != hint.DeriveID(f.s.Key(), "execution-provenance", memExec) || launch.GatePolicyRef != hint.DeriveID(f.s.Key(), "gate-policy", launch.NativeGatePolicyDigest) {
					t.Fatal("launch roles")
				}
				if launch.Binding != launch.NativeBinding || launch.Binding != row.Events[1].Binding || launch.InstalledTarget != "linux-arm64" || launch.ModelRef != f.cfg.Model.Ref {
					t.Fatal("enroll/launch binding")
				}
			}
			if rows[0].Epoch != 1 {
				rows[0], rows[1] = rows[1], rows[0]
			}
			if rows[0].PredecessorEpochDigest != nil || rows[1].PredecessorEpochDigest == nil || *rows[1].PredecessorEpochDigest != rows[0].MembershipDigest || rows[1].NativePredecessorEpochDigest == nil || *rows[1].NativePredecessorEpochDigest != rows[0].NativeMembershipDigest {
				t.Fatal("actual projected predecessor")
			}
			refs = append(refs, rows[0].Events[2].ExecutionProvenanceRef)
			if _, e = json.Marshal(r.Memberships()); e == nil {
				t.Fatal("local rows serialized")
			}
			rows[1].Events[1].PreviousDigest = new(string)
			*rows[1].Events[1].PreviousDigest = "changed"
			rows[1].Events[0].MembershipPolicyRef = "changed"
			*rows[1].PredecessorEpochDigest = "changed"
			for _, row := range r.Memberships() {
				if row.Epoch == 2 && (row.Events[0].MembershipPolicyRef == "changed" || *row.PredecessorEpochDigest == "changed" || *row.Events[1].PreviousDigest == "changed") {
					t.Fatal("mutable getter")
				}
			}
		})
	}
	if len(refs) != 2 || refs[0] == refs[1] {
		t.Fatal("two project keys shared identity")
	}
}
func TestMembershipReceiptOmissionCoarseAggregateAndLedgerState(t *testing.T) {
	f := membershipFixture(t)
	privacyActualEpoch(t, f, "one")
	fields, e := hint.RecordFieldsFor("0.3")
	if e != nil {
		t.Fatal(e)
	}
	keep := []string{}
	for _, field := range fields["Observation"] {
		if field != "membership" {
			keep = append(keep, field)
		}
	}
	f.s.Cfg.Policy.Fields = map[string][]string{"Observation": keep}
	if _, err := BuildPackage(f.s, ExportOptions{All: true, Now: memNow}); err == nil {
		t.Fatal("ordinary membership-required schema changed")
	}
	p, r, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e == nil || p != nil || r != nil {
		t.Fatal("mandatory membership omission fabricated a package or receipt")
	}
	var state string
	f.s.Cfg.Policy.Fields["Observation"] = nil
	keep = nil
	for _, field := range fields["Observation"] {
		if field != "model_ledger" {
			keep = append(keep, field)
		}
	}
	f.s.Cfg.Policy.Fields["Observation"] = keep
	f.s.Cfg.Policy.Profile = "coarse"
	f.s.Cfg.Policy.TimeResolutionSeconds = 60
	_, r, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	state, _ = r.MembershipState()
	if state != "incomplete" || r.Memberships()[0].Reason != "coarsened_profile" {
		t.Fatal("coarse success")
	}
	f.s.Cfg.Policy.Profile = "aggregate"
	f.s.Cfg.Policy.Fields = nil
	_, r, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	state, _ = r.MembershipState()
	if state != "unavailable" || r.Memberships() != nil {
		t.Fatal("aggregate success")
	}
	// Membership-only receipt leaves the existing ledger State unavailable, never redefines it.
	f = membershipFixture(t)
	eph := mustOpen(t, f)
	if _, e = eph.Seal(f.s, memNow); e != nil {
		t.Fatal(e)
	}
	_, r, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	state, reason := r.State()
	memberState, _ := r.MembershipState()
	if state != "unavailable" || reason != "no_ledger_occurrences" || memberState != "complete" {
		t.Fatal("ledger aggregate semantics changed", state, reason, memberState)
	}
}
func TestMembershipReceiptBoundsAmbiguityAndZero(t *testing.T) {
	var zero *LocalProjectionReceipt
	if state, _ := zero.MembershipState(); state != "unavailable" || zero.Memberships() != nil {
		t.Fatal("zero success")
	}
	f := membershipFixture(t)
	privacyActualEpoch(t, f, "one")
	obs, _, _ := ReadSpoolChecked(f.s)
	srcs, _ := Sources(f.s)
	var native activity.Observation
	for _, o := range obs {
		if o.Membership != nil {
			native = o
		}
	}
	c := &projectionCollector{membershipRows: make([]MembershipProjection, maxProjectionRows)}
	c.captureMembershipNative(native, srcs[native.SourceID], false)
	rows, state, _ := c.finishMemberships()
	if state != "unavailable" || rows != nil {
		t.Fatal("row cap partial success")
	}
	c = &projectionCollector{membershipNativeBytes: maxMembershipProjectionNativeBytes}
	c.captureMembershipNative(native, srcs[native.SourceID], false)
	if _, state, _ := c.finishMemberships(); state != "unavailable" {
		t.Fatal("byte cap")
	}
	c = &projectionCollector{membershipEventCount: maxMembershipProjectionEvents}
	c.captureMembershipNative(native, srcs[native.SourceID], false)
	if _, state, _ := c.finishMemberships(); state != "unavailable" {
		t.Fatal("event cap")
	}
	c = &projectionCollector{}
	c.captureMembershipNative(native, srcs[native.SourceID], false)
	c.captureMembershipNative(native, srcs[native.SourceID], false)
	if rows, state, _ := c.finishMemberships(); state != "unavailable" || rows != nil {
		t.Fatal("ambiguous native success")
	}
	// The same observed final document can be missing/ambiguous; no vacuous row succeeds.
	p, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	for _, mutation := range []string{"missing", "duplicate", "binding", "previous", "missing_predecessor", "missing_previous"} {
		c = &projectionCollector{namespace: f.s.Cfg.ProjectNamespace}
		c.captureMembershipNative(native, srcs[native.SourceID], false)
		c.bindMembershipFinal(native, map[string]any{"observation_id": native.ObservationID})
		for _, doc := range p.Docs {
			if doc.Type != "activity_stream" {
				continue
			}
			var body map[string]any
			json.Unmarshal(doc.Raw, &body)
			payload := body["payload"].(map[string]any)
			values := payload["observations"].([]any)
			changed := []any{}
			for _, v := range values {
				o := v.(map[string]any)
				if o["observation_id"] == native.ObservationID {
					switch mutation {
					case "missing":
						continue
					case "duplicate":
						changed = append(changed, o)
					case "binding":
						o["membership"].(map[string]any)["events"].([]any)[1].(map[string]any)["run_ref"] = strings.Repeat("0", 32)
					case "previous":
						o["membership"].(map[string]any)["events"].([]any)[1].(map[string]any)["prev_digest"] = "sha256:" + strings.Repeat("0", 64)
					case "missing_predecessor":
						delete(o["membership"].(map[string]any)["events"].([]any)[0].(map[string]any), "predecessor_epoch_digest")
					case "missing_previous":
						delete(o["membership"].(map[string]any)["events"].([]any)[0].(map[string]any), "prev_digest")
					}
				}
				changed = append(changed, o)
			}
			payload["observations"] = changed
			raw, _ := json.Marshal(body)
			doc.Raw = raw
			c.seal(doc)
		}
		if rows, state, _ := c.finishMemberships(); state != "incomplete" || len(rows) != 1 || rows[0].State == "complete" {
			t.Fatal("mutated final correspondence complete", mutation, state)
		}
	}
}
