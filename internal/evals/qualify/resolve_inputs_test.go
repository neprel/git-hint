package qualify

// All authority statements in this file are SYNTHETIC parser fixtures. They are not
// runtime evidence, project completion, prospective project intent, or source activation.
import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/evaltest/qfixture"
	"github.com/neprel/git-hint/internal/evals/library"
)

type fixture struct {
	l     *library.Library
	req   ResolveIORequest
	rec   map[string]any
	files map[string][]byte
}

func tarFixture(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for n, v := range files {
		if e := w.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(v)), Typeflag: tar.TypeReg}); e != nil {
			t.Fatal(e)
		}
		_, _ = w.Write(v)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func putFixture(t *testing.T, l *library.Library, b []byte) string {
	t.Helper()
	d, e := l.PutObject(b)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func resolveFixture(t *testing.T, edit func(*fixture)) *fixture {
	return resolveFixturePhases(t, nil, edit)
}
func resolveFixturePhases(t *testing.T, before func(*library.Library, map[string]any), edit func(*fixture)) *fixture {
	t.Helper()
	l := evaltest.Library(t)
	q := qfixture.QualifiedIO(t, l, before, func(q *qfixture.QualifiedIOFixture) {
		f := &fixture{l: q.L, req: ResolveIORequest{EvalName: q.EvalName, PreviousManifestDigest: q.PreviousManifestDigest}, rec: q.Record, files: q.Files}
		if edit != nil {
			edit(f)
		}
		q.Record = f.rec
		q.Files = f.files
	})
	return &fixture{l: q.L, req: ResolveIORequest{EvalName: q.EvalName, ManifestDigest: q.ManifestDigest, PreviousManifestDigest: q.PreviousManifestDigest}, rec: q.Record, files: q.Files}
}
func editResult(f *fixture, change func(map[string]any)) {
	for n, b := range f.files {
		if strings.HasSuffix(n, "/result.json") {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			change(m)
			f.files[n], _ = json.Marshal(m)
		}
	}
}

func TestResolveQualifiedIOExactInputsOnly(t *testing.T) {
	f := resolveFixture(t, nil)
	h, ref := ResolveQualifiedIO(context.Background(), f.l, f.req)
	if ref != nil {
		t.Fatal(ref)
	}
	id := h.Identity()
	if id.State != "resolved_inputs" || id.QualificationReferenceRuns != 3 || id.VerificationOracleRuns != 2 || id.SpecDigest == "" || id.InputsDigest == "" || id.ManifestDigest != f.req.ManifestDigest || id.ReferencePatchArchive == id.ReferenceResultTree {
		t.Fatalf("%+v", id)
	}
	id.MandatoryChecks[0] = "forged"
	if h.Identity().MandatoryChecks[0] != "B1" {
		t.Fatal("mutable identity")
	}
	b, _ := h.Object(id.VerifierArchive)
	b[0] ^= 1
	again, _ := h.Object(id.VerifierArchive)
	if bytes.Equal(b, again) {
		t.Fatal("mutable bytes")
	}
}
func TestResolveQualifiedIONamedRefusals(t *testing.T) {
	cases := map[string]func(*fixture){
		"runner identity": func(f *fixture) {
			editResult(f, func(m map[string]any) { obj(m["hint_container"])["runner"] = "other" })
		},
		"io spec identity": func(f *fixture) {
			editResult(f, func(m map[string]any) { obj(obj(m["hint_container"])["io"])["spec"] = library.Digest([]byte("other")) })
		},
		"io inputs identity": func(f *fixture) {
			editResult(f, func(m map[string]any) {
				obj(obj(m["hint_container"])["io"])["inputs"] = library.Digest([]byte("other"))
			})
		},
		"oracle count": func(f *fixture) {
			editResult(f, func(m map[string]any) { obj(obj(m["hint_container"])["io"])["oracle_runs"] = 3 })
		},
		"network unknown": func(f *fixture) {
			editResult(f, func(m map[string]any) { obj(obj(m["hint_container"])["io"])["network"] = "unknown" })
		},
		"trust missing": func(f *fixture) {
			editResult(f, func(m map[string]any) { delete(obj(m["hint_container"]), "result_trust") })
		},
		"reference missing":  func(f *fixture) { delete(f.files, "jobs/reference/reference__3/result.json") },
		"reference artifact": func(f *fixture) { f.files["jobs/base/base__1/oracle/final.tar"] = []byte("changed") },
		"forged probe": func(f *fixture) {
			editResult(f, func(m map[string]any) {
				if m["trial_name"] == "probe__1" {
					obj(obj(m["verifier_result"])["rewards"])["reward"] = 1
				}
			})
		},
		"failed reference": func(f *fixture) {
			f.files["jobs/reference/reference__1/verifier/test-stdout.txt"] = []byte("CHECK B1 FAIL\n")
		},
		"isolation missing": func(f *fixture) { delete(f.files, "jobs/probe/probe__1/agent/oracle.txt") },
		"unsafe archive":    func(f *fixture) { f.files["../outside"] = []byte("x") },
		"changed content":   func(f *fixture) { f.rec["instruction"] = putFixture(t, f.l, []byte("changed")) },
		"qualification control counts": func(f *fixture) {
			f.rec["_edit_qualification"] = func(q map[string]any) { array(q["checks"])[2].(map[string]any)["runs"] = 2 }
		},
		"no q3 audit": func(f *fixture) {
			f.rec["_edit_qualification"] = func(q map[string]any) { array(q["checks"])[0].(map[string]any)["evidence"] = []any{} }
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := resolveFixture(t, change)
			h, ref := ResolveQualifiedIO(context.Background(), f.l, f.req)
			if h != nil || ref == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}
func TestResolveExactNamesAndDeadline(t *testing.T) {
	f := resolveFixture(t, nil)
	for _, req := range []ResolveIORequest{{EvalName: "latest"}, {EvalName: f.req.EvalName, ManifestDigest: library.Digest(nil), PreviousManifestDigest: f.req.PreviousManifestDigest}, {EvalName: f.req.EvalName, ManifestDigest: f.req.ManifestDigest, PreviousManifestDigest: library.Digest(nil)}} {
		if h, e := ResolveQualifiedIO(context.Background(), f.l, req); h != nil || e == nil {
			t.Fatal("accepted substituted identity")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h, e := ResolveQualifiedIO(ctx, f.l, f.req); h != nil || e == nil {
		t.Fatal("accepted cancelled read")
	}
}
func TestResolveSafeArchive(t *testing.T) {
	good := tarFixture(t, map[string][]byte{"jobs/base/t/result.json": []byte("{}")})
	for _, kind := range []string{"duplicate", "link", "device", "truncated", "nonzero trailing", "unsafe", "zero payload missing EOS", "nonzero padding"} {
		t.Run(kind, func(t *testing.T) {
			b := good
			switch kind {
			case "truncated":
				b = b[:len(b)-512]
			case "zero payload missing EOS":
				b = tarFixture(t, map[string][]byte{"zero": make([]byte, 2048)})
				b = b[:len(b)-1024]
			case "nonzero padding":
				b = tarFixture(t, map[string][]byte{"one": []byte("x")})
				b[513] = 1
			case "nonzero trailing":
				b = append(append([]byte{}, b...), make([]byte, 512)...)
				b[len(b)-1] = 1
			default:
				var out bytes.Buffer
				tw := tar.NewWriter(&out)
				h := &tar.Header{Name: "safe", Mode: 0o600, Typeflag: tar.TypeReg}
				if kind == "link" {
					h.Typeflag = tar.TypeSymlink
					h.Linkname = "target"
				}
				if kind == "device" {
					h.Typeflag = tar.TypeChar
				}
				if kind == "unsafe" {
					h.Name = "/absolute"
				}
				_ = tw.WriteHeader(h)
				if kind == "duplicate" {
					_ = tw.WriteHeader(h)
				}
				_ = tw.Close()
				b = out.Bytes()
			}
			if _, e := safeTar(context.Background(), b, ResolveArchiveMax); e == nil {
				t.Fatal("accepted", kind)
			}
		})
	}
}
func TestResolveBoundedDescriptorReads(t *testing.T) {
	f := resolveFixture(t, nil)
	b, e := f.l.ResolveBoundedRead(context.Background(), "eval", f.req.EvalName, ResolveMetadataMax)
	if e != nil || library.Digest(b) != f.req.ManifestDigest {
		t.Fatal(e)
	}
	if _, e = f.l.ResolveBoundedRead(context.Background(), "eval", f.req.EvalName, 16); e == nil {
		t.Fatal("metadata limit ignored")
	}
	r := &resolutionRead{ctx: context.Background(), l: f.l, used: ResolveTotalMax, objects: map[string][]byte{}}
	if _, e = r.read("eval", f.req.EvalName, ResolveMetadataMax); e == nil {
		t.Fatal("aggregate limit ignored")
	}
	d := putFixture(t, f.l, bytes.Repeat([]byte("x"), int(ResolveArchiveMax)+1))
	if _, e = f.l.ResolveBoundedRead(context.Background(), "object", d, ResolveArchiveMax); e == nil {
		t.Fatal("archive limit ignored")
	}
}

// EV-1: partial controls are evidence of an attempted trial, never an ignorable file.
func TestResolveEveryEncounteredTrialHasResult(t *testing.T) {
	cases := map[string]func(*fixture){
		"extra failing reference stdout without result": func(f *fixture) {
			f.files["jobs/reference/reference__4/verifier/test-stdout.txt"] = []byte("CHECK B1 FAIL\n")
		},
		"extra reference oracle artifact alone": func(f *fixture) { f.files["jobs/reference/reference__9/oracle/final.tar"] = []byte("partial") },
		"incomplete near miss group": func(f *fixture) {
			f.files["jobs/near-miss-1/near-miss-1__1/verifier/test-stdout.txt"] = []byte("CHECK B1 FAIL\n")
		},
		"missing result for supported reference prefix": func(f *fixture) { delete(f.files, "jobs/reference/reference__3/result.json") },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := resolveFixture(t, edit)
			h, ref := ResolveQualifiedIO(context.Background(), f.l, f.req)
			if h != nil || ref == nil {
				t.Fatal("partial control omitted")
			}
		})
	}
}

// EV-3: this reason is emitted at initial source reads, before tree parsing/cache reuse.
func TestResolveKnownTreeMetadataCapAtFirstRead(t *testing.T) {
	f := resolveFixturePhases(t, func(l *library.Library, rec map[string]any) {
		huge := putFixture(t, l, bytes.Repeat([]byte("x"), int(ResolveMetadataMax)+1))
		repo := obj(array(rec["repositories"])[0])
		repo["base_tree"] = huge
	}, nil)
	h, ref := ResolveQualifiedIO(context.Background(), f.l, f.req)
	if h != nil || ref == nil || ref.Reason != "definition_object_unavailable_or_limit" {
		t.Fatalf("known metadata was not refused during first read: %v", ref)
	}
}
