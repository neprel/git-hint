package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git-hint")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..") // docFiles are repository files
	when := time.Unix(1700000000, 0).UTC()
	lic := []namedFile{{"licenses/go/LICENSE", []byte("license")}}
	a, b := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "b.tar.gz")
	if _, err := writeArchive(a, "x", root, bin, lic, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := writeArchive(b, "x", root, bin, lic, when); err != nil {
		t.Fatal(err)
	}
	ba, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if !bytes.Equal(ba, bb) {
		t.Fatal("two archives of the same input differ")
	}
}

func TestCheckBinaryFindsForbiddenContent(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"fixture":   "x HINTCANARY_SRC_5e1 y",
		"home":      "path /Users/someone/project/x.go",
		"lan":       "dial 10.20.30.40:8000",
		"pat":       "ghp_abcdefghijklmnopqrstuvwxyz0123",
		"key":       "-----BEGIN OPENSSH PRIVATE KEY-----",
		"go source": "package billing\n",
	} {
		p := filepath.Join(dir, "bad")
		_ = os.WriteFile(p, []byte(body), 0o644)
		if len(checkBinary(p)) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
	ok := filepath.Join(dir, "ok")
	// the validator's bare markers and public addresses are allowed
	_ = os.WriteFile(ok, []byte("HINTCANARY_ canary_dir_e1d2 canarySymbolQ7 127.0.0.1 /home/ 8.8.8.8"), 0o644)
	if bad := checkBinary(ok); len(bad) != 0 {
		t.Errorf("false positive: %v", bad)
	}
}

// distWith writes a release directory with one archive of the given tar content and gzip
// level, and a manifest that names the build host.
func distWith(t *testing.T, content string, level int, host string) string {
	t.Helper()
	dir := t.TempDir()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "x/git-hint", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	var gb bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gb, level)
	zw.Write(tb.Bytes())
	zw.Close()
	name := "git-hint_1.0.0_linux_amd64.tar.gz"
	_ = os.WriteFile(filepath.Join(dir, name), gb.Bytes(), 0o644)
	sum, size, _ := hashFile(filepath.Join(dir, name))
	ts, bs, err := contentSums(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "v1.0.0", Commit: "c", GoVersion: "go1", BuildHost: host,
		Artifacts: []Artifact{{OS: "linux", Arch: "amd64", File: name, SHA256: sum, Size: size, TarSHA256: ts, BinarySHA256: bs}}}
	mb, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	if err := writeSums(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSumsAndVerify(t *testing.T) {
	dir := distWith(t, "program", gzip.BestCompression, "linux/amd64")
	if err := cmdVerify([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "git-hint_1.0.0_linux_amd64.tar.gz"), []byte("b"), 0o644)
	if err := cmdVerify([]string{"-dir", dir}); err == nil {
		t.Fatal("a changed archive passed")
	}
}

func TestCompare(t *testing.T) {
	a := distWith(t, "program", gzip.BestCompression, "linux/amd64")
	same := distWith(t, "program", gzip.BestCompression, "linux/amd64")
	otherGzip := distWith(t, "program", gzip.BestSpeed, "darwin/arm64")
	otherGzipSameHost := distWith(t, "program", gzip.BestSpeed, "linux/amd64")
	otherProgram := distWith(t, "program2", gzip.BestCompression, "linux/amd64")
	if err := cmdCompare([]string{a, same}); err != nil {
		t.Errorf("identical: %v", err)
	}
	if err := cmdCompare([]string{a, otherGzip}); err != nil {
		t.Errorf("other host, same content: %v", err)
	}
	if err := cmdCompare([]string{a, otherGzipSameHost}); err == nil {
		t.Error("same host with different archives passed")
	}
	if err := cmdCompare([]string{a, otherProgram}); err == nil {
		t.Error("different program passed")
	}
}

func TestVersionAndTargetRules(t *testing.T) {
	for v, ok := range map[string]bool{"v0.1.0": true, "v1.2.3-rc.1": true, "v0.0.0-dev": true, "v0.0.0-ci": false, "v1.0.0-rc.01": false,
		"0.1.0": false, "v1.2": false, "v01.2.3": false, "v1.2.3;rm": false} {
		if semver.MatchString(v) != ok {
			t.Errorf("%s: want %v", v, ok)
		}
	}
	if err := cmdBuild([]string{"-version", "v0.1.0", "-out", filepath.Join(t.TempDir(), "d"), "-targets", "windows/amd64"}); err == nil || !strings.Contains(err.Error(), "not a release target") {
		t.Fatalf("a Windows target: %v", err)
	}
}

func TestVersionPatternMatchesSchema(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "v0.1", "common.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for name, d := range s.Defs {
		if strings.Contains(d.Pattern, "alpha|beta|rc|dev") {
			if "^v"+strings.TrimPrefix(d.Pattern, "^") != semver.String() {
				t.Fatalf("release version pattern %s differs from schema %s pattern %s", semver, name, d.Pattern)
			}
			return
		}
	}
	t.Fatal("no version pattern in the schema")
}

// TestBuildUsesOnlyTheCommitsFiles builds from a clone with an untracked Go file, an
// ignored Go file and a modified tracked file. None of them may reach the binary, and the
// manifest names the clone's commit.
func TestBuildUsesOnlyTheCommitsFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the client")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", "--no-hardlinks", repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	marker := func(name string) string {
		return "package main\n\nimport \"os\"\n\nfunc init() {\n\tif os.Getenv(\"HINT_NEVER\") == \"x\" {\n\t\tprintln(\"" + name + "\")\n\t}\n}\n"
	}
	write := func(rel, body string, appendTo bool) {
		p := filepath.Join(clone, rel)
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if appendTo {
			flag = os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(p, flag, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(body)
		f.Close()
	}
	write("cmd/git-hint/zz_untracked.go", marker("HINT_UNTRACKED_MARKER"), false)
	write("cmd/git-hint/zz_ignored.go", marker("HINT_IGNORED_MARKER"), false)
	write(".git/info/exclude", "\ncmd/git-hint/zz_ignored.go\n", true)
	write("cmd/git-hint/main.go", "\nvar hintModifiedFlag bool\n\nfunc init() {\n\tif hintModifiedFlag {\n\t\tprintln(\"HINT_MODIFIED_MARKER\")\n\t}\n}\n", true)
	if st, _ := exec.Command("git", "-C", clone, "status", "--porcelain", "--ignored").Output(); !bytes.Contains(st, []byte("!! cmd/git-hint/zz_ignored.go")) ||
		!bytes.Contains(st, []byte("?? cmd/git-hint/zz_untracked.go")) {
		t.Fatalf("test setup: %s", st)
	}
	head, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
	wd, _ := os.Getwd()
	if err := os.Chdir(clone); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	out := filepath.Join(t.TempDir(), "dist")
	if err := cmdBuild([]string{"-version", "v0.1.0", "-out", out, "-targets", "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	mb, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err := json.Unmarshal(mb, &m); err != nil || m.Commit != strings.TrimSpace(string(head)) {
		t.Fatalf("manifest commit %q, want %q (%v)", m.Commit, head, err)
	}
	f, err := os.Open(filepath.Join(out, m.Artifacts[0].File))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatal("no git-hint program in the archive")
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(h.Name, "/git-hint") {
			continue
		}
		bin, _ := io.ReadAll(tr)
		for _, mk := range []string{"HINT_UNTRACKED_MARKER", "HINT_IGNORED_MARKER", "HINT_MODIFIED_MARKER"} {
			if bytes.Contains(bin, []byte(mk)) {
				t.Errorf("%s reached the release binary", mk)
			}
		}
		return
	}
}
