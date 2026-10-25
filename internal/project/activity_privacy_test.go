package project

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/version"
)

func literalPrivacyRef(key []byte, domain string, parts ...string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("hint:v0.1:" + domain))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
func privacySyntheticLedger(t *testing.T, f memFixture, x Execution) *activity.ModelLedger {
	t.Helper()
	counts := map[string]int{}
	cov := map[string]int{}
	comps := map[string]*int64{}
	for _, k := range activity.LedgerCounts {
		counts[k] = 0
	}
	for _, k := range activity.LedgerComponents {
		cov[k] = 0
		comps[k] = nil
	}
	zero := 0
	snap := func() *activity.LedgerCaptureSnapshot {
		return &activity.LedgerCaptureSnapshot{ImageID: "sha256:" + strings.Repeat("c", 64), HostRef: hint.DeriveID(f.s.Key(), "capture-host", "synthetic-host"), ContainerRef: hint.DeriveID(f.s.Key(), "capture-container", "synthetic-container")}
	}
	m := &activity.ModelLedger{LedgerRef: activity.LedgerRef(f.s.Key(), x.RunRef), Revision: 0, State: "incomplete", Reasons: []string{"no_requests"}, Counts: counts, Components: comps, ComponentCoverage: cov,
		ModelCheck: "not_observed", Bypass: "enforced", Scope: "isolated_container", SealedAt: memNow.Format("2006-01-02T15:04:05Z"),
		Binding:        activity.LedgerBinding{RunRef: x.RunRef, TaskID: x.TaskID, TaskFamilyID: activity.FamilyRef(f.s.Key(), "family"), ConfigurationDigest: f.cfg.Digest(), UnitRef: hint.EvalRef(f.s.Key(), "work-unit", memLabel), AttemptRef: x.AttemptRef, Origin: "project", ExecutionRef: x.ExecutionRef},
		AgentRun:       &activity.LedgerAgentRun{Outcome: runner.AgentExited, ExitCode: &zero, ExecutionDigest: memExec},
		BackendCapture: &activity.LedgerBackendCapture{Method: "runner_bracket_v1", Collector: activity.LedgerCollector{Name: "hint-runner-capture", Version: "0.1.0"}, Start: activity.LedgerCapture{Status: "complete", Snapshot: snap()}, End: activity.LedgerCapture{Status: "complete", Snapshot: snap()}}}
	if p := activity.LedgerProblems(*m); len(p) > 0 {
		t.Fatalf("synthetic ledger inconsistent:%v", p)
	}
	src := activity.LedgerSource(f.s.Key(), "opencode", f.cfg)
	if e := RegisterSource(f.s, src); e != nil {
		t.Fatal(e)
	}
	if e := AppendObservation(f.s, activity.LedgerObservation(f.s.Key(), src, *m)); e != nil {
		t.Fatal(e)
	}
	return m
}
func privacyActualEpoch(t *testing.T, f memFixture, n string) {
	t.Helper()
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-"+n, "run-"+n)
	if err := e.LaunchHook(f.s, x)(f.facts); err != nil {
		t.Fatal(err)
	}
	ledger := privacySyntheticLedger(t, f, x)
	if err := e.Finish(f.s, x, ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(f.s, memNow); err != nil {
		t.Fatal(err)
	}
}
func activityWireDoc(t *testing.T, p *Package) map[string]any {
	t.Helper()
	for _, d := range p.Docs {
		if d.Type == "activity_stream" {
			var m map[string]any
			if e := json.Unmarshal(d.Raw, &m); e != nil {
				t.Fatal(e)
			}
			return m
		}
	}
	t.Fatal("no activity document")
	return nil
}
func wireObservations(d map[string]any) []any {
	return d["payload"].(map[string]any)["observations"].([]any)
}

func TestActivityPrivacyBuildPackageTwoKeys(t *testing.T) {
	var seenRefs []string
	for _, name := range []string{"synthetic-key-one", "synthetic-key-two"} {
		t.Run(name, func(t *testing.T) {
			f := membershipFixture(t)
			privacyActualEpoch(t, f, "one")
			privacyActualEpoch(t, f, "two")
			before, e := os.ReadFile(f.s.Path("state", "activity", spoolFile))
			if e != nil {
				t.Fatal(e)
			}
			sourcesBefore, e := Sources(f.s)
			if e != nil {
				t.Fatal(e)
			}
			localBefore, e := LoadWork(f.s, memLabel)
			if e != nil {
				t.Fatal(e)
			}
			pkg, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
			if e != nil {
				t.Fatal(e)
			}
			if pkg.Version != version.ActivityPrivacyVersion {
				t.Fatal("wrong exact wire version")
			}
			after, _ := os.ReadFile(f.s.Path("state", "activity", spoolFile))
			sourcesAfter, _ := Sources(f.s)
			localAfter, _ := LoadWork(f.s, memLabel)
			if !bytes.Equal(before, after) || !reflect.DeepEqual(sourcesBefore, sourcesAfter) || !reflect.DeepEqual(localBefore, localAfter) {
				t.Fatal("projection mutated local spool/source/journal")
			}
			policy, _ := hint.Digest(f.policy)
			for _, d := range pkg.Docs {
				for _, raw := range []string{memExec, memLaunch, "sha256:" + strings.Repeat("c", 64), policy, runner.GatePolicyDigest(), memModel, memLabel, "synthetic-host", "synthetic-container"} {
					if bytes.Contains(d.Raw, []byte(raw)) {
						t.Fatal("private identity escaped normal package")
					}
				}
			}
			doc := activityWireDoc(t, pkg)
			wantB := literalPrivacyRef(f.s.Key(), "execution-provenance", memExec)
			wantA := literalPrivacyRef(f.s.Key(), "eval-report:observed-config", "setting_override", memLaunch)
			wantImage := literalPrivacyRef(f.s.Key(), "image", "sha256:"+strings.Repeat("c", 64))
			seenRefs = append(seenRefs, wantB)
			members := map[int]map[string]any{}
			for _, raw := range wireObservations(doc) {
				o := raw.(map[string]any)
				if l, ok := o["model_ledger"].(map[string]any); ok {
					if l["agent_run"].(map[string]any)["execution_provenance_ref"] != wantB || l["binding"].(map[string]any)["configuration_digest"] != f.cfg.Digest() {
						t.Fatal("ledger privacy/config binding")
					}
					for _, side := range []string{"start", "end"} {
						if l["backend_capture"].(map[string]any)[side].(map[string]any)["snapshot"].(map[string]any)["image_ref"] != wantImage {
							t.Fatal("image role")
						}
					}
				}
				if m, ok := o["membership"].(map[string]any); ok {
					es := m["events"].([]any)
					epoch := int(es[0].(map[string]any)["epoch"].(float64))
					members[epoch] = m
					var previous any
					for _, raw := range es {
						ev := raw.(map[string]any)
						if ev["prev_digest"] != previous {
							t.Fatal("projected event link")
						}
						previous, _ = hint.Digest(ev)
						if ev["kind"] == "launch" {
							if ev["execution_provenance_ref"] != wantB || ev["launch_configuration_ref"] != wantA || ev["execution_ref"] == wantB {
								t.Fatal("launch semantic roles/instance")
							}
						}
						if ev["kind"] == "epoch_open" && ev["membership_policy_ref"] != literalPrivacyRef(f.s.Key(), "membership-policy", policy) {
							t.Fatal("policy role")
						}
					}
					if m["head_digest"] != previous {
						t.Fatal("projected head")
					}
				}
			}
			if len(members) != 2 {
				t.Fatal("missing projected epochs")
			}
			prior, _ := hint.Digest(members[1])
			if members[2]["events"].([]any)[0].(map[string]any)["predecessor_epoch_digest"] != prior {
				t.Fatal("whole projected predecessor epoch")
			}
			// The current no-private-intent journal uses the exact validated identity mapping.
			first := members[1]["events"].([]any)[0].(map[string]any)
			if first["original_scope_digest"] != localBefore.Revisions[0]["scope_digest"] {
				t.Fatal("invented current journal rewrite")
			}
		})
	}
	if len(seenRefs) != 2 || seenRefs[0] == seenRefs[1] {
		t.Fatal("independent synthetic project keys did not separate provenance")
	}
}

func TestActivityPrivacyRefusesFutureOrUnknownJournalScope(t *testing.T) {
	f := membershipFixture(t)
	privacyActualEpoch(t, f, "one")
	original, _ := LoadWork(f.s, memLabel)
	for _, field := range []string{"verification_intent", "raw_path"} {
		t.Run(field, func(t *testing.T) {
			raw, _ := json.Marshal(original)
			var u WorkUnit
			json.Unmarshal(raw, &u)
			u.Scope[field] = map[string]any{"private": "synthetic-private-sentinel"}
			scope, _ := hint.Digest(u.Scope)
			var prev any
			for _, r := range u.Revisions {
				r["scope"] = u.Scope
				r["scope_digest"] = scope
				r["predecessor"] = prev
				delete(r, "revision_digest")
				d, _ := hint.Digest(r)
				r["revision_digest"] = d
				prev = d
			}
			if e := saveWork(f.s, &u); e != nil {
				t.Fatal(e)
			}
			if p, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow}); e == nil || p != nil {
				t.Fatal("unsupported private/unknown journal admitted")
			}
			if e := saveWork(f.s, original); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestActivityPrivacyRejectsWireMixes(t *testing.T) {
	f := membershipFixture(t)
	privacyActualEpoch(t, f, "one")
	p, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	original := activityWireDoc(t, p)
	for _, mutation := range []string{"raw_b", "wrong_ref", "old_adapter", "minor_fallback", "legacy_patch", "unsupported_minor", "orchestrator_correlation", "ledger_enclosure"} {
		t.Run(mutation, func(t *testing.T) {
			b, _ := json.Marshal(original)
			var d map[string]any
			json.Unmarshal(b, &d)
			var agent map[string]any
			for _, raw := range wireObservations(d) {
				if l, ok := raw.(map[string]any)["model_ledger"].(map[string]any); ok {
					agent = l["agent_run"].(map[string]any)
				}
			}
			switch mutation {
			case "raw_b":
				delete(agent, "execution_provenance_ref")
				agent["execution_digest"] = memExec
			case "wrong_ref":
				agent["execution_provenance_ref"] = memExec
			case "old_adapter":
				for _, raw := range d["sources"].([]any) {
					a := raw.(map[string]any)["adapter"].(map[string]any)
					if a["name"] == "hint-model-ledger" {
						a["version"] = "0.2.0"
					}
				}
			case "minor_fallback":
				d["hint_version"] = "0.2.0"
			case "legacy_patch":
				d["hint_version"] = "0.2.1"
			case "orchestrator_correlation":
				for _, o := range wireObservations(d) {
					m := o.(map[string]any)
					if m["membership"] != nil {
						m["correlation"] = map[string]any{"run_ref": strings.Repeat("a", 32)}
					}
				}
			case "ledger_enclosure":
				agent["exit_code"] = float64(17)
			case "unsupported_minor":
				d["hint_version"] = "0.4.0"
			}
			d["content_digest"], _ = hint.Digest(d["payload"])
			b, _ = json.Marshal(d)
			if Validate(b, nil) == nil {
				t.Fatal("mixed/unsupported wire admitted")
			}
		})
	}
}

func TestActivityPrivacyCopiesAndNoPartialResult(t *testing.T) {
	f := membershipFixture(t)
	privacyActualEpoch(t, f, "one")
	observations, _, e := ReadSpool(f.s)
	if e != nil {
		t.Fatal(e)
	}
	sources, e := Sources(f.s)
	if e != nil {
		t.Fatal(e)
	}
	before, _ := json.Marshal([]any{observations, sources})
	p, e := newActivityPrivacy(f.s, observations, sources, memNow)
	if e != nil {
		t.Fatal(e)
	}
	for id, source := range p.sources {
		if source.Configuration != nil {
			source.Configuration.ConfigRefs.Refs[0] = strings.Repeat("f", 32)
			p.sources[id] = source
		}
	}
	for _, o := range p.observations {
		if l, ok := o["model_ledger"].(map[string]any); ok {
			l["counts"].(map[string]any)["late"] = float64(99)
		}
	}
	after, _ := json.Marshal([]any{observations, sources})
	if !bytes.Equal(before, after) {
		t.Fatal("returned projection aliases private inputs")
	}
	duplicate := append(append([]activity.Observation{}, observations...), observations[0])
	if p, e := newActivityPrivacy(f.s, duplicate, sources, memNow); e == nil || p != nil {
		t.Fatal("duplicate produces partial/winning result")
	}
	sourcesBad := map[string]activity.Source{}
	for k, v := range sources {
		sourcesBad[k] = v
	}
	for k, v := range sourcesBad {
		v.Adapter.Version = "99.0.0"
		sourcesBad[k] = v
	}
	if p, e := newActivityPrivacy(f.s, observations, sourcesBad, memNow); e == nil || p != nil {
		t.Fatal("unknown local source produces partial result")
	}
}
