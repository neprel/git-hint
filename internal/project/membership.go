package project

// The membership producer (draft v0.2) records the HINT-orchestrated executions of one work unit
// per enrollment epoch in a local, append-only, hash-chained event spool and publishes a sealed
// epoch as one administrative MEMBERSHIP record of its own orchestrator source, in one atomic
// activity batch. It states claims only: a sealed epoch says which executions HINT enrolled,
// launched (a durable prestart intent) and finished; it never states quiescence, completion,
// success or usage. Every entry point takes the state lock once and reloads the unit journal,
// the epoch spool and the frozen policy from disk; nothing is cached between calls. A damaged
// spool is never repaired or skipped, the unit journal is never rolled back, and an enrolled
// execution is never forgotten. Only references, digests and enums are stored.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
)

// MembershipCoverage is the only coverage a membership policy states.
const MembershipCoverage = "hint_orchestrated_executions"

const (
	membershipDir      = "membership"
	maxMembershipBytes = 8 << 20
	maxPolicyDigests   = 16
)

// ErrMembership is returned for a refused membership operation.
var ErrMembership = errors.New("membership refused")

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	epochFile     = regexp.MustCompile(`^epoch-([1-9][0-9]{0,5})\.jsonl$`)
	// installedTargets are the targets the schema allows in a launch event.
	installedTargets = []string{"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl", "darwin-x64", "darwin-arm64"}
	// the exact fields of each event kind (the schema's MembershipEvent variants)
	eventFields = map[string][]string{
		"epoch_open": {"seq", "kind", "prev_digest", "epoch", "original_scope_digest", "enrollment_start_revision_digest", "policy_digest", "coverage",
			"predecessor_epoch_digest"},
		"enroll": {"seq", "kind", "prev_digest", "execution_ref", "run_ref", "attempt_ref", "task_id", "planned_configuration_digest"},
		"launch": {"seq", "kind", "prev_digest", "execution_ref", "launch_digest", "execution_digest", "installed_target", "model_ref", "gate_policy_digest",
			"source_configuration_digest", "prestart_checks", "launch_audit_ref"},
		"finish":     {"seq", "kind", "prev_digest", "execution_ref", "finish_state", "finish_audit_ref", "ledger_ref"},
		"epoch_seal": {"seq", "kind", "prev_digest", "frozen_open_revision_digest", "executions"},
	}
	finishAfterLaunch = []string{runner.AgentExited, runner.AgentExitedNonzero, runner.AgentStoppedAfterTimeout, runner.AgentStoppedAfterCancel,
		runner.AgentStoppedAfterWaitError, runner.AgentStopUnconfirmed, runner.AgentStartUnconfirmed}
)

// MembershipPolicy is the prospective policy of one epoch, frozen before its epoch_open as
// canonical bytes; policy_digest is its digest. Its pins are checked against the running build
// at every load.
type MembershipPolicy struct {
	Coverage             string   `json:"coverage"`
	Agent                string   `json:"agent"`
	ConfigurationDigests []string `json:"configuration_digests"` // allowed Configuration.Digest values
	LaunchDigests        []string `json:"launch_digests"`        // allowed frozen planned launch digests A
	StudyLaunchPolicy    string   `json:"study_launch_policy"`
	GatePolicyDigest     string   `json:"gate_policy_digest"`
	RunnerVersion        string   `json:"runner_version"`
	SchemaDigest         string   `json:"schema_digest"`
}

// MembershipSchemaDigest is the digest of the embedded v0.2 activity schema this build writes.
func MembershipSchemaDigest() string {
	b, err := githint.SchemasV02.ReadFile("schemas/v0.2/activity.schema.json")
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func sortedDigests(v []string) bool {
	if len(v) < 1 || len(v) > maxPolicyDigests {
		return false
	}
	for i, d := range v {
		if !digestPattern.MatchString(d) || i > 0 && v[i-1] >= d {
			return false
		}
	}
	return true
}

func (p MembershipPolicy) problem() error {
	switch {
	case p.Coverage != MembershipCoverage, p.Agent != "opencode":
		return fmt.Errorf("%w: the policy states another coverage or agent", ErrMembership)
	case !sortedDigests(p.ConfigurationDigests) || !sortedDigests(p.LaunchDigests):
		return fmt.Errorf("%w: the policy digest lists must be 1-%d sorted unique digests", ErrMembership, maxPolicyDigests)
	case p.StudyLaunchPolicy == "" || runner.StudyLaunchPolicy(p.StudyLaunchPolicy).Check() != nil:
		return fmt.Errorf("%w: the policy names no reviewed study launch policy", ErrMembership)
	case p.GatePolicyDigest != runner.GatePolicyDigest(), p.RunnerVersion != runner.DockerRunnerSemver, p.SchemaDigest != MembershipSchemaDigest():
		return fmt.Errorf("%w: the policy pins are not those of this build", ErrMembership)
	}
	return nil
}

// Execution is one enrolled execution (references only).
type Execution struct {
	ExecutionRef, RunRef, AttemptRef, TaskID, PlannedConfigurationDigest string
}

// Epoch is a handle of one epoch of one unit: the raw label (local only) and the epoch number.
// It holds no state; every operation reloads the journal, the spool and the policy.
type Epoch struct {
	label string
	n     int
}

// Number is the epoch number.
func (e *Epoch) Number() int { return e.n }

func membershipPath(s *store.Store, label string, name ...string) []string {
	return append([]string{"state", membershipDir, hint.DeriveID(s.Key(), "membership-file", label)}, name...)
}

func epochName(n int) string { return "epoch-" + strconv.Itoa(n) + ".jsonl" }

// spoolEvent is one decoded event with its canonical digest.
type spoolEvent struct {
	ev     map[string]any
	digest string
}

// epochView is the validated content of one epoch spool.
type epochView struct {
	events []spoolEvent
	open   map[string]any
	order  []string                  // executions in enrollment order
	exec   map[string]map[string]any // execution -> enroll event
	launch map[string]map[string]any // execution -> launch event
	final  map[string]map[string]any // execution -> finish event
	sealed bool
}

func (v *epochView) head() any {
	if len(v.events) == 0 {
		return nil
	}
	return v.events[len(v.events)-1].digest
}

func damaged(why string) error {
	return fmt.Errorf("%w: the membership spool is damaged (%s)", ErrMembership, why)
}

func evStr(ev map[string]any, k string) string { s, _ := ev[k].(string); return s }

func evInt(ev map[string]any, k string) (int, bool) {
	n, ok := ev[k].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(n.String())
	return i, err == nil
}

// readEpoch reads and validates one epoch spool: complete lines, unique keys, exact fields per
// kind, contiguous seq, the hash chain, the execution state table and the bounds. Any defect
// refuses; nothing is repaired or skipped. A missing file is fs.ErrNotExist.
func readEpoch(s *store.Store, label string, n int) (*epochView, error) {
	raw, err := readBounded(s, s.Path(membershipPath(s, label, epochName(n))...), maxMembershipBytes)
	if err != nil {
		return nil, err
	}
	return parseEpoch(raw)
}

func parseEpoch(raw []byte) (*epochView, error) {
	if len(raw) > maxMembershipBytes {
		return nil, damaged("too large")
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return nil, damaged("partial last line")
	}
	v := &epochView{exec: map[string]map[string]any{}, launch: map[string]map[string]any{}, final: map[string]map[string]any{}}
	runs := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), maxMembershipBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if !uniqueBatchJSON(line) {
			return nil, damaged("unreadable line")
		}
		var ev map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if dec.Decode(&ev) != nil {
			return nil, damaged("unreadable line")
		}
		kind := evStr(ev, "kind")
		fields, ok := eventFields[kind]
		if !ok || len(ev) != len(fields) {
			return nil, damaged("unknown event")
		}
		for _, k := range fields {
			if _, ok := ev[k]; !ok {
				return nil, damaged("unknown event")
			}
		}
		i := len(v.events)
		if seq, ok := evInt(ev, "seq"); !ok || seq != i+1 || i+1 > hint.MembershipMaxEvents {
			return nil, damaged("sequence")
		}
		if ev["prev_digest"] != v.head() {
			return nil, damaged("chain")
		}
		d, err := hint.Digest(ev)
		if err != nil {
			return nil, damaged("chain")
		}
		if v.sealed || (kind == "epoch_open") != (i == 0) {
			return nil, damaged("order")
		}
		x := evStr(ev, "execution_ref")
		switch kind {
		case "epoch_open":
			v.open = ev
		case "enroll":
			if v.exec[x] != nil || runs[evStr(ev, "run_ref")] || len(v.order) >= hint.MembershipMaxExecutions {
				return nil, damaged("enrollment")
			}
			v.exec[x], runs[evStr(ev, "run_ref")] = ev, true
			v.order = append(v.order, x)
		case "launch":
			if v.exec[x] == nil || v.launch[x] != nil || v.final[x] != nil {
				return nil, damaged("launch")
			}
			v.launch[x] = ev
		case "finish":
			f := evStr(ev, "finish_state")
			ok := v.exec[x] != nil && v.final[x] == nil
			if v.launch[x] != nil {
				ok = ok && slices.Contains(finishAfterLaunch, f)
			} else {
				ok = ok && (f == runner.AgentRefusedBeforeLaunch || f == runner.AgentStartUnconfirmed)
			}
			if !ok {
				return nil, damaged("finish")
			}
			v.final[x] = ev
		case "epoch_seal":
			if c, ok := evInt(ev, "executions"); !ok || c != len(v.order) || len(v.final) != len(v.order) {
				return nil, damaged("seal")
			}
			v.sealed = true
		}
		v.events = append(v.events, spoolEvent{ev, d})
	}
	if sc.Err() != nil || v.open == nil {
		return nil, damaged("unreadable")
	}
	return v, nil
}

// appendEvent is the one writer of a spool: it fills seq and prev_digest from the reloaded view,
// appends one line and syncs the file (and the directory when the file is new).
func appendEvent(s *store.Store, label string, n int, v *epochView, ev map[string]any) error {
	ev["seq"], ev["prev_digest"] = len(v.events)+1, v.head()
	if len(v.events)+1 > hint.MembershipMaxEvents {
		return fmt.Errorf("%w: the epoch reached its event bound", ErrMembership)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	p := s.Path(membershipPath(s, label, epochName(n))...)
	if err := s.CheckPath(p); err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if len(v.events) == 0 {
		if d, err := os.Open(dir); err == nil {
			err = d.Sync()
			d.Close()
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	d, err := hint.Digest(plainJSON(ev))
	if err != nil {
		return err
	}
	v.events = append(v.events, spoolEvent{plainJSON(ev), d})
	return nil
}

// plainJSON is the decoded form of a value (numbers as json.Number), the form the spool reader
// sees.
func plainJSON(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// writeImmutable writes canonical bytes once; existing equal bytes are a replay, other bytes
// refuse.
func writeImmutable(s *store.Store, data []byte, elem ...string) error {
	old, err := os.ReadFile(s.Path(elem...))
	if err == nil {
		if bytes.Equal(old, data) {
			return nil
		}
		return fmt.Errorf("%w: an immutable membership file has other bytes", ErrMembership)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.WriteState(data, elem...)
}

func readImmutable(s *store.Store, elem ...string) ([]byte, error) {
	return readBounded(s, s.Path(elem...), maxMembershipBytes)
}

func policyName(n int) string { return "policy-" + strconv.Itoa(n) + ".json" }

// loadPolicy reloads the frozen policy of an epoch: canonical bytes, its digest equal to the
// epoch_open policy_digest, and pins equal to this build.
func loadPolicy(s *store.Store, label string, n int, v *epochView) (MembershipPolicy, error) {
	var p MembershipPolicy
	raw, err := readImmutable(s, membershipPath(s, label, policyName(n))...)
	if err != nil {
		return p, fmt.Errorf("%w: the frozen policy is not readable", ErrMembership)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return p, damaged("policy")
	}
	c, err := hint.Canonical(p)
	if err != nil || !bytes.Equal(c, raw) {
		return p, damaged("policy bytes")
	}
	d, err := hint.Digest(p)
	if err != nil || evStr(v.open, "policy_digest") != d {
		return p, damaged("policy digest")
	}
	return p, p.problem()
}

func configName(digest string) string {
	return "configuration-" + strings.TrimPrefix(digest, "sha256:") + ".json"
}

// loadConfiguration reloads an enrolled configuration by its planned digest.
func loadConfiguration(s *store.Store, label, digest string) (activity.Configuration, error) {
	var c activity.Configuration
	raw, err := readImmutable(s, membershipPath(s, label, configName(digest))...)
	if err != nil {
		return c, fmt.Errorf("%w: the enrolled configuration is not readable", ErrMembership)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.Decode(new(any)) != io.EOF || c.Digest() != digest {
		return c, damaged("configuration")
	}
	return c, nil
}

// epochs lists the epoch numbers of a unit that have a spool, in order; they must be 1..k.
func epochs(s *store.Store, label string) ([]int, error) {
	entries, err := os.ReadDir(s.Path(membershipPath(s, label)...))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ns []int
	for _, e := range entries {
		if m := epochFile.FindStringSubmatch(e.Name()); m != nil {
			if !e.Type().IsRegular() {
				return nil, damaged("spool file type")
			}
			n, _ := strconv.Atoi(m[1])
			ns = append(ns, n)
		}
	}
	slices.Sort(ns)
	for i, n := range ns {
		if n != i+1 {
			return nil, damaged("epoch numbering")
		}
	}
	return ns, nil
}

// latestRevision is the unit's latest journal revision (scope and revision digests).
func latestRevision(u *WorkUnit) (scope, revision string, err error) {
	if len(u.Revisions) == 0 {
		return "", "", fmt.Errorf("%w: the unit has no journal revision", ErrMembership)
	}
	last := u.Revisions[len(u.Revisions)-1]
	scope, _ = last["scope_digest"].(string)
	revision, _ = last["revision_digest"].(string)
	if !digestPattern.MatchString(scope) || !digestPattern.MatchString(revision) {
		return "", "", fmt.Errorf("%w: the unit journal has no digests", ErrMembership)
	}
	return scope, revision, nil
}

// readBounded reads one store file of at most limit bytes (a larger file refuses).
func readBounded(s *store.Store, p string, limit int64) ([]byte, error) {
	if err := s.CheckPath(p); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%w: a store file exceeds its bound", ErrMembership)
	}
	return b, err
}

// maxControlledRunBytes bounds one stored controlled run read for the ownership index.
const maxControlledRunBytes = 64 << 20

// refOwner is one record that names a run or execution reference.
type refOwner struct {
	kind     string // spool, membership, ledger, observation, controlled
	scope    string // spool: membership directory; membership: unit_ref
	epoch    int
	exec     string // the execution an enrollment names
	binding  activity.LedgerBinding
	behavior *behaviorEvidence // observation: the root CLI behavior evidence, when it is one
}

// behaviorEvidence is what a run-correlated observation proves about its owner: a root CLI
// adapter observation of a consistent root CLI source with the source's configuration digest and
// the observation's task, family and run.
type behaviorEvidence struct {
	configuration, task, family, run string
}

// cliRootEvidence is the behavior evidence of an observation, or nil unless the observation and
// its source pass the shared root CLI source and observation rules and the source declares its
// configuration.
func cliRootEvidence(o activity.Observation, sources map[string]activity.Source) *behaviorEvidence {
	src, ok := sources[o.SourceID]
	if !ok || src.Adapter.Name != hint.CLIRootAdapter || src.Configuration == nil {
		return nil
	}
	var ps, po map[string]any
	bs, _ := json.Marshal(src)
	bo, _ := json.Marshal(o)
	if json.Unmarshal(bs, &ps) != nil || json.Unmarshal(bo, &po) != nil || hint.SourceCLIRootProblem(ps) || hint.ObservationCLIRootProblem(po) {
		return nil
	}
	return &behaviorEvidence{configuration: src.Configuration.Digest(), task: o.Correlation["task_id"], family: o.Correlation["task_family_id"], run: o.Correlation["run_ref"]}
}

// refOwners indexes every record of the store that names a run ("run:") or execution
// ("exec:") reference: the enrollments of every epoch spool of every unit, the ledger bindings,
// run correlations and membership enrollments of the activity spool and committed batches, and
// the trials of the stored controlled runs. Every read is bounded; a damaged source refuses and
// nothing is skipped.
func refOwners(s *store.Store) (map[string][]refOwner, error) {
	owners := map[string][]refOwner{}
	add := func(ref string, o refOwner) { owners[ref] = append(owners[ref], o) }
	root := s.Path("state", membershipDir)
	dirs, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() || !batchID.MatchString(d.Name()) {
			return nil, damaged("membership directory")
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			m := epochFile.FindStringSubmatch(f.Name())
			if m == nil {
				continue
			}
			if !f.Type().IsRegular() {
				return nil, damaged("spool file type")
			}
			raw, err := readBounded(s, filepath.Join(root, d.Name(), f.Name()), maxMembershipBytes)
			if err != nil {
				return nil, err
			}
			v, err := parseEpoch(raw)
			if err != nil {
				return nil, err
			}
			n, _ := strconv.Atoi(m[1])
			for x, ev := range v.exec {
				o := refOwner{kind: "spool", scope: d.Name(), epoch: n, exec: x}
				add("exec:"+x, o)
				add("run:"+evStr(ev, "run_ref"), o)
			}
		}
	}
	obs, damage, err := ReadSpoolChecked(s)
	if err != nil {
		return nil, err
	}
	if damage.Any() {
		return nil, fmt.Errorf("%w: the activity spool is damaged", ErrMembership)
	}
	sources, err := Sources(s)
	if err != nil {
		return nil, err
	}
	for _, o := range obs {
		if l := o.ModelLedger; l != nil {
			add("run:"+l.Binding.RunRef, refOwner{kind: "ledger", binding: l.Binding})
			if l.Binding.ExecutionRef != "" {
				add("exec:"+l.Binding.ExecutionRef, refOwner{kind: "ledger", binding: l.Binding})
			}
		}
		// a ledger observation's run correlation mirrors its binding; any other is an owner
		if r := o.Correlation["run_ref"]; r != "" && (o.ModelLedger == nil || o.ModelLedger.Binding.RunRef != r) {
			add("run:"+r, refOwner{kind: "observation", behavior: cliRootEvidence(o, sources)})
		}
		if m := o.Membership; m != nil && len(m.Events) > 0 {
			n, _ := m.Events[0]["epoch"].(float64)
			for _, ev := range m.Events {
				if ev["kind"] == "enroll" {
					x, _ := ev["execution_ref"].(string)
					r, _ := ev["run_ref"].(string)
					own := refOwner{kind: "membership", scope: m.UnitRef, epoch: int(n), exec: x}
					add("exec:"+x, own)
					add("run:"+r, own)
				}
			}
		}
	}
	work := s.Path("state", "evals", "work")
	files, err := os.ReadDir(work)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, f := range files {
		if filepath.Ext(f.Name()) != ".json" {
			continue
		}
		raw, err := readBounded(s, filepath.Join(work, f.Name()), maxControlledRunBytes)
		if err != nil {
			return nil, err
		}
		var run struct {
			Units []struct {
				Trials []string `json:"trials"`
			} `json:"units"`
		}
		if json.Unmarshal(raw, &run) != nil {
			return nil, fmt.Errorf("%w: a stored controlled run is not readable", ErrMembership)
		}
		for _, u := range run.Units {
			for _, t := range u.Trials {
				add("run:"+activity.RunRef(s.Key(), t), refOwner{kind: "controlled"})
			}
		}
	}
	return owners, nil
}

// foreignOwner reports whether a record other than the execution's own enrollment, published
// membership, own project ledger or own root CLI behavior names its run or execution reference
// (a later controlled run, ledger, other correlation or another enrollment poisons the execution).
// Own behavior is a root CLI observation of a consistent source whose configuration digest is
// the planned one, with the enrolled task and run and the unit's family.
func (e *Epoch) foreignOwner(s *store.Store, owners map[string][]refOwner, u *WorkUnit, enroll map[string]any) bool {
	unitRef := u.UnitRef
	family, _ := u.Scope["family_ref"].(string)
	x := evStr(enroll, "execution_ref")
	dir := hint.DeriveID(s.Key(), "membership-file", e.label)
	want := activity.LedgerBinding{RunRef: evStr(enroll, "run_ref"), TaskID: evStr(enroll, "task_id"), ConfigurationDigest: evStr(enroll, "planned_configuration_digest"),
		UnitRef: unitRef, AttemptRef: evStr(enroll, "attempt_ref"), Origin: "project", ExecutionRef: x}
	for _, ref := range []string{"exec:" + x, "run:" + want.RunRef} {
		for _, o := range owners[ref] {
			b := o.binding
			b.TaskFamilyID = ""
			ownBehavior := behaviorEvidence{configuration: want.ConfigurationDigest, task: want.TaskID, family: family, run: want.RunRef}
			own := o.kind == "spool" && o.scope == dir && o.epoch == e.n && o.exec == x ||
				o.kind == "membership" && o.scope == unitRef && o.epoch == e.n && o.exec == x ||
				o.kind == "ledger" && b == want ||
				o.kind == "observation" && o.behavior != nil && family != "" && *o.behavior == ownBehavior
			if !own {
				return true
			}
		}
	}
	return false
}

// memberAttempt is the attempt WorkJoin puts a new task in: the open attempt, or the next one.
func memberAttempt(s *store.Store, u *WorkUnit) string {
	if n := len(u.Attempts); n > 0 && u.Attempts[n-1].Outcome == "open" {
		return u.Attempts[n-1].Ref
	}
	return hint.EvalRef(s.Key(), "attempt", u.Label, strconv.Itoa(len(u.Attempts)+1))
}

// taskOfOtherUnit reports whether a task is a member task of another unit (WorkJoin's rule).
func taskOfOtherUnit(s *store.Store, u *WorkUnit, taskID string) (bool, error) {
	all, err := WorkUnits(s)
	if err != nil {
		return false, err
	}
	for _, o := range all {
		if o.UnitRef == u.UnitRef {
			continue
		}
		for _, a := range o.Attempts {
			if slices.Contains(a.Tasks, taskID) {
				return true, nil
			}
		}
	}
	return false, nil
}

// lockedEpoch is the reloaded state every entry point works on.
type lockedEpoch struct {
	unit   *WorkUnit
	view   *epochView
	policy MembershipPolicy
}

// reload loads the unit, the epoch spool and its frozen policy (under the caller's lock).
func (e *Epoch) reload(s *store.Store) (*lockedEpoch, error) {
	u, err := LoadWork(s, e.label)
	if err != nil {
		return nil, err
	}
	v, err := readEpoch(s, e.label, e.n)
	if err != nil {
		return nil, err
	}
	if n, _ := evInt(v.open, "epoch"); n != e.n || evStr(v.open, "coverage") != MembershipCoverage {
		return nil, damaged("epoch number")
	}
	p, err := loadPolicy(s, e.label, e.n, v)
	if err != nil {
		return nil, err
	}
	return &lockedEpoch{u, v, p}, nil
}

// EpochOpen freezes the policy and opens the next epoch of an open unit: no other epoch of the
// unit may be open, and a later epoch names the digest of the whole previous sealed membership
// record.
func EpochOpen(s *store.Store, label string, p MembershipPolicy, now time.Time) (*Epoch, error) {
	if err := p.problem(); err != nil {
		return nil, err
	}
	lock, err := s.LockState(true)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	u, err := LoadWork(s, label)
	if err != nil {
		return nil, err
	}
	if u.State != "open" {
		return nil, fmt.Errorf("%w: the work unit is not open", ErrMembership)
	}
	ns, err := epochs(s, label)
	if err != nil {
		return nil, err
	}
	n := len(ns) + 1
	var predecessor any
	if n > 1 {
		prev, err := sealedMembership(s, label, n-1)
		if err != nil {
			return nil, err
		}
		if predecessor, err = hint.Digest(prev); err != nil {
			return nil, err
		}
	}
	scope, revision, err := latestRevision(u)
	if err != nil {
		return nil, err
	}
	c, err := hint.Canonical(p)
	if err != nil {
		return nil, err
	}
	if err := writeImmutable(s, c, membershipPath(s, label, policyName(n))...); err != nil {
		return nil, err
	}
	d, err := hint.Digest(p)
	if err != nil {
		return nil, err
	}
	ev := map[string]any{"kind": "epoch_open", "epoch": n, "original_scope_digest": scope, "enrollment_start_revision_digest": revision,
		"policy_digest": d, "coverage": MembershipCoverage, "predecessor_epoch_digest": predecessor}
	if err := appendEvent(s, label, n, &epochView{}, ev); err != nil {
		return nil, err
	}
	return &Epoch{label: label, n: n}, nil
}

// Enroll enrolls one execution of the unit's open attempt: checks first (nothing written), then
// the durable enroll event, then WorkJoin. A WorkJoin failure after the enroll event keeps the
// execution a member and closes it refused_before_launch; the journal is never rolled back.
func (e *Epoch) Enroll(s *store.Store, task, run string, cfg activity.Configuration, now time.Time) (Execution, error) {
	if !ValidTaskID(task) || !ValidTaskID(run) {
		return Execution{}, fmt.Errorf("%w: unusable task or run name", ErrMembership)
	}
	lock, err := s.LockState(true)
	if err != nil {
		return Execution{}, err
	}
	defer lock.Unlock()
	le, err := e.reload(s)
	if err != nil {
		return Execution{}, err
	}
	key := s.Key()
	x := Execution{ExecutionRef: hint.EvalRef(key, "execution", e.label, run), RunRef: activity.RunRef(key, run), TaskID: TaskID(key, task),
		PlannedConfigurationDigest: cfg.Digest()}
	switch {
	case le.view.sealed:
		return Execution{}, fmt.Errorf("%w: the epoch is sealed", ErrMembership)
	case le.unit.State != "open":
		return Execution{}, fmt.Errorf("%w: the work unit is not open", ErrMembership)
	case cfg.Agent.Name != le.policy.Agent || !slices.Contains(le.policy.ConfigurationDigests, x.PlannedConfigurationDigest):
		return Execution{}, fmt.Errorf("%w: the configuration is not allowed by the epoch policy", ErrMembership)
	}
	// the bound must leave room for this execution's launch and finish, every open execution's
	// remaining events and the seal
	need := len(le.view.events) + 3 + 1
	for _, o := range le.view.order {
		if le.view.final[o] == nil {
			need += 1
			if le.view.launch[o] == nil {
				need++
			}
		}
	}
	if need > hint.MembershipMaxEvents || len(le.view.order)+1 > hint.MembershipMaxExecutions {
		return Execution{}, fmt.Errorf("%w: the epoch reached its bounds", ErrMembership)
	}
	owners, err := refOwners(s)
	if err != nil {
		return Execution{}, err
	}
	if len(owners["run:"+x.RunRef]) > 0 || len(owners["exec:"+x.ExecutionRef]) > 0 {
		return Execution{}, fmt.Errorf("%w: the run or execution exists already", ErrMembership)
	}
	if other, err := taskOfOtherUnit(s, le.unit, x.TaskID); err != nil || other {
		return Execution{}, fmt.Errorf("%w: the task belongs to another work unit", ErrMembership)
	}
	x.AttemptRef = memberAttempt(s, le.unit)
	c, err := hint.Canonical(cfg)
	if err != nil {
		return Execution{}, err
	}
	if err := writeImmutable(s, c, membershipPath(s, e.label, configName(x.PlannedConfigurationDigest))...); err != nil {
		return Execution{}, err
	}
	ev := map[string]any{"kind": "enroll", "execution_ref": x.ExecutionRef, "run_ref": x.RunRef, "attempt_ref": x.AttemptRef, "task_id": x.TaskID,
		"planned_configuration_digest": x.PlannedConfigurationDigest}
	if err := appendEvent(s, e.label, e.n, le.view, ev); err != nil {
		return Execution{}, err
	}
	joinErr := WorkJoin(s, e.label, x.TaskID, now)
	if joinErr == nil {
		u, err := LoadWork(s, e.label)
		if err != nil {
			joinErr = err
		} else if a := u.Attempts[len(u.Attempts)-1]; a.Ref != x.AttemptRef || !slices.Contains(a.Tasks, x.TaskID) {
			joinErr = errors.New("the task did not join the enrolled attempt")
		}
	}
	if joinErr != nil {
		if err := e.finishLocked(s, le.view, x.ExecutionRef, runner.AgentRefusedBeforeLaunch, nil); err != nil {
			return Execution{}, fmt.Errorf("%w: the enrolled execution could not join (%v) and stays open for recovery (%v)", ErrMembership, joinErr, err)
		}
		return Execution{}, fmt.Errorf("%w: the enrolled execution could not join and is closed refused_before_launch (%v)", ErrMembership, joinErr)
	}
	return x, nil
}

// LaunchHook is the runner's BeforeAgentStart for one enrolled execution. Under the lock it
// reloads the unit, the spool and the policy, refuses unless the unit and the enrolled attempt
// are still open with the task, the execution is enrolled and not launched, and the actual launch
// facts pair with the enrolled configuration (its launch reference and model reference) and the
// policy; then it appends the launch event (a durable prestart intent).
func (e *Epoch) LaunchHook(s *store.Store, x Execution) func(runner.LaunchFacts) error {
	return func(f runner.LaunchFacts) error {
		lock, err := s.LockState(true)
		if err != nil {
			return err
		}
		defer lock.Unlock()
		le, err := e.reload(s)
		if err != nil {
			return err
		}
		enroll := le.view.exec[x.ExecutionRef]
		switch {
		case le.view.sealed || enroll == nil || le.view.launch[x.ExecutionRef] != nil || le.view.final[x.ExecutionRef] != nil:
			return fmt.Errorf("%w: the execution is not enrolled and waiting for its launch", ErrMembership)
		case evStr(enroll, "run_ref") != x.RunRef || evStr(enroll, "attempt_ref") != x.AttemptRef || evStr(enroll, "task_id") != x.TaskID ||
			evStr(enroll, "planned_configuration_digest") != x.PlannedConfigurationDigest:
			return fmt.Errorf("%w: the execution is not the enrolled one", ErrMembership)
		case le.unit.State != "open" || len(le.unit.Attempts) == 0:
			return fmt.Errorf("%w: the work unit is not open", ErrMembership)
		}
		if a := le.unit.Attempts[len(le.unit.Attempts)-1]; a.Outcome != "open" || a.Ref != x.AttemptRef || !slices.Contains(a.Tasks, x.TaskID) {
			return fmt.Errorf("%w: the enrolled attempt is not the open attempt with the task", ErrMembership)
		}
		owners, err := refOwners(s)
		if err != nil {
			return err
		}
		if e.foreignOwner(s, owners, le.unit, enroll) {
			return fmt.Errorf("%w: another record owns the execution's run or execution reference", ErrMembership)
		}
		cfg, err := loadConfiguration(s, e.label, x.PlannedConfigurationDigest)
		if err != nil {
			return err
		}
		key := s.Key()
		launchRef := activity.ConfigRef(key, "setting_override", f.LaunchDigest)
		switch {
		case !slices.Contains(le.policy.LaunchDigests, f.LaunchDigest):
			return fmt.Errorf("%w: the actual launch digest is not allowed by the policy", ErrMembership)
		case cfg.ConfigRefs.State != "known" || !slices.Equal(cfg.ConfigRefs.Refs, []string{launchRef}):
			return fmt.Errorf("%w: the actual launch is not the enrolled configuration's launch", ErrMembership)
		case cfg.Model.State != "known" || cfg.Model.Ref != activity.ModelRef(key, f.Model):
			return fmt.Errorf("%w: the actual model is not the enrolled configuration's model", ErrMembership)
		case f.GatePolicyDigest != le.policy.GatePolicyDigest || f.GatePolicyDigest != runner.GatePolicyDigest():
			return fmt.Errorf("%w: the actual gate policy is not the policy's", ErrMembership)
		case !slices.Contains(installedTargets, f.Target) || !digestPattern.MatchString(f.ExecutionDigest):
			return fmt.Errorf("%w: the actual target or execution digest is not usable", ErrMembership)
		}
		ev := map[string]any{"kind": "launch", "execution_ref": x.ExecutionRef, "launch_digest": f.LaunchDigest, "execution_digest": f.ExecutionDigest,
			"installed_target": f.Target, "model_ref": cfg.Model.Ref, "gate_policy_digest": f.GatePolicyDigest, "source_configuration_digest": x.PlannedConfigurationDigest,
			"prestart_checks":  map[string]any{"target_matches": true, "model_matches": true, "policy_matches": true},
			"launch_audit_ref": hint.DeriveID(key, "membership-launch-audit", x.ExecutionRef)}
		return appendEvent(s, e.label, e.n, le.view, ev)
	}
}

func (e *Epoch) finishLocked(s *store.Store, v *epochView, x, state string, ledgerRef any) error {
	ev := map[string]any{"kind": "finish", "execution_ref": x, "finish_state": state,
		"finish_audit_ref": hint.DeriveID(s.Key(), "membership-finish-audit", x), "ledger_ref": ledgerRef}
	return appendEvent(s, e.label, e.n, v, ev)
}

// Finish records the terminal state of an execution from its written project ledger (or nil).
// The state is the ledger's agent-run outcome when it fits the spool; otherwise the weakest
// state: start_unconfirmed after a launch event, refused_before_launch without one (the hook never
// ran, so no start was requested). A ledger bound to another execution refuses.
func (e *Epoch) Finish(s *store.Store, x Execution, ledger *activity.ModelLedger) error {
	lock, err := s.LockState(true)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	le, err := e.reload(s)
	if err != nil {
		return err
	}
	if le.view.sealed || le.view.exec[x.ExecutionRef] == nil || le.view.final[x.ExecutionRef] != nil {
		return fmt.Errorf("%w: the execution is not enrolled and open", ErrMembership)
	}
	launch := le.view.launch[x.ExecutionRef]
	enroll := le.view.exec[x.ExecutionRef]
	state := runner.AgentRefusedBeforeLaunch
	if launch != nil {
		state = runner.AgentStartUnconfirmed
	}
	var ledgerRef any
	if ledger != nil {
		want := activity.LedgerBinding{RunRef: evStr(enroll, "run_ref"), TaskID: evStr(enroll, "task_id"), TaskFamilyID: ledger.Binding.TaskFamilyID,
			ConfigurationDigest: evStr(enroll, "planned_configuration_digest"), UnitRef: le.unit.UnitRef, AttemptRef: evStr(enroll, "attempt_ref"),
			Origin: "project", ExecutionRef: x.ExecutionRef}
		if ledger.Binding != want || !writtenLedger(s, ledger) {
			return fmt.Errorf("%w: the ledger is not the written ledger of this execution", ErrMembership)
		}
		ledgerRef = ledger.LedgerRef
		if r := ledger.AgentRun; r != nil {
			switch {
			case launch == nil && r.Outcome == runner.AgentRefusedBeforeLaunch:
				state = r.Outcome
			case launch == nil:
				state = runner.AgentStartUnconfirmed
			case slices.Contains(finishAfterLaunch, r.Outcome) && r.ExecutionDigest == evStr(launch, "execution_digest"):
				state = r.Outcome
			}
		}
	}
	return e.finishLocked(s, le.view, x.ExecutionRef, state, ledgerRef)
}

// writtenLedger reports whether the ledger revision is in the activity spool with this body.
func writtenLedger(s *store.Store, l *activity.ModelLedger) bool {
	obs, _, err := ReadSpool(s)
	if err != nil {
		return false
	}
	for _, o := range obs {
		if o.ModelLedger != nil && o.ModelLedger.LedgerRef == l.LedgerRef && o.ModelLedger.Revision == l.Revision {
			return equalJSON(o.ModelLedger, l)
		}
	}
	return false
}

// ResumeEpoch reopens the latest epoch of a unit after a crash. Under the lock it closes every
// enrolled execution without a terminal: refused_before_launch without a launch event (no start
// was requested), start_unconfirmed with one (a prestart intent proves no start). It never infers
// another state, never relaunches and never removes a member task.
func ResumeEpoch(s *store.Store, label string) (*Epoch, error) {
	lock, err := s.LockState(true)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	ns, err := epochs(s, label)
	if err != nil {
		return nil, err
	}
	if len(ns) == 0 {
		return nil, fmt.Errorf("%w: the unit has no epoch", ErrMembership)
	}
	e := &Epoch{label: label, n: len(ns)}
	le, err := e.reload(s)
	if err != nil {
		return nil, err
	}
	for _, x := range le.view.order {
		if le.view.final[x] != nil {
			continue
		}
		state := runner.AgentRefusedBeforeLaunch
		if le.view.launch[x] != nil {
			state = runner.AgentStartUnconfirmed
		}
		if err := e.finishLocked(s, le.view, x, state, nil); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func sealedName(n int) string { return "sealed-" + strconv.Itoa(n) + ".json" }

// sealedBatch is the persisted, deterministic batch of a sealed epoch.
type sealedBatch struct {
	Source       activity.Source        `json:"source"`
	Observations []activity.Observation `json:"observations"`
}

// sealedMembership is the published membership body of a sealed epoch (from its persisted batch,
// checked against its spool).
func sealedMembership(s *store.Store, label string, n int) (map[string]any, error) {
	v, err := readEpoch(s, label, n)
	if err != nil {
		return nil, err
	}
	b, err := loadSealed(s, label, n, v)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("%w: the previous epoch is not sealed and published", ErrMembership)
	}
	return membershipBody(b.Observations[0].Membership), nil
}

func membershipBody(m *activity.Membership) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// loadSealed reloads the persisted batch of an epoch (nil when there is none) and checks that its
// membership is exactly the sealed spool.
func loadSealed(s *store.Store, label string, n int, v *epochView) (*sealedBatch, error) {
	raw, err := readImmutable(s, membershipPath(s, label, sealedName(n))...)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b sealedBatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if !uniqueBatchJSON(raw) || dec.Decode(&b) != nil || dec.Decode(new(any)) != io.EOF || len(b.Observations) != 1 || b.Observations[0].Membership == nil {
		return nil, damaged("sealed batch")
	}
	if c, err := hint.Canonical(b); err != nil || !bytes.Equal(c, raw) {
		return nil, damaged("sealed batch bytes")
	}
	if !v.sealed || !equalJSON(membershipBody(b.Observations[0].Membership), spoolBody(b.Observations[0].Membership.UnitRef, v)) {
		return nil, damaged("sealed batch differs from the spool")
	}
	return &b, nil
}

func spoolBody(unit string, v *epochView) map[string]any {
	events := []any{}
	for _, e := range v.events {
		events = append(events, e.ev)
	}
	b, _ := json.Marshal(map[string]any{"unit_ref": unit, "events": events, "head_digest": v.head()})
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// orchestratorSource is the administrative source of one epoch of one unit.
func orchestratorSource(key []byte, unit string, n int) activity.Source {
	return activity.Source{SourceID: hint.DeriveID(key, "source", hint.OrchestratorAdapter, unit, strconv.Itoa(n)), Agent: "other",
		Adapter:      activity.Adapter{Name: hint.OrchestratorAdapter, Version: hint.OrchestratorAdapterVersion},
		Capabilities: []string{"membership"}, Unavailable: hint.OrchestratorUnavailable()}
}

// Seal closes the epoch administratively and publishes it. It needs every enrolled execution
// terminal, every enrolled attempt still open (the seal precedes any artifact verification) and
// every listed ledger written; it appends epoch_seal with the latest journal revision, persists
// the deterministic batch bytes, then commits the batch. A replay (also after a crash or an hour
// boundary) republishes the persisted bytes unchanged. A seal states no quiescence, completion or
// usage: unconfirmed, refused and incomplete executions stay unknown.
func (e *Epoch) Seal(s *store.Store, now time.Time) (string, error) {
	lock, err := s.LockState(true)
	if err != nil {
		return "", err
	}
	defer lock.Unlock()
	le, err := e.reload(s)
	if err != nil {
		return "", err
	}
	key := s.Key()
	owners, err := refOwners(s)
	if err != nil {
		return "", err
	}
	for _, x := range le.view.order {
		if e.foreignOwner(s, owners, le.unit, le.view.exec[x]) {
			return "", fmt.Errorf("%w: another record owns an execution's run or execution reference; the epoch is not published", ErrMembership)
		}
	}
	if le.view.sealed {
		b, err := loadSealed(s, e.label, e.n, le.view)
		if err != nil {
			return "", err
		}
		if b != nil {
			return b.Observations[0].ObservationID, writeActivityBatchLocked(s, b.Source, b.Observations)
		}
	} else {
		for _, x := range le.view.order {
			if le.view.final[x] == nil {
				return "", fmt.Errorf("%w: an enrolled execution has no terminal finish", ErrMembership)
			}
			ref := evStr(le.view.exec[x], "attempt_ref")
			open := false
			for _, a := range le.unit.Attempts {
				open = open || a.Ref == ref && a.Outcome == "open"
			}
			if !open {
				return "", fmt.Errorf("%w: an enrolled attempt was closed before the seal", ErrMembership)
			}
			if l, ok := le.view.final[x]["ledger_ref"].(string); ok && !ledgerInSpool(s, l) {
				return "", fmt.Errorf("%w: a listed ledger is not written", ErrMembership)
			}
		}
		_, revision, err := latestRevision(le.unit)
		if err != nil {
			return "", err
		}
		if err := appendEvent(s, e.label, e.n, le.view, map[string]any{"kind": "epoch_seal", "frozen_open_revision_digest": revision, "executions": len(le.view.order)}); err != nil {
			return "", err
		}
		le.view.sealed = true
	}
	// the spool is sealed and no batch is persisted yet: build, persist, publish
	body := spoolBody(le.unit.UnitRef, le.view)
	if hint.MembershipProblem(body) {
		return "", damaged("sealed membership")
	}
	var m activity.Membership
	b, _ := json.Marshal(body)
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	src := orchestratorSource(key, le.unit.UnitRef, e.n)
	at := activity.Timestamp(now)
	o := activity.Observation{ObservationID: hint.DeriveID(key, "membership", le.unit.UnitRef, strconv.Itoa(e.n)), Class: "observation", Op: "MEMBERSHIP",
		Source: "orchestrator", SourceID: src.SourceID, Precision: "exact", Time: activity.Time{Start: &at, End: &at, Clock: "collector"},
		Outcome: activity.Outcome{Status: "success"}, Membership: &m, Missing: []string{},
		Provenance: activity.Provenance{CapturedAt: at, SourceEventID: hint.DeriveID(key, "membership-event", le.unit.UnitRef, strconv.Itoa(e.n)),
			Transformations: []activity.Transform{{Step: "normalize", Tool: "hint-cli", Version: "0.1.0"}}}}
	batch := sealedBatch{Source: src, Observations: []activity.Observation{o}}
	c, err := hint.Canonical(batch)
	if err != nil {
		return "", err
	}
	if err := writeImmutable(s, c, membershipPath(s, e.label, sealedName(e.n))...); err != nil {
		return "", err
	}
	persisted, err := loadSealed(s, e.label, e.n, le.view)
	if err != nil || persisted == nil {
		return "", damaged("persisted sealed batch")
	}
	return o.ObservationID, writeActivityBatchLocked(s, persisted.Source, persisted.Observations)
}

func ledgerInSpool(s *store.Store, ref string) bool {
	obs, _, err := ReadSpool(s)
	if err != nil {
		return false
	}
	for _, o := range obs {
		if o.ModelLedger != nil && o.ModelLedger.LedgerRef == ref {
			return true
		}
	}
	return false
}
