package project

// Local factual correspondence only. No authority, enrollment, capture attestation,
// key, raw execution/image value, source bytes or public wire field is added.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

var projectionRef = regexp.MustCompile(`^[0-9a-f]{32}$`)

const maxProjectionRows = 1024
const maxProjectionDocumentBytes = 16 << 20

// BackendImageProjection names the backend side, not the agent's container image.
type BackendImageProjection struct{ Side, ImageRef string }

// LedgerProjection is an owned factual value. Native digests are PRIVATE local
// record digests, not standard exported payload fields or capture authority.
type LedgerProjection struct {
	State, Reason                                                 string
	NativeObservationID, NativeObservationDigest                  string
	NativeSourceID, NativeSourceDigest, NativeLedgerDigest        string
	LedgerRef                                                     string
	Revision                                                      int
	Binding                                                       activity.LedgerBinding
	ObservationID, ObservationDigest, SourceID, SourceDigest      string
	LedgerDigest, DocumentID, DocumentDigest, DocumentBytesSHA256 string
	ExecutionProvenanceRef                                        string
	BackendImages                                                 []BackendImageProjection
}

// LocalProjectionReceipt is constructed only by the normal package builder.
// Its rows are immutable through public accessors. It has no JSON wire form.
type LocalProjectionReceipt struct {
	namespace     string
	state, reason string
	rows          []LedgerProjection
}

func (r *LocalProjectionReceipt) Namespace() string {
	if r == nil {
		return ""
	}
	return r.namespace
}

func (r *LocalProjectionReceipt) State() (string, string) {
	if r == nil || r.state == "" {
		return "unavailable", "receipt_missing"
	}
	return r.state, r.reason
}
func (r *LocalProjectionReceipt) Ledgers() []LedgerProjection {
	if r == nil {
		return nil
	}
	out := slices.Clone(r.rows)
	for i := range out {
		out[i].BackendImages = slices.Clone(out[i].BackendImages)
	}
	return out
}
func (*LocalProjectionReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("local projection receipt has no public wire form")
}

// BuildPackageWithReceipt shares the exact BuildPackage core. Policy omissions or
// receipt limits never change ordinary successful package bytes into an error.
func BuildPackageWithReceipt(s *store.Store, opt ExportOptions) (*Package, *LocalProjectionReceipt, error) {
	c := &projectionCollector{namespace: s.Cfg.ProjectNamespace}
	p, e := buildPackage(s, opt, c)
	if e != nil {
		return nil, nil, e
	}
	return p, c.finish(), nil
}

type projectionCollector struct {
	namespace   string
	rows        []LedgerProjection
	failed      string
	sawActivity bool
}

func (c *projectionCollector) capture(o activity.Observation, src activity.Source, final map[string]any) {
	if c == nil || o.ModelLedger == nil || c.failed != "" {
		return
	}
	if len(c.rows) >= maxProjectionRows {
		c.failed = "receipt_row_limit"
		c.rows = nil
		return
	}
	r := LedgerProjection{State: "incomplete", Reason: "fields_omitted_or_unavailable", NativeObservationID: o.ObservationID, NativeSourceID: o.SourceID,
		LedgerRef: o.ModelLedger.LedgerRef, Revision: o.ModelLedger.Revision, Binding: o.ModelLedger.Binding, ObservationID: str(final["observation_id"])}
	var e error
	r.NativeObservationDigest, e = hint.Digest(o)
	if e != nil {
		c.failed = "native_digest_unavailable"
		c.rows = nil
		return
	}
	r.NativeSourceDigest, e = hint.Digest(src)
	if e != nil {
		c.failed = "native_digest_unavailable"
		c.rows = nil
		return
	}
	r.NativeLedgerDigest, e = hint.Digest(o.ModelLedger)
	if e != nil {
		c.failed = "native_digest_unavailable"
		c.rows = nil
		return
	}
	c.rows = append(c.rows, r)
}

// seal reads the actual validated final document after filtering/coarsening/seal.
// It does not regenerate privacyLedger or accept an external translation map.
func (c *projectionCollector) seal(doc Doc) {
	if c == nil {
		return
	}
	c.sawActivity = true
	if c.failed != "" {
		return
	}
	if len(doc.Raw) > maxProjectionDocumentBytes {
		c.failed = "receipt_document_limit"
		c.rows = nil
		return
	}
	var body map[string]any
	if json.Unmarshal(doc.Raw, &body) != nil {
		c.failed = "final_document_unavailable"
		c.rows = nil
		return
	}
	payload, _ := body["payload"].(map[string]any)
	observations, _ := payload["observations"].([]any)
	sources, _ := body["sources"].([]any)
	sha := sha256.Sum256(doc.Raw)
	rawDigest := "sha256:" + hex.EncodeToString(sha[:])
	for i := range c.rows {
		r := &c.rows[i]
		r.DocumentID = doc.ID
		r.DocumentDigest = doc.Digest
		r.DocumentBytesSHA256 = rawDigest
		var found map[string]any
		matches := 0
		for _, v := range observations {
			o, ok := v.(map[string]any)
			if ok && r.ObservationID != "" && str(o["observation_id"]) == r.ObservationID {
				found = o
				matches++
			}
		}
		if matches != 1 {
			continue
		}
		r.ObservationDigest, _ = hint.Digest(found)
		r.SourceID = str(found["source_id"])
		smatches := 0
		for _, v := range sources {
			src, ok := v.(map[string]any)
			if ok && str(src["source_id"]) == r.SourceID {
				r.SourceDigest, _ = hint.Digest(src)
				smatches++
			}
		}
		led, ok := found["model_ledger"].(map[string]any)
		if !ok {
			continue
		}
		r.LedgerDigest, _ = hint.Digest(led)
		// Native ledger/ref binding survives unchanged; privacy fields come only from
		// the existing projection, including its independently versioned source.
		bind, e := snapshot.Plain(r.Binding)
		if e != nil {
			continue
		}
		if str(led["ledger_ref"]) != r.LedgerRef || led["revision"] != float64(r.Revision) {
			continue
		}
		publicBind, ok := led["binding"].(map[string]any)
		if !ok {
			continue
		}
		bd, _ := hint.Digest(bind)
		pd, _ := hint.Digest(publicBind)
		if bd != pd {
			continue
		}
		run, _ := led["agent_run"].(map[string]any)
		if _, raw := run["execution_digest"]; raw {
			r.Reason = "legacy_raw_wire"
			continue
		}
		r.ExecutionProvenanceRef = str(run["execution_provenance_ref"])
		captures, _ := led["backend_capture"].(map[string]any)
		for _, side := range []string{"start", "end"} {
			cap, _ := captures[side].(map[string]any)
			snap, _ := cap["snapshot"].(map[string]any)
			ref := str(snap["image_ref"])
			if projectionRef.MatchString(ref) {
				r.BackendImages = append(r.BackendImages, BackendImageProjection{side, ref})
			}
		}
		if body["project_namespace"] != c.namespace || body["hint_version"] != "0.3.0" || !projectionRef.MatchString(r.ExecutionProvenanceRef) || len(r.BackendImages) != 2 || smatches != 1 || r.ObservationDigest == "" || r.SourceDigest == "" {
			continue
		}
		r.State, r.Reason = "complete", ""
	}
}
func (c *projectionCollector) finish() *LocalProjectionReceipt {
	if c.failed != "" {
		return &LocalProjectionReceipt{namespace: c.namespace, state: "unavailable", reason: c.failed}
	}
	if !c.sawActivity {
		return &LocalProjectionReceipt{namespace: c.namespace, state: "unavailable", reason: "activity_not_selected"}
	}
	if len(c.rows) == 0 {
		return &LocalProjectionReceipt{namespace: c.namespace, state: "unavailable", reason: "no_ledger_occurrences"}
	}
	state := "complete"
	for _, r := range c.rows {
		if r.State != "complete" {
			state = "incomplete"
		}
	}
	return &LocalProjectionReceipt{namespace: c.namespace, state: state, rows: c.rows}
}
