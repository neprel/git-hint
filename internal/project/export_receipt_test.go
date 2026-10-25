package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/neprel/git-hint/internal/activity"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
)

func TestPackageReceiptActualProjectionAndByteParity(t *testing.T) {
	refs := []string{}
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			f := membershipFixture(t)
			privacyActualEpoch(t, f, "one")
			opt := ExportOptions{All: true, Now: memNow}
			old, e := BuildPackage(f.s, opt)
			if e != nil {
				t.Fatal(e)
			}
			p, r, e := BuildPackageWithReceipt(f.s, opt)
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(old, p) {
				t.Fatal("receipt changed ordinary package")
			}
			state, reason := r.State()
			if state != "complete" || reason != "" || r.Namespace() != f.s.Cfg.ProjectNamespace {
				t.Fatal("receipt unavailable", state, reason)
			}
			rows := r.Ledgers()
			if len(rows) != 1 {
				t.Fatal("ledger occurrence count", len(rows))
			}
			x := rows[0]
			if x.ExecutionProvenanceRef != hint.DeriveID(f.s.Key(), "execution-provenance", memExec) {
				t.Fatal("wrong execution role")
			}
			refs = append(refs, x.ExecutionProvenanceRef)
			if len(x.BackendImages) != 2 {
				t.Fatal("missing backend image sides")
			}
			for _, v := range x.BackendImages {
				if v.ImageRef != hint.DeriveID(f.s.Key(), "image", "sha256:"+strings.Repeat("c", 64)) {
					t.Fatal("wrong image role")
				}
			}
			obs, _, e := ReadSpool(f.s)
			if e != nil {
				t.Fatal(e)
			}
			srcs, e := Sources(f.s)
			if e != nil {
				t.Fatal(e)
			}
			for _, o := range obs {
				if o.ObservationID == x.NativeObservationID {
					od, _ := hint.Digest(o)
					ld, _ := hint.Digest(o.ModelLedger)
					sd, _ := hint.Digest(srcs[o.SourceID])
					if od != x.NativeObservationDigest || ld != x.NativeLedgerDigest || sd != x.NativeSourceDigest || o.ModelLedger.Binding != x.Binding {
						t.Fatal("native emitted body not bound")
					}
				}
			}
			matched := false
			for _, d := range p.Docs {
				if d.ID == x.DocumentID {
					matched = true
					sum := sha256.Sum256(d.Raw)
					if d.Digest != x.DocumentDigest || "sha256:"+hex.EncodeToString(sum[:]) != x.DocumentBytesSHA256 {
						t.Fatal("sealed document mismatch")
					}
					var b map[string]any
					json.Unmarshal(d.Raw, &b)
					for _, v := range b["payload"].(map[string]any)["observations"].([]any) {
						m := v.(map[string]any)
						if m["observation_id"] == x.ObservationID {
							od, _ := hint.Digest(m)
							ld, _ := hint.Digest(m["model_ledger"])
							if od != x.ObservationDigest || ld != x.LedgerDigest {
								t.Fatal("final body digest mismatch")
							}
						}
					}
					for _, v := range b["sources"].([]any) {
						m := v.(map[string]any)
						if m["source_id"] == x.SourceID {
							sd, _ := hint.Digest(m)
							if sd != x.SourceDigest {
								t.Fatal("final source mismatch")
							}
						}
					}
					if bytes.Contains(d.Raw, []byte(memExec)) || bytes.Contains(d.Raw, []byte(x.NativeLedgerDigest)) {
						t.Fatal("local values entered wire")
					}
				}
			}
			if !matched {
				t.Fatal("document missing")
			}
			rows[0].BackendImages[0].ImageRef = "changed"
			rows[0].State = "changed"
			if r.Ledgers()[0].BackendImages[0].ImageRef == "changed" || r.Ledgers()[0].State != "complete" {
				t.Fatal("mutable accessor")
			}
			if _, e := json.Marshal(r); e == nil {
				t.Fatal("receipt serialized as public JSON")
			}
		})
	}
	if len(refs) != 2 || refs[0] == refs[1] {
		t.Fatal("two keys shared execution identity")
	}
}

func TestPackageReceiptOmissionsKeepValidPackageAndCoarseRefusal(t *testing.T) {
	f := membershipFixture(t)
	privacyActualEpoch(t, f, "one")
	f.s.Cfg.Policy.Profile = "coarse"
	f.s.Cfg.Policy.TimeResolutionSeconds = 60
	if p, r, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow}); e == nil || p != nil || r != nil {
		t.Fatal("coarse ledger restriction changed")
	}
	fields, e := hint.RecordFieldsFor("0.3")
	if e != nil {
		t.Fatal(e)
	}
	var keep []string
	for _, s := range fields["Observation"] {
		if s != "model_ledger" {
			keep = append(keep, s)
		}
	}
	f.s.Cfg.Policy.Fields = map[string][]string{"Observation": keep}
	old, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	p, r, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil || !reflect.DeepEqual(old, p) {
		t.Fatal("valid omission refused", e)
	}
	state, _ := r.State()
	if state != "incomplete" || len(r.Ledgers()) != 1 || r.Ledgers()[0].State != "incomplete" || r.Ledgers()[0].LedgerDigest != "" {
		t.Fatal("omission minted complete receipt")
	}
	f.s.Cfg.Policy.Profile = "aggregate"
	f.s.Cfg.Policy.Fields = nil
	old, e = BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	p, r, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	state, _ = r.State()
	if e != nil || !reflect.DeepEqual(old, p) || state != "unavailable" {
		t.Fatal("aggregate behavior changed")
	}
}

func TestPackageReceiptBoundsAndZero(t *testing.T) {
	var r *LocalProjectionReceipt
	if state, _ := r.State(); state != "unavailable" || r.Ledgers() != nil || r.Namespace() != "" {
		t.Fatal("zero receipt")
	}
	if state, _ := (&LocalProjectionReceipt{}).State(); state != "unavailable" {
		t.Fatal("zero value receipt accepted")
	}
	if maxProjectionRows != 1024 || maxProjectionDocumentBytes != 16<<20 {
		t.Fatal("receipt bounds changed")
	}
	c := &projectionCollector{rows: make([]LedgerProjection, maxProjectionRows)}
	// A bounds refusal never yields partial success or a package error.
	c.capture(activity.Observation{ModelLedger: &activity.ModelLedger{}}, activity.Source{}, nil)
	r = c.finish()
	if state, _ := r.State(); state != "unavailable" || r.Ledgers() != nil {
		t.Fatal("partial bounded receipt")
	}
	c = &projectionCollector{}
	c.seal(Doc{Raw: make([]byte, maxProjectionDocumentBytes+1)})
	r = c.finish()
	if state, _ := r.State(); state != "unavailable" {
		t.Fatal("document cap ignored")
	}
}
