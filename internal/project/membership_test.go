package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

const (
	memLabel = "unit-raw-label-7f3c"
	memModel = "raw-model-name-91ab"
)

var (
	memNow    = time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	memLaunch = "sha256:" + strings.Repeat("a", 64)
	memExec   = "sha256:" + strings.Repeat("b", 64)
)

type memFixture struct {
	s      *store.Store
	policy MembershipPolicy
	cfg    activity.Configuration
	facts  runner.LaunchFacts
}

// membershipFixture is a store with one collected repository and one open work unit, a
// configuration bound to one launch digest and one model, and its epoch policy.
func membershipFixture(t *testing.T) memFixture {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "main.go", "package main\n")
	testkit.Git(t, root, "add", "-A")
	testkit.Git(t, root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "state")
	s, _, err := store.Init(root, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(s, CollectOptions{Now: func() time.Time { return memNow }}); err != nil {
		t.Fatal(err)
	}
	key := s.Key()
	var cfg activity.Configuration
	cfg.Agent.Name, cfg.Agent.Version = "opencode", activity.VersionState{State: "known", Value: runner.DockerOpenCodeVersion}
	cfg.Model = activity.ModelState{State: "known", Ref: activity.ModelRef(key, memModel), Provenance: "launch_intent"}
	cfg.Harness.Kind, cfg.Harness.Version = "other", activity.VersionState{State: "known", Value: runner.DockerRunnerSemver}
	cfg.ContextPolicy = activity.ConfigPolicy{State: "disabled"}
	cfg.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{activity.ConfigRef(key, "setting_override", memLaunch)}}
	cfg.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 5}, MaxSeconds: activity.Limit{State: "known", Value: 60}}
	if _, err := WorkStart(s, memLabel, nil, "", "family", &cfg, memNow); err != nil {
		t.Fatal(err)
	}
	p := MembershipPolicy{Coverage: MembershipCoverage, Agent: "opencode", ConfigurationDigests: []string{cfg.Digest()}, LaunchDigests: []string{memLaunch},
		StudyLaunchPolicy: string(runner.StudyLaunchSingleRootV1), GatePolicyDigest: runner.GatePolicyDigest(), RunnerVersion: runner.DockerRunnerSemver,
		SchemaDigest: MembershipSchemaDigest()}
	f := runner.LaunchFacts{LaunchDigest: memLaunch, ExecutionDigest: memExec, GatePolicyDigest: runner.GatePolicyDigest(), Target: "linux-arm64", Model: memModel}
	return memFixture{s, p, cfg, f}
}

func mustOpen(t *testing.T, f memFixture) *Epoch {
	t.Helper()
	e, err := EpochOpen(f.s, memLabel, f.policy, memNow)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustEnroll(t *testing.T, f memFixture, e *Epoch, task, run string) Execution {
	t.Helper()
	x, err := e.Enroll(f.s, task, run, f.cfg, memNow)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func spoolBytes(t *testing.T, f memFixture, n int) []byte {
	t.Helper()
	b, err := os.ReadFile(f.s.Path(membershipPath(f.s, memLabel, epochName(n))...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func finishStates(t *testing.T, f memFixture, n int) map[string]string {
	t.Helper()
	v, err := readEpoch(f.s, memLabel, n)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for x, ev := range v.final {
		out[x] = evStr(ev, "finish_state")
	}
	return out
}

// activityDoc wraps observations and their sources in one activity document and returns the
// reference checker's findings (schema and semantics).
func activityDoc(t *testing.T, s *store.Store, sources []activity.Source, obs []activity.Observation) []string {
	t.Helper()
	payload := map[string]any{"observations": obs, "metrics": []any{}}
	pd, err := hint.Digest(payload)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"hint_version": "0.2.0", "document_type": "activity_stream", "document_id": strings.Repeat("d", 32),
		"project_namespace": s.Cfg.ProjectNamespace, "repositories": []string{s.RepoID(s.Cfg.Repos[0])}, "producer": map[string]any{"tool": "hint-cli", "version": "0.1.0"},
		"created_at": "2026-10-24T12:00:00Z", "sources": sources,
		"redaction":      map[string]any{"policy_id": strings.Repeat("e", 32), "profile": "detailed", "record_types": []string{"Observation"}, "free_text": "denied", "time_resolution_seconds": 0},
		"content_digest": pd, "payload": payload}
	raw, _ := json.Marshal(doc)
	decoded, err := hint.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := hint.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(decoded); err != nil {
		return []string{err.Error()}
	}
	plain, err := hint.Plain(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range hint.Check(plain, nil) {
		out = append(out, x.Code+" "+x.Path)
	}
	return out
}

func membershipObservations(t *testing.T, s *store.Store) []activity.Observation {
	t.Helper()
	obs, _, err := ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	var out []activity.Observation
	for _, o := range obs {
		if o.Membership != nil {
			out = append(out, o)
		}
	}
	return out
}

func TestMembershipEpochPublishesOneCheckedRecord(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	launched := mustEnroll(t, f, e, "task-1", "run-1")
	if err := e.LaunchHook(f.s, launched)(f.facts); err != nil {
		t.Fatal(err)
	}
	if err := e.Finish(f.s, launched, nil); err != nil {
		t.Fatal(err)
	}
	refused := mustEnroll(t, f, e, "task-2", "run-2")
	if err := e.Finish(f.s, refused, nil); err != nil {
		t.Fatal(err)
	}
	states := finishStates(t, f, 1)
	if states[launched.ExecutionRef] != runner.AgentStartUnconfirmed || states[refused.ExecutionRef] != runner.AgentRefusedBeforeLaunch {
		t.Fatalf("finish states %v", states)
	}
	id, err := e.Seal(f.s, memNow)
	if err != nil {
		t.Fatal(err)
	}
	obs := membershipObservations(t, f.s)
	if len(obs) != 1 || obs[0].ObservationID != id {
		t.Fatalf("published %d membership records", len(obs))
	}
	src := orchestratorSource(f.s.Key(), obs[0].Membership.UnitRef, 1)
	if findings := activityDoc(t, f.s, []activity.Source{src}, obs); len(findings) != 0 {
		t.Fatalf("findings %v", findings)
	}
	u, _ := LoadWork(f.s, memLabel)
	if obs[0].Membership.UnitRef != u.UnitRef || len(u.Attempts) != 1 || len(u.Attempts[0].Tasks) != 2 {
		t.Fatalf("unit %+v", u)
	}
}

// A seal replay after a crash or an hour boundary republishes the persisted bytes unchanged.
func TestMembershipSealReplayIsDeterministic(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	// crash after the seal event, before the batch bytes are persisted
	le, err := e.reload(f.s)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, _ := latestRevision(le.unit)
	if err := appendEvent(f.s, memLabel, 1, le.view, map[string]any{"kind": "epoch_seal", "frozen_open_revision_digest": rev, "executions": 1}); err != nil {
		t.Fatal(err)
	}
	id, err := e.Seal(f.s, memNow)
	if err != nil {
		t.Fatal(err)
	}
	sealed := f.s.Path(membershipPath(f.s, memLabel, sealedName(1))...)
	persisted, _ := os.ReadFile(sealed)
	batch := f.s.Path("state", "activity", activityBatchDir, orchestratorSource(f.s.Key(), le.unit.UnitRef, 1).SourceID+".json")
	published, _ := os.ReadFile(batch)
	// crash after the persisted bytes, before the publication; the replay is hours later
	if err := os.Remove(batch); err != nil {
		t.Fatal(err)
	}
	for _, later := range []time.Time{memNow.Add(90 * time.Minute), memNow.Add(26 * time.Hour)} {
		again, err := e.Seal(f.s, later)
		if err != nil || again != id {
			t.Fatalf("replay %q %v", again, err)
		}
		p2, _ := os.ReadFile(sealed)
		b2, _ := os.ReadFile(batch)
		if !bytes.Equal(p2, persisted) || !bytes.Equal(b2, published) {
			t.Fatal("a replay rewrote the sealed or published bytes")
		}
	}
	if spool := spoolBytes(t, f, 1); bytes.Count(spool, []byte("epoch_seal")) != 1 {
		t.Fatal("a replay appended a second seal")
	}
}

func TestMembershipSecondEpochNamesTheWholePreviousRecord(t *testing.T) {
	f := membershipFixture(t)
	e1 := mustOpen(t, f)
	x := mustEnroll(t, f, e1, "task-1", "run-1")
	if err := e1.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := EpochOpen(f.s, memLabel, f.policy, memNow); !errors.Is(err, ErrMembership) {
		t.Fatalf("a second epoch opened beside an open one: %v", err)
	}
	if _, err := e1.Seal(f.s, memNow); err != nil {
		t.Fatal(err)
	}
	e2 := mustOpen(t, f)
	y := mustEnroll(t, f, e2, "task-2", "run-2")
	if err := e2.LaunchHook(f.s, y)(f.facts); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeEpoch(f.s, memLabel); err != nil { // crash after the launch: start_unconfirmed
		t.Fatal(err)
	}
	if _, err := e2.Seal(f.s, memNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	obs := membershipObservations(t, f.s)
	if len(obs) != 2 {
		t.Fatalf("%d records", len(obs))
	}
	first, _ := hint.Digest(membershipBody(obs[0].Membership))
	if obs[0].Membership.Events[0]["epoch"] != float64(1) {
		obs[0], obs[1] = obs[1], obs[0]
		first, _ = hint.Digest(membershipBody(obs[0].Membership))
	}
	if obs[1].Membership.Events[0]["predecessor_epoch_digest"] != first || first == obs[0].Membership.HeadDigest {
		t.Fatal("the predecessor is not the digest of the whole previous record")
	}
	unit := obs[0].Membership.UnitRef
	srcs := []activity.Source{orchestratorSource(f.s.Key(), unit, 1), orchestratorSource(f.s.Key(), unit, 2)}
	if findings := activityDoc(t, f.s, srcs, obs); len(findings) != 0 {
		t.Fatalf("findings %v", findings)
	}
	if finishStates(t, f, 2)[y.ExecutionRef] != runner.AgentStartUnconfirmed {
		t.Fatal("a resumed launch is not start_unconfirmed")
	}
}

func TestMembershipResumeClosesWithTheWeakestStates(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	a := mustEnroll(t, f, e, "task-1", "run-1")
	b := mustEnroll(t, f, e, "task-2", "run-2")
	if err := e.LaunchHook(f.s, b)(f.facts); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeEpoch(f.s, memLabel); err != nil {
		t.Fatal(err)
	}
	states := finishStates(t, f, 1)
	if states[a.ExecutionRef] != runner.AgentRefusedBeforeLaunch || states[b.ExecutionRef] != runner.AgentStartUnconfirmed {
		t.Fatalf("states %v", states)
	}
	u, _ := LoadWork(f.s, memLabel)
	if len(u.Attempts[0].Tasks) != 2 {
		t.Fatal("recovery removed a member task")
	}
	if err := e.LaunchHook(f.s, a)(f.facts); !errors.Is(err, ErrMembership) {
		t.Fatalf("a closed execution launched: %v", err)
	}
}

func TestMembershipRefusesKnownRunsAndExecutions(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	mustEnroll(t, f, e, "task-1", "run-1")
	// a stored controlled run's trial and a run correlation of the activity spool
	if err := f.s.WriteJSON(map[string]any{"manifest": map[string]any{}, "units": []any{map[string]any{"trials": []string{"run-controlled"}}}}, "state", "evals", "work", "r1.json"); err != nil {
		t.Fatal(err)
	}
	src := activity.Source{SourceID: strings.Repeat("c", 32), Agent: "opencode", Adapter: activity.Adapter{Name: "opencode", Version: "0.1.0"}, Capabilities: []string{}, Unavailable: []string{}}
	if err := RegisterSource(f.s, src); err != nil {
		t.Fatal(err)
	}
	at := "2026-10-24T12:00:00.000Z"
	o := activity.Observation{ObservationID: strings.Repeat("f", 32), Class: "observation", Op: "UNKNOWN", Source: "adapter", SourceID: src.SourceID, Precision: "exact",
		Time: activity.Time{Start: &at, End: &at, Clock: "collector"}, Correlation: map[string]string{"run_ref": activity.RunRef(f.s.Key(), "run-correlated")},
		Outcome: activity.Outcome{Status: "success"}, Missing: []string{}, Provenance: activity.Provenance{CapturedAt: at, Transformations: []activity.Transform{}}}
	if err := AppendObservation(f.s, o); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"run-1", "run-controlled", "run-correlated"} {
		before := spoolBytes(t, f, 1)
		if _, err := e.Enroll(f.s, "task-x-"+run, run, f.cfg, memNow); !errors.Is(err, ErrMembership) {
			t.Fatalf("%s enrolled: %v", run, err)
		}
		if !bytes.Equal(before, spoolBytes(t, f, 1)) {
			t.Fatalf("%s: a refused enrollment wrote", run)
		}
	}
}

func TestMembershipRefusesATaskOfAnotherUnitAndAClosedUnit(t *testing.T) {
	f := membershipFixture(t)
	if _, err := WorkStart(f.s, "other-unit", nil, "", "family", &f.cfg, memNow); err != nil {
		t.Fatal(err)
	}
	if err := WorkJoin(f.s, "other-unit", TaskID(f.s.Key(), "task-o"), memNow); err != nil {
		t.Fatal(err)
	}
	e := mustOpen(t, f)
	if _, err := e.Enroll(f.s, "task-o", "run-o", f.cfg, memNow); !errors.Is(err, ErrMembership) {
		t.Fatalf("a task of another unit enrolled: %v", err)
	}
	x := mustEnroll(t, f, e, "task-1", "run-1")
	// the attempt is closed before the launch: the hook refuses
	if _, err := WorkClose(f.s, memLabel, "failed", memNow); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchHook(f.s, x)(f.facts); !errors.Is(err, ErrMembership) {
		t.Fatalf("a launch of a closed attempt: %v", err)
	}
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(f.s, memNow); !errors.Is(err, ErrMembership) {
		t.Fatalf("a seal after the attempt closed: %v", err)
	}
}

func TestMembershipLaunchFactsMustPairWithTheEnrolledConfiguration(t *testing.T) {
	f := membershipFixture(t)
	other := "sha256:" + strings.Repeat("c", 64)
	f.policy.LaunchDigests = []string{memLaunch, other} // allowed by the policy, not by the enrolled configuration
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	cases := map[string]func(l *runner.LaunchFacts){
		"other allowed launch": func(l *runner.LaunchFacts) { l.LaunchDigest = other },
		"other model":          func(l *runner.LaunchFacts) { l.Model = "another-model" },
		"other gate":           func(l *runner.LaunchFacts) { l.GatePolicyDigest = other },
		"unknown target":       func(l *runner.LaunchFacts) { l.Target = "plan9" },
		"bad execution":        func(l *runner.LaunchFacts) { l.ExecutionDigest = "x" },
	}
	for name, change := range cases {
		facts := f.facts
		change(&facts)
		before := spoolBytes(t, f, 1)
		if err := e.LaunchHook(f.s, x)(facts); !errors.Is(err, ErrMembership) {
			t.Fatalf("%s launched: %v", name, err)
		}
		if !bytes.Equal(before, spoolBytes(t, f, 1)) {
			t.Fatalf("%s: a refused launch wrote", name)
		}
	}
	forged := x
	forged.TaskID = TaskID(f.s.Key(), "task-forged")
	if err := e.LaunchHook(f.s, forged)(f.facts); !errors.Is(err, ErrMembership) {
		t.Fatalf("a forged execution launched: %v", err)
	}
	if err := e.LaunchHook(f.s, x)(f.facts); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchHook(f.s, x)(f.facts); !errors.Is(err, ErrMembership) {
		t.Fatalf("a second launch: %v", err)
	}
}

func TestMembershipPolicyIsFrozenAndPinned(t *testing.T) {
	f := membershipFixture(t)
	for name, change := range map[string]func(p *MembershipPolicy){
		"gate":      func(p *MembershipPolicy) { p.GatePolicyDigest = memLaunch },
		"runner":    func(p *MembershipPolicy) { p.RunnerVersion = "0.0.0" },
		"schema":    func(p *MembershipPolicy) { p.SchemaDigest = memLaunch },
		"coverage":  func(p *MembershipPolicy) { p.Coverage = "all_project_work" },
		"unsorted":  func(p *MembershipPolicy) { p.LaunchDigests = []string{memExec, memLaunch} },
		"duplicate": func(p *MembershipPolicy) { p.LaunchDigests = []string{memLaunch, memLaunch} },
		"empty":     func(p *MembershipPolicy) { p.ConfigurationDigests = nil },
		"no study":  func(p *MembershipPolicy) { p.StudyLaunchPolicy = "" },
	} {
		p := f.policy
		change(&p)
		if _, err := EpochOpen(f.s, memLabel, p, memNow); !errors.Is(err, ErrMembership) {
			t.Fatalf("%s: opened %v", name, err)
		}
	}
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	// a changed policy file refuses every later operation
	pf := f.s.Path(membershipPath(f.s, memLabel, policyName(1))...)
	raw, _ := os.ReadFile(pf)
	os.WriteFile(pf, bytes.Replace(raw, []byte(`"opencode"`), []byte(`"opencode" `), 1), 0o600)
	if _, err := e.Enroll(f.s, "task-2", "run-2", f.cfg, memNow); !errors.Is(err, ErrMembership) {
		t.Fatalf("enrolled with a changed policy: %v", err)
	}
	if err := e.LaunchHook(f.s, x)(f.facts); !errors.Is(err, ErrMembership) {
		t.Fatalf("launched with a changed policy: %v", err)
	}
	if _, err := ResumeEpoch(f.s, memLabel); !errors.Is(err, ErrMembership) {
		t.Fatalf("resumed with a changed policy: %v", err)
	}
}

// A damaged spool refuses every operation and is never repaired.
func TestMembershipDamagedSpoolFailsClosed(t *testing.T) {
	damages := map[string]func(b []byte) []byte{
		"partial last line": func(b []byte) []byte { return b[:len(b)-5] },
		"changed chain": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"prev_digest":"sha256:`), []byte(`"prev_digest":"sha256:0`), 1)[:len(b)]
		},
		"duplicate key": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`{"attempt_ref"`), []byte(`{"kind":"enroll","attempt_ref"`), 1)
		},
		"swapped lines": func(b []byte) []byte {
			l := bytes.SplitAfter(b, []byte("\n"))
			l[1], l[2] = l[2], l[1]
			return bytes.Join(l, nil)
		},
		"extra field": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"kind":"enroll"`), []byte(`"kind":"enroll","raw":"x"`), 1)
		},
	}
	for name, damage := range damages {
		t.Run(name, func(t *testing.T) {
			f := membershipFixture(t)
			e := mustOpen(t, f)
			x := mustEnroll(t, f, e, "task-1", "run-1")
			mustEnroll(t, f, e, "task-2", "run-2")
			p := f.s.Path(membershipPath(f.s, memLabel, epochName(1))...)
			bad := damage(spoolBytes(t, f, 1))
			os.WriteFile(p, bad, 0o600)
			ops := map[string]error{"enroll": func() error { _, err := e.Enroll(f.s, "task-3", "run-3", f.cfg, memNow); return err }(),
				"launch": e.LaunchHook(f.s, x)(f.facts), "finish": e.Finish(f.s, x, nil),
				"seal":   func() error { _, err := e.Seal(f.s, memNow); return err }(),
				"resume": func() error { _, err := ResumeEpoch(f.s, memLabel); return err }(),
				"open":   func() error { _, err := EpochOpen(f.s, memLabel, f.policy, memNow); return err }()}
			for op, err := range ops {
				if !errors.Is(err, ErrMembership) {
					t.Fatalf("%s succeeded on a damaged spool: %v", op, err)
				}
			}
			if after, _ := os.ReadFile(p); !bytes.Equal(after, bad) {
				t.Fatal("the damaged spool was rewritten")
			}
		})
	}
}

// A WorkJoin failure after the durable enroll keeps the member and closes it administratively;
// the journal is not rolled back.
func TestMembershipFailedJoinKeepsTheMember(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	work := filepath.Dir(f.s.Path("state", "work", workFile(f.s, memLabel)))
	before, _ := os.ReadFile(filepath.Join(work, workFile(f.s, memLabel)))
	if err := os.Chmod(work, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := e.Enroll(f.s, "task-1", "run-1", f.cfg, memNow)
	os.Chmod(work, 0o700)
	if !errors.Is(err, ErrMembership) {
		t.Fatalf("enroll: %v", err)
	}
	x := hint.EvalRef(f.s.Key(), "execution", memLabel, "run-1")
	if finishStates(t, f, 1)[x] != runner.AgentRefusedBeforeLaunch {
		t.Fatal("the failed member is not closed refused_before_launch")
	}
	after, _ := os.ReadFile(filepath.Join(work, workFile(f.s, memLabel)))
	if !bytes.Equal(before, after) {
		t.Fatal("the journal changed")
	}
	if _, err := e.Enroll(f.s, "task-1", "run-1", f.cfg, memNow); !errors.Is(err, ErrMembership) {
		t.Fatal("the failed member's run was enrolled again")
	}
}

func TestMembershipSpoolBounds(t *testing.T) {
	var b bytes.Buffer
	v := &epochView{}
	add := func(ev map[string]any) {
		ev["seq"], ev["prev_digest"] = len(v.events)+1, v.head()
		line, _ := json.Marshal(ev)
		b.Write(append(line, '\n'))
		d, _ := hint.Digest(plainJSON(ev))
		v.events = append(v.events, spoolEvent{plainJSON(ev), d})
	}
	add(map[string]any{"kind": "epoch_open", "epoch": 1, "original_scope_digest": memLaunch, "enrollment_start_revision_digest": memLaunch, "policy_digest": memLaunch,
		"coverage": MembershipCoverage, "predecessor_epoch_digest": nil})
	for i := 0; i <= hint.MembershipMaxExecutions; i++ {
		add(map[string]any{"kind": "enroll", "execution_ref": strings.Repeat("0", 28) + hexN(i), "run_ref": strings.Repeat("1", 28) + hexN(i),
			"attempt_ref": strings.Repeat("2", 32), "task_id": strings.Repeat("3", 32), "planned_configuration_digest": memLaunch})
	}
	if _, err := parseEpoch(b.Bytes()); !errors.Is(err, ErrMembership) {
		t.Fatalf("an epoch with %d executions parsed: %v", hint.MembershipMaxExecutions+1, err)
	}
}

func hexN(i int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[i>>12&15], digits[i>>8&15], digits[i>>4&15], digits[i&15]})
}

// No raw label, run, task or model name is stored by the producer.
func TestMembershipStoresNoRawNames(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "raw-task-name-5d2e", "raw-run-name-c41f")
	if err := e.LaunchHook(f.s, x)(f.facts); err != nil {
		t.Fatal(err)
	}
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(f.s, memNow); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, dir := range []string{f.s.Path("state", membershipDir), f.s.Path("state", "activity", activityBatchDir)} {
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				files = append(files, p)
			}
			return nil
		})
	}
	if len(files) < 4 {
		t.Fatalf("files %v", files)
	}
	for _, p := range files {
		b, _ := os.ReadFile(p)
		for _, raw := range []string{memLabel, "raw-task-name-5d2e", "raw-run-name-c41f", memModel} {
			if bytes.Contains(b, []byte(raw)) {
				t.Fatalf("%s contains %q", p, raw)
			}
		}
	}
}

// A record that names the execution's run after its enrollment poisons it: the hook refuses and
// the epoch is not published.
func TestMembershipLaterForeignOwnerPoisonsTheExecution(t *testing.T) {
	foreign := map[string]func(t *testing.T, f memFixture, x Execution){
		"controlled run": func(t *testing.T, f memFixture, x Execution) {
			if err := f.s.WriteJSON(map[string]any{"units": []any{map[string]any{"trials": []string{"run-1"}}}}, "state", "evals", "work", "r1.json"); err != nil {
				t.Fatal(err)
			}
		},
		"controlled ledger": func(t *testing.T, f memFixture, x Execution) {
			m := activity.ModelLedger{LedgerRef: strings.Repeat("9", 32), State: "incomplete", Reasons: []string{"no_requests"}, Counts: map[string]int{},
				Components: map[string]*int64{}, ComponentCoverage: map[string]int{}, SealedAt: "2026-10-24T12:00:00Z",
				Binding: activity.LedgerBinding{RunRef: x.RunRef, TaskID: x.TaskID, TaskFamilyID: strings.Repeat("4", 32), ConfigurationDigest: x.PlannedConfigurationDigest,
					UnitRef: strings.Repeat("5", 32), AttemptRef: strings.Repeat("6", 32)}}
			src := activity.LedgerSource(f.s.Key(), "opencode", f.cfg)
			if err := RegisterSource(f.s, src); err != nil {
				t.Fatal(err)
			}
			if err := AppendObservation(f.s, activity.LedgerObservation(f.s.Key(), src, m)); err != nil {
				t.Fatal(err)
			}
		},
		"run correlation": func(t *testing.T, f memFixture, x Execution) {
			src := activity.Source{SourceID: strings.Repeat("c", 32), Agent: "opencode", Adapter: activity.Adapter{Name: "opencode", Version: "0.1.0"}, Capabilities: []string{}, Unavailable: []string{}}
			if err := RegisterSource(f.s, src); err != nil {
				t.Fatal(err)
			}
			at := "2026-10-24T12:00:00.000Z"
			if err := AppendObservation(f.s, activity.Observation{ObservationID: strings.Repeat("f", 32), Class: "observation", Op: "UNKNOWN", Source: "adapter",
				SourceID: src.SourceID, Precision: "exact", Time: activity.Time{Start: &at, End: &at, Clock: "collector"}, Correlation: map[string]string{"run_ref": x.RunRef},
				Outcome: activity.Outcome{Status: "success"}, Missing: []string{}, Provenance: activity.Provenance{CapturedAt: at, Transformations: []activity.Transform{}}}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range foreign {
		t.Run(name, func(t *testing.T) {
			f := membershipFixture(t)
			e := mustOpen(t, f)
			x := mustEnroll(t, f, e, "task-1", "run-1")
			write(t, f, x)
			before := spoolBytes(t, f, 1)
			if err := e.LaunchHook(f.s, x)(f.facts); !errors.Is(err, ErrMembership) {
				t.Fatalf("a poisoned execution launched: %v", err)
			}
			if !bytes.Equal(before, spoolBytes(t, f, 1)) {
				t.Fatal("a refused launch wrote")
			}
			if err := e.Finish(f.s, x, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Seal(f.s, memNow); !errors.Is(err, ErrMembership) {
				t.Fatalf("a poisoned epoch was published: %v", err)
			}
			if len(membershipObservations(t, f.s)) != 0 {
				t.Fatal("a poisoned epoch has a published record")
			}
		})
	}
}

// A poisoned epoch whose seal event was already written is not published either.
func TestMembershipForeignOwnerAfterTheSealEvent(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	le, _ := e.reload(f.s)
	_, rev, _ := latestRevision(le.unit)
	if err := appendEvent(f.s, memLabel, 1, le.view, map[string]any{"kind": "epoch_seal", "frozen_open_revision_digest": rev, "executions": 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WriteJSON(map[string]any{"units": []any{map[string]any{"trials": []string{"run-1"}}}}, "state", "evals", "work", "r1.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(f.s, memNow); !errors.Is(err, ErrMembership) {
		t.Fatalf("published: %v", err)
	}
	if _, err := os.Stat(f.s.Path(membershipPath(f.s, memLabel, sealedName(1))...)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("batch bytes were persisted for a poisoned epoch")
	}
}

func TestMembershipGlobalEpochReadsAreBounded(t *testing.T) {
	f := membershipFixture(t)
	mustOpen(t, f)
	p := f.s.Path("state", membershipDir, strings.Repeat("a", 32), epochName(1))
	os.MkdirAll(filepath.Dir(p), 0o700)
	big := bytes.Repeat([]byte("x"), maxMembershipBytes+1)
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := refOwners(f.s); !errors.Is(err, ErrMembership) {
		t.Fatalf("an oversized foreign spool was read: %v", err)
	}
}

// memBehavior decodes the root CLI fixture as the behavior of a run with the given task, family
// and configuration (the real decoder, synthetic data).
func memBehavior(t *testing.T, f memFixture, cfg activity.Configuration, task, family, run string) activity.CLIRootImport {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "activity", "testdata", "cli-root", "p3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := activity.DecodeCLIRoot(bytes.NewReader(raw), activity.CLIRootOptions{Key: f.s.Key(), AgentVersion: activity.CLIRootAgentVersion, Configuration: &cfg,
		TaskID: task, TaskFamilyID: family, RunRef: run, Cwd: "/app", Resolve: func(string) (string, bool) { return strings.Repeat("1", 32), true },
		CaptureComplete: true, CapturedAt: memNow})
	if err != nil || len(d.Observations) == 0 {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// The execution's own root CLI behavior is not a foreign owner, before and after the seal event
// and in a published replay.
func TestMembershipOwnBehaviorIsNotForeign(t *testing.T) {
	for _, afterSealEvent := range []bool{false, true} {
		f := membershipFixture(t)
		e := mustOpen(t, f)
		x := mustEnroll(t, f, e, "task-1", "run-1")
		if err := e.LaunchHook(f.s, x)(f.facts); err != nil {
			t.Fatal(err)
		}
		if err := e.Finish(f.s, x, nil); err != nil {
			t.Fatal(err)
		}
		if afterSealEvent {
			le, _ := e.reload(f.s)
			_, rev, _ := latestRevision(le.unit)
			if err := appendEvent(f.s, memLabel, 1, le.view, map[string]any{"kind": "epoch_seal", "frozen_open_revision_digest": rev, "executions": 1}); err != nil {
				t.Fatal(err)
			}
		}
		d := memBehavior(t, f, f.cfg, x.TaskID, activity.FamilyRef(f.s.Key(), "family"), x.RunRef)
		if err := WriteActivityBatch(f.s, d.Source, d.Observations); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Seal(f.s, memNow); err != nil {
			t.Fatalf("after seal event %v: own behavior prevents publication: %v", afterSealEvent, err)
		}
		if _, err := e.Seal(f.s, memNow.Add(3*time.Hour)); err != nil {
			t.Fatalf("published replay: %v", err)
		}
	}
}

// Behavior that is not exactly the execution's own stays a foreign owner.
func TestMembershipOtherBehaviorStaysForeign(t *testing.T) {
	cases := map[string]func(t *testing.T, f memFixture, x Execution){
		"other task": func(t *testing.T, f memFixture, x Execution) {
			d := memBehavior(t, f, f.cfg, TaskID(f.s.Key(), "task-other"), activity.FamilyRef(f.s.Key(), "family"), x.RunRef)
			if err := WriteActivityBatch(f.s, d.Source, d.Observations); err != nil {
				t.Fatal(err)
			}
		},
		"other family": func(t *testing.T, f memFixture, x Execution) {
			d := memBehavior(t, f, f.cfg, x.TaskID, activity.FamilyRef(f.s.Key(), "family-other"), x.RunRef)
			if err := WriteActivityBatch(f.s, d.Source, d.Observations); err != nil {
				t.Fatal(err)
			}
		},
		"other configuration": func(t *testing.T, f memFixture, x Execution) {
			cfg := f.cfg
			cfg.Budget.MaxRequests.Value = 6
			d := memBehavior(t, f, cfg, x.TaskID, activity.FamilyRef(f.s.Key(), "family"), x.RunRef)
			if err := WriteActivityBatch(f.s, d.Source, d.Observations); err != nil {
				t.Fatal(err)
			}
		},
		"inconsistent source declaration": func(t *testing.T, f memFixture, x Execution) {
			d := memBehavior(t, f, f.cfg, x.TaskID, activity.FamilyRef(f.s.Key(), "family"), x.RunRef)
			d.Source.Capabilities = append(d.Source.Capabilities, "usage")
			if err := RegisterSource(f.s, d.Source); err != nil {
				t.Fatal(err)
			}
			for _, o := range d.Observations {
				if err := AppendObservation(f.s, o); err != nil {
					t.Fatal(err)
				}
			}
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			f := membershipFixture(t)
			e := mustOpen(t, f)
			x := mustEnroll(t, f, e, "task-1", "run-1")
			write(t, f, x)
			if err := e.LaunchHook(f.s, x)(f.facts); !errors.Is(err, ErrMembership) {
				t.Fatalf("launched beside foreign behavior: %v", err)
			}
			if err := e.Finish(f.s, x, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Seal(f.s, memNow); !errors.Is(err, ErrMembership) {
				t.Fatalf("published beside foreign behavior: %v", err)
			}
		})
	}
}

// membershipFiles lists the producer's files of the fixture unit with their bytes.
func membershipFiles(t *testing.T, f memFixture) map[string]string {
	t.Helper()
	out := map[string]string{}
	dir := f.s.Path(membershipPath(f.s, memLabel)...)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

func sealedFirstEpoch(t *testing.T, f memFixture) (*Epoch, string) {
	t.Helper()
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(f.s, memNow); err != nil {
		t.Fatal(err)
	}
	u, _ := LoadWork(f.s, memLabel)
	return e, f.s.Path("state", "activity", activityBatchDir, orchestratorSource(f.s.Key(), u.UnitRef, 1).SourceID+".json")
}

// C1: a next epoch never opens over an uncommitted predecessor; the predecessor's persisted
// bytes are committed first, unchanged.
func TestMembershipNextEpochCommitsTheExactPredecessorFirst(t *testing.T) {
	f := membershipFixture(t)
	_, batch := sealedFirstEpoch(t, f)
	published, _ := os.ReadFile(batch)
	if err := os.Remove(batch); err != nil { // crash image: persisted, not committed
		t.Fatal(err)
	}
	e2, err := EpochOpen(f.s, memLabel, f.policy, memNow.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(batch); !bytes.Equal(again, published) {
		t.Fatal("the predecessor was not committed with its persisted bytes")
	}
	if e2.Number() != 2 || len(membershipObservations(t, f.s)) != 1 {
		t.Fatal("unexpected epochs")
	}
}

// C1: a predecessor sealed in its spool whose bytes were never persisted is persisted and
// committed before the next epoch opens.
func TestMembershipNextEpochPublishesASealedUnpersistedPredecessor(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	le, _ := e.reload(f.s)
	_, rev, _ := latestRevision(le.unit)
	if err := appendEvent(f.s, memLabel, 1, le.view, map[string]any{"kind": "epoch_seal", "frozen_open_revision_digest": rev, "executions": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := EpochOpen(f.s, memLabel, f.policy, memNow); err != nil {
		t.Fatal(err)
	}
	obs := membershipObservations(t, f.s)
	if len(obs) != 1 || obs[0].Membership.Events[0]["epoch"] != float64(1) {
		t.Fatal("the predecessor was not published first")
	}
}

// C1/C2: an unpublishable or invalid predecessor refuses the next epoch and writes nothing.
func TestMembershipNextEpochRefusesAnInvalidPredecessorWithoutWriting(t *testing.T) {
	cases := map[string]func(t *testing.T, f memFixture, batch string){
		"predecessor open": func(t *testing.T, f memFixture, batch string) {},
		"predecessor policy bytes": func(t *testing.T, f memFixture, batch string) {
			p := f.s.Path(membershipPath(f.s, memLabel, policyName(1))...)
			raw, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(raw, []byte(`"opencode"`), []byte(`"opencode" `), 1), 0o600)
		},
		"predecessor policy pin": func(t *testing.T, f memFixture, batch string) {
			p := f.s.Path(membershipPath(f.s, memLabel, policyName(1))...)
			raw, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(raw, []byte(runner.DockerRunnerSemver), []byte("0.0.0-other"), 1), 0o600)
		},
		"predecessor persisted bytes": func(t *testing.T, f memFixture, batch string) {
			p := f.s.Path(membershipPath(f.s, memLabel, sealedName(1))...)
			raw, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(raw, []byte(`"success"`), []byte(`"failure"`), 1), 0o600)
		},
		"predecessor committed other bytes": func(t *testing.T, f memFixture, batch string) {
			raw, _ := os.ReadFile(batch)
			os.WriteFile(batch, bytes.Replace(raw, []byte(`2026-10-24T12:30:00.000Z`), []byte(`2026-10-24T12:31:00.000Z`), 2), 0o600)
		},
		"predecessor poisoned": func(t *testing.T, f memFixture, batch string) {
			if err := f.s.WriteJSON(map[string]any{"units": []any{map[string]any{"trials": []string{"run-1"}}}}, "state", "evals", "work", "r1.json"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := membershipFixture(t)
			var batch string
			if name == "predecessor open" {
				mustOpen(t, f)
			} else {
				_, batch = sealedFirstEpoch(t, f)
			}
			change(t, f, batch)
			before := membershipFiles(t, f)
			if _, err := EpochOpen(f.s, memLabel, f.policy, memNow); err == nil {
				t.Fatal("a next epoch opened over an invalid predecessor")
			}
			after := membershipFiles(t, f)
			if len(after) != len(before) {
				t.Fatalf("a refused open wrote files: %d -> %d", len(before), len(after))
			}
			for k, v := range before {
				if after[k] != v {
					t.Fatalf("a refused open changed %s", k)
				}
			}
		})
	}
}

// C3: no seal, with or without members, after the unit closed; nothing is appended.
func TestMembershipSealNeedsAnOpenUnitRevision(t *testing.T) {
	for _, members := range []bool{false, true} {
		f := membershipFixture(t)
		e := mustOpen(t, f)
		if members {
			x := mustEnroll(t, f, e, "task-1", "run-1")
			if err := e.Finish(f.s, x, nil); err != nil {
				t.Fatal(err)
			}
		} else if err := WorkJoin(f.s, memLabel, TaskID(f.s.Key(), "external-task"), memNow); err != nil {
			t.Fatal(err)
		}
		if _, err := WorkClose(f.s, memLabel, "abandoned", memNow); err != nil {
			t.Fatal(err)
		}
		before := spoolBytes(t, f, 1)
		if _, err := e.Seal(f.s, memNow); !errors.Is(err, ErrMembership) {
			t.Fatalf("members %v: sealed a closed unit: %v", members, err)
		}
		if !bytes.Equal(before, spoolBytes(t, f, 1)) || len(membershipObservations(t, f.s)) != 0 {
			t.Fatalf("members %v: a refused seal wrote", members)
		}
	}
}
