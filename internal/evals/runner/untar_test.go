package runner

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, link, body string
	typ              byte
}

func archive(t *testing.T, es ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

// G10-1: the reviewer's chain d -> ., x -> d/.., then x/file, must not write outside.
func TestUntarLinkChainEscape(t *testing.T) {
	cases := map[string][]entry{
		"chain": {
			{name: "d", link: ".", typ: tar.TypeSymlink},
			{name: "x", link: "d/..", typ: tar.TypeSymlink},
			{name: "x/file", body: "escaped", typ: tar.TypeReg},
		},
		"chain-dir-entry": {
			{name: "d", link: ".", typ: tar.TypeSymlink},
			{name: "x", link: "d/..", typ: tar.TypeSymlink},
			{name: "x/sub/", typ: tar.TypeDir},
			{name: "x/sub/file", body: "escaped", typ: tar.TypeReg},
		},
		"chain-without-write": {
			{name: "d", link: ".", typ: tar.TypeSymlink},
			{name: "x", link: "d/..", typ: tar.TypeSymlink},
		},
		"reverse-order": {
			{name: "x", link: "d/..", typ: tar.TypeSymlink},
			{name: "d", link: ".", typ: tar.TypeSymlink},
		},
		"through-inside-link": {
			{name: "sub/", typ: tar.TypeDir},
			{name: "l", link: "sub", typ: tar.TypeSymlink},
			{name: "l/file", body: "x", typ: tar.TypeReg},
		},
		"duplicate-over-link": {
			{name: "f", link: "sub/f", typ: tar.TypeSymlink},
			{name: "f", body: "x", typ: tar.TypeReg},
		},
		"absolute-link": {{name: "a", link: "/etc", typ: tar.TypeSymlink}},
		"dotdot-entry":  {{name: "../file", body: "x", typ: tar.TypeReg}},
	}
	for name, es := range cases {
		t.Run(name, func(t *testing.T) {
			outer := t.TempDir()
			if err := untar(archive(t, es...), filepath.Join(outer, "in")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			for _, p := range []string{"file", "sub", "escaped"} {
				if _, err := os.Lstat(filepath.Join(outer, p)); err == nil {
					t.Fatalf("%s written outside the target", p)
				}
			}
		})
	}
}

func TestUntarKeepsInsideLinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "in")
	err := untar(archive(t,
		entry{name: "pkg/", typ: tar.TypeDir},
		entry{name: "pkg/a.go", body: "package pkg", typ: tar.TypeReg},
		entry{name: "pkg/b.go", link: "a.go", typ: tar.TypeSymlink},
		entry{name: "top.go", link: "pkg/a.go", typ: tar.TypeSymlink},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "top.go")); err != nil || string(b) != "package pkg" {
		t.Fatalf("%q %v", b, err)
	}
}
