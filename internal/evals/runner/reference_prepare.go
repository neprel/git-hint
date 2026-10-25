package runner

// Reference preparation is deliberately not enrolled in a production policy. It is a
// runner primitive, not project/source authority or a successful-work measurement.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const ReferencePrepareVersion = "hint-b1-reference-prepare-v0.1.0"

const referenceTARDomain = "hint-b1-frozen-final-tar-v1"

var errReferencePrepare = errors.New("reference_preparation_unknown")

// ReferenceInputIdentity pins the actual exported B1 task. These are byte domains,
// not qualification, source authority, project identity or a caller trust assertion.
type ReferenceInputIdentity struct{ SpecDigest, InputsDigest, ReferenceDigest, EnvironmentDigest string }
type ReferencePrepareOptions struct {
	OutputDir string
	Expected  ReferenceInputIdentity
	policy    *referencePreparationPolicy // no exported constructor or activation path
}
type ReferenceArtifact struct {
	Domain, Digest, File string
	Size                 int64
}
type ReferencePhaseAudit struct {
	CPUsNano, MemoryBytes, PidsLimit                                    int64
	Role, ContainerRef, ImageID, CommandDigest                          string
	WorkEndMS, StopEndMS, RemoveEndMS, JoinEndMS, StartedMS, FinishedMS int64
	ExitStatus                                                          int64
	Started, WaitObserved, Exited, NetworkNone, Removed, StreamsJoined  bool
}
type ReferenceCaseAudit struct {
	CaseIndex, Repetition           int
	InputDigest, OutputDigest, File string
	Size                            int
	Status                          int64
	Complete, NetworkNone           bool
}
type ReferencePrepareAudit struct {
	// Installed policy selection is a separate domain from the original procedure PolicyDigest.
	InstalledPolicy                                                                             ReferencePreparationPolicyRef
	PolicyBundleDigest, PolicyCatalogueDigest, PolicyEndpointDigest, PolicySocketIdentityDigest string
	Inputs                                                                                      ReferenceInputIdentity
	BaseImageRefs                                                                               []string
	SetupDigest, SetupState                                                                     string
	Interface                                                                                   string
	OracleRuns                                                                                  int
	Version, Authority                                                                          string
	State, InputDigest, PolicyDigest, ScheduleDigest                                            string
	EngineRef, EngineVersion, EngineOS, EngineArchitecture                                      string
	ImageID, ImageOS, ImageArchitecture, ImageVariant, BuildContextDigest                       string
	FinalTAR                                                                                    ReferenceArtifact
	Phases                                                                                      []ReferencePhaseAudit
	Cases                                                                                       []ReferenceCaseAudit
	OracleOutputsDigest                                                                         string
	FailureCodes                                                                                []string
}

// The unexported policy is available only to a future reviewed in-package resolver.
// Today callers cannot enable this API. Tests exercise the real path with a private value.
type referencePreparationPolicy struct {
	identity                                                    string
	operation, resolve, commit, build, apply, overhead, cleanup time.Duration
	archiveCap, totalCap                                        int64
}
type referencePhaseKey struct{}
type referencePhase struct {
	parent                                       context.Context
	workEnd, stopEnd, removeEnd, joinEnd, anchor time.Time
	archiveCap                                   int64
	referenceDigest                              string
	artifactDigest                               string
	audit                                        ReferencePhaseAudit
	cid                                          string
	err                                          error
	removed                                      bool
	cleaning                                     bool
	work                                         context.Context
	cleanup                                      func()
}

func referencePhaseFrom(ctx context.Context) *referencePhase {
	p, _ := ctx.Value(referencePhaseKey{}).(*referencePhase)
	return p
}
func referenceDigest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func referenceJSON(v any) string { b, _ := json.Marshal(v); return referenceDigest(b) }
func (p *referencePhase) fail(e error) {
	if e != nil {
		p.err = errReferencePrepare
	}
}
func (p *referencePhase) deadline(end time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(p.parent, end)
}

// ensureCleanup is the single owner, shared by early join failure and the outer
// phase finalizer. Its callback is installed only by actual successful container creation.
func (p *referencePhase) ensureCleanup() {
	if p.cleanup != nil && !p.removed {
		p.cleaning = true
		p.cleanup()
	}
}
func (p *referencePhase) join(ch <-chan error) error {
	accept := func(e error, ok bool) error {
		if !ok || !time.Now().Before(p.joinEnd) {
			p.fail(errReferencePrepare)
			return errReferencePrepare
		}
		p.fail(e)
		return e
	}
	if !time.Now().Before(p.joinEnd) {
		p.fail(errReferencePrepare)
		p.ensureCleanup()
		return errReferencePrepare
	}
	// A cancelled/expired work window cannot defer cleanup even for a ready worker.
	if (p.work != nil && p.work.Err() != nil) || (!p.workEnd.IsZero() && !time.Now().Before(p.workEnd)) {
		p.fail(errReferencePrepare)
		p.ensureCleanup()
		final := time.NewTimer(time.Until(p.joinEnd))
		defer final.Stop()
		select {
		case <-ch:
		case <-final.C:
		}
		return errReferencePrepare
	}
	// A completed worker needs no wait. This does not reclassify earlier work failures.
	select {
	case e, ok := <-ch:
		return accept(e, ok)
	default:
	}
	var workDone <-chan struct{}
	if p.work != nil {
		workDone = p.work.Done()
	}
	workEnd := p.workEnd
	if workEnd.IsZero() {
		workEnd = p.joinEnd
	}
	timer := time.NewTimer(time.Until(workEnd))
	defer timer.Stop()
	select {
	case e, ok := <-ch:
		return accept(e, ok)
	case <-workDone:
	case <-timer.C:
	}
	// Never spend the later join allowance before the original stop/remove slots.
	p.fail(errReferencePrepare)
	p.ensureCleanup()
	final := time.NewTimer(time.Until(p.joinEnd))
	defer final.Stop()
	select {
	case <-ch:
	case <-final.C:
	}
	return errReferencePrepare
}
func streamEnd(ctx context.Context, legacy time.Duration) <-chan time.Time {
	if p := referencePhaseFrom(ctx); p != nil {
		return time.After(time.Until(p.workEnd))
	}
	return time.After(legacy)
}
func inspectionContext(ctx context.Context) context.Context {
	if referencePhaseFrom(ctx) != nil {
		return ctx
	}
	return context.Background()
}
func closeForPhase(ctx context.Context, c io.Closer) {
	e := c.Close()
	if p := referencePhaseFrom(ctx); p != nil {
		p.fail(e)
	}
}
func (d *Docker) removeForPhase(ctx context.Context, id string) {
	p := referencePhaseFrom(ctx)
	if p == nil {
		d.remove(id)
		return
	}
	// The operation owns cleanup after work/result validation, not an inner defer.
	// create's refusal path also retains the ID for this single outer cleanup owner.
	if !p.cleaning {
		return
	}
	if p.removed {
		return
	}
	p.removed = true
	p.cid = id
	// Cleanup uses only endpoints allocated before work. Cancellation never renews them.
	c, cancel := p.deadline(p.stopEnd)
	ins, e := d.cli.ContainerInspect(c, id, client.ContainerInspectOptions{})
	if e == nil && ins.Container.State != nil {
		if ins.Container.State.Running {
			seconds := 1
			_, e = d.cli.ContainerStop(c, id, client.ContainerStopOptions{Timeout: &seconds})
			if e != nil {
				p.fail(e)
				_, _ = d.cli.ContainerKill(c, id, client.ContainerKillOptions{})
			}
			ins, e = d.cli.ContainerInspect(c, id, client.ContainerInspectOptions{})
		}
		if e == nil && ins.Container.State != nil && !ins.Container.State.Running {
			p.audit.Exited = true
			h := ins.Container.HostConfig
			if h != nil {
				p.audit.CPUsNano = h.NanoCPUs
				p.audit.MemoryBytes = h.Memory
				if h.PidsLimit != nil {
					p.audit.PidsLimit = *h.PidsLimit
				}
			}
			if p.audit.CPUsNano != 2e9 || p.audit.MemoryBytes != 2048<<20 || p.audit.PidsLimit != 1024 {
				p.fail(errReferencePrepare)
			}
			if ins.Container.HostConfig == nil || noHostAccess(ins.Container.HostConfig) != nil || ins.Container.HostConfig.NetworkMode != "none" || (ins.Container.NetworkSettings != nil && !onlyNone(ins.Container.NetworkSettings.Networks)) {
				p.fail(errReferencePrepare)
			} else {
				p.audit.NetworkNone = true
			}
			if ins.Container.Config == nil || referenceJSON(ins.Container.Config.Cmd) != p.audit.CommandDigest || ins.Container.Image != p.audit.ImageID {
				p.fail(errReferencePrepare)
			}
			p.audit.ExitStatus = int64(ins.Container.State.ExitCode)
			if ins.Container.State.ExitCode != 0 {
				p.fail(errReferencePrepare)
			}
		} else {
			p.fail(errReferencePrepare)
		}
	} else {
		p.fail(errReferencePrepare)
	}
	if time.Now().After(p.stopEnd) {
		p.fail(errReferencePrepare)
	}
	cancel()
	c, cancel = p.deadline(p.removeEnd)
	_, e = d.cli.ContainerRemove(c, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	p.fail(e)
	_, e = d.cli.ContainerInspect(c, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(e) && time.Now().Before(p.removeEnd) {
		p.audit.Removed = true
	} else {
		p.fail(errReferencePrepare)
	}
	cancel()
}

type referenceReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *referenceReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}

type referenceOutput struct {
	w    io.Writer
	left int64
}

func (w *referenceOutput) Write(b []byte) (int, error) {
	if int64(len(b)) > w.left {
		return 0, errReferencePrepare
	}
	n, e := w.w.Write(b)
	w.left -= int64(n)
	return n, e
}

// PrepareReference applies one stored reference and checks that exact retained TAR with
// the existing two-run B1 expectation recipe. Default options refuse before engine access.
// Returned audits own their slices. A failed or late operation never has a successful state.
func (d *Docker) PrepareReference(ctx context.Context, taskDir string, o ReferencePrepareOptions) (a ReferencePrepareAudit, err error) {
	anchor := time.Now()
	a.Version = ReferencePrepareVersion
	a.Authority = "unknown"
	a.State = "unknown"
	fail := func(code string) (ReferencePrepareAudit, error) {
		a.FailureCodes = append(a.FailureCodes, code)
		return a, errReferencePrepare
	}
	p := o.policy
	if p == nil {
		return fail("policy_disabled")
	}
	if p.identity == "" || p.operation <= 0 || p.operation > 1800*time.Second || p.resolve <= 0 || p.resolve > 30*time.Second || p.commit <= 0 || p.commit > 30*time.Second || p.build <= 0 || p.build > 600*time.Second || p.apply <= 0 || p.apply > 300*time.Second || p.cleanup <= 0 || p.cleanup > 30*time.Second || p.overhead < 0 || p.overhead > 10*time.Second || p.archiveCap <= 0 || p.archiveCap > 96<<20 || p.totalCap < p.archiveCap || p.totalCap > 256<<20 {
		return fail("policy_bounds")
	}
	end := anchor.Add(p.operation)
	if dl, ok := ctx.Deadline(); ok && dl.Before(end) {
		end = dl
	}
	// This independent context has one original absolute deadline, not a fresh grace period.
	cleanup, cleanupCancel := context.WithDeadline(context.WithoutCancel(ctx), end)
	defer cleanupCancel()
	if ctx.Err() != nil || !time.Now().Before(end) {
		return fail("cancelled")
	}
	t, got, code := inspectReferencePreparationInputs(ctx, taskDir)
	if code != "" {
		return fail(code)
	}
	var e error
	if got != o.Expected || !time.Now().Before(anchor.Add(p.resolve)) || ctx.Err() != nil {
		return fail("input_mismatch_or_late")
	}
	lim, e := readLimits(taskDir)
	if e != nil || lim.lan || lim.cpus != 2 || lim.memoryMB != 2048 {
		return fail("runtime_limits")
	}
	// Preserve actual B1 case limits; no model or candidate phase is scheduled.
	count := len(t.spec.Cases) * OracleRuns
	needed := p.resolve + p.build + p.apply + time.Duration(count)*(time.Duration(t.spec.CaseSeconds)*time.Second+p.overhead) + time.Duration(count+2)*p.cleanup + p.commit
	if needed > p.operation || anchor.Add(needed).After(end) {
		return fail("schedule_capacity")
	}
	if !filepath.IsAbs(o.OutputDir) || filepath.Clean(o.OutputDir) != o.OutputDir || !filepath.IsAbs(taskDir) || o.OutputDir == taskDir || strings.HasPrefix(o.OutputDir, taskDir+string(filepath.Separator)) {
		return fail("output_path")
	}
	if e = os.Mkdir(o.OutputDir, 0700); e != nil {
		return fail("output_not_fresh")
	}
	if e = os.Mkdir(filepath.Join(o.OutputDir, "case-outputs"), 0700); e != nil {
		return fail("output_not_fresh")
	}
	a.Inputs = got
	a.Interface = IOInterface
	a.OracleRuns = OracleRuns
	bases, e := pinnedFrom(filepath.Join(taskDir, "environment", "Dockerfile"))
	if e != nil {
		return fail("base_input")
	}
	for _, base := range bases {
		a.BaseImageRefs = append(a.BaseImageRefs, referenceDigest([]byte(base)))
	}
	setup, e := os.ReadFile(filepath.Join(taskDir, "environment", "setup.sh"))
	if os.IsNotExist(e) {
		a.SetupState = "absent"
	} else if e != nil {
		return fail("setup_input")
	} else {
		a.SetupState = "present"
		a.SetupDigest = referenceDigest(setup)
	}
	a.InputDigest = referenceJSON(got)
	a.PolicyDigest = referenceJSON(struct {
		Identity  string
		Durations []time.Duration
		Caps      []int64
	}{p.identity, []time.Duration{p.operation, p.resolve, p.commit, p.build, p.apply, p.overhead, p.cleanup}, []int64{p.archiveCap, p.totalCap}})
	cursor := anchor.Add(p.resolve)
	var schedule [][4]int64
	durations := []time.Duration{p.build, p.apply}
	for k := 0; k < count; k++ {
		durations = append(durations, time.Duration(t.spec.CaseSeconds)*time.Second+p.overhead)
	}
	for _, duration := range durations {
		work := cursor.Add(duration)
		stop := work.Add(p.cleanup / 3)
		remove := work.Add(2 * p.cleanup / 3)
		join := work.Add(p.cleanup)
		cursor = join
		schedule = append(schedule, [4]int64{work.Sub(anchor).Milliseconds(), stop.Sub(anchor).Milliseconds(), remove.Sub(anchor).Milliseconds(), join.Sub(anchor).Milliseconds()})
	}
	a.ScheduleDigest = referenceJSON(schedule)
	phaseIndex := 0
	used := int64(0)
	store := func(name string, b []byte) error {
		if ctx.Err() != nil || !time.Now().Before(end.Add(-p.commit)) || used+int64(len(b)) > p.totalCap {
			return errReferencePrepare
		}
		f, e := os.OpenFile(filepath.Join(o.OutputDir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, we := f.Write(b)
		ce := f.Close()
		if we != nil {
			return we
		}
		if ce != nil {
			return ce
		}
		used += int64(len(b))
		return nil
	}
	phase := func(role string, duration time.Duration, fn func(context.Context, *referencePhase) error) error {
		if phaseIndex >= len(schedule) {
			return errReferencePrepare
		}
		window := schedule[phaseIndex]
		phaseIndex++
		audit, er := runBoundedPhase(ctx, cleanup, anchor, window, duration, p.cleanup, p.archiveCap, role, fn)
		if audit != nil {
			a.ScheduleDigest = referenceJSON(schedule)
			a.Phases = append(a.Phases, *audit)
		}
		return er
	}

	var img string
	e = phase("build", p.build, func(c context.Context, x *referencePhase) error {
		info, e := d.cli.Info(c, client.InfoOptions{})
		if e != nil {
			return e
		}
		a.EngineRef = referenceDigest([]byte(info.Info.ID))
		a.EngineOS = info.Info.OSType
		a.EngineArchitecture = info.Info.Architecture
		a.EngineVersion = info.Info.ServerVersion
		if b := d.referenceBinding; b != nil && (a.EngineRef != b.EngineRef || a.EngineVersion != b.EngineVersion || a.EngineOS != b.EngineOS || a.EngineArchitecture != b.EngineArchitecture) {
			return errReferencePrepare
		}
		if info.Info.ID == "" || a.EngineOS != "linux" || a.EngineArchitecture == "" || a.EngineVersion == "" {
			return errReferencePrepare
		}
		id, _, digest, e := d.build(c, filepath.Join(taskDir, "environment"), "agent")
		if e != nil {
			return e
		}
		if digest != got.EnvironmentDigest {
			return errReferencePrepare
		}
		im, e := d.resolveImage(c, id)
		if e != nil {
			return e
		}
		engineArch := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}[a.EngineArchitecture]
		if im.OS != "linux" || im.Arch == "" || engineArch == "" || im.Arch != engineArch {
			return errReferencePrepare
		}
		a.ImageID = im.ID
		a.ImageOS = im.OS
		a.ImageArchitecture = im.Arch
		a.ImageVariant = im.Variant
		a.BuildContextDigest = digest
		img = im.ID
		x.audit.ImageID = img
		return nil
	})
	if e != nil {
		return fail("build_unknown")
	}
	artifact := filepath.Join(o.OutputDir, "final.tar")
	e = phase("apply_reference", p.apply, func(c context.Context, x *referencePhase) error {
		x.referenceDigest = got.ReferenceDigest
		lim.agent = p.apply
		digest, e := d.applyPhase(c, filepath.Join(taskDir, "tests", "io", "reference"), "oracle", lim, img, artifact)
		if e != nil {
			return e
		}
		st, e := os.Stat(artifact)
		if e != nil || st.Size() > p.archiveCap {
			return errReferencePrepare
		}
		if log, le := os.Stat(filepath.Join(o.OutputDir, "oracle.txt")); le != nil {
			return errReferencePrepare
		} else {
			used += log.Size()
		}
		used += st.Size()
		if used > p.totalCap {
			return errReferencePrepare
		}
		a.FinalTAR = ReferenceArtifact{referenceTARDomain, digest, "final.tar", st.Size()}
		return nil
	})
	if e != nil {
		return fail("application_unknown")
	}
	index := map[string]int{}
	for i, id := range t.spec.Cases {
		index[id] = i
	}
	_, outputs, allNone, e := referenceExpectations(t, func(id string, k int) (r caseRun, er error) {
		er = phase("reference_case", time.Duration(t.spec.CaseSeconds)*time.Second+p.overhead, func(c context.Context, x *referencePhase) error {
			var e error
			x.artifactDigest = a.FinalTAR.Digest
			r, e = d.runCase(c, img, artifact, t.spec.Entry, t.inputs[id], time.Duration(t.spec.CaseSeconds)*time.Second, lim)
			if e != nil {
				return e
			}
			x.audit.NetworkNone = r.networkNone
			x.audit.StreamsJoined = true
			if !r.usable() || !r.networkNone || r.status != 0 {
				return errReferencePrepare
			}
			name := filepath.Join("case-outputs", fmtCase(index[id], k))
			if e = store(name, r.out); e != nil {
				return e
			}
			a.Cases = append(a.Cases, ReferenceCaseAudit{index[id], k, referenceDigest(t.inputs[id]), referenceDigest(r.out), name, len(r.out), r.status, r.usable(), r.networkNone})
			return nil
		})
		return r, er
	})
	if e != nil || !allNone {
		return fail("oracle_unknown")
	}
	a.OracleOutputsDigest = outputs
	a.ScheduleDigest = referenceJSON(schedule)
	if ctx.Err() != nil || !time.Now().Before(end.Add(-p.commit)) {
		return fail("late_completion")
	}
	a.State = "prepared_reference_observed"
	return a, nil
}
func fmtCase(i, k int) string { return strconv.Itoa(i) + "-" + strconv.Itoa(k) + ".stdout" }

// Transfer the retained artifact through the existing retree implementation. Its digest
// covers the same bounded descriptor bytes sent to the container, including TAR trailer.
// A mismatch fails before the staged container can execute a case.
func referenceRetree(ctx context.Context, path, expected, prefix string, w io.Writer, cap int64) (err error) {
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() || st.Size() > cap {
		return errReferencePrepare
	}
	f, e := os.Open(path)
	if e != nil {
		return errReferencePrepare
	}
	defer func() {
		if e := f.Close(); e != nil {
			err = errReferencePrepare
		}
	}()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(st, opened) {
		return errReferencePrepare
	}
	hash := sha256.New()
	bounded := io.LimitReader(&referenceReader{ctx: ctx, r: f}, cap+1)
	source := io.TeeReader(bounded, hash)
	if e = retreeFrom(source, prefix, w); e != nil {
		return errReferencePrepare
	}
	if _, e = io.Copy(io.Discard, source); e != nil {
		return errReferencePrepare
	}
	after, e := f.Stat()
	if e != nil || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() {
		return errReferencePrepare
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expected || ctx.Err() != nil {
		return errReferencePrepare
	}
	return nil
}

// InspectReferencePreparationInputs describes actual exported task bytes. It grants
// no qualification, source, procedure, origin or execution authority.
func InspectReferencePreparationInputs(ctx context.Context, taskDir string) (ReferenceInputIdentity, error) {
	_, id, code := inspectReferencePreparationInputs(ctx, taskDir)
	if code != "" {
		return ReferenceInputIdentity{}, referencePolicyFailure(code)
	}
	return id, nil
}

// The one existing input identity algorithm used by the inspector and preparation.
func inspectReferencePreparationInputs(ctx context.Context, taskDir string) (*ioTask, ReferenceInputIdentity, string) {
	zero := ReferenceInputIdentity{}
	if ctx == nil || ctx.Err() != nil {
		return nil, zero, "input_context"
	}
	t, e := readIOTask(taskDir)
	if e != nil || t == nil {
		return nil, zero, "b1_input"
	}
	ref, e := tarDir(filepath.Join(taskDir, "tests", "io", "reference"), "solution")
	if e != nil {
		return nil, zero, "reference_input"
	}
	env, e := tarDir(filepath.Join(taskDir, "environment"), "")
	if e != nil {
		return nil, zero, "environment_input"
	}
	if ctx.Err() != nil {
		return nil, zero, "input_context"
	}
	return t, ReferenceInputIdentity{t.specDigest, t.inputsDigest, referenceDigest(ref), referenceDigest(env)}, ""
}
