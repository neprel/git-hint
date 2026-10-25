package project

// This is a policy projection of caller-supplied local snapshot records. It does
// not attest prelaunch provenance and never changes the ordinary current graph.
import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/snapshot"
)

const maxSnapshotProjectionRecords = 200000

// SnapshotProjectionRef is the exact standard identity/digest pair in one repo.
// A later producer fact supplies its separate record reference; this API invents none.
type SnapshotProjectionRef struct{ RepoID, SnapshotID, Digest string }

type SnapshotProjectionMetadata struct {
	Namespace, Version, PolicyID, PolicyDigest, SourceSnapshotDigest string
	TimeResolutionSeconds                                            int
	Ref                                                              SnapshotProjectionRef
}

// LocalSnapshotProjection exposes only copied filtered records and factual
// bindings. Local index/state-key/content hashes and keys have no accessor.
type LocalSnapshotProjection struct {
	state, reason string
	metadata      SnapshotProjectionMetadata
	payload       []byte
}

func (p *LocalSnapshotProjection) State() (string, string) {
	if p == nil || p.state == "" {
		return "unavailable", "projection_missing"
	}
	return p.state, p.reason
}
func (p *LocalSnapshotProjection) Metadata() SnapshotProjectionMetadata {
	if p == nil {
		return SnapshotProjectionMetadata{}
	}
	return p.metadata
}
func (p *LocalSnapshotProjection) Payload() []byte {
	if p == nil {
		return nil
	}
	return bytes.Clone(p.payload)
}
func (*LocalSnapshotProjection) MarshalJSON() ([]byte, error) {
	return nil, errors.New("local snapshot projection has no public wire form")
}

type snapshotProjectionPolicy struct {
	namespace, version, profile, policyID, policyDigest string
	resolution                                          int
	repositories                                        map[string]bool
	fields                                              map[string][]string
	allowed                                             map[string]bool
}

func copyProjectionFields(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}
func (c *projectionCollector) freezeSnapshotPolicy(version string, pol map[string]any, allowed map[string]bool, fields map[string][]string, res int, configured []string) {
	d, e := hint.Digest(pol)
	if e != nil {
		return
	}
	repos := map[string]bool{}
	for _, repoID := range configured {
		repos[repoID] = true
	}
	c.snapshotPolicy = &snapshotProjectionPolicy{namespace: c.namespace, version: version, profile: str(pol["profile"]), policyID: str(pol["policy_id"]), policyDigest: d,
		resolution: res, repositories: repos, fields: copyProjectionFields(fields), allowed: maps.Clone(allowed)}
}

// ProjectSnapshot applies this builder's frozen normal policy and actual normal
// snapshot transformer. It returns no document and performs no storage or scan IO.
// Caller input is factual data, never proof of capture, namespace membership,
// source authority or a prelaunch role. The repo must belong to the frozen package.
func (r *LocalProjectionReceipt) ProjectSnapshot(local snapshot.Local) (*LocalSnapshotProjection, error) {
	unavailable := func(reason string) *LocalSnapshotProjection {
		return &LocalSnapshotProjection{state: "unavailable", reason: reason}
	}
	if r == nil || r.snapshotPolicy == nil {
		return unavailable("snapshot_policy_unavailable"), nil
	}
	ctx := r.snapshotPolicy
	if !projectionRef.MatchString(ctx.namespace) || ctx.namespace != r.namespace || !ctx.repositories[local.Record.RepoID] {
		return nil, errors.New("snapshot_projection_scope_invalid")
	}
	if ctx.profile == "aggregate" || !ctx.allowed["Snapshot"] {
		return unavailable("snapshot_policy_omits_scope"), nil
	}
	if len(local.Entities)+len(local.Relations)+len(local.Record.ParentSnapshotIDs) > maxSnapshotProjectionRecords {
		return unavailable("snapshot_projection_record_limit"), nil
	}
	// Copy ONLY structural records. Index, StateKey, Stats, raw hashes and a caller's
	// other local state do not enter serialization or the returned payload.
	raw, e := json.Marshal(struct {
		Snapshots []snapshot.Record         `json:"snapshots"`
		Entities  []snapshot.EntityRecord   `json:"entities"`
		Relations []snapshot.RelationRecord `json:"relations"`
	}{[]snapshot.Record{local.Record}, local.Entities, local.Relations})
	if e != nil {
		return nil, errors.New("snapshot_projection_input_invalid")
	}
	if len(raw) > maxProjectionDocumentBytes {
		return unavailable("snapshot_projection_byte_limit"), nil
	}
	var copied struct {
		Snapshots []snapshot.Record         `json:"snapshots"`
		Entities  []snapshot.EntityRecord   `json:"entities"`
		Relations []snapshot.RelationRecord `json:"relations"`
	}
	if json.Unmarshal(raw, &copied) != nil {
		return nil, errors.New("snapshot_projection_input_invalid")
	}
	// Snapshot.Build emits empty arrays, not null, in a standard payload.
	native := map[string]any{}
	if json.Unmarshal(raw, &native) != nil {
		return nil, errors.New("snapshot_projection_input_invalid")
	}
	if native["entities"] == nil {
		native["entities"] = []any{}
	}
	if native["relations"] == nil {
		native["relations"] = []any{}
	}
	schemas, e := hint.LoadSchemas()
	if e != nil {
		return nil, errors.New("snapshot_projection_validator_unavailable")
	}
	if schemas.ValidateSnapshotPayload(ctx.version, native) != nil || len(hint.CheckSnapshotPayload(ctx.version, native, []string{local.Record.RepoID})) != 0 {
		return nil, errors.New("snapshot_projection_input_invalid")
	}
	for _, entity := range copied.Entities {
		if entity.SnapshotID != local.Record.SnapshotID || entity.RepoID != local.Record.RepoID {
			return nil, errors.New("snapshot_projection_scope_invalid")
		}
	}
	for _, relation := range copied.Relations {
		if relation.SnapshotID != local.Record.SnapshotID {
			return nil, errors.New("snapshot_projection_scope_invalid")
		}
	}
	source := snapshot.Local{Record: copied.Snapshots[0], Entities: copied.Entities, Relations: copied.Relations}
	// A fresh memo is essential: equal snapshot IDs with different actual scan
	// CapturedAt values can have different digests. Never reuse a current payload.
	report := Report{RemovedFields: map[string]int{}}
	tf := transform{fields: copyProjectionFields(ctx.fields), res: ctx.resolution, report: &report}
	rec, ents, rels, d, e := tf.snapshot(&source, ctx.allowed)
	if e != nil {
		return nil, errors.New("snapshot_projection_transform_invalid")
	}
	payload := map[string]any{"snapshots": []any{rec}, "entities": ents, "relations": rels}
	sortBy(ents, "entity_id")
	sortBy(rels, "relation_id")
	if schemas.ValidateSnapshotPayload(ctx.version, payload) != nil || len(hint.CheckSnapshotPayload(ctx.version, payload, []string{source.Record.RepoID})) != 0 {
		return nil, errors.New("snapshot_projection_transform_invalid")
	}
	encoded, e := json.Marshal(payload)
	if e != nil {
		return nil, errors.New("snapshot_projection_transform_invalid")
	}
	if len(encoded) > maxProjectionDocumentBytes {
		return unavailable("snapshot_projection_byte_limit"), nil
	}
	state, reason := "complete", ""
	if !ctx.allowed["Entity"] || !ctx.allowed["Relation"] || !contains(ctx.fields["Entity"], "parent_id") || !contains(ctx.fields["Entity"], "features") {
		state, reason = "incomplete", "snapshot_policy_omits_geometry"
	}
	return &LocalSnapshotProjection{state: state, reason: reason, payload: encoded, metadata: SnapshotProjectionMetadata{Namespace: ctx.namespace, Version: ctx.version, PolicyID: ctx.policyID,
		PolicyDigest: ctx.policyDigest, SourceSnapshotDigest: source.Record.Digest, TimeResolutionSeconds: ctx.resolution, Ref: SnapshotProjectionRef{source.Record.RepoID, source.Record.SnapshotID, d}}}, nil
}
