package runner

// Internal offline machinery only. No production caller issues this policy and
// no public path/handle, catalogue, completion or producer authority is added.
import (
	"context"
	"path/filepath"
	"time"

	"github.com/neprel/git-hint/internal/evals/iodecl"
)

type frozenVerificationIssuer struct{ nonzero byte }

var frozenVerificationProcess = &frozenVerificationIssuer{1}

type frozenVerificationPolicy struct {
	issuer *frozenVerificationIssuer
	image  string
}
type frozenIOArtifact struct{ path, digest, image string }
type frozenIORequest struct {
	reference, candidate frozenIOArtifact
	output               string
}
type frozenIOAudit struct {
	State, Authority, ReferenceDigest, CandidateDigest, ScheduleDigest string
	Phases                                                             []ReferencePhaseAudit
}

// verifyFrozenIO has no exported constructor or call path. Factual artifact hashes
// and this private testable execution policy establish no origin or result trust.
// The already parsed declaration and output directory belong to the runner.
func (d *Docker) verifyFrozenIO(parent context.Context, t *ioTask, lim taskLimits, req frozenIORequest, p *frozenVerificationPolicy) (float64, map[string]any, frozenIOAudit, error) {
	anchor := time.Now()
	a := frozenIOAudit{State: "unknown", Authority: "unknown"}
	refuse := func() (float64, map[string]any, frozenIOAudit, error) {
		return 0, nil, a, errIOVerdict{"frozen verification unavailable or incomplete"}
	}
	// Policy and original bounds precede file or engine IO. Preparation's policy
	// has a different private type and cannot enable this procedure.
	if parent == nil || parent.Err() != nil || p == nil || p.issuer != frozenVerificationProcess || !referencePolicySHA.MatchString(p.image) || d == nil || d.cli == nil || t == nil || len(t.spec.Cases) < 1 || len(t.spec.Cases) > maxIOCases || lim.verifier <= 0 || lim.lan || lim.cpus != 2 || lim.memoryMB != 2048 {
		return refuse()
	}
	for _, f := range []frozenIOArtifact{req.reference, req.candidate} {
		if f.image != p.image || !referencePolicySHA.MatchString(f.digest) || !filepath.IsAbs(f.path) || filepath.Clean(f.path) != f.path {
			return refuse()
		}
	}
	if !filepath.IsAbs(req.output) || filepath.Clean(req.output) != req.output {
		return refuse()
	}
	end := anchor.Add(1800 * time.Second)
	if v := anchor.Add(lim.verifier); v.Before(end) {
		end = v
	}
	if v, ok := parent.Deadline(); ok && v.Before(end) {
		end = v
	}
	const resolve = 30 * time.Second
	const final = 30 * time.Second
	const overhead = 10 * time.Second
	const cleanupDuration = 30 * time.Second
	const archiveCap = int64(96 << 20)
	duration := time.Duration(t.spec.CaseSeconds)*time.Second + overhead
	if t.spec.CaseSeconds < 1 || t.spec.CaseSeconds > iodecl.MaxCaseSeconds {
		return refuse()
	}
	count := len(t.spec.Cases) * (OracleRuns + 1)
	needed := resolve + time.Duration(count)*(duration+cleanupDuration) + final
	if anchor.Add(needed).After(end) {
		return refuse()
	}
	ctx, cancel := context.WithDeadline(parent, end)
	defer cancel()
	cleanup, cc := context.WithDeadline(context.WithoutCancel(parent), end)
	defer cc()
	cursor := anchor.Add(resolve)
	schedule := make([][4]int64, 0, count)
	for i := 0; i < count; i++ {
		work := cursor.Add(duration)
		stop := work.Add(cleanupDuration / 3)
		remove := work.Add(2 * cleanupDuration / 3)
		join := work.Add(cleanupDuration)
		schedule = append(schedule, [4]int64{work.Sub(anchor).Milliseconds(), stop.Sub(anchor).Milliseconds(), remove.Sub(anchor).Milliseconds(), join.Sub(anchor).Milliseconds()})
		cursor = join
	}
	a.ReferenceDigest, a.CandidateDigest = req.reference.digest, req.candidate.digest
	a.ScheduleDigest = referenceJSON(schedule)
	index := 0
	prov := map[string]any{"interface": IOInterface, "spec": t.specDigest, "inputs": t.inputsDigest, "oracle_runs": OracleRuns, "verdict_scope": "equivalence_to_reference_on_declared_cases", "reference_tree": req.reference.digest}
	reward, prov, e := compareFrozenIO(ctx, t, req.output, prov, true, func(oracle bool, id string, repetition int) (r caseRun, er error) {
		if index >= len(schedule) {
			return r, errReferencePrepare
		}
		f, role := req.candidate, "candidate_case"
		if oracle {
			f, role = req.reference, "reference_case"
		}
		window := schedule[index]
		index++
		audit, er := runBoundedPhase(ctx, cleanup, anchor, window, duration, cleanupDuration, archiveCap, role, func(c context.Context, x *referencePhase) error {
			x.artifactDigest = f.digest
			var e error
			r, e = d.runCase(c, p.image, f.path, t.spec.Entry, t.inputs[id], time.Duration(t.spec.CaseSeconds)*time.Second, lim)
			if e != nil {
				return e
			}
			x.audit.NetworkNone = r.networkNone
			x.audit.StreamsJoined = true
			if !r.usable() || !r.networkNone {
				return errReferencePrepare
			}
			return nil
		})
		if audit != nil {
			a.Phases = append(a.Phases, *audit)
		}
		return r, er
	})
	if e != nil || ctx.Err() != nil || !time.Now().Before(end) || index != count {
		return 0, prov, a, errIOVerdict{"frozen verification incomplete"}
	}
	if reward == 1 {
		a.State = "finite_equivalence_observed"
	} else {
		a.State = "finite_difference_observed"
	}
	return reward, prov, a, nil
}
