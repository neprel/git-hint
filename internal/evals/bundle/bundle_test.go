package bundle

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
)

func packed(t *testing.T) (*library.Library, string, []byte) {
	t.Helper()
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	name, err := l.PutManifest("eval", rec)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Pack(l, []string{name}, &buf); err != nil {
		t.Fatal(err)
	}
	return l, name, buf.Bytes()
}

func TestRoundTripKeepsIdentityAndDigests(t *testing.T) {
	src, name, b := packed(t)
	dst := evaltest.Library(t)
	res, err := Unpack(dst, bytes.NewReader(b))
	if err != nil || len(res.Evals) != 1 || res.Evals[0] != name {
		t.Fatalf("%+v %v", res, err)
	}
	var a, c map[string]any
	src.Manifest("eval", name, &a)
	dst.Manifest("eval", name, &c)
	if fmt.Sprint(a) != fmt.Sprint(c) {
		t.Fatal("record changed in the round trip")
	}
	repo := c["repositories"].([]any)[0].(map[string]any)
	for _, d := range []string{c["instruction"].(string), repo["base_archive"].(string), repo["base_tree"].(string)} {
		if _, err := dst.GetObject(d); err != nil {
			t.Fatalf("object %s missing after unpack", d[:20])
		}
	}
	if _, err := Unpack(dst, bytes.NewReader(b)); err != nil {
		t.Fatalf("second unpack of the same bundle: %v", err)
	}
}

// rewrite changes one entry of a bundle.
func rewrite(t *testing.T, b []byte, fn func(name string, data []byte) (string, []byte)) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		data, _ := io.ReadAll(tr)
		name, data := fn(h.Name, data)
		if name == "" {
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write(data)
	}
	tw.Close()
	return out.Bytes()
}

func TestDamagedBundlesAreRefusedWhole(t *testing.T) {
	_, name, b := packed(t)
	cases := map[string][]byte{
		"tampered object": rewrite(t, b, func(n string, d []byte) (string, []byte) {
			if strings.HasPrefix(n, "objects/") {
				return n, append(d, 'x')
			}
			return n, d
		}),
		"missing object": rewrite(t, b, func(n string, d []byte) (string, []byte) {
			if strings.HasPrefix(n, "objects/") {
				return "", nil
			}
			return n, d
		}),
		"traversal name": rewrite(t, b, func(n string, d []byte) (string, []byte) {
			if n == "bundle.json" {
				return "../../evil.json", d
			}
			return n, d
		}),
		"wrong version": rewrite(t, b, func(n string, d []byte) (string, []byte) {
			if n == "bundle.json" {
				return n, bytes.Replace(d, []byte(`"bundle_version": 1`), []byte(`"bundle_version": 9`), 1)
			}
			return n, d
		}),
	}
	for what, bad := range cases {
		dst := evaltest.Library(t)
		if _, err := Unpack(dst, bytes.NewReader(bad)); !errors.Is(err, ErrBundle) {
			t.Errorf("%s: %v", what, err)
		}
		if names, _ := dst.Manifests("eval"); len(names) != 0 {
			t.Errorf("%s: a record was kept from a refused bundle", what)
		}
		if u, _ := dst.Usage(); u != 0 {
			t.Errorf("%s: objects of a refused bundle were stored", what)
		}
	}
	_ = name
}

func TestConflictingVersionIsRefused(t *testing.T) {
	_, name, b := packed(t)
	dst := evaltest.Library(t)
	other := evaltest.MapGoBugFix(t, dst, evaltest.GoBugFix(t, dst))
	other["state"] = "validated"
	if _, err := dst.PutManifest("eval", other); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(dst, bytes.NewReader(b)); !errors.Is(err, library.ErrImmutable) {
		t.Fatalf("conflict for %s: %v", name, err)
	}
}
