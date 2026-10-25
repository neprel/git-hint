package project

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
)

// Doc is one document of an export package.
type Doc struct {
	Type   string
	File   string // <document_type>-<first 12 hex of content_digest>.hint.json
	Raw    []byte // the bytes that leave the machine
	Digest string
	ID     string
}

// Package is a validated export package.
type Package struct {
	Version         string // hint_version of every document
	Docs            []Doc
	Policy          map[string]any    // export_policy payload
	Observations    []string          // observation IDs in the package
	SnapshotDigests map[string]string // snapshot ID -> exported digest
	Report          Report
}

// Report summarizes a package for the user before it leaves the machine.
type Report struct {
	Profile               string              `json:"profile"`
	TimeResolutionSeconds int                 `json:"time_resolution_seconds"`
	PolicyID              string              `json:"policy_id"`
	Documents             map[string]int      `json:"documents"`
	Records               map[string]int      `json:"records"`
	EntityKinds           map[string]int      `json:"entity_kinds"`
	Ops                   map[string]int      `json:"observation_ops"`
	Fields                map[string][]string `json:"fields"`
	TimeMin               string              `json:"time_min,omitempty"`
	TimeMax               string              `json:"time_max,omitempty"`
	Features              []string            `json:"exact_features"`
	RemovedFields         map[string]int      `json:"removed_fields"` // record type -> fields dropped by the policy
	Notes                 []string            `json:"notes"`
}

// ExportOptions control BuildPackage.
type ExportOptions struct {
	All bool // include observations that were already exported
	Now time.Time
}

// BuildPackage builds and validates the export package from the local state. It reads no
// network and writes nothing. Every document passes the schema, the semantic rules and the
// export policy; otherwise BuildPackage fails (deny by default).
func BuildPackage(s *store.Store, opt ExportOptions) (*Package, error) {
	return buildPackage(s, opt, nil)
}

func buildPackage(s *store.Store, opt ExportOptions, receipt *projectionCollector) (*Package, error) {
	ver, err := packageVersion(s, opt)
	if err != nil {
		return nil, err
	}
	pol, err := policyPayload(s, ver)
	if err != nil {
		return nil, err
	}
	res := s.Cfg.Policy.TimeResolutionSeconds
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	created := coarse(now.UTC().Format(time.RFC3339Nano), res, true)
	allowed := map[string]bool{}
	for _, rt := range pol["record_types"].([]string) {
		allowed[rt] = true
	}
	fields := pol["fields"].(map[string][]string)
	p := &Package{Version: ver, Policy: pol, SnapshotDigests: map[string]string{}, Report: Report{Profile: s.Cfg.Policy.Profile,
		TimeResolutionSeconds: res, PolicyID: pol["policy_id"].(string), Documents: map[string]int{},
		Records: map[string]int{}, EntityKinds: map[string]int{}, Ops: map[string]int{}, Fields: fields,
		RemovedFields: map[string]int{}}}
	tf := &transform{fields: fields, res: res, report: &p.Report}

	// 1. export policy: the receiver checks the allowlist against it
	if err := p.add(s, "export_policy", nil, nil, nil, pol, created); err != nil {
		return nil, err
	}
	// 2. repository snapshots (latest of every repository)
	var latest []*snapshot.Local
	for _, r := range s.Cfg.Repos {
		l, err := LoadLatest(s, s.RepoID(r))
		if err != nil {
			return nil, err
		}
		if l == nil {
			return nil, fmt.Errorf("repository %q has no snapshot; run `git hint collect` first", r.Name)
		}
		latest = append(latest, l)
	}
	if allowed["Snapshot"] {
		payload := map[string]any{"snapshots": []any{}, "entities": []any{}, "relations": []any{}}
		var repos []string
		types := []string{"Snapshot"}
		for _, l := range latest {
			snap, ents, rels, d, err := tf.snapshot(l, allowed)
			if err != nil {
				return nil, err
			}
			p.SnapshotDigests[l.Record.SnapshotID] = d
			payload["snapshots"] = append(payload["snapshots"].([]any), snap)
			payload["entities"] = append(payload["entities"].([]any), ents...)
			payload["relations"] = append(payload["relations"].([]any), rels...)
			repos = append(repos, l.Record.RepoID)
			p.Report.Records["Snapshot"]++
			p.Report.Records["Entity"] += len(ents)
			p.Report.Records["Relation"] += len(rels)
			for _, e := range l.Entities {
				p.Report.EntityKinds[e.Kind]++
			}
		}
		sortBy(payload["entities"].([]any), "entity_id")
		sortBy(payload["relations"].([]any), "relation_id")
		if allowed["Entity"] {
			types = append(types, "Entity")
		}
		if allowed["Relation"] {
			types = append(types, "Relation")
		}
		if err := p.add(s, "repository_snapshot", repos, types, nil, payload, created); err != nil {
			return nil, err
		}
	}
	// 3. change sets: latest snapshot against its parent; a declared change (git hint changes
	// --declare-plan) is sent with its frozen snapshots and plan_application
	if allowed["Change"] {
		for _, l := range latest {
			decls, err := Declarations(s, l.Record.RepoID)
			if err != nil {
				return nil, err
			}
			type pair struct {
				prev, cur *snapshot.Local
				decl      *Declaration
			}
			var pairs []pair
			for i := range decls {
				d := &decls[i]
				if d.AfterSnapshotID == l.Record.SnapshotID {
					continue
				}
				prev, err := LoadSnapshot(s, d.RepoID, d.BeforeSnapshotID)
				if err != nil {
					return nil, fmt.Errorf("declared change %s: %w", d.AfterSnapshotID, err)
				}
				cur, err := LoadSnapshot(s, d.RepoID, d.AfterSnapshotID)
				if err != nil {
					return nil, fmt.Errorf("declared change %s: %w", d.AfterSnapshotID, err)
				}
				pairs = append(pairs, pair{prev, cur, d})
			}
			var latestDecl *Declaration
			for i := range decls {
				d := &decls[i]
				if d.AfterSnapshotID != l.Record.SnapshotID {
					continue
				}
				if len(l.Record.ParentSnapshotIDs) > 0 && d.BeforeSnapshotID == l.Record.ParentSnapshotIDs[0] {
					latestDecl = d
					continue
				}
				// a work unit change from its frozen base to the latest snapshot (several
				// snapshots in between): its own pair; the plain latest change stays too
				prev, err := LoadSnapshot(s, d.RepoID, d.BeforeSnapshotID)
				if err != nil {
					return nil, fmt.Errorf("declared change %s: %w", d.AfterSnapshotID, err)
				}
				pairs = append(pairs, pair{prev, l, d})
			}
			if len(l.Record.ParentSnapshotIDs) > 0 {
				prev, err := LoadSnapshot(s, l.Record.RepoID, l.Record.ParentSnapshotIDs[0])
				if err == nil {
					pairs = append(pairs, pair{prev, l, latestDecl})
				} else if latestDecl != nil {
					return nil, fmt.Errorf("declared change %s: %w", latestDecl.AfterSnapshotID, err)
				} // else: parent not stored (for example after a store reset)
			}
			for _, pr := range pairs {
				prev, l := pr.prev, pr.cur
				cs, _ := snapshot.Diff(s.Key(), prev, l)
				_, _, _, bd, err := tf.snapshot(prev, allowed)
				if err != nil {
					return nil, err
				}
				_, _, _, ad, err := tf.snapshot(l, allowed)
				if err != nil {
					return nil, err
				}
				cs.BeforeSnapshot.Digest, cs.AfterSnapshot.Digest = bd, ad
				payload, err := snapshot.Plain(cs)
				if err != nil {
					return nil, err
				}
				var changes []any
				for _, c := range payload["changes"].([]any) {
					changes = append(changes, tf.filter("Change", c.(map[string]any)))
				}
				if changes == nil {
					changes = []any{}
				}
				payload["changes"] = changes
				if d := pr.decl; d != nil {
					if d.BeforeSnapshotID != prev.Record.SnapshotID || bd != d.PlanSnapshotDigest {
						return nil, fmt.Errorf("declared change %s: the parent snapshot no longer exports with the plan's snapshot digest", d.AfterSnapshotID)
					}
					payload["plan_application"] = d.planApplication()
				}
				p.Report.Records["Change"] += len(changes)
				if err := p.add(s, "change_set", []string{l.Record.RepoID}, []string{"Change"}, nil, payload, created); err != nil {
					return nil, err
				}
			}
		}
	}
	// 4. activity
	if allowed["Observation"] && s.Cfg.Policy.Profile != "aggregate" {
		obs, damage, err := ReadSpoolChecked(s)
		if err != nil {
			return nil, err
		}
		if damage.Any() { // conflicting records are withheld (no winner); affected sessions get an invalidating closure (C7BE-21)
			inv := invalidations(s, obs, damage, opt.Now)
			obs = append(obs, inv...)
			p.Report.Notes = append(p.Report.Notes, fmt.Sprintf("activity spool damaged: %d unreadable line(s), %d conflicting record(s) withheld; %d session closure(s) invalidated",
				damage.Unreadable, len(damage.Conflicts), len(inv)))
		}
		st, err := loadExportState(s)
		if err != nil {
			return nil, err
		}
		done := map[string]bool{}
		if !opt.All {
			for _, id := range st.Observations {
				done[id] = true
			}
		}
		srcs, err := Sources(s)
		if err != nil {
			return nil, err
		}
		emitted := make([]activity.Observation, 0, len(obs))
		for _, o := range obs {
			if done[o.ObservationID] {
				continue
			}
			// Ledger instance/anchor timestamps require lossless comparison. Coarse-ledger
			// export is unsupported; never silently retain exact nested timestamps.
			if o.ModelLedger != nil && contains(fields["Observation"], "model_ledger") && res > 0 {
				return nil, errLedgerProfileUnsupported
			}
			emitted = append(emitted, o)
		}
		var private *activityPrivacy
		if ver == version.ActivityPrivacyVersion {
			inputs, selectErr := activityPrivacyInputs(obs, emitted)
			if selectErr != nil {
				return nil, errActivityPrivacy
			}
			private, err = newActivityPrivacy(s, inputs, srcs, now)
			if err != nil {
				return nil, errActivityPrivacy
			}
		}
		var list []any
		used := map[string]bool{}
		for _, o := range emitted {
			if _, ok := srcs[o.SourceID]; !ok {
				return nil, fmt.Errorf("observation source %s is not registered; the spool is inconsistent", o.SourceID)
			}
			var m map[string]any
			var err error
			if private != nil && private.observations[o.ObservationID] != nil {
				m, err = tf.observationMap(private.observations[o.ObservationID])
			} else {
				m, err = tf.observation(o)
			}
			if err != nil {
				return nil, err
			}
			receipt.capture(o, srcs[o.SourceID], m)
			list = append(list, m)
			used[o.SourceID] = true
			p.Observations = append(p.Observations, o.ObservationID)
			p.Report.Ops[o.Op]++
		}
		if len(list) > 0 {
			var sources []any
			ids := keys(used)
			for _, id := range ids {
				src := srcs[id]
				if private != nil {
					if projected, ok := private.sources[id]; ok {
						src = projected
					}
				}
				sources = append(sources, sourceFor(src, p.Version))
			}
			p.Report.Records["Observation"] += len(list)
			var repos []string
			for _, l := range latest {
				repos = append(repos, l.Record.RepoID)
			}
			if err := p.add(s, "activity_stream", repos, []string{"Observation"}, sources,
				map[string]any{"observations": list}, created); err != nil {
				return nil, err
			}
			receipt.seal(p.Docs[len(p.Docs)-1])
		}
	}
	p.Report.Notes = append(p.Report.Notes,
		"All identifiers are HMAC pseudonyms under the project key; the key and the name mapping stay local.",
		"Structure, sizes and times still carry information (privacy-v0.1 §4). Use the coarse or aggregate profile to reduce it.")
	return p, nil
}

// add seals, validates and appends one document.
func (p *Package) add(s *store.Store, docType string, repos, types []string, sources []any, payload map[string]any, created string) error {
	if repos == nil {
		repos = []string{}
	}
	if types == nil {
		types = []string{}
	}
	sort.Strings(repos)
	sort.Strings(types)
	plain, err := snapshot.Plain(payload)
	if err != nil {
		return err
	}
	digest, err := hint.Digest(plain)
	if err != nil {
		return err
	}
	doc := map[string]any{
		"hint_version": p.Version, "document_type": docType,
		"document_id":       hint.DeriveID(s.Key(), "document", docType, digest),
		"project_namespace": s.Cfg.ProjectNamespace, "repositories": repos,
		"producer":   map[string]any{"tool": "hint-cli", "version": version.Version},
		"created_at": created,
		"redaction": map[string]any{"policy_id": p.Policy["policy_id"], "profile": p.Policy["profile"],
			"record_types": types, "free_text": "denied", "time_resolution_seconds": p.Policy["time_resolution_seconds"]},
		"content_digest": digest, "payload": plain,
	}
	if sources != nil {
		doc["sources"] = sources
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := Validate(raw, p.Policy); err != nil {
		return fmt.Errorf("%s: %w (the document is not exported)", docType, err)
	}
	p.Docs = append(p.Docs, Doc{Type: docType, Raw: raw, Digest: digest, ID: doc["document_id"].(string),
		File: fmt.Sprintf("%s-%s.hint.json", docType, strings.TrimPrefix(digest, "sha256:")[:12])})
	p.Report.Documents[docType]++
	return nil
}

// Validate runs the schema, the semantic rules and the policy allowlist on one document.
func Validate(raw []byte, policy map[string]any) error {
	sch, err := hint.LoadSchemas()
	if err != nil {
		return err
	}
	dec, err := hint.Decode(raw)
	if err != nil {
		return fmt.Errorf("schema_invalid: %v", err)
	}
	if err := sch.Validate(dec); err != nil {
		return fmt.Errorf("schema_invalid: %v", err)
	}
	doc, err := hint.Plain(dec)
	if err != nil {
		return err
	}
	var pol map[string]any
	if policy != nil {
		pol, err = snapshot.Plain(policy)
		if err != nil {
			return err
		}
	}
	if f := hint.Check(doc, pol); len(f) > 0 {
		return fmt.Errorf("%s", f[0].String())
	}
	return nil
}

// packageVersion selects v0.3 for model-ledger/membership privacy projection, v0.2
// for other draft fields, and v0.1 for ordinary legacy packages.
func packageVersion(s *store.Store, opt ExportOptions) (string, error) {
	if s.Cfg.Policy.Profile == "aggregate" {
		return version.SpecVersion, nil
	}
	needDraft := false
	for _, r := range s.Cfg.Repos { // a declared plan application is a draft v0.2 field
		d, err := Declarations(s, s.RepoID(r))
		if err != nil {
			return "", err
		}
		if len(d) > 0 {
			needDraft = true
		}
	}
	obs, damage, err := ReadSpoolChecked(s)
	if err != nil {
		return "", err
	}
	if damage.Any() { // an invalidating closure (draft v0.2 collection) may be published (C7BE-21)
		for _, o := range append(slices.Clone(obs), damage.Withheld...) {
			if o.Collection != nil {
				needDraft = true
			}
		}
	}
	st, err := loadExportState(s)
	if err != nil {
		return "", err
	}
	done := map[string]bool{}
	if !opt.All {
		for _, id := range st.Observations {
			done[id] = true
		}
	}
	srcs, err := Sources(s)
	if err != nil {
		return "", err
	}
	for _, o := range obs {
		if done[o.ObservationID] {
			continue
		}
		if o.ModelLedger != nil || o.Membership != nil {
			return version.ActivityPrivacyVersion, nil
		}
		// v0.2 fields: a context measurement, a declared task family, a source configuration,
		// a model call (MODEL_CALL, request kind, call identity, collector closure, normalization)
		if o.Context != nil || slices.Contains(o.Missing, "context") || o.Correlation["task_family_id"] != "" || srcs[o.SourceID].Configuration != nil ||
			o.Op == "MODEL_CALL" || o.RequestKind != "" || o.Collection != nil || o.Correlation["call_ref"] != "" || srcs[o.SourceID].UsageNormalization != "" ||
			o.ModelLedger != nil || o.Correlation["run_ref"] != "" {
			needDraft = true
		}
	}
	if needDraft {
		return version.DraftVersion, nil
	}
	return version.SpecVersion, nil
}

// policyPayload builds the export_policy payload of a package version from the configuration.
func policyPayload(s *store.Store, ver string) (map[string]any, error) {
	cp := s.Cfg.Policy
	switch cp.Profile {
	case "detailed":
		if cp.TimeResolutionSeconds != 0 && cp.TimeResolutionSeconds != 1 {
			return nil, fmt.Errorf("profile detailed needs time_resolution_seconds 0 or 1")
		}
	case "coarse":
		if cp.TimeResolutionSeconds < 60 {
			return nil, fmt.Errorf("profile coarse needs time_resolution_seconds 60, 3600 or 86400")
		}
	case "aggregate":
	default:
		return nil, fmt.Errorf("unknown profile %q", cp.Profile)
	}
	all, err := hint.RecordFieldsFor(ver)
	if err != nil {
		return nil, err
	}
	types := append([]string{}, cp.RecordTypes...)
	sort.Strings(types)
	fields := map[string][]string{}
	for _, rt := range types {
		known, ok := all[rt]
		if !ok {
			return nil, fmt.Errorf("unknown record type %q in the export policy", rt)
		}
		f := known
		if len(cp.Fields[rt]) > 0 {
			f = append([]string{}, cp.Fields[rt]...)
			sort.Strings(f)
			for _, x := range f {
				if !contains(known, x) {
					return nil, fmt.Errorf("record type %s has no field %q", rt, x)
				}
			}
			req, _ := hint.RequiredFields(rt)
			for _, x := range req {
				if !contains(f, x) {
					return nil, fmt.Errorf("the export policy removes the required field %s.%s; the record type cannot be exported without it", rt, x)
				}
			}
		}
		fields[rt] = f
	}
	body := map[string]any{"profile": cp.Profile, "record_types": types, "fields": fields,
		"time_resolution_seconds": cp.TimeResolutionSeconds}
	d, err := hint.Digest(body)
	if err != nil {
		return nil, err
	}
	body["policy_id"] = hint.DeriveID(s.Key(), "policy", d)
	return body, nil
}

// transform applies the field allowlist and the time resolution.
type transform struct {
	fields map[string][]string
	res    int
	report *Report
	memo   map[string]exported // snapshot ID -> exported form (each snapshot is transformed once)
}

type exported struct {
	rec        map[string]any
	ents, rels []any
	digest     string
}

func (t *transform) filter(rt string, m map[string]any) map[string]any {
	allow := t.fields[rt]
	for k := range m {
		if !contains(allow, k) {
			delete(m, k)
			t.report.RemovedFields[rt]++
		}
	}
	if rt == "Entity" {
		if f, ok := m["features"].(map[string]any); ok {
			for k := range f {
				if !contains(t.report.Features, k) {
					t.report.Features = append(t.report.Features, k)
					sort.Strings(t.report.Features)
				}
			}
		}
	}
	return m
}

// snapshot returns the exported form of one snapshot and its digest.
func (t *transform) snapshot(l *snapshot.Local, allowed map[string]bool) (map[string]any, []any, []any, string, error) {
	if x, ok := t.memo[l.Record.SnapshotID]; ok {
		return x.rec, x.ents, x.rels, x.digest, nil
	}
	rec, ents, rels, d, err := t.snapshotOnce(l, allowed)
	if err == nil {
		if t.memo == nil {
			t.memo = map[string]exported{}
		}
		t.memo[l.Record.SnapshotID] = exported{rec, ents, rels, d}
	}
	return rec, ents, rels, d, err
}

func (t *transform) snapshotOnce(l *snapshot.Local, allowed map[string]bool) (map[string]any, []any, []any, string, error) {
	rec, err := snapshot.Plain(l.Record)
	if err != nil {
		return nil, nil, nil, "", err
	}
	rec["captured_at"] = coarse(rec["captured_at"].(string), t.res, true)
	t.span(rec["captured_at"].(string))
	var ents, rels []any
	if allowed["Entity"] {
		for _, e := range l.Entities {
			m, err := snapshot.Plain(e)
			if err != nil {
				return nil, nil, nil, "", err
			}
			ents = append(ents, t.filter("Entity", m))
		}
	}
	if allowed["Relation"] {
		for _, r := range l.Relations {
			m, err := snapshot.Plain(r)
			if err != nil {
				return nil, nil, nil, "", err
			}
			rels = append(rels, t.filter("Relation", m))
		}
	}
	if ents == nil {
		ents = []any{}
	}
	if rels == nil {
		rels = []any{}
	}
	rec = t.filter("Snapshot", rec)
	rec["digest"] = "sha256:" + strings.Repeat("0", 64)
	payload, err := snapshot.Plain(map[string]any{"snapshots": []any{rec}, "entities": ents, "relations": rels})
	if err != nil {
		return nil, nil, nil, "", err
	}
	d, err := hint.SnapshotDigest(payload, l.Record.SnapshotID)
	if err != nil {
		return nil, nil, nil, "", err
	}
	rec["digest"] = d
	return rec, ents, rels, d, nil
}

func (t *transform) observation(o activity.Observation) (map[string]any, error) {
	m, err := snapshot.Plain(o)
	if err != nil {
		return nil, err
	}
	return t.observationMap(m)
}

// observationMap shares the normal field/profile transformation with privacy-projected values.
func (t *transform) observationMap(m map[string]any) (map[string]any, error) {
	tm := m["time"].(map[string]any)
	for _, k := range []string{"start", "end"} {
		if v, ok := tm[k].(string); ok {
			tm[k] = coarse(v, t.res, false)
			t.span(tm[k].(string))
		}
	}
	if col, ok := m["collection"].(map[string]any); ok {
		if v, ok := col["closed_at"].(string); ok {
			col["closed_at"] = coarse(v, t.res, false)
		}
	}
	pv := m["provenance"].(map[string]any)
	pv["captured_at"] = coarse(pv["captured_at"].(string), t.res, false)
	if t.res > 1 {
		tr, _ := pv["transformations"].([]any)
		pv["transformations"] = append(tr, map[string]any{"step": "coarsen", "tool": "hint-cli", "version": version.Version})
	}
	return t.filter("Observation", m), nil
}

func (t *transform) span(ts string) {
	if t.report.TimeMin == "" || ts < t.report.TimeMin {
		t.report.TimeMin = ts
	}
	if ts > t.report.TimeMax {
		t.report.TimeMax = ts
	}
}

// coarse truncates an RFC 3339 timestamp to a multiple of res seconds. With res 0 the
// value stays as recorded; with res 1 the fraction is dropped.
func coarse(ts string, res int, _ bool) string {
	if res < 1 {
		return ts
	}
	tm, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	u := tm.Unix()
	u -= u % int64(res)
	return time.Unix(u, 0).UTC().Format(time.RFC3339)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WritePackage writes the documents into dir with owner-only permissions and returns the
// file paths.
func WritePackage(p *Package, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var out []string
	for _, d := range p.Docs {
		f := filepath.Join(dir, d.File)
		if err := os.WriteFile(f, d.Raw, 0o600); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// MarkExported records the package in the export state and history.
func MarkExported(s *store.Store, p *Package, mode, target string, now time.Time) error {
	st, err := loadExportState(s)
	if err != nil {
		return err
	}
	if err := st.record(s, p.Observations, p.SnapshotDigests); err != nil {
		return err
	}
	var docs []map[string]string
	for _, d := range p.Docs {
		docs = append(docs, map[string]string{"type": d.Type, "document_id": d.ID, "content_digest": d.Digest, "file": d.File})
	}
	line, err := json.Marshal(map[string]any{"at": now.UTC().Format(time.RFC3339), "mode": mode, "target": target,
		"policy_id": p.Report.PolicyID, "documents": docs, "observations": len(p.Observations)})
	if err != nil {
		return err
	}
	return s.AppendLine(line, "state", "exports", "history.jsonl")
}

// sortBy orders records by (snapshot_id, id) across snapshots (hint-data §9.2 unsorted).
func sortBy(recs []any, idField string) {
	key := func(r any) string {
		m := r.(map[string]any)
		return str(m["snapshot_id"]) + "/" + str(m[idField])
	}
	sort.SliceStable(recs, func(i, j int) bool { return key(recs[i]) < key(recs[j]) })
}

// sourceFor adapts a source entry to the package version: v0.1 cannot name the "context"
// capability (observed hint_context calls), so a v0.1 package leaves it out.
func sourceFor(src activity.Source, ver string) activity.Source {
	if ver != version.SpecVersion {
		return src
	}
	src.Capabilities = slices.DeleteFunc(slices.Clone(src.Capabilities), func(c string) bool {
		return c == "context" || c == "correlation.task_family_id" || c == "request_kind" || c == "correlation.call_ref" || c == "collection" ||
			c == "correlation.run_ref" || c == "model_ledger"
	})
	src.Configuration = nil
	src.UsageNormalization = ""
	return src
}

// invalidations are the canonical answer to spool damage (C7BE-21): for every collector
// closure of an affected session a second closure record of the same session with closure
// unknown and unknown counts, in its own collection segment. A session with two closures is
// never complete for the server or the client, also when the first closure was exported
// earlier; earlier records stay as they were. An unreadable line or a withheld record without
// a session can belong to any session: then every closure is invalidated. The record ID
// derives from the closure it invalidates, so a repeated export sends it once.
func invalidations(s *store.Store, obs []activity.Observation, d SpoolDamage, now time.Time) []activity.Observation {
	type scope struct{ src, ses string }
	all := d.Unreadable > 0
	affected := map[scope]bool{}
	for _, w := range d.Withheld {
		if w.Correlation["session_id"] == "" {
			all = true
		}
		affected[scope{w.SourceID, w.Correlation["session_id"]}] = true
	}
	if now.IsZero() {
		now = time.Now()
	}
	at := activity.Timestamp(now)
	var out []activity.Observation
	// the closures in the spool and a withheld conflicting closure (its first version)
	for _, o := range append(slices.Clone(obs), d.Withheld...) {
		if o.Collection == nil || !all && !affected[scope{o.SourceID, o.Correlation["session_id"]}] {
			continue
		}
		v := o
		v.ObservationID = hint.DeriveID(s.Key(), "closure-invalidation", o.SourceID, o.ObservationID)
		v.Correlation = maps.Clone(o.Correlation)
		unknown := scan.Quantity{Unit: "count", State: "unknown", Reason: "not_collected"}
		v.Collection = &activity.Collection{CollectionRef: hint.DeriveID(s.Key(), "damaged-segment", o.SourceID, o.ObservationID),
			Lifecycle: "unknown", Closure: "unknown", EventsDropped: unknown, Errors: unknown, OpenCalls: unknown}
		if o.Time.Start != nil && *o.Time.Start > at {
			at = *o.Time.Start
		}
		end := at
		v.Time = activity.Time{Start: o.Time.Start, End: &end, Clock: o.Time.Clock}
		v.Provenance.SourceEventID = ""
		out = append(out, v)
	}
	return out
}
