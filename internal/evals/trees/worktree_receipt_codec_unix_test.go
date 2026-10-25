//go:build unix

package trees

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/testkit"
)

type codecFixture struct {
	wire    SelectedSourceWire
	pair    *DecodedSelectedSourcePair
	objects map[string][]byte
}

func actualCodecFixture(t *testing.T, shape string) codecFixture {
	t.Helper()
	v, lim := openValidationView(t, shape)
	if shape == "known_empty" || shape == "detached" {
		if e := v.Close(); e != nil {
			t.Fatal(e)
		}
		if shape == "known_empty" {
			if e := os.WriteFile(filepath.Join(v.root, "known-empty"), nil, 0600); e != nil {
				t.Fatal(e)
			}
		} else {
			testkit.Git(t, v.root, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "checkout", "--detach", "-q")
		}
		next, e := OpenSelectedSourceView(context.Background(), v.session.library, v.root, SourceViewOptions{Limits: lim})
		if e != nil {
			t.Fatal(e)
		}
		v = next
	}
	defer func() {
		if e := v.Close(); e != nil {
			t.Error(e)
		}
	}()
	a, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	b, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	wire, e := EncodeSelectedSourcePair(lim, a.Selected().Receipt(), b.Selected().Receipt(), a.Sources(), b.Sources())
	if e != nil {
		t.Fatal(e)
	}
	pair, e := DecodeSelectedSourcePair(lim, wire)
	if e != nil {
		t.Fatal(e)
	}
	objects := map[string][]byte{}
	for _, r := range pair.ObjectRefs() {
		raw, e := v.session.library.ResolveBoundedRead(context.Background(), "object", r.Digest, r.MaxBytes)
		if e != nil {
			t.Fatal(e)
		}
		objects[r.Digest] = raw
	}
	if _, e := pair.ValidateObjects(objects); e != nil {
		t.Fatal("actual object emission refused", e)
	}
	return codecFixture{wire, pair, objects}
}
func codecWireCopy(w SelectedSourceWire) SelectedSourceWire {
	return SelectedSourceWire{append([]byte(nil), w.SelectedA...), append([]byte(nil), w.SelectedB...), append([]byte(nil), w.SourceA...), append([]byte(nil), w.SourceB...)}
}
func codecObjectCopy(m map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for d, b := range m {
		out[d] = append([]byte(nil), b...)
	}
	return out
}
func codecDocument(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if e := d.Decode(&m); e != nil {
		t.Fatal(e)
	}
	return m
}
func codecCanonical(t *testing.T, v any) []byte {
	t.Helper()
	b, e := hint.Canonical(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestSourceReceiptCodecActualEmissionAndCopies(t *testing.T) {
	for _, shape := range []string{"mode_untracked", "absent", "empty", "known_empty", "detached"} {
		t.Run(shape, func(t *testing.T) {
			f := actualCodecFixture(t, shape)
			a, b, x, y := f.pair.Receipts()
			again, e := EncodeSelectedSourcePair(a.EffectiveLimits, a, b, x, y)
			if e != nil || !reflect.DeepEqual(again, f.wire) {
				t.Fatal("canonical round trip", e)
			}
			a.Selected = append(a.Selected, SelectedPathFact{Path: "mutation"})
			x.Facts[0].Path = "mutation"
			refs := f.pair.ObjectRefs()
			refs[0].Digest = "mutation"
			f.wire.SelectedA[0] = '!'
			ca, _, cx, _ := f.pair.Receipts()
			if len(ca.Selected) == len(a.Selected) || cx.Facts[0].Path == "mutation" || f.pair.ObjectRefs()[0].Digest == "mutation" {
				t.Fatal("decoded aliases input/output")
			}
			if _, e := f.pair.ValidateObjects(f.objects); e != nil {
				t.Fatal(e)
			}
			if shape == "known_empty" {
				d := library.Digest(nil)
				if b, ok := f.objects[d]; !ok || len(b) != 0 {
					t.Fatal("known-empty raw object missing")
				}
				objects := codecObjectCopy(f.objects)
				objects[d] = []byte("x")
				if _, e := f.pair.ValidateObjects(objects); e == nil {
					t.Fatal("one byte accepted as known-empty")
				}
			}
		})
	}
}
func TestSourceReceiptCodecRequiredClosedWire(t *testing.T) {
	f := actualCodecFixture(t, "known_empty")
	a, _, _, _ := f.pair.Receipts()
	lim := a.EffectiveLimits
	// Literal reviewed LOCAL v1 names, independent of the production registry.
	selected := strings.Fields("Version Pass RootIdentity Selected Stages PresentCount IndexedAbsentCount UnindexedAbsentCount SelectedInventoryDigest StageInventoryDigest TreeDigest BeginOffsetNS EndOffsetNS EffectiveLimits")
	source := strings.Fields("Version Authority State ChainTiming Pass InvocationIdentity ExecutableDigest ExecutableNode CandidatePaths ConfigObject StateObject SourceDigest SelectedInventoryDigest StageInventoryDigest TreeDigest Facts SourceQueryAttempts SourceStdoutBytes SourceStderrBytes TotalDispatchAttempts PersistenceGuardAttempts ExecutableReads ExecutableReadBytes GuardStdoutBytes GuardStderrBytes MetadataReads MetadataBytes BeginOffsetNS EndOffsetNS")
	for _, row := range []struct {
		name   string
		raw    []byte
		fields []string
		source bool
	}{{"selected", f.wire.SelectedA, selected, false}, {"source", f.wire.SourceA, source, true}} {
		for _, field := range row.fields {
			for _, kind := range []string{"missing", "null", "wrong_type"} {
				t.Run(row.name+"/"+field+"/"+kind, func(t *testing.T) {
					m := codecDocument(t, row.raw)
					r := m["receipt"].(map[string]any)
					switch kind {
					case "missing":
						delete(r, field)
					case "null":
						r[field] = nil
					case "wrong_type":
						r[field] = map[string]any{}
					}
					w := codecWireCopy(f.wire)
					raw := codecCanonical(t, m)
					if row.source {
						w.SourceA = raw
					} else {
						w.SelectedA = raw
					}
					if p, e := DecodeSelectedSourcePair(lim, w); e == nil || p != nil {
						t.Fatal("missing/null/type silently defaulted")
					}
				})
			}
		}
	}
	for _, field := range strings.Fields("Path State Mode ObjectDigest Size Indexed PathIdentity AbsenceParentIdentity") {
		for _, kind := range []string{"missing", "null", "wrong_type"} {
			t.Run("fact/"+field+"/"+kind, func(t *testing.T) {
				m := codecDocument(t, f.wire.SelectedA)
				r := m["receipt"].(map[string]any)["Selected"].([]any)[0].(map[string]any)
				switch kind {
				case "missing":
					delete(r, field)
				case "null":
					r[field] = nil
				case "wrong_type":
					r[field] = []any{}
				}
				w := codecWireCopy(f.wire)
				w.SelectedA = codecCanonical(t, m)
				if p, e := DecodeSelectedSourcePair(lim, w); e == nil || p != nil {
					t.Fatal("nested field defaulted")
				}
			})
		}
	}
	for _, kind := range []string{"unknown", "case_alias", "duplicate", "trailing", "fraction", "exponent", "bool_string", "nil_array", "unknown_kind", "depth", "oversize", "invalid_utf8"} {
		t.Run(kind, func(t *testing.T) {
			w := codecWireCopy(f.wire)
			m := codecDocument(t, w.SelectedA)
			r := m["receipt"].(map[string]any)
			switch kind {
			case "unknown":
				r["unknown"] = "PRIVATE_SENTINEL"
			case "case_alias":
				r["version"] = r["Version"]
				delete(r, "Version")
			case "duplicate":
				w.SelectedA = bytes.Replace(w.SelectedA, []byte(`"Pass":1`), []byte(`"Pass":1,"Pass":1`), 1)
			case "trailing":
				w.SelectedA = append(w.SelectedA, []byte(`{}`)...)
			case "fraction":
				r["Pass"] = json.Number("1.5")
			case "exponent":
				w.SelectedA = bytes.Replace(w.SelectedA, []byte(`"Pass":1`), []byte(`"Pass":1e0`), 1)
			case "bool_string":
				r["Selected"].([]any)[0].(map[string]any)["Indexed"] = "false"
			case "nil_array":
				r["Selected"] = nil
			case "unknown_kind":
				m["kind"] = "unknown"
			case "depth":
				w.SelectedA = []byte(strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18))
			case "oversize":
				w.SelectedA = bytes.Repeat([]byte(" "), receiptWireMax+1)
			case "invalid_utf8":
				w.SelectedA = []byte{255}
			}
			if kind == "unknown" || kind == "case_alias" || kind == "fraction" || kind == "bool_string" || kind == "nil_array" || kind == "unknown_kind" {
				w.SelectedA = codecCanonical(t, m)
			}
			p, e := DecodeSelectedSourcePair(lim, w)
			if e == nil || p != nil {
				t.Fatal("malformed wire accepted")
			}
			if strings.Contains(e.Error(), "PRIVATE_SENTINEL") {
				t.Fatal("raw error leaked")
			}
		})
	}
	if shapeMatches(reflect.TypeOf(struct {
		Version string
		Extra   bool
	}{}), []string{"Version"}) {
		t.Fatal("shape drift widened v1")
	}
}
func TestSourceReceiptObjectsRefuseTamperAndContradictions(t *testing.T) {
	f := actualCodecFixture(t, "known_empty")
	a, _, x, _ := f.pair.Receipts()
	for _, kind := range []string{"missing", "extra", "content", "tree", "config", "state", "unborn_head", "wrong_branch", "state_facts", "state_extra", "state_null"} {
		t.Run(kind, func(t *testing.T) {
			m := codecObjectCopy(f.objects)
			target := ""
			switch kind {
			case "missing":
				delete(m, x.ConfigObject)
			case "extra":
				m[library.Digest([]byte("extra"))] = []byte("extra")
			case "content":
				for _, v := range a.Selected {
					if v.State == SelectedRegularPresent {
						m[v.ObjectDigest] = []byte("changed")
						break
					}
				}
			case "tree":
				m[a.TreeDigest] = []byte(`{"tree_version":1,"files":[]}`)
			case "config":
				target = x.ConfigObject
				m[target] = []byte("broken config")
			case "state":
				target = x.StateObject
				m[target] = []byte(`{}`)
			default:
				target = x.StateObject
				doc := codecDocument(t, m[target])
				switch kind {
				case "unborn_head":
					doc["Head"] = ""
				case "wrong_branch":
					doc["Branch"] = "refs/heads/other"
				case "state_facts":
					doc["Facts"] = []any{}
				case "state_extra":
					doc["extra"] = "PRIVATE_SENTINEL"
				case "state_null":
					doc["Facts"] = nil
				}
				if kind == "unborn_head" || kind == "wrong_branch" || kind == "state_facts" {
					var original sourceStateObject
					if e := json.Unmarshal(f.objects[target], &original); e != nil {
						t.Fatal(e)
					}
					switch kind {
					case "unborn_head":
						original.Head = ""
					case "wrong_branch":
						original.Branch = "refs/heads/other"
					case "state_facts":
						original.Facts = []SourceFact{}
					}
					raw, e := sourceStateBytes(original.Head, original.Branch, original.Facts)
					if e != nil {
						t.Fatal(e)
					}
					m[target] = raw
				} else {
					b, _ := json.Marshal(doc)
					m[target] = b
				}
			}
			pair := f.pair
			// Rebind the declared ref so raw shape/semantic checks, not just SHA mismatch,
			// reject malformed ConfigObject/StateObject bytes.
			if target != "" {
				p := *f.pair
				d := library.Digest(m[target])
				raw := m[target]
				delete(m, target)
				m[d] = raw
				if target == x.ConfigObject {
					p.x.ConfigObject = d
					p.y.ConfigObject = d
				} else {
					p.x.StateObject = d
					p.y.StateObject = d
				}
				refs, e := p.objectRequirements()
				if e != nil {
					t.Fatal("raw fixture reference could not bind", e)
				}
				p.refs = refs
				pair = &p
			}
			if _, e := pair.ValidateObjects(m); e == nil {
				t.Fatal("raw contradiction accepted")
			}
		})
	}
	// Same digest referenced with conflicting exact sizes must refuse before fetch.
	p := *f.pair
	p.a.Selected = append([]SelectedPathFact{}, p.a.Selected...)
	for i, v := range p.a.Selected {
		if v.Path == ".gitignore" {
			p.a.Selected[i].Size++
		}
	}
	if _, e := p.objectRequirements(); e == nil {
		t.Fatal("known-size alias conflict accepted")
	}
}
func TestSourceStateEncoderPreservesOriginalBytes(t *testing.T) {
	for _, facts := range [][]SourceFact{nil, {}, {{Role: "synthetic", Path: "/synthetic", State: "observed_absent"}}} {
		old, e := json.Marshal(struct {
			Head, Branch string
			Facts        []SourceFact
		}{"", "", facts})
		if e != nil {
			t.Fatal(e)
		}
		got, e := sourceStateBytes("", "", facts)
		if e != nil || !bytes.Equal(got, old) {
			t.Fatal("state encoder bytes changed")
		}
	}
}

func TestSourceReceiptConfigOriginsAndUnprovedDerivation(t *testing.T) {
	f := actualCodecFixture(t, "known_empty")
	for _, kind := range []string{"absent_origin", "wrong_scope", "unsafe_origin", "include", "unproved_value"} {
		t.Run(kind, func(t *testing.T) {
			p := *f.pair
			m := codecObjectCopy(f.objects)
			old := p.x.ConfigObject
			profile := receiptConfigProfile(p.x)
			scope, origin, key, value := "local", "file:"+filepath.Join(profile.gitdir, "config"), "synthetic.projection", "synthetic_value"
			switch kind {
			case "absent_origin":
				scope = "global"
				origin = "file:" + profile.candidates[1]
			case "wrong_scope":
				scope = "system"
			case "unsafe_origin":
				origin = "file:/unbound/PRIVATE_SENTINEL"
			case "include":
				key = "include.path"
				value = "/unbound"
			}
			raw := append(append([]byte(nil), m[old]...), []byte(scope+"\x00"+origin+"\x00"+key+"\n"+value+"\x00")...)
			d := library.Digest(raw)
			delete(m, old)
			m[d] = raw
			p.x.ConfigObject = d
			p.y.ConfigObject = d
			refs, e := p.objectRequirements()
			if e != nil {
				t.Fatal(e)
			}
			p.refs = refs
			_, e = p.ValidateObjects(m)
			if kind == "unproved_value" {
				if e != nil {
					t.Fatal("raw-file config evaluation was falsely required", e)
				}
			} else if e == nil {
				t.Fatal("contradictory config origin/scope accepted")
			}
		})
	}
}
func TestSourceReceiptAllNestedMembersRequired(t *testing.T) {
	f := actualCodecFixture(t, "known_empty")
	a, _, _, _ := f.pair.Receipts()
	for _, row := range []struct {
		name, fields string
		source       bool
	}{{"stage", "Path Mode ObjectID Stage", false}, {"limits", "MaxFileBytes MaxTotalBytes MaxFiles", false}, {"source_fact", "Role Path State ObjectDigest NodeIdentity AbsenceIdentity Bytes", true}} {
		for _, field := range strings.Fields(row.fields) {
			for _, kind := range []string{"missing", "null"} {
				t.Run(row.name+"/"+field+"/"+kind, func(t *testing.T) {
					w := codecWireCopy(f.wire)
					raw := w.SelectedA
					if row.source {
						raw = w.SourceA
					}
					doc := codecDocument(t, raw)
					r := doc["receipt"].(map[string]any)
					var target map[string]any
					switch row.name {
					case "stage":
						target = r["Stages"].([]any)[0].(map[string]any)
					case "limits":
						target = r["EffectiveLimits"].(map[string]any)
					case "source_fact":
						target = r["Facts"].([]any)[0].(map[string]any)
					}
					if kind == "missing" {
						delete(target, field)
					} else {
						target[field] = nil
					}
					raw = codecCanonical(t, doc)
					if row.source {
						w.SourceA = raw
					} else {
						w.SelectedA = raw
					}
					if h, e := DecodeSelectedSourcePair(a.EffectiveLimits, w); e == nil || h != nil {
						t.Fatal("nested missing/null defaulted")
					}
				})
			}
		}
	}
}
