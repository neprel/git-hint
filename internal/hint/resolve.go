package hint

import (
	"strings"
)

// LocalState is what a client knows locally when it imports a recommendation_set
// (exchange §4). It holds no names: the client resolves opaque IDs with its own protected
// mapping, represented here by the sets of IDs that resolve uniquely or ambiguously.
type LocalState struct {
	SupportedVersions []string          `json:"supported_versions"` // "major.minor", e.g. "0.1"
	ProjectNamespace  string            `json:"project_namespace"`
	Now               string            `json:"now"`
	SnapshotDigests   map[string]string `json:"snapshot_digests"` // repo_id -> current local snapshot digest
	// SnapshotDigestSets holds more digests of the same current snapshot per repo_id: the
	// digests of its exported forms (an export policy can change the exported records).
	SnapshotDigestSets map[string][]string `json:"snapshot_digest_sets,omitempty"`
	Resolvable         []string            `json:"resolvable"`
	Ambiguous          []string            `json:"ambiguous"`
}

// PlanResult is the import result of one plan. Status is accept, partial, render_only or reject.
type PlanResult struct {
	PlanID string   `json:"plan_id"`
	Status string   `json:"status"`
	Codes  []string `json:"codes"`
}

// ImportResult is the import result of one recommendation_set document.
type ImportResult struct {
	Document string       `json:"document"` // accepted or the rejection code
	Plans    []PlanResult `json:"plans"`
}

// ImportRecommendations applies the client import validation of exchange §4 in its fixed
// order. It never executes a step; it only decides what the client may show or apply.
func ImportRecommendations(raw []byte, s *Schemas, local LocalState, policy map[string]any) ImportResult {
	// 1. version
	dec, err := Decode(raw)
	if err != nil {
		return ImportResult{Document: "schema_invalid"}
	}
	top, _ := dec.(map[string]any)
	v, _ := top["hint_version"].(string)
	if !supported(v, local.SupportedVersions) {
		return ImportResult{Document: "unsupported_version"}
	}
	// 2. schema, 3. semantic rules (digests, references), 4. namespace
	if s.Validate(dec) != nil {
		return ImportResult{Document: "schema_invalid"}
	}
	doc, err := Plain(dec)
	if err != nil || doc["document_type"] != "recommendation_set" {
		return ImportResult{Document: "schema_invalid"}
	}
	if f := Check(doc, policy); len(f) > 0 {
		return ImportResult{Document: f[0].Code}
	}
	if doc["project_namespace"] != local.ProjectNamespace {
		return ImportResult{Document: CodeNamespace}
	}
	res := ImportResult{Document: "accepted"}
	ok := toSet(local.Resolvable)
	amb := toSet(local.Ambiguous)
	payload, _ := doc["payload"].(map[string]any)
	for _, p := range list(payload["plans"]) {
		m, _ := p.(map[string]any)
		r := PlanResult{PlanID: str(m["plan_id"]), Codes: []string{}}
		// 5. expiry
		if !before(local.Now, str(m["expires_at"])) {
			r.Codes = append(r.Codes, "expired")
		}
		// 6. snapshot digests, one per plan repository (the semantic rule plan_snapshot_set
		// already rejects a document with a missing, repeated or extra snapshot)
		planDigests := map[string][]any{}
		for _, sn := range list(m["snapshots"]) {
			sm, _ := sn.(map[string]any)
			planDigests[str(sm["repo_id"])] = append(planDigests[str(sm["repo_id"])], sm["digest"])
		}
		for _, rid := range list(m["repo_ids"]) {
			d, known := local.SnapshotDigests[str(rid)]
			pd := planDigests[str(rid)]
			switch {
			case !known:
				r.Codes = appendOnce(r.Codes, "repository_unknown")
			case len(pd) != 1 || (d != pd[0] && !inList(local.SnapshotDigestSets[str(rid)], pd[0])):
				r.Codes = appendOnce(r.Codes, "snapshot_mismatch")
			}
		}
		// 7. identifier resolution
		for _, id := range planIDs(m) {
			switch {
			case amb[id]:
				r.Codes = appendOnce(r.Codes, "ambiguous_id")
			case !ok[id]:
				r.Codes = appendOnce(r.Codes, "unresolved_id")
			}
		}
		switch {
		case len(r.Codes) > 0:
			r.Status = "reject"
		case m["decision"] != "recommend": // 8. refusals and evidence requests are shown, not applied
			r.Status = "render_only"
		default: // 9. unsupported or ambiguous steps are never applied
			r.Status = "accept"
			for _, st := range list(m["steps"]) {
				if sm, _ := st.(map[string]any); sm["support"] != "supported" {
					r.Status = "partial"
					r.Codes = appendOnce(r.Codes, "unsupported_step")
				}
			}
		}
		res.Plans = append(res.Plans, r)
	}
	return res
}

func planIDs(m map[string]any) []string {
	var ids []string
	for _, t := range list(m["targets"]) {
		ids = append(ids, str(t))
	}
	for _, st := range list(m["steps"]) {
		sm, _ := st.(map[string]any)
		for _, t := range list(sm["targets"]) {
			ids = append(ids, str(t))
		}
		if d, ok := sm["destination"].(string); ok {
			ids = append(ids, d)
		}
	}
	return ids
}

func supported(v string, versions []string) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	for _, s := range versions {
		if s == parts[0]+"."+parts[1] {
			return true
		}
	}
	return false
}

func toSet(l []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range l {
		m[v] = true
	}
	return m
}

func appendOnce(l []string, v string) []string {
	for _, x := range l {
		if x == v {
			return l
		}
	}
	return append(l, v)
}

func inList(l []string, v any) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
