package project_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runjournal"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/project"
)

func TestWorkDefinitionActualUnrelatedControlledStartAndCorruptions(t *testing.T) {
	cases := []string{"valid", "legacy-empty-harness", "failed-member", "closed-failed-members", "shared-objective-runs", "filename", "manifest-run", "manifest-field", "missing-member", "unit-null", "unit-ref", "unit-scope", "revision-null", "revision-digest", "revision-predecessor", "revision-attempts", "reported-foreign", "missing-eligibility", "null-eligibility", "missing-trial-inventory", "missing-attempt-inventory", "extra-revision-field", "empty-revisions", "empty-member-digest", "missing-unit", "duplicate-unit", "duplicate-member", "unqualified-flag-with-unit", "stale-run-scope", "null-limit", "revision-origin"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s, l, request, now := project.DefinitionFixtureForExternalTest(t)
			m := workunits.Manifest{RunID: "jb-0000000000000042", Suite: "su-0000000000000042", Agent: "opencode", AgentVersion: "2.0.26", Arm: "baseline", Attempts: 2, RunnerVersion: runner.DockerRunnerSemver, Harness: "hint-docker", ModelLabel: "synthetic", LaunchDigest: "sha256:" + strings.Repeat("a", 64), MaxRequests: 8, MaxSeconds: 300, CreatedAt: now.Format(time.RFC3339), Members: []workunits.Member{{Eval: request.QualifiedIO.EvalName, EvalDigest: request.QualifiedIO.ManifestDigest, Baseline: request.QualifiedIO.EvalName, BaselineDigest: request.QualifiedIO.ManifestDigest, Objective: strings.Split(request.QualifiedIO.EvalName, "@")[0], TaskFamily: "feature", Eligible: true}}}
			if name == "legacy-empty-harness" {
				m.Harness = ""
			}
			if e := workunits.Start(s, l, m, now); e != nil {
				t.Fatal(e)
			}
			recordFailed := func(m workunits.Manifest, ordinal int) {
				mb := m.Members[0]
				input := suites.TrialInput{RunID: m.RunID, Suite: m.Suite, Eval: mb.Eval, Attempt: ordinal, Arm: m.Arm, Agent: m.Agent, AgentVersion: m.AgentVersion, Model: m.ModelLabel, RunnerVersion: m.RunnerVersion, RunnerName: "hint-docker", Started: now.Add(time.Duration(ordinal) * time.Second), MaxRequests: m.MaxRequests, MaxSeconds: m.MaxSeconds, ModelCheck: "not_observed", Objective: mb.Objective, Baseline: mb.Baseline, BaselineDigest: mb.BaselineDigest, EvalDigest: mb.EvalDigest, ObservedConfig: []map[string]string{{"kind": "setting_override", "digest": m.LaunchDigest}}}
				trial, e := suites.RecordTrial(l, input, runner.Trial{Name: fmt.Sprintf("synthetic-failed-%d", ordinal), Exception: "DockerDaemonError"})
				if e != nil {
					t.Fatal(e)
				}
				if e = workunits.Record(s, l, m.RunID, mb.Eval, trial); e != nil {
					t.Fatal(e)
				}
			}
			if name == "failed-member" || name == "closed-failed-members" || name == "shared-objective-runs" {
				count := 1
				if name == "closed-failed-members" {
					count = 2
				}
				for ordinal := 1; ordinal <= count; ordinal++ {
					recordFailed(m, ordinal)
				}
			}
			if name == "shared-objective-runs" {
				second := m
				second.RunID = "jb-0000000000000043"
				second.Suite = "su-0000000000000043"
				if e := workunits.Start(s, l, second, now); e != nil {
					t.Fatal(e)
				}
				recordFailed(second, 1)
			}

			path := s.Path("state", "evals", "work", m.RunID+".json")
			raw, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			parsed, e := runjournal.Parse(context.Background(), raw, m.RunID+".json", s.Key())
			if e != nil {
				t.Fatal("actual Start output refused", e)
			}
			if name == "valid" {
				cancelled, cancel := context.WithCancel(context.Background())
				cancel()
				if _, e = runjournal.Parse(cancelled, raw, m.RunID+".json", s.Key()); !errors.Is(e, runjournal.ErrStoredRun) {
					t.Fatal("cancelled parser accepted", e)
				}
				if _, e = runjournal.Parse(nil, raw, m.RunID+".json", s.Key()); !errors.Is(e, runjournal.ErrStoredRun) {
					t.Fatal("nil context accepted", e)
				}
			}
			if len(parsed.Units) != 1 {
				t.Fatal("actual producer omitted planned unit")
			}
			var doc map[string]any
			_ = json.Unmarshal(raw, &doc)
			manifest := doc["manifest"].(map[string]any)
			unit := doc["units"].([]any)[0].(map[string]any)
			rev := unit["revisions"].([]any)[0].(map[string]any)
			switch name {
			case "filename":
				if e = os.Rename(path, s.Path("state", "evals", "work", "jb-0000000000000043.json")); e != nil {
					t.Fatal(e)
				}
			case "manifest-run":
				manifest["run_id"] = "jb-0000000000000043"
			case "missing-unit":
				doc["units"] = []any{}
			case "duplicate-unit":
				doc["units"] = []any{unit, unit}
			case "duplicate-member":
				members := manifest["members"].([]any)
				manifest["members"] = append(members, members[0])
			case "unqualified-flag-with-unit":
				manifest["members"].([]any)[0].(map[string]any)["eligible"] = false
			case "stale-run-scope":
				unit["scope"].(map[string]any)["run_ref"] = strings.Repeat("0", 32)
			case "null-limit":
				manifest["max_seconds"] = nil
			case "revision-origin":
				rev["origin"] = "project"
			case "missing-eligibility":
				delete(manifest["members"].([]any)[0].(map[string]any), "eligible")
			case "null-eligibility":
				manifest["members"].([]any)[0].(map[string]any)["eligible"] = nil
			case "missing-trial-inventory":
				delete(unit, "trials")
			case "missing-attempt-inventory":
				delete(unit, "attempts")
			case "extra-revision-field":
				rev["unsupported"] = true
			case "empty-revisions":
				unit["revisions"] = []any{}
			case "empty-member-digest":
				manifest["members"].([]any)[0].(map[string]any)["eval_digest"] = ""
			case "manifest-field":
				manifest["unsupported_authority"] = true
			case "missing-member":
				manifest["members"] = []any{}
			case "unit-null":
				doc["units"] = []any{nil}
			case "unit-ref":
				unit["unit_ref"] = strings.Repeat("b", 32)
			case "unit-scope":
				unit["scope"].(map[string]any)["family_ref"] = strings.Repeat("c", 32)
			case "revision-null":
				unit["revisions"] = []any{nil}
			case "revision-digest":
				rev["revision_digest"] = "sha256:" + strings.Repeat("0", 64)
			case "revision-predecessor":
				rev["predecessor"] = "sha256:" + strings.Repeat("0", 64)
			case "revision-attempts":
				rev["attempts"] = []any{map[string]any{}}
			case "reported-foreign":
				unit["reported"] = []any{"sha256:" + strings.Repeat("0", 64)}
			}
			if name != "filename" {
				changed, _ := json.Marshal(doc)
				if e = os.WriteFile(path, changed, 0600); e != nil {
					t.Fatal(e)
				}
			}
			_, e = project.WriteWorkDefinitionInputs(context.Background(), s, request, now)
			if name == "valid" || name == "legacy-empty-harness" || name == "failed-member" || name == "closed-failed-members" || name == "shared-objective-runs" {
				if e != nil {
					t.Fatal("unrelated genuine controlled run blocked definition", e)
				}
				return
			}
			if !errors.Is(e, project.ErrWorkDefinition) {
				t.Fatal("corrupt controlled ownership admitted", e)
			}
			if _, e = os.Lstat(s.Path("state", library.WorkDefinitionStateDir)); !os.IsNotExist(e) {
				t.Fatal("definition reserved before history refusal", e)
			}
		})
	}
}
