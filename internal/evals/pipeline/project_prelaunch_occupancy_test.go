package pipeline

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/neprel/git-hint/internal/evals/runner"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedAbsenceIncludesActualMaterializedImplicitDirectory(t *testing.T) {
	selected, inv := selectedFactsFixture()
	inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: "removed.go/child", Type: tar.TypeReg, Mode: 0644, LogicalMode: 0100644, Size: 0, Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"})
	// The ordinary accepted materializer creates this implicit ancestor. No engine runs.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if e := tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}); e != nil {
		t.Fatal(e)
	}
	if e := tw.WriteHeader(&tar.Header{Name: "app/removed.go/child", Typeflag: tar.TypeReg, Mode: 0644}); e != nil {
		t.Fatal(e)
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(raw.Bytes())
	archive := filepath.Join(t.TempDir(), "actual.tar")
	if e := os.WriteFile(archive, raw.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	dest := t.TempDir()
	if _, e := runner.MaterializePrelaunchTree(archive, "sha256:"+hex.EncodeToString(h[:]), dest); e != nil {
		t.Fatal("valid implicit-directory archive refused", e)
	}
	st, e := os.Lstat(filepath.Join(dest, "removed.go"))
	if e != nil || !st.IsDir() {
		t.Fatal("missing actual implicit ancestor", e)
	}
	if _, _, _, e := selectedInventoryFacts(selected, inv, false); e == nil {
		t.Fatal("selected absence accepted despite actual implicit directory at removed.go")
	}
}
