package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
)

// Live tests use only the owner's LAN model (AGENTS.md §11) with finite limits. Set
// HINT_LIVE_MODEL_URL (private address, .../v1), HINT_LIVE_MODEL and HINT_LOCAL_MODEL_KEY.
// HINT_LIVE_OUT is a directory that keeps the outputs for review and offline fixtures.

func liveModel(t *testing.T) *evals.Model {
	t.Helper()
	url := os.Getenv("HINT_LIVE_MODEL_URL")
	if url == "" {
		t.Skip("set HINT_LIVE_MODEL_URL")
	}
	return &evals.Model{Use: "lan", BaseURL: url, Model: os.Getenv("HINT_LIVE_MODEL"), KeyEnv: "HINT_LOCAL_MODEL_KEY",
		MaxRequestsPerJob: 2, MaxRetries: 1, MaxSecondsPerJob: 300}
}

func liveOut(t *testing.T, name string, data []byte) {
	if dir := os.Getenv("HINT_LIVE_OUT"); dir != "" {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, name), data, 0o600)
	}
}

// TestLiveRequirementMap asks the LAN model for the requirement mapping of the reviewer's
// G10-3 case (a requested panic that no test checks). At most 2 requests. The outcome is
// logged; the test fails only on the budget, not on the model's judgment.
func TestLiveRequirementMap(t *testing.T) {
	m := liveModel(t)
	l := evaltest.Library(t)
	c := evaltest.PanicRequirement()
	b, err := evaltest.Package(t, l, c, "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	p := evals.Enabled(nil)
	p.Model = m
	if err := p.Check(evals.OpModel, ""); err != nil {
		t.Fatal(err) // a refused policy would end without a request
	}
	start := time.Now()
	rec, why, detail := mapRequirementsDetail(context.Background(), l, &p, b, evaltest.Tree(t, l, c.Result))
	liveOut(t, "requirement-map-raw-answer.txt", []byte(detail.Answer))
	t.Logf("refusal cause: %q", detail.Cause)
	jobs, _ := l.Jobs()
	var requests, retries int
	for _, j := range jobs {
		requests, retries = requests+j.Requests, retries+j.Retries
	}
	t.Logf("requests %d retries %d wall %s reason %q unmapped %v", requests, retries, time.Since(start).Round(time.Second), why, build.Unmapped(rec))
	for _, r := range rec["requirements"].([]any) {
		q := r.(map[string]any)
		text, _ := l.GetObject(q["text"].(string))
		t.Logf("  %s %-12s %-12v checks %v tests %v: %s", q["req_id"], q["origin"], q["mapping"], q["checks"], q["tests"], text)
		if g, ok := q["generator"].(map[string]any); ok {
			if out, err := l.GetObject(g["output"].(string)); err == nil {
				liveOut(t, "requirement-map-answer.json", out)
			}
		}
	}
	summary, _ := json.MarshalIndent(map[string]any{"requests": requests, "retries": retries, "reason": why, "unmapped": build.Unmapped(rec)}, "", "  ")
	liveOut(t, "requirement-map-summary.json", summary)
	if requests > 2 {
		t.Fatalf("budget exceeded: %d requests", requests)
	}
}

// TestLiveOpenCodeTrial is one session of the bounded §11 check of the pinned OpenCode on the
// built-in runner: the validated synthetic bug fix, 1 eval x 1 attempt, at most 8 forwarded
// model requests in total (the gate admits every forwarded request, an agent retry included; the
// agent's retries are not observed and not limited separately: Model.MaxRetries applies to HINT's
// own model client, not to OpenCode) and
// 300 seconds of agent time, no trial retry. The endpoint is checked first by a TCP connect
// only (no request); when it is unreachable nothing starts. Set HINT_DOCKER_TESTS=1 too;
// HINT_OPENCODE_TGZ is a local copy of the pinned OpenCode archive (else it is fetched once).
// A key that collides with an exported identifier (for example a part of the model name) is
// refused first; a key equal to the declared model is supported (keyCollision). The run checks
// the chain (gate, ledger, result trust, report) and that the key appears in no output outside
// the one declared public field of the summary; it does not measure canonical tokens or savings.
// captureExpect is the private file of the expected raw backend values (OPS C3 record):
// {"host": ..., "container": ..., "served_model": ...}. It stays on the machine.
type captureExpect struct {
	Host        string `json:"host"`
	Container   string `json:"container"`
	ServedModel string `json:"served_model"`
}

var fullContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// captureCheck compares every written ledger's capture snapshots with the references derived
// from the expected raw values with key; it returns booleans only and the raw host and container
// values for the output scan. Without an expect file the check is "not_checked".
func captureCheck(key []byte, ledgers []*activity.ModelLedger, expectFile string) (map[string]any, []string, error) {
	if expectFile == "" {
		return map[string]any{"state": "not_checked"}, nil, nil
	}
	b, err := os.ReadFile(expectFile)
	if err != nil {
		return nil, nil, errors.New("the capture expect file is not readable")
	}
	var e captureExpect
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	// one complete object; the container is the full engine ID (not a shortened record value)
	var extra json.RawMessage
	if d.Decode(&e) != nil || d.Decode(&extra) != io.EOF || e.Host == "" || !fullContainerID.MatchString(e.Container) || e.ServedModel == "" {
		return nil, nil, errors.New("the capture expect file is not usable")
	}
	host, ctr, served := hint.DeriveID(key, "capture-host", e.Host), hint.DeriveID(key, "capture-container", e.Container),
		hint.DeriveID(key, "capture-served-model", e.ServedModel)
	out := map[string]any{"state": "checked", "ledgers": len(ledgers)}
	all := map[string]bool{"complete": len(ledgers) > 0, "host_ref_equal": len(ledgers) > 0, "container_ref_equal": len(ledgers) > 0,
		"served_model_ref_equal": len(ledgers) > 0, "equal_snapshots": len(ledgers) > 0}
	for _, m := range ledgers {
		bc := m.BackendCapture
		if bc == nil || bc.Start.Status != "complete" || bc.End.Status != "complete" || bc.Start.Snapshot == nil || bc.End.Snapshot == nil {
			for k := range all {
				all[k] = false
			}
			continue
		}
		sb, _ := json.Marshal(bc.Start.Snapshot)
		eb, _ := json.Marshal(bc.End.Snapshot)
		all["equal_snapshots"] = all["equal_snapshots"] && bytes.Equal(sb, eb)
		for _, sn := range []*activity.LedgerCaptureSnapshot{bc.Start.Snapshot, bc.End.Snapshot} {
			all["host_ref_equal"] = all["host_ref_equal"] && sn.HostRef == host
			all["container_ref_equal"] = all["container_ref_equal"] && sn.ContainerRef == ctr
			all["served_model_ref_equal"] = all["served_model_ref_equal"] && sn.Flags.ServedModelRef != nil && *sn.Flags.ServedModelRef == served
		}
	}
	for k, v := range all {
		out[k] = v
	}
	return out, []string{e.Host, e.Container}, nil
}

// capturePreflight runs before any request: with captures, the expect file is required and
// usable (the reference check is never skipped); without captures nothing is needed.
func capturePreflight(key []byte, capture *runner.SSHCapture, expectFile string) error {
	if capture == nil {
		return nil
	}
	if expectFile == "" {
		return errors.New("captures need HINT_LIVE_CAPTURE_EXPECT")
	}
	_, _, err := captureCheck(key, nil, expectFile)
	return err
}

// liveCapture is the controlled research setup's opt-in to the real backend captures: both
// HINT_LIVE_CAPTURE_SSH and HINT_LIVE_CAPTURE_CONTAINER, or neither (no capture); one without the
// other or an unusable value fails before any request.
func liveCapture() (*runner.SSHCapture, error) {
	target, container := os.Getenv("HINT_LIVE_CAPTURE_SSH"), os.Getenv("HINT_LIVE_CAPTURE_CONTAINER")
	if target == "" && container == "" {
		return nil, nil
	}
	if target == "" || container == "" {
		return nil, errors.New("set both HINT_LIVE_CAPTURE_SSH and HINT_LIVE_CAPTURE_CONTAINER, or neither")
	}
	return runner.NewSSHCapture(target, container)
}

func TestLiveOpenCodeTrial(t *testing.T) {
	m := liveModel(t)
	if os.Getenv("HINT_DOCKER_TESTS") != "1" {
		t.Skip("set HINT_DOCKER_TESTS=1")
	}
	if refused, unreachable := liveGate(m); refused != nil {
		t.Fatal(refused)
	} else if unreachable != nil {
		t.Skip(unreachable)
	}
	const maxRequests, maxSeconds = 8, 300
	ctx := context.Background()
	h, err := runner.FindDocker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, l := validatedProject(t)
	p := evals.Enabled(nil)
	p.Model = m
	p.Model.MaxRetries = 1
	p.Run = &evals.Run{Use: "docker", Runner: "docker", MaxTrialsPerRun: 1, MaxSecondsPerTrial: maxSeconds, MaxRequestsPerTrial: maxRequests, CPUs: 2, MemoryMB: 2048}
	s.Cfg.Evals = &p
	lv, err := runner.StartLive(runner.LiveConfig{BaseURL: m.BaseURL, Model: m.Model, KeyEnv: m.KeyEnv, AgentVersion: runner.DockerOpenCodeVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	start := time.Now()
	capture, err := liveCapture()
	if err != nil {
		t.Fatal(err)
	}
	if err := capturePreflight(s.Key(), capture, os.Getenv("HINT_LIVE_CAPTURE_EXPECT")); err != nil {
		t.Fatal(err) // before any request
	}
	rep, err := RunSuite(ctx, s, h, runner.DockerRunnerSemver, SuiteOptions{Agent: "opencode", Attempts: 1, Arm: "baseline", Live: lv, OpenCodeTgz: os.Getenv("HINT_OPENCODE_TGZ"), BackendCapture: capture}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	summary := map[string]any{"runner": "hint-docker " + runner.DockerRunnerSemver, "opencode": runner.DockerOpenCodeVersion, "trials": rep.Trials, "outcomes": rep.Outcomes,
		"model_requests": deref(rep.Requests), "ledgers_not_written": rep.LedgersNotWritten, "wall_seconds": time.Since(start).Round(time.Second).Seconds()}
	sess := liveSession{Trials: rep.Trials, Stop: rep.Stopped, Requests: rep.Requests, MaxRequests: maxRequests, LedgersNotWritten: rep.LedgersNotWritten}
	outputs := map[string][]byte{}
	names, _ := l.Manifests("trial")
	for _, n := range names {
		var tr map[string]any
		if l.Manifest("trial", n, &tr) != nil {
			continue
		}
		sess.Outcome, _ = tr["outcome"].(string)
		summary["trial"] = map[string]any{"outcome": tr["outcome"], "checks": tr["checks"], "counters": tr["counters"], "budget": tr["budget"], "agent": tr["agent"], "runner": tr["runner"], "result_trust": tr["result_trust"]}
		if arts, ok := tr["artifacts"].([]any); ok && len(arts) == 1 {
			if a, err := l.GetObject(arts[0].(string)); err == nil {
				outputs["opencode-trial-run.tar"] = a
				liveOut(t, "opencode-trial-run.tar", a)
			}
		}
	}
	// the ledger handed over after teardown (gate, enforcement, completeness)
	obs, _, err := project.ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	var ledgers []any
	var written []*activity.ModelLedger
	for _, o := range obs {
		if m := o.ModelLedger; m != nil {
			ledgers = append(ledgers, map[string]any{"state": m.State, "bypass": m.Bypass, "reasons": m.Reasons, "counts": m.Counts})
			written = append(written, m)
			sess.Ledgers++
			sess.LedgerState, sess.LedgerBypass, sess.LedgerReasons, sess.CompleteResponses = m.State, m.Bypass, m.Reasons, m.Counts["complete"]
		}
	}
	summary["ledgers"] = ledgers
	// the written ledger revisions themselves (project references only): traffic evidence for a
	// later offline replay, not a builder or behavior chain
	if lb, err := json.MarshalIndent(written, "", "  "); err == nil {
		outputs["ledger.json"] = lb
		liveOut(t, "ledger.json", lb)
	}
	// with captures: the snapshot references against the expected raw values (OPS C3), derived
	// in process with this run's store key; only booleans leave the process
	var forbidden []string
	if capture != nil {
		check, raw, err := captureCheck(s.Key(), written, os.Getenv("HINT_LIVE_CAPTURE_EXPECT"))
		if err != nil {
			t.Fatal(err)
		}
		summary["capture_check"], forbidden = check, raw
	}
	// the report with the journal validates
	revs, _ := workunits.Unreported(s)
	doc, _, rerr := report.Builder{Key: s.Key(), Namespace: s.Cfg.ProjectNamespace, WorkUnits: revs}.Build(l, names, time.Now())
	summary["report_error"] = fmt.Sprint(rerr)
	sess.ReportErr = rerr
	outputs["report.json"] = doc
	liveOut(t, "report.json", doc)
	b, _ := json.MarshalIndent(summary, "", "  ")
	outputs["summary.json"] = b
	// the key appears in no output, in no form (raw, escaped, encoded; archives entry by entry, in
	// order), except as the declared model in the summary's one public field when the key equals
	// it; a hit names the output and the form, never the value; a scan that could not read
	// everything is not clean
	sess.KeyHits, sess.KeyScanIncomplete = keyHits(os.Getenv(m.KeyEnv), outputs, summaryPublic("summary.json", m.Model))
	summary["key_hits"], summary["key_scan_incomplete"] = sess.KeyHits, sess.KeyScanIncomplete
	// the expected raw host and container identifiers appear in no output either (the served
	// name is the declared model, exported on purpose)
	for _, v := range forbidden {
		hits, incomplete := keyHits(v, outputs)
		sess.KeyHits, sess.KeyScanIncomplete = append(sess.KeyHits, hits...), append(sess.KeyScanIncomplete, incomplete...)
	}
	summary["key_hits"], summary["key_scan_incomplete"] = sess.KeyHits, sess.KeyScanIncomplete
	// the series continues (macOS before Linux) only on a verified session, never on a test
	// PASS alone; the actual result is kept either way
	verr := sess.verdict()
	if c, ok := summary["capture_check"].(map[string]any); ok && c["state"] == "checked" {
		for _, k := range []string{"complete", "equal_snapshots", "host_ref_equal", "container_ref_equal", "served_model_ref_equal"} {
			if c[k] != true {
				verr = errors.Join(verr, fmt.Errorf("capture check %s failed", k))
			}
		}
	}
	summary["series_may_continue"], summary["verdict"] = verr == nil, fmt.Sprint(verr)
	b, _ = json.MarshalIndent(summary, "", "  ")
	liveOut(t, liveSummaryFile, b) // the actual summary; the runner's scan (TestLiveOutputScan) masks the same one field
	// the log shows the summary with the public field masked, so the go test log never holds the
	// declared model (and with it a key equal to it)
	t.Logf("summary (trial.agent.model as %q):\n%s", publicPlaceholder, maskPublic(b, []publicField{summaryPublic("", m.Model)}))
	if verr != nil {
		t.Fatalf("the session did not verify the chain; stop the series: %v", verr)
	}
}

// liveGate runs before any model or agent request: first the key preflight (a refusal fails
// the test), then the TCP check (unreachable skips it).
func liveGate(m *evals.Model) (refused, unreachable error) {
	if err := livePreflight(m); err != nil {
		return err, nil
	}
	return nil, liveReachable(m)
}

// livePreflight refuses, before any model or agent request, a key that the output scan could
// not tell from an identifier the session exports on purpose; the key may equal the model.
func livePreflight(m *evals.Model) error {
	return keyCollision(os.Getenv(m.KeyEnv), map[string]string{
		"model": m.Model, "base_url": m.BaseURL, "model_use": m.Use, "key_env": m.KeyEnv,
		"agent": "opencode", "agent_version": runner.DockerOpenCodeVersion,
		"runner": "hint-docker", "runner_version": runner.DockerRunnerSemver, "arm": "baseline",
	})
}

// liveSummaryFile is the stored session summary in HINT_LIVE_OUT.
const liveSummaryFile = "opencode-trial-summary.json"

// TestLiveOutputScan is the session runner's check after a session: the stored outputs and the
// go test log (HINT_LIVE_SCAN: files or directories, separated by the OS path list separator)
// with the same key scan and the same one public field (the stored summary's declared model,
// HINT_LIVE_MODEL) as the session itself; no request, no other exemption.
func TestLiveOutputScan(t *testing.T) {
	paths := os.Getenv("HINT_LIVE_SCAN")
	if paths == "" {
		t.Skip("set HINT_LIVE_SCAN")
	}
	key, model := os.Getenv("HINT_LOCAL_MODEL_KEY"), os.Getenv("HINT_LIVE_MODEL")
	if key == "" || model == "" {
		t.Fatal("set HINT_LOCAL_MODEL_KEY and HINT_LIVE_MODEL")
	}
	outputs, err := liveFiles(filepath.SplitList(paths))
	if err != nil {
		t.Fatal(err)
	}
	hits, incomplete := keyHits(key, outputs, summaryPublic(liveSummaryFile, model))
	t.Logf("files %d, key hits %v, incomplete %v", len(outputs), hits, incomplete)
	if hits != nil || incomplete != nil {
		t.Fatal("the key scan of the stored outputs is not clean")
	}
}

// liveFiles reads the named files (named by their base name) and every file in the named
// directories (named by their path in it). Anything that is not a regular file, and two files
// with one name, are refused: the scan reads everything or fails.
func liveFiles(paths []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	add := func(name, path string) error {
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", name)
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("%s: two files with this name", name)
		}
		b, err := os.ReadFile(path)
		out[name] = b
		return err
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			if err := add(filepath.Base(p), p); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(p, path)
			return add(filepath.ToSlash(rel), path)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// liveReachable is a TCP connect only (no request): an unreachable endpoint starts nothing.
func liveReachable(m *evals.Model) error {
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		return fmt.Errorf("the LAN endpoint is not reachable (TCP %s): 0 model requests", u.Host)
	}
	return c.Close()
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
