package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func custodyRecord(t *testing.T, l *library.Library) map[string]any {
	t.Helper()
	base := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "cat\n"})
	result := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
	b, e := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Repo: "example", Base: base, Result: result, Request: "Uppercase stdin.", RequestAt: map[string]any{"source": library.Digest([]byte("synthetic")), "record": "u1"}, Entry: []string{"sh", "tool.sh"}, Cases: []build.IOCase{{ID: "lower", Input: []byte("hello\n")}}, CaseSeconds: 5, Now: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	return b.Record
}
func TestBoundedExportBaseCustodyParityAndCopies(t *testing.T) {
	l := evaltest.Library(t)
	rec := custodyRecord(t, l)
	one, two := filepath.Join(t.TempDir(), "ordinary"), filepath.Join(t.TempDir(), "bounded")
	if _, e := Export(l, rec, one, DefaultSettings); e != nil {
		t.Fatal(e)
	}
	task, e := ExportBounded(context.Background(), l, rec, two, DefaultSettings)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := DirDigest(one)
	b, _ := DirDigest(two)
	if a != b {
		t.Fatal("ordinary directory bytes changed")
	}
	inputs, e := task.FrozenIOInputs()
	if e != nil {
		t.Fatal(e)
	}
	data, e := inputs.BaseArchive()
	if e != nil {
		t.Fatal(e)
	}
	inv, e := inputs.BaseInventory()
	if e != nil || inv.Digest != sumDigest(data) || len(inv.Entries) != 2 {
		t.Fatal("actual extraction inventory", e)
	}
	task.Inputs["base_archive"] = "changed"
	again, e := task.FrozenIOInputs()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := again.BaseArchive()
	if e != nil || !bytes.Equal(data, raw) {
		t.Fatal("mutable Task.Inputs changed frozen custody", e)
	}
	data[0] ^= 1
	inv.Entries[0].Digest = "changed"
	frozen, _ := again.BaseInventory()
	if frozen.Entries[0].Digest == "changed" {
		t.Fatal("inventory alias")
	}
	if e = os.RemoveAll(two); e != nil {
		t.Fatal(e)
	}
	if b, e := inputs.BaseArchive(); e != nil || sumDigest(b) != frozen.Digest {
		t.Fatal("path replacement changed retained bytes", e)
	}
	if reflect.DeepEqual(data, raw) {
		t.Fatal("byte copy alias")
	}
}
func TestBoundedExportBaseObservedPassStrictCompletion(t *testing.T) {
	for _, bad := range []string{"short_eos", "trailing", "duplicate", "alias", "unsafe_mode"} {
		t.Run(bad, func(t *testing.T) {
			l := evaltest.Library(t)
			rec := custodyRecord(t, l)
			var b bytes.Buffer
			w := tar.NewWriter(&b)
			names := []string{"tool.sh"}
			if bad == "duplicate" {
				names = append(names, "tool.sh")
			}
			if bad == "alias" {
				names = append(names, "TOOL.SH")
			}
			for _, n := range names {
				mode := int64(0644)
				if bad == "unsafe_mode" {
					mode = 04755
				}
				w.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: mode, Size: 1})
				w.Write([]byte("x"))
			}
			w.Close()
			raw := b.Bytes()
			if bad == "short_eos" {
				raw = raw[:len(raw)-512]
			}
			if bad == "trailing" {
				raw = append(raw, 1)
			}
			d, e := l.PutObject(raw)
			if e != nil {
				t.Fatal(e)
			}
			plain, _ := json.Marshal(rec)
			json.Unmarshal(plain, &rec)
			rec["repositories"].([]any)[0].(map[string]any)["base_archive"] = d
			if task, e := ExportBounded(context.Background(), l, rec, filepath.Join(t.TempDir(), "bad"), DefaultSettings); e == nil || task != nil {
				t.Fatal("invalid BASE granted custody")
			}
		})
	}

}
