// Exact stored input consistency only; never runtime or prospective-start authority.
package qualify

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/iodecl"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
)

const ResolveMetadataMax int64 = 1 << 20
const ResolveArchiveMax int64 = 32 << 20
const ResolveTotalMax int64 = 128 << 20
const ResolveTimeout = 30 * time.Second

// ResolveIORequest names exact immutable versions. Never a latest index or caller trust flag.
type ResolveIORequest struct{ EvalName, ManifestDigest, PreviousManifestDigest string }
type ResolveRefusal struct{ Reason string }

func (r *ResolveRefusal) Error() string { return r.Reason }

// IOInputIdentity has no completion, project intent, capture, or attestation field.
type IOInputIdentity struct {
	State, EvalName, ManifestDigest, PreviousName, PreviousManifestDigest string
	VerifierArchive, SpecDigest, InputsDigest, ReferencePatchArchive      string
	NegativeControlTree, NegativeControlArchive, ReferenceResultTree      string
	Image, SetupDigest, Workdir, RunnerVersion, QualificationPolicy       string
	MandatoryChecksDigest, MandatoryTestsDigest                           string
	MandatoryChecks, MandatoryTests, QualificationArchives                []string
	QualificationReferenceRuns, VerificationOracleRuns, CaseSeconds       int
}

// ResolvedQualifiedIO is immutable to callers. Returned values/bytes are deep copies.
type ResolvedQualifiedIO struct {
	identity IOInputIdentity
	objects  map[string][]byte
}

func (h *ResolvedQualifiedIO) Identity() IOInputIdentity {
	b, _ := json.Marshal(h.identity)
	var v IOInputIdentity
	_ = json.Unmarshal(b, &v)
	return v
}
func (h *ResolvedQualifiedIO) Object(digest string) ([]byte, bool) {
	b, ok := h.objects[digest]
	return append([]byte(nil), b...), ok
}

type resolutionRead struct {
	ctx     context.Context
	l       *library.Library
	used    int64
	objects map[string][]byte
}

func (r *resolutionRead) read(kind, id string, cap int64) ([]byte, error) {
	if kind == "object" {
		if b, ok := r.objects[id]; ok {
			if int64(len(b)) > cap {
				return nil, errors.New("cached_object_limit")
			}
			return b, nil
		}
	}
	if r.used >= ResolveTotalMax {
		return nil, errors.New("aggregate_limit")
	}
	if cap > ResolveTotalMax-r.used {
		cap = ResolveTotalMax - r.used
	}
	b, e := r.l.ResolveBoundedRead(r.ctx, kind, id, cap)
	if e != nil {
		return nil, e
	}
	r.used += int64(len(b))
	if kind == "object" {
		r.objects[id] = b
	}
	return b, nil
}
func fail(s string) (*ResolvedQualifiedIO, *ResolveRefusal) { return nil, &ResolveRefusal{Reason: s} }
func obj(v any) map[string]any                              { m, _ := v.(map[string]any); return m }
func str(v any) string                                      { s, _ := v.(string); return s }
func array(v any) []any                                     { s, _ := v.([]any); return s }

var exactEval = regexp.MustCompile(`^ev-[0-9a-f]{16}@[1-9][0-9]{0,8}$`)
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ResolveQualifiedIO resolves exact stored q3 inputs. Reported trust is never attestation.
// The input handle does not authorize execution, project closure, capture, or economic use.
func ResolveQualifiedIO(parent context.Context, l *library.Library, request ResolveIORequest) (*ResolvedQualifiedIO, *ResolveRefusal) {
	if parent == nil {
		return fail("resolve_context_required")
	}
	ctx, cancel := context.WithTimeout(parent, ResolveTimeout)
	defer cancel()
	if l == nil || !exactEval.MatchString(request.EvalName) || !digestRE.MatchString(request.ManifestDigest) || !digestRE.MatchString(request.PreviousManifestDigest) {
		return fail("exact_definition_required")
	}
	r := &resolutionRead{ctx: ctx, l: l, objects: map[string][]byte{}}
	raw, e := r.read("eval", request.EvalName, ResolveMetadataMax)
	if e != nil {
		return fail("definition_unavailable_or_limit")
	}
	if library.Digest(raw) != request.ManifestDigest || evals.Validate("eval", raw) != nil || strictJSON(raw) != nil {
		return fail("definition_invalid")
	}
	var rec map[string]any
	_ = json.Unmarshal(raw, &rec)
	if fmt.Sprintf("%s@%.0f", rec["eval_id"], rec["version"]) != request.EvalName || rec["state"] != "validated" || rec["override"] != nil || len(array(rec["reasons"])) != 0 {
		return fail("qualification_not_validated")
	}
	prevName := str(rec["previous"])
	if !exactEval.MatchString(prevName) {
		return fail("qualification_predecessor_missing")
	}
	prior, e := r.read("eval", prevName, ResolveMetadataMax)
	if e != nil || library.Digest(prior) != request.PreviousManifestDigest || evals.Validate("eval", prior) != nil || strictJSON(prior) != nil {
		return fail("qualification_predecessor_invalid")
	}
	var prev map[string]any
	_ = json.Unmarshal(prior, &prev)
	if str(prev["eval_id"]) != str(rec["eval_id"]) || prev["version"].(float64)+1 != rec["version"].(float64) || prev["state"] != "packaged" {
		return fail("qualification_predecessor_invalid")
	}
	semantic := func(m map[string]any) map[string]any {
		n := map[string]any{}
		for k, v := range m {
			switch k {
			case "version", "previous", "created_at", "state", "reasons", "qualification":
			default:
				n[k] = v
			}
		}
		return n
	}
	if !reflect.DeepEqual(semantic(prev), semantic(rec)) {
		return fail("qualification_content_changed")
	}
	q := obj(rec["qualification"])
	if q["policy"] != Policy || q["result_trust"] != runner.TrustProcessSeparated {
		return fail("qualification_trust_unknown")
	}
	checks := map[string]map[string]any{}
	for _, v := range array(q["checks"]) {
		c := obj(v)
		name := str(c["name"])
		if checks[name] != nil {
			return fail("qualification_checks_ambiguous")
		}
		checks[name] = c
	}
	required := []string{"leak_probe", "base_fails", "reference_passes", "repeatable", "near_miss_fails", "requirement_coverage", "forgery_probe", "isolation"}
	archiveIDs := map[string]bool{}
	for _, name := range required {
		c := checks[name]
		if c == nil || c["status"] != "pass" || c["runs"].(float64) < 1 {
			return fail("qualification_control_missing")
		}
		for _, d := range array(c["evidence"]) {
			archiveIDs[str(d)] = true
		}
		if len(array(c["evidence"])) != 1 {
			return fail("qualification_audit_missing")
		}
	}
	if checks["leak_probe"]["runs"] != float64(1) || checks["base_fails"]["runs"] != float64(1) || checks["reference_passes"]["runs"] != float64(ReferenceRuns) || checks["repeatable"]["runs"] != float64(ReferenceRuns) || checks["forgery_probe"]["runs"] != float64(1) || checks["isolation"]["runs"] != float64(1) || len(archiveIDs) != 1 {
		return fail("qualification_control_counts")
	}
	v := obj(rec["verifier"])
	env := obj(rec["environment"])
	repos := array(rec["repositories"])
	if v["kind"] != iodecl.Interface || len(repos) != 1 || env["workdir"] != "/app" || env["test_command"] != "tests/io/spec.json" || !strings.Contains(str(env["image"]), "@sha256:") {
		return fail("unsupported_verifier")
	}
	repo := obj(repos[0])
	identity := IOInputIdentity{State: "resolved_inputs", EvalName: request.EvalName, ManifestDigest: request.ManifestDigest, PreviousName: prevName, PreviousManifestDigest: request.PreviousManifestDigest, VerifierArchive: str(v["archive"]), ReferencePatchArchive: str(rec["reference"]), NegativeControlTree: str(repo["base_tree"]), NegativeControlArchive: str(repo["base_archive"]), ReferenceResultTree: str(repo["result_tree"]), Image: str(env["image"]), SetupDigest: str(env["setup"]), Workdir: str(env["workdir"]), RunnerVersion: runner.DockerRunnerVersion, QualificationPolicy: Policy, QualificationReferenceRuns: ReferenceRuns, VerificationOracleRuns: runner.OracleRuns}
	// Read every referenced object from the same bounded descriptor, including tree/file objects.
	refs := map[string]bool{}
	for _, d := range []string{str(rec["instruction"]), identity.SetupDigest, identity.VerifierArchive, identity.ReferencePatchArchive, identity.NegativeControlArchive, identity.NegativeControlTree, identity.ReferenceResultTree} {
		if !digestRE.MatchString(d) {
			return fail("definition_object_identity_missing")
		}
		refs[d] = true
	}
	for _, req := range array(rec["requirements"]) {
		refs[str(obj(req)["text"])] = true
	}
	for d := range archiveIDs {
		delete(refs, d)
	}
	for d := range refs {
		cap := ResolveTotalMax
		if d == identity.NegativeControlTree || d == identity.ReferenceResultTree {
			cap = ResolveMetadataMax
		}
		if _, e = r.read("object", d, cap); e != nil {
			return fail("definition_object_unavailable_or_limit")
		}
	}
	vb := r.objects[identity.VerifierArchive]
	files, e := safeTar(ctx, vb, ResolveTotalMax)
	if e != nil {
		return fail("verifier_archive_invalid")
	}
	spec, ok := files["io/spec.json"]
	if !ok {
		return fail("io_spec_missing")
	}
	inputs := map[string][]byte{}
	for name, b := range files {
		if name == "io/spec.json" {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "io/inputs/"), ".in")
		if name != "io/inputs/"+id+".in" {
			return fail("io_archive_extra_file")
		}
		inputs[id] = b
	}
	decl, e := iodecl.Parse(spec, inputs)
	if e != nil || strictJSON(spec) != nil {
		return fail("io_declaration_invalid")
	}
	identity.SpecDigest, identity.InputsDigest, identity.CaseSeconds = decl.SpecDigest, decl.InputsDigest, decl.Spec.CaseSeconds
	identity.MandatoryChecks = runner.RequiredChecks(rec)
	identity.MandatoryTests = iodecl.Tests(decl.Spec.Cases)
	mb, _ := json.Marshal(identity.MandatoryChecks)
	identity.MandatoryChecksDigest = library.Digest(mb)
	tb, _ := json.Marshal(identity.MandatoryTests)
	identity.MandatoryTestsDigest = library.Digest(tb)
	vcs := array(v["checks"])
	if !reflect.DeepEqual(identity.MandatoryChecks, []string{"B1"}) || len(vcs) != 1 || !reflect.DeepEqual(array(obj(vcs[0])["tests"]), toAny(identity.MandatoryTests)) {
		return fail("mandatory_set_mismatch")
	}
	for _, d := range []string{identity.ReferencePatchArchive, identity.NegativeControlArchive} {
		fs, e := safeTar(ctx, r.objects[d], ResolveTotalMax)
		if e != nil || len(fs) == 0 {
			return fail("reference_or_control_archive_invalid")
		}
		if d == identity.ReferencePatchArchive {
			if _, ok := fs["solve.sh"]; !ok {
				return fail("reference_missing")
			}
		}
	}
	// Tree manifests and their file-object references are checked recursively without executing files.
	for _, d := range []string{identity.NegativeControlTree, identity.ReferenceResultTree} {
		if e = resolveTree(r, d); e != nil {
			return fail("tree_identity_invalid")
		}
	}
	var audit []byte
	for d := range archiveIDs {
		audit, e = r.read("object", d, ResolveArchiveMax)
		if e != nil {
			return fail("qualification_audit_unavailable_or_limit")
		}
		identity.QualificationArchives = append(identity.QualificationArchives, d)
	}
	sort.Strings(identity.QualificationArchives)
	if e = resolveAudit(r, rec, checks, decl, audit); e != nil {
		return fail(e.Error())
	}
	if ctx.Err() != nil {
		return fail("resolve_deadline")
	}
	r.objects[request.ManifestDigest] = append([]byte(nil), raw...)
	r.objects[request.PreviousManifestDigest] = append([]byte(nil), prior...)
	return &ResolvedQualifiedIO{identity: identity, objects: r.objects}, nil
}
func toAny(ss []string) []any {
	v := make([]any, len(ss))
	for i, s := range ss {
		v[i] = s
	}
	return v
}
func collectDigests(v any, out map[string]bool) {
	switch x := v.(type) {
	case string:
		if digestRE.MatchString(x) {
			out[x] = true
		}
	case map[string]any:
		for _, y := range x {
			collectDigests(y, out)
		}
	case []any:
		for _, y := range x {
			collectDigests(y, out)
		}
	}
}

// strictJSON rejects duplicate object keys and trailing values before the shared schema sees bytes.
func strictJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					s, ok := k.(string)
					if !ok || seen[s] {
						return errors.New("duplicate_json")
					}
					seen[s] = true
					if e = walk(); e != nil {
						return e
					}
				}
			case '[':
				for d.More() {
					if e = walk(); e != nil {
						return e
					}
				}
			default:
				return errors.New("json_delimiter")
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if e := walk(); e != nil {
		return e
	}
	_, e := d.Token()
	if e != io.EOF {
		return errors.New("trailing_json")
	}
	return nil
}

// safeTar never extracts. It rejects duplicate paths, all links/special files and missing end markers.
func safeTar(ctx context.Context, b []byte, max int64) (map[string][]byte, error) {
	if int64(len(b)) > max || len(b) < 1024 || len(b)%512 != 0 || !bytes.Equal(b[len(b)-1024:], make([]byte, 1024)) {
		return nil, errors.New("archive_frame")
	}
	br := bytes.NewReader(b)
	tr := tar.NewReader(br)
	out := map[string][]byte{}
	var total int64
	padding := 0
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		before := br.Len()
		start := len(b) - before
		h, e := tr.Next()
		if e == io.EOF {
			if before-br.Len() < padding+1024 || !bytes.Equal(b[start:len(b)-br.Len()], make([]byte, before-br.Len())) {
				return nil, errors.New("archive_eos")
			}
			break
		}
		if e != nil {
			return nil, e
		}
		if !bytes.Equal(b[start:start+padding], make([]byte, padding)) {
			return nil, errors.New("archive_padding")
		}
		padding = int((512 - h.Size%512) % 512)
		n := h.Name
		if n == "" || path.Clean(n) != n || strings.HasPrefix(n, "/") || n == ".." || strings.HasPrefix(n, "../") || strings.ContainsAny(n, "\\\x00") || h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || out[n] != nil || len(out) >= 4096 || h.Size < 0 || h.Size > max-total {
			return nil, errors.New("archive_entry")
		}
		total += h.Size
		data, e := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if e != nil || int64(len(data)) != h.Size {
			return nil, errors.New("archive_truncated")
		}
		out[n] = data
	}
	rest, _ := io.ReadAll(br)
	if !bytes.Equal(rest, make([]byte, len(rest))) {
		return nil, errors.New("archive_trailing")
	}
	return out, nil
}
func resolveTree(r *resolutionRead, d string) error {
	b, e := r.read("object", d, ResolveMetadataMax)
	if e != nil || strictJSON(b) != nil {
		return errors.New("tree")
	}
	var tree trees.Tree
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&tree) != nil || tree.TreeVersion != 1 || len(tree.Files) > 4096 {
		return errors.New("tree")
	}
	previous := ""
	for _, f := range tree.Files {
		if f.Path == "" || path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "/") || f.Path == ".." || strings.HasPrefix(f.Path, "../") || strings.ContainsAny(f.Path, "\\\x00") || f.Path <= previous || f.Mode != "100644" && f.Mode != "100755" || f.Size < 0 || !digestRE.MatchString(f.Digest) {
			return errors.New("tree_file")
		}
		for _, part := range strings.Split(f.Path, "/") {
			if part == ".git" {
				return errors.New("tree_git")
			}
		}
		data, e := r.read("object", f.Digest, ResolveTotalMax)
		if e != nil || int64(len(data)) != f.Size {
			return errors.New("tree_file_identity")
		}
		previous = f.Path
	}
	return nil
}

func resolveAudit(r *resolutionRead, rec map[string]any, checks map[string]map[string]any, decl *iodecl.Declaration, archive []byte) error {
	files, e := safeTar(r.ctx, archive, ResolveArchiveMax)
	if e != nil {
		return errors.New("qualification_archive_invalid")
	}
	tmp, e := os.MkdirTemp("", "hint-qualified-inputs-")
	if e != nil {
		return errors.New("qualification_temp_unavailable")
	}
	defer os.RemoveAll(tmp)
	groups := map[string]bool{}
	// Any file witnesses a trial, including interrupted/partial controls without result.json.
	trialPrefixes := map[string]int{}
	groupPrefixes := map[string]map[string]bool{}
	var count int
	for name, b := range files {
		parts := strings.Split(name, "/")
		if len(parts) < 4 || parts[0] != "jobs" {
			return errors.New("qualification_archive_layout")
		}
		group := parts[1]
		if group != "base" && group != "reference" && group != "probe" && !regexp.MustCompile(`^(near-miss|alternative)-[1-3]$`).MatchString(group) {
			return errors.New("qualification_job_unknown")
		}
		trialName := parts[2]
		numberText, ok := strings.CutPrefix(trialName, group+"__")
		if !ok || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(numberText) {
			return errors.New("qualification_trial_layout")
		}
		maxTrial := 1
		if group == "reference" {
			maxTrial = ReferenceRuns
		}
		var ordinal int
		if _, err := fmt.Sscan(numberText, &ordinal); err != nil || ordinal < 1 || ordinal > maxTrial {
			return errors.New("qualification_trial_layout")
		}
		prefix := strings.Join(parts[:3], "/")
		if _, seen := trialPrefixes[prefix]; !seen {
			trialPrefixes[prefix] = 0
		}
		if groupPrefixes[group] == nil {
			groupPrefixes[group] = map[string]bool{}
		}
		groupPrefixes[group][prefix] = true
		groups[group] = true
		if len(parts) == 4 && parts[3] == "result.json" {
			trialPrefixes[prefix]++
			if int64(len(b)) > ResolveMetadataMax || strictJSON(b) != nil {
				return errors.New("qualification_result_invalid")
			}
			var raw map[string]any
			if json.Unmarshal(b, &raw) != nil {
				return errors.New("qualification_result_invalid")
			}
			c := obj(raw["hint_container"])
			p := obj(c["io"])
			if c["runner"] != runner.DockerRunnerVersion || p["interface"] != iodecl.Interface || p["spec"] != decl.SpecDigest || p["inputs"] != decl.InputsDigest || p["oracle_runs"] != float64(runner.OracleRuns) || p["verdict_scope"] != "equivalence_to_reference_on_declared_cases" || p["network"] != "engine_inspect:blocked" {
				return errors.New("qualification_provenance_mismatch")
			}
			for _, k := range []string{"reference_tree", "oracle_outputs", "candidate_outputs"} {
				if !digestRE.MatchString(str(p[k])) {
					return errors.New("qualification_provenance_missing")
				}
			}
			artifact := strings.Join(parts[:3], "/") + "/oracle/final.tar"
			if data, ok := files[artifact]; !ok || library.Digest(data) != p["reference_tree"] {
				return errors.New("qualification_reference_artifact_missing")
			}
			if raw["trial_name"] != parts[2] {
				return errors.New("qualification_trial_identity")
			}
			count++
			groups[group] = true
		}
		// Only the exact files consumed by the existing parser/probe readers enter the temp tree.
		selectFile := len(parts) == 4 && parts[3] == "result.json" || len(parts) == 5 && parts[3] == "verifier" && (parts[4] == "test-stdout.txt" || parts[4] == "network.txt") || len(parts) == 5 && parts[3] == "agent" && parts[4] == "oracle.txt"
		if selectFile {
			if int64(len(b)) > ResolveMetadataMax {
				return errors.New("qualification_metadata_limit")
			}
			dest := filepath.Join(tmp, filepath.FromSlash(name))
			if e = os.MkdirAll(filepath.Dir(dest), 0o700); e != nil {
				return errors.New("qualification_temp_unavailable")
			}
			if e = os.WriteFile(dest, b, 0o600); e != nil {
				return errors.New("qualification_temp_unavailable")
			}
		}
	}
	for _, resultCount := range trialPrefixes {
		if resultCount != 1 {
			return errors.New("qualification_trial_result_missing")
		}
	}
	if len(trialPrefixes) != count {
		return errors.New("qualification_trial_inventory_mismatch")
	}
	if count > 1+ReferenceRuns+MaxNearMisses+1+3 {
		return errors.New("qualification_control_counts")
	}
	trials := map[string][]runner.Trial{}
	for group := range groups {
		if r.ctx.Err() != nil {
			return errors.New("resolve_deadline")
		}
		ts, e := runner.ReadJob(filepath.Join(tmp, "jobs", group))
		if e != nil || len(ts) != len(groupPrefixes[group]) {
			return errors.New("qualification_trials_missing")
		}
		for _, t := range ts {
			if t.TrustOver([]string{"B1"}) != runner.TrustProcessSeparated || t.TrustInvalid || t.Tampered || t.Exception != "" {
				return errors.New("qualification_trial_untrusted")
			}
		}
		trials[group] = ts
	}
	base, refs, probe := trials["base"], trials["reference"], trials["probe"]
	if len(base) != 1 || base[0].Outcome() != "failed" || base[0].Checks["B1"] != "fail" || len(refs) != ReferenceRuns || len(probe) != 1 || probe[0].Reward == nil || *probe[0].Reward != 0 {
		return errors.New("qualification_control_result")
	}
	for _, t := range refs {
		if t.Outcome() != "passed" || !allPass(t, []string{"B1"}) {
			return errors.New("qualification_reference_failed")
		}
	}
	cov, requirements, _ := coverage(rec, &base[0], refs, true)
	if cov.Status != "pass" || checks["requirement_coverage"]["runs"] != float64(cov.Runs) {
		return errors.New("qualification_requirement_coverage")
	}
	claimed := array(obj(rec["qualification"])["requirements"])
	rawReq, _ := json.Marshal(requirements)
	var actualReq []any
	_ = json.Unmarshal(rawReq, &actualReq)
	if !reflect.DeepEqual(claimed, actualReq) {
		return errors.New("qualification_requirement_mismatch")
	}
	near := 0
	alts := 0
	for name, ts := range trials {
		if strings.HasPrefix(name, "near-miss-") {
			if len(ts) != 1 || ts[0].Outcome() != "failed" {
				return errors.New("qualification_near_miss_failed")
			}
			near++
		}
		if strings.HasPrefix(name, "alternative-") {
			if len(ts) != 1 || ts[0].Outcome() != "passed" || !allPass(ts[0], []string{"B1"}) {
				return errors.New("qualification_alternative_failed")
			}
			alts++
		}
	}
	for i := 1; i <= near; i++ {
		if !groups[fmt.Sprintf("near-miss-%d", i)] {
			return errors.New("qualification_near_miss_gap")
		}
	}
	for i := 1; i <= alts; i++ {
		if !groups[fmt.Sprintf("alternative-%d", i)] {
			return errors.New("qualification_alternative_gap")
		}
	}
	expectedNear := near
	if expectedNear == 0 {
		expectedNear = 1
	} // q3 uses the observed base as its empty-change near miss
	if checks["near_miss_fails"]["runs"] != float64(expectedNear) {
		return errors.New("qualification_near_miss_count")
	}
	alt := checks["alternative_passes"]
	if alt == nil || alt["runs"] != float64(alts) || alts == 0 && alt["status"] != "missing" || alts > 0 && alt["status"] != "pass" {
		return errors.New("qualification_alternative_count")
	}
	if netSeen(filepath.Join(tmp, "jobs", "probe")) != "blocked" || verifierNetSeen(filepath.Join(tmp, "jobs", "probe")) != "blocked" {
		return errors.New("qualification_isolation_missing")
	}
	return nil
}

// ObjectDigests returns copied sorted identities of the exact resolved inputs,
// including the two original manifest byte domains. It confers no authority.
func (h *ResolvedQualifiedIO) ObjectDigests() []string {
	keys := make([]string, 0, len(h.objects))
	for k := range h.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
